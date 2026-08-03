package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

const (
	testToken = "SECRET-forge-tok-abc123" // distinctive so error tests can assert it never leaks
	apiPrefix = "/api/v1/repos/Cloonar/nixos"
)

// newTestClient spins up an httptest server with h and returns a Client bound
// to Cloonar/nixos against it.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.Client(), srv.URL+"/api/v1", testToken, "Cloonar", "nixos")
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode request body %q: %v", string(b), err)
	}
	return m
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

// --- ReadyIssues -----------------------------------------------------------

func TestReadyIssues(t *testing.T) {
	var gotQuery url.Values
	var gotAuth, gotPath, gotRawURL string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
			return
		}
		gotQuery = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotRawURL = r.URL.String()
		_, _ = io.WriteString(w, `[
		  {"number":62,"title":"first","body":"body one","state":"open",
		   "labels":[{"name":"ready-for-agent"},{"name":"bug"}],
		   "created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-03T04:05:06Z"},
		  {"number":7,"title":"second","state":"open",
		   "labels":[{"name":"ready-for-agent"}],
		   "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
		]`)
	})

	issues, err := c.ReadyIssues(context.Background())
	if err != nil {
		t.Fatalf("ReadyIssues: %v", err)
	}

	// Request shape: type=issues excludes PRs; state=open + labels=ready-for-agent
	// filter the queue server-side; token in the header, never the URL.
	if gotPath != apiPrefix+"/issues" {
		t.Errorf("path = %q; want %q", gotPath, apiPrefix+"/issues")
	}
	if gotAuth != "token "+testToken {
		t.Errorf("Authorization = %q; want %q", gotAuth, "token "+testToken)
	}
	if strings.Contains(gotRawURL, testToken) {
		t.Errorf("token leaked into request URL %q", gotRawURL)
	}
	for k, want := range map[string]string{
		"type":   "issues",
		"state":  "open",
		"labels": "ready-for-agent",
	} {
		if got := gotQuery.Get(k); got != want {
			t.Errorf("query %s = %q; want %q", k, got, want)
		}
	}

	if len(issues) != 2 {
		t.Fatalf("got %d issues; want 2", len(issues))
	}
	i0 := issues[0]
	if i0.Number != 62 || i0.Title != "first" || i0.Body != "body one" || i0.State != "open" {
		t.Errorf("issue[0] scalar mismatch: %+v", i0)
	}
	if len(i0.Labels) != 2 || i0.Labels[0] != "ready-for-agent" || i0.Labels[1] != "bug" {
		t.Errorf("issue[0].Labels = %v; want [ready-for-agent bug]", i0.Labels)
	}
	if !i0.CreatedAt.Equal(mustTime(t, "2026-01-02T03:04:05Z")) ||
		!i0.UpdatedAt.Equal(mustTime(t, "2026-01-03T04:05:06Z")) {
		t.Errorf("issue[0] timestamps: created=%s updated=%s", i0.CreatedAt, i0.UpdatedAt)
	}
	// List view loads no comments.
	if i0.Comments != nil {
		t.Errorf("issue[0].Comments = %v; want nil (list view)", i0.Comments)
	}
	if issues[1].Number != 7 || len(issues[1].Labels) != 1 || issues[1].Comments != nil {
		t.Errorf("issue[1] mismatch: %+v", issues[1])
	}
}

// TestReadyIssues_dropsIssuesWithoutReadyLabel is the regression for
// Forgejo's "non existent labels are discarded" semantics: until the
// ready-for-agent label exists on the forge repo, the server ignores the
// labels= filter entirely and returns EVERY open issue. The client must
// re-check membership so no unlabeled issue ever reaches the ready queue
// (and, in M5, an AFK agent).
func TestReadyIssues_dropsIssuesWithoutReadyLabel(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
			return
		}
		// Simulates the discarded-label response: open issues with no labels,
		// with unrelated labels, and one genuinely ready.
		_, _ = io.WriteString(w, `[
		  {"number":1,"title":"unlabeled","state":"open",
		   "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},
		  {"number":2,"title":"labeled bug","state":"open","labels":[{"name":"bug"}],
		   "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},
		  {"number":3,"title":"actually ready","state":"open",
		   "labels":[{"name":"bug"},{"name":"ready-for-agent"}],
		   "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
		]`)
	})

	issues, err := c.ReadyIssues(context.Background())
	if err != nil {
		t.Fatalf("ReadyIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 3 {
		t.Fatalf("ready queue = %+v; want only issue #3 (the one carrying %q)", issues, tracker.ReadyLabel)
	}
}

// A queue that post-filters down to nothing is still a non-nil empty slice.
func TestReadyIssues_emptyAfterFilterIsNonNil(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, `[{"number":1,"title":"unlabeled","state":"open",
		  "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`)
	})
	issues, err := c.ReadyIssues(context.Background())
	if err != nil {
		t.Fatalf("ReadyIssues: %v", err)
	}
	if issues == nil || len(issues) != 0 {
		t.Fatalf("filtered-empty queue = %#v; want non-nil empty slice", issues)
	}
}

// --- Issues(state) ---------------------------------------------------------

// issueRows renders n sequential fjIssue rows in the given state, numbered
// downward from start (the newest-first order Forgejo lists in), as one JSON
// page — the bulk generator behind the request-count pins.
func issueRows(start, n int, state string) string {
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		num := start - i
		rows = append(rows, fmt.Sprintf(
			`{"number":%d,"title":"t%d","state":%q,"created_at":"2026-02-02T00:00:00Z","updated_at":"2026-02-02T00:00:00Z"}`,
			num, num, state))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// TestIssues_openWalkUnchanged pins the open view's shape after issue #176:
// still the full fetchPages walk (state=open, type=issues, paginate until
// empty) — the open set is bounded by the working set, so it did not need
// the window.
func TestIssues_openWalkUnchanged(t *testing.T) {
	var gotState, gotType string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
			return
		}
		gotState = r.URL.Query().Get("state")
		gotType = r.URL.Query().Get("type")
		_, _ = io.WriteString(w, `[{"number":3,"title":"x","state":"open",
		  "created_at":"2026-02-02T00:00:00Z","updated_at":"2026-02-02T00:00:00Z"}]`)
	})
	issues, err := c.Issues(context.Background(), "open")
	if err != nil {
		t.Fatalf("Issues(open): %v", err)
	}
	if gotState != "open" {
		t.Errorf("state param = %q; want open", gotState)
	}
	if gotType != "issues" {
		t.Errorf("type param = %q; want issues (excludes PRs)", gotType)
	}
	if len(issues) != 1 || issues[0].Number != 3 || issues[0].Comments != nil {
		t.Errorf("mapping mismatch: %+v", issues)
	}
}

// TestIssues_closedIsOneRecentUpdateWindow pins the closed view's request
// math (issue #176): exactly ONE request — type=issues&state=closed&
// sort=recentupdate&page=1&limit=50 — and NO page=2 even though the window
// comes back completely full: a full window is a full window, not an
// invitation to paginate. (sort=recentupdate because Forgejo's issue sort
// enum has no recentclose; closing bumps updated_at.)
func TestIssues_closedIsOneRecentUpdateWindow(t *testing.T) {
	var requests int
	var gotQuery url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotQuery = r.URL.Query()
		// A FULL window page: 50 rows, the strongest bait for a page=2 probe.
		_, _ = io.WriteString(w, issueRows(500, tracker.RecentClosedWindow, "closed"))
	})
	issues, err := c.Issues(context.Background(), "closed")
	if err != nil {
		t.Fatalf("Issues(closed): %v", err)
	}
	if requests != 1 {
		t.Errorf("closed view made %d requests; want exactly 1 (never a page=2 probe)", requests)
	}
	for k, want := range map[string]string{
		"type":  "issues",
		"state": "closed",
		"sort":  "recentupdate",
		"page":  "1",
		"limit": strconv.Itoa(tracker.RecentClosedWindow),
	} {
		if got := gotQuery.Get(k); got != want {
			t.Errorf("closed query %s = %q; want %q", k, got, want)
		}
	}
	if len(issues) != tracker.RecentClosedWindow {
		t.Errorf("got %d issues; want the %d-row window", len(issues), tracker.RecentClosedWindow)
	}
}

// TestIssues_allIsOpenWalkPlusClosedWindow pins the all view (issue #176):
// the open walk (1 page + 1 empty probe here) plus the one-request closed
// window = 3 requests, open rows first.
func TestIssues_allIsOpenWalkPlusClosedWindow(t *testing.T) {
	var requests, closedRequests int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Query().Get("state") {
		case "open":
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
				return
			}
			_, _ = io.WriteString(w, issueRows(7, 1, "open"))
		case "closed":
			closedRequests++
			if got := r.URL.Query().Get("sort"); got != "recentupdate" {
				t.Errorf("closed sort = %q; want recentupdate", got)
			}
			_, _ = io.WriteString(w, issueRows(5, 2, "closed"))
		default:
			t.Errorf("unexpected state param %q", r.URL.Query().Get("state"))
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	issues, err := c.Issues(context.Background(), "all")
	if err != nil {
		t.Fatalf("Issues(all): %v", err)
	}
	if requests != 3 || closedRequests != 1 {
		t.Errorf("all view = %d requests (%d closed); want 3 total, 1 closed", requests, closedRequests)
	}
	wantNumbers := []int{7, 5, 4} // open first, then the window newest-first
	if len(issues) != len(wantNumbers) {
		t.Fatalf("got %d issues (%+v); want %d", len(issues), issues, len(wantNumbers))
	}
	for i, n := range wantNumbers {
		if issues[i].Number != n {
			t.Errorf("issues[%d].Number = %d; want %d (open set first, then the closed window)", i, issues[i].Number, n)
		}
	}
}

// An unrecognized state filter fails loud instead of silently misquerying —
// the filter fans out into distinct reads since issue #176.
func TestIssues_invalidStateIsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL)
	})
	if _, err := c.Issues(context.Background(), "bogus"); err == nil {
		t.Fatal("Issues(bogus) err = nil; want the invalid-filter error")
	}
}

func TestIssues_emptyListIsNonNil(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	issues, err := c.Issues(context.Background(), "all")
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if issues == nil {
		t.Fatal("empty result is nil; contract wants a non-nil empty slice")
	}
	if len(issues) != 0 {
		t.Fatalf("len = %d; want 0", len(issues))
	}
}

// --- Issue(n) with comments ------------------------------------------------

func TestIssue_withComments(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == apiPrefix+"/issues/62" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"number":62,"title":"deep","body":"the body","state":"open",
			  "labels":[{"name":"bug"}],
			  "created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-02T11:00:00Z"}`)
		case r.URL.Path == apiPrefix+"/issues/62/comments" && r.Method == http.MethodGet:
			// Two comments: author from user.login, and a fallback to user.username
			// when login is empty.
			_, _ = io.WriteString(w, `[
			  {"body":"first comment","user":{"login":"alice","username":"alice"},"created_at":"2026-03-01T12:00:00Z"},
			  {"body":"second comment","user":{"login":"","username":"bob"},"created_at":"2026-03-01T13:00:00Z"}
			]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	issue, err := c.Issue(context.Background(), 62)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issue.Number != 62 || issue.Title != "deep" || issue.Body != "the body" || issue.State != "open" {
		t.Errorf("issue scalars mismatch: %+v", issue)
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != "bug" {
		t.Errorf("labels = %v; want [bug]", issue.Labels)
	}
	if len(issue.Comments) != 2 {
		t.Fatalf("got %d comments; want 2", len(issue.Comments))
	}
	if issue.Comments[0].Author != "alice" || issue.Comments[0].Body != "first comment" ||
		!issue.Comments[0].CreatedAt.Equal(mustTime(t, "2026-03-01T12:00:00Z")) {
		t.Errorf("comment[0] mismatch: %+v", issue.Comments[0])
	}
	// login empty → fall back to username.
	if issue.Comments[1].Author != "bob" {
		t.Errorf("comment[1].Author = %q; want bob (username fallback)", issue.Comments[1].Author)
	}
}

func genCommentsJSON(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"body":"c%d","user":{"login":"alice"},"created_at":"2026-03-01T12:00:00Z"}`, i+1)
	}
	b.WriteByte(']')
	return b.String()
}

// TestIssue_manyCommentsFetchedInOneUnpaginatedGET is the regression for the
// comments infinite loop: Forgejo's per-issue comments endpoint accepts only
// since/before — it IGNORES page/limit and always returns the full list. The
// fake mirrors that (same full list on every request, whatever the params);
// paging over it would spin forever once an issue has >= pageLimit comments,
// so the client must issue exactly one un-paginated GET.
func TestIssue_manyCommentsFetchedInOneUnpaginatedGET(t *testing.T) {
	const n = pageLimit + 9
	var commentReqs int
	var gotCommentQuery url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/issues/62":
			_, _ = io.WriteString(w, `{"number":62,"title":"chatty","state":"open",
			  "created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-02T11:00:00Z"}`)
		case apiPrefix + "/issues/62/comments":
			commentReqs++
			if commentReqs > 3 {
				// Break a would-be infinite loop so a regression fails fast
				// instead of hanging the suite.
				t.Errorf("comments endpoint requested %d times; want exactly 1", commentReqs)
				_, _ = io.WriteString(w, `[]`)
				return
			}
			gotCommentQuery = r.URL.Query()
			_, _ = io.WriteString(w, genCommentsJSON(n)) // full list, regardless of any page/limit params
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	issue, err := c.Issue(context.Background(), 62)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if commentReqs != 1 {
		t.Errorf("comments endpoint requested %d times; want exactly 1 (un-paginated GET)", commentReqs)
	}
	if gotCommentQuery.Get("page") != "" || gotCommentQuery.Get("limit") != "" {
		t.Errorf("comments request carried pagination params %v; the endpoint ignores them", gotCommentQuery)
	}
	if len(issue.Comments) != n {
		t.Fatalf("got %d comments; want all %d", len(issue.Comments), n)
	}
	if issue.Comments[0].Body != "c1" || issue.Comments[n-1].Body != fmt.Sprintf("c%d", n) {
		t.Errorf("comment order lost: first=%q last=%q", issue.Comments[0].Body, issue.Comments[n-1].Body)
	}
}

// --- CreateComment ---------------------------------------------------------

func TestCreateComment(t *testing.T) {
	var gotMethod, gotPath, gotCT string
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		gotBody = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"body":"hi there","user":{"login":"operator"},"created_at":"2026-04-01T00:00:00Z"}`)
	})

	if err := c.CreateComment(context.Background(), 62, "hi there"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotPath != apiPrefix+"/issues/62/comments" {
		t.Errorf("path = %q; want %q", gotPath, apiPrefix+"/issues/62/comments")
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", gotCT)
	}
	if gotBody["body"] != "hi there" {
		t.Errorf("request body = %v; want {body: hi there}", gotBody)
	}
}

// --- Pulls -----------------------------------------------------------------

// pullRows renders n sequential fjPull rows in the given state (closed rows
// unmerged), numbered downward from start, as one JSON page — the bulk
// generator behind the request-count pins.
func pullRows(start, n int, state string) string {
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		num := start - i
		rows = append(rows, fmt.Sprintf(
			`{"number":%d,"state":%q,"merged":false,"head":{"ref":"afk/%d"},"html_url":"u%d"}`,
			num, state, num, num))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// TestPulls_openPlusRecentClosedWindow pins the bounded contract (issue
// #176): the open walk concatenated with the one-page recent-closed window
// (state=closed&sort=recentclose), open rows first, the three-valued state
// still derived from state+merged per row — a merged pull in the window maps
// to "merged". #9 and #80 share head afk/7 across the two sets; the client
// returns BOTH (open-beats-closed precedence when heads collide is the
// tracker package's pure PullState fn, not this client's job).
func TestPulls_openPlusRecentClosedWindow(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("state") {
		case "open":
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
				return
			}
			_, _ = io.WriteString(w, `[
			  {"number":72,"state":"open","merged":false,"head":{"ref":"afk/63"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/72"},
			  {"number":80,"state":"open","merged":false,"head":{"ref":"afk/7"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/80"}
			]`)
		case "closed":
			if got := r.URL.Query().Get("sort"); got != "recentclose" {
				t.Errorf("closed sort = %q; want recentclose", got)
			}
			_, _ = io.WriteString(w, `[
			  {"number":40,"state":"closed","merged":true,"head":{"ref":"afk/12"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/40"},
			  {"number":9,"state":"closed","merged":false,"head":{"ref":"afk/7"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/9"}
			]`)
		default:
			t.Errorf("unexpected state param %q (state=all is the walk #176 removed)", r.URL.Query().Get("state"))
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	pulls, err := c.Pulls(context.Background())
	if err != nil {
		t.Fatalf("Pulls: %v", err)
	}
	want := []tracker.PullRef{
		{Number: 72, HeadBranch: "afk/63", State: "open", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/72"},
		{Number: 80, HeadBranch: "afk/7", State: "open", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/80"},
		{Number: 40, HeadBranch: "afk/12", State: "merged", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/40"},
		{Number: 9, HeadBranch: "afk/7", State: "closed", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/9"},
	}
	if len(pulls) != len(want) {
		t.Fatalf("got %d pulls; want %d", len(pulls), len(want))
	}
	for i := range want {
		if pulls[i] != want[i] {
			t.Errorf("pull[%d] = %+v; want %+v", i, pulls[i], want[i])
		}
	}
}

// TestPulls_requestCountBounded is the request math of issue #176 on a repo
// with 60 open and arbitrarily many closed PRs: 2 open pages + 1 empty probe
// + 1 closed page = exactly 4 requests, independent of how deep the closed
// history goes. The closed page carries state=closed&sort=recentclose&
// page=1&limit=50 and is NEVER followed by a page=2 — even though it comes
// back completely full.
func TestPulls_requestCountBounded(t *testing.T) {
	var requests, closedRequests int
	var closedQuery url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		switch q.Get("state") {
		case "open":
			switch q.Get("page") {
			case "1":
				_, _ = io.WriteString(w, pullRows(60, 50, "open"))
			case "2":
				_, _ = io.WriteString(w, pullRows(10, 10, "open"))
			default:
				_, _ = io.WriteString(w, `[]`) // the one empty-page probe
			}
		case "closed":
			closedRequests++
			closedQuery = q
			// A FULL window: 50 rows, the strongest bait for a page=2 probe.
			_, _ = io.WriteString(w, pullRows(1000, tracker.RecentClosedWindow, "closed"))
		default:
			t.Errorf("unexpected state param %q", q.Get("state"))
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	pulls, err := c.Pulls(context.Background())
	if err != nil {
		t.Fatalf("Pulls: %v", err)
	}
	if requests != 4 {
		t.Errorf("Pulls made %d requests; want exactly 4 (2 open pages + 1 probe + 1 closed page)", requests)
	}
	if closedRequests != 1 {
		t.Errorf("closed window fetched %d times; want exactly 1 (a full page is not followed by page=2)", closedRequests)
	}
	for k, want := range map[string]string{
		"state": "closed",
		"sort":  "recentclose",
		"page":  "1",
		"limit": strconv.Itoa(tracker.RecentClosedWindow),
	} {
		if got := closedQuery.Get(k); got != want {
			t.Errorf("closed query %s = %q; want %q", k, got, want)
		}
	}
	if wantLen := 60 + tracker.RecentClosedWindow; len(pulls) != wantLen {
		t.Errorf("got %d pulls; want %d (60 open + the %d-row window)", len(pulls), wantLen, tracker.RecentClosedWindow)
	}
}

// --- PullsForHead ----------------------------------------------------------

// pullsForHeadPath is the escaped by-base-head lookup the fast path must hit
// for head afk/9 onto main: the '/' in the branch name reaches the forge as
// %2F, one path segment per branch (verified against the live forge).
const pullsForHeadPath = apiPrefix + "/pulls/main/afk%2F9"

// TestPullsForHead_doneSignalConformance is the shared done-signal table (the
// same cases run against all three backends): PullsForHead only enumerates
// candidates, and tracker.DonePull projects the same verdict it would from a
// full Pulls() walk. The closed-unmerged case exercises the fallback listing;
// the different-base case is Forgejo's own 404 (the by-base-head lookup is
// base-scoped server-side, so a pull onto another base simply is not there).
func TestPullsForHead_doneSignalConformance(t *testing.T) {
	const head, base = "afk/9", "main"
	for _, tc := range []struct {
		name        string
		lookupCode  int    // by-base-head answer status
		lookupBody  string // by-base-head answer (when 200)
		listBody    string // state=all fallback page 1; "" ⇒ the walk must not run
		wantNumbers []int
		wantDone    bool
		wantDoneNum int
	}{
		{
			name:        "open pull is the done-signal",
			lookupCode:  200,
			lookupBody:  `{"number":80,"state":"open","merged":false,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/80"}`,
			wantNumbers: []int{80}, wantDone: true, wantDoneNum: 80,
		},
		{
			name:        "merged pull is the done-signal",
			lookupCode:  200,
			lookupBody:  `{"number":40,"state":"closed","merged":true,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/40"}`,
			wantNumbers: []int{40}, wantDone: true, wantDoneNum: 40,
		},
		{
			name:        "closed-unmerged only is no done-signal",
			lookupCode:  200,
			lookupBody:  `{"number":9,"state":"closed","merged":false,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/9"}`,
			listBody:    `[{"number":9,"state":"closed","merged":false,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/9"}]`,
			wantNumbers: []int{9}, wantDone: false,
		},
		{
			name:        "no pull at all is empty success",
			lookupCode:  404,
			wantNumbers: nil, wantDone: false,
		},
		{
			// A pull from afk/9 onto dev does not answer for base main: the
			// by-base-head lookup is base-scoped, so the forge 404s.
			name:        "pull onto a different base does not match",
			lookupCode:  404,
			wantNumbers: nil, wantDone: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.EscapedPath() == pullsForHeadPath:
					if tc.lookupCode != 200 {
						w.WriteHeader(tc.lookupCode)
						return
					}
					_, _ = io.WriteString(w, tc.lookupBody)
				case r.URL.Path == apiPrefix+"/pulls":
					if tc.listBody == "" {
						t.Errorf("fallback walk ran (%s), want the fast path only", r.URL)
					}
					if r.URL.Query().Get("state") != "all" {
						t.Errorf("fallback state = %q; want all", r.URL.Query().Get("state"))
					}
					if r.URL.Query().Get("page") != "1" {
						_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
						return
					}
					_, _ = io.WriteString(w, tc.listBody)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			})

			refs, err := c.PullsForHead(context.Background(), head, base)
			if err != nil {
				t.Fatalf("PullsForHead: %v", err)
			}
			if refs == nil {
				t.Fatalf("PullsForHead returned nil; want a non-nil slice (empty result is success)")
			}
			if len(refs) != len(tc.wantNumbers) {
				t.Fatalf("got %d refs (%+v); want %d", len(refs), refs, len(tc.wantNumbers))
			}
			for i, n := range tc.wantNumbers {
				if refs[i].Number != n {
					t.Errorf("refs[%d].Number = %d; want %d", i, refs[i].Number, n)
				}
				if refs[i].HeadBranch != head {
					t.Errorf("refs[%d].HeadBranch = %q; want the queried %q", i, refs[i].HeadBranch, head)
				}
			}
			done, ok := tracker.DonePull(refs, head)
			if ok != tc.wantDone {
				t.Fatalf("DonePull ok = %v; want %v", ok, tc.wantDone)
			}
			if ok && done.Number != tc.wantDoneNum {
				t.Errorf("DonePull = #%d; want #%d", done.Number, tc.wantDoneNum)
			}
		})
	}
}

// TestPullsForHead_fastPathIsOneEscapedRequest pins the bounded read (#176):
// an open answer costs exactly ONE forge request, and that request's path
// carries the branch names escaped as single segments (afk%2F9, never a bare
// afk/9 that would split the branch into two path segments).
func TestPullsForHead_fastPathIsOneEscapedRequest(t *testing.T) {
	var requests int
	var gotEscaped string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotEscaped = r.URL.EscapedPath()
		_, _ = io.WriteString(w, `{"number":80,"state":"open","merged":false,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/80"}`)
	})

	refs, err := c.PullsForHead(context.Background(), "afk/9", "main")
	if err != nil {
		t.Fatalf("PullsForHead: %v", err)
	}
	if requests != 1 {
		t.Errorf("fast path made %d requests; want exactly 1", requests)
	}
	if gotEscaped != pullsForHeadPath {
		t.Errorf("request path = %q; want %q", gotEscaped, pullsForHeadPath)
	}
	if len(refs) != 1 || refs[0].Number != 80 || refs[0].State != tracker.PullOpen {
		t.Errorf("refs = %+v; want [#80 open]", refs)
	}
}

// TestPullsForHead_syntheticHeadRefUsesQueriedBranch is the merged-and-branch-
// deleted regression: Forgejo renders such a pull's head.ref as a synthetic
// refs/pull/N/head (verified live: PR 178 for the deleted afk/175). The
// by-name query key is authoritative — the returned ref must carry the
// QUERIED branch, or the caller's DonePull match against the run branch
// silently fails on the exact pull that proves the run succeeded.
func TestPullsForHead_syntheticHeadRefUsesQueriedBranch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"number":178,"state":"closed","merged":true,"head":{"ref":"refs/pull/178/head"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/coding-lab/pulls/178"}`)
	})

	refs, err := c.PullsForHead(context.Background(), "afk/175", "main")
	if err != nil {
		t.Fatalf("PullsForHead: %v", err)
	}
	if len(refs) != 1 || refs[0].HeadBranch != "afk/175" || refs[0].State != tracker.PullMerged {
		t.Fatalf("refs = %+v; want [#178 merged HeadBranch afk/175]", refs)
	}
	if done, ok := tracker.DonePull(refs, "afk/175"); !ok || done.Number != 178 {
		t.Errorf("DonePull = (%+v, %v); want (#178, true)", done, ok)
	}
}

// TestPullsForHead_closedAnswerFallbackFindsOpenSibling pins the rare-case
// walk: the by-base-head lookup answers with NO ORDER BY, so a closed pull
// may shadow a live sibling on a re-used branch. A closed answer must trigger
// the state=all listing, matches filter on head AND base client-side, and the
// open sibling wins DonePull — never the shadowing closed one.
func TestPullsForHead_closedAnswerFallbackFindsOpenSibling(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.EscapedPath() == pullsForHeadPath:
			// The unordered lookup happened to return the closed pull.
			_, _ = io.WriteString(w, `{"number":9,"state":"closed","merged":false,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/9"}`)
		default:
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
				return
			}
			// #77 shares the head but targets dev; #72 shares the base but not
			// the head — both must be filtered out client-side.
			_, _ = io.WriteString(w, `[
			  {"number":9,"state":"closed","merged":false,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/9"},
			  {"number":80,"state":"open","merged":false,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/80"},
			  {"number":77,"state":"open","merged":false,"head":{"ref":"afk/9"},"base":{"ref":"dev"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/77"},
			  {"number":72,"state":"open","merged":false,"head":{"ref":"afk/63"},"base":{"ref":"main"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/72"}
			]`)
		}
	})

	refs, err := c.PullsForHead(context.Background(), "afk/9", "main")
	if err != nil {
		t.Fatalf("PullsForHead: %v", err)
	}
	if len(refs) != 2 || refs[0].Number != 9 || refs[1].Number != 80 {
		t.Fatalf("refs = %+v; want [#9 closed, #80 open]", refs)
	}
	if done, ok := tracker.DonePull(refs, "afk/9"); !ok || done.Number != 80 {
		t.Errorf("DonePull = (%+v, %v); want the live sibling (#80, true)", done, ok)
	}
}

// TestPullsForHead_serverErrorSurfaces: only a 404 means "no pull"; any other
// forge failure surfaces as an error with no partial data (seam convention).
func TestPullsForHead_serverErrorSurfaces(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	refs, err := c.PullsForHead(context.Background(), "afk/9", "main")
	if err == nil {
		t.Fatalf("PullsForHead on 500 = %+v, nil; want an error", refs)
	}
	if refs != nil {
		t.Errorf("refs = %+v; want nil alongside the error (no partial data)", refs)
	}
}

func TestDerivePullState(t *testing.T) {
	for _, tc := range []struct {
		state  string
		merged bool
		want   string
	}{
		{"open", false, "open"},
		{"closed", true, "merged"},  // merged afk PR = done-signal
		{"closed", false, "closed"}, // closed-unmerged = "no PR" to the reaper
		{"open", true, "merged"},    // defensive: merged wins even if state=open
	} {
		if got := derivePullState(tc.state, tc.merged); got != tc.want {
			t.Errorf("derivePullState(%q,%v) = %q; want %q", tc.state, tc.merged, got, tc.want)
		}
	}
}

// --- Pull ------------------------------------------------------------------

// TestPull pins the single-PR detail read (labctl pr view): one GET
// /pulls/{n}, the full body carried through verbatim (the captured-card-YAML
// case), and the same state+merged → three-valued derivation as Pulls.
func TestPull(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"number":72,"title":"feat: capture card",
		  "body":"card: |\n  kind: capture\n  target: nixos",
		  "state":"closed","merged":true,"head":{"ref":"afk/63"},
		  "html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/72"}`)
	})

	pd, err := c.Pull(context.Background(), 72)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != apiPrefix+"/pulls/72" {
		t.Errorf("request = %s %s; want GET %s/pulls/72", gotMethod, gotPath, apiPrefix)
	}
	if gotAuth != "token "+testToken {
		t.Errorf("Authorization = %q; want %q", gotAuth, "token "+testToken)
	}
	want := tracker.PullDetail{
		Number:     72,
		Title:      "feat: capture card",
		Body:       "card: |\n  kind: capture\n  target: nixos",
		State:      tracker.PullMerged, // state=closed + merged=true → merged
		HeadBranch: "afk/63",
		URL:        "https://git.cloonar.com/Cloonar/nixos/pulls/72",
	}
	if pd != want {
		t.Errorf("PullDetail = %+v; want %+v", pd, want)
	}
}

// TestPull_notFound pins the unknown-number path: Forgejo's 404 unwraps to
// tracker.ErrNotFound (the labctl error-envelope path), never leaking the token.
func TestPull_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"pull request does not exist"}`)
	})

	_, err := c.Pull(context.Background(), 999)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// --- Checks ------------------------------------------------------------

// TestChecks pins the two-call shape (GET the pull for head.sha, then GET the
// combined-status endpoint for that SHA) and the normalization table: every
// row not affirmatively "success" or "pending" collapses to CheckFailure,
// while RawState keeps the forge's own word verbatim.
//
// The fixture rows carry their state under the JSON key `status` — that is
// Forgejo's real wire shape (swagger CommitStatus.status); only the combined
// object's aggregate is keyed `state`. Regression: the decoder originally
// read `state` off the rows, so every live row decoded as "" and the
// unrecognized-state default turned an all-green run into an all-red one.
func TestChecks(t *testing.T) {
	const sha = "deadbeefcafe0123456789abcdef0123456789a"
	var gotPullPath, gotStatusPath string
	var gotStatusAuth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == apiPrefix+"/pulls/72" && r.Method == http.MethodGet:
			gotPullPath = r.URL.Path
			_, _ = fmt.Fprintf(w, `{"number":72,"title":"feat: thing","state":"open","merged":false,
			  "head":{"ref":"afk/63","sha":%q},
			  "html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/72"}`, sha)
		case r.URL.Path == apiPrefix+"/commits/"+sha+"/status" && r.Method == http.MethodGet:
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `{"state":"pending","statuses":[]}`) // paginate-until-empty probe
				return
			}
			gotStatusPath = r.URL.Path
			gotStatusAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"state":"pending","statuses":[
			  {"context":"ci/build","status":"success","description":"build ok","target_url":"https://ci/1"},
			  {"context":"ci/test","status":"pending","description":"running","target_url":"https://ci/2"},
			  {"context":"ci/lint","status":"failure","description":"lint failed","target_url":"https://ci/3"},
			  {"context":"ci/deploy","status":"error","description":"errored out","target_url":"https://ci/4"},
			  {"context":"ci/flaky","status":"warning","description":"flaky warn","target_url":"https://ci/5"},
			  {"context":"ci/weird","status":"totally-unknown","description":"???","target_url":"https://ci/6"}
			]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	checks, err := c.Checks(context.Background(), 72)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if gotPullPath != apiPrefix+"/pulls/72" {
		t.Errorf("pull request path = %q; want %q", gotPullPath, apiPrefix+"/pulls/72")
	}
	if gotStatusPath != apiPrefix+"/commits/"+sha+"/status" {
		t.Errorf("status request path = %q; want %q (must carry the head SHA)", gotStatusPath, apiPrefix+"/commits/"+sha+"/status")
	}
	if gotStatusAuth != "token "+testToken {
		t.Errorf("status request Authorization = %q; want %q", gotStatusAuth, "token "+testToken)
	}

	want := []tracker.Check{
		{Name: "ci/build", State: tracker.CheckSuccess, RawState: "success", Summary: "build ok", URL: "https://ci/1"},
		{Name: "ci/test", State: tracker.CheckPending, RawState: "pending", Summary: "running", URL: "https://ci/2"},
		{Name: "ci/lint", State: tracker.CheckFailure, RawState: "failure", Summary: "lint failed", URL: "https://ci/3"},
		{Name: "ci/deploy", State: tracker.CheckFailure, RawState: "error", Summary: "errored out", URL: "https://ci/4"},
		{Name: "ci/flaky", State: tracker.CheckFailure, RawState: "warning", Summary: "flaky warn", URL: "https://ci/5"},
		{Name: "ci/weird", State: tracker.CheckFailure, RawState: "totally-unknown", Summary: "???", URL: "https://ci/6"},
	}
	if len(checks) != len(want) {
		t.Fatalf("got %d checks; want %d: %+v", len(checks), len(want), checks)
	}
	for i := range want {
		if checks[i] != want[i] {
			t.Errorf("check[%d] = %+v; want %+v", i, checks[i], want[i])
		}
	}
}

// TestChecks_zeroStatuses is the load-bearing case of the whole feature: the
// combined endpoint answers a combined state of "pending" for a commit with
// zero registered statuses (there is no CI to be pending on), and that
// combined verdict must never be trusted. Checks must return a non-nil empty
// slice, and the aggregate — computed client-side by tracker.ChecksState,
// never read off the forge — must be ChecksNone, not CheckPending.
func TestChecks_zeroStatuses(t *testing.T) {
	const sha = "0000000000000000000000000000000000000a"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == apiPrefix+"/pulls/9" && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"number":9,"state":"open","merged":false,"head":{"ref":"afk/9","sha":%q}}`, sha)
		case r.URL.Path == apiPrefix+"/commits/"+sha+"/status" && r.Method == http.MethodGet:
			// The forge's own combined state says "pending" despite zero jobs.
			_, _ = io.WriteString(w, `{"state":"pending","statuses":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	checks, err := c.Checks(context.Background(), 9)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if checks == nil {
		t.Fatal("checks = nil; want a non-nil empty slice")
	}
	if len(checks) != 0 {
		t.Fatalf("checks = %+v; want empty", checks)
	}
	if got := tracker.ChecksState(checks); got != tracker.ChecksNone {
		t.Fatalf("ChecksState(checks) = %q; want %q — the forge's combined \"pending\" verdict must never be trusted", got, tracker.ChecksNone)
	}
}

// TestChecks_notFound mirrors TestPull_notFound: an unknown pull number fails
// on the first GET (the pull lookup) and unwraps to tracker.ErrNotFound,
// never leaking the token.
func TestChecks_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"pull request does not exist"}`)
	})

	_, err := c.Checks(context.Background(), 999)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// --- CheckLog --------------------------------------------------------------

const (
	checkLogSHA      = "abc123def4560000000000000000000000000000"
	checkLogJobRoute = "/Cloonar/nixos/actions/runs/329/jobs/1"
	checkLogContext  = "ci/build"
)

// checkLogHarness wires an httptest server serving pull 72 (head sha
// checkLogSHA), a single combined-status row for checkLogContext whose
// target_url the test picks via targetFn (given srv.URL, so the absolute-URL
// case can point back at the test server), and the per-attempt web log route
// — GET {job}/attempt/{k}/logs — delegated to logFn, which answers
// (status, contentType, body) for attempt k. It records every log route path
// requested and the Authorization header the log requests carried.
type checkLogHarness struct {
	client   *Client
	logAuth  string
	logPaths []string
}

func newCheckLogHarness(t *testing.T, targetFn func(srvURL string) string, logFn func(k int) (int, string, string)) *checkLogHarness {
	t.Helper()
	h := &checkLogHarness{}
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == apiPrefix+"/pulls/72" && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"number":72,"state":"open","merged":false,"head":{"ref":"afk/63","sha":%q}}`, checkLogSHA)
		case r.URL.Path == apiPrefix+"/commits/"+checkLogSHA+"/status" && r.Method == http.MethodGet:
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `{"state":"failure","statuses":[]}`) // paginate-until-empty probe
				return
			}
			_, _ = fmt.Fprintf(w, `{"state":"failure","statuses":[{"context":%q,"status":"failure","description":"d","target_url":%q}]}`,
				checkLogContext, target)
		case strings.HasPrefix(r.URL.Path, checkLogJobRoute+"/attempt/") && strings.HasSuffix(r.URL.Path, "/logs") && r.Method == http.MethodGet:
			h.logAuth = r.Header.Get("Authorization")
			h.logPaths = append(h.logPaths, r.URL.Path)
			status, ct, body := logFn(attemptOf(t, r.URL.Path))
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	target = targetFn(srv.URL)
	h.client = New(srv.Client(), srv.URL+"/api/v1", testToken, "Cloonar", "nixos")
	return h
}

// attemptOf extracts the k in {job}/attempt/{k}/logs.
func attemptOf(t *testing.T, path string) int {
	t.Helper()
	s := strings.TrimSuffix(strings.TrimPrefix(path, checkLogJobRoute+"/attempt/"), "/logs")
	k, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("cannot parse attempt from log path %q: %v", path, err)
	}
	return k
}

// TestCheckLog_happyPath pins the whole flow with an ABSOLUTE target_url: pull
// 72 → statuses → the one row's Actions job page → probe attempt 1 (200
// text/plain) then attempt 2 (404), returning attempt 1's body verbatim, with
// the log request carrying the same token auth the REST calls send.
func TestCheckLog_happyPath(t *testing.T) {
	h := newCheckLogHarness(t,
		func(srvURL string) string { return srvURL + checkLogJobRoute },
		func(k int) (int, string, string) {
			if k == 1 {
				return http.StatusOK, "text/plain; charset=utf-8", "attempt-1-log"
			}
			return http.StatusNotFound, "", "no such attempt"
		})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	if string(res.Log) != "attempt-1-log" {
		t.Errorf("log = %q; want %q", res.Log, "attempt-1-log")
	}
	if res.Attempt != 1 {
		t.Errorf("Attempt = %d; want 1", res.Attempt)
	}
	if res.FallbackFrom != 0 {
		t.Errorf("FallbackFrom = %d; want 0 (attempt 1 IS the latest)", res.FallbackFrom)
	}
	if h.logAuth != "token "+testToken {
		t.Errorf("log request Authorization = %q; want %q", h.logAuth, "token "+testToken)
	}
	want := []string{checkLogJobRoute + "/attempt/1/logs", checkLogJobRoute + "/attempt/2/logs"}
	if len(h.logPaths) != 2 || h.logPaths[0] != want[0] || h.logPaths[1] != want[1] {
		t.Errorf("log paths = %v; want %v", h.logPaths, want)
	}
}

// TestCheckLog_pathOnlyTargetURL: a path-only target_url (no scheme/host)
// resolves identically — only its .Path is ever used, rebuilt on the web
// origin.
func TestCheckLog_pathOnlyTargetURL(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(k int) (int, string, string) {
			if k == 1 {
				return http.StatusOK, "text/plain", "path-only-log"
			}
			return http.StatusNotFound, "", ""
		})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	if string(res.Log) != "path-only-log" {
		t.Errorf("log = %q; want %q", res.Log, "path-only-log")
	}
}

// TestCheckLog_rerunLatestAttemptWins: attempts 1..3 answer 200 with distinct
// bodies and attempt 4 is the terminating 404, so the newest attempt's log is
// returned — a rerun serves the latest attempt.
func TestCheckLog_rerunLatestAttemptWins(t *testing.T) {
	bodies := map[int]string{1: "first-run", 2: "second-run", 3: "third-run"}
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(k int) (int, string, string) {
			if b, ok := bodies[k]; ok {
				return http.StatusOK, "text/plain", b
			}
			return http.StatusNotFound, "", ""
		})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	if string(res.Log) != "third-run" {
		t.Errorf("log = %q; want the latest attempt %q", res.Log, "third-run")
	}
	if res.Attempt != 3 {
		t.Errorf("Attempt = %d; want 3", res.Attempt)
	}
	if len(h.logPaths) != 4 {
		t.Errorf("probed %d attempts; want 4 (three 200s then the terminating 404)", len(h.logPaths))
	}
}

// TestCheckLog_attemptOne404IsMismatch: a 404 on the VERY first attempt means
// the route the adapter is coupled to has moved — a loud, actionable mismatch,
// never an empty success.
func TestCheckLog_attemptOne404IsMismatch(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(int) (int, string, string) { return http.StatusNotFound, "", "not here" })

	_, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if !errors.Is(err, tracker.ErrLogAdapterMismatch) {
		t.Fatalf("err = %v; want ErrLogAdapterMismatch", err)
	}
	if !strings.Contains(err.Error(), "file an issue on coding-lab") {
		t.Errorf("error %q does not tell the operator to file an issue on coding-lab", err.Error())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestCheckLog_serverErrorFallsBackToOlderAttempt is the issue-#259 bug in one
// fixture: a run retried while the forge was unwell has a real attempt 2 with
// no stored log blob, so its route 500s while attempt 1 still serves the full
// log. The probe must NOT die on that (it used to, as a mismatch) — it serves
// attempt 1 and reports, out of band, that attempt 2 is what the reader is not
// getting and why.
func TestCheckLog_serverErrorFallsBackToOlderAttempt(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(k int) (int, string, string) {
			switch k {
			case 1:
				return http.StatusOK, "text/plain", "attempt-1-log"
			case 2:
				return http.StatusInternalServerError, "", "boom"
			default:
				return http.StatusNotFound, "", ""
			}
		})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if err != nil {
		t.Fatalf("CheckLog: %v; want the older attempt served, not an error", err)
	}
	if string(res.Log) != "attempt-1-log" {
		t.Errorf("log = %q; want attempt 1's body %q", res.Log, "attempt-1-log")
	}
	if res.Attempt != 1 {
		t.Errorf("Attempt = %d; want 1 (the newest attempt that answered)", res.Attempt)
	}
	if res.FallbackFrom != 2 || res.FallbackStatus != http.StatusInternalServerError {
		t.Errorf("FallbackFrom/FallbackStatus = %d/%d; want 2/500 (the newer attempt that failed)",
			res.FallbackFrom, res.FallbackStatus)
	}
}

// TestCheckLog_noFallbackWhenNewerAttemptServes: a 5xx does not terminate the
// probe — the first 404 still does — so a broken attempt 2 sandwiched between
// serving attempts 1 and 3 costs nothing: attempt 3 is served and NO fallback
// is reported, because the log handed over IS the latest one.
func TestCheckLog_noFallbackWhenNewerAttemptServes(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(k int) (int, string, string) {
			switch k {
			case 1:
				return http.StatusOK, "text/plain", "a1"
			case 2:
				return http.StatusInternalServerError, "", "boom"
			case 3:
				return http.StatusOK, "text/plain", "a3"
			default:
				return http.StatusNotFound, "", ""
			}
		})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	if string(res.Log) != "a3" {
		t.Errorf("log = %q; want the latest attempt %q (the 5xx must not stop the probe)", res.Log, "a3")
	}
	if res.Attempt != 3 {
		t.Errorf("Attempt = %d; want 3", res.Attempt)
	}
	if res.FallbackFrom != 0 {
		t.Errorf("FallbackFrom = %d; want 0 — the served log IS the latest attempt", res.FallbackFrom)
	}
}

// TestCheckLog_everyAttemptServerErrorIsUpstream: when no attempt ever answers
// a log, the forge — not lab's adapter — is why, so this is ErrLogUpstream with
// the route and the raw status, and explicitly NOT the mismatch prose that
// would send the reader off to debug lab. With no attempt answering 404 either,
// the probe honestly walks the full attempt cap before giving up; the cap is
// incidental here, so the error still names the outage rather than the cap.
func TestCheckLog_everyAttemptServerErrorIsUpstream(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(int) (int, string, string) { return http.StatusInternalServerError, "", "boom" })

	_, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if !errors.Is(err, tracker.ErrLogUpstream) {
		t.Fatalf("err = %v; want ErrLogUpstream", err)
	}
	if want := checkLogJobRoute + "/attempt/1/logs"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the requested log route %q", err.Error(), want)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not name the raw upstream status 500", err.Error())
	}
	if strings.Contains(err.Error(), "does not match this forge version") {
		t.Errorf("error %q blames lab's adapter for a forge-side failure", err.Error())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
	if len(h.logPaths) != checkLogAttemptCap {
		t.Errorf("probed %d attempts; want the full cap %d (no 404 ever terminated the probe)",
			len(h.logPaths), checkLogAttemptCap)
	}
}

// TestCheckLog_serverErrorThen404IsUpstream: the probe terminates normally on
// the 404, but nothing answered before it — so there is no older attempt to
// fall back to and the FIRST server error (the earliest evidence of the
// outage) is what the ErrLogUpstream message names.
func TestCheckLog_serverErrorThen404IsUpstream(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(k int) (int, string, string) {
			if k == 1 {
				return http.StatusInternalServerError, "", "boom"
			}
			return http.StatusNotFound, "", ""
		})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if !errors.Is(err, tracker.ErrLogUpstream) {
		t.Fatalf("err = %v; want ErrLogUpstream", err)
	}
	if want := checkLogJobRoute + "/attempt/1/logs"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name attempt 1's log route %q", err.Error(), want)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not name the raw upstream status 500", err.Error())
	}
	if strings.Contains(err.Error(), "does not match this forge version") {
		t.Errorf("error %q blames lab's adapter for a forge-side failure", err.Error())
	}
	if len(h.logPaths) != 2 {
		t.Errorf("probed %d attempts; want 2 (the 5xx, then the terminating 404)", len(h.logPaths))
	}
}

// TestCheckLog_nonPlainContentTypeIsMismatch: a 200 whose media type is not
// text/plain (an HTML login page, say) is a mismatch — the adapter must never
// hand an agent a chunk of HTML as if it were a job log.
func TestCheckLog_nonPlainContentTypeIsMismatch(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(int) (int, string, string) {
			return http.StatusOK, "text/html; charset=utf-8", "<html><body>login</body></html>"
		})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if !errors.Is(err, tracker.ErrLogAdapterMismatch) {
		t.Fatalf("err = %v; want ErrLogAdapterMismatch", err)
	}
}

// TestCheckLog_unknownContext: a context name no head-commit status row carries
// is ErrUnknownCheck, before any log route is touched.
func TestCheckLog_unknownContext(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(int) (int, string, string) {
			t.Errorf("log route must not be reached for an unknown context")
			return http.StatusOK, "text/plain", ""
		})

	_, err := h.client.CheckLog(context.Background(), 72, "ci/does-not-exist")
	if !errors.Is(err, tracker.ErrUnknownCheck) {
		t.Fatalf("err = %v; want ErrUnknownCheck", err)
	}
}

// TestCheckLog_externalCITargetIsUnsupported: a target_url that is not a
// Forgejo Actions job page (an external CI service) has no forge-served log to
// proxy — ErrUnsupported.
func TestCheckLog_externalCITargetIsUnsupported(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return "https://external-ci.example/build/1" },
		func(int) (int, string, string) {
			t.Errorf("log route must not be reached for an external-CI target_url")
			return http.StatusOK, "text/plain", ""
		})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if !errors.Is(err, tracker.ErrUnsupported) {
		t.Fatalf("err = %v; want ErrUnsupported", err)
	}
}

// TestCheckLog_unknownPull mirrors TestChecks_notFound: an unknown pull number
// fails on the first GET and unwraps to tracker.ErrNotFound, token never leaked.
func TestCheckLog_unknownPull(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"pull request does not exist"}`)
	})

	_, err := c.CheckLog(context.Background(), 999, checkLogContext)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestCheckLog_noTokenLeakOnLogRouteError forces the log route — the one call
// that reads a raw body with the token in the request header — to fail (403),
// and asserts the distinctive token never surfaces in the error.
func TestCheckLog_noTokenLeakOnLogRouteError(t *testing.T) {
	h := newCheckLogHarness(t,
		func(string) string { return checkLogJobRoute },
		func(int) (int, string, string) { return http.StatusForbidden, "", "denied" })

	_, err := h.client.CheckLog(context.Background(), 72, checkLogContext)
	if !errors.Is(err, tracker.ErrLogAdapterMismatch) {
		t.Fatalf("err = %v; want ErrLogAdapterMismatch", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// --- CreatePull ------------------------------------------------------------

func TestCreatePull(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":100,"state":"open","merged":false,"head":{"ref":"afk/63"},
		  "html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/100"}`)
	})

	ref, err := c.CreatePull(context.Background(), "afk/63", "main", "Fix thing", "Closes #63")
	if err != nil {
		t.Fatalf("CreatePull: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != apiPrefix+"/pulls" {
		t.Errorf("request = %s %s; want POST %s/pulls", gotMethod, gotPath, apiPrefix)
	}
	for k, want := range map[string]string{
		"head": "afk/63", "base": "main", "title": "Fix thing", "body": "Closes #63",
	} {
		if gotBody[k] != want {
			t.Errorf("request body %s = %v; want %q", k, gotBody[k], want)
		}
	}
	want := tracker.PullRef{Number: 100, HeadBranch: "afk/63", State: "open", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/100"}
	if ref != want {
		t.Errorf("returned PullRef = %+v; want %+v", ref, want)
	}
}

// --- MergePull -------------------------------------------------------------

// TestMergePull_success drives the open→merge path: GET the pull (idempotency
// probe), then POST the fixed "merge" method to /pulls/{n}/merge; the returned
// ref reports the merged state.
func TestMergePull_success(t *testing.T) {
	var mergePath string
	var mergeBody map[string]any
	postCalled := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"number":42,"state":"open","merged":false,"head":{"ref":"afk/42"},
			  "html_url":"https://git.cloonar.com/o/r/pulls/42"}`)
		case http.MethodPost:
			postCalled = true
			mergePath = r.URL.Path
			mergeBody = decodeBody(t, r)
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	ref, err := c.MergePull(context.Background(), 42)
	if err != nil {
		t.Fatalf("MergePull: %v", err)
	}
	if !postCalled {
		t.Fatal("MergePull did not POST a merge")
	}
	if mergePath != apiPrefix+"/pulls/42/merge" {
		t.Errorf("merge path = %s; want %s/pulls/42/merge", mergePath, apiPrefix)
	}
	if mergeBody["Do"] != "merge" {
		t.Errorf("merge Do = %v; want the fixed \"merge\" method", mergeBody["Do"])
	}
	// The head branch must survive the merge (acceptance criterion 3): the
	// merge request carries ONLY Do — no delete_branch_after_merge.
	if len(mergeBody) != 1 {
		t.Errorf("merge body = %v; want only Do (no delete_branch_after_merge)", mergeBody)
	}
	want := tracker.PullRef{Number: 42, HeadBranch: "afk/42", State: tracker.PullMerged, URL: "https://git.cloonar.com/o/r/pulls/42"}
	if ref != want {
		t.Errorf("PullRef = %+v; want %+v", ref, want)
	}
}

// TestMergePull_alreadyMergedIsConvergentNoOp pins the idempotency contract:
// a pull the forge already reports merged is a no-op success — no merge POST.
func TestMergePull_alreadyMergedIsConvergentNoOp(t *testing.T) {
	postCalled := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalled = true
		}
		_, _ = io.WriteString(w, `{"number":42,"state":"closed","merged":true,"head":{"ref":"afk/42"},
		  "html_url":"https://git.cloonar.com/o/r/pulls/42"}`)
	})

	ref, err := c.MergePull(context.Background(), 42)
	if err != nil {
		t.Fatalf("MergePull: %v", err)
	}
	if postCalled {
		t.Fatal("MergePull POSTed a merge for an already-merged pull; want a convergent no-op")
	}
	if ref.State != tracker.PullMerged {
		t.Errorf("state = %s; want merged", ref.State)
	}
}

// TestMergePull_rejectedSurfacesForgeWordsVerbatim pins the "mergeability is
// the forge's call" contract: a forge refusal (here a 405 with the required-
// check message) becomes tracker.ErrMergeRejected carrying the forge's own
// words — and never the token.
func TestMergePull_rejectedSurfacesForgeWordsVerbatim(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"number":42,"state":"open","merged":false,"head":{"ref":"afk/42"}}`)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"message":"Please check the required status checks before merging"}`)
	})

	_, err := c.MergePull(context.Background(), 42)
	if !errors.Is(err, tracker.ErrMergeRejected) {
		t.Fatalf("err = %v, want ErrMergeRejected", err)
	}
	if !strings.Contains(err.Error(), "required status checks") {
		t.Fatalf("err = %q, want the forge's own words verbatim", err.Error())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestMergePull_notFound: an unknown number's 404 unwraps to ErrNotFound.
func TestMergePull_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"pull request does not exist"}`)
	})

	_, err := c.MergePull(context.Background(), 999)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// --- Reviews ---------------------------------------------------------------

// TestReviews_stateMappingAndDismissed pins the read: every Forgejo review
// state maps onto lab's Review* vocabulary, an unknown state passes through
// lowercased, the dismissed flag carries through, and a login-empty reviewer
// falls back to username.
func TestReviews_stateMappingAndDismissed(t *testing.T) {
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
			return
		}
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `[
		  {"user":{"login":"alice"},"state":"APPROVED","body":"lgtm","dismissed":false},
		  {"user":{"login":"bob"},"state":"REQUEST_CHANGES","body":"fix this","dismissed":false},
		  {"user":{"login":"carol"},"state":"COMMENT","body":"nit","dismissed":false},
		  {"user":{"login":"dave"},"state":"REQUEST_REVIEW","body":"","dismissed":false},
		  {"user":{"login":"erin"},"state":"PENDING","body":"","dismissed":false},
		  {"user":{"login":"frank"},"state":"WEIRD_STATE","body":"?","dismissed":false},
		  {"user":{"login":"grace"},"state":"APPROVED","body":"ok","dismissed":true},
		  {"user":{"login":"","username":"heidi"},"state":"COMMENT","body":"via username","dismissed":false}
		]`)
	})

	reviews, err := c.Reviews(context.Background(), 42)
	if err != nil {
		t.Fatalf("Reviews: %v", err)
	}
	if gotPath != apiPrefix+"/pulls/42/reviews" {
		t.Errorf("path = %q; want %q", gotPath, apiPrefix+"/pulls/42/reviews")
	}
	want := []tracker.Review{
		{Reviewer: "alice", State: tracker.ReviewApproved, Body: "lgtm"},
		{Reviewer: "bob", State: tracker.ReviewChangesRequested, Body: "fix this"},
		{Reviewer: "carol", State: tracker.ReviewCommented, Body: "nit"},
		{Reviewer: "dave", State: tracker.ReviewRequested},
		{Reviewer: "erin", State: "pending"},
		{Reviewer: "frank", State: "weird_state", Body: "?"},
		{Reviewer: "grace", State: tracker.ReviewApproved, Body: "ok", Dismissed: true},
		{Reviewer: "heidi", State: tracker.ReviewCommented, Body: "via username"},
	}
	if len(reviews) != len(want) {
		t.Fatalf("got %d reviews; want %d", len(reviews), len(want))
	}
	for i, r := range reviews {
		if r != want[i] {
			t.Errorf("review[%d] = %+v; want %+v", i, r, want[i])
		}
	}
}

func TestReviews_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"pull request does not exist"}`)
	})
	if _, err := c.Reviews(context.Background(), 999); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// --- RerequestReview -------------------------------------------------------

// TestRerequestReview_postsChangesRequestedReviewers pins the latest-per-
// reviewer reduction: alice's latest is an approval (drops out), carol's
// changes-request is dismissed (skipped), dave's changes-request is followed by
// a PENDING that is skipped (so dave still counts). The POST carries exactly
// [bob, dave] in first-seen order.
func TestRerequestReview_postsChangesRequestedReviewers(t *testing.T) {
	var gotReqMethod, gotReqPath string
	var gotReqBody map[string]any
	postCalled := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[
			  {"user":{"login":"alice"},"state":"REQUEST_CHANGES","dismissed":false},
			  {"user":{"login":"bob"},"state":"REQUEST_CHANGES","dismissed":false},
			  {"user":{"login":"alice"},"state":"APPROVED","dismissed":false},
			  {"user":{"login":"carol"},"state":"REQUEST_CHANGES","dismissed":true},
			  {"user":{"login":"dave"},"state":"REQUEST_CHANGES","dismissed":false},
			  {"user":{"login":"dave"},"state":"PENDING","dismissed":false}
			]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
			postCalled = true
			gotReqMethod, gotReqPath = r.Method, r.URL.Path
			gotReqBody = decodeBody(t, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if err := c.RerequestReview(context.Background(), 42); err != nil {
		t.Fatalf("RerequestReview: %v", err)
	}
	if !postCalled {
		t.Fatal("RerequestReview did not POST requested_reviewers")
	}
	if gotReqMethod != http.MethodPost || gotReqPath != apiPrefix+"/pulls/42/requested_reviewers" {
		t.Errorf("request = %s %s; want POST %s/pulls/42/requested_reviewers", gotReqMethod, gotReqPath, apiPrefix)
	}
	raw, ok := gotReqBody["reviewers"].([]any)
	if !ok {
		t.Fatalf("reviewers field = %v; want a JSON array", gotReqBody["reviewers"])
	}
	got := make([]string, len(raw))
	for i, v := range raw {
		got[i], _ = v.(string)
	}
	if len(got) != 2 || got[0] != "bob" || got[1] != "dave" {
		t.Errorf("reviewers = %v; want [bob dave] (alice approved, carol dismissed)", got)
	}
}

// TestRerequestReview_noChangesRequestedIsNoOp: with no reviewer currently
// requesting changes, the re-request is a convergent no-op — no POST, nil error.
func TestRerequestReview_noChangesRequestedIsNoOp(t *testing.T) {
	postCalled := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalled = true
			t.Fatalf("unexpected POST %s; want a no-op", r.URL.Path)
		}
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, `[
		  {"user":{"login":"alice"},"state":"APPROVED","dismissed":false},
		  {"user":{"login":"bob"},"state":"COMMENT","dismissed":false}
		]`)
	})

	if err := c.RerequestReview(context.Background(), 42); err != nil {
		t.Fatalf("RerequestReview no-op: %v", err)
	}
	if postCalled {
		t.Fatal("RerequestReview POSTed with nobody requesting changes")
	}
}

// TestRerequestReview_nonVerdictRowsDoNotClearVerdict pins the fold rule that
// only verdict-bearing rows (APPROVED/REQUEST_CHANGES) update a reviewer's
// latest verdict: alice's REQUEST_CHANGES followed by a COMMENT reply and
// bob's REQUEST_CHANGES followed by a REQUEST_REVIEW row leave BOTH still in
// the POSTed set — the forge itself (preparePullReviewType) treats neither
// COMMENT nor REQUEST_REVIEW as a verdict event, so a reviewer who replied in
// the thread, or was pinged again, is still requesting changes.
func TestRerequestReview_nonVerdictRowsDoNotClearVerdict(t *testing.T) {
	var gotReqBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[
			  {"user":{"login":"alice"},"state":"REQUEST_CHANGES","dismissed":false},
			  {"user":{"login":"alice"},"state":"COMMENT","dismissed":false},
			  {"user":{"login":"bob"},"state":"REQUEST_CHANGES","dismissed":false},
			  {"user":{"login":"bob"},"state":"REQUEST_REVIEW","dismissed":false}
			]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
			gotReqBody = decodeBody(t, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	if err := c.RerequestReview(context.Background(), 42); err != nil {
		t.Fatalf("RerequestReview: %v", err)
	}
	raw, ok := gotReqBody["reviewers"].([]any)
	if !ok {
		t.Fatalf("reviewers field = %v; want a JSON array (the POST must happen)", gotReqBody["reviewers"])
	}
	got := make([]string, len(raw))
	for i, v := range raw {
		got[i], _ = v.(string)
	}
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("reviewers = %v; want [alice bob] — non-verdict rows must not clear the verdict", got)
	}
}

// TestRerequestReview_refusalSurfacesForgeWordsVerbatim: a forge refusal of the
// /requested_reviewers POST becomes tracker.ErrReviewRejected carrying the
// forge's own words — never the token.
func TestRerequestReview_refusalSurfacesForgeWordsVerbatim(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[{"user":{"login":"alice"},"state":"REQUEST_CHANGES","dismissed":false}]`)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"invalid review request"}`)
	})

	err := c.RerequestReview(context.Background(), 42)
	if !errors.Is(err, tracker.ErrReviewRejected) {
		t.Fatalf("err = %v, want ErrReviewRejected", err)
	}
	if !strings.Contains(err.Error(), "invalid review request") {
		t.Fatalf("err = %q, want the forge's own words verbatim", err.Error())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestRerequestReview_notFound: an unknown number's 404 (on the reviews read)
// stays ErrNotFound, NOT a review rejection.
func TestRerequestReview_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"pull request does not exist"}`)
	})

	err := c.RerequestReview(context.Background(), 999)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if errors.Is(err, tracker.ErrReviewRejected) {
		t.Fatalf("404 mislabeled as a review rejection: %v", err)
	}
}

// --- CommentPull -----------------------------------------------------------

// TestCommentPull posts a plain comment on the PR's shared issue-comment
// endpoint.
func TestCommentPull(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"body":"a note","user":{"login":"bot"},"created_at":"2026-04-01T00:00:00Z"}`)
	})

	if err := c.CommentPull(context.Background(), 42, "a note"); err != nil {
		t.Fatalf("CommentPull: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != apiPrefix+"/issues/42/comments" {
		t.Errorf("request = %s %s; want POST %s/issues/42/comments", gotMethod, gotPath, apiPrefix)
	}
	if gotBody["body"] != "a note" {
		t.Errorf("request body = %v; want {body: a note}", gotBody)
	}
}

// --- PullComments ------------------------------------------------------------

// TestPullComments_mapsSharedIssueCommentEndpoint pins that a PR's discussion
// comments are read/mapped exactly like Issue's comment thread — the shared
// issue-comment number space — including the login/username fallback.
func TestPullComments_mapsSharedIssueCommentEndpoint(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = io.WriteString(w, `[
		  {"body":"first comment","user":{"login":"alice","username":"alice"},"created_at":"2026-03-01T12:00:00Z"},
		  {"body":"second comment","user":{"login":"","username":"bob"},"created_at":"2026-03-01T13:00:00Z"}
		]`)
	})

	comments, err := c.PullComments(context.Background(), 62)
	if err != nil {
		t.Fatalf("PullComments: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != apiPrefix+"/issues/62/comments" {
		t.Errorf("request = %s %s; want GET %s/issues/62/comments", gotMethod, gotPath, apiPrefix)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments; want 2", len(comments))
	}
	if comments[0].Author != "alice" || comments[0].Body != "first comment" ||
		!comments[0].CreatedAt.Equal(mustTime(t, "2026-03-01T12:00:00Z")) {
		t.Errorf("comment[0] mismatch: %+v", comments[0])
	}
	if comments[1].Author != "bob" { // login empty → fall back to username
		t.Errorf("comment[1].Author = %q; want bob (username fallback)", comments[1].Author)
	}
}

// TestPullComments_manyCommentsFetchedInOneUnpaginatedGET is PullComments'
// half of the Issue comments-endpoint regression: the SAME endpoint ignores
// page/limit and always returns the full list, so a pagination loop would
// spin forever once a pull has >= pageLimit comments.
func TestPullComments_manyCommentsFetchedInOneUnpaginatedGET(t *testing.T) {
	const n = pageLimit + 9
	var commentReqs int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		commentReqs++
		if commentReqs > 3 {
			t.Errorf("comments endpoint requested %d times; want exactly 1", commentReqs)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, genCommentsJSON(n))
	})

	comments, err := c.PullComments(context.Background(), 62)
	if err != nil {
		t.Fatalf("PullComments: %v", err)
	}
	if commentReqs != 1 {
		t.Errorf("comments endpoint requested %d times; want exactly 1 (un-paginated GET)", commentReqs)
	}
	if len(comments) != n {
		t.Fatalf("got %d comments; want all %d", len(comments), n)
	}
}

func TestPullComments_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"pull request does not exist"}`)
	})
	if _, err := c.PullComments(context.Background(), 999); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// --- CloseIssue ------------------------------------------------------------

func TestCloseIssue(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = decodeBody(t, r)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"number":62,"state":"closed"}`)
	})

	if err := c.CloseIssue(context.Background(), 62); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != apiPrefix+"/issues/62" {
		t.Errorf("request = %s %s; want PATCH %s/issues/62", gotMethod, gotPath, apiPrefix)
	}
	if gotBody["state"] != "closed" {
		t.Errorf("request body = %v; want {state: closed}", gotBody)
	}
}

// --- EditIssue -------------------------------------------------------------

// strPtr is a *string literal helper for the patch cases.
func strPtr(s string) *string { return &s }

// TestEditIssue pins the title/body patch: PATCH /issues/{n} carrying ONLY the
// set fields (a nil pointer is omitted from the wire, so a title-only edit
// sends no body key and a body-only edit no title key; a non-nil empty Body is
// still sent, clearing it), the response mapped in LIST shape, and the request
// values matching the patch.
func TestEditIssue(t *testing.T) {
	cases := []struct {
		name       string
		edit       tracker.IssueEdit
		wantKeys   []string
		absentKeys []string
	}{
		{"title only", tracker.IssueEdit{Title: strPtr("new title")}, []string{"title"}, []string{"body"}},
		{"body only", tracker.IssueEdit{Body: strPtr("new body")}, []string{"body"}, []string{"title"}},
		{"both", tracker.IssueEdit{Title: strPtr("t"), Body: strPtr("b")}, []string{"title", "body"}, nil},
		{"clear body", tracker.IssueEdit{Body: strPtr("")}, []string{"body"}, []string{"title"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath string
			var gotBody map[string]any
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotBody = decodeBody(t, r)
				_, _ = io.WriteString(w, `{"number":62,"title":"new title","body":"new body","state":"open",
				  "comments":3,"created_at":"2026-07-06T00:00:00Z","updated_at":"2026-07-07T00:00:00Z"}`)
			})

			is, err := c.EditIssue(context.Background(), 62, tc.edit)
			if err != nil {
				t.Fatalf("EditIssue: %v", err)
			}
			if gotMethod != http.MethodPatch || gotPath != apiPrefix+"/issues/62" {
				t.Errorf("request = %s %s; want PATCH %s/issues/62", gotMethod, gotPath, apiPrefix)
			}
			for _, k := range tc.absentKeys {
				if _, ok := gotBody[k]; ok {
					t.Errorf("request body carried unset key %q: %v", k, gotBody)
				}
			}
			if tc.edit.Title != nil && gotBody["title"] != *tc.edit.Title {
				t.Errorf("wire title = %v; want %q", gotBody["title"], *tc.edit.Title)
			}
			if tc.edit.Body != nil && gotBody["body"] != *tc.edit.Body {
				t.Errorf("wire body = %v; want %q", gotBody["body"], *tc.edit.Body)
			}
			// LIST shape: no comment thread, count carried from `comments`.
			if is.Number != 62 || is.Comments != nil || is.CommentsCount != 3 {
				t.Errorf("edited issue = %+v; want #62 LIST shape (nil comments, count 3)", is)
			}
		})
	}
}

// TestEditIssue_notFound: an unknown number's 404 unwraps to ErrNotFound.
func TestEditIssue_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"issue does not exist"}`)
	})

	_, err := c.EditIssue(context.Background(), 999, tracker.IssueEdit{Title: strPtr("x")})
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// --- Error mapping ---------------------------------------------------------

// --- labels / triage surface (ADR-0014) --------------------------------------

// labelsJSON is the fixture label set: Forgejo emits colors WITHOUT the
// leading '#', and this deliberately arrives unsorted.
const labelsJSON = `[
  {"id":31,"name":"needs-triage","color":"6b7280","description":"triage me"},
  {"id":7,"name":"bug","color":"ee0701","description":""},
  {"id":12,"name":"kind/feature","color":"a2eeef","description":"new behavior"}
]`

// labelFixture answers GET /labels with labelsJSON (page 1, then the empty
// probe) and delegates everything else to next, recording each non-labels
// request method+path.
func labelFixture(t *testing.T, next http.HandlerFunc) (http.HandlerFunc, *[]string) {
	t.Helper()
	var calls []string
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == apiPrefix+"/labels" {
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, labelsJSON)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		next(w, r)
	}, &calls
}

func TestLabels_paginatedSortedNormalized(t *testing.T) {
	pages := map[string]string{"1": labelsJSON, "2": `[]`}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPrefix+"/labels" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, pages[r.URL.Query().Get("page")])
	})

	labels, err := c.Labels(context.Background())
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	// Name-ordered (contract), colors normalized to lab's #rrggbb vocabulary.
	want := []tracker.Label{
		{Name: "bug", Color: "#ee0701"},
		{Name: "kind/feature", Color: "#a2eeef", Description: "new behavior"},
		{Name: "needs-triage", Color: "#6b7280", Description: "triage me"},
	}
	if len(labels) != len(want) {
		t.Fatalf("Labels = %+v, want %d entries", labels, len(want))
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("labels[%d] = %+v, want %+v", i, labels[i], want[i])
		}
	}
}

// TestCreateIssue_resolvesLabelNamesToIDs pins the labels-are-IDs quirk
// staying behind the seam: callers pass names, the wire carries Forgejo ids —
// and an unknown name aborts BEFORE the create reaches the forge (Forgejo
// silently discards unknown entries, which would swallow a typo).
func TestCreateIssue_resolvesLabelNamesToIDs(t *testing.T) {
	var created map[string]any
	fixture, calls := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		created = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":9,"title":"found a bug","body":"details","state":"open",
		  "labels":[{"id":7,"name":"bug","color":"ee0701"}],
		  "created_at":"2026-07-06T00:00:00Z","updated_at":"2026-07-06T00:00:00Z"}`)
	})
	c := newTestClient(t, fixture)

	is, err := c.CreateIssue(context.Background(), "found a bug", "details", []string{"bug", "kind/feature"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "POST "+apiPrefix+"/issues" {
		t.Fatalf("forge calls = %v, want one POST /issues", *calls)
	}
	if created["title"] != "found a bug" || created["body"] != "details" {
		t.Errorf("create request = %v", created)
	}
	if ids, _ := created["labels"].([]any); len(ids) != 2 || ids[0] != float64(7) || ids[1] != float64(12) {
		t.Errorf("wire labels = %v, want the resolved ids [7 12]", created["labels"])
	}
	if is.Number != 9 || is.State != "open" || len(is.Labels) != 1 || is.Labels[0] != "bug" {
		t.Errorf("created issue = %+v", is)
	}

	// Unknown name: typed error naming it, and the create never leaves.
	_, err = c.CreateIssue(context.Background(), "doomed", "", []string{"bug", "nope"})
	if !errors.Is(err, tracker.ErrUnknownLabel) || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown label err = %v, want ErrUnknownLabel naming it", err)
	}
	if len(*calls) != 1 {
		t.Errorf("forge calls after refused create = %v, want no new POST", *calls)
	}
}

func TestAddIssueLabels_onePostWithResolvedIDs(t *testing.T) {
	var added map[string]any
	fixture, calls := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		added = decodeBody(t, r)
		_, _ = io.WriteString(w, `[]`)
	})
	c := newTestClient(t, fixture)

	if err := c.AddIssueLabels(context.Background(), 7, []string{"needs-triage", "bug"}); err != nil {
		t.Fatalf("AddIssueLabels: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "POST "+apiPrefix+"/issues/7/labels" {
		t.Fatalf("forge calls = %v, want one POST /issues/7/labels", *calls)
	}
	if ids, _ := added["labels"].([]any); len(ids) != 2 || ids[0] != float64(31) || ids[1] != float64(7) {
		t.Errorf("wire labels = %v, want [31 7]", added["labels"])
	}

	if err := c.AddIssueLabels(context.Background(), 7, []string{"ghost"}); !errors.Is(err, tracker.ErrUnknownLabel) {
		t.Fatalf("unknown label err = %v, want ErrUnknownLabel", err)
	}
	if len(*calls) != 1 {
		t.Errorf("forge calls after refused add = %v, want no new POST", *calls)
	}
}

func TestRemoveIssueLabels_oneDeletePerResolvedID(t *testing.T) {
	fixture, calls := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := newTestClient(t, fixture)

	if err := c.RemoveIssueLabels(context.Background(), 7, []string{"kind/feature", "bug"}); err != nil {
		t.Fatalf("RemoveIssueLabels: %v", err)
	}
	want := []string{
		"DELETE " + apiPrefix + "/issues/7/labels/12",
		"DELETE " + apiPrefix + "/issues/7/labels/7",
	}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("forge calls = %v, want %v", *calls, want)
	}

	if err := c.RemoveIssueLabels(context.Background(), 7, []string{"ghost", "bug"}); !errors.Is(err, tracker.ErrUnknownLabel) {
		t.Fatalf("unknown label err = %v, want ErrUnknownLabel", err)
	}
	if len(*calls) != 2 {
		t.Errorf("forge calls after refused remove = %v — strict resolution must abort before the first DELETE", *calls)
	}
}

func TestEnsureLabel_existingIsReturnedWithoutCreate(t *testing.T) {
	fixture, calls := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected forge call %s %s", r.Method, r.URL.Path)
	})
	c := newTestClient(t, fixture)

	l, err := c.EnsureLabel(context.Background(), "bug", "#123456", "ignored — existing wins")
	if err != nil {
		t.Fatalf("EnsureLabel: %v", err)
	}
	if l.Name != "bug" || l.Color != "#ee0701" || l.Description != "" {
		t.Errorf("ensured label = %+v, want the existing forge label untouched", l)
	}
	if len(*calls) != 0 {
		t.Errorf("forge calls = %v, want none beyond the list", *calls)
	}
}

func TestEnsureLabel_createsAbsentWithDefaultColor(t *testing.T) {
	var created map[string]any
	fixture, calls := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		created = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":99,"name":"triage/ready","color":"6b7280","description":"queued"}`)
	})
	c := newTestClient(t, fixture)

	l, err := c.EnsureLabel(context.Background(), "triage/ready", "", "queued")
	if err != nil {
		t.Fatalf("EnsureLabel: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "POST "+apiPrefix+"/labels" {
		t.Fatalf("forge calls = %v, want one POST /labels", *calls)
	}
	if created["name"] != "triage/ready" || created["color"] != defaultLabelColor || created["description"] != "queued" {
		t.Errorf("create request = %v, want the builtin-parity default color", created)
	}
	if l.Name != "triage/ready" || l.Color != "#6b7280" || l.Description != "queued" {
		t.Errorf("ensured label = %+v", l)
	}
}

// TestEnsureLabel_duplicateAnswerResolvesByRelist pins idempotency against
// the forge's duplicate answer: a concurrent ensure won the race between our
// list and create, the create comes back 409, and the client resolves to the
// now-existing label instead of failing the retry-safe op.
func TestEnsureLabel_duplicateAnswerResolvesByRelist(t *testing.T) {
	listCalls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == apiPrefix+"/labels":
			if r.URL.Query().Get("page") != "1" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			listCalls++
			if listCalls == 1 {
				_, _ = io.WriteString(w, `[]`) // not there yet…
				return
			}
			// …but the re-list after the conflict sees the winner's label.
			_, _ = io.WriteString(w, `[{"id":5,"name":"bug","color":"ee0701","description":"raced"}]`)
		case r.Method == http.MethodPost && r.URL.Path == apiPrefix+"/labels":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"label already exists"}`)
		default:
			t.Errorf("unexpected forge call %s %s", r.Method, r.URL.Path)
		}
	})

	l, err := c.EnsureLabel(context.Background(), "bug", "", "")
	if err != nil {
		t.Fatalf("EnsureLabel after duplicate answer: %v", err)
	}
	if l.Name != "bug" || l.Color != "#ee0701" || l.Description != "raced" {
		t.Errorf("ensured label = %+v, want the winner's label", l)
	}
	if listCalls != 2 {
		t.Errorf("list calls = %d, want 2 (initial + conflict re-list)", listCalls)
	}
}

func TestErrorMapping_statusAndNoTokenLeak(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				// A realistic Forgejo error envelope. Crucially it does NOT contain
				// the token — the client must never surface it regardless.
				_, _ = io.WriteString(w, `{"message":"boom happened","url":"https://git.cloonar.com/api/swagger"}`)
			})
			_, err := c.ReadyIssues(context.Background())
			if err == nil {
				t.Fatal("expected an error for non-2xx status")
			}
			msg := err.Error()
			if !strings.Contains(msg, strconv.Itoa(status)) {
				t.Errorf("error %q does not mention status %d", msg, status)
			}
			if !strings.Contains(msg, "boom happened") {
				t.Errorf("error %q does not carry the body snippet", msg)
			}
			if strings.Contains(msg, testToken) {
				t.Fatalf("token leaked into error: %q", msg)
			}
		})
	}
}

// --- Pagination ------------------------------------------------------------

func genIssuesJSON(start, n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"number":%d,"title":"i%d","state":"open","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`, start+i, start+i)
	}
	b.WriteByte(']')
	return b.String()
}

// TestPagination_walksUntilEmptyPage pins the paginate-until-EMPTY rule
// (port-spec §7): a short-but-nonempty page proves nothing — Forgejo silently
// clamps limit to its [api].MAX_RESPONSE_ITEMS, so a server clamped below our
// pageLimit serves nothing but short pages even when more exist. The client
// must follow a short page with one more request and stop only on [].
// (The pagination vehicle is the OPEN walk — since issue #176 that is the
// listing that still rides fetchPages; the closed view is a one-request
// window with no pagination to pin.)
func TestPagination_walksUntilEmptyPage(t *testing.T) {
	var pages []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		switch page {
		case "1":
			_, _ = io.WriteString(w, genIssuesJSON(1, pageLimit)) // full page → client asks for more
		case "2":
			_, _ = io.WriteString(w, genIssuesJSON(pageLimit+1, 3)) // short but NONEMPTY → keep going
		case "3":
			_, _ = io.WriteString(w, `[]`) // empty page → stop
		default:
			t.Errorf("unexpected page %q (should have stopped on the empty page)", page)
			_, _ = io.WriteString(w, `[]`)
		}
	})

	issues, err := c.Issues(context.Background(), "open")
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if len(issues) != pageLimit+3 {
		t.Fatalf("got %d issues; want %d (both pages concatenated)", len(issues), pageLimit+3)
	}
	if len(pages) != 3 || pages[0] != "1" || pages[1] != "2" || pages[2] != "3" {
		t.Fatalf("requested pages = %v; want [1 2 3] (short page must be followed by the empty-page probe)", pages)
	}
	// Order preserved across pages: first of page 1, last of page 2.
	if issues[0].Number != 1 || issues[len(issues)-1].Number != pageLimit+3 {
		t.Errorf("page order not preserved: first=%d last=%d", issues[0].Number, issues[len(issues)-1].Number)
	}
}

// TestPagination_clampedServerNotTruncated is the regression for the
// MAX_RESPONSE_ITEMS clamp: a server that caps every page at 25 (< pageLimit)
// still has all three pages collected — under short-page termination the
// client would have silently stopped after page 1.
func TestPagination_clampedServerNotTruncated(t *testing.T) {
	const clamp = 25
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch page {
		case 1, 2, 3:
			_, _ = io.WriteString(w, genIssuesJSON((page-1)*clamp+1, clamp))
		default:
			_, _ = io.WriteString(w, `[]`)
		}
	})

	issues, err := c.Issues(context.Background(), "open")
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if len(issues) != 3*clamp {
		t.Fatalf("got %d issues; want %d (clamped pages must all be collected, not truncated)", len(issues), 3*clamp)
	}
	if issues[len(issues)-1].Number != 3*clamp {
		t.Errorf("last issue = %d; want %d", issues[len(issues)-1].Number, 3*clamp)
	}
}

// TestPagination_pageCapIsExplicitError pins the defensive cap: an endpoint
// that never runs dry (a bug, or one that ignores pagination) must yield a
// loud error after maxPages pages — never silent truncation, never an
// infinite loop.
func TestPagination_pageCapIsExplicitError(t *testing.T) {
	var requests int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > maxPages+1 {
			// Break a would-be infinite loop so a regression fails fast
			// instead of hanging the suite.
			t.Errorf("more than %d requests; page cap not enforced", maxPages+1)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, genIssuesJSON((requests-1)*pageLimit+1, pageLimit)) // always a full page
	})

	_, err := c.Issues(context.Background(), "open")
	if err == nil {
		t.Fatal("expected an explicit error once the page cap is exceeded")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxPages)) || !strings.Contains(err.Error(), "truncate") {
		t.Errorf("error %q should name the page cap and refuse truncation", err)
	}
	if requests != maxPages {
		t.Errorf("made %d requests; want exactly %d (the cap)", requests, maxPages)
	}
}

// --- path escaping ---------------------------------------------------------

// TestRequestPathsEscapeOwnerRepo: owner/repo come from the operator-supplied
// remote URL, so hostile segments must be path-escaped into request paths —
// un-escaped, a "?" would swallow the rest of the path as a bogus query
// (hitting the wrong endpoint) and a "#" would silently drop the real query
// params as a fragment. Escaped, the request targets the literal path and the
// query survives.
func TestRequestPathsEscapeOwnerRepo(t *testing.T) {
	var gotEscaped, gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
			return
		}
		gotEscaped = r.URL.EscapedPath()
		gotState = r.URL.Query().Get("state")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.Client(), srv.URL+"/api/v1", testToken, "evil?owner", "re#po/../x")

	if _, err := c.Issues(context.Background(), "open"); err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if want := "/api/v1/repos/evil%3Fowner/re%23po%2F..%2Fx/issues"; gotEscaped != want {
		t.Errorf("escaped path = %q; want %q", gotEscaped, want)
	}
	if gotState != "open" {
		t.Errorf("state param = %q; want %q (query must survive a hostile owner/repo)", gotState, "open")
	}
}

// --- constructor defaults --------------------------------------------------

func TestNew_trimsTrailingSlashAndDefaultsClient(t *testing.T) {
	c := New(nil, "https://git.cloonar.com/api/v1/", "tok", "o", "r")
	if c.baseURL != "https://git.cloonar.com/api/v1" {
		t.Errorf("baseURL = %q; want trailing slash trimmed", c.baseURL)
	}
	if c.httpClient == nil {
		t.Error("nil httpClient should default to a non-nil client")
	}
	if c.timeout != defaultRequestTimeout {
		t.Errorf("timeout = %s; want %s", c.timeout, defaultRequestTimeout)
	}
}
