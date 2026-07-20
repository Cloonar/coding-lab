package httpapi

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

const repoWaitTimeout = 30 * time.Second

type repoTestServer struct {
	*testServer
	svc      *reposvc.Service
	reposDir string
	runtime  string
	home     string
}

// newRepoTestServer builds a logged-in test server with the full M2 stack:
// vault + materializer + gitx engine + reposvc, hermetic git env. The
// two-provider registry (claude-code + fake-b) backs the repo provider
// validation (issue #66).
func newRepoTestServer(t *testing.T) *repoTestServer {
	t.Helper()
	testutil.RequireTool(t, "git")

	v, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	home := t.TempDir()
	stateDir := t.TempDir()
	runtime := filepath.Join(stateDir, "runtime")
	reposDir := filepath.Join(stateDir, "repos")
	mat, err := vault.NewMaterializer(runtime)
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	second := providertest.New()
	second.SetID("fake-b")
	reg, err := provider.NewRegistry(providertest.New(), second)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	var svc *reposvc.Service
	x := newTestServer(t, func(o *Options) {
		svc, err = reposvc.New(reposvc.Options{
			Store:        o.Store,
			Vault:        v,
			Materializer: mat,
			Git:          gitx.New("git"),
			Bus:          o.Bus,
			Logger:       logx.New(io.Discard),
			ReposDir:     reposDir,
			GitEnv:       testutil.HermeticGitEnv(home),
			Providers:    reg,
		})
		if err != nil {
			t.Fatalf("reposvc.New: %v", err)
		}
		o.Vault = v
		o.Repos = svc
		o.Providers = reg
	})
	t.Cleanup(svc.Close)
	x.setup("op", "password123")
	return &repoTestServer{testServer: x, svc: svc, reposDir: reposDir, runtime: runtime, home: home}
}

// repoGitCmd runs one git command with the hermetic env.
func repoGitCmd(t *testing.T, home, dir string, args ...string) string {
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

// makeRepoOrigin builds a fixture origin on the given branch.
func makeRepoOrigin(t *testing.T, home, branch string, commits int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	repoGitCmd(t, home, "", "init", "-q", "-b", branch, dir)
	for i := range commits {
		name := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(name, fmt.Appendf(nil, "content %d\n", i), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
		repoGitCmd(t, home, dir, "add", ".")
		repoGitCmd(t, home, dir, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}
	return dir
}

// busLog records bus events (the SSE feed's source) for assertions.
type busLog struct {
	mu     sync.Mutex
	events []events.Event
}

func recordBus(t *testing.T, bus *events.Bus) *busLog {
	t.Helper()
	ch, cancel := bus.Subscribe(context.Background())
	t.Cleanup(cancel)
	l := &busLog{}
	go func() {
		for e := range ch {
			l.mu.Lock()
			l.events = append(l.events, e)
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *busLog) snapshot() []events.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]events.Event(nil), l.events...)
}

func (x *repoTestServer) getRepo(t *testing.T, id string) map[string]any {
	t.Helper()
	resp := x.do("GET", "/api/v1/repos/"+id, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	return decodeBody(t, resp)
}

func (x *repoTestServer) waitCloneStatus(t *testing.T, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(repoWaitTimeout)
	for {
		repo := x.getRepo(t, id)
		if repo["clone_status"] == want {
			return repo
		}
		if time.Now().After(deadline) {
			t.Fatalf("repo %s clone_status = %v, want %s within %s", id, repo["clone_status"], want, repoWaitTimeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRepoCreateCloneLifecycleAndEvents(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	log := recordBus(t, x.bus)
	origin := makeRepoOrigin(t, x.home, "trunk", 3)

	resp := x.do("POST", "/api/v1/repos", map[string]any{"remote_url": "file://" + origin}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo := decodeBody(t, resp)

	// Pinned defaults on the create response.
	if repo["clone_status"] != "cloning" {
		t.Errorf("clone_status = %v, want cloning", repo["clone_status"])
	}
	if repo["name"] != "origin" {
		t.Errorf("derived name = %v, want origin", repo["name"])
	}
	// Provider is NULL at create (issue #66): inherit — the effective provider
	// resolves at spawn (global provider_default → first registered), never a
	// code-stamped row value.
	if repo["provider"] != nil {
		t.Errorf("provider = %v, want null (inherit)", repo["provider"])
	}
	if repo["tracker_binding"] != "builtin" || repo["forge_kind"] != "none" {
		t.Errorf("binding/kind = %v/%v, want builtin/none", repo["tracker_binding"], repo["forge_kind"])
	}
	if repo["afk_branch_pattern"] != "afk/<N>" || repo["manual_branch_prefix"] != "lab/" {
		t.Errorf("patterns = %v/%v", repo["afk_branch_pattern"], repo["manual_branch_prefix"])
	}
	if repo["incogni"] != false || repo["afk_auto_enabled"] != false {
		t.Errorf("flags = incogni %v, afk_auto %v, want false/false", repo["incogni"], repo["afk_auto_enabled"])
	}
	// Autoland defaults (issue #181 / ADR-0048): off, a 2-attempt fix-run
	// bound, auto-merge on, and no lander provider/model/effort override
	// (issue #189: model/effort join provider as nullable = inherit).
	if repo["autoland_enabled"] != false || repo["max_fix_attempts"] != float64(2) ||
		repo["auto_merge"] != true || repo["lander_provider"] != nil ||
		repo["lander_model"] != nil || repo["lander_effort"] != nil {
		t.Errorf("autoland defaults = enabled=%v attempts=%v automerge=%v provider=%v model=%v effort=%v, want false/2/true/null/null/null",
			repo["autoland_enabled"], repo["max_fix_attempts"], repo["auto_merge"],
			repo["lander_provider"], repo["lander_model"], repo["lander_effort"])
	}
	// The pinned repo JSON: every key present (nullables as null).
	for _, k := range []string{"id", "name", "remote_url", "credential_id", "forge_credential_id",
		"tracker_binding", "forge_kind", "default_branch", "provider", "incogni", "model_default",
		"effort_default", "git_author_name", "git_author_email", "afk_branch_pattern",
		"manual_branch_prefix", "afk_auto_enabled", "consecutive_failures", "budget_minutes",
		"max_instances_override", "clone_status", "clone_error", "created_at", "last_opened_at",
		"afk_prompt", "afk_prompt_effective", "afk_provider_default",
		"autoland_enabled", "max_fix_attempts", "auto_merge", "lander_provider",
		"lander_model", "lander_effort"} {
		if _, ok := repo[k]; !ok {
			t.Errorf("repo JSON missing pinned key %q", k)
		}
	}
	id := repo["id"].(string)

	final := x.waitCloneStatus(t, id, "ready")
	if final["default_branch"] != "trunk" {
		t.Errorf("default_branch = %v, want detected trunk", final["default_branch"])
	}
	if final["clone_error"] != nil {
		t.Errorf("clone_error = %v, want null", final["clone_error"])
	}

	// List contains it.
	resp = x.do("GET", "/api/v1/repos", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	repos, ok := list["repos"].([]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("repos list = %v, want one repo", list)
	}

	// Bus events (the SSE feed): clone.progress with the pinned envelope
	// fields, repo.changed for the row changes.
	var sawProgress, sawChanged bool
	for _, ev := range log.snapshot() {
		switch ev.Type {
		case "clone.progress":
			sawProgress = true
		case "repo.changed":
			sawChanged = true
		}
	}
	if !sawProgress || !sawChanged {
		t.Errorf("events: progress=%v changed=%v, want both", sawProgress, sawChanged)
	}
}

func TestRepoCreateIncogniDefaults(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": filepath.Join(t.TempDir(), "nowhere"),
		"incogni":    true,
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo := decodeBody(t, resp)
	if repo["afk_branch_pattern"] != "issue-<N>" || repo["manual_branch_prefix"] != "wip/" {
		t.Errorf("incogni patterns = %v/%v, want issue-<N> and wip/", repo["afk_branch_pattern"], repo["manual_branch_prefix"])
	}
	if repo["incogni"] != true {
		t.Error("incogni flag not persisted")
	}
}

func TestRepoNameDerivationAndCollision(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	origin := makeRepoOrigin(t, x.home, "main", 1)

	// Explicit name is sanitized with the v0 scanner rules.
	resp := x.do("POST", "/api/v1/repos", map[string]any{"remote_url": origin, "name": "My Repo!"}, h)
	wantStatus(t, resp, http.StatusCreated)
	if repo := decodeBody(t, resp); repo["name"] != "My_Repo_" {
		t.Errorf("sanitized name = %v, want My_Repo_", repo["name"])
	}

	// A second repo sanitizing to the same name → 409.
	resp = x.do("POST", "/api/v1/repos", map[string]any{"remote_url": origin, "name": "My?Repo?"}, h)
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Fatal("409 without error message")
	}

	// Underivable name → 400.
	resp = x.do("POST", "/api/v1/repos", map[string]any{"remote_url": "///"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = decodeBody(t, resp)
}

func TestRepoAutoBindingAndKindMismatch(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "forge-tok", "kind": "forge_token",
		"payload": map[string]any{"host": "git.cloonar.com", "token": "t"},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	forgeCred := decodeBody(t, resp)["id"].(string)

	// Auto binding (the default) resolves to forge only with BOTH a forge
	// host and an attached forge credential (design §3a: forge binding
	// requires forge_credential_id). Forgejo host without one → builtin,
	// kind still recorded.
	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": "forgejo@git.cloonar.com:me/proj.git",
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo := decodeBody(t, resp)
	if repo["tracker_binding"] != "builtin" || repo["forge_kind"] != "forgejo" {
		t.Errorf("credential-less forgejo binding/kind = %v/%v, want builtin/forgejo", repo["tracker_binding"], repo["forge_kind"])
	}
	credlessID := repo["id"].(string)

	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": "forgejo@git.cloonar.com:me/proj2.git", "forge_credential_id": forgeCred,
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo = decodeBody(t, resp)
	if repo["tracker_binding"] != "forge" || repo["forge_kind"] != "forgejo" {
		t.Errorf("forgejo binding/kind = %v/%v, want forge/forgejo", repo["tracker_binding"], repo["forge_kind"])
	}
	forgeBoundID := repo["id"].(string)

	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": "git@gitlab.com:me/other.git", "tracker_binding": "auto",
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo = decodeBody(t, resp)
	if repo["tracker_binding"] != "builtin" || repo["forge_kind"] != "none" {
		t.Errorf("gitlab binding/kind = %v/%v, want builtin/none", repo["tracker_binding"], repo["forge_kind"])
	}

	// Explicit forge on a non-forge remote → 400.
	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": "/tmp/plain", "tracker_binding": "forge",
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = decodeBody(t, resp)

	// Explicit forge on a forge host WITHOUT a forge credential → 400.
	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": "forgejo@git.cloonar.com:me/proj3.git", "tracker_binding": "forge",
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); !strings.Contains(fmt.Sprint(got["error"]), "forge_token credential") {
		t.Errorf("400 body = %v, want message naming the forge_token credential", got["error"])
	}

	// PATCH side of the invariant: flipping to forge without a credential
	// → 400; clearing the credential while forge-bound → 400; clearing it
	// together with the flip to builtin → 200.
	resp = x.do("PATCH", "/api/v1/repos/"+credlessID, map[string]any{"tracker_binding": "forge"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = decodeBody(t, resp)

	resp = x.do("PATCH", "/api/v1/repos/"+forgeBoundID, map[string]any{"forge_credential_id": nil}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); !strings.Contains(fmt.Sprint(got["error"]), "forge_token credential") {
		t.Errorf("400 body = %v, want message naming the forge_token credential", got["error"])
	}

	resp = x.do("PATCH", "/api/v1/repos/"+forgeBoundID, map[string]any{
		"tracker_binding": "builtin", "forge_credential_id": nil,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["tracker_binding"] != "builtin" || repo["forge_credential_id"] != nil {
		t.Errorf("after flip to builtin = %v/%v, want builtin/null", repo["tracker_binding"], repo["forge_credential_id"])
	}

	// Credential-kind mismatches → 400 (design §3a split).
	resp = x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "ssh-key", "kind": "ssh_key",
		"payload": map[string]any{"private_key": "k"},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	sshCred := decodeBody(t, resp)["id"].(string)

	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": "/tmp/x1", "credential_id": forgeCred,
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = decodeBody(t, resp)

	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": "/tmp/x2", "forge_credential_id": sshCred,
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = decodeBody(t, resp)
}

func TestRepoPatchValidationMatrix(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	origin := makeRepoOrigin(t, x.home, "main", 1)

	resp := x.do("POST", "/api/v1/repos", map[string]any{"remote_url": origin}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)
	x.waitCloneStatus(t, id, "ready")

	// Grammar and value 400s (design §4a).
	for name, patch := range map[string]map[string]any{
		"pattern without <N>":   {"afk_branch_pattern": "plain"},
		"overlapping pair":      {"afk_branch_pattern": "lab/<N>"},
		"empty manual prefix":   {"manual_branch_prefix": ""},
		"bad pattern chars":     {"afk_branch_pattern": "afk ~<N>"},
		"budget zero":           {"budget_minutes": 0},
		"max instances zero":    {"max_instances_override": 0},
		"binding auto stored":   {"tracker_binding": "auto"},
		"binding forge on none": {"tracker_binding": "forge"},
		"empty default branch":  {"default_branch": ""},
		"unknown field":         {"nonsense": 1},
		"wrong type":            {"incogni": "yes"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := x.do("PATCH", "/api/v1/repos/"+id, patch, h)
			wantStatus(t, resp, http.StatusBadRequest)
			if got := decodeBody(t, resp); got["error"] == "" {
				t.Fatal("400 without error message")
			}
		})
	}

	// A full valid PATCH.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{
		"afk_branch_pattern":   "issue-<N>",
		"manual_branch_prefix": "wip/",
		"budget_minutes":       45,
		"model_default":        "opus[1m]",
		"git_author_name":      "Lab Bot",
		"afk_auto_enabled":     true,
		"default_branch":       "main",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo := decodeBody(t, resp)
	if repo["afk_branch_pattern"] != "issue-<N>" || repo["manual_branch_prefix"] != "wip/" {
		t.Errorf("patterns = %v/%v", repo["afk_branch_pattern"], repo["manual_branch_prefix"])
	}
	if repo["budget_minutes"] != float64(45) || repo["model_default"] != "opus[1m]" {
		t.Errorf("budget/model = %v/%v", repo["budget_minutes"], repo["model_default"])
	}
	if repo["afk_auto_enabled"] != true || repo["git_author_name"] != "Lab Bot" {
		t.Errorf("afk_auto/author = %v/%v", repo["afk_auto_enabled"], repo["git_author_name"])
	}

	// Incogni flip does NOT rewrite patterns (pinned contract).
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{"incogni": true}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["afk_branch_pattern"] != "issue-<N>" || repo["manual_branch_prefix"] != "wip/" {
		t.Error("incogni PATCH rewrote branch patterns")
	}

	// null clears nullable overrides.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{
		"budget_minutes": nil, "model_default": nil, "git_author_name": nil,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["budget_minutes"] != nil || repo["model_default"] != nil || repo["git_author_name"] != nil {
		t.Errorf("cleared fields = %v/%v/%v, want nulls", repo["budget_minutes"], repo["model_default"], repo["git_author_name"])
	}

	// Unknown repo → 404.
	resp = x.do("PATCH", "/api/v1/repos/repo_missing", map[string]any{"name": "x"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)
}

// TestRepoAFKPromptOverride pins the #52/ADR-0027 repo surface: afk_prompt is
// null when unset and afk_prompt_effective falls back to the built-in; the global
// afk_prompt becomes a no-own-override repo's effective; a repo's own PATCHed
// override echoes back but does NOT change effective (effective is the fallback a
// cleared field reveals); whitespace-only clears to null; an over-cap value is a
// 400; and an incogni repo's effective carries the incogni sentence.
func TestRepoAFKPromptOverride(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	origin := makeRepoOrigin(t, x.home, "main", 1)

	resp := x.do("POST", "/api/v1/repos", map[string]any{"remote_url": origin}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)
	x.waitCloneStatus(t, id, "ready")

	// Unset: afk_prompt is null; effective is the built-in (literal <N>).
	repo := x.getRepo(t, id)
	if _, ok := repo["afk_prompt"]; !ok {
		t.Fatal("repo JSON missing afk_prompt key")
	}
	if repo["afk_prompt"] != nil {
		t.Errorf("afk_prompt = %v, want null when unset", repo["afk_prompt"])
	}
	if eff, _ := repo["afk_prompt_effective"].(string); !strings.Contains(eff, "<N>") {
		t.Errorf("afk_prompt_effective = %q, want the built-in with literal <N>", repo["afk_prompt_effective"])
	}

	// A global override becomes the effective for a repo with no own override.
	if err := x.st.SetSetting(context.Background(), store.SettingAFKPrompt, "global override <N>"); err != nil {
		t.Fatalf("set global afk_prompt: %v", err)
	}
	if repo = x.getRepo(t, id); repo["afk_prompt_effective"] != "global override <N>" {
		t.Errorf("effective with global set = %v, want the global override", repo["afk_prompt_effective"])
	}
	if err := x.st.SetSetting(context.Background(), store.SettingAFKPrompt, ""); err != nil {
		t.Fatalf("clear global afk_prompt: %v", err)
	}

	// A repo's own override echoes in afk_prompt but effective still ignores it
	// (effective is the fallback, computed as if the repo's own value were empty).
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{"afk_prompt": "repo playbook <BRANCH>"}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["afk_prompt"] != "repo playbook <BRANCH>" {
		t.Errorf("afk_prompt after PATCH = %v, want the stored value", repo["afk_prompt"])
	}
	if eff, _ := repo["afk_prompt_effective"].(string); !strings.Contains(eff, "<N>") {
		t.Errorf("afk_prompt_effective after repo PATCH = %q, want the built-in (effective ignores the repo's own override)", repo["afk_prompt_effective"])
	}

	// Whitespace-only clears back to null (inherit).
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{"afk_prompt": "   \n "}, h)
	wantStatus(t, resp, http.StatusOK)
	if repo = decodeBody(t, resp); repo["afk_prompt"] != nil {
		t.Errorf("afk_prompt after whitespace PATCH = %v, want null", repo["afk_prompt"])
	}

	// Over the 16 KiB cap → 400.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{"afk_prompt": strings.Repeat("a", (16<<10)+1)}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Fatal("over-cap afk_prompt 400 without error message")
	}

	// An incogni repo inheriting the built-in sees the incogni sentence in
	// effective (asserted on the create response, no clone wait needed).
	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": origin, "name": "incog", "incogni": true,
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	inc := decodeBody(t, resp)
	if eff, _ := inc["afk_prompt_effective"].(string); !strings.Contains(eff, "No AI attribution") {
		t.Errorf("incogni effective = %q, want the incogni attribution sentence", inc["afk_prompt_effective"])
	}
}

// TestRepoProviderOverride pins the issue-#66 repo surface: POST /repos takes
// an optional provider (strict when non-empty, null/absent = inherit), PATCH
// sets/clears provider and afk_provider_default (null or "" → NULL), and an
// unknown id is a 400 on both routes.
func TestRepoProviderOverride(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	origin := makeRepoOrigin(t, x.home, "main", 1)

	// POST with an unknown provider → 400, nothing created.
	resp := x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": origin, "name": "badprov", "provider": "ghost",
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); !strings.Contains(fmt.Sprint(got["error"]), "ghost") {
		t.Errorf("400 body = %v, want message naming the unknown provider", got["error"])
	}

	// POST with a registered provider stores it.
	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": origin, "name": "onb", "provider": "fake-b",
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo := decodeBody(t, resp)
	if repo["provider"] != "fake-b" {
		t.Errorf("created provider = %v, want fake-b", repo["provider"])
	}
	if repo["afk_provider_default"] != nil {
		t.Errorf("afk_provider_default = %v, want null", repo["afk_provider_default"])
	}
	id := repo["id"].(string)

	// PATCH both provider knobs.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{
		"provider": "claude-code", "afk_provider_default": "fake-b",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["provider"] != "claude-code" || repo["afk_provider_default"] != "fake-b" {
		t.Errorf("patched providers = %v/%v, want claude-code/fake-b", repo["provider"], repo["afk_provider_default"])
	}

	// Unknown ids → 400 on either knob; the row is untouched.
	for _, patch := range []map[string]any{
		{"provider": "ghost"},
		{"afk_provider_default": "ghost"},
	} {
		resp = x.do("PATCH", "/api/v1/repos/"+id, patch, h)
		wantStatus(t, resp, http.StatusBadRequest)
		if got := decodeBody(t, resp); got["error"] == "" {
			t.Fatal("unknown provider PATCH 400 without error message")
		}
	}
	repo = x.getRepo(t, id)
	if repo["provider"] != "claude-code" || repo["afk_provider_default"] != "fake-b" {
		t.Errorf("row changed by rejected PATCH: %v/%v", repo["provider"], repo["afk_provider_default"])
	}

	// null clears both back to inherit; "" clears too.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{
		"provider": nil, "afk_provider_default": "",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["provider"] != nil || repo["afk_provider_default"] != nil {
		t.Errorf("cleared providers = %v/%v, want nulls", repo["provider"], repo["afk_provider_default"])
	}
}

// TestRepoRemoteDefaults pins the issue-#163 repo surface — the tri-state a
// BOOLEAN override needs, and the trap it exists to avoid. Both keys are always
// present (null when unset = inherit); true pins on; FALSE pins OFF and must
// come back as false, not null, because a repo's explicit "never remote" has to
// beat a global true; null clears back to inherit. A non-boolean is a 400 that
// leaves the row untouched.
func TestRepoRemoteDefaults(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	origin := makeRepoOrigin(t, x.home, "main", 1)

	resp := x.do("POST", "/api/v1/repos", map[string]any{"remote_url": origin}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo := decodeBody(t, resp)
	id := repo["id"].(string)

	keys := []string{"remote_default", "afk_remote_default"}
	for _, key := range keys {
		v, present := repo[key]
		if !present {
			t.Fatalf("repo JSON missing %s (nullable keys are always present)", key)
		}
		if v != nil {
			t.Errorf("%s = %v on a fresh repo, want null (inherit)", key, v)
		}
	}

	// true pins remote ON at repo scope.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{
		"remote_default": true, "afk_remote_default": true,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	for _, key := range keys {
		if repo[key] != true {
			t.Errorf("%s = %v (%T) after PATCH true, want true", key, repo[key], repo[key])
		}
	}

	// THE TRAP: false is a VALUE, not "unset". It must round-trip as false —
	// never collapse into null the way an empty string does for the string knobs.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{
		"remote_default": false, "afk_remote_default": false,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	for _, key := range keys {
		v, present := repo[key]
		if !present {
			t.Fatalf("repo JSON missing %s after PATCH false", key)
		}
		if v == nil {
			t.Errorf("%s = null after PATCH false, want false — an explicit off must not read as inherit", key)
			continue
		}
		if b, ok := v.(bool); !ok || b {
			t.Errorf("%s = %v (%T) after PATCH false, want the JSON bool false", key, v, v)
		}
	}
	// Persisted as a PINNED false (non-nil pointer), not NULL — this is what
	// ResolveRemote reads to beat a global true.
	row, err := x.st.RepoByID(context.Background(), id)
	if err != nil {
		t.Fatalf("RepoByID: %v", err)
	}
	if row.RemoteDefault == nil || *row.RemoteDefault {
		t.Errorf("stored remote_default = %v, want a non-nil pointer to false", row.RemoteDefault)
	}
	if row.AFKRemoteDefault == nil || *row.AFKRemoteDefault {
		t.Errorf("stored afk_remote_default = %v, want a non-nil pointer to false", row.AFKRemoteDefault)
	}

	// null clears both back to inherit (a NULL column, not a stored false).
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{
		"remote_default": nil, "afk_remote_default": nil,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["remote_default"] != nil || repo["afk_remote_default"] != nil {
		t.Errorf("cleared remotes = %v/%v, want nulls", repo["remote_default"], repo["afk_remote_default"])
	}
	if row, err = x.st.RepoByID(context.Background(), id); err != nil {
		t.Fatalf("RepoByID: %v", err)
	}
	if row.RemoteDefault != nil || row.AFKRemoteDefault != nil {
		t.Errorf("stored remotes = %v/%v after PATCH null, want NULL columns", row.RemoteDefault, row.AFKRemoteDefault)
	}

	// A non-boolean is a 400 on either knob; the row stays at inherit.
	for _, patch := range []map[string]any{
		{"remote_default": "on"},
		{"remote_default": 3},
		{"afk_remote_default": "off"},
		{"afk_remote_default": 0},
	} {
		resp = x.do("PATCH", "/api/v1/repos/"+id, patch, h)
		wantStatus(t, resp, http.StatusBadRequest)
		if got := decodeBody(t, resp); got["error"] == "" {
			t.Fatalf("non-boolean %v 400 without error message", patch)
		}
	}
	if repo = x.getRepo(t, id); repo["remote_default"] != nil || repo["afk_remote_default"] != nil {
		t.Errorf("row changed by a rejected PATCH: %v/%v", repo["remote_default"], repo["afk_remote_default"])
	}
}

// TestRepoAutolandSettings pins the issue-#181 / ADR-0048 repo surface: the
// four settings default off/2/true/null, PATCH sets and clears them, an
// unknown lander_provider or a negative max_fix_attempts is a 400, and
// autoland_enabled can only be turned ON for a forge-bound repo — the
// poller has no PR-comment listing to read on the builtin binding — while
// turning it off stays legal on any binding.
func TestRepoAutolandSettings(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	origin := makeRepoOrigin(t, x.home, "main", 1)

	resp := x.do("POST", "/api/v1/repos", map[string]any{"remote_url": origin}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo := decodeBody(t, resp)
	if repo["tracker_binding"] != "builtin" {
		t.Fatalf("tracker_binding = %v, want builtin (test fixture assumption)", repo["tracker_binding"])
	}
	id := repo["id"].(string)

	// autoland_enabled can never turn ON while builtin-bound.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{"autoland_enabled": true}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); !strings.Contains(fmt.Sprint(got["error"]), "forge tracker binding") {
		t.Errorf("400 body = %v, want message naming the forge tracker binding requirement", got["error"])
	}
	// ...but turning it off is always legal, even on builtin.
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{"autoland_enabled": false}, h)
	wantStatus(t, resp, http.StatusOK)
	if repo = decodeBody(t, resp); repo["autoland_enabled"] != false {
		t.Errorf("autoland_enabled = %v after PATCH false, want false", repo["autoland_enabled"])
	}

	// Set all four on a forge-bound repo.
	resp = x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "forge-tok-autoland", "kind": "forge_token",
		"payload": map[string]any{"host": "git.cloonar.com", "token": "t"},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	forgeCred := decodeBody(t, resp)["id"].(string)
	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url": "forgejo@git.cloonar.com:me/autoland.git", "forge_credential_id": forgeCred,
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	repo = decodeBody(t, resp)
	if repo["tracker_binding"] != "forge" {
		t.Fatalf("tracker_binding = %v, want forge (test fixture assumption)", repo["tracker_binding"])
	}
	forgeID := repo["id"].(string)

	resp = x.do("PATCH", "/api/v1/repos/"+forgeID, map[string]any{
		"autoland_enabled": true, "max_fix_attempts": 5, "auto_merge": false, "lander_provider": "fake-b",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["autoland_enabled"] != true || repo["max_fix_attempts"] != float64(5) ||
		repo["auto_merge"] != false || repo["lander_provider"] != "fake-b" {
		t.Errorf("patched autoland settings = %+v, want true/5/false/fake-b",
			[]any{repo["autoland_enabled"], repo["max_fix_attempts"], repo["auto_merge"], repo["lander_provider"]})
	}

	// null clears lander_provider back to inherit (this repo's own Provider).
	resp = x.do("PATCH", "/api/v1/repos/"+forgeID, map[string]any{"lander_provider": nil}, h)
	wantStatus(t, resp, http.StatusOK)
	if repo = decodeBody(t, resp); repo["lander_provider"] != nil {
		t.Errorf("lander_provider after PATCH null = %v, want nil", repo["lander_provider"])
	}

	// lander_model / lander_effort (issue #189) round-trip as plain nullable
	// strings — real-looking ids set and echo back.
	resp = x.do("PATCH", "/api/v1/repos/"+forgeID, map[string]any{
		"lander_model": "sonnet", "lander_effort": "high",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["lander_model"] != "sonnet" || repo["lander_effort"] != "high" {
		t.Errorf("patched lander model/effort = %v/%v, want sonnet/high", repo["lander_model"], repo["lander_effort"])
	}

	// DELIBERATE (issue #189): an arbitrary unknown model/effort string is
	// ACCEPTED at write time — there is NO catalog to validate against here
	// (per-provider, dynamic #156/#157; the lander's effective provider isn't
	// knowable at PATCH). Strictness lives at lander launch, which fails loudly
	// on an unknown value. So "ghost-model" round-trips, unlike an unknown
	// lander_provider (registry-checked, 400 below).
	resp = x.do("PATCH", "/api/v1/repos/"+forgeID, map[string]any{
		"lander_model": "ghost-model", "lander_effort": "ghost-effort",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["lander_model"] != "ghost-model" || repo["lander_effort"] != "ghost-effort" {
		t.Errorf("unknown lander model/effort = %v/%v, want them accepted as-is (validation is at launch, not write)",
			repo["lander_model"], repo["lander_effort"])
	}

	// null clears one, "" clears the other — pinning patchNullableString's
	// null-or-empty → NULL (inherit) behavior for these two fields too.
	resp = x.do("PATCH", "/api/v1/repos/"+forgeID, map[string]any{
		"lander_model": nil, "lander_effort": "",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	repo = decodeBody(t, resp)
	if repo["lander_model"] != nil || repo["lander_effort"] != nil {
		t.Errorf("cleared lander model/effort = %v/%v, want nulls (null and \"\" both clear)",
			repo["lander_model"], repo["lander_effort"])
	}

	// Rejections, each leaving the row untouched.
	for name, patch := range map[string]map[string]any{
		"negative max_fix_attempts": {"max_fix_attempts": -1},
		"unknown lander_provider":   {"lander_provider": "ghost"},
		"null max_fix_attempts":     {"max_fix_attempts": nil},
		"null autoland_enabled":     {"autoland_enabled": nil},
		"null auto_merge":           {"auto_merge": nil},
	} {
		t.Run(name, func(t *testing.T) {
			resp := x.do("PATCH", "/api/v1/repos/"+forgeID, patch, h)
			wantStatus(t, resp, http.StatusBadRequest)
			if got := decodeBody(t, resp); got["error"] == "" {
				t.Fatal("400 without error message")
			}
		})
	}
	repo = x.getRepo(t, forgeID)
	if repo["autoland_enabled"] != true || repo["max_fix_attempts"] != float64(5) ||
		repo["auto_merge"] != false || repo["lander_provider"] != nil {
		t.Errorf("row changed by a rejected PATCH: %+v",
			[]any{repo["autoland_enabled"], repo["max_fix_attempts"], repo["auto_merge"], repo["lander_provider"]})
	}

	// The invariant holds from the other side too: while autoland is on, the
	// binding cannot flip away from forge — the same PATCH must turn it off.
	resp = x.do("PATCH", "/api/v1/repos/"+forgeID, map[string]any{"tracker_binding": "builtin"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); !strings.Contains(fmt.Sprint(got["error"]), "forge tracker binding") {
		t.Errorf("400 body = %v, want message naming the forge tracker binding requirement", got["error"])
	}
	resp = x.do("PATCH", "/api/v1/repos/"+forgeID, map[string]any{
		"tracker_binding": "builtin", "autoland_enabled": false,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	if repo = decodeBody(t, resp); repo["tracker_binding"] != "builtin" || repo["autoland_enabled"] != false {
		t.Errorf("combined flip = %v/%v, want builtin/false", repo["tracker_binding"], repo["autoland_enabled"])
	}
}

func TestRepoDeleteMatrix(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)

	// Ready repo: plain delete works, bare dir removed.
	origin := makeRepoOrigin(t, x.home, "main", 1)
	resp := x.do("POST", "/api/v1/repos", map[string]any{"remote_url": origin}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)
	x.waitCloneStatus(t, id, "ready")

	resp = x.do("DELETE", "/api/v1/repos/"+id, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if _, err := os.Stat(filepath.Join(x.reposDir, id+".git")); !os.IsNotExist(err) {
		t.Errorf("bare dir survived delete: %v", err)
	}
	resp = x.do("GET", "/api/v1/repos/"+id, nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)

	// Cloning repo: refuse without force, remove with force. A ~32MB
	// incompressible blob keeps the clone busy while the requests land.
	slow := makeRepoOrigin(t, x.home, "main", 1)
	blob := make([]byte, 32<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slow, "blob.bin"), blob, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	repoGitCmd(t, x.home, slow, "add", ".")
	repoGitCmd(t, x.home, slow, "commit", "-q", "-m", "blob")

	resp = x.do("POST", "/api/v1/repos", map[string]any{"remote_url": "file://" + slow, "name": "slow"}, h)
	wantStatus(t, resp, http.StatusCreated)
	slowID := decodeBody(t, resp)["id"].(string)

	resp = x.do("DELETE", "/api/v1/repos/"+slowID, nil, h)
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Fatal("409 without error message")
	}

	resp = x.do("DELETE", "/api/v1/repos/"+slowID+"?force=true", nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if _, err := os.Stat(filepath.Join(x.reposDir, slowID+".git")); !os.IsNotExist(err) {
		t.Errorf("bare dir survived force delete: %v", err)
	}

	// Unknown repo → 404.
	resp = x.do("DELETE", "/api/v1/repos/repo_missing", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)
}

func TestRepoCloneRetryFlowAndErrorWithoutSecrets(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)

	// An HTTPS credential whose token must never surface in clone_error.
	const secret = "sekrit-clone-token-4711"
	resp := x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "https", "kind": "https_token",
		"payload": map[string]any{"username": "bot", "token": secret},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	credID := decodeBody(t, resp)["id"].(string)

	// invalid.invalid is guaranteed NXDOMAIN: the clone fails fast.
	resp = x.do("POST", "/api/v1/repos", map[string]any{
		"remote_url":    "https://invalid.invalid/owner/repo.git",
		"credential_id": credID,
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	badID := decodeBody(t, resp)["id"].(string)
	failed := x.waitCloneStatus(t, badID, "error")
	cloneErr, _ := failed["clone_error"].(string)
	if cloneErr == "" {
		t.Fatal("clone_error empty after failed clone")
	}
	if strings.Contains(cloneErr, secret) {
		t.Errorf("clone_error leaks the token: %q", cloneErr)
	}

	// Retry against a still-missing remote → 202, fails again.
	future := filepath.Join(t.TempDir(), "origin")
	resp = x.do("POST", "/api/v1/repos", map[string]any{"remote_url": future, "name": "willfix"}, h)
	wantStatus(t, resp, http.StatusCreated)
	fixID := decodeBody(t, resp)["id"].(string)
	x.waitCloneStatus(t, fixID, "error")

	// Retry on a non-error repo → 409.
	origin := makeRepoOrigin(t, x.home, "main", 1)
	resp = x.do("POST", "/api/v1/repos", map[string]any{"remote_url": origin, "name": "fine"}, h)
	wantStatus(t, resp, http.StatusCreated)
	fineID := decodeBody(t, resp)["id"].(string)
	x.waitCloneStatus(t, fineID, "ready")
	resp = x.do("POST", "/api/v1/repos/"+fineID+"/clone/retry", nil, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = decodeBody(t, resp)

	// Create the origin where the failed repo points; retry → ready.
	repoGitCmd(t, x.home, "", "init", "-q", "-b", "main", future)
	if err := os.WriteFile(filepath.Join(future, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	repoGitCmd(t, x.home, future, "add", ".")
	repoGitCmd(t, x.home, future, "commit", "-q", "-m", "c")

	resp = x.do("POST", "/api/v1/repos/"+fixID+"/clone/retry", nil, h)
	wantStatus(t, resp, http.StatusAccepted)
	_ = decodeBody(t, resp)
	fixed := x.waitCloneStatus(t, fixID, "ready")
	if fixed["clone_error"] != nil {
		t.Errorf("clone_error after successful retry = %v, want null", fixed["clone_error"])
	}

	// Retry on an unknown repo → 404.
	resp = x.do("POST", "/api/v1/repos/repo_missing/clone/retry", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)
}

// TestRepoCredentialGoneMapsTo409 pins the writeRepoError mapping for the
// FK-race sentinel: store.ErrCredentialGone → 409 {"error":"credential no
// longer exists"}. The race itself (credential deleted between reposvc's
// kind check and the row write) cannot be triggered deterministically over
// HTTP, so the mapping is exercised directly.
func TestRepoCredentialGoneMapsTo409(t *testing.T) {
	x := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	x.srv.writeRepoError(rec, "updating repo",
		fmt.Errorf("update repo %q: %w", "repo_x", store.ErrCredentialGone))
	resp := rec.Result()
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp); got["error"] != store.ErrCredentialGone.Error() {
		t.Errorf("error body = %v, want %q", got["error"], store.ErrCredentialGone.Error())
	}
}

func TestRepoRoutesRequireAuthAndCSRF(t *testing.T) {
	x := newRepoTestServer(t)

	// No auth at all → 401.
	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/repos", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	// Cookie auth without the CSRF header on a mutation → 403.
	resp = x.do("POST", "/api/v1/repos", map[string]any{"remote_url": "/tmp/x"}, nil)
	wantStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()
}
