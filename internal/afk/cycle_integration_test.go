package afk

// The M5 acceptance smoke, local form: the FULL AFK cycle over the real
// seams. REAL engine + REAL sqlite store + REAL git fixtures + REAL tmux on a
// private socket + the REAL tracker registry (credential decrypt → the real
// Forgejo REST client) against a stateful fake forge over httptest + the REAL
// agent API served over httptest + the REAL labctl binary driven by a fake
// claude shell script inside the tmux session (LAB_URL/LAB_TOKEN from the
// session env lab sets, labctl resolved via PATH).
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

	"git.cloonar.com/Cloonar/coding-lab/internal/agentapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/reconcile"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/forgejo"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

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

func (p *cycleProvider) SpawnArgv(session, _, _, initialPrompt string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for prefix, script := range p.scripts {
		if strings.HasPrefix(session, prefix) {
			// Seed prompt carried as the trailing positional (pinned v0
			// mechanism) — the fake claude reads it from $1, not stdin.
			if initialPrompt != "" {
				return []string{script, initialPrompt}
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
	svc   *Service
	inst  *instance.Service
	recon *reconcile.Service
	st    *store.Store
	tmux  *tmuxx.Tmux
	forge *cycleForge
	prov  *cycleProvider
	clock *testutil.FakeClock
	agent *httptest.Server

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
	})

	clock := testutil.NewFakeClock(clockTime)

	// The REAL agent API over httptest — LAB_URL for every spawned session.
	agent := httptest.NewServer(agentapi.New(st, trackers, nil, clock.Now).Handler())
	t.Cleanup(agent.Close)

	prov := &cycleProvider{Fake: providertest.New(), scripts: map[string]string{}}
	reg, err := provider.NewRegistry(prov)
	if err != nil {
		t.Fatalf("provider.NewRegistry: %v", err)
	}

	git := gitx.New("git")
	runner := cycleTmux(t)
	bus := events.NewBus()
	guard := startguard.New()

	inst, err := instance.New(instance.Options{
		Store: st, Git: git, Runner: runner, Providers: reg, Vault: vlt, Materializer: mat,
		Guard: guard, Bus: bus, ReposDir: reposDir, WorktreeRoot: worktreeRoot,
		LabURL: agent.URL, GitEnv: env, CaptureCtx: context.Background(), Now: clock.Now,
	})
	if err != nil {
		t.Fatalf("instance.New: %v", err)
	}
	svc, err := New(Options{
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
		forge: forge, prov: prov, clock: clock, agent: agent,
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
func successScript(out, bin string) string {
	return `#!/bin/sh
OUT=` + shq(out) + `
BIN=` + shq(bin) + `
printf '%s\n' "$1" | head -n 1 > "$OUT/seed.txt"
printf '%s\n' "$LAB_TOKEN" > "$OUT/token.txt"
PATH="$BIN:$PATH"; export PATH
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
(
  set -ex
  labctl issue view > "$OUT/issue.txt"
  labctl issue list > "$OUT/list.txt"
  printf 'done\n' > work.txt
  git add work.txt
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
	script := writeCycleScript(t, "claude-success.sh", successScript(out, w.labctlDir))
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
		if got, want := strings.TrimSpace(string(seed)), strings.SplitN(SeedPrompt(1, "afk/1", false), "\n", 2)[0]; got != want {
			t.Errorf("seed first line = %q, want %q", got, want)
		}
		// labctl issue view answered the run's CLAIMED issue with the pinned
		// plain format; issue list shows the open queue.
		issueOut, _ := os.ReadFile(filepath.Join(out, "issue.txt"))
		if !strings.HasPrefix(string(issueOut), "#1 Wire the flux capacitor\n") || !strings.Contains(string(issueOut), "make it hum") {
			t.Errorf("labctl issue view output:\n%s", issueOut)
		}
		listOut, _ := os.ReadFile(filepath.Join(out, "list.txt"))
		if !strings.Contains(string(listOut), "#1\topen\tWire the flux capacitor") {
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
		for strike := 1; strike <= PauseThreshold; strike++ {
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
		if _, err := w.svc.StartManualAFK(w.ctx, doom.ID); !errors.Is(err, ErrRepoPaused) {
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
