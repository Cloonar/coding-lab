package agentapi

// Handler-contract tests (pinned M5/M6): repo scope comes strictly from the
// run row, Closes-injection on AFK PR creation, run-authored builtin
// comments, builtin PR create → change request (with the injected Closes
// flowing into cr_closes), and the 404/409/502 tracker error mapping.

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

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// fakeTracker is a recording tracker.Tracker for the forge-bound paths.
type fakeTracker struct {
	issues []tracker.Issue
	labels []tracker.Label
	err    error // returned by every read/write when set

	createdPull  *pullArgs
	pullRef      tracker.PullRef
	mergeRef     tracker.PullRef // returned by MergePull on success
	merged       []int           // records MergePull arguments
	pulls        []tracker.PullRef
	pullDetails  []tracker.PullDetail
	checks       []tracker.Check // returned by Checks (with f.err)
	comments     []commentArgs
	createdIssue *issueArgs
	labelAdds    []labelsArgs
	labelRemoves []labelsArgs
	ensured      []tracker.Label
	closed       []int
}

type pullArgs struct{ head, base, title, body string }
type commentArgs struct {
	number int
	body   string
}
type issueArgs struct {
	title, body string
	labels      []string
}
type labelsArgs struct {
	number int
	labels []string
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
	if f.err != nil {
		return nil, f.err
	}
	if f.pulls == nil {
		return []tracker.PullRef{}, nil
	}
	return f.pulls, nil
}
func (f *fakeTracker) Pull(_ context.Context, number int) (tracker.PullDetail, error) {
	if f.err != nil {
		return tracker.PullDetail{}, f.err
	}
	for _, pd := range f.pullDetails {
		if pd.Number == number {
			return pd, nil
		}
	}
	return tracker.PullDetail{}, fmt.Errorf("pull %d: %w", number, tracker.ErrNotFound)
}
func (f *fakeTracker) Checks(context.Context, int) ([]tracker.Check, error) {
	return f.checks, f.err
}
func (f *fakeTracker) CreatePull(_ context.Context, head, base, title, body string) (tracker.PullRef, error) {
	if f.err != nil {
		return tracker.PullRef{}, f.err
	}
	f.createdPull = &pullArgs{head: head, base: base, title: title, body: body}
	return f.pullRef, nil
}
func (f *fakeTracker) MergePull(_ context.Context, number int) (tracker.PullRef, error) {
	if f.err != nil {
		return tracker.PullRef{}, f.err
	}
	f.merged = append(f.merged, number)
	return f.mergeRef, nil
}
func (f *fakeTracker) CloseIssue(_ context.Context, number int) error {
	if f.err != nil {
		return f.err
	}
	f.closed = append(f.closed, number)
	return nil
}
func (f *fakeTracker) CreateIssue(_ context.Context, title, body string, labels []string) (tracker.Issue, error) {
	if f.err != nil {
		return tracker.Issue{}, f.err
	}
	f.createdIssue = &issueArgs{title: title, body: body, labels: labels}
	return tracker.Issue{Number: 101, Title: title, Body: body, State: tracker.StateOpen, Labels: labels}, nil
}
func (f *fakeTracker) AddIssueLabels(_ context.Context, number int, labels []string) error {
	if f.err != nil {
		return f.err
	}
	f.labelAdds = append(f.labelAdds, labelsArgs{number: number, labels: labels})
	return nil
}
func (f *fakeTracker) RemoveIssueLabels(_ context.Context, number int, labels []string) error {
	if f.err != nil {
		return f.err
	}
	f.labelRemoves = append(f.labelRemoves, labelsArgs{number: number, labels: labels})
	return nil
}
func (f *fakeTracker) Labels(context.Context) ([]tracker.Label, error) {
	return f.labels, f.err
}
func (f *fakeTracker) EnsureLabel(_ context.Context, name, color, description string) (tracker.Label, error) {
	if f.err != nil {
		return tracker.Label{}, f.err
	}
	l := tracker.Label{Name: name, Color: color, Description: description}
	f.ensured = append(f.ensured, l)
	return l, nil
}

// forgeServer builds a Server whose resolver answers fk for every repo. It
// wires the claude fixture scrub (claudeScrub) as the sanitizer's declared
// union, so the incogni body tests exercise the real per-line predicate
// (ADR-0033); non-incogni tests gate out before the scrub is consulted.
func (f *testFixture) forgeServer(fk tracker.Tracker) *Server {
	return New(f.st, resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) {
		return fk, nil
	}), nil, discard(), func() time.Time { return f.now }, claudeScrub)
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

// TestIssueCreate_builtinAttributionLabelsAndEvent pins the ADR-0014 agent
// create on a builtin repo: the issue lands author_kind=run with the run's id
// (the same ForRun seam as comments), labels attach at creation, the response
// is the list-shape issue, and issue.changed is published for the operator UI.
func TestIssueCreate_builtinAttributionLabelsAndEvent(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedLabel(t, "lbl_nt", "repo_a", "needs-triage")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)

	bus := events.NewBus()
	ch, cancel := bus.Subscribe(context.Background())
	defer cancel()
	s := New(f.st, builtinResolver(f.st), bus, discard(), func() time.Time { return f.now }, nil)

	rr := doJSON(t, s.Handler(), "POST", "/agent/v1/issues", token,
		`{"title":"found: flaky teardown","body":"Discovered mid-run.","labels":["needs-triage"]}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var got issueResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Number != 1 || got.State != "open" || got.Title != "found: flaky teardown" {
		t.Errorf("response = %+v, want #1 open", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "needs-triage" {
		t.Errorf("response labels = %v, want [needs-triage]", got.Labels)
	}

	is, err := f.st.IssueByRepoNumber(context.Background(), "repo_a", 1)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if is.AuthorKind != store.CommentAuthorRun || is.RunID == nil || *is.RunID != "run_afk" {
		t.Errorf("stored author = (%q, %v), want (run, run_afk)", is.AuthorKind, is.RunID)
	}
	if len(is.Labels) != 1 || is.Labels[0] != "needs-triage" {
		t.Errorf("stored labels = %v, want [needs-triage]", is.Labels)
	}

	select {
	case e := <-ch:
		if e.Type != EventIssueChanged {
			t.Errorf("event type = %q, want %q", e.Type, EventIssueChanged)
		}
	default:
		t.Error("no issue.changed published on builtin issue create")
	}
}

// TestIssueCreate_validation pins the failure half: a blank title and an
// unknown label are 400s and nothing is created — the typo never becomes a
// label or an issue.
func TestIssueCreate_validation(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedLabel(t, "lbl_nt", "repo_a", "needs-triage")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)
	handler := f.server().Handler()

	rr := doJSON(t, handler, "POST", "/agent/v1/issues", token, `{"title":"  ","body":"b"}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "title is required") {
		t.Fatalf("blank title: status = %d, body %q", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, handler, "POST", "/agent/v1/issues", token,
		`{"title":"t","body":"","labels":["needs-triage","ready-for-agnet"]}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "ready-for-agnet") {
		t.Fatalf("unknown label: status = %d, body %q, want 400 naming the typo", rr.Code, rr.Body.String())
	}

	issues, err := f.st.IssuesByRepo(context.Background(), "repo_a", store.IssueStateAll)
	if err != nil {
		t.Fatalf("IssuesByRepo: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("issues after rejected creates = %+v, want none", issues)
	}
	labels, err := f.st.LabelsByRepo(context.Background(), "repo_a")
	if err != nil {
		t.Fatalf("LabelsByRepo: %v", err)
	}
	if len(labels) != 1 {
		t.Errorf("labels after rejected create = %+v — the typo must not become a label", labels)
	}
}

// TestIssueLabelsAndClose_builtin drives the triage state machine end-to-end
// on a builtin repo: label add, role swap via remove, unknown-label 400,
// empty-set 400, close after the explanation comment.
func TestIssueLabelsAndClose_builtin(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix it", "")
	f.seedLabel(t, "lbl_nt", "repo_a", "needs-triage")
	f.seedLabel(t, "lbl_wf", "repo_a", "wontfix")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)
	handler := f.server().Handler()
	ctx := context.Background()

	rr := doJSON(t, handler, "POST", "/agent/v1/issues/7/labels", token, `{"labels":["needs-triage"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("label add: status = %d, body %s", rr.Code, rr.Body.String())
	}
	is, _ := f.st.IssueByRepoNumber(ctx, "repo_a", 7)
	if len(is.Labels) != 1 || is.Labels[0] != "needs-triage" {
		t.Fatalf("labels after add = %v", is.Labels)
	}

	// The triage-role swap: wontfix on, needs-triage off.
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/labels", token, `{"labels":["wontfix"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("swap add: status = %d", rr.Code)
	}
	rr = doJSON(t, handler, "DELETE", "/agent/v1/issues/7/labels", token, `{"labels":["needs-triage"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("swap remove: status = %d, body %s", rr.Code, rr.Body.String())
	}
	is, _ = f.st.IssueByRepoNumber(ctx, "repo_a", 7)
	if len(is.Labels) != 1 || is.Labels[0] != "wontfix" {
		t.Fatalf("labels after swap = %v, want [wontfix]", is.Labels)
	}

	// Failure shapes: unknown label 400 (both ops), empty set 400, missing
	// issue 404.
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/labels", token, `{"labels":["nope"]}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "nope") {
		t.Fatalf("unknown add: status = %d, body %q", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, handler, "DELETE", "/agent/v1/issues/7/labels", token, `{"labels":["nope"]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown remove: status = %d", rr.Code)
	}
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/labels", token, `{"labels":[]}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "labels is required") {
		t.Fatalf("empty labels: status = %d, body %q", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/999/labels", token, `{"labels":["wontfix"]}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing issue label add: status = %d, want 404", rr.Code)
	}

	// The wontfix path: explanation comment, then close — two calls.
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, `{"body":"wontfix: by design"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("explanation comment: status = %d", rr.Code)
	}
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/close", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("close: status = %d, body %s", rr.Code, rr.Body.String())
	}
	is, _ = f.st.IssueByRepoNumber(ctx, "repo_a", 7)
	if is.State != store.IssueStateClosed || is.ClosedAt == nil {
		t.Errorf("after close = state %q closed_at %v, want closed", is.State, is.ClosedAt)
	}
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/999/close", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("close missing: status = %d, want 404", rr.Code)
	}
}

// TestLabelListAndEnsure_builtin pins the self-healing label surface: list,
// create-if-absent, and the idempotent re-ensure a skill runs unconditionally.
func TestLabelListAndEnsure_builtin(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedLabel(t, "lbl_nt", "repo_a", "needs-triage")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)
	handler := f.server().Handler()

	rr := doJSON(t, handler, "POST", "/agent/v1/labels", token,
		`{"name":"ready-for-agent","description":"fully specified"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("ensure new: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var created labelResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Name != "ready-for-agent" || created.Color != store.LabelDefaultColor {
		t.Errorf("created = %+v, want the default color", created)
	}

	// Idempotent retry: same 200, the existing label untouched.
	rr = doJSON(t, handler, "POST", "/agent/v1/labels", token, `{"name":"ready-for-agent","color":"#ff0000"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("re-ensure: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var again labelResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &again)
	if again.Color != store.LabelDefaultColor || again.Description != "fully specified" {
		t.Errorf("re-ensured = %+v, want the original untouched", again)
	}

	rr = doJSON(t, handler, "POST", "/agent/v1/labels", token, `{"name":"  "}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "name is required") {
		t.Fatalf("blank name: status = %d, body %q", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, handler, "GET", "/agent/v1/labels", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("label list: status = %d", rr.Code)
	}
	var list struct {
		Labels []labelResponse `json:"labels"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Labels) != 2 || list.Labels[0].Name != "needs-triage" || list.Labels[1].Name != "ready-for-agent" {
		t.Errorf("labels = %+v, want [needs-triage ready-for-agent] (name order)", list.Labels)
	}
}

// TestForgeTriagePassthrough pins binding symmetry (one labctl surface, both
// bindings): on a forge-bound repo every triage op forwards through the
// tracker seam with the caller's names — never ids, never rewritten.
func TestForgeTriagePassthrough(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)

	fk := &fakeTracker{labels: []tracker.Label{{Name: "bug", Color: "#ee0701"}}}
	handler := f.forgeServer(fk).Handler()

	rr := doJSON(t, handler, "POST", "/agent/v1/issues", token,
		`{"title":"t","body":"b","labels":["bug","kind/feature"]}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue create: status = %d, body %s", rr.Code, rr.Body.String())
	}
	if fk.createdIssue == nil || fk.createdIssue.title != "t" ||
		len(fk.createdIssue.labels) != 2 || fk.createdIssue.labels[1] != "kind/feature" {
		t.Errorf("forwarded create = %+v", fk.createdIssue)
	}
	if !strings.Contains(rr.Body.String(), `"number":101`) {
		t.Errorf("create response = %s, want the tracker's issue", rr.Body.String())
	}

	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/labels", token, `{"labels":["bug"]}`)
	if rr.Code != http.StatusOK || len(fk.labelAdds) != 1 || fk.labelAdds[0].number != 7 || fk.labelAdds[0].labels[0] != "bug" {
		t.Fatalf("label add: status = %d, forwarded %+v", rr.Code, fk.labelAdds)
	}
	rr = doJSON(t, handler, "DELETE", "/agent/v1/issues/7/labels", token, `{"labels":["bug"]}`)
	if rr.Code != http.StatusOK || len(fk.labelRemoves) != 1 || fk.labelRemoves[0].number != 7 {
		t.Fatalf("label remove: status = %d, forwarded %+v", rr.Code, fk.labelRemoves)
	}
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/close", token, "")
	if rr.Code != http.StatusOK || len(fk.closed) != 1 || fk.closed[0] != 7 {
		t.Fatalf("close: status = %d, forwarded %+v", rr.Code, fk.closed)
	}
	rr = doJSON(t, handler, "POST", "/agent/v1/labels", token, `{"name":"bug","color":"#ee0701"}`)
	if rr.Code != http.StatusOK || len(fk.ensured) != 1 || fk.ensured[0].Name != "bug" {
		t.Fatalf("ensure: status = %d, forwarded %+v", rr.Code, fk.ensured)
	}
	rr = doJSON(t, handler, "GET", "/agent/v1/labels", token, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"#ee0701"`) {
		t.Fatalf("label list: status = %d, body %s", rr.Code, rr.Body.String())
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

// TestPRCreateBuiltinChangeRequest pins the M6 seam flip: a builtin-bound PR
// create opens a change request through the store-backed tracker — the run's
// branch as head, the repo default branch as base, the AFK-injected
// `Closes #<N>` persisted as a cr_closes row via the shared parser, the URL
// the lab-relative CR route — and publishes cr.changed on the bus.
func TestPRCreateBuiltinChangeRequest(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix it", "")
	f.seedRun(t, "run_afk", "repo_a", "active") // afk_auto, issue 7, branch afk/7
	token := f.seedToken(t, "run_afk", nil)

	bus := events.NewBus()
	ch, cancel := bus.Subscribe(context.Background())
	defer cancel()
	s := New(f.st, builtinResolver(f.st), bus, discard(), func() time.Time { return f.now }, nil)

	// The body carries no Closes — the server injects "Closes #7" (pinned
	// AFK rule), and THAT injected directive must reach cr_closes.
	rr := doJSON(t, s.Handler(), "POST", "/agent/v1/prs", token, `{"title":"feat: fix it","body":"Implements the thing."}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Number != 1 {
		t.Errorf("number = %d, want 1 (first CR of the repo)", resp.Number)
	}
	if resp.URL != "/repos/repo_a/crs/1" {
		t.Errorf("url = %q, want the lab-relative CR route", resp.URL)
	}

	cr, err := f.st.CRByRepoNumber(context.Background(), "repo_a", 1)
	if err != nil {
		t.Fatalf("CRByRepoNumber: %v", err)
	}
	if cr.State != store.CRStateOpen || cr.HeadBranch != "afk/7" || cr.BaseBranch != "main" {
		t.Errorf("cr = state %q head %q base %q, want open afk/7 main", cr.State, cr.HeadBranch, cr.BaseBranch)
	}
	if cr.Title != "feat: fix it" {
		t.Errorf("title = %q", cr.Title)
	}
	if !strings.Contains(cr.Body, "Closes #7") {
		t.Errorf("body = %q, want the injected Closes #7", cr.Body)
	}
	if len(cr.Closes) != 1 || cr.Closes[0] != 7 {
		t.Errorf("closes = %v, want [7] (injection must flow into cr_closes)", cr.Closes)
	}

	select {
	case e := <-ch:
		if e.Type != EventCRChanged {
			t.Errorf("event type = %q, want %q", e.Type, EventCRChanged)
		}
	default:
		t.Error("no cr.changed published on builtin PR create")
	}
}

// TestPRViewAndList_forge pins the agent PR read surface (issue #8) on a
// forge-bound repo: GET /prs/{n} answers the full detail including the BODY —
// the captured-card-YAML retrieval path — GET /prs lists the repo's PRs
// across states, and an unknown number is the canonical 404 envelope.
// TestPRMerge_forge drives POST /agent/v1/prs/{n}/merge on a forge-bound
// repo: success reports the merged state; a forge refusal surfaces verbatim as
// a 409; an unknown number is the canonical 404 envelope.
func TestPRMerge_forge(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_m", "forge", "forgejo")
	f.seedRunKind(t, "run_m", "repo_m", "afk_auto", "active", intp(9), "afk/9")
	token := f.seedToken(t, "run_m", nil)

	// Success: the forge merged; the response reports the merged state and the
	// handler passed the path number through to MergePull.
	fk := &fakeTracker{mergeRef: tracker.PullRef{
		Number: 9, HeadBranch: "afk/9", State: tracker.PullMerged,
		URL: "https://git.example.com/o/r/pulls/9",
	}}
	rr := doJSON(t, f.forgeServer(fk).Handler(), "POST", "/agent/v1/prs/9/merge", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("merge: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var got prMergeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := prMergeResponse{Number: 9, State: "merged", Head: "afk/9", URL: "https://git.example.com/o/r/pulls/9"}
	if got != want {
		t.Errorf("merge response = %+v, want %+v", got, want)
	}
	if len(fk.merged) != 1 || fk.merged[0] != 9 {
		t.Errorf("MergePull args = %v, want [9]", fk.merged)
	}

	// Rejection: the backend's own words surface verbatim as a 409.
	reject := &fakeTracker{err: fmt.Errorf("%w: required status check \"ci\" is expected", tracker.ErrMergeRejected)}
	rr = doJSON(t, f.forgeServer(reject).Handler(), "POST", "/agent/v1/prs/9/merge", token, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("rejected merge: status = %d, want 409 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "required status check") {
		t.Errorf("409 body = %s, want the backend's own words", rr.Body.String())
	}

	// Unknown number → the canonical 404 envelope.
	notFound := &fakeTracker{err: fmt.Errorf("pull 999: %w", tracker.ErrNotFound)}
	rr = doJSON(t, f.forgeServer(notFound).Handler(), "POST", "/agent/v1/prs/999/merge", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown merge: status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
}

func TestPRViewAndList_forge(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)

	fk := &fakeTracker{
		pullDetails: []tracker.PullDetail{{
			Number: 12, Title: "feat: capture card", Body: "card: |\n  kind: capture",
			State: tracker.PullOpen, HeadBranch: "afk/7",
			URL: "https://git.example.com/o/r/pulls/12",
		}},
		pulls: []tracker.PullRef{
			{Number: 12, HeadBranch: "afk/7", State: tracker.PullOpen, URL: "https://git.example.com/o/r/pulls/12"},
			{Number: 3, HeadBranch: "afk/2", State: tracker.PullMerged, URL: "https://git.example.com/o/r/pulls/3"},
		},
	}
	handler := f.forgeServer(fk).Handler()

	rr := doJSON(t, handler, "GET", "/agent/v1/prs/12", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /prs/12: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var got prDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := prDetailResponse{
		Number: 12, Title: "feat: capture card", Body: "card: |\n  kind: capture",
		State: "open", Head: "afk/7", URL: "https://git.example.com/o/r/pulls/12",
	}
	if got != want {
		t.Errorf("PR detail = %+v, want %+v", got, want)
	}

	rr = doJSON(t, handler, "GET", "/agent/v1/prs", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /prs: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var list struct {
		PRs []prListItem `json:"prs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	wantList := []prListItem{
		{Number: 12, State: "open", Head: "afk/7", URL: "https://git.example.com/o/r/pulls/12"},
		{Number: 3, State: "merged", Head: "afk/2", URL: "https://git.example.com/o/r/pulls/3"},
	}
	if len(list.PRs) != len(wantList) {
		t.Fatalf("PR list = %+v, want %+v", list.PRs, wantList)
	}
	for i := range wantList {
		if list.PRs[i] != wantList[i] {
			t.Errorf("PR list[%d] = %+v, want %+v", i, list.PRs[i], wantList[i])
		}
	}

	// Unknown number → the canonical 404 envelope (the fake wraps ErrNotFound
	// like the real client's upstream 404); a non-numeric segment names no PR.
	for _, path := range []string{"/agent/v1/prs/999", "/agent/v1/prs/abc"} {
		rr = doJSON(t, handler, "GET", path, token, "")
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), `"error"`) {
			t.Errorf("GET %s: status = %d, body %q, want 404 envelope", path, rr.Code, rr.Body.String())
		}
	}
}

// TestPRViewAndList_builtin runs the same read surface end-to-end on a
// builtin-bound repo: the CR a run created comes back in full through
// GET /prs/{n} — injected Closes and all — and through the list.
func TestPRViewAndList_builtin(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix it", "")
	f.seedRun(t, "run_afk", "repo_a", "active") // afk_auto, issue 7, branch afk/7
	token := f.seedToken(t, "run_afk", nil)

	s := New(f.st, builtinResolver(f.st), nil, discard(), func() time.Time { return f.now }, nil)
	handler := s.Handler()

	rr := doJSON(t, handler, "POST", "/agent/v1/prs", token, `{"title":"feat: capture card","body":"card: |\n  kind: capture"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /prs: status = %d, body %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, handler, "GET", "/agent/v1/prs/1", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /prs/1: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var got prDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Number != 1 || got.Title != "feat: capture card" || got.State != "open" ||
		got.Head != "afk/7" || got.URL != "/repos/repo_a/crs/1" {
		t.Errorf("CR detail = %+v", got)
	}
	if !strings.Contains(got.Body, "card: |\n  kind: capture") || !strings.Contains(got.Body, "Closes #7") {
		t.Errorf("CR body = %q, want the card YAML and the injected Closes #7", got.Body)
	}

	rr = doJSON(t, handler, "GET", "/agent/v1/prs", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /prs: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var list struct {
		PRs []prListItem `json:"prs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.PRs) != 1 || list.PRs[0].Number != 1 || list.PRs[0].Head != "afk/7" {
		t.Errorf("PR list = %+v, want the one created CR", list.PRs)
	}

	// Unknown number → 404 (builtin surfaces store.ErrNotFound).
	rr = doJSON(t, handler, "GET", "/agent/v1/prs/999", token, "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /prs/999: status = %d, want 404", rr.Code)
	}
}

// TestPRChecks_forge pins GET /agent/v1/prs/{n}/checks (issue #72): the rows
// come back verbatim and the aggregate is computed SERVER-SIDE via the pure
// tracker.ChecksState. The fixture rows carry success + pending + failure, so
// a naive "any pending" client would say pending — the body proving `failure`
// proves precedence ran on the server. The full JSON body is asserted,
// including the camelCase `rawState` key. An unknown number is the canonical
// 404 envelope; a throttled forge is a distinct 503.
func TestPRChecks_forge(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)

	// success + pending + failure → aggregate failure (precedence over a live
	// pending sibling), proving the server did not just forward a forge verdict.
	fk := &fakeTracker{checks: []tracker.Check{
		{Name: "build", State: tracker.CheckSuccess, RawState: "success", Summary: "ok", URL: "https://ci/build"},
		{Name: "lint", State: tracker.CheckPending, RawState: "in_progress", Summary: "", URL: "https://ci/lint"},
		{Name: "test", State: tracker.CheckFailure, RawState: "failure", Summary: "3 failing", URL: "https://ci/test"},
	}}
	rr := doJSON(t, f.forgeServer(fk).Handler(), "GET", "/agent/v1/prs/12/checks", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("checks: status = %d, body %s", rr.Code, rr.Body.String())
	}
	wantBody := `{"state":"failure","checks":[` +
		`{"name":"build","state":"success","rawState":"success","summary":"ok","url":"https://ci/build"},` +
		`{"name":"lint","state":"pending","rawState":"in_progress","summary":"","url":"https://ci/lint"},` +
		`{"name":"test","state":"failure","rawState":"failure","summary":"3 failing","url":"https://ci/test"}]}`
	if got := strings.TrimSpace(rr.Body.String()); got != wantBody {
		t.Errorf("checks body =\n%s\nwant\n%s", got, wantBody)
	}

	// Zero rows: state none and the array is literal [], never null (an agent
	// parsing the array must not special-case a nil).
	empty := &fakeTracker{checks: []tracker.Check{}}
	rr = doJSON(t, f.forgeServer(empty).Handler(), "GET", "/agent/v1/prs/12/checks", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("empty checks: status = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"state":"none","checks":[]}` {
		t.Errorf("empty checks body = %s, want {\"state\":\"none\",\"checks\":[]}", got)
	}

	// Unknown number → the canonical 404 envelope (the fake wraps ErrNotFound
	// like the real client's upstream 404).
	notFound := &fakeTracker{err: fmt.Errorf("pull 999: %w", tracker.ErrNotFound)}
	rr = doJSON(t, f.forgeServer(notFound).Handler(), "GET", "/agent/v1/prs/999/checks", token, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown checks: status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}

	// Throttled forge → 503 with the diagnostic (ErrRateLimited, not the
	// generic 502): a distinct state the agent comes back later on.
	limited := &fakeTracker{err: fmt.Errorf("github checks: %w (resets 12:30)", tracker.ErrRateLimited)}
	rr = doJSON(t, f.forgeServer(limited).Handler(), "GET", "/agent/v1/prs/12/checks", token, "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("rate-limited checks: status = %d, want 503 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "rate limited") {
		t.Errorf("503 body = %s, want the rate-limit diagnostic", rr.Body.String())
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
	}), nil, discard(), func() time.Time { return f.now }, nil)
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
