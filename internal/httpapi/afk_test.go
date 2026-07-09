package httpapi

// httptest suite for the M5 AFK operator surface: manual start (202 + the
// full 409 matrix), the auto toggle, the failure reset, and the claimable
// count on the ready endpoint. Real store + real gitx bare fixture + real
// tracker registry (builtin binding), fake tmux + fake provider — the same
// bar as the instance suite; the engine's own behavior (reaper, scheduler,
// classify) is covered in internal/afk.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

var afkClock = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

type afkTestServer struct {
	*testServer
	runner *tmuxx.Fake
	prov   *providertest.Fake
	repo   store.Repo
}

// afkConfig tunes newAFKServer for a specific test. builtinFactory wraps the
// store-backed tracker constructor the registry uses — a counting wrapper lets
// a test assert how many tracker round-trips a handler makes.
type afkConfig struct {
	builtinFactory tracker.BuiltinFactory
}

// newAFKServer builds the full M5 stack: instance service + AFK engine over
// a real bare clone, both resolving trackers through the REAL registry with
// the builtin binding (ready issues are store issues carrying the
// ready-for-agent label seeded at repo creation).
func newAFKServer(t *testing.T, mods ...func(*afkConfig)) *afkTestServer {
	t.Helper()
	cfg := afkConfig{builtinFactory: builtin.New}
	for _, m := range mods {
		m(&cfg)
	}
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeRepoOrigin(t, home, "main", 2)
	env := testutil.HermeticGitEnv(home)

	stateDir := t.TempDir()
	reposDir := filepath.Join(stateDir, "repos")
	worktreeRoot := filepath.Join(stateDir, "worktrees")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}

	git := gitx.New("git")
	runner := tmuxx.NewFake()
	prov := providertest.New()
	var repo store.Repo

	x := newTestServer(t, func(o *Options) {
		st := o.Store
		if err := st.SeedDefaultSettings(context.Background(), 6, "claude-code"); err != nil {
			t.Fatal(err)
		}
		repoID := ids.NewID("repo")
		bare := filepath.Join(reposDir, repoID+".git")
		if err := git.CloneBare(context.Background(), "file://"+origin, bare, env, nil); err != nil {
			t.Fatalf("CloneBare: %v", err)
		}
		var err error
		repo, err = st.CreateRepo(context.Background(), store.Repo{
			ID: repoID, Name: "proj", RemoteURL: "file://" + origin,
			TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
			AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
			CloneStatus: store.CloneStatusReady, CreatedAt: afkClock,
		})
		if err != nil {
			t.Fatalf("CreateRepo: %v", err)
		}
		vlt, err := vault.New(make([]byte, vault.KeySize))
		if err != nil {
			t.Fatal(err)
		}
		mat, err := vault.NewMaterializer(filepath.Join(stateDir, "runtime"))
		if err != nil {
			t.Fatal(err)
		}
		reg, err := provider.NewRegistry(prov)
		if err != nil {
			t.Fatal(err)
		}
		// The instance service and the AFK engine share ONE starting-set guard
		// (design §4b — the same instance instance/ and reconcile/ are built
		// with), so the reaper skips a mid-Launch run whose session is not yet
		// live.
		guard := startguard.New()
		inst, err := instance.New(instance.Options{
			Store: st, Git: git, Runner: runner, Providers: reg, Vault: vlt, Materializer: mat,
			Guard: guard, Bus: o.Bus, Logger: logx.New(io.Discard), ReposDir: reposDir,
			WorktreeRoot: worktreeRoot, LabURL: "http://127.0.0.1:8080", GitEnv: env,
			CaptureCtx: context.Background(), Now: func() time.Time { return afkClock },
		})
		if err != nil {
			t.Fatal(err)
		}
		trackerReg := tracker.NewRegistry(st, vlt, nil, cfg.builtinFactory,
			func(tracker.ForgejoConfig) tracker.Tracker {
				panic("forgejo factory invoked in a builtin-only test")
			},
			func(tracker.GitHubConfig) tracker.Tracker {
				panic("github factory invoked in a builtin-only test")
			})
		afkSvc, err := afk.New(afk.Options{
			Store: st, Git: git, Runner: runner, Trackers: trackerReg,
			Instances: inst, Materializer: mat, Bus: o.Bus, Logger: logx.New(io.Discard),
			Guard: guard, ReposDir: reposDir, WorktreeRoot: worktreeRoot, GitEnv: env,
			Now: func() time.Time { return afkClock },
		})
		if err != nil {
			t.Fatal(err)
		}
		inst.SetAFKStopper(afkSvc)
		o.Instances = inst
		o.Providers = reg
		o.Tracker = trackerReg
		o.AFK = afkSvc
	})
	x.setup("op", "password123")
	return &afkTestServer{testServer: x, runner: runner, prov: prov, repo: repo}
}

// seedReadyIssue creates a builtin issue carrying the ready-for-agent label
// (seeded by CreateRepo) — one entry of the AFK claimable queue. Numbers run
// 1,2,… per repo.
func seedReadyIssue(t *testing.T, x *afkTestServer, title string) store.Issue {
	t.Helper()
	ctx := context.Background()
	labels, err := x.st.LabelsByRepo(ctx, x.repo.ID)
	if err != nil {
		t.Fatalf("LabelsByRepo: %v", err)
	}
	readyID := ""
	for _, l := range labels {
		if l.Name == tracker.ReadyLabel {
			readyID = l.ID
		}
	}
	if readyID == "" {
		t.Fatalf("repo has no %s label", tracker.ReadyLabel)
	}
	is, err := x.st.CreateIssueWithLabels(ctx, x.repo.ID, title, "", []string{readyID}, store.CommentAuthorOperator, nil, afkClock)
	if err != nil {
		t.Fatalf("CreateIssueWithLabels: %v", err)
	}
	return is
}

func pauseRepo(t *testing.T, x *afkTestServer) {
	t.Helper()
	for i := 0; i < afk.PauseThreshold; i++ {
		if _, err := x.st.IncrementRepoFailures(context.Background(), x.repo.ID); err != nil {
			t.Fatalf("IncrementRepoFailures: %v", err)
		}
	}
}

func TestAPI_AFKStartHappyPath(t *testing.T) {
	x := newAFKServer(t)
	h := csrfHeaders(x.ts.URL)
	log := recordBus(t, x.bus)
	seedReadyIssue(t, x, "fix the flux capacitor")

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/afk/start", map[string]any{}, h)
	wantStatus(t, resp, http.StatusAccepted)
	// The pinned M5 envelope: 202 {run}.
	run, ok := decodeBody(t, resp)["run"].(map[string]any)
	if !ok {
		t.Fatalf("202 body has no run envelope")
	}
	if run["kind"] != "afk_manual" || run["outcome"] != "active" {
		t.Fatalf("run kind/outcome = %v/%v", run["kind"], run["outcome"])
	}
	if run["branch"] != "afk/1" || run["session_name"] != "proj~afk-1" {
		t.Errorf("run branch/session = %v/%v", run["branch"], run["session_name"])
	}
	if run["issue_number"] != float64(1) {
		t.Errorf("issue_number = %v, want 1", run["issue_number"])
	}
	// The budget clock is persisted on the row (D12b), never in-memory.
	if want := store.FormatTime(afkClock.Add(120 * time.Minute)); run["budget_deadline"] != want {
		t.Errorf("budget_deadline = %v, want %v", run["budget_deadline"], want)
	}

	// The claim is real: the session is live and the run row is active.
	if _, live := x.runner.Session("proj~afk-1"); !live {
		t.Error("no live tmux session for the AFK run")
	}
	if !sawEvent(log, "run.changed") {
		t.Error("no run.changed event on AFK start")
	}
}

func TestAPI_AFKStartConflictMatrix(t *testing.T) {
	t.Run("paused", func(t *testing.T) {
		x := newAFKServer(t)
		pauseRepo(t, x)
		seedReadyIssue(t, x, "never claimed")
		resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/afk/start", map[string]any{}, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusConflict)
		if got := decodeBody(t, resp); got["error"] != afk.ErrRepoPaused.Error() {
			t.Errorf("error = %q, want the paused message", got["error"])
		}
	})
	t.Run("no claimable issue", func(t *testing.T) {
		x := newAFKServer(t)
		resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/afk/start", map[string]any{}, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusConflict)
		// The pinned body, byte for byte.
		if got := decodeBody(t, resp); got["error"] != "no ready-for-agent issues to start" {
			t.Errorf("error = %q, want the pinned no-ready message", got["error"])
		}
	})
	t.Run("over cap", func(t *testing.T) {
		x := newAFKServer(t)
		seedReadyIssue(t, x, "capped out")
		if err := x.st.SetSetting(context.Background(), store.SettingMaxInstances, "1"); err != nil {
			t.Fatal(err)
		}
		x.runner.AddLive("proj~existing")
		resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/afk/start", map[string]any{}, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusConflict)
		if got := decodeBody(t, resp); got["error"] != instance.ErrOverCap.Error() {
			t.Errorf("error = %q, want the over-cap message", got["error"])
		}
	})
	t.Run("logged out", func(t *testing.T) {
		x := newAFKServer(t)
		seedReadyIssue(t, x, "locked out")
		x.prov.SetLoggedIn(false)
		resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/afk/start", map[string]any{}, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusConflict)
		if got := decodeBody(t, resp); got["error"] != instance.ErrLoggedOut.Error() {
			t.Errorf("error = %q, want the logged-out message", got["error"])
		}
	})
	t.Run("unknown repo", func(t *testing.T) {
		x := newAFKServer(t)
		resp := x.do("POST", "/api/v1/repos/ghost/afk/start", map[string]any{}, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusNotFound)
		_ = resp.Body.Close()
	})
}

func TestAPI_AFKAutoTogglePersists(t *testing.T) {
	x := newAFKServer(t)
	h := csrfHeaders(x.ts.URL)
	log := recordBus(t, x.bus)

	// enabled is mandatory — a typoed body must not silently toggle.
	resp := x.do("PUT", "/api/v1/repos/"+x.repo.ID+"/afk/auto", map[string]any{"enable": true}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()

	resp = x.do("PUT", "/api/v1/repos/"+x.repo.ID+"/afk/auto", map[string]any{"enabled": true}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["afk_auto_enabled"] != true {
		t.Fatalf("toggle response = %v, want afk_auto_enabled true", got)
	}

	// Persisted, not just echoed.
	repo, err := x.st.RepoByID(context.Background(), x.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !repo.AFKAutoEnabled {
		t.Error("afk_auto_enabled not persisted")
	}
	if !sawEvent(log, "repo.changed") {
		t.Error("no repo.changed event on auto toggle")
	}

	resp = x.do("PUT", "/api/v1/repos/"+x.repo.ID+"/afk/auto", map[string]any{"enabled": false}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["afk_auto_enabled"] != false {
		t.Fatalf("toggle-off response = %v", got)
	}

	resp = x.do("PUT", "/api/v1/repos/ghost/afk/auto", map[string]any{"enabled": true}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

func TestAPI_AFKResetClearsFailures(t *testing.T) {
	x := newAFKServer(t)
	h := csrfHeaders(x.ts.URL)
	pauseRepo(t, x)
	log := recordBus(t, x.bus)

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/afk/reset", nil, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["consecutive_failures"] != float64(0) {
		t.Fatalf("reset response = %v, want consecutive_failures 0", got)
	}
	repo, err := x.st.RepoByID(context.Background(), x.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repo.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d after reset", repo.ConsecutiveFailures)
	}
	if !sawEvent(log, "repo.changed") {
		t.Error("no repo.changed event on reset")
	}

	resp = x.do("POST", "/api/v1/repos/ghost/afk/reset", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// seedForgeRepo creates a clone-ready repo whose tracker binding can never be
// driven: a github forge kind, for which the registry always fails with
// ErrForgeUnsupported. It shares the fixture's provider so the auth check
// passes and the failure is reached at tracker resolution, not before.
func seedForgeRepo(t *testing.T, x *afkTestServer) store.Repo {
	t.Helper()
	repo, err := x.st.CreateRepo(context.Background(), store.Repo{
		ID: ids.NewID("repo"), Name: "ghforge", RemoteURL: "https://github.com/acme/widget",
		TrackerBinding: store.TrackerBindingForge, ForgeKind: "github", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: afkClock,
	})
	if err != nil {
		t.Fatalf("CreateRepo forge: %v", err)
	}
	return repo
}

// A repo whose tracker binding can't be driven is a 409 refusal on POST
// afk/start (the same config-conflict the issues routes answer), never an
// opaque 500.
func TestAPI_AFKStartTrackerConfigConflict(t *testing.T) {
	x := newAFKServer(t)
	forge := seedForgeRepo(t, x)

	resp := x.do("POST", "/api/v1/repos/"+forge.ID+"/afk/start", map[string]any{}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp)["error"].(string); !strings.Contains(got, "no forge credential") {
		t.Errorf("error = %q, want the tracker-config diagnostic", got)
	}
}

// Enabling auto on a repo whose tracker can never be driven is refused (no
// silently dead loop); disabling stays allowed.
func TestAPI_AFKAutoRefusesUndrivableTracker(t *testing.T) {
	x := newAFKServer(t)
	h := csrfHeaders(x.ts.URL)
	forge := seedForgeRepo(t, x)

	resp := x.do("PUT", "/api/v1/repos/"+forge.ID+"/afk/auto", map[string]any{"enabled": true}, h)
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp)["error"].(string); !strings.Contains(got, "no forge credential") {
		t.Errorf("error = %q, want the tracker-config diagnostic", got)
	}
	// Not persisted.
	repo, err := x.st.RepoByID(context.Background(), forge.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repo.AFKAutoEnabled {
		t.Error("afk_auto_enabled persisted despite the refusal")
	}
	// Disabling is always allowed, even on an undrivable repo.
	resp = x.do("PUT", "/api/v1/repos/"+forge.ID+"/afk/auto", map[string]any{"enabled": false}, h)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
}

// countingReadyTracker counts ReadyIssues calls, delegating everything else to
// the wrapped store-backed tracker.
type countingReadyTracker struct {
	tracker.Tracker
	calls *int32
}

func (c *countingReadyTracker) ReadyIssues(ctx context.Context) ([]tracker.Issue, error) {
	atomic.AddInt32(c.calls, 1)
	return c.Tracker.ReadyIssues(ctx)
}

// GET /repos/{id}/ready fetches the ready queue from the tracker exactly once
// per request — the claimable filter reuses that snapshot instead of re-fetching.
func TestAPI_ReadyFetchesTrackerOnce(t *testing.T) {
	var readyCalls int32
	x := newAFKServer(t, func(c *afkConfig) {
		c.builtinFactory = func(cfg tracker.BuiltinConfig) tracker.Tracker {
			return &countingReadyTracker{Tracker: builtin.New(cfg), calls: &readyCalls}
		}
	})
	seedReadyIssue(t, x, "first")

	resp := x.do("GET", "/api/v1/repos/"+x.repo.ID+"/ready", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	if got := atomic.LoadInt32(&readyCalls); got != 1 {
		t.Errorf("ReadyIssues called %d times, want 1 (no double-fetch)", got)
	}
}

func TestAPI_ReadyClaimableCount(t *testing.T) {
	x := newAFKServer(t)
	h := csrfHeaders(x.ts.URL)
	seedReadyIssue(t, x, "first")
	seedReadyIssue(t, x, "second")

	resp := x.do("GET", "/api/v1/repos/"+x.repo.ID+"/ready", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	if body["claimable_count"] != float64(2) {
		t.Fatalf("claimable_count = %v, want 2", body["claimable_count"])
	}

	// Claiming #1 (the branch IS the claim) drops the claimable count while
	// the ready list itself still shows both open labeled issues.
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/afk/start", map[string]any{}, h)
	wantStatus(t, resp, http.StatusAccepted)
	_ = resp.Body.Close()

	resp = x.do("GET", "/api/v1/repos/"+x.repo.ID+"/ready", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body = decodeBody(t, resp)
	if issues, _ := body["issues"].([]any); len(issues) != 2 {
		t.Errorf("ready issues = %d, want 2 (the claim is invisible to triage)", len(issues))
	}
	if body["claimable_count"] != float64(1) {
		t.Errorf("claimable_count = %v, want 1 after the claim", body["claimable_count"])
	}

	// ?claimable=1 filters the {issues} list itself to the claimable set —
	// the SPA contract: same envelope, server-side filtered, hint = length.
	resp = x.do("GET", "/api/v1/repos/"+x.repo.ID+"/ready?claimable=1", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body = decodeBody(t, resp)
	issues, _ := body["issues"].([]any)
	if len(issues) != 1 {
		t.Fatalf("claimable issues = %d, want 1 (claimed #1 filtered out)", len(issues))
	}
	if first, _ := issues[0].(map[string]any); first["number"] != float64(2) {
		t.Errorf("claimable issue = %v, want #2", first["number"])
	}
	if body["claimable_count"] != float64(1) {
		t.Errorf("claimable_count = %v, want 1 with ?claimable=1", body["claimable_count"])
	}
}
