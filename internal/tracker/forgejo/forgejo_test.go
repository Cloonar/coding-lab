package forgejo

import (
	"context"
	"encoding/json"
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

func TestIssues_stateForwardedAndMapped(t *testing.T) {
	for _, state := range []string{"open", "closed", "all"} {
		t.Run(state, func(t *testing.T) {
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
			issues, err := c.Issues(context.Background(), state)
			if err != nil {
				t.Fatalf("Issues(%q): %v", state, err)
			}
			if gotState != state {
				t.Errorf("state param = %q; want %q", gotState, state)
			}
			if gotType != "issues" {
				t.Errorf("type param = %q; want issues (excludes PRs)", gotType)
			}
			if len(issues) != 1 || issues[0].Number != 3 || issues[0].Comments != nil {
				t.Errorf("mapping mismatch: %+v", issues)
			}
		})
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

func TestPulls_stateMappingAndHeads(t *testing.T) {
	var gotState string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			_, _ = io.WriteString(w, `[]`) // paginate-until-empty probe
			return
		}
		gotState = r.URL.Query().Get("state")
		// #9 and #80 share head afk/7 — one closed-unmerged, one open. The client
		// returns BOTH; open-beats-closed precedence when heads collide is the
		// tracker package's pure PullState fn, not this client's job.
		_, _ = io.WriteString(w, `[
		  {"number":72,"state":"open","merged":false,"head":{"ref":"afk/63"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/72"},
		  {"number":40,"state":"closed","merged":true,"head":{"ref":"afk/12"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/40"},
		  {"number":9,"state":"closed","merged":false,"head":{"ref":"afk/7"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/9"},
		  {"number":80,"state":"open","merged":false,"head":{"ref":"afk/7"},"html_url":"https://git.cloonar.com/Cloonar/nixos/pulls/80"}
		]`)
	})

	pulls, err := c.Pulls(context.Background())
	if err != nil {
		t.Fatalf("Pulls: %v", err)
	}
	// state=all is required: a merged afk PR is no longer "open" but the reaper
	// must still see it.
	if gotState != "all" {
		t.Errorf("state param = %q; want all", gotState)
	}
	want := []tracker.PullRef{
		{Number: 72, HeadBranch: "afk/63", State: "open", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/72"},
		{Number: 40, HeadBranch: "afk/12", State: "merged", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/40"},
		{Number: 9, HeadBranch: "afk/7", State: "closed", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/9"},
		{Number: 80, HeadBranch: "afk/7", State: "open", URL: "https://git.cloonar.com/Cloonar/nixos/pulls/80"},
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

// --- Error mapping ---------------------------------------------------------

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

	issues, err := c.Issues(context.Background(), "all")
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

	issues, err := c.Issues(context.Background(), "all")
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

	_, err := c.Issues(context.Background(), "all")
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

	if _, err := c.Issues(context.Background(), "all"); err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if want := "/api/v1/repos/evil%3Fowner/re%23po%2F..%2Fx/issues"; gotEscaped != want {
		t.Errorf("escaped path = %q; want %q", gotEscaped, want)
	}
	if gotState != "all" {
		t.Errorf("state param = %q; want %q (query must survive a hostile owner/repo)", gotState, "all")
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
