package httpapi

// httptest suite for the M3 instance/parked/providers surface: fake tmux + fake
// provider + real store + real gitx on a fixture bare repo. Verifies status
// codes, JSON shapes, the live-join, error mapping, and the run.changed /
// parked.changed events (the SSE feed's source).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/chat"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/reconcile"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

var instClock = time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)

type instTestServer struct {
	*testServer
	runner       *tmuxx.Fake
	prov         *providertest.Fake
	chatSvc      *chat.Service
	repo         store.Repo
	worktreeRoot string
}

// firstSessionName is the deterministic session an unlabelled Start yields
// under instClock.
const firstSessionName = "proj~20260608-1530"

func newInstanceServer(t *testing.T) *instTestServer {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeRepoOrigin(t, home, "main", 2)
	env := testutil.HermeticGitEnv(home)

	stateDir := t.TempDir()
	reposDir := filepath.Join(stateDir, "repos")
	worktreeRoot := filepath.Join(stateDir, "worktrees")
	runtime := filepath.Join(stateDir, "runtime")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}

	git := gitx.New("git")
	runner := tmuxx.NewFake()
	prov := providertest.New()
	var repo store.Repo
	var chatSvc *chat.Service

	x := newTestServer(t, func(o *Options) {
		st := o.Store
		if err := st.SeedDefaultSettings(context.Background(), 6); err != nil {
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
			Provider: "claude-code", AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
			CloneStatus: store.CloneStatusReady, CreatedAt: instClock,
		})
		if err != nil {
			t.Fatalf("CreateRepo: %v", err)
		}
		vlt, err := vault.New(make([]byte, vault.KeySize))
		if err != nil {
			t.Fatal(err)
		}
		mat, err := vault.NewMaterializer(runtime)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := provider.NewRegistry(prov)
		if err != nil {
			t.Fatal(err)
		}
		guard := startguard.New()
		svc, err := instance.New(instance.Options{
			Store: st, Git: git, Runner: runner, Providers: reg, Vault: vlt, Materializer: mat,
			Guard: guard, Bus: o.Bus, Logger: logx.New(io.Discard), ReposDir: reposDir,
			WorktreeRoot: worktreeRoot, LabURL: "http://127.0.0.1:8080", GitEnv: env,
			CaptureCtx: context.Background(), Now: func() time.Time { return instClock },
		})
		if err != nil {
			t.Fatal(err)
		}
		rec, err := reconcile.New(reconcile.Options{
			Store: st, Git: git, Runner: runner, Guard: guard, Materializer: mat, Bus: o.Bus,
			Logger: logx.New(io.Discard), ReposDir: reposDir, GitEnv: env,
			ArmCapture: svc.ArmCapture, Now: func() time.Time { return instClock },
		})
		if err != nil {
			t.Fatal(err)
		}
		cs, err := chat.New(chat.Options{
			Store: st, Providers: reg, Bus: o.Bus, Logger: logx.New(io.Discard),
			Now: func() time.Time { return instClock },
		})
		if err != nil {
			t.Fatal(err)
		}
		svc.SetChatState(cs)
		chatSvc = cs
		o.Instances = svc
		o.Reconcile = rec
		o.Providers = reg
		o.Chat = cs
	})
	x.setup("op", "password123")
	return &instTestServer{
		testServer: x, runner: runner, prov: prov, chatSvc: chatSvc, repo: repo, worktreeRoot: worktreeRoot,
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestAPI_InstanceStartHappyPath(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	log := recordBus(t, x.bus)

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusCreated)
	run := decodeBody(t, resp)
	if run["session_name"] != firstSessionName || run["branch"] != "lab/20260608-1530" {
		t.Fatalf("run = %v", run)
	}
	if run["outcome"] != "active" || run["kind"] != "manual" {
		t.Errorf("run outcome/kind = %v/%v", run["outcome"], run["kind"])
	}

	// GET /instances joins the live session + provider connecting state.
	resp = x.do("GET", "/api/v1/instances", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	items, ok := list["instances"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("instances = %v", list)
	}
	inst := items[0].(map[string]any)
	if inst["repo_name"] != "proj" || inst["live"] != true {
		t.Errorf("instance view = %v, want repo proj + live", inst)
	}

	// run.changed was published (the SSE feed's source).
	if !sawEvent(log, "run.changed") {
		t.Error("no run.changed event on instance start")
	}
}

func TestAPI_InstanceStartRollbackOnSpawnFailure(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	x.runner.FailStart(firstSessionName, errors.New("session exited immediately"))

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusInternalServerError)
	body := decodeBody(t, resp)
	if body["error"] == "" {
		t.Error("spawn failure returned no error message")
	}

	// Full rollback: no active run remains.
	resp = x.do("GET", "/api/v1/instances", nil, nil)
	list := decodeBody(t, resp)
	if items, _ := list["instances"].([]any); len(items) != 0 {
		t.Errorf("rollback left %d instances", len(items))
	}
}

func TestAPI_InstanceStartOverCap(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	if err := x.st.SetSetting(context.Background(), store.SettingMaxInstances, "1"); err != nil {
		t.Fatal(err)
	}
	x.runner.AddLive("proj~existing-20260608-1500")

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusConflict)
}

func TestAPI_InstanceStartLoggedOut(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	x.prov.SetLoggedIn(false)

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusConflict)
}

func TestAPI_InstanceStartRepoNotReady(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	if err := x.st.UpdateRepoCloneStatus(context.Background(), x.repo.ID, store.CloneStatusCloning, ""); err != nil {
		t.Fatal(err)
	}
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusConflict)
}

func TestAPI_InstanceStopRemovedAndParked(t *testing.T) {
	t.Run("clean merged → removed", func(t *testing.T) {
		x := newInstanceServer(t)
		h := csrfHeaders(x.ts.URL)
		start(t, x, h)
		resp := x.do("DELETE", "/api/v1/instances/"+firstSessionName, nil, h)
		wantStatus(t, resp, http.StatusOK)
		if got := decodeBody(t, resp); got["outcome"] != "removed" {
			t.Errorf("outcome = %v, want removed", got["outcome"])
		}
	})
	t.Run("dirty → parked", func(t *testing.T) {
		x := newInstanceServer(t)
		h := csrfHeaders(x.ts.URL)
		start(t, x, h)
		wt := filepath.Join(x.worktreeRoot, "proj-20260608-1530")
		writeFile(t, filepath.Join(wt, "scratch.txt"), "wip")
		resp := x.do("DELETE", "/api/v1/instances/"+firstSessionName, nil, h)
		wantStatus(t, resp, http.StatusOK)
		if got := decodeBody(t, resp); got["outcome"] != "parked" {
			t.Errorf("outcome = %v, want parked", got["outcome"])
		}
	})
}

func TestAPI_InstanceStopAFKRefused(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	name := "proj~afk-7"
	x.runner.AddLive(name)
	if _, err := x.st.CreateRun(context.Background(), store.Run{
		ID: ids.NewID("run"), RepoID: x.repo.ID, Kind: store.RunKindAFKManual, Provider: "claude-code",
		Branch: "afk/7", WorktreePath: "/wt/proj-7", SessionName: name, Model: "opus[1m]", Effort: "max",
		StartedAt: instClock, Outcome: store.RunOutcomeActive,
	}); err != nil {
		t.Fatal(err)
	}
	resp := x.do("DELETE", "/api/v1/instances/"+name, nil, h)
	wantStatus(t, resp, http.StatusNotImplemented)
}

func TestAPI_StopAll(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	startLabelled(t, x, h, "a")
	startLabelled(t, x, h, "b")
	x.runner.AddLive(tmuxx.LoginSession)

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/stop-all", nil, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["stopped"] != float64(2) {
		t.Errorf("stopped = %v, want 2", got["stopped"])
	}
	if _, live := x.runner.Session(tmuxx.LoginSession); !live {
		t.Error("stop-all killed the login session")
	}
}

func TestAPI_ParkedAndDiscard(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	log := recordBus(t, x.bus)
	// Start then stop-dirty so lab/…-1530 parks.
	start(t, x, h)
	wt := filepath.Join(x.worktreeRoot, "proj-20260608-1530")
	writeFile(t, filepath.Join(wt, "scratch.txt"), "wip")
	resp := x.do("DELETE", "/api/v1/instances/"+firstSessionName, nil, h)
	wantStatus(t, resp, http.StatusOK)

	// Parked lists the kept branch with its stats.
	resp = x.do("GET", "/api/v1/repos/"+x.repo.ID+"/parked", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	parked := decodeBody(t, resp)
	items, _ := parked["parked"].([]any)
	if len(items) != 1 {
		t.Fatalf("parked = %v, want one entry", parked)
	}
	entry := items[0].(map[string]any)
	if entry["branch"] != "lab/20260608-1530" || entry["dirty"] != true {
		t.Errorf("parked entry = %v", entry)
	}

	// Non-managed branch → 400.
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/parked/discard", map[string]any{"branch": "main"}, h)
	wantStatus(t, resp, http.StatusBadRequest)

	// Discard the managed branch (unguarded: dirty removed anyway) → 204.
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/parked/discard", map[string]any{"branch": "lab/20260608-1530"}, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	resp = x.do("GET", "/api/v1/repos/"+x.repo.ID+"/parked", nil, nil)
	if p := decodeBody(t, resp); len(p["parked"].([]any)) != 0 {
		t.Errorf("parked not empty after discard: %v", p)
	}
	if !sawEvent(log, "parked.changed") {
		t.Error("no parked.changed event on discard")
	}
}

func TestAPI_RunsHistory(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	start(t, x, h)
	resp := x.do("DELETE", "/api/v1/instances/"+firstSessionName, nil, h)
	wantStatus(t, resp, http.StatusOK)

	resp = x.do("GET", "/api/v1/runs?repo="+x.repo.ID+"&limit=50", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	runs := decodeBody(t, resp)
	items, _ := runs["runs"].([]any)
	if len(items) != 1 {
		t.Fatalf("runs = %v, want one", runs)
	}
	if items[0].(map[string]any)["outcome"] != "stopped" {
		t.Errorf("run outcome = %v, want stopped", items[0])
	}

	// repo is required.
	resp = x.do("GET", "/api/v1/runs", nil, nil)
	wantStatus(t, resp, http.StatusBadRequest)
}

func TestAPI_Providers(t *testing.T) {
	x := newInstanceServer(t)

	resp := x.do("GET", "/api/v1/providers", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	provs, _ := body["providers"].([]any)
	if len(provs) != 1 {
		t.Fatalf("providers = %v", body)
	}
	p := provs[0].(map[string]any)
	if p["id"] != "claude-code" {
		t.Errorf("provider id = %v", p["id"])
	}
	if models, _ := p["models"].([]any); len(models) != 4 {
		t.Errorf("models = %v, want 4", p["models"])
	}
	// A DeepLinker provider (the Fake) exposes fallback-open metadata (ADR-0017)
	// so the SPA needs no hardcoded provider URL/title.
	fo, ok := p["fallback_open"].(map[string]any)
	if !ok {
		t.Fatalf("fallback_open missing for a DeepLinker provider: %v", p)
	}
	if fo["url"] != "https://claude.ai/code" || fo["title"] == "" {
		t.Errorf("fallback_open = %v, want the claude.ai picker url + a title", fo)
	}
}

func TestAPI_ClaudeAuthStatusAndLogin(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("GET", "/api/v1/providers/claude/auth/status", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if st := decodeBody(t, resp); st["logged_in"] != true || st["checked_at"] == "" {
		t.Errorf("auth status = %v", st)
	}

	// Login start while logged in → 409.
	resp = x.do("POST", "/api/v1/providers/claude/auth/login/start", map[string]any{}, h)
	wantStatus(t, resp, http.StatusConflict)

	// Logged out → 200 with the oauth url.
	x.prov.SetLoggedIn(false)
	resp = x.do("POST", "/api/v1/providers/claude/auth/login/start", map[string]any{}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["oauth_url"] == "" {
		t.Errorf("oauth_url empty: %v", got)
	}

	// Code submit → 202.
	resp = x.do("POST", "/api/v1/providers/claude/auth/login/code", map[string]any{"code": "abc#state=1"}, h)
	wantStatus(t, resp, http.StatusAccepted)
	_ = resp.Body.Close()
	if codes := x.prov.SubmittedCodes(); len(codes) != 1 || codes[0] != "abc#state=1" {
		t.Errorf("submitted codes = %v", codes)
	}
}

// --- helpers -------------------------------------------------------------

func start(t *testing.T, x *instTestServer, h map[string]string) {
	t.Helper()
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()
}

func startLabelled(t *testing.T, x *instTestServer, h map[string]string, label string) {
	t.Helper()
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{"label": label}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()
}

func sawEvent(log *busLog, typ string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range log.snapshot() {
			if ev.Type == typ {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
