package reposvc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// The Warpgate SSH-bastion identity lifecycle hooks (ADR-0068, issue #39),
// mirroring onecli_test.go's structure exactly: eager creation in Add, the
// startup convergence (which also refreshes the repo_ssh_targets cache), the
// delete in Delete. All three are best-effort over a seam that is nil on most
// labs, so what these tests pin is as much what does NOT happen — no calls
// when unconfigured, no error when the sidecar is down, no delete ever issued
// from startup — as what does.

// identityCall is one recorded EnsureRepoIdentity: the repo id and the name
// handed along, kept apart so a crossed pair — the failure mode #35 and this
// mirror both exist to prevent — is visible in a failing assertion.
type identityCall struct{ repoID, repoName string }

// roleID derives a deterministic, distinct role id per repo id, standing in
// for Warpgate's own opaque ids — deterministic so a test can script
// targetsByRole against it before Add/repoRow ever runs.
func roleID(repoID string) string { return "role_" + repoID }

// stubIdentities is a hand-written WarpgateIdentities double. It records
// every call in order, can be scripted to fail per method, and answers
// RoleSSHTargets from a per-role-id table — so all three hooks and the
// startup cache refresh are driven with no HTTP and no sidecar. Locked for
// the same reason stubAgents is: Add's hook runs on the caller's goroutine
// while a clone job runs on another — the recorder must not be the thing
// that makes a test flaky under -race.
type stubIdentities struct {
	mu      sync.Mutex
	ensured []identityCall
	deleted []string

	// ensureErr/deleteErr fail every call when set — the sidecar-down shape
	// every best-effort path here has to survive.
	ensureErr error
	deleteErr error
	// deleteFound is DeleteRepoIdentity's bool answer. False is the ordinary
	// "nothing carried that slug" outcome (a repo predating Warpgate), which
	// must never be treated as a failure.
	deleteFound bool

	// targetsByRole scripts RoleSSHTargets's answer per role id (see roleID).
	// roleTargetsErr, when set, fails every RoleSSHTargets call regardless of
	// role id — the sidecar-down shape the startup reconcile must stop the
	// sweep on, exactly like ensureErr does for EnsureRepoIdentity.
	targetsByRole  map[string][]warpgate.Target
	roleTargetsErr error
}

func (s *stubIdentities) EnsureRepoIdentity(_ context.Context, repoID, repoName string) (warpgate.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensured = append(s.ensured, identityCall{repoID: repoID, repoName: repoName})
	if s.ensureErr != nil {
		return warpgate.Identity{}, s.ensureErr
	}
	slug := warpgate.RepoSlug(repoID)
	return warpgate.Identity{
		User: warpgate.User{ID: "usr_" + repoID, Username: slug, Description: repoName},
		Role: warpgate.Role{ID: roleID(repoID), Name: slug, Description: repoName},
	}, nil
}

func (s *stubIdentities) DeleteRepoIdentity(_ context.Context, repoID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, repoID)
	if s.deleteErr != nil {
		return false, s.deleteErr
	}
	return s.deleteFound, nil
}

func (s *stubIdentities) RoleSSHTargets(_ context.Context, roleID string) ([]warpgate.Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roleTargetsErr != nil {
		return nil, s.roleTargetsErr
	}
	return s.targetsByRole[roleID], nil
}

func (s *stubIdentities) ensureCalls() []identityCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]identityCall(nil), s.ensured...)
}

func (s *stubIdentities) deleteCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// warpgateEnv is a Service with just the seam these hooks touch (no provider
// registry, no pinner — the identity lifecycle is orthogonal to both),
// mirroring agentEnv in onecli_test.go.
type warpgateEnv struct {
	svc  *Service
	st   *store.Store
	home string
}

// newWarpgateEnv builds the Service with exactly the seam the caller passes:
// the stub for a configured lab, an untyped nil for the unconfigured one. As
// in newAgentEnv, the nil has to be untyped AT THE CALL SITE — a
// (*stubIdentities)(nil) handed through this interface parameter would be a
// non-nil interface and would defeat the very gate the no-op tests are about.
func newWarpgateEnv(t *testing.T, identities WarpgateIdentities) *warpgateEnv {
	t.Helper()
	testutil.RequireTool(t, "git")

	st := testutil.TempStore(t)
	home := t.TempDir()
	stateDir := t.TempDir()
	mat, err := vault.NewMaterializer(filepath.Join(stateDir, "runtime"))
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	v, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	svc, err := New(Options{
		Store: st, Vault: v, Materializer: mat, Git: gitx.New("git"), Bus: events.NewBus(),
		Logger: logx.New(io.Discard), ReposDir: filepath.Join(stateDir, "repos"),
		GitEnv:   testutil.HermeticGitEnv(home),
		Warpgate: identities,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// LIFO: cancel and drain Add's clone jobs before the temp dirs and the
	// store go away under them.
	t.Cleanup(svc.Close)
	return &warpgateEnv{svc: svc, st: st, home: home}
}

// repoRow inserts a ready repo straight into the store — the startup and
// delete hooks act on rows, never on the clone.
func (e *warpgateEnv) repoRow(t *testing.T, name string) store.Repo {
	t.Helper()
	r, err := e.st.CreateRepo(context.Background(), store.Repo{
		ID: ids.NewID("repo"), Name: name, RemoteURL: "/tmp/" + name,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return r
}

// addRepo drives the real Add over a fixture origin, so the hook is exercised
// where it actually sits — after the row, the publish and the clone job.
func (e *warpgateEnv) addRepo(t *testing.T, name string) store.Repo {
	t.Helper()
	origin := makeOrigin(t, e.home, "main", 1)
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin, Name: name})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return repo
}

func TestAddEnsuresWarpgateIdentity(t *testing.T) {
	stub := &stubIdentities{}
	e := newWarpgateEnv(t, stub)

	repo := e.addRepo(t, "my-project")

	calls := stub.ensureCalls()
	if len(calls) != 1 {
		t.Fatalf("EnsureRepoIdentity called %d times, want exactly 1: %+v", len(calls), calls)
	}
	// Unlike OneCLIAgents, EnsureRepoIdentity takes the RAW store id and
	// derives the slug itself — but the two arguments are still not
	// interchangeable: crossing them would hand Warpgate a user/role
	// description that is a store id instead of the repo's display name.
	if calls[0].repoID != repo.ID {
		t.Errorf("repoID = %q, want the raw store id %q", calls[0].repoID, repo.ID)
	}
	if calls[0].repoName != repo.Name {
		t.Errorf("repoName = %q, want the repo name %q", calls[0].repoName, repo.Name)
	}
	if calls[0].repoName == repo.ID {
		t.Errorf("repoName = %q, the store id — unreadable as a Warpgate user/role description", calls[0].repoName)
	}
}

func TestAddWithoutWarpgateIsSilentNoOp(t *testing.T) {
	// The stub is built but NOT wired: an unconfigured lab must not reach a
	// seam it does not have, and this is the assertion that says so.
	stub := &stubIdentities{}
	e := newWarpgateEnv(t, nil)

	repo := e.addRepo(t, "my-project")

	if _, err := e.st.RepoByID(t.Context(), repo.ID); err != nil {
		t.Fatalf("repo row missing after Add on an unconfigured lab: %v", err)
	}
	if n := len(stub.ensureCalls()); n != 0 {
		t.Errorf("EnsureRepoIdentity called %d times with no Warpgate configured", n)
	}
}

func TestAddSurvivesWarpgateFailure(t *testing.T) {
	// The sidecar is down. Repo creation must still succeed: the SPAWN is the
	// fail-closed enforcement point (ADR-0068), not the repo row.
	stub := &stubIdentities{ensureErr: errors.New("connection refused")}
	e := newWarpgateEnv(t, stub)

	repo := e.addRepo(t, "my-project")

	if repo.ID == "" || repo.Name != "my-project" {
		t.Fatalf("Add returned %+v, want the created repo", repo)
	}
	if _, err := e.st.RepoByID(t.Context(), repo.ID); err != nil {
		t.Fatalf("repo row missing after a failed identity ensure: %v", err)
	}
	if n := len(stub.ensureCalls()); n != 1 {
		t.Errorf("EnsureRepoIdentity called %d times, want 1", n)
	}
}

func TestDeleteRemovesWarpgateIdentity(t *testing.T) {
	stub := &stubIdentities{deleteFound: true}
	e := newWarpgateEnv(t, stub)
	repo := e.repoRow(t, "doomed")

	if err := e.svc.Delete(t.Context(), repo.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	calls := stub.deleteCalls()
	if len(calls) != 1 {
		t.Fatalf("DeleteRepoIdentity called %d times, want exactly 1: %v", len(calls), calls)
	}
	if calls[0] != repo.ID {
		t.Errorf("repoID = %q, want the raw store id %q", calls[0], repo.ID)
	}
	if _, err := e.st.RepoByID(t.Context(), repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo row survived Delete: %v", err)
	}
}

func TestDeleteOrphansIdentityOnWarpgateFailure(t *testing.T) {
	// Warn-and-orphan: a leaked identity is the status quo of every repo
	// deleted before this hook existed, so it can never hold the repo back.
	stub := &stubIdentities{deleteErr: errors.New("connection refused")}
	e := newWarpgateEnv(t, stub)
	repo := e.repoRow(t, "doomed")

	if err := e.svc.Delete(t.Context(), repo.ID, false); err != nil {
		t.Fatalf("Delete err = %v, want nil despite the failing identity delete", err)
	}
	if n := len(stub.deleteCalls()); n != 1 {
		t.Errorf("DeleteRepoIdentity called %d times, want 1", n)
	}
	if _, err := e.st.RepoByID(t.Context(), repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo row survived Delete: %v", err)
	}
}

func TestDeleteWithNoIdentityToRemoveSucceeds(t *testing.T) {
	// (false, nil): nothing carried that slug — a repo created before
	// Warpgate was configured. An ordinary outcome, not a failure.
	stub := &stubIdentities{deleteFound: false}
	e := newWarpgateEnv(t, stub)
	repo := e.repoRow(t, "never-had-one")

	if err := e.svc.Delete(t.Context(), repo.ID, false); err != nil {
		t.Fatalf("Delete err = %v, want nil when no identity carried the slug", err)
	}
	if n := len(stub.deleteCalls()); n != 1 {
		t.Errorf("DeleteRepoIdentity called %d times, want 1", n)
	}
}

func TestDeleteWithoutWarpgateIsSilentNoOp(t *testing.T) {
	stub := &stubIdentities{}
	e := newWarpgateEnv(t, nil)
	repo := e.repoRow(t, "doomed")

	if err := e.svc.Delete(t.Context(), repo.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := len(stub.deleteCalls()); n != 0 {
		t.Errorf("DeleteRepoIdentity called %d times with no Warpgate configured", n)
	}
}

func TestStartupHealEnsuresEveryRepoIdentityAndCachesTargets(t *testing.T) {
	stub := &stubIdentities{targetsByRole: map[string][]warpgate.Target{}}
	e := newWarpgateEnv(t, stub)
	repos := map[string]store.Repo{}
	for i, name := range []string{"alpha", "beta", "gamma"} {
		repo := e.repoRow(t, name)
		repos[name] = repo
		stub.targetsByRole[roleID(repo.ID)] = []warpgate.Target{
			{ID: fmt.Sprintf("tgt-%d-a", i), Name: "staging"},
			{ID: fmt.Sprintf("tgt-%d-b", i), Name: "build"},
		}
	}

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal: %v", err)
	}

	calls := stub.ensureCalls()
	if len(calls) != len(repos) {
		t.Fatalf("EnsureRepoIdentity called %d times, want one per repo (%d): %+v", len(calls), len(repos), calls)
	}
	// Compared as a set: the sweep's order is the store's, and pinning it
	// here would test store.Repos rather than the hook. What matters is that
	// each repo contributed its OWN (id, name) pair.
	got := map[string]string{}
	for _, c := range calls {
		got[c.repoID] = c.repoName
	}
	for name, repo := range repos {
		if got[repo.ID] != name {
			t.Errorf("identity %q ensured with name %q, want %q", repo.ID, got[repo.ID], name)
		}
	}

	// Each repo's cache now holds exactly the targets its role's fresh read
	// answered (ADR-0068 decision 1: every full read replaces the cache
	// wholesale) — this is also how a target assigned directly in Warpgate's
	// own UI would be picked up.
	for name, repo := range repos {
		cached, err := e.st.RepoSSHTargets(t.Context(), repo.ID)
		if err != nil {
			t.Fatalf("RepoSSHTargets(%s): %v", name, err)
		}
		want := stub.targetsByRole[roleID(repo.ID)]
		if len(cached) != len(want) {
			t.Fatalf("repo %s: cached %d targets, want %d: %+v", name, len(cached), len(want), cached)
		}
		byID := map[string]string{}
		for _, c := range cached {
			byID[c.ID] = c.Name
		}
		for _, w := range want {
			if byID[w.ID] != w.Name {
				t.Errorf("repo %s: cached target %s = %q, want %q", name, w.ID, byID[w.ID], w.Name)
			}
		}
	}

	// One-way: the startup sweep must never delete a Warpgate identity.
	if n := len(stub.deleteCalls()); n != 0 {
		t.Errorf("DeleteRepoIdentity called %d times during startup heal, want 0 — the sweep never deletes", n)
	}
}

func TestStartupHealReplacesStaleCachedTargets(t *testing.T) {
	stub := &stubIdentities{}
	e := newWarpgateEnv(t, stub)
	repo := e.repoRow(t, "alpha")

	// Seed a stale row the picker (or an earlier boot) left behind, naming a
	// target Warpgate's role no longer carries.
	if err := e.st.AddRepoSSHTarget(t.Context(), repo.ID, store.SSHTarget{ID: "stale-target", Name: "decommissioned"}); err != nil {
		t.Fatalf("AddRepoSSHTarget (seed): %v", err)
	}
	stub.targetsByRole = map[string][]warpgate.Target{
		roleID(repo.ID): {{ID: "fresh-target", Name: "staging"}},
	}

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal: %v", err)
	}

	cached, err := e.st.RepoSSHTargets(t.Context(), repo.ID)
	if err != nil {
		t.Fatalf("RepoSSHTargets: %v", err)
	}
	if len(cached) != 1 || cached[0].ID != "fresh-target" {
		t.Fatalf("cached targets = %+v, want exactly the fresh read (the stale row replaced, not merged)", cached)
	}
}

func TestStartupHealStopsAtFirstWarpgateEnsureFailure(t *testing.T) {
	// A sidecar that is not up yet is the EXPECTED case (same compose stack),
	// so boot must survive it — and one outage must be one warning, not one
	// per repo, which is why the loop stops instead of grinding through the
	// rest.
	stub := &stubIdentities{ensureErr: errors.New("connection refused")}
	e := newWarpgateEnv(t, stub)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		e.repoRow(t, name)
	}

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal err = %v, want nil — this error return fails lab boot", err)
	}
	if n := len(stub.ensureCalls()); n != 1 {
		t.Errorf("EnsureRepoIdentity called %d times, want 1 (the loop must stop at the first failure)", n)
	}
}

func TestStartupHealStopsAtFirstRoleSSHTargetsFailure(t *testing.T) {
	// The identity ensure itself succeeds for every repo (no ensureErr
	// scripted) — only the subsequent targets read fails — and that must stop
	// the sweep exactly like an EnsureRepoIdentity failure does: it is the
	// same sidecar-level error waiting for every remaining repo.
	stub := &stubIdentities{roleTargetsErr: errors.New("connection refused")}
	e := newWarpgateEnv(t, stub)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		e.repoRow(t, name)
	}

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal err = %v, want nil — this error return fails lab boot", err)
	}
	if n := len(stub.ensureCalls()); n != 1 {
		t.Errorf("EnsureRepoIdentity called %d times, want 1 (the loop must stop at the first RoleSSHTargets failure)", n)
	}
}

func TestStartupHealSurvivesStoreCacheFailure(t *testing.T) {
	// A store-side failure writing the cache is a DIFFERENT kind of failure
	// from a Warpgate error: Warpgate answered fine, lab's own write did not,
	// so it must not stop the sweep — every remaining repo's own identity and
	// targets are still reconciled. An empty target name fails the store's
	// own validation (a NOT NULL column) before anything is written, which
	// stands in for any store-side failure without needing a live DB fault.
	stub := &stubIdentities{}
	e := newWarpgateEnv(t, stub)
	bad := e.repoRow(t, "bad")
	good := e.repoRow(t, "good")
	stub.targetsByRole = map[string][]warpgate.Target{
		roleID(bad.ID):  {{ID: "tgt-bad", Name: ""}},
		roleID(good.ID): {{ID: "tgt-good", Name: "staging"}},
	}

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal: %v", err)
	}

	calls := stub.ensureCalls()
	if len(calls) != 2 {
		t.Fatalf("EnsureRepoIdentity called %d times, want 2 (a store cache error must not stop the sweep): %+v", len(calls), calls)
	}

	cached, err := e.st.RepoSSHTargets(t.Context(), good.ID)
	if err != nil {
		t.Fatalf("RepoSSHTargets(good): %v", err)
	}
	if len(cached) != 1 || cached[0].ID != "tgt-good" {
		t.Fatalf("good repo cache = %+v, want the fresh read to have landed despite the other repo's store error", cached)
	}
}

func TestStartupHealWithoutWarpgateIsSilentNoOp(t *testing.T) {
	stub := &stubIdentities{}
	e := newWarpgateEnv(t, nil)
	e.repoRow(t, "alpha")

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal: %v", err)
	}
	if n := len(stub.ensureCalls()); n != 0 {
		t.Errorf("EnsureRepoIdentity called %d times with no Warpgate configured", n)
	}
}
