package httpapi

// M6 change-request suite: httptest against the real stack — store on a
// t.TempDir sqlite DB, real vault + materializer, real gitx engine on real
// git fixture repos (design §11 / D17). The fixture mirrors production
// topology exactly like gitx's own crmerge suite: a BARE origin (the repo's
// real remote), a work clone that advances origin's main (the "someone else
// pushed" actor), and the lab bare reference clone at <reposDir>/<id>.git.
// CR head branches are created the way runs create them (AddWorktree fork,
// commit, RemoveWorktree with the branch kept). The agent-API flow test
// mounts the real agentapi handler over the same store and registry.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/agentapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/crmerge"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

type crTestServer struct {
	*testServer
	home     string
	reposDir string
	eng      *gitx.Engine
	env      []string
}

// newCRServer builds a logged-in test server with the M6 stack mounted: the
// tracker registry (real builtin factory), vault + materializer, and the git
// engine + repos dir the CR routes need. The agent API is mounted too (same
// store, same registry, same bus) for the builtin PR-create flow test.
func newCRServer(t *testing.T) *crTestServer {
	t.Helper()
	testutil.RequireTool(t, "git")

	vlt, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	home := t.TempDir()
	stateDir := t.TempDir()
	reposDir := filepath.Join(stateDir, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatalf("mkdir repos dir: %v", err)
	}
	mat, err := vault.NewMaterializer(filepath.Join(stateDir, "runtime"))
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	eng := gitx.New("git")
	env := testutil.HermeticGitEnv(home)

	x := newTestServer(t, func(o *Options) {
		reg := tracker.NewRegistry(o.Store, vlt, nil, builtin.New,
			func(tracker.ForgejoConfig) tracker.Tracker {
				panic("forgejo factory invoked in a builtin-only CR test")
			},
			func(tracker.GitHubConfig) tracker.Tracker {
				panic("github factory invoked in a builtin-only CR test")
			})
		mergeSvc := crmerge.New(crmerge.Config{
			Store: o.Store, Git: eng, Vault: vlt, Materializer: mat,
			Bus: o.Bus, ReposDir: reposDir, GitEnv: env, Now: time.Now, Logger: o.Logger,
		})
		reg.SetCRMerger(mergeSvc)
		o.Tracker = reg
		o.Vault = vlt
		o.Git = eng
		o.Materializer = mat
		o.ReposDir = reposDir
		o.GitEnv = env
		o.CRMerge = mergeSvc
		o.AgentHandler = agentapi.New(o.Store, reg, o.Bus, o.Logger, time.Now, nil).Handler()
	})
	x.setup("op", "password123")
	return &crTestServer{testServer: x, home: home, reposDir: reposDir, eng: eng, env: env}
}

// crRepoFixture is one repo's production-shaped git topology.
type crRepoFixture struct {
	x      *crTestServer
	repo   store.Repo
	origin string // bare push target — the repo's REAL remote
	work   string // working clone that advances origin's main
	bare   string // <reposDir>/<repoID>.git — the lab bare reference clone
}

// newCRRepo seeds a builtin-bound repo row (via issues_test's seedTrackerRepo)
// and builds its git fixture: work repo pushed to a bare origin, bare
// reference clone at the repo's production path.
func newCRRepo(t *testing.T, x *crTestServer, name string, mod func(*store.Repo)) *crRepoFixture {
	t.Helper()
	repo := seedTrackerRepo(t, x.testServer, name, mod)

	work := makeRepoOrigin(t, x.home, "main", 2) // f0.txt, f1.txt
	origin := filepath.Join(t.TempDir(), "origin.git")
	repoGitCmd(t, x.home, "", "init", "-q", "--bare", "-b", "main", origin)
	repoGitCmd(t, x.home, work, "remote", "add", "origin", origin)
	repoGitCmd(t, x.home, work, "push", "-q", "origin", "main")

	bare := filepath.Join(x.reposDir, repo.ID+".git")
	if err := x.eng.CloneBare(t.Context(), origin, bare, x.env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	return &crRepoFixture{x: x, repo: repo, origin: origin, work: work, bare: bare}
}

// addHead creates a CR head branch the way a run does: worktree forked from
// origin/main in the bare clone, mutate + commit, worktree removed with the
// branch kept (the guarded teardown keeps an unmerged branch). Returns the
// head sha.
func (f *crRepoFixture) addHead(t *testing.T, branch string, mutate func(dir string)) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), strings.ReplaceAll(branch, "/", "-"))
	if err := f.x.eng.AddWorktree(t.Context(), f.bare, wt, branch, "main", f.x.env); err != nil {
		t.Fatalf("AddWorktree(%s): %v", branch, err)
	}
	mutate(wt)
	repoGitCmd(t, f.x.home, wt, "add", "-A")
	repoGitCmd(t, f.x.home, wt, "commit", "-q", "-m", "cr work on "+branch)
	sha := repoGitCmd(t, f.x.home, wt, "rev-parse", "HEAD")
	if err := f.x.eng.RemoveWorktree(t.Context(), f.bare, wt, f.x.env); err != nil {
		t.Fatalf("RemoveWorktree(%s): %v", branch, err)
	}
	return sha
}

// advanceOrigin commits file=content in the work clone and pushes it to
// origin's main — base movement the bare reference clone has NOT fetched.
func (f *crRepoFixture) advanceOrigin(t *testing.T, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	repoGitCmd(t, f.x.home, f.work, "add", "-A")
	repoGitCmd(t, f.x.home, f.work, "commit", "-q", "-m", "advance "+file)
	repoGitCmd(t, f.x.home, f.work, "push", "-q", "origin", "main")
	return repoGitCmd(t, f.x.home, f.work, "rev-parse", "HEAD")
}

// installRejectHook makes origin refuse every push via a pre-receive hook —
// the protected-branch stand-in. msg is what the hook prints to stderr.
func (f *crRepoFixture) installRejectHook(t *testing.T, msg string) {
	t.Helper()
	hook := "#!/bin/sh\necho \"" + msg + "\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(f.origin, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatalf("install pre-receive hook: %v", err)
	}
}

func (f *crRepoFixture) originMain(t *testing.T) string {
	t.Helper()
	return repoGitCmd(t, f.x.home, f.origin, "rev-parse", "main")
}

// seedCR inserts a CR row directly (the create path is the agent API's; the
// flow test exercises it end-to-end). Closes are parsed from the body with
// the shared grammar, exactly like builtin CreatePull.
func seedCR(t *testing.T, x *crTestServer, repoID, title, body, head string) store.CR {
	t.Helper()
	cr, err := x.st.CreateCR(context.Background(), repoID, title, body, head, "main", tracker.ParseCloses(body), time.Now())
	if err != nil {
		t.Fatalf("CreateCR: %v", err)
	}
	return cr
}

// seedCRIssue creates a builtin issue and returns its number.
func seedCRIssue(t *testing.T, x *crTestServer, repoID, title string) int {
	t.Helper()
	is, err := x.st.CreateIssueWithLabels(context.Background(), repoID, title, "", nil, store.CommentAuthorOperator, nil, time.Now())
	if err != nil {
		t.Fatalf("CreateIssueWithLabels: %v", err)
	}
	return is.Number
}

// setAuthorIdentity configures the global settings identity pair.
func setAuthorIdentity(t *testing.T, x *crTestServer, name, email string) {
	t.Helper()
	if err := x.st.SetSetting(context.Background(), store.SettingGitAuthorName, name); err != nil {
		t.Fatalf("SetSetting name: %v", err)
	}
	if err := x.st.SetSetting(context.Background(), store.SettingGitAuthorEmail, email); err != nil {
		t.Fatalf("SetSetting email: %v", err)
	}
}

// waitForBusEvent polls the recorded bus until an event of the wanted type
// shows up (publishes happen before the HTTP response, but the recorder's
// goroutine drains asynchronously).
func waitForBusEvent(t *testing.T, log *busLog, want string) events.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, e := range log.snapshot() {
			if e.Type == want {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event on the bus within 5s (got %+v)", want, log.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func crsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["crs"].([]any)
	if !ok {
		t.Fatalf("no crs array in %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestCRListAndDetailWithLiveDiff(t *testing.T) {
	x := newCRServer(t)
	f := newCRRepo(t, x, "proj", nil)
	issueN := seedCRIssue(t, x, f.repo.ID, "the tracked issue")
	f.addHead(t, "afk/1", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature line\n"), 0o644); err != nil {
			t.Fatalf("write feature.txt: %v", err)
		}
	})
	seedCR(t, x, f.repo.ID, "feat: the feature", "Adds it.\n\nCloses #1", "afk/1")
	base := "/api/v1/repos/" + f.repo.ID

	// List (state defaults to all): the pinned item shape.
	resp := x.do("GET", base+"/crs", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	items := crsOf(t, decodeBody(t, resp))
	if len(items) != 1 {
		t.Fatalf("crs = %v, want 1", items)
	}
	it := items[0]
	if it["number"] != float64(1) || it["state"] != "open" || it["title"] != "feat: the feature" {
		t.Fatalf("list item = %v", it)
	}
	if it["head_branch"] != "afk/1" || it["base_branch"] != "main" {
		t.Fatalf("branches = %v / %v", it["head_branch"], it["base_branch"])
	}
	if closes := it["closes"].([]any); len(closes) != 1 || closes[0] != float64(issueN) {
		t.Fatalf("closes = %v, want [%d]", closes, issueN)
	}
	if it["merged_at"] != nil || it["merge_commit"] != nil {
		t.Fatalf("open CR carries merged_at/merge_commit: %v", it)
	}
	if it["created_at"] == "" {
		t.Fatal("created_at empty")
	}

	// State filters + the 400 on garbage.
	resp = x.do("GET", base+"/crs?state=open", nil, nil)
	if got := crsOf(t, decodeBody(t, resp)); len(got) != 1 {
		t.Fatalf("state=open list = %v", got)
	}
	resp = x.do("GET", base+"/crs?state=merged", nil, nil)
	if got := crsOf(t, decodeBody(t, resp)); len(got) != 0 {
		t.Fatalf("state=merged list = %v", got)
	}
	resp = x.do("GET", base+"/crs?state=bogus", nil, nil)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()

	// Detail: body plus the LIVE diff from the bare repo.
	resp = x.do("GET", base+"/crs/1", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	detail := decodeBody(t, resp)
	if detail["body"] != "Adds it.\n\nCloses #1" {
		t.Fatalf("detail body = %v", detail["body"])
	}
	diff, ok := detail["diff"].(string)
	if !ok {
		t.Fatalf("detail carries no diff: %v", detail)
	}
	for _, want := range []string{"diff --git a/feature.txt b/feature.txt", "+feature line"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
	if detail["diff_truncated"] != false {
		t.Fatalf("diff_truncated = %v, want false", detail["diff_truncated"])
	}
	if _, hasNote := detail["note"]; hasNote {
		t.Fatalf("detail with a live diff carries a note: %v", detail)
	}

	// Unknown / non-numeric CR numbers: 404.
	for _, path := range []string{base + "/crs/99", base + "/crs/abc", base + "/crs/0"} {
		resp = x.do("GET", path, nil, nil)
		wantStatus(t, resp, http.StatusNotFound)
		_ = resp.Body.Close()
	}
}

func TestCRMergeFastForwardEndToEnd(t *testing.T) {
	x := newCRServer(t)
	f := newCRRepo(t, x, "proj", nil)
	setAuthorIdentity(t, x, "Real Author", "real@example.invalid")
	issueN := seedCRIssue(t, x, f.repo.ID, "fix the wobble")
	head := f.addHead(t, "afk/1", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
			t.Fatalf("write fix.txt: %v", err)
		}
	})
	seedCR(t, x, f.repo.ID, "fix: wobble", "Closes #1", "afk/1")
	log := recordBus(t, x.bus)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + f.repo.ID

	resp := x.do("POST", base+"/crs/1/merge", nil, h)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	cr, ok := body["cr"].(map[string]any)
	if !ok {
		t.Fatalf("merge response = %v, want {cr}", body)
	}
	if cr["state"] != store.CRStateMerged {
		t.Fatalf("cr state = %v, want merged", cr["state"])
	}
	// Fast-forward: the merge commit IS the head sha, origin advanced to it.
	if cr["merge_commit"] != head {
		t.Fatalf("merge_commit = %v, want head sha %s", cr["merge_commit"], head)
	}
	if cr["merged_at"] == nil || cr["merged_at"] == "" {
		t.Fatalf("merged_at = %v", cr["merged_at"])
	}
	if o := f.originMain(t); o != head {
		t.Fatalf("origin main = %s, want %s (fast-forward push)", o, head)
	}

	// The Closes #1 built-in issue is closed.
	is, err := x.st.IssueByRepoNumber(context.Background(), f.repo.ID, issueN)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if is.State != store.IssueStateClosed {
		t.Fatalf("issue state = %q, want closed", is.State)
	}

	// Both pinned events reached the bus.
	waitForBusEvent(t, log, EventCRChanged)
	waitForBusEvent(t, log, EventIssueChanged)

	// Non-open guards: a second merge and a close both 409.
	resp = x.do("POST", base+"/crs/1/merge", nil, h)
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp); !strings.Contains(got["error"].(string), "not open") {
		t.Fatalf("second merge error = %v", got["error"])
	}
	resp = x.do("POST", base+"/crs/1/close", nil, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()
}

// TestCRMergeCommitPathUsesRepoIdentity drives the non-fast-forward path
// (origin's main advanced after the fork) and pins D15 measure 5: the merge
// commit is authored with the repo's configured identity, which overrides
// the global settings pair.
func TestCRMergeCommitPathUsesRepoIdentity(t *testing.T) {
	x := newCRServer(t)
	name, email := "Repo Author", "repo@example.invalid"
	f := newCRRepo(t, x, "proj", func(r *store.Repo) {
		r.GitAuthorName, r.GitAuthorEmail = &name, &email
	})
	setAuthorIdentity(t, x, "Settings Author", "settings@example.invalid") // must lose to the repo override
	head := f.addHead(t, "afk/2", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "cr.txt"), []byte("cr\n"), 0o644); err != nil {
			t.Fatalf("write cr.txt: %v", err)
		}
	})
	baseSHA := f.advanceOrigin(t, "base.txt", "base\n") // diverge; the bare clone has NOT fetched this
	seedCR(t, x, f.repo.ID, "feat: cr", "no closes here", "afk/2")

	resp := x.do("POST", "/api/v1/repos/"+f.repo.ID+"/crs/1/merge", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusOK)
	cr := decodeBody(t, resp)["cr"].(map[string]any)
	mc, _ := cr["merge_commit"].(string)
	if mc == "" || mc == head || mc == baseSHA {
		t.Fatalf("merge_commit = %q, want a fresh merge commit (head %s, base %s)", mc, head, baseSHA)
	}
	if o := f.originMain(t); o != mc {
		t.Fatalf("origin main = %s, want merge commit %s", o, mc)
	}
	wantID := name + "|" + email + "|" + name + "|" + email
	if id := repoGitCmd(t, x.home, f.origin, "log", "-1", "--format=%an|%ae|%cn|%ce", mc); id != wantID {
		t.Fatalf("merge commit identity = %q, want repo override %q", id, wantID)
	}
}

func TestCRMergePushRejected409(t *testing.T) {
	x := newCRServer(t)
	f := newCRRepo(t, x, "proj", nil)
	setAuthorIdentity(t, x, "Real Author", "real@example.invalid")
	f.addHead(t, "afk/1", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "cr.txt"), []byte("cr\n"), 0o644); err != nil {
			t.Fatalf("write cr.txt: %v", err)
		}
	})
	seedCR(t, x, f.repo.ID, "feat: cr", "", "afk/1")
	f.installRejectHook(t, "push declined: protected branch main")

	resp := x.do("POST", "/api/v1/repos/"+f.repo.ID+"/crs/1/merge", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
	got := decodeBody(t, resp)
	// The 409 body carries the hook's stderr verbatim — that text is what
	// the operator needs to read.
	if !strings.Contains(got["error"].(string), "push declined: protected branch main") {
		t.Fatalf("409 body = %v, want the hook stderr", got["error"])
	}
	cr, err := x.st.CRByRepoNumber(context.Background(), f.repo.ID, 1)
	if err != nil {
		t.Fatalf("CRByRepoNumber: %v", err)
	}
	if cr.State != store.CRStateOpen {
		t.Fatalf("CR state after rejected push = %q, want still open", cr.State)
	}
}

// TestCRMissingHeadBranch pins both halves of the missing-head contract: the
// detail view omits the diff with a note instead of failing, and the merge
// answers 409 (e.g. after a Parked Discard deleted the branch).
func TestCRMissingHeadBranch(t *testing.T) {
	x := newCRServer(t)
	f := newCRRepo(t, x, "proj", nil)
	setAuthorIdentity(t, x, "Real Author", "real@example.invalid")
	seedCR(t, x, f.repo.ID, "feat: ghost", "", "afk/ghost")
	base := "/api/v1/repos/" + f.repo.ID

	resp := x.do("GET", base+"/crs/1", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	detail := decodeBody(t, resp)
	if _, hasDiff := detail["diff"]; hasDiff {
		t.Fatalf("detail with a missing head carries a diff: %v", detail)
	}
	note, _ := detail["note"].(string)
	if !strings.Contains(note, "afk/ghost") {
		t.Fatalf("note = %q, want it to name the missing branch", note)
	}

	resp = x.do("POST", base+"/crs/1/merge", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp); !strings.Contains(got["error"].(string), "head branch missing") {
		t.Fatalf("merge 409 body = %v, want the head-missing diagnostic", got["error"])
	}
}

// TestCRMergeAuthorIdentityRequired: with neither the repo fields nor the
// settings pair configured there is no real identity to author the merge as
// (D15 measure 5 forbids a bot fallback) — 409 before any git runs.
func TestCRMergeAuthorIdentityRequired(t *testing.T) {
	x := newCRServer(t)
	f := newCRRepo(t, x, "proj", nil)
	f.addHead(t, "afk/1", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "cr.txt"), []byte("cr\n"), 0o644); err != nil {
			t.Fatalf("write cr.txt: %v", err)
		}
	})
	seedCR(t, x, f.repo.ID, "feat: cr", "", "afk/1")
	before := f.originMain(t)

	resp := x.do("POST", "/api/v1/repos/"+f.repo.ID+"/crs/1/merge", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp); got["error"] != crmerge.NoAuthorIdentityMessage {
		t.Fatalf("409 body = %v, want %q", got["error"], crmerge.NoAuthorIdentityMessage)
	}
	if o := f.originMain(t); o != before {
		t.Fatalf("origin main moved (%s → %s) despite the identity refusal", before, o)
	}
}

func TestCRCloseLifecycle(t *testing.T) {
	x := newCRServer(t)
	repo := seedTrackerRepo(t, x.testServer, "proj", nil) // no git needed: close never touches the bare repo
	if _, err := x.st.CreateCR(context.Background(), repo.ID, "feat: park it", "", "afk/1", "main", nil, time.Now()); err != nil {
		t.Fatalf("CreateCR: %v", err)
	}
	log := recordBus(t, x.bus)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	resp := x.do("POST", base+"/crs/1/close", nil, h)
	wantStatus(t, resp, http.StatusOK)
	cr := decodeBody(t, resp)["cr"].(map[string]any)
	if cr["state"] != store.CRStateClosed || cr["merged_at"] != nil || cr["merge_commit"] != nil {
		t.Fatalf("closed cr = %v (closed-unmerged must not carry merge fields)", cr)
	}
	waitForBusEvent(t, log, EventCRChanged)

	// Closed is terminal for both transitions (reopen is roadmap).
	resp = x.do("POST", base+"/crs/1/close", nil, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()
	resp = x.do("POST", base+"/crs/1/merge", nil, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()

	// Unknown number: 404.
	resp = x.do("POST", base+"/crs/9/close", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// TestCRRoutesForgeBound409: every /crs route answers the pinned 409 on a
// forge-bound repo — its PRs live on the forge.
func TestCRRoutesForgeBound409(t *testing.T) {
	x := newCRServer(t)
	repo := seedTrackerRepo(t, x.testServer, "forged", func(r *store.Repo) {
		r.TrackerBinding = store.TrackerBindingForge
		r.ForgeKind = "forgejo"
	})
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	for _, rt := range []struct{ method, path string }{
		{"GET", base + "/crs"},
		{"GET", base + "/crs/1"},
		{"POST", base + "/crs/1/merge"},
		{"POST", base + "/crs/1/close"},
	} {
		resp := x.do(rt.method, rt.path, nil, h)
		wantStatus(t, resp, http.StatusConflict)
		if got := decodeBody(t, resp); got["error"] != forgeCRMessage {
			t.Fatalf("%s %s error = %v, want %q", rt.method, rt.path, got["error"], forgeCRMessage)
		}
	}
}

// TestCRMutationsRequireCSRF: the merge/close mutations sit behind the CSRF
// guard like every ambient-credential mutation.
func TestCRMutationsRequireCSRF(t *testing.T) {
	x := newCRServer(t)
	repo := seedTrackerRepo(t, x.testServer, "proj", nil)
	if _, err := x.st.CreateCR(context.Background(), repo.ID, "t", "", "afk/1", "main", nil, time.Now()); err != nil {
		t.Fatalf("CreateCR: %v", err)
	}
	base := "/api/v1/repos/" + repo.ID
	for _, path := range []string{base + "/crs/1/merge", base + "/crs/1/close"} {
		resp := x.do("POST", path, nil, nil) // session cookie, no CSRF headers
		wantStatus(t, resp, http.StatusForbidden)
		_ = resp.Body.Close()
	}
	// Nothing changed.
	cr, err := x.st.CRByRepoNumber(context.Background(), repo.ID, 1)
	if err != nil {
		t.Fatalf("CRByRepoNumber: %v", err)
	}
	if cr.State != store.CRStateOpen {
		t.Fatalf("CR state = %q after CSRF-blocked mutations, want open", cr.State)
	}
}

// TestAgentBuiltinPRCreateFullLoop is the M6 acceptance slice through the
// real stack: an AFK run's `labctl pr create` (POST /agent/v1/prs with a run
// token) opens a change request whose injected Closes lands in cr_closes,
// the CR shows in the operator list, and the operator's merge closes the
// built-in issue — a full cycle with no forge anywhere.
func TestAgentBuiltinPRCreateFullLoop(t *testing.T) {
	x := newCRServer(t)
	f := newCRRepo(t, x, "proj", nil)
	setAuthorIdentity(t, x, "Real Author", "real@example.invalid")
	issueN := seedCRIssue(t, x, f.repo.ID, "make it work")
	head := f.addHead(t, "afk/1", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o644); err != nil {
			t.Fatalf("write work.txt: %v", err)
		}
	})
	log := recordBus(t, x.bus)

	// The run row + token, the way the AFK engine mints them.
	run, err := x.st.CreateRun(context.Background(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: store.RunKindAFKAuto,
		Provider: "claude-code", IssueNumber: &issueN, Branch: "afk/1",
		WorktreePath: "/tmp/wt", SessionName: "sess-cr-test", Model: "opus[1m]",
		Effort: "max", StartedAt: time.Now(), Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	token, hash := ids.NewToken("run")
	if err := x.st.CreateRunToken(context.Background(), run.ID, hash, nil, time.Now()); err != nil {
		t.Fatalf("CreateRunToken: %v", err)
	}

	// Agent: create the PR (builtin → change request). The body carries no
	// Closes; the agent API injects `Closes #<claimed issue>` (pinned rule).
	resp := doWith(t, http.DefaultClient, x.ts.URL, "POST", "/agent/v1/prs",
		map[string]any{"title": "feat: make it work", "body": "All done."},
		map[string]string{"Authorization": "Bearer " + token})
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	if created["number"] != float64(1) {
		t.Fatalf("created number = %v, want 1", created["number"])
	}
	if created["url"] != "/repos/"+f.repo.ID+"/crs/1" {
		t.Fatalf("created url = %v, want the lab-relative CR route", created["url"])
	}
	waitForBusEvent(t, log, EventCRChanged)

	// Operator: the CR is in the list with the injected closes.
	base := "/api/v1/repos/" + f.repo.ID
	resp = x.do("GET", base+"/crs", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	items := crsOf(t, decodeBody(t, resp))
	if len(items) != 1 || items[0]["head_branch"] != "afk/1" || items[0]["state"] != "open" {
		t.Fatalf("crs list = %v", items)
	}
	if closes := items[0]["closes"].([]any); len(closes) != 1 || closes[0] != float64(issueN) {
		t.Fatalf("closes = %v, want [%d] (the injected directive)", closes, issueN)
	}

	// Operator: merge from the UI; origin advances, the issue closes.
	resp = x.do("POST", base+"/crs/1/merge", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	if o := f.originMain(t); o != head {
		t.Fatalf("origin main = %s, want %s", o, head)
	}
	is, err := x.st.IssueByRepoNumber(context.Background(), f.repo.ID, issueN)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if is.State != store.IssueStateClosed {
		t.Fatalf("issue state = %q, want closed after the merge", is.State)
	}
}

// TestAgentBuiltinPRMergeFullLoop proves the acceptance criteria on a
// builtin-bound repo entirely over the run-token agent surface: the agent's
// POST /agent/v1/prs/{n}/merge lands the CR through the SAME crmerge service
// the operator route uses (ff push here), closing every Closes #N and emitting
// cr.changed/issue.changed; the head branch survives the merge (teardown/sweep
// GCs it, not merge); and a re-merge is a convergent no-op success rather than
// an error.
func TestAgentBuiltinPRMergeFullLoop(t *testing.T) {
	x := newCRServer(t)
	f := newCRRepo(t, x, "proj", nil)
	setAuthorIdentity(t, x, "Real Author", "real@example.invalid")
	issueN := seedCRIssue(t, x, f.repo.ID, "land it")
	head := f.addHead(t, "afk/1", func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "land.txt"), []byte("land\n"), 0o644); err != nil {
			t.Fatalf("write land.txt: %v", err)
		}
	})
	seedCR(t, x, f.repo.ID, "feat: land it", "Closes #1", "afk/1")
	log := recordBus(t, x.bus)

	// The run row + token, the way the AFK engine mints them.
	run, err := x.st.CreateRun(context.Background(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: store.RunKindAFKAuto,
		Provider: "claude-code", IssueNumber: &issueN, Branch: "afk/1",
		WorktreePath: "/tmp/wt", SessionName: "sess-merge-test", Model: "opus[1m]",
		Effort: "max", StartedAt: time.Now(), Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	token, hash := ids.NewToken("run")
	if err := x.st.CreateRunToken(context.Background(), run.ID, hash, nil, time.Now()); err != nil {
		t.Fatalf("CreateRunToken: %v", err)
	}
	auth := map[string]string{"Authorization": "Bearer " + token}

	// Agent merge: 200 with the merged state, over the run-token surface only.
	resp := doWith(t, http.DefaultClient, x.ts.URL, "POST", "/agent/v1/prs/1/merge", nil, auth)
	wantStatus(t, resp, http.StatusOK)
	merged := decodeBody(t, resp)
	if merged["state"] != store.CRStateMerged {
		t.Fatalf("merge state = %v, want merged", merged["state"])
	}
	if merged["url"] != "/repos/"+f.repo.ID+"/crs/1" {
		t.Fatalf("merge url = %v, want the lab-relative CR route", merged["url"])
	}

	// The same service the operator route uses: origin advanced (fast-forward),
	// the Closes #1 issue closed, both events published.
	if o := f.originMain(t); o != head {
		t.Fatalf("origin main = %s, want %s (fast-forward push)", o, head)
	}
	is, err := x.st.IssueByRepoNumber(context.Background(), f.repo.ID, issueN)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if is.State != store.IssueStateClosed {
		t.Fatalf("issue state = %q, want closed after the agent merge", is.State)
	}
	waitForBusEvent(t, log, EventCRChanged)
	waitForBusEvent(t, log, EventIssueChanged)

	// The head branch still exists immediately after the merge — teardown/the
	// sweep GCs a merged head, merge does not touch it (acceptance criterion).
	if sha := repoGitCmd(t, x.home, f.bare, "rev-parse", "refs/heads/afk/1"); sha != head {
		t.Fatalf("head branch afk/1 = %q after merge, want it still at %s", sha, head)
	}

	// Convergent: re-merging an already-merged CR is a no-op SUCCESS, not an
	// error (unlike the operator route's 409 — the agent land step is retried).
	resp = doWith(t, http.DefaultClient, x.ts.URL, "POST", "/agent/v1/prs/1/merge", nil, auth)
	wantStatus(t, resp, http.StatusOK)
	if again := decodeBody(t, resp); again["state"] != store.CRStateMerged {
		t.Fatalf("re-merge state = %v, want a convergent merged no-op", again["state"])
	}
}
