package reposvc

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

const waitTimeout = 30 * time.Second

type testEnv struct {
	svc      *Service
	st       *store.Store
	bus      *events.Bus
	reposDir string
	runtime  string
	home     string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	testutil.RequireTool(t, "git")

	st := testutil.TempStore(t)
	bus := events.NewBus()
	home := t.TempDir()
	stateDir := t.TempDir()
	runtime := filepath.Join(stateDir, "runtime")

	mat, err := vault.NewMaterializer(runtime)
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	v, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	reposDir := filepath.Join(stateDir, "repos")
	svc, err := New(Options{
		Store:        st,
		Vault:        v,
		Materializer: mat,
		Git:          gitx.New("git"),
		Bus:          bus,
		Logger:       logx.New(io.Discard),
		ReposDir:     reposDir,
		GitEnv:       testutil.HermeticGitEnv(home),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// LIFO cleanup: cancel and drain clone jobs BEFORE the temp dirs and
	// the store go away under them.
	t.Cleanup(svc.Close)
	return &testEnv{svc: svc, st: st, bus: bus, reposDir: reposDir, runtime: runtime, home: home}
}

// gitCmd runs one git command with the hermetic env, failing t on error.
func gitCmd(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(home)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// makeOrigin builds a fixture origin repo on the given branch.
func makeOrigin(t *testing.T, home, branch string, commits int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	gitCmd(t, home, "", "init", "-q", "-b", branch, dir)
	for i := range commits {
		name := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(name, fmt.Appendf(nil, "content %d\n", i), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
		gitCmd(t, home, dir, "add", ".")
		gitCmd(t, home, dir, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}
	return dir
}

// eventLog collects bus events for assertions.
type eventLog struct {
	mu     sync.Mutex
	events []events.Event
}

func collectEvents(t *testing.T, bus *events.Bus) *eventLog {
	t.Helper()
	ch, cancel := bus.Subscribe(context.Background())
	t.Cleanup(cancel)
	l := &eventLog{}
	go func() {
		for e := range ch {
			l.mu.Lock()
			l.events = append(l.events, e)
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *eventLog) snapshot() []events.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]events.Event(nil), l.events...)
}

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s: %s", waitTimeout, desc)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *testEnv) waitCloneStatus(t *testing.T, id, want string) store.Repo {
	t.Helper()
	var repo store.Repo
	waitFor(t, fmt.Sprintf("repo %s clone_status %s", id, want), func() bool {
		r, err := e.st.RepoByID(context.Background(), id)
		if err != nil {
			t.Fatalf("RepoByID: %v", err)
		}
		repo = r
		return r.CloneStatus == want
	})
	return repo
}

func (e *testEnv) createCredential(t *testing.T, name, kind string, payload any) store.Credential {
	t.Helper()
	sealed, err := e.svc.vault.EncryptPayload(payload)
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	c, err := e.st.CreateCredential(context.Background(), ids.NewID("cred"), name, kind, sealed, time.Now())
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	return c
}

func TestAddCloneLifecycle(t *testing.T) {
	e := newTestEnv(t)
	log := collectEvents(t, e.bus)
	origin := makeOrigin(t, e.home, "trunk", 3)

	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Create-time row: pinned defaults, binding resolved at CREATE.
	if repo.CloneStatus != store.CloneStatusCloning {
		t.Errorf("clone_status = %q, want cloning", repo.CloneStatus)
	}
	if repo.Name != "origin" {
		t.Errorf("derived name = %q, want origin", repo.Name)
	}
	if repo.Provider != DefaultProvider {
		t.Errorf("provider = %q, want %q", repo.Provider, DefaultProvider)
	}
	if repo.TrackerBinding != store.TrackerBindingBuiltin || repo.ForgeKind != "none" {
		t.Errorf("binding/forge_kind = %q/%q, want builtin/none", repo.TrackerBinding, repo.ForgeKind)
	}
	if repo.AFKBranchPattern != "afk/<N>" || repo.ManualBranchPrefix != "lab/" {
		t.Errorf("patterns = %q/%q, want afk/<N> and lab/", repo.AFKBranchPattern, repo.ManualBranchPrefix)
	}

	final := e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	if final.DefaultBranch != "trunk" {
		t.Errorf("default_branch = %q, want detected trunk", final.DefaultBranch)
	}
	if final.CloneError != nil {
		t.Errorf("clone_error = %q, want nil", *final.CloneError)
	}

	bare := filepath.Join(e.reposDir, repo.ID+".git")
	if got := gitCmd(t, e.home, bare, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("bare dir is not a bare repository: %q", got)
	}

	// Events: repo.changed at create + completion, clone.progress in between.
	var changed, progress int
	for _, ev := range log.snapshot() {
		switch ev.Type {
		case EventRepoChanged:
			p, ok := ev.Payload.(repoChangedPayload)
			if !ok || p.RepoID != repo.ID || p.Type != EventRepoChanged {
				t.Errorf("bad repo.changed payload: %#v", ev.Payload)
			}
			changed++
		case EventCloneProgress:
			p, ok := ev.Payload.(cloneProgressPayload)
			if !ok || p.RepoID != repo.ID || p.Line == "" {
				t.Errorf("bad clone.progress payload: %#v", ev.Payload)
			}
			progress++
		}
	}
	if changed < 2 {
		t.Errorf("saw %d repo.changed events, want >= 2 (create + ready)", changed)
	}
	if progress == 0 {
		t.Error("saw no clone.progress events")
	}
}

func TestAddValidation(t *testing.T) {
	e := newTestEnv(t)
	forgeCred := e.createCredential(t, "forge", vault.KindForgeToken, vault.ForgeTokenPayload{Host: "git.cloonar.com", Token: "tk"})
	sshCred := e.createCredential(t, "ssh", vault.KindSSHKey, vault.SSHKeyPayload{PrivateKey: "KEY"})
	missing := "cred_00000000000000000000000000000000"

	tests := []struct {
		name string
		p    AddParams
		want string // substring of the BadRequestError
	}{
		{"empty remote", AddParams{RemoteURL: "  "}, "remote_url is required"},
		{"underivable name", AddParams{RemoteURL: "///"}, "cannot derive"},
		{"bad binding", AddParams{RemoteURL: "/tmp/x", TrackerBinding: "jira"}, "tracker_binding"},
		// The relaxed gate (ADR-0015): explicit forge no longer requires a
		// detected host — a forge credential is the only gate, so both an
		// undetected and a detected host answer the same credential error.
		{"forge binding without credential (undetected host)", AddParams{RemoteURL: "/tmp/x", TrackerBinding: "forge"}, "requires a forge_token credential"},
		{"forge binding without credential (detected host)", AddParams{RemoteURL: "forgejo@git.cloonar.com:me/proj.git", TrackerBinding: "forge"}, "requires a forge_token credential"},
		{"forge token as git cred", AddParams{RemoteURL: "/tmp/x", CredentialID: &forgeCred.ID}, "want ssh_key or https_token"},
		{"ssh key as forge cred", AddParams{RemoteURL: "/tmp/x", ForgeCredentialID: &sshCred.ID}, "want forge_token"},
		{"missing git cred", AddParams{RemoteURL: "/tmp/x", CredentialID: &missing}, "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.Add(t.Context(), tt.p)
			var bad *BadRequestError
			if err == nil || !asBadRequest(err, &bad) {
				t.Fatalf("Add error = %v, want BadRequestError", err)
			}
			if !strings.Contains(bad.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", bad.Error(), tt.want)
			}
		})
	}

	// Duplicate name after sanitization ("same name" and "same?name" both
	// sanitize to "same_name") surfaces the store sentinel.
	origin := makeOrigin(t, e.home, "main", 1)
	if _, err := e.svc.Add(t.Context(), AddParams{RemoteURL: origin, Name: "same name"}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	_, err := e.svc.Add(t.Context(), AddParams{RemoteURL: origin, Name: "same?name"})
	if !isErr(err, store.ErrNameTaken) {
		t.Fatalf("collision Add error = %v, want ErrNameTaken", err)
	}
}

func TestAddIncogniAndAutoBinding(t *testing.T) {
	e := newTestEnv(t)

	// Incogni seeds neutral patterns at CREATE (pinned contract).
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: filepath.Join(t.TempDir(), "nowhere"), Incogni: true})
	if err != nil {
		t.Fatalf("Add incogni: %v", err)
	}
	if repo.AFKBranchPattern != "issue-<N>" || repo.ManualBranchPrefix != "wip/" {
		t.Errorf("incogni patterns = %q/%q, want issue-<N> and wip/", repo.AFKBranchPattern, repo.ManualBranchPrefix)
	}
	if !repo.Incogni {
		t.Error("incogni flag not persisted")
	}

	// Auto binding resolves to forge only when BOTH a forge host is
	// detected AND a forge credential is attached (design §3a: forge
	// binding requires forge_credential_id, so auto never produces the
	// invalid combination). Without one the repo lands on builtin — the
	// forge kind is still recorded for a later flip.
	forgeCred := e.createCredential(t, "forge-auto", vault.KindForgeToken,
		vault.ForgeTokenPayload{Host: "git.cloonar.com", Token: "tk"})

	bare, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "forgejo@git.cloonar.com:me/proj.git", TrackerBinding: "auto"})
	if err != nil {
		t.Fatalf("Add forgejo remote without forge credential: %v", err)
	}
	if bare.TrackerBinding != store.TrackerBindingBuiltin || bare.ForgeKind != "forgejo" {
		t.Errorf("credential-less forgejo binding/kind = %q/%q, want builtin/forgejo", bare.TrackerBinding, bare.ForgeKind)
	}

	forge, err := e.svc.Add(t.Context(), AddParams{
		RemoteURL: "forgejo@git.cloonar.com:me/proj2.git", TrackerBinding: "auto",
		ForgeCredentialID: &forgeCred.ID,
	})
	if err != nil {
		t.Fatalf("Add forgejo remote: %v", err)
	}
	if forge.TrackerBinding != store.TrackerBindingForge || forge.ForgeKind != "forgejo" {
		t.Errorf("forgejo binding/kind = %q/%q, want forge/forgejo", forge.TrackerBinding, forge.ForgeKind)
	}
	gh, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "git@github.com:me/ghproj.git", ForgeCredentialID: &forgeCred.ID})
	if err != nil {
		t.Fatalf("Add github remote: %v", err)
	}
	if gh.TrackerBinding != store.TrackerBindingForge || gh.ForgeKind != "github" {
		t.Errorf("github binding/kind = %q/%q, want forge/github", gh.TrackerBinding, gh.ForgeKind)
	}
	other, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "git@gitlab.com:me/glproj.git", ForgeCredentialID: &forgeCred.ID})
	if err != nil {
		t.Fatalf("Add gitlab remote: %v", err)
	}
	if other.TrackerBinding != store.TrackerBindingBuiltin || other.ForgeKind != "none" {
		t.Errorf("gitlab binding/kind = %q/%q, want builtin/none", other.TrackerBinding, other.ForgeKind)
	}
}

func TestCloneFailureRecordsErrorWithoutSecrets(t *testing.T) {
	e := newTestEnv(t)
	const secret = "sekrit-token-b64Ue"
	cred := e.createCredential(t, "https", vault.KindHTTPSToken,
		vault.HTTPSTokenPayload{Username: "bot-user-xzq", Token: secret})

	// invalid.invalid is guaranteed NXDOMAIN (RFC 2606): the clone fails
	// without the token ever leaving the askpass file.
	repo, err := e.svc.Add(t.Context(), AddParams{
		RemoteURL:    "https://invalid.invalid/owner/repo.git",
		CredentialID: &cred.ID,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	final := e.waitCloneStatus(t, repo.ID, store.CloneStatusError)
	if final.CloneError == nil || *final.CloneError == "" {
		t.Fatal("clone_error empty after failed clone")
	}
	if strings.Contains(*final.CloneError, secret) || strings.Contains(*final.CloneError, "bot-user-xzq") {
		t.Errorf("clone_error leaks credential bytes: %q", *final.CloneError)
	}

	// Partial dir removed; this op's materialized askpass
	// (<credID>.<repoID>.askpass — per-op filenames) cleaned at op end.
	if _, err := os.Stat(filepath.Join(e.reposDir, repo.ID+".git")); !os.IsNotExist(err) {
		t.Errorf("failed clone left bare dir behind: %v", err)
	}
	waitFor(t, "askpass cleaned", func() bool {
		_, err := os.Stat(filepath.Join(e.runtime, cred.ID+"."+repo.ID+".askpass"))
		return os.IsNotExist(err)
	})
	leftovers, err := filepath.Glob(filepath.Join(e.runtime, "*.askpass"))
	if err != nil {
		t.Fatalf("glob runtime: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("askpass files left in runtime dir after op end: %v", leftovers)
	}
}

func TestRetryFlow(t *testing.T) {
	e := newTestEnv(t)

	// The remote does not exist yet: the first clone fails.
	future := filepath.Join(t.TempDir(), "origin")
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: future})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusError)

	// Retry from any non-error state is refused.
	origin2 := makeOrigin(t, e.home, "main", 1)
	ready, err := e.svc.Add(t.Context(), AddParams{RemoteURL: origin2, Name: "other"})
	if err != nil {
		t.Fatalf("Add other: %v", err)
	}
	e.waitCloneStatus(t, ready.ID, store.CloneStatusReady)
	if err := e.svc.Retry(t.Context(), ready.ID); !isErr(err, ErrCloneNotFailed) {
		t.Fatalf("Retry on ready repo = %v, want ErrCloneNotFailed", err)
	}

	// Create the origin where the failed repo points, then retry → ready.
	gitCmd(t, e.home, "", "init", "-q", "-b", "main", future)
	if err := os.WriteFile(filepath.Join(future, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitCmd(t, e.home, future, "add", ".")
	gitCmd(t, e.home, future, "commit", "-q", "-m", "c")

	if err := e.svc.Retry(t.Context(), repo.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	final := e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	if final.CloneError != nil {
		t.Errorf("clone_error after successful retry = %q, want nil", *final.CloneError)
	}
	if final.DefaultBranch != "main" {
		t.Errorf("default_branch = %q, want main", final.DefaultBranch)
	}
}

func TestDeleteGuardMatrix(t *testing.T) {
	e := newTestEnv(t)

	// A ~32MB incompressible blob keeps the clone busy long enough for the
	// guard checks to land mid-clone.
	origin := makeOrigin(t, e.home, "main", 1)
	blob := make([]byte, 32<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(filepath.Join(origin, "blob.bin"), blob, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	gitCmd(t, e.home, origin, "add", ".")
	gitCmd(t, e.home, origin, "commit", "-q", "-m", "blob")

	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// While the clone job runs: refuse without force.
	if err := e.svc.Delete(t.Context(), repo.ID, false); !isErr(err, ErrCloneInProgress) {
		t.Fatalf("Delete during clone = %v, want ErrCloneInProgress", err)
	}

	// Force: cancel the job, remove row and dir.
	if err := e.svc.Delete(t.Context(), repo.ID, true); err != nil {
		t.Fatalf("force Delete: %v", err)
	}
	if _, err := e.st.RepoByID(t.Context(), repo.ID); !isErr(err, store.ErrNotFound) {
		t.Fatalf("repo row after force delete: %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(e.reposDir, repo.ID+".git")); !os.IsNotExist(err) {
		t.Errorf("bare dir survived force delete: %v", err)
	}

	// Deleting a ready repo needs no force.
	small := makeOrigin(t, e.home, "main", 1)
	repo2, err := e.svc.Add(t.Context(), AddParams{RemoteURL: small, Name: "small"})
	if err != nil {
		t.Fatalf("Add small: %v", err)
	}
	e.waitCloneStatus(t, repo2.ID, store.CloneStatusReady)
	if err := e.svc.Delete(t.Context(), repo2.ID, false); err != nil {
		t.Fatalf("Delete ready repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.reposDir, repo2.ID+".git")); !os.IsNotExist(err) {
		t.Errorf("bare dir survived delete: %v", err)
	}

	// Unknown id → ErrNotFound.
	if err := e.svc.Delete(t.Context(), "repo_missing", false); !isErr(err, store.ErrNotFound) {
		t.Fatalf("Delete unknown = %v, want ErrNotFound", err)
	}
}

func TestStartupHeal(t *testing.T) {
	e := newTestEnv(t)
	log := collectEvents(t, e.bus)

	// Two rows stuck in 'cloning': one with a healthy bare dir, one with
	// nothing on disk.
	mkRepo := func(name string) store.Repo {
		r := store.Repo{
			ID: ids.NewID("repo"), Name: name, RemoteURL: "/tmp/" + name,
			TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none",
			DefaultBranch: "main", Provider: DefaultProvider,
			AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
			CloneStatus: store.CloneStatusCloning, CreatedAt: time.Now(),
		}
		created, err := e.st.CreateRepo(t.Context(), r)
		if err != nil {
			t.Fatalf("CreateRepo: %v", err)
		}
		return created
	}
	// The healthy bare repo's own HEAD points at master — heal-to-ready
	// must re-derive default_branch from it, not keep the provisional
	// "main" the row was created with.
	healthy := mkRepo("healthy")
	broken := mkRepo("broken")
	gitCmd(t, e.home, "", "init", "-q", "--bare", "-b", "master", filepath.Join(e.reposDir, healthy.ID+".git"))

	// Orphaned runtime credential files (no live runs exist in M2, per-op
	// <credID>.<opID>.<suffix> names) plus a known_hosts file CleanupAll
	// must never touch.
	orphanKey := filepath.Join(e.runtime, "cred_dead.repo_dead.key")
	orphanAskpass := filepath.Join(e.runtime, "cred_dead.repo_dead.askpass")
	knownHosts := filepath.Join(e.runtime, "known_hosts")
	for _, f := range []string{orphanKey, orphanAskpass, knownHosts} {
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	// Pre-restart orphans are old; backdate them past the vault's in-flight
	// age guard (>5min) so heal's keep-set actually reaps them.
	old := time.Now().Add(-10 * time.Minute)
	for _, f := range []string{orphanKey, orphanAskpass} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", f, err)
		}
	}

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal: %v", err)
	}

	got, err := e.st.RepoByID(t.Context(), healthy.ID)
	if err != nil {
		t.Fatalf("RepoByID: %v", err)
	}
	if got.CloneStatus != store.CloneStatusReady {
		t.Errorf("healthy repo status = %q, want ready", got.CloneStatus)
	}
	if got.DefaultBranch != "master" {
		t.Errorf("healed default_branch = %q, want master re-derived from the bare HEAD (not provisional main)", got.DefaultBranch)
	}
	got, err = e.st.RepoByID(t.Context(), broken.ID)
	if err != nil {
		t.Fatalf("RepoByID: %v", err)
	}
	if got.CloneStatus != store.CloneStatusError {
		t.Errorf("broken repo status = %q, want error", got.CloneStatus)
	}
	if got.CloneError == nil || *got.CloneError != store.CloneErrorInterrupted {
		t.Errorf("broken repo clone_error = %v, want %q", got.CloneError, store.CloneErrorInterrupted)
	}

	for _, f := range []string{orphanKey, orphanAskpass} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("orphaned runtime file %s survived heal", f)
		}
	}
	if _, err := os.Stat(knownHosts); err != nil {
		t.Errorf("known_hosts was removed by heal: %v", err)
	}

	// Both healed repos published repo.changed.
	waitFor(t, "heal events", func() bool {
		seen := map[string]bool{}
		for _, ev := range log.snapshot() {
			if p, ok := ev.Payload.(repoChangedPayload); ok {
				seen[p.RepoID] = true
			}
		}
		return seen[healthy.ID] && seen[broken.ID]
	})
}

// TestStartupHealProbeIsAnchored: a leftover EMPTY bare dir (daemon killed
// between mkdir and git init) must heal to 'error' even when the repos dir
// sits inside a git checkout and the daemon environment carries GIT_DIR —
// an unanchored `git rev-parse --git-dir` would resolve the ancestor .git
// (or $GIT_DIR) and mark the garbage dir 'ready', which Retry (error-only)
// could never recover. A real bare repo in the same nested location must
// still probe true.
func TestStartupHealProbeIsAnchored(t *testing.T) {
	e := newTestEnv(t)

	// Make the repos dir itself a git checkout: every bare dir under it now
	// has an ancestor .git for an unanchored rev-parse to latch onto.
	gitCmd(t, e.home, "", "init", "-q", e.reposDir)
	// And poison the daemon environment the way a dev shell might.
	t.Setenv("GIT_DIR", filepath.Join(e.reposDir, ".git"))

	mkRepo := func(name string) store.Repo {
		r := store.Repo{
			ID: ids.NewID("repo"), Name: name, RemoteURL: "/tmp/" + name,
			TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none",
			DefaultBranch: "main", Provider: DefaultProvider,
			AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
			CloneStatus: store.CloneStatusCloning, CreatedAt: time.Now(),
		}
		created, err := e.st.CreateRepo(t.Context(), r)
		if err != nil {
			t.Fatalf("CreateRepo: %v", err)
		}
		return created
	}
	empty := mkRepo("nested-empty")
	if err := os.MkdirAll(filepath.Join(e.reposDir, empty.ID+".git"), 0o755); err != nil {
		t.Fatalf("mkdir empty bare dir: %v", err)
	}
	real := mkRepo("nested-real")
	gitCmd(t, e.home, "", "init", "-q", "--bare", filepath.Join(e.reposDir, real.ID+".git"))

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal: %v", err)
	}

	got, err := e.st.RepoByID(t.Context(), empty.ID)
	if err != nil {
		t.Fatalf("RepoByID: %v", err)
	}
	if got.CloneStatus != store.CloneStatusError {
		t.Errorf("empty nested dir healed to %q, want error (probe must not resolve the ancestor .git or $GIT_DIR)", got.CloneStatus)
	}
	got, err = e.st.RepoByID(t.Context(), real.ID)
	if err != nil {
		t.Fatalf("RepoByID: %v", err)
	}
	if got.CloneStatus != store.CloneStatusReady {
		t.Errorf("real nested bare repo healed to %q, want ready", got.CloneStatus)
	}
}

// TestRetryConcurrentSingleFlight: two Retries racing on the same failed
// repo must resolve to exactly one clone job — the loser is refused, never
// allowed to RemoveAll the directory the winner's live clone is writing
// into, and never able to strand the repo in 'cloning' with no job.
func TestRetryConcurrentSingleFlight(t *testing.T) {
	e := newTestEnv(t)

	// A slow origin (~32MB incompressible blob) keeps the winner's clone
	// running while the loser lands.
	origin := makeOrigin(t, e.home, "main", 1)
	blob := make([]byte, 32<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(filepath.Join(origin, "blob.bin"), blob, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	gitCmd(t, e.home, origin, "add", ".")
	gitCmd(t, e.home, origin, "commit", "-q", "-m", "blob")

	// A repo already in clone_status 'error' (the store persists rows as
	// given), pointing at the now-valid origin.
	failed := store.Repo{
		ID: ids.NewID("repo"), Name: "raceable", RemoteURL: "file://" + origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none",
		DefaultBranch: "main", Provider: DefaultProvider,
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusError, CloneError: ptr("first clone failed"),
		CreatedAt: time.Now(),
	}
	if _, err := e.st.CreateRepo(t.Context(), failed); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e.svc.Retry(context.Background(), failed.ID)
		}()
	}
	wg.Wait()

	var won, refused int
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case isErr(err, ErrCloneInProgress) || isErr(err, ErrCloneNotFailed):
			refused++ // refused cleanly — the winner's job owns the repo
		default:
			t.Fatalf("unexpected Retry error: %v", err)
		}
	}
	if won != 1 || refused != 1 {
		t.Fatalf("concurrent Retry = %d won / %d refused, want exactly 1/1", won, refused)
	}

	// The winner's clone completes on an intact directory: no concurrent
	// RemoveAll corrupted it, and the repo is not stranded in 'cloning'.
	e.waitCloneStatus(t, failed.ID, store.CloneStatusReady)
	bare := filepath.Join(e.reposDir, failed.ID+".git")
	if got := gitCmd(t, e.home, bare, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("bare dir after concurrent retry is not a bare repository: %q", got)
	}
}

// TestForceDeleteSurvivesClientDisconnect: once force-delete has cancelled
// the clone job it is past the point of no return — the client hanging up
// mid-request must not abort the teardown, or the repo is stranded in
// clone_status 'cloning' with no job (runClone's cancelled path writes no
// status) until a restart heals it.
func TestForceDeleteSurvivesClientDisconnect(t *testing.T) {
	e := newTestEnv(t)

	repo := store.Repo{
		ID: ids.NewID("repo"), Name: "vanishing", RemoteURL: "/tmp/vanishing",
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none",
		DefaultBranch: "main", Provider: DefaultProvider,
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusCloning, CreatedAt: time.Now(),
	}
	if _, err := e.st.CreateRepo(t.Context(), repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	// A fake registered clone job the test controls — a deterministic
	// stand-in for a running clone goroutine mid-cancellation.
	var cancelled atomic.Bool
	job := &cloneJob{cancel: func() { cancelled.Store(true) }, done: make(chan struct{})}
	e.svc.mu.Lock()
	e.svc.jobs[repo.ID] = job
	e.svc.mu.Unlock()
	t.Cleanup(func() { // runClone would deregister; the fake must too
		e.svc.mu.Lock()
		delete(e.svc.jobs, repo.ID)
		e.svc.mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- e.svc.Delete(ctx, repo.ID, true) }()

	// The client disconnects while Delete awaits the cancelled job.
	waitFor(t, "force delete cancelled the job", cancelled.Load)
	cancel()
	select {
	case err := <-errCh:
		t.Fatalf("Delete returned before the job finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The job terminates; the teardown must now complete on the dead ctx.
	close(job.done)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("force Delete after client disconnect: %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Delete did not finish after the job ended")
	}
	if _, err := e.st.RepoByID(context.Background(), repo.ID); !isErr(err, store.ErrNotFound) {
		t.Fatalf("repo row after disconnected force delete: %v, want ErrNotFound", err)
	}
}

// TestAddForgeBindingUndetectedHostWithCredential pins the ADR-0015 unlock: an
// explicit forge binding on an UNRECOGNIZED host (forge_kind 'none') succeeds
// with a forge credential alone — the detected-host requirement is gone, so a
// second Forgejo instance or a GitHub Enterprise host binds without a code
// change. (The clone job Add starts async will fail against the fake remote;
// the row is created before that, which is all this asserts.)
func TestAddForgeBindingUndetectedHostWithCredential(t *testing.T) {
	e := newTestEnv(t)
	forgeCred := e.createCredential(t, "ghe", vault.KindForgeToken,
		vault.ForgeTokenPayload{Host: "ghe.example.com/api/v3", Token: "tk", Forge: vault.ForgeGitHub})
	repo, err := e.svc.Add(t.Context(), AddParams{
		RemoteURL:         "git@ghe.example.com:team/proj.git",
		TrackerBinding:    "forge",
		ForgeCredentialID: &forgeCred.ID,
	})
	if err != nil {
		t.Fatalf("Add forge binding on undetected host: %v", err)
	}
	if repo.TrackerBinding != store.TrackerBindingForge {
		t.Errorf("tracker_binding = %q, want forge", repo.TrackerBinding)
	}
	if repo.ForgeKind != "none" {
		t.Errorf("forge_kind = %q, want none (the host is unrecognized)", repo.ForgeKind)
	}
}

// TestUpdateSettingsForgeBindingRequiresForgeCredential pins the design
// §3a cross-field invariant on PATCH: a repo can hold tracker_binding
// 'forge' only while a forge credential is attached.
func TestUpdateSettingsForgeBindingRequiresForgeCredential(t *testing.T) {
	e := newTestEnv(t)
	forgeCred := e.createCredential(t, "forge", vault.KindForgeToken,
		vault.ForgeTokenPayload{Host: "git.cloonar.com", Token: "tk"})

	// A forge-host repo that auto-resolved to builtin (no credential).
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "forgejo@git.cloonar.com:me/proj.git"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if repo.TrackerBinding != store.TrackerBindingBuiltin {
		t.Fatalf("auto binding without credential = %q, want builtin", repo.TrackerBinding)
	}

	// Flipping to forge without a credential → 400.
	_, err = e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		TrackerBinding: store.Set(store.TrackerBindingForge),
	})
	var bad *BadRequestError
	if err == nil || !asBadRequest(err, &bad) || !strings.Contains(bad.Error(), "requires a forge_token credential") {
		t.Fatalf("flip to forge without credential = %v, want 400 naming the forge_token credential", err)
	}

	// Attaching the credential in the same PATCH → OK.
	updated, err := e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		TrackerBinding:    store.Set(store.TrackerBindingForge),
		ForgeCredentialID: store.Set(&forgeCred.ID),
	})
	if err != nil {
		t.Fatalf("flip to forge with credential: %v", err)
	}
	if updated.TrackerBinding != store.TrackerBindingForge {
		t.Errorf("binding = %q, want forge", updated.TrackerBinding)
	}

	// Clearing the credential while forge-bound → 400.
	_, err = e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		ForgeCredentialID: store.Set[*string](nil),
	})
	if err == nil || !asBadRequest(err, &bad) || !strings.Contains(bad.Error(), "requires a forge_token credential") {
		t.Fatalf("clear credential while forge-bound = %v, want 400", err)
	}

	// Clearing it together with the flip back to builtin → OK.
	updated, err = e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		TrackerBinding:    store.Set(store.TrackerBindingBuiltin),
		ForgeCredentialID: store.Set[*string](nil),
	})
	if err != nil {
		t.Fatalf("flip to builtin clearing credential: %v", err)
	}
	if updated.TrackerBinding != store.TrackerBindingBuiltin || updated.ForgeCredentialID != nil {
		t.Errorf("after flip back = %q/%v, want builtin/nil", updated.TrackerBinding, updated.ForgeCredentialID)
	}
}

func TestUpdateSettingsValidationAndEvents(t *testing.T) {
	e := newTestEnv(t)
	origin := makeOrigin(t, e.home, "main", 1)
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: origin})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	log := collectEvents(t, e.bus)

	// Grammar violations (design §4a) are 400s.
	for _, u := range []store.RepoSettingsUpdate{
		{AFKBranchPattern: store.Set("no-token")},
		{AFKBranchPattern: store.Set("lab/<N>")}, // overlaps existing manual prefix lab/
		{ManualBranchPrefix: store.Set("")},
		{BudgetMinutes: store.Set(ptr(0))},
		{MaxInstancesOverride: store.Set(ptr(-1))},
		{TrackerBinding: store.Set("forge")}, // no forge credential attached (ADR-0015: the detected-host gate is gone; the credential gate remains)
		{TrackerBinding: store.Set("auto")},  // not a stored binding value
		{DefaultBranch: store.Set("  ")},
		{Name: store.Set("   ")}, // whitespace-only sanitizes to ""
	} {
		_, err := e.svc.UpdateSettings(t.Context(), repo.ID, u)
		var bad *BadRequestError
		if err == nil || !asBadRequest(err, &bad) {
			t.Errorf("UpdateSettings(%+v) error = %v, want BadRequestError", u, err)
		}
	}

	// A valid pair change + overrides; incogni flip must NOT rewrite patterns.
	updated, err := e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		AFKBranchPattern:   store.Set("issue-<N>"),
		ManualBranchPrefix: store.Set("wip/"),
		BudgetMinutes:      store.Set(ptr(45)),
		Incogni:            store.Set(true),
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if updated.AFKBranchPattern != "issue-<N>" || updated.ManualBranchPrefix != "wip/" {
		t.Errorf("patterns = %q/%q", updated.AFKBranchPattern, updated.ManualBranchPrefix)
	}
	if updated.BudgetMinutes == nil || *updated.BudgetMinutes != 45 {
		t.Errorf("budget_minutes = %v, want 45", updated.BudgetMinutes)
	}

	updated, err = e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(false),
	})
	if err != nil {
		t.Fatalf("UpdateSettings incogni flip: %v", err)
	}
	if updated.AFKBranchPattern != "issue-<N>" || updated.ManualBranchPrefix != "wip/" {
		t.Error("incogni PATCH silently rewrote branch patterns")
	}

	// Clearing an override with an explicit nil.
	updated, err = e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		BudgetMinutes: store.Set[*int](nil),
	})
	if err != nil {
		t.Fatalf("UpdateSettings clear budget: %v", err)
	}
	if updated.BudgetMinutes != nil {
		t.Errorf("budget_minutes = %v, want nil", updated.BudgetMinutes)
	}

	waitFor(t, "repo.changed after PATCH", func() bool {
		for _, ev := range log.snapshot() {
			if p, ok := ev.Payload.(repoChangedPayload); ok && p.RepoID == repo.ID {
				return true
			}
		}
		return false
	})
}

func ptr[T any](v T) *T { return &v }

func asBadRequest(err error, target **BadRequestError) bool { return errors.As(err, target) }

func isErr(err, target error) bool { return errors.Is(err, target) }
