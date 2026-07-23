package httpapi

// httptest suite for the M3 instance/parked/providers surface: fake tmux + fake
// provider + real store + real gitx on a fixture bare repo. Verifies status
// codes, JSON shapes, the live-join, error mapping, and the run.changed /
// parked.changed events (the SSE feed's source).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/chat"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/pull"
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
	// home, origin, reposDir, eng and env expose the fixture's git topology
	// (issue #149's commits_behind tests): origin is the non-bare fixture
	// remote makeRepoOrigin built (commit straight into it to advance),
	// reposDir/repo.ID+".git" is the lab bare reference clone commits_behind
	// reads, and eng/env are what fetches it.
	home     string
	origin   string
	reposDir string
	eng      *gitx.Engine
	env      []string
}

// firstSessionName is the deterministic session an unlabelled Start yields
// under instClock.
const firstSessionName = "proj~20260608-1530"

func newInstanceServer(t *testing.T) *instTestServer {
	t.Helper()
	return newInstanceServerWith(t)
}

// newInstanceServerWith registers extraProvs AFTER the primary claude-code
// fake (issue #66: multi-provider registries for the provider-pick tests).
func newInstanceServerWith(t *testing.T, extraProvs ...provider.AgentProvider) *instTestServer {
	t.Helper()
	return newInstanceServerMod(t, nil, extraProvs...)
}

// newInstanceServerMod additionally lets a test rewrite the assembled Options
// after the fixture wiring completed (issue #149: the no-pull-service server).
func newInstanceServerMod(t *testing.T, mod func(*Options), extraProvs ...provider.AgentProvider) *instTestServer {
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
		reg, err := provider.NewRegistry(append([]provider.AgentProvider{prov}, extraProvs...)...)
		if err != nil {
			t.Fatal(err)
		}
		guard := startguard.New()
		homes := instancehome.New(filepath.Join(stateDir, "instances"))
		svc, err := instance.New(instance.Options{
			Store: st, Git: git, Runner: runner, Providers: reg, Vault: vlt, Materializer: mat,
			Homes: homes, Guard: guard, Bus: o.Bus, Logger: logx.New(io.Discard), ReposDir: reposDir,
			WorktreeRoot: worktreeRoot, LabURL: "http://127.0.0.1:8080", GitEnv: env,
			CaptureCtx: context.Background(), Now: func() time.Time { return instClock },
		})
		if err != nil {
			t.Fatal(err)
		}
		rec, err := reconcile.New(reconcile.Options{
			Store: st, Git: git, Runner: runner, Guard: guard, Materializer: mat, Homes: homes, Bus: o.Bus,
			Logger: logx.New(io.Discard), ReposDir: reposDir, GitEnv: env,
			ArmCapture: svc.ArmCapture, Now: func() time.Time { return instClock },
		})
		if err != nil {
			t.Fatal(err)
		}
		cs, err := chat.New(chat.Options{
			Store: st, Providers: reg, Bus: o.Bus, Logger: logx.New(io.Discard),
			RuntimeDirFor: homes.RuntimePath, HomeFor: homes.HomePath, Now: func() time.Time { return instClock },
		})
		if err != nil {
			t.Fatal(err)
		}
		svc.SetChatState(cs)
		chatSvc = cs
		o.Instances = svc
		o.Reconcile = rec
		o.Providers = reg
		o.Homes = homes
		o.Chat = cs
		// Wires commits_behind's git dependency (issue #149): the real engine
		// + this fixture's reposDir/env, same as instance's own Start/List.
		o.Git = git
		o.ReposDir = reposDir
		o.GitEnv = env
		// The /pull-base lab command (issue #149): the same real-git service
		// production wires, on this fixture's bare/worktree topology. Vault/
		// Materializer stay nil — the file:// origin needs no credential.
		o.Pull = pull.New(pull.Options{
			Store: st, Git: git, Bus: o.Bus, ReposDir: reposDir, GitEnv: env,
			Logger: logx.New(io.Discard),
		})
		if mod != nil {
			mod(o)
		}
	})
	x.setup("op", "password123")
	return &instTestServer{
		testServer: x, runner: runner, prov: prov, chatSvc: chatSvc, repo: repo, worktreeRoot: worktreeRoot,
		home: home, origin: origin, reposDir: reposDir, eng: git, env: env,
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

// The per-spawn provider pick (issue #66): POST /repos/{id}/instances
// {provider} runs on the named provider with skip-layer model/effort; an
// unknown pick is a strict 400; an absent pick inherits the repo/global chain
// (the seeded provider_default = claude-code here).
func TestAPI_InstanceStartProviderPick(t *testing.T) {
	b := providertest.New()
	b.SetID("fake-b")
	b.SetCatalogs(
		[]provider.Option{{Value: "b-fast", Label: "B Fast"}, {Value: "b-deep", Label: "B Deep"}},
		[]provider.Option{},
	)
	x := newInstanceServerWith(t, b)
	h := csrfHeaders(x.ts.URL)

	// Unknown pick → 400, nothing spawned.
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{"provider": "ghost"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Fatal("unknown provider 400 without error message")
	}

	// Absent pick inherits: the repo has no override, the seeded global
	// provider_default is claude-code.
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusCreated)
	if run := decodeBody(t, resp); run["provider"] != "claude-code" {
		t.Errorf("inherited provider = %v, want claude-code", run["provider"])
	}

	// Explicit pick runs on provider B: the run row records the resolved
	// provider and the skip-layer model/effort (B's first model, effort ""
	// for an empty efforts catalog) even though the seeded global model
	// default is the claude-shaped opus[1m].
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances",
		map[string]any{"provider": "fake-b", "label": "on-b"}, h)
	wantStatus(t, resp, http.StatusCreated)
	run := decodeBody(t, resp)
	if run["provider"] != "fake-b" {
		t.Errorf("picked provider = %v, want fake-b", run["provider"])
	}
	if run["model"] != "b-fast" || run["effort"] != "" {
		t.Errorf("model/effort = %v/%v, want b-fast/\"\" (skip-layer against B)", run["model"], run["effort"])
	}
	if got := len(b.SpawnSpecs()); got != 1 {
		t.Errorf("provider B spawned %d times, want 1", got)
	}
}

// Issue #96: POST /repos/{id}/instances {first_message} carries the operator's
// first chat message into the spawn's trailing positional
// (provider.SpawnSpec.InitialPrompt), verified via the fake provider's
// recorded SpawnSpecs. Shape-only validation (ADR-0027): whitespace-only
// normalizes to no prompt (spawn still succeeds); an oversized message is a
// 400 that creates nothing.
func TestAPI_InstanceStartFirstMessage(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)

	// A real message reaches InitialPrompt verbatim.
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances",
		map[string]any{"first_message": "fix the flaky test"}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()
	specs := x.prov.SpawnSpecs()
	if got := len(specs); got != 1 {
		t.Fatalf("SpawnArgv called %d times, want 1", got)
	}
	if specs[0].InitialPrompt != "fix the flaky test" {
		t.Errorf("InitialPrompt = %q, want the first_message verbatim", specs[0].InitialPrompt)
	}

	// Whitespace-only first_message normalizes to "" (no trailing prompt);
	// the spawn still succeeds (a second instance on the same repo).
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances",
		map[string]any{"first_message": "   \n\t  ", "label": "ws"}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()
	specs = x.prov.SpawnSpecs()
	if got := len(specs); got != 2 {
		t.Fatalf("SpawnArgv called %d times, want 2", got)
	}
	if specs[1].InitialPrompt != "" {
		t.Errorf("whitespace-only first_message → InitialPrompt = %q, want \"\"", specs[1].InitialPrompt)
	}

	// An oversized first_message is a 400; nothing is created or spawned.
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances",
		map[string]any{"first_message": strings.Repeat("x", afkPromptMaxBytes+1), "label": "big"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Error("oversized first_message 400 without error message")
	}
	if got := len(x.prov.SpawnSpecs()); got != 2 {
		t.Errorf("SpawnArgv called %d times after the oversized rejection, want still 2 (nothing spawned)", got)
	}
	resp = x.do("GET", "/api/v1/instances", nil, nil)
	list := decodeBody(t, resp)
	items, _ := list["instances"].([]any)
	if len(items) != 2 {
		t.Errorf("instances after the rejected 400 = %d, want 2 (nothing created)", len(items))
	}
}

// The per-spawn remote-control pick (issue #163): POST /repos/{id}/instances
// {remote} reaches the provider's SpawnSpec and is STAMPED on the run row. The
// knob is boolean, so the tri-state is what is actually under test — an absent
// pick inherits (the seeded spawn_remote_default is false, so a plain start is
// NOT remote and, capturing nothing, keeps deep_link_url null), an explicit true
// turns it on, and an explicit FALSE beats a global true rather than reading as
// "no pick".
func TestAPI_InstanceStartRemotePick(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)

	// Absent pick → the seeded base default (false): the run is off the remote
	// surface, and ArmCapture never runs, so the deep link stays null.
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusCreated)
	run := decodeBody(t, resp)
	if v, ok := run["remote"].(bool); !ok || v {
		t.Errorf("inherited remote = %v (%T), want JSON false (the seeded spawn_remote_default)", run["remote"], run["remote"])
	}
	if run["deep_link_url"] != nil {
		t.Errorf("deep_link_url = %v on a non-remote run, want null", run["deep_link_url"])
	}
	specs := x.prov.SpawnSpecs()
	if len(specs) != 1 {
		t.Fatalf("SpawnArgv called %d times, want 1", len(specs))
	}
	if specs[0].Remote {
		t.Error("SpawnSpec.Remote = true for an inherited-off start")
	}

	// An explicit true reaches the provider's SpawnSpec and the run row.
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances",
		map[string]any{"remote": true, "label": "on"}, h)
	wantStatus(t, resp, http.StatusCreated)
	if run = decodeBody(t, resp); run["remote"] != true {
		t.Errorf("run.remote = %v after {\"remote\":true}, want true", run["remote"])
	}
	specs = x.prov.SpawnSpecs()
	if len(specs) != 2 {
		t.Fatalf("SpawnArgv called %d times, want 2", len(specs))
	}
	if !specs[1].Remote {
		t.Error("SpawnSpec.Remote = false though the request asked for remote:true")
	}

	// With the global flipped ON, an explicit false still wins — the boolean
	// trap: false is a PICK, not an absence.
	if err := x.st.SetSetting(context.Background(), store.SettingSpawnRemoteDefault, "true"); err != nil {
		t.Fatal(err)
	}
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances",
		map[string]any{"remote": false, "label": "off"}, h)
	wantStatus(t, resp, http.StatusCreated)
	if run = decodeBody(t, resp); run["remote"] != false {
		t.Errorf("run.remote = %v for {\"remote\":false} under a global true, want false", run["remote"])
	}
	specs = x.prov.SpawnSpecs()
	if len(specs) != 3 {
		t.Fatalf("SpawnArgv called %d times, want 3", len(specs))
	}
	if specs[2].Remote {
		t.Error("SpawnSpec.Remote = true though the request explicitly asked for remote:false")
	}

	// …while an ABSENT pick now inherits that same global true.
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{"label": "inherit"}, h)
	wantStatus(t, resp, http.StatusCreated)
	if run = decodeBody(t, resp); run["remote"] != true {
		t.Errorf("inherited remote = %v under a global true, want true", run["remote"])
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
	loginSession := tmuxx.LoginSessionName("claude-code")
	x.runner.AddLive(loginSession)

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/stop-all", nil, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["stopped"] != float64(2) {
		t.Errorf("stopped = %v, want 2", got["stopped"])
	}
	if _, live := x.runner.Session(loginSession); !live {
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

// hasKey reports whether the decoded JSON body carries key at all — the
// omitempty contract commits_behind relies on means "uncomputable" must be
// entirely ABSENT from the wire body, not merely zero-valued, so assertions
// read the raw decoded map rather than a Go struct (which always has the
// field, zero or not).
func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

// TestAPI_RunCommitsBehind pins the commits_behind badge (issue #149) on both
// surfaces that render it — run detail and the instances list row — and its
// omitempty contract: absent while uncomputable/zero, present with the exact
// rev-list count once the bare reference clone has fetched an advanced
// origin, and absent again once the run ends (its branch may already be gone).
func TestAPI_RunCommitsBehind(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, h)
	wantStatus(t, resp, http.StatusCreated)
	run := decodeBody(t, resp)
	runID, _ := run["id"].(string)
	if runID == "" {
		t.Fatalf("run id missing from create response: %v", run)
	}

	// Origin not advanced beyond the branch's fork point: commits_behind is
	// entirely absent, not a present 0.
	resp = x.do("GET", "/api/v1/runs/"+runID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); hasKey(got, "commits_behind") {
		t.Errorf("commits_behind present before origin advances: %v", got["commits_behind"])
	}

	// Advance the fixture origin's main by 3 commits directly (it is the
	// non-bare working repo makeRepoOrigin built), then fetch the lab bare
	// reference clone — the one refresh commits_behind is allowed to read,
	// never a fetch of its own.
	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(x.origin, fmt.Sprintf("adv%d.txt", i)), "advance")
		repoGitCmd(t, x.home, x.origin, "add", ".")
		repoGitCmd(t, x.home, x.origin, "commit", "-q", "-m", fmt.Sprintf("advance %d", i))
	}
	bareDir := filepath.Join(x.reposDir, x.repo.ID+".git")
	if err := x.eng.Fetch(context.Background(), bareDir, x.env); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	resp = x.do("GET", "/api/v1/runs/"+runID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["commits_behind"] != float64(3) {
		t.Errorf("run detail commits_behind = %v, want 3", got["commits_behind"])
	}

	// The instances list row carries the same value under the same
	// conditions (handleInstanceList's own O(1)-query enrichment).
	resp = x.do("GET", "/api/v1/instances", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	items, _ := list["instances"].([]any)
	if len(items) != 1 {
		t.Fatalf("instances = %v, want 1", list)
	}
	if inst := items[0].(map[string]any); inst["commits_behind"] != float64(3) {
		t.Errorf("instance commits_behind = %v, want 3", inst["commits_behind"])
	}

	// An ended run never carries the badge — Stop moves its outcome off
	// active, and the origin is still measurably ahead, so this proves the
	// active-only predicate rather than a coincidental zero.
	resp = x.do("DELETE", "/api/v1/instances/"+firstSessionName, nil, h)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	resp = x.do("GET", "/api/v1/runs/"+runID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); hasKey(got, "commits_behind") {
		t.Errorf("commits_behind present on an ended run: %v", got["commits_behind"])
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
	// A second, codex-shaped provider (NoLinkFake: per-model efforts + a
	// reported default effort) rides along so the enriched model catalog
	// (issue #156) is asserted in both shapes.
	x := newInstanceServerWith(t, providertest.NewNoLink())

	resp := x.do("GET", "/api/v1/providers", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	provs, _ := body["providers"].([]any)
	if len(provs) != 2 {
		t.Fatalf("providers = %v", body)
	}
	p := provs[0].(map[string]any)
	if p["id"] != "claude-code" {
		t.Errorf("provider id = %v", p["id"])
	}
	// display_name drives all user-facing copy (issue #51 decision 9): the SPA
	// renders it instead of hardcoding a provider name.
	if p["display_name"] != "Claude Code" {
		t.Errorf("display_name = %v, want Claude Code", p["display_name"])
	}
	// auth is the always-present auth-flow descriptor (issue #51 decision 7)
	// the SPA's auth card renders from — oauth-code for claude.
	auth, ok := p["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth descriptor missing: %v", p)
	}
	if auth["kind"] != provider.AuthFlowOAuthCode {
		t.Errorf("auth.kind = %v, want %q", auth["kind"], provider.AuthFlowOAuthCode)
	}
	// Models are per-model enriched (issue #156): every claude-shaped entry
	// carries its own five-entry efforts list and NO default_effort key
	// (claude reports none; omitempty drops it from the wire).
	models, _ := p["models"].([]any)
	if len(models) != 4 {
		t.Fatalf("models = %v, want 4", p["models"])
	}
	for i, raw := range models {
		m := raw.(map[string]any)
		if m["value"] == "" || m["label"] == "" {
			t.Errorf("models[%d] = %v, want inlined value/label", i, m)
		}
		if efforts, _ := m["efforts"].([]any); len(efforts) != 5 {
			t.Errorf("models[%d].efforts = %v, want the full five-entry list", i, m["efforts"])
		}
		if _, present := m["default_effort"]; present {
			t.Errorf("models[%d] carries default_effort %v, want the key absent for a model reporting none", i, m["default_effort"])
		}
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
	// supports_remote is the provider.RemoteCapable assertion (issue #163) —
	// always present, and true for this claude-shaped fake: the SPA renders the
	// remote toggle only where the CLI would honor it.
	if v, ok := p["supports_remote"].(bool); !ok || !v {
		t.Errorf("supports_remote = %v (%T) for a RemoteCapable provider, want true", p["supports_remote"], p["supports_remote"])
	}

	// The codex-shaped provider serves its per-model efforts AND the reported
	// default_effort (issue #156).
	q := provs[1].(map[string]any)
	if q["id"] != "codex-fake" {
		t.Fatalf("second provider id = %v, want codex-fake", q["id"])
	}
	// It implements NEITHER DeepLinker NOR RemoteCapable: no fallback_open, and
	// supports_remote is present-and-false (never absent — the SPA reads the key,
	// not its absence) so the toggle disappears wherever it is the effective
	// provider.
	if v, ok := q["supports_remote"].(bool); !ok || v {
		t.Errorf("supports_remote = %v (%T) for a link-less, remote-less provider, want false", q["supports_remote"], q["supports_remote"])
	}
	qModels, _ := q["models"].([]any)
	if len(qModels) != 1 {
		t.Fatalf("codex-fake models = %v, want 1", q["models"])
	}
	qm := qModels[0].(map[string]any)
	if qm["value"] != "gpt-5-codex" {
		t.Errorf("codex-fake model value = %v", qm["value"])
	}
	if efforts, _ := qm["efforts"].([]any); len(efforts) != 1 {
		t.Errorf("codex-fake model efforts = %v, want [medium]", qm["efforts"])
	}
	if qm["default_effort"] != "medium" {
		t.Errorf("codex-fake default_effort = %v, want medium", qm["default_effort"])
	}
}

// The auth routes are keyed by the registered provider id (issue #51 decision
// 7): {id}="claude-code" resolves; an unknown id 404s; the login-code error
// mapping runs on the provider-generic sentinels (400 for a rejected code, 504
// on login timeout), so httpapi couples to no concrete provider.
func TestAPI_ProviderAuthStatusAndLogin(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	const base = "/api/v1/providers/claude-code/auth"

	resp := x.do("GET", base+"/status", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if st := decodeBody(t, resp); st["logged_in"] != true || st["checked_at"] == "" {
		t.Errorf("auth status = %v", st)
	}

	// Login start while logged in → 409.
	resp = x.do("POST", base+"/login/start", map[string]any{}, h)
	wantStatus(t, resp, http.StatusConflict)

	// Logged out → 200 with the oauth url. A provider without the
	// LoginCodeReporter capability (the plain Fake) carries no user_code key.
	x.prov.SetLoggedIn(false)
	resp = x.do("POST", base+"/login/start", map[string]any{}, h)
	wantStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	if got["oauth_url"] == "" {
		t.Errorf("oauth_url empty: %v", got)
	}
	if _, present := got["user_code"]; present {
		t.Errorf("user_code present for a provider without LoginCodeReporter: %v", got)
	}

	// Code submit → 202.
	resp = x.do("POST", base+"/login/code", map[string]any{"code": "abc#state=1"}, h)
	wantStatus(t, resp, http.StatusAccepted)
	_ = resp.Body.Close()
	if codes := x.prov.SubmittedCodes(); len(codes) != 1 || codes[0] != "abc#state=1" {
		t.Errorf("submitted codes = %v", codes)
	}

	// A rejected code → 400 via the generic provider.ErrInvalidCode sentinel.
	x.prov.SetCodeError(provider.ErrInvalidCode)
	resp = x.do("POST", base+"/login/code", map[string]any{"code": "bad"}, h)
	wantStatus(t, resp, http.StatusBadRequest)

	// A login that never lands → 504 via provider.ErrLoginTimeout.
	x.prov.SetCodeError(provider.ErrLoginTimeout)
	resp = x.do("POST", base+"/login/code", map[string]any{"code": "slow"}, h)
	wantStatus(t, resp, http.StatusGatewayTimeout)

	// An unknown provider id 404s on every auth route.
	const nope = "/api/v1/providers/nope/auth"
	resp = x.do("GET", nope+"/status", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	resp = x.do("POST", nope+"/login/start", map[string]any{}, h)
	wantStatus(t, resp, http.StatusNotFound)
	resp = x.do("POST", nope+"/login/code", map[string]any{"code": "x"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	resp = x.do("POST", nope+"/logout", map[string]any{}, h)
	wantStatus(t, resp, http.StatusNotFound)
}

// codeReporterFake adds provider.LoginCodeReporter on top of the Fake — the
// device-code capability the Fake itself does not implement — so the
// login/start user_code passthrough has a provider to advertise it.
type codeReporterFake struct {
	*providertest.Fake
	code string
}

func (f *codeReporterFake) PendingLoginCode() string { return f.code }

// Device-code login support (issue #87): a provider implementing
// provider.LoginCodeReporter gets its pending one-time user code echoed on
// login/start (the operator enters it browser-side at the verification URL —
// it never travels back into lab), and its LoginSubmitCode returning
// provider.ErrLoginCodeUnsupported maps to 409.
func TestAPI_ProviderLoginDeviceCode(t *testing.T) {
	dev := providertest.New()
	dev.SetID("fake-device")
	dev.SetLoggedIn(false)
	x := newInstanceServerWith(t, &codeReporterFake{Fake: dev, code: "WDJB-MJHT"})
	h := csrfHeaders(x.ts.URL)
	const base = "/api/v1/providers/fake-device/auth"

	// login/start carries both the verification URL and the user code.
	resp := x.do("POST", base+"/login/start", map[string]any{}, h)
	wantStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	if got["user_code"] != "WDJB-MJHT" {
		t.Errorf("user_code = %v, want WDJB-MJHT", got["user_code"])
	}
	if got["oauth_url"] == "" {
		t.Errorf("oauth_url empty alongside the user code: %v", got)
	}

	// A device-code flow takes no pasted code → 409 via the generic
	// provider.ErrLoginCodeUnsupported sentinel.
	dev.SetCodeError(provider.ErrLoginCodeUnsupported)
	resp = x.do("POST", base+"/login/code", map[string]any{"code": "x"}, h)
	wantStatus(t, resp, http.StatusConflict)
	if body := decodeBody(t, resp); body["error"] == "" {
		t.Error("409 returned no error message")
	}
}

// Machine-wide logout (issue #46, per-id in issue #51 decision 7): the endpoint
// is CSRF-guarded, calls the provider's Logout, emits provider.auth.changed
// carrying the provider's own id, and leaves the status cache reading
// logged-out. A failed logout surfaces as a 500.
func TestAPI_ProviderLogout(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	log := recordBus(t, x.bus)
	const base = "/api/v1/providers/claude-code/auth"

	// The Fake starts logged in.
	resp := x.do("GET", base+"/status", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if st := decodeBody(t, resp); st["logged_in"] != true {
		t.Fatalf("precondition: auth status = %v; want logged in", st)
	}

	// CSRF-guarded like every mutating route: no CSRF header → 403, and the
	// provider is never called.
	resp = x.do("POST", base+"/logout", map[string]any{}, nil)
	wantStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()
	if n := x.prov.Logouts(); n != 0 {
		t.Fatalf("Logout called %d times on a CSRF-rejected request; want 0", n)
	}

	// Happy path: 200 with the now-logged-out status echoed back.
	resp = x.do("POST", base+"/logout", map[string]any{}, h)
	wantStatus(t, resp, http.StatusOK)
	if st := decodeBody(t, resp); st["logged_in"] != false {
		t.Errorf("logout response = %v; want logged_in false", st)
	}
	if n := x.prov.Logouts(); n != 1 {
		t.Errorf("Logout called %d times; want 1", n)
	}

	// provider.auth.changed fired with the provider's own id in the payload
	// (the SPA scopes its refetch to the right auth card).
	ev := waitForBusEvent(t, log, provider.EventAuthChanged)
	payload, ok := ev.Payload.(provider.AuthChangedPayload)
	if !ok {
		t.Fatalf("auth-changed payload = %T; want provider.AuthChangedPayload", ev.Payload)
	}
	if payload.Provider != "claude-code" || payload.Type != provider.EventAuthChanged {
		t.Errorf("auth-changed payload = %+v; want provider=claude-code type=%s", payload, provider.EventAuthChanged)
	}

	// The status cache now reads logged-out without forcing.
	resp = x.do("GET", base+"/status", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if st := decodeBody(t, resp); st["logged_in"] != false {
		t.Errorf("status after logout = %v; want logged_in false (cache invalidated)", st)
	}

	// A logout the provider could not complete is a 500.
	x.prov.SetLoggedIn(true)
	x.prov.SetLogoutError(errors.New("still logged in after credentials removal"))
	resp = x.do("POST", base+"/logout", map[string]any{}, h)
	wantStatus(t, resp, http.StatusInternalServerError)
	if body := decodeBody(t, resp); body["error"] == "" {
		t.Error("failed logout returned no error message")
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
