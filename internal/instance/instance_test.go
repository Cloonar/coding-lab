package instance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/assets"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
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
	runtime                string
	repo                   store.Repo
}

// fixtureOpts parametrizes the provider under test. The zero value gives the
// default claude-code Fake (a DeepLinker); a link-less provider is supplied via
// prov + providerID + the model/effort defaults its catalog offers. extraProvs
// are registered AFTER the primary provider (issue #66: multi-provider
// registries for the provider-layering tests).
type fixtureOpts struct {
	prov       provider.AgentProvider
	providerID string
	modelDef   *string
	effortDef  *string
	extraProvs []provider.AgentProvider
}

func newFixture(t *testing.T) *fixture {
	return newFixtureWith(t, fixtureOpts{})
}

func newFixtureWith(t *testing.T, o fixtureOpts) *fixture {
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
	if err := st.SeedDefaultSettings(t.Context(), 6, "claude-code"); err != nil {
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
	providerID := o.providerID
	if providerID == "" {
		providerID = "claude-code"
	}
	repo, err := st.CreateRepo(t.Context(), store.Repo{
		ID: repoID, Name: "proj", RemoteURL: "file://" + origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		Provider: &providerID, AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		ModelDefault: o.modelDef, EffortDefault: o.effortDef,
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
	// Default provider is the claude-code Fake (a DeepLinker); a link-less
	// provider is injected via o.prov. f.prov stays nil in the latter case —
	// only claude-code tests reach for the scriptable Fake.
	var prov *providertest.Fake
	agentProv := o.prov
	if agentProv == nil {
		prov = providertest.New()
		agentProv = prov
	}
	reg, err := provider.NewRegistry(append([]provider.AgentProvider{agentProv}, o.extraProvs...)...)
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
		home: home, env: env, reposDir: reposDir, worktreeRoot: worktreeRoot, runtime: runtime, repo: repo,
	}
}

// wantSpawnArgv is the exact argv the fake runner should record for a run: the
// provider's flags (built with an empty prompt), then the injected per-run
// dialog-capture --settings flag (ADR-0020), then the seed prompt as the
// trailing positional when non-empty. Flags precede the positional so the
// parser never swallows --settings as prompt text.
func (f *fixture) wantSpawnArgv(name, model, effort, seed, runID string) []string {
	argv := f.prov.SpawnArgv(provider.SpawnSpec{SessionName: name, Model: model, Effort: effort})
	argv = append(argv, "--settings", filepath.Join(f.runtime, "settings."+runID+".json"))
	if seed != "" {
		argv = append(argv, seed)
	}
	return argv
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

// LiveInstanceCount excludes every provider login session — two providers'
// login sessions coexisting, plus a leftover legacy bare "lab-login", must
// never inflate the cap count (issue #77).
func TestLiveInstanceCount_excludesAllLoginSessions(t *testing.T) {
	live := []string{
		"proj~afk-7", // one real instance session
		tmuxx.LoginSessionName("claude-code"),
		tmuxx.LoginSessionName("codex"),
		"lab-login", // legacy, pre-per-provider
	}
	if got := LiveInstanceCount(live); got != 1 {
		t.Errorf("LiveInstanceCount(%v) = %d, want 1", live, got)
	}
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
	// Spawn argv is the provider's (recorded verbatim by the fake runner). A
	// manual run carries NO seed prompt, so SpawnArgv is called with "" and the
	// argv has no trailing prompt positional — just the injected per-run
	// dialog-capture --settings flag (ADR-0020).
	wantArgv := f.wantSpawnArgv(name, "opus[1m]", "max", "", run.ID)
	if strings.Join(sess.Argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("spawn argv = %v, want %v", sess.Argv, wantArgv)
	}
	// Regression (finding: seed-via-argv): the last argv element is the injected
	// settings path, not a stray/empty trailing prompt — a manual spawn never
	// appends one.
	if last := sess.Argv[len(sess.Argv)-1]; last != filepath.Join(f.runtime, "settings."+run.ID+".json") {
		t.Errorf("manual spawn last argv = %q, want the settings path (no trailing seed prompt)", last)
	}
	// And no seed was injected post-spawn via keystrokes (the mechanism moved
	// into the spawn argv; SendKeys is the login-code path only).
	if sent := f.runner.Sent(name); len(sent) != 0 {
		t.Errorf("manual spawn sent %d keystroke batches, want 0 (no post-spawn seeding)", len(sent))
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

	// Lab-side seeding ran (D13, every spawn): the skills bundle landed under
	// .claude/skills/ with the embedded content, CLAUDE.local.md carries the
	// binding line, and NONE of it shows up in the run's own git status.
	gotSkill, err := os.ReadFile(filepath.Join(wt, ".claude", "skills", "tdd", "SKILL.md"))
	if err != nil {
		t.Fatalf("seeded skill missing after Launch: %v", err)
	}
	wantSkill, err := assets.Skills.ReadFile("skills/tdd/SKILL.md")
	if err != nil {
		t.Fatalf("embedded bundle: %v", err)
	}
	if !bytes.Equal(gotSkill, wantSkill) {
		t.Error("seeded tdd/SKILL.md differs from the embedded bundle")
	}
	local, err := os.ReadFile(filepath.Join(wt, "CLAUDE.local.md"))
	if err != nil {
		t.Fatalf("CLAUDE.local.md missing after Launch: %v", err)
	}
	for _, want := range []string{"tracker binding is **builtin**", "labctl issue view [n]", "`ready-for-agent`"} {
		if !strings.Contains(string(local), want) {
			t.Errorf("CLAUDE.local.md missing %q", want)
		}
	}
	if out := gitCmd(t, f.home, wt, "status", "--porcelain"); out != "" {
		t.Errorf("git status after a seeded Launch = %q; want empty (seeded files excluded)", out)
	}
}

// Issue #96: a manual Start's StartParams.FirstMessage — the operator's first
// chat message — rides the same SeedPrompt → spawn-argv mechanism as the AFK
// seed prompt, present before the process starts as claude's trailing
// positional. The injected per-run --settings flag (ADR-0020) still lands
// among the flags, BEFORE that trailing prompt (mirrors
// TestLaunch_seedPromptIsTrailingSpawnPositional / TestLaunch_
// threadsOptionsAndKeepsSettingsBeforePrompt for the AFK path).
func TestStart_firstMessageIsTrailingSpawnPositional(t *testing.T) {
	f := newFixture(t)
	const msg = "Please add a health-check endpoint."

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, FirstMessage: msg})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	name := "proj~20260608-1530"
	sess, live := f.runner.Session(name)
	if !live {
		t.Fatal("session not live after Start")
	}
	if last := sess.Argv[len(sess.Argv)-1]; last != msg {
		t.Errorf("last spawn argv = %q, want the first message %q as one trailing positional", last, msg)
	}
	wantArgv := f.wantSpawnArgv(name, run.Model, run.Effort, msg, run.ID)
	if strings.Join(sess.Argv, " ") != strings.Join(wantArgv, " ") {
		t.Errorf("spawn argv = %v, want %v", sess.Argv, wantArgv)
	}
	// No post-spawn keystroke injection — same argv-only mechanism as AFK.
	if sent := f.runner.Sent(name); len(sent) != 0 {
		t.Errorf("first message sent via %d keystroke batches, want 0 (argv-only)", len(sent))
	}
}

// The repo's incogni flag flows into the provider's SeedOpts (D15 §9
// measure 1): an incogni repo's launch asks the provider to seed
// attribution-off settings; a plain repo's launch does not.
func TestStart_incogniFlagReachesProviderSeedOpts(t *testing.T) {
	f := newFixture(t)

	if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID}); err != nil {
		t.Fatalf("Start (plain): %v", err)
	}
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(true),
	}); err != nil {
		t.Fatalf("UpdateRepoSettings: %v", err)
	}
	if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "wip"}); err != nil {
		t.Fatalf("Start (incogni): %v", err)
	}

	opts := f.prov.SeededOpts()
	if len(opts) != 2 {
		t.Fatalf("SeedWorkspace called %d times, want 2", len(opts))
	}
	if opts[0].Incogni {
		t.Error("plain repo's launch passed Incogni=true to the provider")
	}
	if !opts[1].Incogni {
		t.Error("incogni repo's launch passed Incogni=false to the provider")
	}
}

// The repo's configured REAL git identity reaches the spawned session's env
// (D15 §9 measure 5): GIT_AUTHOR_* and GIT_COMMITTER_* all four, so the
// agent's commits are authored as the operator, never a bot. The CR-merge
// half of the measure is pinned in gitx/crmerge_test.go.
func TestStart_repoGitIdentityReachesSpawnEnv(t *testing.T) {
	f := newFixture(t)
	name, email := "Dominik", "dominik@example.invalid"
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		GitAuthorName:  store.Set(&name),
		GitAuthorEmail: store.Set(&email),
	}); err != nil {
		t.Fatalf("UpdateRepoSettings: %v", err)
	}

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatal("session not live")
	}
	for key, want := range map[string]string{
		"GIT_AUTHOR_NAME": name, "GIT_AUTHOR_EMAIL": email,
		"GIT_COMMITTER_NAME": name, "GIT_COMMITTER_EMAIL": email,
	} {
		if got := envValue(sess.ExtraEnv, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// failingSeeder drives the lab-side-seeding rollback path.
type failingSeeder struct{}

func (failingSeeder) SeedWorkspace(string, store.Repo, provider.SeedMeta, seeder.Opts) error {
	return errors.New("skills bundle write failed")
}

// A lab-side seeding failure aborts the Start through the same full rollback
// as a provider seed failure — worktree, branch, and guard all restored.
func TestStart_rollbackOnLabSeedFailure(t *testing.T) {
	f := newFixture(t)
	f.svc.seeder = failingSeeder{}

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var startFailed *StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("Start err = %v, want StartFailedError", err)
	}
	if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
		t.Error("worktree survived lab-seed-failure rollback")
	}
	if f.branchExists("lab/20260608-1530") {
		t.Error("branch survived lab-seed-failure rollback")
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Error("run row created despite the lab seed failure")
	}
	if snap := f.guard.Snapshot(); len(snap) != 0 {
		t.Errorf("startguard not cleared after rollback: %v", snap)
	}
}

// Regression (finding: AFK seed prompt raced the cold-start TUI when delivered
// via post-spawn SendKeys). Launch must carry the seed prompt into the spawn
// argv as claude's trailing positional — one element even when multi-line —
// present BEFORE the process, with no post-spawn keystroke injection.
func TestLaunch_seedPromptIsTrailingSpawnPositional(t *testing.T) {
	f := newFixture(t)
	const seed = "You are an autonomous AFK run.\nResolve issue #42 and open a PR."
	n := 42
	name := "proj~afk-42"
	run, err := f.svc.Launch(t.Context(), LaunchSpec{
		Repo:         f.repo,
		Provider:     f.prov,
		Kind:         store.RunKindAFKManual,
		IssueNumber:  &n,
		SessionName:  name,
		Branch:       "afk/42",
		WorktreePath: filepath.Join(f.worktreeRoot, "proj-42"),
		Model:        "opus[1m]",
		Effort:       "max",
		SeedPrompt:   seed,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if run.Kind != store.RunKindAFKManual {
		t.Fatalf("kind = %q, want afk_manual", run.Kind)
	}
	sess, live := f.runner.Session(name)
	if !live {
		t.Fatal("session not live after Launch")
	}
	// The seed prompt is the single trailing argv positional (never split on
	// its newline, never fragmented across pty reads) and follows the flags.
	if last := sess.Argv[len(sess.Argv)-1]; last != seed {
		t.Errorf("last spawn argv = %q, want the seed prompt %q as one trailing positional", last, seed)
	}
	if strings.Join(sess.Argv, " ") != strings.Join(f.wantSpawnArgv(name, "opus[1m]", "max", seed, run.ID), " ") {
		t.Errorf("spawn argv = %v", sess.Argv)
	}
	// The prompt existed before the process — nothing was typed in afterward.
	if sent := f.runner.Sent(name); len(sent) != 0 {
		t.Errorf("seeded via SendKeys (%d batches), want the argv-only mechanism", len(sent))
	}
}

// The per-run dialog-capture settings file (ADR-0020) is written under runtime/
// before the spawn and survives a successful Start; a spawn-failure rollback
// unlinks it (no orphan in the runtime dir).
func TestStart_dialogSettingsFileLifecycle(t *testing.T) {
	f := newFixture(t)
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	settings := filepath.Join(f.runtime, "settings."+run.ID+".json")
	if _, err := os.Stat(settings); err != nil {
		t.Fatalf("settings file missing after Start: %v", err)
	}

	// A second Start whose spawn fails must roll back its own settings file.
	f2 := newFixture(t)
	name := "proj~20260608-1530"
	f2.runner.FailStart(name, errors.New("boom"))
	if _, err := f2.svc.Start(t.Context(), StartParams{RepoID: f2.repo.ID}); err == nil {
		t.Fatal("Start: want spawn failure")
	}
	entries, _ := os.ReadDir(f2.runtime)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "settings.") {
			t.Errorf("settings file %q survived spawn-failure rollback", e.Name())
		}
	}
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

// Two providers' login sessions coexist, plus the legacy bare name a
// pre-per-provider binary would have left behind — StopAll must skip every
// one of them (issue #77).
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
	loginSessionA := tmuxx.LoginSessionName("claude-code")
	loginSessionB := tmuxx.LoginSessionName("codex")
	legacyLoginSession := "lab-login"
	f.runner.AddLive(loginSessionA)      // must be excluded
	f.runner.AddLive(loginSessionB)      // a second provider's login — coexists, also excluded
	f.runner.AddLive(legacyLoginSession) // legacy bare name — still recognized, excluded

	n, err := f.svc.StopAll(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if n != 2 {
		t.Errorf("stopped = %d, want 2", n)
	}
	for _, login := range []string{loginSessionA, loginSessionB, legacyLoginSession} {
		if _, live := f.runner.Session(login); !live {
			t.Errorf("StopAll killed the login session %q", login)
		}
	}
	for _, run := range []store.Run{a, b} {
		if _, live := f.runner.Session(run.SessionName); live {
			t.Errorf("session %s still live after StopAll", run.SessionName)
		}
	}
}

// fakeAFKStopper stands in for the AFK engine's neutral Stop: it records the
// delegated session and kills it in the runner, as the real StopAFK does under
// the stop-vs-reap lock.
type fakeAFKStopper struct {
	runner  *tmuxx.Fake
	stopped []string
	err     error
}

func (s *fakeAFKStopper) StopAFK(_ context.Context, session string) error {
	if s.err != nil {
		return s.err
	}
	s.stopped = append(s.stopped, session)
	s.runner.Kill(session)
	return nil
}

// Regression (finding: StopAll skipped AFK runs, so a forced repo delete
// stranded a live AFK agent working against a deleted repo). StopAll now
// delegates AFK sessions to the engine's neutral Stop so no session outlives
// its repo.
func TestStopAll_stopsAFKViaDelegation(t *testing.T) {
	f := newFixture(t)
	stopper := &fakeAFKStopper{runner: f.runner}
	f.svc.SetAFKStopper(stopper)

	m, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	afkName := "proj~afk-9"
	f.runner.AddLive(afkName)
	n9 := 9
	if _, err := f.st.CreateRun(t.Context(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: store.RunKindAFKManual, Provider: "claude-code",
		IssueNumber: &n9, Branch: "afk/9", WorktreePath: "/wt/proj-9", SessionName: afkName,
		Model: "opus[1m]", Effort: "max", StartedAt: clockTime, Outcome: store.RunOutcomeActive,
	}); err != nil {
		t.Fatal(err)
	}

	stopped, err := f.svc.StopAll(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if stopped != 2 {
		t.Errorf("stopped = %d, want 2 (manual + AFK; no session left behind)", stopped)
	}
	if len(stopper.stopped) != 1 || stopper.stopped[0] != afkName {
		t.Errorf("AFK delegation = %v, want [%q]", stopper.stopped, afkName)
	}
	if _, live := f.runner.Session(afkName); live {
		t.Error("AFK session survived StopAll — a forced repo delete would strand it")
	}
	if _, live := f.runner.Session(m.SessionName); live {
		t.Error("manual session survived StopAll")
	}
}

// A best-effort AFK stop hiccup (missing stopper or delegation error) never
// aborts the rest of StopAll — the manual runs are still torn down.
func TestStopAll_afkStopHiccupDoesNotAbort(t *testing.T) {
	f := newFixture(t)
	f.svc.SetAFKStopper(&fakeAFKStopper{runner: f.runner, err: errors.New("engine busy")})

	m, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	afkName := "proj~afk-9"
	f.runner.AddLive(afkName)
	n9 := 9
	if _, err := f.st.CreateRun(t.Context(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: store.RunKindAFKAuto, Provider: "claude-code",
		IssueNumber: &n9, Branch: "afk/9", WorktreePath: "/wt/proj-9", SessionName: afkName,
		Model: "opus[1m]", Effort: "max", StartedAt: clockTime, Outcome: store.RunOutcomeActive,
	}); err != nil {
		t.Fatal(err)
	}

	stopped, err := f.svc.StopAll(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if stopped != 1 {
		t.Errorf("stopped = %d, want 1 (manual torn down; failed AFK delegation not counted)", stopped)
	}
	if _, live := f.runner.Session(m.SessionName); live {
		t.Error("manual session survived a StopAll where the AFK delegation errored")
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

// ADR-0017 / issue #6 AC1: a provider that does NOT implement DeepLinker runs a
// session end to end with no capture machinery armed and keeps deep_link_url
// NULL. The AFK path shares the same Launch core (s.ArmCapture(created)), so the
// manual Start here is representative of both.
func TestStart_linklessProvider_noCaptureNoDeepLink(t *testing.T) {
	codexModel, codexEffort := "gpt-5-codex", "medium"
	nolink := providertest.NewNoLink()
	f := newFixtureWith(t, fixtureOpts{
		prov: nolink, providerID: nolink.ID(),
		modelDef: &codexModel, effortDef: &codexEffort,
	})

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The session spawned and the run is active — the link-less provider runs
	// end to end.
	if _, live := f.runner.Session(run.SessionName); !live {
		t.Fatal("tmux session not live after Start on a link-less provider")
	}
	if len(nolink.Seeded()) != 1 {
		t.Errorf("SeedWorkspace calls = %d, want 1", len(nolink.Seeded()))
	}
	got, err := f.st.RunBySession(t.Context(), run.SessionName)
	if err != nil {
		t.Fatalf("RunBySession: %v", err)
	}
	if got.Outcome != store.RunOutcomeActive {
		t.Errorf("outcome = %q, want active", got.Outcome)
	}

	// No capture goroutine ever armed: deep_link_url stays NULL. Give any
	// wrongly-armed goroutine a beat to have written before asserting.
	time.Sleep(50 * time.Millisecond)
	got, err = f.st.RunBySession(t.Context(), run.SessionName)
	if err != nil {
		t.Fatalf("RunBySession: %v", err)
	}
	if got.DeepLinkURL != nil {
		t.Errorf("deep_link_url = %v, want NULL (no capture for a link-less provider)", *got.DeepLinkURL)
	}

	// Re-adoption re-arms nothing either: ArmCapture is a no-op for a run whose
	// provider has no DeepLinker capability.
	f.svc.ArmCapture(got)
	time.Sleep(50 * time.Millisecond)
	if again, _ := f.st.RunBySession(t.Context(), run.SessionName); again.DeepLinkURL != nil {
		t.Errorf("ArmCapture wrote a deep link for a link-less provider: %v", *again.DeepLinkURL)
	}

	// The row surfaces no connecting pulse (the link-less provider is not a
	// ConnectingReporter) — the SPA falls through to the tmux-attach affordance.
	views, err := f.svc.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 || views[0].Connecting {
		t.Errorf("view = %+v, want a single non-connecting row", views)
	}
}
