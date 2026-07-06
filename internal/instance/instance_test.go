package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// clockTime is the frozen clock the fixtures use so derived labels are
// deterministic: an unlabelled Start yields label 20260608-1530.
var clockTime = time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)

type fixture struct {
	t                      *testing.T
	svc                    *Service
	st                     *store.Store
	runner                 *tmuxx.Fake
	prov                   *providertest.Fake
	guard                  *startguard.Guard
	bus                    *events.Bus
	clock                  *testutil.FakeClock
	home                   string
	env                    []string
	reposDir, worktreeRoot string
	repo                   store.Repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeOrigin(t, home, "main", 2)
	env := testutil.HermeticGitEnv(home)

	stateDir := t.TempDir()
	reposDir := filepath.Join(stateDir, "repos")
	worktreeRoot := filepath.Join(stateDir, "worktrees")
	runtime := filepath.Join(stateDir, "runtime")

	git := gitx.New("git")
	st := testutil.TempStore(t)
	if err := st.SeedDefaultSettings(t.Context(), 6); err != nil {
		t.Fatalf("SeedDefaultSettings: %v", err)
	}
	repoID := ids.NewID("repo")
	bare := filepath.Join(reposDir, repoID+".git")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatalf("mkdir repos: %v", err)
	}
	if err := git.CloneBare(t.Context(), "file://"+origin, bare, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	repo, err := st.CreateRepo(t.Context(), store.Repo{
		ID: repoID, Name: "proj", RemoteURL: "file://" + origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		Provider: "claude-code", AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: clockTime,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	vlt, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	mat, err := vault.NewMaterializer(runtime)
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	prov := providertest.New()
	reg, err := provider.NewRegistry(prov)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	runner := tmuxx.NewFake()
	guard := startguard.New()
	bus := events.NewBus()
	clock := testutil.NewFakeClock(clockTime)

	svc, err := New(Options{
		Store: st, Git: git, Runner: runner, Providers: reg, Vault: vlt, Materializer: mat,
		Guard: guard, Bus: bus, ReposDir: reposDir, WorktreeRoot: worktreeRoot,
		LabURL: "http://127.0.0.1:8080", GitEnv: env, CaptureCtx: context.Background(),
		Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("instance.New: %v", err)
	}
	return &fixture{
		t: t, svc: svc, st: st, runner: runner, prov: prov, guard: guard, bus: bus, clock: clock,
		home: home, env: env, reposDir: reposDir, worktreeRoot: worktreeRoot, repo: repo,
	}
}

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

func makeOrigin(t *testing.T, home, branch string, commits int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	gitCmd(t, home, "", "init", "-q", "-b", branch, dir)
	for i := range commits {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), fmt.Appendf(nil, "c%d\n", i), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
		gitCmd(t, home, dir, "add", ".")
		gitCmd(t, home, dir, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}
	return dir
}

func (f *fixture) bare() string { return filepath.Join(f.reposDir, f.repo.ID+".git") }

func (f *fixture) branchExists(branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = f.bare()
	cmd.Env = append(os.Environ(), f.env...)
	return cmd.Run() == nil
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStart_happyPath(t *testing.T) {
	f := newFixture(t)
	f.prov.SetDeepLink("https://claude.ai/code/session_real")

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	name := "proj~20260608-1530"
	if run.SessionName != name || run.Branch != "lab/20260608-1530" {
		t.Fatalf("run = %+v", run)
	}
	if run.Model != "opus[1m]" || run.Effort != "max" {
		t.Errorf("resolved model/effort = %q/%q, want settings defaults (missing → provider-valid)", run.Model, run.Effort)
	}

	// The forced pre-spawn auth refresh happened.
	if f.prov.ForcedAuthChecks() == 0 {
		t.Error("Start did not force an auth refresh before spawning")
	}
	// Row is active.
	got, err := f.st.RunBySession(t.Context(), name)
	if err != nil {
		t.Fatalf("RunBySession: %v", err)
	}
	if got.Outcome != store.RunOutcomeActive {
		t.Errorf("outcome = %q, want active", got.Outcome)
	}
	// Worktree on disk, branch created, session live.
	wt := filepath.Join(f.worktreeRoot, "proj-20260608-1530")
	if !dirExists(wt) {
		t.Errorf("worktree %s not created", wt)
	}
	if !f.branchExists("lab/20260608-1530") {
		t.Error("branch lab/20260608-1530 not created")
	}
	sess, live := f.runner.Session(name)
	if !live {
		t.Fatal("tmux session not live after Start")
	}
	if sess.Dir != wt {
		t.Errorf("spawn cwd = %q, want the worktree %q", sess.Dir, wt)
	}
	// Spawn argv is the provider's (recorded verbatim by the fake runner).
	wantArgv := f.prov.SpawnArgv(name, "opus[1m]", "max")
	if strings.Join(sess.Argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("spawn argv = %v, want %v", sess.Argv, wantArgv)
	}
	// Session env carries LAB_URL + a minted LAB_TOKEN.
	if v := envValue(sess.ExtraEnv, "LAB_URL"); v != "http://127.0.0.1:8080" {
		t.Errorf("LAB_URL = %q", v)
	}
	token := envValue(sess.ExtraEnv, "LAB_TOKEN")
	if !strings.HasPrefix(token, "lab_run_") {
		t.Fatalf("LAB_TOKEN = %q, want a lab_run_ token", token)
	}
	// The token resolves to this active run (manual → NULL expiry).
	info, err := f.st.RunTokenByHash(t.Context(), ids.HashToken(token))
	if err != nil {
		t.Fatalf("RunTokenByHash: %v", err)
	}
	if info.RunID != run.ID || info.Outcome != store.RunOutcomeActive || info.ExpiresAt != nil {
		t.Errorf("token info = %+v", info)
	}
	// The async capture persists the real deep link and re-publishes.
	waitFor(t, "deep link captured", func() bool {
		r, err := f.st.RunBySession(t.Context(), name)
		return err == nil && r.DeepLinkURL != nil && *r.DeepLinkURL == "https://claude.ai/code/session_real"
	})
}

func TestStart_rollbackOnSpawnFailure(t *testing.T) {
	f := newFixture(t)
	name := "proj~20260608-1530"
	f.runner.FailStart(name, errors.New("boom: session exited"))

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var startFailed *StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("Start err = %v, want StartFailedError", err)
	}
	// Full rollback to pre-Start state.
	if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
		t.Error("worktree survived spawn-failure rollback")
	}
	if f.branchExists("lab/20260608-1530") {
		t.Error("branch survived spawn-failure rollback")
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("run row survived rollback: %d active", len(active))
	}
	if snap := f.guard.Snapshot(); len(snap) != 0 {
		t.Errorf("startguard not cleared after rollback: %v", snap)
	}
}

// Regression: runner.Start can return an error while the tmux session is
// already live (daemonized new-session succeeds, then the request context is
// cancelled during tmuxx's post-spawn recheck). Rollback MUST kill the session
// — otherwise a live agent is orphaned in a just-deleted worktree, invisible to
// re-adoption and the sweep yet still counting against the cap.
func TestStart_rollbackKillsLiveSessionOnSpawnError(t *testing.T) {
	f := newFixture(t)
	name := "proj~20260608-1530"
	f.runner.FailStartLive(name, errors.New("context canceled after new-session"))

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var startFailed *StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("Start err = %v, want StartFailedError", err)
	}
	if live, _ := f.runner.IsRunning(t.Context(), name); live {
		t.Error("live session orphaned by spawn-error rollback (agent left running in a deleted worktree)")
	}
	if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
		t.Error("worktree survived rollback")
	}
	if f.branchExists("lab/20260608-1530") {
		t.Error("branch survived rollback")
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("run row survived rollback: %d active", len(active))
	}
	if snap := f.guard.Snapshot(); len(snap) != 0 {
		t.Errorf("startguard not cleared after rollback: %v", snap)
	}
}

func TestStart_rollbackOnSeedFailure(t *testing.T) {
	f := newFixture(t)
	f.prov.SetSeedError(providertest.ErrSeed)

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err == nil {
		t.Fatal("Start succeeded despite a seed failure")
	}
	if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
		t.Error("worktree survived seed-failure rollback")
	}
	if f.branchExists("lab/20260608-1530") {
		t.Error("branch survived seed-failure rollback")
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Error("run row created despite seed failure (row is created after seeding)")
	}
}

func TestStart_failsLoudWhenWorktreeCreationFails(t *testing.T) {
	f := newFixture(t)
	// Point the bare repo's origin at nowhere so the fail-loud fetch aborts
	// AddWorktree before anything is created.
	gitCmd(t, f.home, f.bare(), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var startFailed *StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("Start err = %v, want StartFailedError with the git cause", err)
	}
	if !strings.Contains(err.Error(), "does not appear to be a git repository") {
		t.Errorf("error does not surface git stderr verbatim: %v", err)
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Error("a failed worktree creation left a run row")
	}
}

func TestStart_overCapGlobalAndRepoOverride(t *testing.T) {
	t.Run("global cap", func(t *testing.T) {
		f := newFixture(t)
		if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
			t.Fatal(err)
		}
		f.runner.AddLive("proj~existing-20260608-1500")
		_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		if !errors.Is(err, ErrOverCap) {
			t.Fatalf("Start err = %v, want ErrOverCap", err)
		}
	})
	t.Run("per-repo override", func(t *testing.T) {
		f := newFixture(t)
		// Global stays high; the repo override of 1 is the binding cap.
		one := 1
		if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
			MaxInstancesOverride: store.Set(&one),
		}); err != nil {
			t.Fatal(err)
		}
		f.runner.AddLive("proj~existing-20260608-1500")
		_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		if !errors.Is(err, ErrOverCap) {
			t.Fatalf("Start err = %v, want ErrOverCap", err)
		}
	})
}

func TestStart_loggedOut(t *testing.T) {
	f := newFixture(t)
	f.prov.SetLoggedIn(false)
	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if !errors.Is(err, ErrLoggedOut) {
		t.Fatalf("Start err = %v, want ErrLoggedOut", err)
	}
	if f.prov.ForcedAuthChecks() == 0 {
		t.Error("logged-out refusal did not force an auth refresh")
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Error("logged-out Start created a run row")
	}
}

func TestStart_unknownModelIsBadRequest(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Model: "gpt-4"})
	var bad *BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("Start err = %v, want BadRequestError", err)
	}
}

func TestStart_repoNotReady(t *testing.T) {
	f := newFixture(t)
	if err := f.st.UpdateRepoCloneStatus(t.Context(), f.repo.ID, store.CloneStatusCloning, ""); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if !errors.Is(err, ErrRepoNotReady) {
		t.Fatalf("Start err = %v, want ErrRepoNotReady", err)
	}
}

func TestStop_cleanMergedRemoves(t *testing.T) {
	f := newFixture(t)
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	outcome, err := f.svc.Stop(t.Context(), run.SessionName)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if outcome != OutcomeRemoved {
		t.Errorf("outcome = %q, want removed (clean + merged fresh fork)", outcome)
	}
	if f.branchExists("lab/20260608-1530") {
		t.Error("clean+merged branch not deleted")
	}
	if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
		t.Error("clean worktree not removed")
	}
	if _, live := f.runner.Session(run.SessionName); live {
		t.Error("tmux session still live after Stop")
	}
	// Run is terminal; its token is gone.
	if _, err := f.st.RunBySession(t.Context(), run.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("run still active after Stop: %v", err)
	}
	hist, _ := f.st.RunsByRepo(t.Context(), f.repo.ID, 10)
	if len(hist) != 1 || hist[0].Outcome != store.RunOutcomeStopped {
		t.Errorf("history outcome = %v, want stopped", hist)
	}
}

func TestStop_dirtyParks(t *testing.T) {
	f := newFixture(t)
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	wt := filepath.Join(f.worktreeRoot, "proj-20260608-1530")
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("dirty the worktree: %v", err)
	}
	outcome, err := f.svc.Stop(t.Context(), run.SessionName)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if outcome != OutcomeParked {
		t.Errorf("outcome = %q, want parked (dirty worktree kept)", outcome)
	}
	if !f.branchExists("lab/20260608-1530") {
		t.Error("dirty branch was deleted")
	}
	if !dirExists(wt) {
		t.Error("dirty worktree was removed")
	}
}

func TestStop_afkRefused(t *testing.T) {
	f := newFixture(t)
	// A live AFK run (M5 territory): Stop must refuse.
	name := "proj~afk-7"
	f.runner.AddLive(name)
	if _, err := f.st.CreateRun(t.Context(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: store.RunKindAFKManual, Provider: "claude-code",
		Branch: "afk/7", WorktreePath: "/wt/proj-7", SessionName: name, Model: "opus[1m]", Effort: "max",
		StartedAt: clockTime, Outcome: store.RunOutcomeActive,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Stop(t.Context(), name)
	if !errors.Is(err, ErrAFKStopUnsupported) {
		t.Fatalf("Stop(afk) err = %v, want ErrAFKStopUnsupported", err)
	}
}

func TestStopAll_skipsLoginAndCountsManual(t *testing.T) {
	f := newFixture(t)
	a, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "a"})
	if err != nil {
		t.Fatalf("Start a: %v", err)
	}
	b, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "b"})
	if err != nil {
		t.Fatalf("Start b: %v", err)
	}
	f.runner.AddLive(tmuxx.LoginSession) // must be excluded

	n, err := f.svc.StopAll(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if n != 2 {
		t.Errorf("stopped = %d, want 2", n)
	}
	if _, live := f.runner.Session(tmuxx.LoginSession); !live {
		t.Error("StopAll killed the login session")
	}
	for _, run := range []store.Run{a, b} {
		if _, live := f.runner.Session(run.SessionName); live {
			t.Errorf("session %s still live after StopAll", run.SessionName)
		}
	}
}

func TestList_joinsLiveAndConnecting(t *testing.T) {
	f := newFixture(t)
	// Keep capture pending so the run stays "connecting" and its link NULL.
	f.prov.SetDeepLink("")
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.prov.SetConnecting(run.SessionName, true)

	views, err := f.svc.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("List = %d views, want 1", len(views))
	}
	v := views[0]
	if v.RepoName != "proj" || !v.Live || !v.Connecting {
		t.Errorf("view = %+v, want repo proj, live, connecting", v)
	}

	// A dead session (killed out from under lab) shows live=false.
	f.runner.Kill(run.SessionName)
	views, _ = f.svc.List(t.Context())
	if len(views) != 1 || views[0].Live {
		t.Errorf("dead session still reports live: %+v", views)
	}
}
