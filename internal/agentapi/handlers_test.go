package agentapi

// Handler-contract tests (pinned M5): repo scope comes strictly from the run
// row, Closes-injection on AFK PR creation, run-authored builtin comments,
// the builtin 501 CR seam, and the 404/409/502 tracker error mapping.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// fakeTracker is a recording tracker.Tracker for the forge-bound paths.
type fakeTracker struct {
	issues []tracker.Issue
	err    error // returned by every read/write when set

	createdPull *pullArgs
	pullRef     tracker.PullRef
	comments    []commentArgs
}

type pullArgs struct{ head, base, title, body string }
type commentArgs struct {
	number int
	body   string
}

func (f *fakeTracker) ReadyIssues(context.Context) ([]tracker.Issue, error) {
	return f.issues, f.err
}
func (f *fakeTracker) Issues(context.Context, string) ([]tracker.Issue, error) {
	return f.issues, f.err
}
func (f *fakeTracker) Issue(_ context.Context, number int) (tracker.Issue, error) {
	if f.err != nil {
		return tracker.Issue{}, f.err
	}
	for _, is := range f.issues {
		if is.Number == number {
			return is, nil
		}
	}
	return tracker.Issue{}, fmt.Errorf("issue %d: %w", number, tracker.ErrNotFound)
}
func (f *fakeTracker) CreateComment(_ context.Context, number int, body string) error {
	if f.err != nil {
		return f.err
	}
	f.comments = append(f.comments, commentArgs{number: number, body: body})
	return nil
}
func (f *fakeTracker) Pulls(context.Context) ([]tracker.PullRef, error) {
	return []tracker.PullRef{}, f.err
}
func (f *fakeTracker) CreatePull(_ context.Context, head, base, title, body string) (tracker.PullRef, error) {
	if f.err != nil {
		return tracker.PullRef{}, f.err
	}
	f.createdPull = &pullArgs{head: head, base: base, title: title, body: body}
	return f.pullRef, nil
}
func (f *fakeTracker) CloseIssue(context.Context, int) error { return f.err }

// forgeServer builds a Server whose resolver answers fk for every repo.
func (f *testFixture) forgeServer(fk tracker.Tracker) *Server {
	return New(f.st, resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) {
		return fk, nil
	}), discard(), func() time.Time { return f.now })
}

func doJSON(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestClaimedIssue(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix the frobnicator", "It wobbles.")
	f.seedRun(t, "run_afk", "repo_a", "active")
	f.seedRunKind(t, "run_manual", "repo_a", "manual", "active", nil, "lab/tinker")
	afkToken := f.seedToken(t, "run_afk", nil)
	manualToken := f.seedToken(t, "run_manual", nil)

	// Comment on the issue so the detail view provably carries the thread.
	if _, err := f.st.CreateIssueComment(context.Background(), "iss7", store.CommentAuthorOperator, nil, "please fix", f.now); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}

	handler := f.server().Handler()

	rr := doJSON(t, handler, "GET", "/agent/v1/issue", afkToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /issue: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var got issueDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Number != 7 || got.Title != "Fix the frobnicator" || got.State != "open" {
		t.Fatalf("issue = %+v", got)
	}
	if len(got.Comments) != 1 || got.Comments[0].Author != store.CommentAuthorOperator {
		t.Fatalf("comments = %+v, want the seeded operator comment", got.Comments)
	}
	if got.Comments[0].Body != "please fix" {
		t.Fatalf("comment body = %q", got.Comments[0].Body)
	}

	// A manual run claims no issue: 404 with the pinned message.
	rr = doJSON(t, handler, "GET", "/agent/v1/issue", manualToken, "")
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), noClaimedIssueMessage) {
		t.Fatalf("manual run GET /issue: status = %d, body %q", rr.Code, rr.Body.String())
	}
}

// TestRepoScopeEnforcement pins the authorization model: the run's token
// reaches only ITS repo's issues, regardless of what numbers the URL names.
func TestRepoScopeEnforcement(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRepo(t, "repo_b")
	f.seedIssue(t, "iss_a1", "repo_a", 1, "A's issue", "")
	f.seedIssue(t, "iss_b42", "repo_b", 42, "B's secret issue", "hidden")
	f.seedRunKind(t, "run_a", "repo_a", "afk_manual", "active", intp(1), "afk/1")
	tokenA := f.seedToken(t, "run_a", nil)

	handler := f.server().Handler()

	// Issue numbers of another repo do not resolve — same 404 as nonexistent.
	for _, path := range []string{"/agent/v1/issues/42", "/agent/v1/issues/9999"} {
		rr := doJSON(t, handler, "GET", path, tokenA, "")
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s: status = %d, want 404 (body %s)", path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "secret") {
			t.Fatalf("GET %s leaked another repo's issue: %s", path, rr.Body.String())
		}
	}

	// The list view carries only the run's repo.
	rr := doJSON(t, handler, "GET", "/agent/v1/issues", tokenA, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /issues: status = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "secret") || !strings.Contains(rr.Body.String(), "A's issue") {
		t.Fatalf("GET /issues = %s, want only repo_a's issues", rr.Body.String())
	}

	// Commenting on another repo's issue number fails without side effects.
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/42/comments", tokenA, `{"body":"intrusion"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-repo comment: status = %d, want 404", rr.Code)
	}
	isB, err := f.st.IssueByRepoNumber(context.Background(), "repo_b", 42)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if len(isB.Comments) != 0 {
		t.Fatalf("repo_b issue gained comments: %+v", isB.Comments)
	}
}

// TestCommentAttribution pins the builtin identity seam: an agent comment
// lands author_kind=run carrying the run's id.
func TestCommentAttribution(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix it", "")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)

	handler := f.server().Handler()
	rr := doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, `{"body":"working on it"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}

	is, err := f.st.IssueByRepoNumber(context.Background(), "repo_a", 7)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if len(is.Comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(is.Comments))
	}
	c := is.Comments[0]
	if c.AuthorKind != store.CommentAuthorRun {
		t.Errorf("author kind = %q, want run", c.AuthorKind)
	}
	if c.RunID == nil || *c.RunID != "run_afk" {
		t.Errorf("run id = %v, want run_afk", c.RunID)
	}
	if c.Body != "working on it" {
		t.Errorf("body = %q", c.Body)
	}

	// Empty body: 400, nothing written.
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, `{"body":"  "}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty body: status = %d, want 400", rr.Code)
	}
}

// TestForgeCommentPassthrough: on a forge-bound repo the comment goes through
// the tracker as-is (identity is the forge token's, not lab's).
func TestForgeCommentPassthrough(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)

	fk := &fakeTracker{}
	handler := f.forgeServer(fk).Handler()

	rr := doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, `{"body":"status update"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if len(fk.comments) != 1 || fk.comments[0] != (commentArgs{number: 7, body: "status update"}) {
		t.Fatalf("forwarded comments = %+v", fk.comments)
	}
}

// TestPRCreateClosesMatrix pins the pinned-contract injection rule: AFK runs
// always ship `Closes #<N>`; manual runs are untouched; head/base come from
// the run row and repo, never the request.
func TestPRCreateClosesMatrix(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		issue    *int
		branch   string
		body     string
		wantBody string
	}{
		{"afk missing closes", "afk_auto", intp(7), "afk/7",
			"Implements the thing.", "Implements the thing.\n\nCloses #7"},
		{"afk closes present", "afk_manual", intp(7), "afk/7",
			"Done.\n\nCloses #7", "Done.\n\nCloses #7"},
		{"afk closes case-insensitive", "afk_auto", intp(7), "afk/7",
			"closes #7 in passing", "closes #7 in passing"},
		{"afk wrong issue number", "afk_auto", intp(7), "afk/7",
			"Closes #70", "Closes #70\n\nCloses #7"},
		{"afk empty body", "afk_auto", intp(7), "afk/7",
			"", "Closes #7"},
		{"manual run untouched", "manual", nil, "lab/tinker",
			"No issue here.", "No issue here."},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
			runID := fmt.Sprintf("run_%d", i)
			f.seedRunKind(t, runID, "repo_f", tt.kind, "active", tt.issue, tt.branch)
			token := f.seedToken(t, runID, nil)

			fk := &fakeTracker{pullRef: tracker.PullRef{Number: 12, URL: "https://forge.example/pr/12"}}
			handler := f.forgeServer(fk).Handler()

			payload, _ := json.Marshal(map[string]string{"title": "feat: do it", "body": tt.body})
			rr := doJSON(t, handler, "POST", "/agent/v1/prs", token, string(payload))
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
			}
			if fk.createdPull == nil {
				t.Fatal("CreatePull not called")
			}
			if fk.createdPull.body != tt.wantBody {
				t.Errorf("PR body = %q, want %q", fk.createdPull.body, tt.wantBody)
			}
			if fk.createdPull.head != tt.branch {
				t.Errorf("head = %q, want the run's branch %q", fk.createdPull.head, tt.branch)
			}
			if fk.createdPull.base != "main" {
				t.Errorf("base = %q, want the repo default branch", fk.createdPull.base)
			}
			if fk.createdPull.title != "feat: do it" {
				t.Errorf("title = %q", fk.createdPull.title)
			}

			var resp struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Number != 12 || resp.URL != "https://forge.example/pr/12" {
				t.Errorf("response = %+v", resp)
			}
		})
	}
}

func TestPRCreateValidation(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)

	fk := &fakeTracker{}
	handler := f.forgeServer(fk).Handler()

	rr := doJSON(t, handler, "POST", "/agent/v1/prs", token, `{"title":"  ","body":"b"}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "title is required") {
		t.Fatalf("blank title: status = %d, body %q", rr.Code, rr.Body.String())
	}
	if fk.createdPull != nil {
		t.Fatal("CreatePull called despite validation failure")
	}
}

// TestPRCreateBuiltinSeam pins the M6 seam: builtin-bound → 501 with the
// pinned message, before any tracker is even resolved.
func TestPRCreateBuiltinSeam(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)

	resolved := false
	s := New(f.st, resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) {
		resolved = true
		return nil, errors.New("must not be called")
	}), discard(), func() time.Time { return f.now })

	rr := doJSON(t, s.Handler(), "POST", "/agent/v1/prs", token, `{"title":"t","body":"b"}`)
	if rr.Code != http.StatusNotImplemented || !strings.Contains(rr.Body.String(), crSeamMessage) {
		t.Fatalf("status = %d, body %q, want 501 %q", rr.Code, rr.Body.String(), crSeamMessage)
	}
	if resolved {
		t.Fatal("tracker resolved for a builtin PR create (the 501 must answer first)")
	}
}

// TestTrackerErrorMapping pins the pinned taxonomy: upstream 404 → 404,
// resolver config conflict → 409, other forge failures → 502.
func TestTrackerErrorMapping(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)

	// Upstream miss → 404.
	fk := &fakeTracker{err: fmt.Errorf("GET issue: status 404: %w", tracker.ErrNotFound)}
	rr := doJSON(t, f.forgeServer(fk).Handler(), "GET", "/agent/v1/issues/7", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("upstream 404: status = %d, want 404", rr.Code)
	}

	// Upstream outage → 502 with the diagnostic.
	fk = &fakeTracker{err: errors.New("forge unreachable")}
	rr = doJSON(t, f.forgeServer(fk).Handler(), "GET", "/agent/v1/issues", token, "")
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "forge unreachable") {
		t.Fatalf("upstream failure: status = %d, body %q, want 502", rr.Code, rr.Body.String())
	}

	// Tracker configuration conflict → 409.
	s := New(f.st, resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) {
		return nil, fmt.Errorf("tracker for repo: %w", tracker.ErrForgeCredentialMissing)
	}), discard(), func() time.Time { return f.now })
	rr = doJSON(t, s.Handler(), "GET", "/agent/v1/issues", token, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("config conflict: status = %d, want 409", rr.Code)
	}
}

func TestEnsureCloses(t *testing.T) {
	tests := []struct {
		body string
		n    int
		want string
	}{
		{"", 7, "Closes #7"},
		{"Does the thing.", 7, "Does the thing.\n\nCloses #7"},
		{"Closes #7", 7, "Closes #7"},
		{"closes #7", 7, "closes #7"},
		{"CLOSES #7 and more", 7, "CLOSES #7 and more"},
		{"Closes #70", 7, "Closes #70\n\nCloses #7"},
		{"Closes #7!", 7, "Closes #7!"},
		{"See Closes #71, Closes #7.", 7, "See Closes #71, Closes #7."},
		{"Closes #17", 7, "Closes #17\n\nCloses #7"},
		// Word/number boundaries: bare substrings and trailing tokens are NOT a
		// real close of #7, so the trailer is still injected.
		{"This discloses #7 in the docs.", 7, "This discloses #7 in the docs.\n\nCloses #7"},
		{"Closes #7abc", 7, "Closes #7abc\n\nCloses #7"},
		// The full closing-keyword grammar counts (not just "closes"): a real
		// "Fixes #7" already closes the issue, so nothing is injected.
		{"Fixes #7", 7, "Fixes #7"},
		{"Resolved #7 earlier.", 7, "Resolved #7 earlier."},
		{"fix #7", 7, "fix #7"},
	}
	for _, tt := range tests {
		if got := ensureCloses(tt.body, tt.n); got != tt.want {
			t.Errorf("ensureCloses(%q, %d) = %q, want %q", tt.body, tt.n, got, tt.want)
		}
	}
}
