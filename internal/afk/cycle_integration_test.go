package afk_test

// The M5 acceptance smoke, local form: the FULL AFK cycle over the real
// seams. REAL engine + REAL sqlite store + REAL git fixtures + REAL tmux on a
// private socket + the REAL tracker registry (credential decrypt → the real
// Forgejo REST client) against a stateful fake forge over httptest + the REAL
// agent API served over httptest + the REAL labctl binary driven by a fake
// claude shell script inside the tmux session (LAB_URL/LAB_TOKEN from the
// session env lab sets, labctl resolved via PATH).
//
// External test package on purpose: the M6 builtin variant
// (TestAFKCycleBuiltinIntegration) merges its change request through the REAL
// operator API (internal/httpapi), and httpapi imports afk — an in-package
// test file could never import it back.
//
// Registry seam note: the registry pins BaseURL to https://<host>/api/v1,
// which can never reach a plain-HTTP httptest server, so — like the M4
// forge-live smoke — the injected ForgejoFactory keeps the registry-resolved
// Token/Owner/Repo and swaps in the fake forge's URL. Everything else is the
// production wiring.
//
// Phases (sequential, shared world):
//  1. success: manual AFK start claims the lowest ready issue (the branch IS
//     the claim), fake claude reads the seed prompt, runs labctl issue
//     view/list, commits, pushes to the local origin, opens the PR through
//     labctl pr create (Closes #N injected server-side), the reaper
//     classifies success, guarded teardown removes the clean worktree and
//     keeps the unmerged branch, failures reset to 0, run tokens 401.
//  2. three strikes: a doomed run (fake claude sleeps; 1-minute repo budget
//     override + clock advance to the inclusive boundary) times out three
//     times → repo paused: the scheduler skips it and a manual start is
//     refused (ErrRepoPaused — the operator API's 409); reset re-arms and
//     the next ScheduleOnce launches an auto run.
//  3. neutral Stop mid-run through the instance.Stop delegation seam:
//     outcome stopped, counter untouched, worktree + branch survive and the
//     parked listing (reconcile.Parked) shows the branch; an over-budget
//     ReapOnce afterwards never reclassifies it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"path/filepath"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/agentapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/crmerge"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/reconcile"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/forgejo"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// clockTime/gitCmd/makeOrigin mirror the in-package helpers of engine_test.go
// (package afk) — this file lives in the external afk_test package (see the
// header note) and cannot see them.
var clockTime = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

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

func makeOrigin(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	gitCmd(t, home, "", "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	gitCmd(t, home, dir, "add", ".")
	gitCmd(t, home, dir, "commit", "-q", "-m", "c0")
	return dir
}

// --- fake forge (stateful Forgejo REST surface) ------------------------------

type cycleForgeIssue struct {
	Number int
	Title  string
	Body   string
	State  string
	Labels []string
}

type cycleForgePull struct {
	Number int
	Head   string
	Base   string
	Title  string
	Body   string
	State  string
	Merged bool
}

type cycleForgeRepo struct {
	issues     []cycleForgeIssue
	pulls      []cycleForgePull
	pullsLists int // GET /pulls page-1 requests — one per Tracker.Pulls()
}

// cycleForge is a minimal stateful Forgejo: exactly the endpoints the real
// forgejo REST client drives during the cycle, per repo, behind token auth.
type cycleForge struct {
	t     *testing.T
	token string

	mu    sync.Mutex
	repos map[string]*cycleForgeRepo // "owner/name"
}

func newCycleForge(t *testing.T, token string) *cycleForge {
	return &cycleForge{t: t, token: token, repos: map[string]*cycleForgeRepo{}}
}

func (f *cycleForge) addRepo(owner, name string, issues ...cycleForgeIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[owner+"/"+name] = &cycleForgeRepo{issues: issues}
}

func (f *cycleForge) pull(owner, name string, i int) (cycleForgePull, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.repos[owner+"/"+name]
	if st == nil || i >= len(st.pulls) {
		return cycleForgePull{}, len(st.pulls)
	}
	return st.pulls[i], len(st.pulls)
}

func (f *cycleForge) pullsListCount(owner, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repos[owner+"/"+name].pullsLists
}

func (f *cycleForge) issueJSON(is cycleForgeIssue) map[string]any {
	labels := make([]map[string]any, 0, len(is.Labels))
	for _, l := range is.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	return map[string]any{
		"number": is.Number, "title": is.Title, "body": is.Body,
		"state": is.State, "labels": labels, "comments": 0,
		"created_at": "2026-07-01T09:00:00Z", "updated_at": "2026-07-01T09:00:00Z",
	}
}

func (f *cycleForge) pullJSON(owner, name string, p cycleForgePull) map[string]any {
	return map[string]any{
		"number": p.Number, "state": p.State, "merged": p.Merged,
		"head":     map[string]any{"ref": p.Head},
		"html_url": "https://forge.test/" + owner + "/" + name + "/pulls/" + strconv.Itoa(p.Number),
	}
}

func (f *cycleForge) handler() http.Handler {
	mux := http.NewServeMux()

	state := func(w http.ResponseWriter, r *http.Request) *cycleForgeRepo {
		if got := r.Header.Get("Authorization"); got != "token "+f.token {
			f.t.Errorf("fake forge: Authorization = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return nil
		}
		st := f.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
		if st == nil {
			w.WriteHeader(http.StatusNotFound)
			return nil
		}
		return st
	}
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Errorf("fake forge: encode: %v", err)
		}
	}

	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st := state(w, r)
		if st == nil {
			return
		}
		q := r.URL.Query()
		out := []map[string]any{}
		if q.Get("page") == "1" {
			for _, is := range st.issues {
				if s := q.Get("state"); s != "" && s != tracker.StateAll && is.State != s {
					continue
				}
				if l := q.Get("labels"); l != "" && !cycleHasLabel(is, l) {
					continue
				}
				out = append(out, f.issueJSON(is))
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st := state(w, r)
		if st == nil {
			return
		}
		n, _ := strconv.Atoi(r.PathValue("n"))
		for _, is := range st.issues {
			if is.Number == n {
				writeJSON(w, http.StatusOK, f.issueJSON(is))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if state(w, r) == nil {
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{})
	})
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st := state(w, r)
		if st == nil {
			return
		}
		out := []map[string]any{}
		if r.URL.Query().Get("page") == "1" {
			st.pullsLists++
			for _, p := range st.pulls {
				out = append(out, f.pullJSON(r.PathValue("owner"), r.PathValue("repo"), p))
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st := state(w, r)
		if st == nil {
			return
		}
		var req struct {
			Head  string `json:"head"`
			Base  string `json:"base"`
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p := cycleForgePull{
			Number: len(st.pulls) + 1,
			Head:   req.Head, Base: req.Base, Title: req.Title, Body: req.Body,
			State: "open",
		}
		st.pulls = append(st.pulls, p)
		writeJSON(w, http.StatusCreated, f.pullJSON(r.PathValue("owner"), r.PathValue("repo"), p))
	})
	return mux
}

func cycleHasLabel(is cycleForgeIssue, name string) bool {
	for _, l := range is.Labels {
		if l == name {
			return true
		}
	}
	return false
}

// --- scripted provider --------------------------------------------------------

// cycleProvider is providertest.Fake with SpawnArgv swapped for the fake
// claude script of the session's repo (session names are <repo>~<label>).
type cycleProvider struct {
	*providertest.Fake
	mu      sync.Mutex
	scripts map[string]string // repo-name prefix "<repo>~" → script path
}

func (p *cycleProvider) setScript(repoName, script string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts[repoName+"~"] = script
}

func (p *cycleProvider) SpawnArgv(spec provider.SpawnSpec) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for prefix, script := range p.scripts {
		if strings.HasPrefix(spec.SessionName, prefix) {
			// Seed prompt carried as the trailing positional (pinned v0
			// mechanism) — the fake claude reads it from the last arg (launch
			// injects a --settings flag ahead of it), not stdin.
			if spec.InitialPrompt != "" {
				return []string{script, spec.InitialPrompt}
			}
			return []string{script}
		}
	}
	return []string{"sleep", "600"}
}

// --- helpers -------------------------------------------------------------------

var cycleSocketSeq int

// cycleTmux binds a real tmux to a private socket (design §11) and kills
// that private server on cleanup.
func cycleTmux(t *testing.T) *tmuxx.Tmux {
	t.Helper()
	cycleSocketSeq++
	socket := fmt.Sprintf("lab-cycle-%d-%d", os.Getpid(), cycleSocketSeq)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	return tmuxx.New("tmux", tmuxx.WithSocket(socket))
}

// buildLabctl compiles the REAL labctl binary into a temp dir the fake
// claude script prepends to PATH.
func buildLabctl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "labctl")
	cmd := exec.Command("go", "build", "-o", bin, "git.cloonar.com/Cloonar/coding-lab/cmd/labctl")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/labctl: %v\n%s", err, out)
	}
	return bin
}

func writeCycleScript(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// waitForCycleFile polls path until it has content, failing t after deadline
// with the fake claude log for diagnosis.
func waitForCycleFile(t *testing.T, path, logPath string, deadline time.Duration) string {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	log, _ := os.ReadFile(logPath)
	t.Fatalf("%s never got content; fake claude log:\n%s", path, log)
	return ""
}

func cycleBranchExists(env []string, bareDir, branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = bareDir
	cmd.Env = append(os.Environ(), env...)
	return cmd.Run() == nil
}

// --- the world -----------------------------------------------------------------

type cycleWorld struct {
	t     *testing.T
	ctx   context.Context
	svc   *afk.Service
	inst  *instance.Service
	recon *reconcile.Service
	st    *store.Store
	tmux  *tmuxx.Tmux
	forge *cycleForge
	prov  *cycleProvider
	clock *testutil.FakeClock
	agent *httptest.Server
	bus   *events.Bus

	home         string
	env          []string
	reposDir     string
	labctlDir    string
	forgeOwner   string
	forgeToken   string
	forgeCredID  string
	worktreeRoot string
}

func newCycleWorld(t *testing.T) *cycleWorld {
	t.Helper()
	testutil.RequireTool(t, "git")
	testutil.RequireTool(t, "tmux")
	testutil.RequireTool(t, "go")

	ctx := t.Context()
	home := t.TempDir()
	env := testutil.HermeticGitEnv(home)
	stateDir := t.TempDir()
	reposDir := filepath.Join(stateDir, "repos")
	worktreeRoot := filepath.Join(stateDir, "worktrees")

	labctlBin := buildLabctl(t)

	st := testutil.TempStore(t)
	if err := st.SeedDefaultSettings(ctx, 6); err != nil {
		t.Fatalf("SeedDefaultSettings: %v", err)
	}
	vlt, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	mat, err := vault.NewMaterializer(filepath.Join(stateDir, "runtime"))
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}

	const forgeToken = "cycle-forge-tok"
	forge := newCycleForge(t, forgeToken)
	forgeTS := httptest.NewServer(forge.handler())
	t.Cleanup(forgeTS.Close)

	blob, err := vlt.EncryptPayload(vault.ForgeTokenPayload{Host: "forge.test", Token: forgeToken})
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	credID := ids.NewID("cred")
	if _, err := st.CreateCredential(ctx, credID, "forge", store.CredentialKindForgeToken, blob, clockTime); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	// The REAL registry: credential decrypt + RepoPath resolution; only the
	// unreachable https BaseURL is swapped for the fake forge's URL.
	trackers := tracker.NewRegistry(st, vlt, nil, builtin.New, func(c tracker.ForgejoConfig) tracker.Tracker {
		return forgejo.New(c.HTTPClient, forgeTS.URL+"/api/v1", c.Token, c.Owner, c.Repo)
	}, func(tracker.GitHubConfig) tracker.Tracker {
		panic("github factory invoked in a forgejo integration test")
	})

	clock := testutil.NewFakeClock(clockTime)
	bus := events.NewBus()

	// The REAL agent API over httptest — LAB_URL for every spawned session.
	// It carries the real bus (production wiring): a builtin PR create
	// publishes cr.changed.
	agent := httptest.NewServer(agentapi.New(st, trackers, bus, nil, clock.Now).Handler())
	t.Cleanup(agent.Close)

	prov := &cycleProvider{Fake: providertest.New(), scripts: map[string]string{}}
	reg, err := provider.NewRegistry(prov)
	if err != nil {
		t.Fatalf("provider.NewRegistry: %v", err)
	}

	git := gitx.New("git")
	runner := cycleTmux(t)
	guard := startguard.New()

	inst, err := instance.New(instance.Options{
		Store: st, Git: git, Runner: runner, Providers: reg, Vault: vlt, Materializer: mat,
		Guard: guard, Bus: bus, ReposDir: reposDir, WorktreeRoot: worktreeRoot,
		LabURL: agent.URL, GitEnv: env, CaptureCtx: context.Background(), Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("instance.New: %v", err)
	}
	svc, err := afk.New(afk.Options{
		Store: st, Git: git, Runner: runner, Providers: reg, Trackers: trackers,
		Instances: inst, Materializer: mat, Bus: bus, Guard: guard,
		ReposDir: reposDir, WorktreeRoot: worktreeRoot, GitEnv: env, Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("afk.New: %v", err)
	}
	inst.SetAFKStopper(svc)

	recon, err := reconcile.New(reconcile.Options{
		Store: st, Git: git, Runner: runner, Guard: guard, Materializer: mat, Bus: bus,
		ReposDir: reposDir, GitEnv: env, Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}

	return &cycleWorld{
		t: t, ctx: ctx, svc: svc, inst: inst, recon: recon, st: st, tmux: runner,
		forge: forge, prov: prov, clock: clock, agent: agent, bus: bus,
		home: home, env: env, reposDir: reposDir, labctlDir: filepath.Dir(labctlBin),
		forgeOwner: "it", forgeToken: forgeToken, forgeCredID: credID,
		worktreeRoot: worktreeRoot,
	}
}

// addRepo builds one forge-bound repo: a real local origin, its bare
// reference clone, the repos row (remote_url is the FORGE shape the registry
// parses owner/repo from; the bare clone's origin remote — where worktree
// fetches and the fake claude's push go — was pinned to the local origin at
// clone time), and its fake-forge state.
func (w *cycleWorld) addRepo(name, script string, mut func(*store.Repo), issues ...cycleForgeIssue) (store.Repo, string) {
	w.t.Helper()
	origin := makeOrigin(w.t, w.home)
	repoID := ids.NewID("repo")
	bare := filepath.Join(w.reposDir, repoID+".git")
	if err := os.MkdirAll(w.reposDir, 0o755); err != nil {
		w.t.Fatalf("mkdir repos: %v", err)
	}
	if err := gitx.New("git").CloneBare(w.ctx, "file://"+origin, bare, w.env, nil); err != nil {
		w.t.Fatalf("CloneBare: %v", err)
	}
	author, email := "Cycle Bot", "cycle@lab.test"
	r := store.Repo{
		ID: repoID, Name: name,
		RemoteURL:      "https://forge.test/" + w.forgeOwner + "/" + name + ".git",
		TrackerBinding: store.TrackerBindingForge, ForgeKind: "forgejo",
		ForgeCredentialID: &w.forgeCredID,
		DefaultBranch:     "main", Provider: "claude-code",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		GitAuthorName: &author, GitAuthorEmail: &email,
		CloneStatus: store.CloneStatusReady, CreatedAt: clockTime,
	}
	if mut != nil {
		mut(&r)
	}
	repo, err := w.st.CreateRepo(w.ctx, r)
	if err != nil {
		w.t.Fatalf("CreateRepo: %v", err)
	}
	w.forge.addRepo(w.forgeOwner, name, issues...)
	w.prov.setScript(name, script)
	return repo, origin
}

func (w *cycleWorld) bare(repo store.Repo) string {
	return filepath.Join(w.reposDir, repo.ID+".git")
}

func (w *cycleWorld) alive(session string) bool {
	w.t.Helper()
	ok, err := w.tmux.IsRunning(w.ctx, session)
	if err != nil {
		w.t.Fatalf("IsRunning(%s): %v", session, err)
	}
	return ok
}

func (w *cycleWorld) run(repo store.Repo, id string) store.Run {
	w.t.Helper()
	runs, err := w.st.RunsByRepo(w.ctx, repo.ID, 0)
	if err != nil {
		w.t.Fatalf("RunsByRepo: %v", err)
	}
	for _, r := range runs {
		if r.ID == id {
			return r
		}
	}
	w.t.Fatalf("run %s not found", id)
	return store.Run{}
}

func (w *cycleWorld) failures(repo store.Repo) int {
	w.t.Helper()
	got, err := w.st.RepoByID(w.ctx, repo.ID)
	if err != nil {
		w.t.Fatalf("RepoByID: %v", err)
	}
	return got.ConsecutiveFailures
}

// agentGet hits the real agent API with a Bearer run token and returns the
// status code.
func (w *cycleWorld) agentGet(token, path string) int {
	w.t.Helper()
	req, err := http.NewRequestWithContext(w.ctx, http.MethodGet, w.agent.URL+path, nil)
	if err != nil {
		w.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("agent GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// successScript is the fake claude of the happy path: capture the seed
// prompt's first line + LAB_TOKEN, then drive the REAL labctl (via PATH) and
// git exactly as the seed prompt instructs, and stay alive for the reaper.
// workFile is the file the "work" touches — two runs in the same repo must
// touch different files or the second commit would be empty.
func successScript(out, bin, workFile string) string {
	return `#!/bin/sh
OUT=` + shq(out) + `
BIN=` + shq(bin) + `
WORK=` + shq(workFile) + `
for seed in "$@"; do :; done  # the seed prompt is the trailing positional (after any --settings flag)
printf '%s\n' "$seed" | head -n 1 > "$OUT/seed.txt"
printf '%s\n' "$LAB_TOKEN" > "$OUT/token.txt"
PATH="$BIN:$PATH"; export PATH
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
(
  set -ex
  labctl issue view > "$OUT/issue.txt"
  labctl issue list > "$OUT/list.txt"
  printf 'done\n' > "$WORK"
  git add "$WORK"
  git commit -q -m 'feat: resolve the flux issue'
  git push -q origin HEAD
  labctl pr create --title 'resolve the flux issue' --body 'work is done' > "$OUT/pr.txt"
) >> "$OUT/log.txt" 2>&1
printf '%s\n' "$?" > "$OUT/status"
exec sleep 600
`
}

// shq single-quotes s for /bin/sh.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- the test ---------------------------------------------------------------

func TestAFKCycleIntegration(t *testing.T) {
	w := newCycleWorld(t)

	// Phase 1 world: repo "cyc", one ready issue, fake claude does the work.
	out := t.TempDir()
	script := writeCycleScript(t, "claude-success.sh", successScript(out, w.labctlDir, "work.txt"))
	cyc, cycOrigin := w.addRepo("cyc", script, nil,
		cycleForgeIssue{Number: 1, Title: "Wire the flux capacitor", Body: "make it hum", State: "open", Labels: []string{tracker.ReadyLabel}},
		cycleForgeIssue{Number: 2, Title: "later", Body: "", State: "open", Labels: nil},
	)

	ok := t.Run("success cycle", func(t *testing.T) {
		run, err := w.svc.StartManualAFK(w.ctx, cyc.ID)
		if err != nil {
			t.Fatalf("StartManualAFK: %v", err)
		}
		if run.Kind != store.RunKindAFKManual || run.Branch != "afk/1" || run.SessionName != "cyc~afk-1" {
			t.Fatalf("run identity = %s/%s/%s", run.Kind, run.Branch, run.SessionName)
		}
		if run.IssueNumber == nil || *run.IssueNumber != 1 {
			t.Fatalf("issue_number = %v, want 1", run.IssueNumber)
		}
		if run.BudgetDeadline == nil || !run.BudgetDeadline.Equal(clockTime.Add(120*time.Minute)) {
			t.Fatalf("budget_deadline = %v, want clock+120m persisted", run.BudgetDeadline)
		}
		// The claim is the branch (ADR-0013), and the session is really live.
		if !cycleBranchExists(w.env, w.bare(cyc), "afk/1") {
			t.Fatal("claim branch afk/1 missing from the bare clone")
		}
		if !w.alive("cyc~afk-1") {
			t.Fatal("tmux session cyc~afk-1 not live")
		}

		// Fake claude drives the real labctl against the real agent API.
		status := waitForCycleFile(t, filepath.Join(out, "status"), filepath.Join(out, "log.txt"), 90*time.Second)
		if strings.TrimSpace(status) != "0" {
			log, _ := os.ReadFile(filepath.Join(out, "log.txt"))
			t.Fatalf("fake claude exited %s; log:\n%s", strings.TrimSpace(status), log)
		}

		// Seed prompt arrived as the spawn argv's trailing positional ($1):
		// its first line matches the pinned SeedPrompt.
		seed, _ := os.ReadFile(filepath.Join(out, "seed.txt"))
		if got, want := strings.TrimSpace(string(seed)), strings.SplitN(afk.SeedPrompt(1, "afk/1", false, ""), "\n", 2)[0]; got != want {
			t.Errorf("seed first line = %q, want %q", got, want)
		}
		// labctl issue view answered the run's CLAIMED issue with the pinned
		// plain format; issue list shows the open queue.
		issueOut, _ := os.ReadFile(filepath.Join(out, "issue.txt"))
		if !strings.HasPrefix(string(issueOut), "#1 Wire the flux capacitor\n") || !strings.Contains(string(issueOut), "make it hum") {
			t.Errorf("labctl issue view output:\n%s", issueOut)
		}
		// List columns: number, state, created-at, labels, title (ADR-0014 —
		// the triage buckets come from this one call).
		listOut, _ := os.ReadFile(filepath.Join(out, "list.txt"))
		if !strings.Contains(string(listOut), "#1\topen\t") ||
			!strings.Contains(string(listOut), "\tready-for-agent\tWire the flux capacitor") {
			t.Errorf("labctl issue list output:\n%s", listOut)
		}
		// The push landed on the local origin.
		gitCmd(t, w.home, cycOrigin, "rev-parse", "--verify", "--quiet", "refs/heads/afk/1")

		// The PR the fake forge recorded: head = the run's branch, base =
		// the default branch, Closes #1 injected server-side.
		pr, n := w.forge.pull(w.forgeOwner, "cyc", 0)
		if n != 1 {
			t.Fatalf("forge recorded %d pulls, want 1", n)
		}
		if pr.Head != "afk/1" || pr.Base != "main" {
			t.Errorf("PR head/base = %s/%s, want afk/1/main", pr.Head, pr.Base)
		}
		if pr.Body != "work is done\n\nCloses #1" {
			t.Errorf("PR body = %q, want the injected Closes #1", pr.Body)
		}
		prOut, _ := os.ReadFile(filepath.Join(out, "pr.txt"))
		if got, want := string(prOut), "1\thttps://forge.test/it/cyc/pulls/1\n"; got != want {
			t.Errorf("labctl pr create output = %q, want %q", got, want)
		}

		// Reap: PR-with-head-branch is the done-signal → success; guarded
		// teardown removes the clean worktree, keeps the unmerged branch;
		// a success resets the failure counter; the run token dies.
		if _, err := w.st.IncrementRepoFailures(w.ctx, cyc.ID); err != nil {
			t.Fatalf("IncrementRepoFailures: %v", err)
		}
		token, _ := os.ReadFile(filepath.Join(out, "token.txt"))
		if code := w.agentGet(string(token), "/agent/v1/issue"); code != http.StatusOK {
			t.Fatalf("run token pre-reap = %d, want 200", code)
		}

		w.svc.ReapOnce(w.ctx, w.clock.Now())

		got := w.run(cyc, run.ID)
		if got.Outcome != store.RunOutcomeSuccess {
			t.Fatalf("outcome = %s, want success", got.Outcome)
		}
		if w.alive("cyc~afk-1") {
			t.Error("session still live after the success reap")
		}
		if _, err := os.Stat(run.WorktreePath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("clean worktree not removed: %v", err)
		}
		if !cycleBranchExists(w.env, w.bare(cyc), "afk/1") {
			t.Error("unmerged branch afk/1 was deleted by the success teardown")
		}
		if f := w.failures(cyc); f != 0 {
			t.Errorf("consecutive_failures = %d, want 0 after success", f)
		}
		if code := w.agentGet(string(token), "/agent/v1/issue"); code != http.StatusUnauthorized {
			t.Errorf("run token post-reap = %d, want 401", code)
		}
		// A second sweep is a total no-op: no active AFK runs → no Pulls.
		lists := w.forge.pullsListCount(w.forgeOwner, "cyc")
		w.svc.ReapOnce(w.ctx, w.clock.Now())
		if after := w.forge.pullsListCount(w.forgeOwner, "cyc"); after != lists {
			t.Errorf("second ReapOnce listed pulls (%d → %d)", lists, after)
		}
	})
	if !ok {
		t.Fatal("success cycle failed; skipping the dependent phases")
	}

	// Phase 2 world: repo "doom", auto-enabled, 1-minute budget override,
	// fake claude just sleeps.
	doomBudget := 1
	doomScript := writeCycleScript(t, "claude-doom.sh", "#!/bin/sh\nexec sleep 600\n")
	doom, _ := w.addRepo("doom", doomScript, func(r *store.Repo) {
		r.BudgetMinutes = &doomBudget
		r.AFKAutoEnabled = true
	},
		cycleForgeIssue{Number: 1, Title: "impossible", Body: "never done", State: "open", Labels: []string{tracker.ReadyLabel}},
	)

	ok = t.Run("three strikes pause and reset", func(t *testing.T) {
		for strike := 1; strike <= afk.PauseThreshold; strike++ {
			run, err := w.svc.StartManualAFK(w.ctx, doom.ID)
			if err != nil {
				t.Fatalf("strike %d start: %v", strike, err)
			}
			if !w.alive("doom~afk-1") {
				t.Fatalf("strike %d: session not live", strike)
			}
			// Advance exactly to the persisted deadline: the boundary is
			// inclusive (now >= deadline → timeout).
			w.clock.Advance(time.Duration(doomBudget) * time.Minute)
			w.svc.ReapOnce(w.ctx, w.clock.Now())

			got := w.run(doom, run.ID)
			if got.Outcome != store.RunOutcomeTimeout {
				t.Fatalf("strike %d outcome = %s, want timeout", strike, got.Outcome)
			}
			if got.FailureReason == nil || *got.FailureReason != "budget exhausted before its done-signal" {
				t.Errorf("strike %d failure_reason = %v", strike, got.FailureReason)
			}
			if f := w.failures(doom); f != strike {
				t.Fatalf("failures after strike %d = %d", strike, f)
			}
			if w.alive("doom~afk-1") {
				t.Fatalf("strike %d: session survived the timeout reap", strike)
			}
			// The fresh fork was clean AND merged → branch deleted, issue
			// claimable again for the next strike.
			if cycleBranchExists(w.env, w.bare(doom), "afk/1") {
				t.Fatalf("strike %d: trivially-merged claim branch survived", strike)
			}
		}

		// Paused: the scheduler skips the repo entirely...
		w.svc.ScheduleOnce(w.ctx)
		if w.alive("doom~afk-auto-1") {
			t.Fatal("scheduler launched on a paused repo")
		}
		// ...and a manual start is refused (the operator API's 409).
		if _, err := w.svc.StartManualAFK(w.ctx, doom.ID); !errors.Is(err, afk.ErrRepoPaused) {
			t.Fatalf("manual start on paused repo = %v, want ErrRepoPaused", err)
		}

		// POST reset re-arms: the next scheduler tick claims and launches an
		// AUTO run for the same lowest issue.
		if changed, err := w.st.ResetRepoFailures(w.ctx, doom.ID); err != nil || !changed {
			t.Fatalf("ResetRepoFailures: changed=%v err=%v", changed, err)
		}
		w.svc.ScheduleOnce(w.ctx)
		run, err := w.st.RunBySession(w.ctx, "doom~afk-auto-1")
		if err != nil {
			t.Fatalf("no auto run after reset: %v", err)
		}
		if run.Kind != store.RunKindAFKAuto || run.Branch != "afk/1" {
			t.Fatalf("auto run = %s/%s", run.Kind, run.Branch)
		}
		if !w.alive("doom~afk-auto-1") {
			t.Fatal("auto session not live")
		}
	})
	if !ok {
		t.Fatal("three-strikes phase failed; skipping the stop phase")
	}

	t.Run("neutral stop parks", func(t *testing.T) {
		run, err := w.st.RunBySession(w.ctx, "doom~afk-auto-1")
		if err != nil {
			t.Fatalf("RunBySession: %v", err)
		}

		// Stop through the instance service — the M5 delegation seam.
		outcome, err := w.inst.Stop(w.ctx, "doom~afk-auto-1")
		if err != nil {
			t.Fatalf("instance.Stop: %v", err)
		}
		if outcome != instance.OutcomeParked {
			t.Errorf("stop outcome = %q, want parked", outcome)
		}
		got := w.run(doom, run.ID)
		if got.Outcome != store.RunOutcomeStopped {
			t.Fatalf("run outcome = %s, want stopped", got.Outcome)
		}
		if w.alive("doom~afk-auto-1") {
			t.Error("session still live after Stop")
		}
		// Neutral: counter untouched, worktree + branch kept.
		if f := w.failures(doom); f != 0 {
			t.Errorf("consecutive_failures = %d after neutral Stop, want 0", f)
		}
		if _, err := os.Stat(run.WorktreePath); err != nil {
			t.Errorf("worktree gone after neutral Stop: %v", err)
		}
		if !cycleBranchExists(w.env, w.bare(doom), "afk/1") {
			t.Error("claim branch gone after neutral Stop")
		}
		// The parked listing shows the branch with its worktree.
		parked, err := w.recon.Parked(w.ctx, doom.ID)
		if err != nil {
			t.Fatalf("Parked: %v", err)
		}
		found := false
		for _, p := range parked {
			if p.Branch == "afk/1" && p.WorktreePath == run.WorktreePath {
				found = true
			}
		}
		if !found {
			t.Errorf("parked listing = %+v, want branch afk/1 with the run's worktree", parked)
		}

		// An over-budget sweep NEVER reclassifies a stopped run — it is not
		// even a candidate (no pull listing happens).
		lists := w.forge.pullsListCount(w.forgeOwner, "doom")
		w.clock.Advance(2 * time.Duration(doomBudget) * time.Minute)
		w.svc.ReapOnce(w.ctx, w.clock.Now())
		if got := w.run(doom, run.ID); got.Outcome != store.RunOutcomeStopped {
			t.Errorf("stopped run reclassified to %s", got.Outcome)
		}
		if f := w.failures(doom); f != 0 {
			t.Errorf("counter moved to %d on a stopped run", f)
		}
		if after := w.forge.pullsListCount(w.forgeOwner, "doom"); after != lists {
			t.Errorf("sweep listed pulls for a repo with only a stopped run (%d → %d)", lists, after)
		}
	})
}

// --- the M6 builtin variant ---------------------------------------------------

// addBuiltinRepo builds one BUILTIN-bound repo in the production git topology
// of the M6 merge path: a BARE origin (the repo's REAL remote — CRMerge pushes
// refs/heads/main straight to it), a work clone that can advance origin's main
// (the "someone else pushed" actor of the merge-commit variant), and the lab
// bare reference clone. The repo row carries the git author identity the merge
// commit must be authored with (D15 measure 5); CreateRepo seeds the triage
// labels, among them tracker.ReadyLabel. mut (optional) adjusts the row before
// CreateRepo — the M7 incogni variant flips the flag and the branch patterns.
func (w *cycleWorld) addBuiltinRepo(name, script string, mut func(*store.Repo)) (repo store.Repo, origin, work string) {
	w.t.Helper()
	work = makeOrigin(w.t, w.home) // c0 on main
	origin = filepath.Join(w.t.TempDir(), name+"-origin.git")
	gitCmd(w.t, w.home, "", "init", "-q", "--bare", "-b", "main", origin)
	gitCmd(w.t, w.home, work, "remote", "add", "origin", origin)
	gitCmd(w.t, w.home, work, "push", "-q", "origin", "main")

	repoID := ids.NewID("repo")
	if err := os.MkdirAll(w.reposDir, 0o755); err != nil {
		w.t.Fatalf("mkdir repos: %v", err)
	}
	if err := gitx.New("git").CloneBare(w.ctx, "file://"+origin, filepath.Join(w.reposDir, repoID+".git"), w.env, nil); err != nil {
		w.t.Fatalf("CloneBare: %v", err)
	}
	author, email := "Cycle Human", "human@lab.test"
	r := store.Repo{
		ID: repoID, Name: name,
		RemoteURL:      "file://" + origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none",
		DefaultBranch: "main", Provider: "claude-code",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		GitAuthorName: &author, GitAuthorEmail: &email,
		CloneStatus: store.CloneStatusReady, CreatedAt: clockTime,
	}
	if mut != nil {
		mut(&r)
	}
	repo, err := w.st.CreateRepo(w.ctx, r)
	if err != nil {
		w.t.Fatalf("CreateRepo: %v", err)
	}
	w.prov.setScript(name, script)
	return repo, origin, work
}

// readyIssue files a built-in issue carrying tracker.ReadyLabel — the builtin
// ready queue is the store, not a forge.
func (w *cycleWorld) readyIssue(repo store.Repo, title, body string) store.Issue {
	w.t.Helper()
	labels, err := w.st.LabelsByRepo(w.ctx, repo.ID)
	if err != nil {
		w.t.Fatalf("LabelsByRepo: %v", err)
	}
	readyID := ""
	for _, l := range labels {
		if l.Name == tracker.ReadyLabel {
			readyID = l.ID
		}
	}
	if readyID == "" {
		w.t.Fatalf("repo %s has no %q label", repo.Name, tracker.ReadyLabel)
	}
	is, err := w.st.CreateIssueWithLabels(w.ctx, repo.ID, title, body, []string{readyID}, store.CommentAuthorOperator, nil, w.clock.Now())
	if err != nil {
		w.t.Fatalf("CreateIssueWithLabels: %v", err)
	}
	return is
}

// operatorAPI is the REAL httpapi server over httptest, authenticated with a
// PAT (explicit credential — bypasses CSRF, so the test needs no cookie jar).
// It shares the world's store, bus, git env and repos dir: exactly the
// production wiring of the CR routes.
type operatorAPI struct {
	t   *testing.T
	ctx context.Context
	url string
	pat string
}

func newOperatorAPI(w *cycleWorld) *operatorAPI {
	w.t.Helper()
	git := gitx.New("git")
	mergeSvc := crmerge.New(crmerge.Config{
		Store: w.st, Git: git, Bus: w.bus,
		ReposDir: w.reposDir, GitEnv: w.env, Now: w.clock.Now,
	})
	srv, err := httpapi.New(httpapi.Options{
		Store:  w.st,
		Bus:    w.bus,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Git:    git, ReposDir: w.reposDir, GitEnv: w.env,
		CRMerge: mergeSvc,
		Now:     w.clock.Now,
	})
	if err != nil {
		w.t.Fatalf("httpapi.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	w.t.Cleanup(ts.Close)

	user, err := w.st.CreateUser(w.ctx, "cycle-op", "unused-password-hash")
	if err != nil {
		w.t.Fatalf("CreateUser: %v", err)
	}
	token, hash := ids.NewToken("pat")
	if _, err := w.st.CreateAPIToken(w.ctx, user.ID, "cycle", hash); err != nil {
		w.t.Fatalf("CreateAPIToken: %v", err)
	}
	return &operatorAPI{t: w.t, ctx: w.ctx, url: ts.URL, pat: token}
}

// do sends one PAT-authenticated request and decodes the JSON body.
func (a *operatorAPI) do(method, path string) (int, map[string]any) {
	a.t.Helper()
	req, err := http.NewRequestWithContext(a.ctx, method, a.url+path, nil)
	if err != nil {
		a.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.pat)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		a.t.Fatalf("%s %s: decode body: %v", method, path, err)
	}
	return resp.StatusCode, body
}

// waitForBusEvents drains ch until every wanted event type was seen (other
// events in between are fine) or fails after a deadline.
func waitForBusEvents(t *testing.T, ch <-chan events.Event, want ...string) {
	t.Helper()
	missing := map[string]bool{}
	for _, name := range want {
		missing[name] = true
	}
	deadline := time.After(10 * time.Second)
	for len(missing) > 0 {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("bus subscription closed; still missing %v", missing)
			}
			delete(missing, e.Type)
		case <-deadline:
			t.Fatalf("bus events never arrived: %v", missing)
		}
	}
}

// crOf pulls the {cr} envelope out of a merge/close response.
func crOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	cr, ok := body["cr"].(map[string]any)
	if !ok {
		t.Fatalf("no cr envelope in %v", body)
	}
	return cr
}

// runBuiltinCycle drives one full agent run on a builtin repo through the real
// stack — manual start (the claim), fake claude working through labctl against
// the real agent API (the PR create lands as a change request via
// builtin.Tracker.CreatePull), then the reap — and asserts the M6 pinned
// signals: the CR row with its parsed closes, SUCCESS classified off builtin
// Pulls (the done-signal), and the guarded teardown keeping the unmerged head
// branch. Returns the run and the head branch's sha.
func runBuiltinCycle(t *testing.T, w *cycleWorld, repo store.Repo, out string, issueN, crN int) (store.Run, string) {
	t.Helper()
	branch := fmt.Sprintf("afk/%d", issueN)
	session := fmt.Sprintf("%s~afk-%d", repo.Name, issueN)

	run, err := w.svc.StartManualAFK(w.ctx, repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if run.Branch != branch || run.SessionName != session {
		t.Fatalf("run identity = %s/%s, want %s/%s", run.Branch, run.SessionName, branch, session)
	}
	if run.IssueNumber == nil || *run.IssueNumber != issueN {
		t.Fatalf("issue_number = %v, want %d", run.IssueNumber, issueN)
	}

	status := waitForCycleFile(t, filepath.Join(out, "status"), filepath.Join(out, "log.txt"), 90*time.Second)
	if strings.TrimSpace(status) != "0" {
		log, _ := os.ReadFile(filepath.Join(out, "log.txt"))
		t.Fatalf("fake claude exited %s; log:\n%s", strings.TrimSpace(status), log)
	}

	// labctl issue view answered the claimed issue from the STORE-backed
	// builtin tracker (no forge anywhere in this test).
	issueOut, _ := os.ReadFile(filepath.Join(out, "issue.txt"))
	if !strings.HasPrefix(string(issueOut), fmt.Sprintf("#%d ", issueN)) {
		t.Errorf("labctl issue view output:\n%s", issueOut)
	}

	// labctl pr create landed as a CHANGE REQUEST row: head = the run's
	// branch, base = the default branch, closes parsed from the server-side
	// injected `Closes #N`, and the lab-relative URL printed by labctl.
	cr, err := w.st.CRByRepoNumber(w.ctx, repo.ID, crN)
	if err != nil {
		t.Fatalf("CRByRepoNumber(%d): %v", crN, err)
	}
	if cr.State != store.CRStateOpen || cr.HeadBranch != branch || cr.BaseBranch != "main" {
		t.Fatalf("CR = %s %s→%s, want open %s→main", cr.State, cr.HeadBranch, cr.BaseBranch, branch)
	}
	if len(cr.Closes) != 1 || cr.Closes[0] != issueN {
		t.Fatalf("CR closes = %v, want [%d]", cr.Closes, issueN)
	}
	if cr.Body != fmt.Sprintf("work is done\n\nCloses #%d", issueN) {
		t.Errorf("CR body = %q, want the injected Closes #%d", cr.Body, issueN)
	}
	prOut, _ := os.ReadFile(filepath.Join(out, "pr.txt"))
	if got, want := string(prOut), fmt.Sprintf("%d\t/repos/%s/crs/%d\n", crN, repo.ID, crN); got != want {
		t.Errorf("labctl pr create output = %q, want %q", got, want)
	}

	// The reap: the OPEN CR from builtin Pulls IS the done-signal → success;
	// the guarded teardown removes the clean worktree, keeps the unmerged
	// head branch (the CR's merge source).
	w.svc.ReapOnce(w.ctx, w.clock.Now())
	got := w.run(repo, run.ID)
	if got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %s, want success off the builtin CR done-signal", got.Outcome)
	}
	if w.alive(session) {
		t.Errorf("session %s still live after the success reap", session)
	}
	if _, err := os.Stat(run.WorktreePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("clean worktree not removed: %v", err)
	}
	if !cycleBranchExists(w.env, w.bare(repo), branch) {
		t.Fatalf("unmerged head branch %s was deleted by the success teardown", branch)
	}
	return got, gitCmd(t, w.home, w.bare(repo), "rev-parse", "refs/heads/"+branch)
}

// TestAFKCycleBuiltinIntegration is the M6 acceptance, local form: the FULL
// circle on a repo with NO forge — claim → work → change request → operator
// merge → sweep. Same real seams as the forge cycle (engine, store, tmux,
// labctl-driven fake claude, agent API over httptest), plus the REAL operator
// API (httpapi over httptest, PAT-authenticated) for the merges:
//
//  1. builtin success + ff merge: a ready store issue is claimed, the fake
//     claude opens a CR through labctl (closes=[1] from the injected
//     directive), the reaper classifies success off builtin Pulls, the
//     teardown keeps the unmerged branch; POST /crs/1/merge fast-forwards
//     origin main to the head sha, auto-closes the built-in issue, publishes
//     cr.changed + issue.changed; the runtime sweep then GCs the now-merged
//     branch.
//  2. merge-commit path: a second cycle, but origin main advances between the
//     reap and the merge → POST /crs/2/merge builds a real merge commit
//     (parent1 = origin's advanced tip, parent2 = the head) authored with the
//     repo's configured REAL identity, and the sweep GCs that branch too.
func TestAFKCycleBuiltinIntegration(t *testing.T) {
	w := newCycleWorld(t)
	api := newOperatorAPI(w)

	out1 := t.TempDir()
	script1 := writeCycleScript(t, "claude-builtin-1.sh", successScript(out1, w.labctlDir, "work-1.txt"))
	repo, origin, work := w.addBuiltinRepo("bolt", script1, nil)
	w.readyIssue(repo, "Wire the flux capacitor", "make it hum")
	crBase := "/api/v1/repos/" + repo.ID + "/crs"

	ok := t.Run("builtin success cycle and ff merge", func(t *testing.T) {
		_, headSHA := runBuiltinCycle(t, w, repo, out1, 1, 1)

		// Merge through the REAL operator API: fast-forward path (origin main
		// never moved), watched on the world's bus.
		evts, cancel := w.bus.Subscribe(w.ctx)
		defer cancel()
		code, body := api.do(http.MethodPost, crBase+"/1/merge")
		if code != http.StatusOK {
			t.Fatalf("merge = %d (%v), want 200", code, body)
		}
		cr := crOf(t, body)
		if cr["state"] != "merged" || cr["merge_commit"] != headSHA {
			t.Fatalf("merged cr = state %v, merge_commit %v; want merged @ head %s (ff)", cr["state"], cr["merge_commit"], headSHA)
		}
		if got := gitCmd(t, w.home, origin, "rev-parse", "refs/heads/main"); got != headSHA {
			t.Errorf("origin main = %s, want the head sha %s (fast-forward)", got, headSHA)
		}
		// The CR's built-in issue auto-closed, and both events fired.
		is, err := w.st.IssueByRepoNumber(w.ctx, repo.ID, 1)
		if err != nil {
			t.Fatalf("IssueByRepoNumber: %v", err)
		}
		if is.State != store.IssueStateClosed {
			t.Errorf("issue #1 state = %s, want closed after the merge", is.State)
		}
		waitForBusEvents(t, evts, httpapi.EventCRChanged, "issue.changed")

		// The runtime sweep GCs the now-merged branch — the full circle.
		if !cycleBranchExists(w.env, w.bare(repo), "afk/1") {
			t.Fatal("merged branch afk/1 gone before the sweep ran")
		}
		w.recon.RuntimeSweep(w.ctx)
		if cycleBranchExists(w.env, w.bare(repo), "afk/1") {
			t.Error("runtime sweep kept the merged branch afk/1")
		}
	})
	if !ok {
		t.Fatal("builtin success cycle failed; skipping the merge-commit phase")
	}

	// Phase 2: same repo, fresh ready issue, and origin main ADVANCES between
	// the reap and the merge — the merge-commit path.
	out2 := t.TempDir()
	script2 := writeCycleScript(t, "claude-builtin-2.sh", successScript(out2, w.labctlDir, "work-2.txt"))
	w.prov.setScript(repo.Name, script2)
	w.readyIssue(repo, "Polish the flux capacitor", "make it shine")

	t.Run("merge-commit path after base advance", func(t *testing.T) {
		_, headSHA := runBuiltinCycle(t, w, repo, out2, 2, 2)

		// Someone else advances origin main (a non-conflicting file).
		gitCmd(t, w.home, work, "fetch", "-q", "origin")
		gitCmd(t, w.home, work, "reset", "-q", "--hard", "origin/main")
		if err := os.WriteFile(filepath.Join(work, "base.txt"), []byte("moved\n"), 0o644); err != nil {
			t.Fatalf("write base.txt: %v", err)
		}
		gitCmd(t, w.home, work, "add", "base.txt")
		gitCmd(t, w.home, work, "commit", "-q", "-m", "base advances")
		gitCmd(t, w.home, work, "push", "-q", "origin", "main")
		baseSHA := gitCmd(t, w.home, origin, "rev-parse", "refs/heads/main")

		evts, cancel := w.bus.Subscribe(w.ctx)
		defer cancel()
		code, body := api.do(http.MethodPost, crBase+"/2/merge")
		if code != http.StatusOK {
			t.Fatalf("merge = %d (%v), want 200", code, body)
		}
		cr := crOf(t, body)
		mergeSHA, _ := cr["merge_commit"].(string)
		if cr["state"] != "merged" || mergeSHA == "" || mergeSHA == headSHA {
			t.Fatalf("merged cr = state %v, merge_commit %v; want merged @ a fresh merge commit", cr["state"], cr["merge_commit"])
		}
		// Origin main is the merge commit: parent1 = the advanced base,
		// parent2 = the CR head, authored with the repo's REAL identity.
		if got := gitCmd(t, w.home, origin, "rev-parse", "refs/heads/main"); got != mergeSHA {
			t.Errorf("origin main = %s, want the merge commit %s", got, mergeSHA)
		}
		if p1 := gitCmd(t, w.home, origin, "rev-parse", "main^1"); p1 != baseSHA {
			t.Errorf("merge parent1 = %s, want the advanced base %s", p1, baseSHA)
		}
		if p2 := gitCmd(t, w.home, origin, "rev-parse", "main^2"); p2 != headSHA {
			t.Errorf("merge parent2 = %s, want the CR head %s", p2, headSHA)
		}
		if id := gitCmd(t, w.home, origin, "log", "-1", "--format=%an <%ae>", "main"); id != "Cycle Human <human@lab.test>" {
			t.Errorf("merge commit author = %q, want the repo's configured identity", id)
		}
		is, err := w.st.IssueByRepoNumber(w.ctx, repo.ID, 2)
		if err != nil {
			t.Fatalf("IssueByRepoNumber: %v", err)
		}
		if is.State != store.IssueStateClosed {
			t.Errorf("issue #2 state = %s, want closed after the merge", is.State)
		}
		waitForBusEvents(t, evts, httpapi.EventCRChanged, "issue.changed")

		// And the sweep completes the circle again.
		w.recon.RuntimeSweep(w.ctx)
		if cycleBranchExists(w.env, w.bare(repo), "afk/2") {
			t.Error("runtime sweep kept the merged branch afk/2")
		}
	})
}

// --- the M7 incogni variant ----------------------------------------------------

// incogniSuccessScript is the fake claude of the incogni happy path: capture
// the FULL seed prompt and the worktree's git status at spawn and again after
// the clean commit (both must be empty — every lab-seeded file is excluded,
// D15 §9 measure 6), commit with a NEUTRAL message, push through the bare
// clone's pre-push guard, then open the PR with a POISONED body — the
// attribution stripping (measure 3) is the agent API's job, not the script's.
func incogniSuccessScript(out, bin string) string {
	return `#!/bin/sh
OUT=` + shq(out) + `
BIN=` + shq(bin) + `
for seed in "$@"; do :; done  # the seed prompt is the trailing positional (after any --settings flag)
printf '%s' "$seed" > "$OUT/seed.txt"
git status --porcelain > "$OUT/status-spawn.txt"
PATH="$BIN:$PATH"; export PATH
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
(
  set -ex
  labctl issue view > "$OUT/issue.txt"
  printf 'done\n' > work.txt
  git add work.txt
  git commit -q -m 'feat: neutral work'
  git status --porcelain > "$OUT/status-prepush.txt"
  git push -q origin HEAD
  labctl pr create --title 'neutral work' --body 'work is done

🤖 Generated with [Claude Code](https://claude.com/claude-code)

Co-Authored-By: Claude Opus <noreply@anthropic.com>' > "$OUT/pr.txt"
) >> "$OUT/log.txt" 2>&1
printf '%s\n' "$?" > "$OUT/status"
exec sleep 600
`
}

// incogniPoisonedScript is the fake claude of the guard variant: it commits a
// Co-Authored-By: Claude trailer and tries to push — the pre-push hook in the
// bare reference repo (measure 7) must reject it. The push result and output
// are captured for the test; no PR ever exists, so the run times out.
func incogniPoisonedScript(out string) string {
	return `#!/bin/sh
OUT=` + shq(out) + `
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
(
  set -ex
  printf 'sneaky\n' > sneak.txt
  git add sneak.txt
  git commit -q -m 'feat: sneak work' -m 'Co-Authored-By: Claude Opus <noreply@anthropic.com>'
  git rev-parse HEAD > "$OUT/head.txt"
) >> "$OUT/log.txt" 2>&1
if git push origin HEAD > "$OUT/push-out.txt" 2>&1; then
  printf 'pushed\n' > "$OUT/push-result.txt"
else
  printf 'rejected\n' > "$OUT/push-result.txt"
fi
exec sleep 600
`
}

// TestAFKCycleIncogniBuiltinIntegration is the M7 acceptance, local form: the
// full AFK circle on an INCOGNI builtin repo, all seven D15 §9 measures in one
// flow over the same real seams as the other cycles (engine, store, tmux,
// labctl-driven fake claude, agent API over httptest, real git + the real
// pre-push guard in the bare reference clone):
//
//  1. clean cycle: neutral claim branch issue-1 (the repo's pattern — NEVER
//     afk/1), the incogni seed prompt sentence, a spawn-clean and pre-push-
//     clean worktree (seeded skills + CLAUDE.local.md all excluded), a clean
//     push THROUGH the installed guard, a poisoned PR body sanitized
//     server-side (the CR row carries zero markers), reap success — then
//     remote-side proof: neutral branch name, marker-free commit messages,
//     the repo's configured REAL author identity.
//  2. guard variant: a run that commits a Co-Authored-By: Claude trailer gets
//     its push REJECTED by the hook (the failure text names the offending
//     commit), nothing reaches origin, and the PR-less run reaps as timeout.
func TestAFKCycleIncogniBuiltinIntegration(t *testing.T) {
	w := newCycleWorld(t)

	out1 := t.TempDir()
	script1 := writeCycleScript(t, "claude-incogni-1.sh", incogniSuccessScript(out1, w.labctlDir))
	incBudget := 1
	neutralName, neutralEmail := "Neutral Human", "neutral@lab.test"
	repo, origin, _ := w.addBuiltinRepo("inc", script1, func(r *store.Repo) {
		r.Incogni = true
		// The incogni create-time defaults (D15 measure 4, reposvc.Add).
		r.AFKBranchPattern = "issue-<N>"
		r.ManualBranchPrefix = "wip/"
		r.GitAuthorName, r.GitAuthorEmail = &neutralName, &neutralEmail
		r.BudgetMinutes = &incBudget // phase 2's timeout is one clock tick away
	})
	// The fixture mirrors reposvc's clone-completion install for incogni
	// repos: the guard lives in the BARE reference clone, shared by every
	// worktree via the common git dir (D15 measure 7). Its scrub/seeded-path
	// patterns come from the repo's provider (issue #51 decision 8), exactly
	// as reposvc resolves them from repos.provider.
	incMeta := w.prov.SeedMeta()
	if err := seeder.InstallPrePushHook(w.bare(repo), incMeta.ScrubPatterns, incMeta.SeededPathPatterns); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	w.readyIssue(repo, "Wire the flux capacitor", "make it hum")

	ok := t.Run("clean cycle sanitizes and stays neutral", func(t *testing.T) {
		// The M7 gate's hook-file assertion: installed in the bare hooks dir
		// and executable — the clean push below then proves it non-obstructive.
		hookPath := seeder.PrePushHookPath(w.bare(repo))
		st, err := os.Stat(hookPath)
		if err != nil {
			t.Fatalf("pre-push hook missing from the bare hooks dir: %v", err)
		}
		if st.Mode()&0o111 == 0 {
			t.Fatalf("pre-push hook %s is not executable (mode %v)", hookPath, st.Mode())
		}
		if !seeder.PrePushHookInstalled(w.bare(repo)) {
			t.Fatal("bare clone does not carry lab's incogni guard marker")
		}

		run, err := w.svc.StartManualAFK(w.ctx, repo.ID)
		if err != nil {
			t.Fatalf("StartManualAFK: %v", err)
		}
		// The claim branch is the repo's PATTERN rendered — never a literal
		// afk/ prefix on an incogni repo (D15 measure 4).
		if run.Branch != "issue-1" || run.SessionName != "inc~afk-1" {
			t.Fatalf("run identity = %s/%s, want issue-1/inc~afk-1", run.Branch, run.SessionName)
		}
		if !cycleBranchExists(w.env, w.bare(repo), "issue-1") {
			t.Fatal("claim branch issue-1 missing from the bare clone")
		}
		if cycleBranchExists(w.env, w.bare(repo), "afk/1") {
			t.Fatal("literal afk/1 branch appeared on an incogni repo")
		}
		// The launch passed the incogni flag to the provider's seeding
		// (measure 1's wiring; the fake provider records SeedOpts).
		if opts := w.prov.SeededOpts(); len(opts) == 0 || !opts[len(opts)-1].Incogni {
			t.Errorf("provider SeedOpts = %+v, want Incogni=true on the launch", opts)
		}

		status := waitForCycleFile(t, filepath.Join(out1, "status"), filepath.Join(out1, "log.txt"), 90*time.Second)
		if strings.TrimSpace(status) != "0" {
			log, _ := os.ReadFile(filepath.Join(out1, "log.txt"))
			t.Fatalf("fake claude exited %s; log:\n%s", strings.TrimSpace(status), log)
		}

		// The FULL seed prompt is the incogni rendering (measure 2): branch
		// issue-1 and the no-attribution sentence on the commit step.
		seed, _ := os.ReadFile(filepath.Join(out1, "seed.txt"))
		if got, want := string(seed), afk.SeedPrompt(1, "issue-1", true, ""); got != want {
			t.Errorf("seed prompt = %q, want %q", got, want)
		}
		if !strings.Contains(string(seed), "No AI attribution, no Co-Authored-By, no generated-with footers anywhere.") {
			t.Error("seed prompt lacks the incogni sentence")
		}
		// The seeded worktree was clean at spawn AND pre-push: skills bundle,
		// CLAUDE.local.md and .claude/ are all excluded (measure 6).
		for _, f := range []string{"status-spawn.txt", "status-prepush.txt"} {
			if b, err := os.ReadFile(filepath.Join(out1, f)); err != nil || len(strings.TrimSpace(string(b))) != 0 {
				t.Errorf("worktree git status (%s) = %q, err %v; want empty", f, b, err)
			}
		}
		// The lab seeding itself landed (D13 — every spawn): the skills bundle
		// and the generated CLAUDE.local.md are in the (still live) worktree.
		if _, err := os.Stat(filepath.Join(run.WorktreePath, "CLAUDE.local.md")); err != nil {
			t.Errorf("CLAUDE.local.md not seeded: %v", err)
		}
		if _, err := os.Stat(filepath.Join(run.WorktreePath, ".claude", "skills", "tdd", "SKILL.md")); err != nil {
			t.Errorf("skills bundle not seeded: %v", err)
		}

		// The CR row: the poisoned body was sanitized SERVER-SIDE (measure 3)
		// — the injected Closes #1 survived, every marker line is gone.
		cr, err := w.st.CRByRepoNumber(w.ctx, repo.ID, 1)
		if err != nil {
			t.Fatalf("CRByRepoNumber(1): %v", err)
		}
		if cr.State != store.CRStateOpen || cr.HeadBranch != "issue-1" || cr.BaseBranch != "main" {
			t.Fatalf("CR = %s %s→%s, want open issue-1→main", cr.State, cr.HeadBranch, cr.BaseBranch)
		}
		if len(cr.Closes) != 1 || cr.Closes[0] != 1 {
			t.Fatalf("CR closes = %v, want [1]", cr.Closes)
		}
		if cr.Body != "work is done\n\nCloses #1" {
			t.Errorf("CR body = %q, want the sanitized body with the injected Closes #1", cr.Body)
		}
		for _, marker := range []string{"claude", "generated with", "co-authored-by"} {
			if strings.Contains(strings.ToLower(cr.Body), marker) {
				t.Errorf("CR body still carries %q: %q", marker, cr.Body)
			}
		}

		// Remote-side proof on the origin fixture: the branch name is neutral,
		// the pushed commit messages carry zero markers, and the author is the
		// repo's configured REAL identity (measure 5).
		if !cycleBranchExists(w.env, origin, "issue-1") {
			t.Fatal("origin lacks the pushed branch issue-1")
		}
		if cycleBranchExists(w.env, origin, "afk/1") {
			t.Fatal("origin grew a literal afk/1 branch")
		}
		msgs := gitCmd(t, w.home, origin, "log", "--format=%B", "main..issue-1")
		for _, marker := range []string{"claude", "generated with", "co-authored-by"} {
			if strings.Contains(strings.ToLower(msgs), marker) {
				t.Errorf("pushed commit messages carry %q:\n%s", marker, msgs)
			}
		}
		if id := gitCmd(t, w.home, origin, "log", "-1", "--format=%an <%ae>", "issue-1"); id != "Neutral Human <neutral@lab.test>" {
			t.Errorf("pushed commit author = %q, want the repo's configured identity", id)
		}

		// Reap: the open CR is the done-signal → success; teardown keeps the
		// unmerged claim branch.
		w.svc.ReapOnce(w.ctx, w.clock.Now())
		got := w.run(repo, run.ID)
		if got.Outcome != store.RunOutcomeSuccess {
			t.Fatalf("outcome = %s, want success", got.Outcome)
		}
		if w.alive("inc~afk-1") {
			t.Error("session still live after the success reap")
		}
		if !cycleBranchExists(w.env, w.bare(repo), "issue-1") {
			t.Error("unmerged claim branch issue-1 was deleted by the success teardown")
		}
	})
	if !ok {
		t.Fatal("clean incogni cycle failed; skipping the guard variant")
	}

	// Phase 2 world: a second ready issue (issue-1's surviving claim branch
	// keeps #1 out of selection) and a fake claude that commits attribution.
	out2 := t.TempDir()
	script2 := writeCycleScript(t, "claude-incogni-2.sh", incogniPoisonedScript(out2))
	w.prov.setScript(repo.Name, script2)
	w.readyIssue(repo, "Polish the flux capacitor", "make it shine")

	t.Run("pre-push guard rejects attribution", func(t *testing.T) {
		run, err := w.svc.StartManualAFK(w.ctx, repo.ID)
		if err != nil {
			t.Fatalf("StartManualAFK: %v", err)
		}
		if run.Branch != "issue-2" || run.SessionName != "inc~afk-2" {
			t.Fatalf("run identity = %s/%s, want issue-2/inc~afk-2", run.Branch, run.SessionName)
		}

		// The push must come back REJECTED by lab's guard, naming the commit.
		result := waitForCycleFile(t, filepath.Join(out2, "push-result.txt"), filepath.Join(out2, "log.txt"), 90*time.Second)
		if strings.TrimSpace(result) != "rejected" {
			t.Fatalf("push result = %q, want rejected", strings.TrimSpace(result))
		}
		pushOut, _ := os.ReadFile(filepath.Join(out2, "push-out.txt"))
		if !strings.Contains(string(pushOut), "lab incogni guard: commit") ||
			!strings.Contains(string(pushOut), "carries AI attribution") {
			t.Errorf("push failure text lacks the guard's message:\n%s", pushOut)
		}
		sha, _ := os.ReadFile(filepath.Join(out2, "head.txt"))
		if s := strings.TrimSpace(string(sha)); s == "" || !strings.Contains(string(pushOut), s) {
			t.Errorf("push failure text does not name the offending commit %q:\n%s", s, pushOut)
		}
		// Nothing reached the remote.
		if cycleBranchExists(w.env, origin, "issue-2") {
			t.Fatal("poisoned branch issue-2 reached origin despite the guard")
		}

		// No PR can exist → the run times out at its budget deadline and the
		// unpushed claim branch survives as the parked claim.
		w.clock.Advance(time.Duration(incBudget) * time.Minute)
		w.svc.ReapOnce(w.ctx, w.clock.Now())
		got := w.run(repo, run.ID)
		if got.Outcome != store.RunOutcomeTimeout {
			t.Fatalf("outcome = %s, want timeout (push rejected → no done-signal)", got.Outcome)
		}
		if f := w.failures(repo); f != 1 {
			t.Errorf("consecutive_failures = %d, want 1 after the timeout", f)
		}
		if !cycleBranchExists(w.env, w.bare(repo), "issue-2") {
			t.Error("unmerged claim branch issue-2 was deleted by the timeout teardown")
		}
	})
}
