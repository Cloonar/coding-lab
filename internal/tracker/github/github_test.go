package github

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
	testToken = "SECRET-gh-tok-abc123" // distinctive so error tests can assert it never leaks
	apiPrefix = "/repos/octocat/hello-world"
)

// newTestClient spins up an httptest server with h and returns a Client bound
// to octocat/hello-world against it. The base URL is the server root verbatim
// (github's BaseURL is the API origin, no /api/v1 suffix).
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.Client(), srv.URL, testToken, "octocat", "hello-world")
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
	var gotAuth, gotAccept, gotVersion, gotPath, gotRawURL string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.Header.Get("X-GitHub-Api-Version")
		gotPath = r.URL.Path
		gotRawURL = r.URL.String()
		// One PR folded into the issues list (carries pull_request) must be
		// dropped; the two genuine issues carry the ready label.
		_, _ = io.WriteString(w, `[
		  {"number":62,"title":"first","body":"body one","state":"open",
		   "labels":[{"name":"ready-for-agent"},{"name":"bug"}],"comments":3,
		   "created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-03T04:05:06Z"},
		  {"number":50,"title":"a PR folded in","state":"open",
		   "labels":[{"name":"ready-for-agent"}],"pull_request":{"url":"https://api.github.com/x"},
		   "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},
		  {"number":7,"title":"second","state":"open",
		   "labels":[{"name":"ready-for-agent"}],
		   "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}
		]`)
	})

	issues, err := c.ReadyIssues(context.Background())
	if err != nil {
		t.Fatalf("ReadyIssues: %v", err)
	}

	if gotPath != apiPrefix+"/issues" {
		t.Errorf("path = %q; want %q", gotPath, apiPrefix+"/issues")
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q; want %q", gotAuth, "Bearer "+testToken)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q; want application/vnd.github+json", gotAccept)
	}
	if gotVersion != apiVersion {
		t.Errorf("X-GitHub-Api-Version = %q; want %q", gotVersion, apiVersion)
	}
	if strings.Contains(gotRawURL, testToken) {
		t.Errorf("token leaked into request URL %q", gotRawURL)
	}
	for k, want := range map[string]string{
		"state":    "open",
		"labels":   "ready-for-agent",
		"per_page": "100",
	} {
		if got := gotQuery.Get(k); got != want {
			t.Errorf("query %s = %q; want %q", k, got, want)
		}
	}

	if len(issues) != 2 {
		t.Fatalf("got %d issues; want 2 (the PR is dropped)", len(issues))
	}
	i0 := issues[0]
	if i0.Number != 62 || i0.Title != "first" || i0.Body != "body one" || i0.State != "open" || i0.CommentsCount != 3 {
		t.Errorf("issue[0] mismatch: %+v", i0)
	}
	if len(i0.Labels) != 2 || i0.Labels[0] != "ready-for-agent" || i0.Labels[1] != "bug" {
		t.Errorf("issue[0].Labels = %v; want [ready-for-agent bug]", i0.Labels)
	}
	if !i0.CreatedAt.Equal(mustTime(t, "2026-01-02T03:04:05Z")) || i0.Comments != nil {
		t.Errorf("issue[0] time/comments: %+v", i0)
	}
	if issues[1].Number != 7 {
		t.Errorf("issue[1] = %+v; want #7 (the PR #50 filtered out)", issues[1])
	}
}

// TestReadyIssues_dropsIssuesWithoutReadyLabel: parity with the Forgejo
// client's client-side recheck — an issue the server returns without the label
// (a discarded/ignored label filter) never reaches the ready queue.
func TestReadyIssues_dropsIssuesWithoutReadyLabel(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[
		  {"number":1,"title":"unlabeled","state":"open",
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
		t.Fatalf("ready queue = %+v; want only issue #3", issues)
	}
}

func TestReadyIssues_emptyIsNonNil(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	issues, err := c.ReadyIssues(context.Background())
	if err != nil {
		t.Fatalf("ReadyIssues: %v", err)
	}
	if issues == nil || len(issues) != 0 {
		t.Fatalf("empty queue = %#v; want non-nil empty slice", issues)
	}
}

// --- Issues(state) ---------------------------------------------------------

// TestIssues_openWalkUnchangedAndPRsFiltered pins the open view's shape after
// issue #176: still the full Link-following walk (state=open), with GitHub's
// folded-in PRs dropped client-side.
func TestIssues_openWalkUnchangedAndPRsFiltered(t *testing.T) {
	var gotState string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotState = r.URL.Query().Get("state")
		_, _ = io.WriteString(w, `[
		  {"number":3,"title":"an issue","state":"open",
		   "created_at":"2026-02-02T00:00:00Z","updated_at":"2026-02-02T00:00:00Z"},
		  {"number":4,"title":"a PR","state":"open","pull_request":{"url":"u"},
		   "created_at":"2026-02-02T00:00:00Z","updated_at":"2026-02-02T00:00:00Z"}
		]`)
	})
	issues, err := c.Issues(context.Background(), "open")
	if err != nil {
		t.Fatalf("Issues(open): %v", err)
	}
	if gotState != "open" {
		t.Errorf("state param = %q; want open", gotState)
	}
	if len(issues) != 1 || issues[0].Number != 3 {
		t.Errorf("issues = %+v; want only #3 (PR filtered)", issues)
	}
}

// TestIssues_closedWindowSingleRequestLinkIgnored pins the closed view's
// request math (issue #176): exactly ONE request — state=closed&sort=updated&
// direction=desc&per_page=50 (GitHub has no recently-closed sort;
// updated-desc approximates it) — whose Link rel="next" header is
// deliberately NOT followed, and whose folded-in PRs are still dropped (up to
// the window of rows, fewer after the PR filtering).
func TestIssues_closedWindowSingleRequestLinkIgnored(t *testing.T) {
	var requests int
	var gotQuery url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotQuery = r.URL.Query()
		setNextLink(w, r) // the bait: a next page that must never be fetched
		_, _ = io.WriteString(w, `[
		  {"number":9,"title":"newest closed","state":"closed",
		   "created_at":"2026-02-02T00:00:00Z","updated_at":"2026-02-03T00:00:00Z"},
		  {"number":8,"title":"a closed PR","state":"closed","pull_request":{"url":"u"},
		   "created_at":"2026-02-02T00:00:00Z","updated_at":"2026-02-02T12:00:00Z"},
		  {"number":5,"title":"older closed","state":"closed",
		   "created_at":"2026-02-01T00:00:00Z","updated_at":"2026-02-02T00:00:00Z"}
		]`)
	})
	issues, err := c.Issues(context.Background(), "closed")
	if err != nil {
		t.Fatalf("Issues(closed): %v", err)
	}
	if requests != 1 {
		t.Errorf("closed view made %d requests; want exactly 1 (the Link header must be ignored)", requests)
	}
	for k, want := range map[string]string{
		"state":     "closed",
		"sort":      "updated",
		"direction": "desc",
		"per_page":  strconv.Itoa(tracker.RecentClosedWindow),
	} {
		if got := gotQuery.Get(k); got != want {
			t.Errorf("closed query %s = %q; want %q", k, got, want)
		}
	}
	if len(issues) != 2 || issues[0].Number != 9 || issues[1].Number != 5 {
		t.Errorf("issues = %+v; want [#9 #5] (the folded PR #8 filtered out)", issues)
	}
}

// TestIssues_allIsOpenWalkPlusClosedWindow pins the all view (issue #176):
// the Link-following open walk plus the one-request closed window, open rows
// first.
func TestIssues_allIsOpenWalkPlusClosedWindow(t *testing.T) {
	var requests, closedRequests int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Query().Get("state") {
		case "open":
			_, _ = io.WriteString(w, `[{"number":7,"title":"open one","state":"open",
			  "created_at":"2026-02-02T00:00:00Z","updated_at":"2026-02-02T00:00:00Z"}]`)
		case "closed":
			closedRequests++
			setNextLink(w, r) // must be ignored
			_, _ = io.WriteString(w, `[{"number":5,"title":"closed one","state":"closed",
			  "created_at":"2026-02-01T00:00:00Z","updated_at":"2026-02-02T00:00:00Z"}]`)
		default:
			t.Errorf("unexpected state param %q", r.URL.Query().Get("state"))
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	issues, err := c.Issues(context.Background(), "all")
	if err != nil {
		t.Fatalf("Issues(all): %v", err)
	}
	if requests != 2 || closedRequests != 1 {
		t.Errorf("all view = %d requests (%d closed); want 2 total, 1 closed", requests, closedRequests)
	}
	if len(issues) != 2 || issues[0].Number != 7 || issues[1].Number != 5 {
		t.Errorf("issues = %+v; want [#7 open, #5 closed] (open set first)", issues)
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

func TestIssues_emptyIsNonNil(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	issues, err := c.Issues(context.Background(), "all")
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if issues == nil || len(issues) != 0 {
		t.Fatalf("empty = %#v; want non-nil empty slice", issues)
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
			_, _ = io.WriteString(w, `[
			  {"body":"first comment","user":{"login":"alice"},"created_at":"2026-03-01T12:00:00Z"},
			  {"body":"second comment","user":{"login":"bob"},"created_at":"2026-03-01T13:00:00Z"}
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
	if issue.Number != 62 || issue.Title != "deep" || issue.Body != "the body" {
		t.Errorf("issue scalars: %+v", issue)
	}
	if len(issue.Comments) != 2 || issue.Comments[0].Author != "alice" || issue.Comments[1].Author != "bob" {
		t.Errorf("comments = %+v", issue.Comments)
	}
	if issue.Comments[0].Body != "first comment" ||
		!issue.Comments[0].CreatedAt.Equal(mustTime(t, "2026-03-01T12:00:00Z")) {
		t.Errorf("comment[0] mismatch: %+v", issue.Comments[0])
	}
}

func genCommentsJSON(start, n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"body":"c%d","user":{"login":"alice"},"created_at":"2026-03-01T12:00:00Z"}`, start+i)
	}
	b.WriteByte(']')
	return b.String()
}

// TestIssue_commentsPaginate: unlike Forgejo, GitHub's per-issue comments
// endpoint paginates with Link, so the client must follow it (a >100-comment
// thread would otherwise be truncated at 100).
func TestIssue_commentsPaginate(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/issues/62":
			_, _ = io.WriteString(w, `{"number":62,"title":"chatty","state":"open",
			  "created_at":"2026-03-01T10:00:00Z","updated_at":"2026-03-02T11:00:00Z"}`)
		case apiPrefix + "/issues/62/comments":
			switch r.URL.Query().Get("page") {
			case "1":
				setNextLink(w, r)
				_, _ = io.WriteString(w, genCommentsJSON(1, pageLimit))
			case "2":
				_, _ = io.WriteString(w, genCommentsJSON(pageLimit+1, 5))
			default:
				t.Errorf("unexpected comments page %q", r.URL.Query().Get("page"))
				_, _ = io.WriteString(w, `[]`)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	issue, err := c.Issue(context.Background(), 62)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(issue.Comments) != pageLimit+5 {
		t.Fatalf("got %d comments; want %d (both pages)", len(issue.Comments), pageLimit+5)
	}
	if issue.Comments[0].Body != "c1" || issue.Comments[pageLimit+4].Body != fmt.Sprintf("c%d", pageLimit+5) {
		t.Errorf("comment order lost across pages")
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
	if gotMethod != http.MethodPost || gotPath != apiPrefix+"/issues/62/comments" {
		t.Errorf("request = %s %s; want POST %s/issues/62/comments", gotMethod, gotPath, apiPrefix)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["body"] != "hi there" {
		t.Errorf("request body = %v", gotBody)
	}
}

// --- Pulls -----------------------------------------------------------------

// TestPulls_openPlusRecentClosedWindow pins the bounded contract (issue
// #176) and its request math: the open walk still follows Link (2 pages
// here), the closed window is exactly ONE request — state=closed&
// sort=updated&direction=desc&per_page=50 — whose Link rel="next" header is
// deliberately NOT followed (the bait is served and must rot). Open rows come
// first; the merged state still derives from merged_at per row (no `merged`
// bool on the list), so a merged pull in the window maps to "merged".
func TestPulls_openPlusRecentClosedWindow(t *testing.T) {
	var requests, closedRequests int
	var closedQuery url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		q := r.URL.Query()
		switch q.Get("state") {
		case "open":
			if q.Get("page") == "1" {
				setNextLink(w, r) // the open walk DOES follow Link
				_, _ = io.WriteString(w, `[{"number":72,"state":"open","merged_at":null,"head":{"ref":"afk/63"},"html_url":"https://github.com/octocat/hello-world/pull/72"}]`)
				return
			}
			// page 2, no Link → the walk stops.
			_, _ = io.WriteString(w, `[{"number":80,"state":"open","merged_at":null,"head":{"ref":"afk/7"},"html_url":"https://github.com/octocat/hello-world/pull/80"}]`)
		case "closed":
			closedRequests++
			closedQuery = q
			setNextLink(w, r) // the bait: the window must NOT follow it
			_, _ = io.WriteString(w, `[
			  {"number":40,"state":"closed","merged_at":"2026-01-01T00:00:00Z","head":{"ref":"afk/12"},"html_url":"https://github.com/octocat/hello-world/pull/40"},
			  {"number":9,"state":"closed","merged_at":null,"head":{"ref":"afk/7"},"html_url":"https://github.com/octocat/hello-world/pull/9"}
			]`)
		default:
			t.Errorf("unexpected state param %q (state=all is the walk #176 removed)", q.Get("state"))
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	pulls, err := c.Pulls(context.Background())
	if err != nil {
		t.Fatalf("Pulls: %v", err)
	}
	if requests != 3 {
		t.Errorf("Pulls made %d requests; want exactly 3 (2 open pages + 1 closed page)", requests)
	}
	if closedRequests != 1 {
		t.Errorf("closed window fetched %d times; want exactly 1 (its Link header must be ignored)", closedRequests)
	}
	for k, want := range map[string]string{
		"state":     "closed",
		"sort":      "updated",
		"direction": "desc",
		"per_page":  strconv.Itoa(tracker.RecentClosedWindow),
	} {
		if got := closedQuery.Get(k); got != want {
			t.Errorf("closed query %s = %q; want %q", k, got, want)
		}
	}
	want := []tracker.PullRef{
		{Number: 72, HeadBranch: "afk/63", State: "open", URL: "https://github.com/octocat/hello-world/pull/72"},
		{Number: 80, HeadBranch: "afk/7", State: "open", URL: "https://github.com/octocat/hello-world/pull/80"},
		{Number: 40, HeadBranch: "afk/12", State: "merged", URL: "https://github.com/octocat/hello-world/pull/40"},
		{Number: 9, HeadBranch: "afk/7", State: "closed", URL: "https://github.com/octocat/hello-world/pull/9"},
	}
	if len(pulls) != len(want) {
		t.Fatalf("got %d pulls (%+v); want %d", len(pulls), pulls, len(want))
	}
	for i := range want {
		if pulls[i] != want[i] {
			t.Errorf("pull[%d] = %+v; want %+v", i, pulls[i], want[i])
		}
	}
}

// --- PullsForHead ----------------------------------------------------------

// TestPullsForHead_doneSignalConformance is the shared done-signal table (the
// same cases run against all three backends): PullsForHead only enumerates
// candidates, and tracker.DonePull projects the same verdict it would from a
// full Pulls() walk. Every case also pins the GitHub request shape: the head
// filter MUST carry the owner: prefix (a bare branch name is silently ignored
// and the listing degenerates into the unbounded walk #176 exists to kill),
// alongside base and state=all. The different-base case plays a misfiltering
// server (rows for another base and another head) to prove the defensive
// client-side re-filter.
func TestPullsForHead_doneSignalConformance(t *testing.T) {
	const head, base = "afk/9", "main"
	for _, tc := range []struct {
		name        string
		body        string
		wantNumbers []int
		wantDone    bool
		wantDoneNum int
	}{
		{
			name:        "open pull is the done-signal",
			body:        `[{"number":80,"state":"open","merged_at":null,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://github.com/octocat/hello-world/pull/80"}]`,
			wantNumbers: []int{80}, wantDone: true, wantDoneNum: 80,
		},
		{
			name:        "merged pull is the done-signal",
			body:        `[{"number":40,"state":"closed","merged_at":"2026-01-01T00:00:00Z","head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://github.com/octocat/hello-world/pull/40"}]`,
			wantNumbers: []int{40}, wantDone: true, wantDoneNum: 40,
		},
		{
			name:        "closed-unmerged only is no done-signal",
			body:        `[{"number":9,"state":"closed","merged_at":null,"head":{"ref":"afk/9"},"base":{"ref":"main"},"html_url":"https://github.com/octocat/hello-world/pull/9"}]`,
			wantNumbers: []int{9}, wantDone: false,
		},
		{
			name:        "no pull at all is empty success",
			body:        `[]`,
			wantNumbers: nil, wantDone: false,
		},
		{
			// A server that ignored the filters: a pull from afk/9 onto dev and
			// one from another head onto main — both re-filtered out client-side.
			name: "pull onto a different base does not match",
			body: `[
			  {"number":77,"state":"open","merged_at":null,"head":{"ref":"afk/9"},"base":{"ref":"dev"},"html_url":"https://github.com/octocat/hello-world/pull/77"},
			  {"number":72,"state":"open","merged_at":null,"head":{"ref":"afk/63"},"base":{"ref":"main"},"html_url":"https://github.com/octocat/hello-world/pull/72"}
			]`,
			wantNumbers: nil, wantDone: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery url.Values
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				_, _ = io.WriteString(w, tc.body)
			})

			refs, err := c.PullsForHead(context.Background(), head, base)
			if err != nil {
				t.Fatalf("PullsForHead: %v", err)
			}
			for k, want := range map[string]string{
				"state": "all",
				"head":  "octocat:afk/9", // owner-qualified, or GitHub ignores the filter
				"base":  "main",
			} {
				if got := gotQuery.Get(k); got != want {
					t.Errorf("query %s = %q; want %q", k, got, want)
				}
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

// TestPullsForHead_serverErrorSurfaces: an upstream failure surfaces as an
// error with no partial data (seam convention).
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
		{"closed", true, "merged"},
		{"closed", false, "closed"},
		{"open", true, "merged"},
	} {
		if got := derivePullState(tc.state, tc.merged); got != tc.want {
			t.Errorf("derivePullState(%q,%v) = %q; want %q", tc.state, tc.merged, got, tc.want)
		}
	}
}

// --- Pull ------------------------------------------------------------------

// TestPull pins the single-PR detail read (labctl pr view): one GET
// /pulls/{n}, the full body carried through verbatim, and the same
// merged_at-derived three-valued state as Pulls.
func TestPull(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"number":72,"title":"feat: capture card",
		  "body":"card: |\n  kind: capture\n  target: nixos",
		  "state":"closed","merged_at":"2026-07-01T10:00:00Z","head":{"ref":"afk/63"},
		  "html_url":"https://github.com/octocat/hello-world/pull/72"}`)
	})

	pd, err := c.Pull(context.Background(), 72)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != apiPrefix+"/pulls/72" {
		t.Errorf("request = %s %s; want GET %s/pulls/72", gotMethod, gotPath, apiPrefix)
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q; want %q", gotAuth, "Bearer "+testToken)
	}
	want := tracker.PullDetail{
		Number:     72,
		Title:      "feat: capture card",
		Body:       "card: |\n  kind: capture\n  target: nixos",
		State:      tracker.PullMerged, // state=closed + merged_at set → merged
		HeadBranch: "afk/63",
		URL:        "https://github.com/octocat/hello-world/pull/72",
	}
	if pd != want {
		t.Errorf("PullDetail = %+v; want %+v", pd, want)
	}
}

// TestPull_notFound pins the unknown-number path: GitHub's 404 unwraps to
// tracker.ErrNotFound, never leaking the token.
func TestPull_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})

	_, err := c.Pull(context.Background(), 999)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestPull_rateLimited pins ADR-0015 decision 6 on the new read: a throttled
// call unwraps to ErrRateLimited like every other GitHub client op.
func TestPull_rateLimited(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1720000000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	})

	_, err := c.Pull(context.Background(), 72)
	if !errors.Is(err, tracker.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// --- Checks ------------------------------------------------------------

// TestChecks pins the three-call shape (GET the pull for head.sha, then GET
// the Checks API and the legacy combined-status API for that SHA) and the
// union: check-runs rows first, then status rows, with NO deduplication —
// the "ci" check-run and "ci" status below are the SAME name from the two
// APIs and BOTH must survive into the result. It also covers the full
// conclusion mapping table (success, neutral, skipped, cancelled, timed_out,
// action_required, failure, one unrecognized conclusion) plus in-flight
// check-run statuses (queued, in_progress), and the status-row mapping
// (success, pending, error, failure).
func TestChecks(t *testing.T) {
	const sha = "deadbeefcafe0123456789abcdef0123456789a"
	var gotPullPath, gotCheckRunsPath, gotStatusPath string
	var gotCheckRunsAuth, gotStatusAuth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == apiPrefix+"/pulls/72" && r.Method == http.MethodGet:
			gotPullPath = r.URL.Path
			_, _ = fmt.Fprintf(w, `{"number":72,"title":"feat: thing","state":"open","merged_at":null,
			  "head":{"ref":"afk/63","sha":%q},
			  "html_url":"https://github.com/octocat/hello-world/pull/72"}`, sha)
		case r.URL.Path == apiPrefix+"/commits/"+sha+"/check-runs" && r.Method == http.MethodGet:
			gotCheckRunsPath = r.URL.Path
			gotCheckRunsAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"total_count":11,"check_runs":[
			  {"name":"build","status":"completed","conclusion":"success","html_url":"https://ci.example/build","output":{"title":"Build passed"}},
			  {"name":"lint","status":"completed","conclusion":"neutral","html_url":"https://ci.example/lint","output":{"title":""}},
			  {"name":"docs","status":"completed","conclusion":"skipped","html_url":"https://ci.example/docs","output":{"title":""}},
			  {"name":"flaky","status":"completed","conclusion":"cancelled","html_url":"https://ci.example/flaky","output":{"title":""}},
			  {"name":"slow","status":"completed","conclusion":"timed_out","html_url":"https://ci.example/slow","output":{"title":""}},
			  {"name":"gated","status":"completed","conclusion":"action_required","html_url":"https://ci.example/gated","output":{"title":""}},
			  {"name":"tests","status":"completed","conclusion":"failure","html_url":"https://ci.example/tests","output":{"title":"3 failed"}},
			  {"name":"weird","status":"completed","conclusion":"totally_unknown","html_url":"https://ci.example/weird","output":{"title":""}},
			  {"name":"deploy","status":"queued","conclusion":null,"html_url":"https://ci.example/deploy","output":{"title":""}},
			  {"name":"e2e","status":"in_progress","conclusion":null,"html_url":"https://ci.example/e2e","output":{"title":""}},
			  {"name":"ci","status":"completed","conclusion":"success","html_url":"https://checks.example/ci","output":{"title":"all good"}}
			]}`)
		case r.URL.Path == apiPrefix+"/commits/"+sha+"/status" && r.Method == http.MethodGet:
			gotStatusPath = r.URL.Path
			gotStatusAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"state":"success","statuses":[
			  {"context":"ci","state":"success","description":"external ci also ok","target_url":"https://external.example/ci"},
			  {"context":"external-success","state":"success","description":"","target_url":"https://external.example/success"},
			  {"context":"external-pending","state":"pending","description":"","target_url":"https://external.example/pending"},
			  {"context":"external-error","state":"error","description":"","target_url":"https://external.example/error"},
			  {"context":"external-failure","state":"failure","description":"","target_url":"https://external.example/failure"}
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
	if gotCheckRunsPath != apiPrefix+"/commits/"+sha+"/check-runs" {
		t.Errorf("check-runs request path = %q; want %q (must carry the head SHA)", gotCheckRunsPath, apiPrefix+"/commits/"+sha+"/check-runs")
	}
	if gotStatusPath != apiPrefix+"/commits/"+sha+"/status" {
		t.Errorf("status request path = %q; want %q (must carry the head SHA)", gotStatusPath, apiPrefix+"/commits/"+sha+"/status")
	}
	if gotCheckRunsAuth != "Bearer "+testToken {
		t.Errorf("check-runs Authorization = %q; want %q", gotCheckRunsAuth, "Bearer "+testToken)
	}
	if gotStatusAuth != "Bearer "+testToken {
		t.Errorf("status Authorization = %q; want %q", gotStatusAuth, "Bearer "+testToken)
	}

	want := []tracker.Check{
		// --- check-runs (first) ---
		{Name: "build", State: tracker.CheckSuccess, RawState: "success", Summary: "Build passed", URL: "https://ci.example/build"},
		{Name: "lint", State: tracker.CheckSuccess, RawState: "neutral", URL: "https://ci.example/lint"},
		{Name: "docs", State: tracker.CheckSuccess, RawState: "skipped", URL: "https://ci.example/docs"},
		{Name: "flaky", State: tracker.CheckFailure, RawState: "cancelled", URL: "https://ci.example/flaky"},
		{Name: "slow", State: tracker.CheckFailure, RawState: "timed_out", URL: "https://ci.example/slow"},
		{Name: "gated", State: tracker.CheckFailure, RawState: "action_required", URL: "https://ci.example/gated"},
		{Name: "tests", State: tracker.CheckFailure, RawState: "failure", Summary: "3 failed", URL: "https://ci.example/tests"},
		{Name: "weird", State: tracker.CheckFailure, RawState: "totally_unknown", URL: "https://ci.example/weird"},
		{Name: "deploy", State: tracker.CheckPending, RawState: "queued", URL: "https://ci.example/deploy"},
		{Name: "e2e", State: tracker.CheckPending, RawState: "in_progress", URL: "https://ci.example/e2e"},
		{Name: "ci", State: tracker.CheckSuccess, RawState: "success", Summary: "all good", URL: "https://checks.example/ci"},
		// --- statuses (second; "ci" duplicates the check-run above — no dedup) ---
		{Name: "ci", State: tracker.CheckSuccess, RawState: "success", Summary: "external ci also ok", URL: "https://external.example/ci"},
		{Name: "external-success", State: tracker.CheckSuccess, RawState: "success", URL: "https://external.example/success"},
		{Name: "external-pending", State: tracker.CheckPending, RawState: "pending", URL: "https://external.example/pending"},
		{Name: "external-error", State: tracker.CheckFailure, RawState: "error", URL: "https://external.example/error"},
		{Name: "external-failure", State: tracker.CheckFailure, RawState: "failure", URL: "https://external.example/failure"},
	}
	if len(checks) != len(want) {
		t.Fatalf("got %d checks; want %d: %+v", len(checks), len(want), checks)
	}
	for i := range want {
		if checks[i] != want[i] {
			t.Errorf("check[%d] = %+v; want %+v", i, checks[i], want[i])
		}
	}
	// The two "ci" rows (index 10, the check-run; index 11, the status) both
	// survive — the pinned no-dedup decision.
	if checks[10].Name != "ci" || checks[11].Name != "ci" || checks[10].URL == checks[11].URL {
		t.Fatalf("expected two distinct same-named \"ci\" rows (one per API), got %+v and %+v", checks[10], checks[11])
	}
}

// TestChecks_zeroRows: both CI endpoints answer zero rows (the combined
// status still reports "pending" — GitHub's own verdict, ignored) → a
// non-nil empty slice, nil error, and the client-side aggregate is
// ChecksNone, never trusting the forge's "pending" for a commit with nothing
// really pending on it (the same trap the Forgejo sibling documents).
func TestChecks_zeroRows(t *testing.T) {
	const sha = "0000000000000000000000000000000000000a"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == apiPrefix+"/pulls/9" && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"number":9,"state":"open","merged_at":null,"head":{"ref":"afk/9","sha":%q}}`, sha)
		case r.URL.Path == apiPrefix+"/commits/"+sha+"/check-runs" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"total_count":0,"check_runs":[]}`)
		case r.URL.Path == apiPrefix+"/commits/"+sha+"/status" && r.Method == http.MethodGet:
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
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})

	_, err := c.Checks(context.Background(), 999)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestChecks_rateLimited mirrors TestPull_rateLimited: a throttled call on
// the pull lookup unwraps to tracker.ErrRateLimited like every other GitHub
// client op.
func TestChecks_rateLimited(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1720000000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	})

	_, err := c.Checks(context.Background(), 72)
	if !errors.Is(err, tracker.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// --- CheckLog --------------------------------------------------------------

const (
	// checkLogSHA is the head commit every CheckLog fixture resolves pull 72 to,
	// and checkLogName is the check the tests ask for. The name deliberately
	// carries a space and parentheses: a check-run name is MATCHED against
	// listing rows, never escaped into a route, so nothing in the flow may start
	// quoting or splitting it.
	checkLogSHA  = "feedface0123456789abcdef0123456789abcd12"
	checkLogName = "build (ubuntu-latest)"

	// The join keys the fixtures share: the check run the name resolves to, a
	// second check-run id used as the decoy every wrong-join test points at, the
	// workflow run that owns the publishing job, and that job's id.
	checkLogCheckRunID      = 987654321
	checkLogOtherCheckRunID = 111222333
	checkLogRunID           = 424242
	checkLogJobID           = 55667788

	// The job's own run_attempt and its workflow run's are deliberately
	// DIFFERENT so the happy path proves Attempt is read off the JOB row, and
	// the id-join test proves the documented fallback to the run's.
	checkLogAttempt    = 3
	checkLogRunAttempt = 7

	// checkLogBody is what the blob host serves as the log.
	checkLogBody = "==> build (ubuntu-latest)\nstep 4/7 failed\nexit status 1\n"

	// checkLogSigMarker rides the query of the signed URL the job-log route
	// redirects to. That URL is itself a credential — anyone holding it can read
	// the log for as long as it lives — so it must never surface in an error;
	// making the marker distinctive is what gives
	// TestCheckLog_noCredentialLeakOnErrorPaths something real to assert on.
	checkLogSigMarker = "SIGNED-BLOB-SIGNATURE-xyz"
	checkLogSigQuery  = "?sig=" + checkLogSigMarker
)

// The default wire bodies of one CheckLog fixture, plus the variants more than
// one test needs. A test that varies a listing supplies its own JSON inline.
var (
	// checkLogCheckRunsBody: exactly one completed Actions check run carrying
	// the requested name — the happy-path Checks API page.
	checkLogCheckRunsBody = fmt.Sprintf(`{"total_count":1,"check_runs":[
	  {"id":%d,"name":%q,"status":"completed","conclusion":"failure","html_url":"https://github.com/octocat/hello-world/runs/%d","app":{"slug":"github-actions"},"output":{"title":"3 failed"}}
	]}`, checkLogCheckRunID, checkLogName, checkLogCheckRunID)

	// checkLogRunsBody: the one workflow run on the head SHA.
	checkLogRunsBody = fmt.Sprintf(`{"total_count":1,"workflow_runs":[{"id":%d,"run_attempt":%d}]}`,
		checkLogRunID, checkLogRunAttempt)

	// checkLogJobsBody: that run's one job, whose check_run_url's LAST SEGMENT
	// is the id the join runs on.
	checkLogJobsBody = fmt.Sprintf(`{"total_count":1,"jobs":[
	  {"id":%d,"name":%q,"run_attempt":%d,"check_run_url":"https://api.github.com/repos/octocat/hello-world/check-runs/%d"}
	]}`, checkLogJobID, checkLogName, checkLogAttempt, checkLogCheckRunID)

	// checkLogNoRunsBody: a head commit with no check runs at all, which is what
	// sends CheckLog on to the legacy combined-status surface.
	checkLogNoRunsBody = `{"total_count":0,"check_runs":[]}`

	// checkLogOtherStatusBody / checkLogExternalStatusBody: legacy
	// combined-status pages that do NOT and DO carry the requested context.
	checkLogOtherStatusBody = `{"state":"success","statuses":[
	  {"context":"external/other","state":"success","description":"","target_url":"https://external-ci.example/builds/1"}
	]}`
	checkLogExternalStatusBody = fmt.Sprintf(`{"state":"failure","statuses":[
	  {"context":%q,"state":"failure","description":"the external service ran it","target_url":"https://external-ci.example/builds/9"}
	]}`, checkLogName)

	// checkLogOtherAppBody / checkLogNoAppBody: the requested name on a check run
	// that is not the Actions app's — another app's, and one with no app object
	// at all.
	checkLogOtherAppBody = fmt.Sprintf(`{"total_count":1,"check_runs":[
	  {"id":%d,"name":%q,"status":"completed","conclusion":"failure","app":{"slug":"sonarcloud"},"output":{"title":""}}
	]}`, checkLogCheckRunID, checkLogName)
	checkLogNoAppBody = fmt.Sprintf(`{"total_count":1,"check_runs":[
	  {"id":%d,"name":%q,"status":"completed","conclusion":"failure","output":{"title":""}}
	]}`, checkLogCheckRunID, checkLogName)

	// checkLogAmbiguousBody: two check runs on the head sharing the requested
	// name, which the name alone cannot tell apart.
	checkLogAmbiguousBody = fmt.Sprintf(`{"total_count":2,"check_runs":[
	  {"id":%d,"name":%q,"status":"completed","conclusion":"failure","app":{"slug":"github-actions"},"output":{"title":""}},
	  {"id":%d,"name":%q,"status":"completed","conclusion":"success","app":{"slug":"github-actions"},"output":{"title":""}}
	]}`, checkLogCheckRunID, checkLogName, checkLogOtherCheckRunID, checkLogName)

	// checkLogStrayJobsBody: a jobs page whose only job points back at a
	// DIFFERENT check run, so the join walks the head and finds nothing.
	checkLogStrayJobsBody = fmt.Sprintf(`{"total_count":1,"jobs":[
	  {"id":%d,"name":%q,"run_attempt":%d,"check_run_url":"https://api.github.com/repos/octocat/hello-world/check-runs/%d"}
	]}`, checkLogJobID, checkLogName, checkLogAttempt, checkLogOtherCheckRunID)
)

func checkLogStatusPath() string { return apiPrefix + "/commits/" + checkLogSHA + "/status" }

func checkLogRunsPath() string { return apiPrefix + "/actions/runs" }

func checkLogJobsPath(runID int64) string {
	return apiPrefix + "/actions/runs/" + strconv.FormatInt(runID, 10) + "/jobs"
}

func checkLogLogsPath(jobID int64) string {
	return apiPrefix + "/actions/jobs/" + strconv.FormatInt(jobID, 10) + "/logs"
}

// staticJSON answers a listing that does not vary by request.
func staticJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }
}

// blobStatus answers the signed-URL host with one status and a short body — the
// forge-side failures the log classification has to tell apart.
func blobStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// checkLogReq is one request a CheckLog fixture host received: the path and
// query the client built, plus the three headers this seam cares about — the
// forge credential and the two GitHub-specific headers, which must ride every
// hop aimed at the API host and NO hop that leaves it.
type checkLogReq struct {
	path    string
	query   string
	auth    string
	accept  string
	version string
}

func recordCheckLogReq(r *http.Request) checkLogReq {
	return checkLogReq{
		path:    r.URL.Path,
		query:   r.URL.RawQuery,
		auth:    r.Header.Get("Authorization"),
		accept:  r.Header.Get("Accept"),
		version: r.Header.Get("X-GitHub-Api-Version"),
	}
}

// checkLogFixture varies one wire fixture off the happy-path shape: pull 72 on
// checkLogSHA, one Actions check run named checkLogName, one workflow run whose
// one job points back at it, and a job-log route answering GitHub's own
// redirect to a signed URL on the blob host. A zero field keeps that default —
// except statuses, where empty means the legacy combined-status endpoint is not
// part of this fixture and requesting it FAILS the test, which is how "the
// status read is skipped on the check-run path" stays pinned.
type checkLogFixture struct {
	checkRuns string                                                       // GET commits/{sha}/check-runs
	statuses  string                                                       // GET commits/{sha}/status
	runs      http.HandlerFunc                                             // GET actions/runs
	jobs      http.HandlerFunc                                             // GET actions/runs/{run_id}/jobs
	log       func(w http.ResponseWriter, r *http.Request, blobURL string) // GET actions/jobs/{job_id}/logs
	blob      http.HandlerFunc                                             // the signed-URL host
}

// checkLogHarness is the TWO hosts a GitHub job-log fetch spans: the API host,
// and a separate host standing in for the short-lived signed blob URL the
// Actions log route redirects to. Two genuinely different hosts is the only
// thing that makes the credential-stripping assertions real — a redirect back
// to the same server would prove nothing at all. Every request either host saw
// is recorded, in order.
type checkLogHarness struct {
	client   *Client
	blobURL  string // origin of the signed-log host (scheme://host, no path)
	apiReqs  []checkLogReq
	blobReqs []checkLogReq
}

func newCheckLogHarness(t *testing.T, fx checkLogFixture) *checkLogHarness {
	t.Helper()
	h := &checkLogHarness{}

	// The blob host starts FIRST so the API handler can name its URL in a
	// Location header.
	blobSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.blobReqs = append(h.blobReqs, recordCheckLogReq(r))
		if fx.blob != nil {
			fx.blob(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, checkLogBody)
	}))
	t.Cleanup(blobSrv.Close)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.apiReqs = append(h.apiReqs, recordCheckLogReq(r))
		isJobs := strings.HasPrefix(r.URL.Path, apiPrefix+"/actions/runs/") && strings.HasSuffix(r.URL.Path, "/jobs")
		isLog := strings.HasPrefix(r.URL.Path, apiPrefix+"/actions/jobs/") && strings.HasSuffix(r.URL.Path, "/logs")
		switch {
		case r.URL.Path == apiPrefix+"/pulls/72" && r.Method == http.MethodGet:
			_, _ = fmt.Fprintf(w, `{"number":72,"title":"feat: thing","state":"open","merged_at":null,
			  "head":{"ref":"afk/20","sha":%q},
			  "html_url":"https://github.com/octocat/hello-world/pull/72"}`, checkLogSHA)
		case r.URL.Path == apiPrefix+"/commits/"+checkLogSHA+"/check-runs" && r.Method == http.MethodGet:
			body := fx.checkRuns
			if body == "" {
				body = checkLogCheckRunsBody
			}
			_, _ = io.WriteString(w, body)
		case r.URL.Path == checkLogStatusPath() && r.Method == http.MethodGet:
			if fx.statuses == "" {
				t.Errorf("legacy status endpoint requested; this fixture matches a check run, and there that read can contribute nothing")
				http.Error(w, "unexpected", http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, fx.statuses)
		case r.URL.Path == checkLogRunsPath() && r.Method == http.MethodGet:
			if fx.runs != nil {
				fx.runs(w, r)
				return
			}
			_, _ = io.WriteString(w, checkLogRunsBody)
		case isJobs && r.Method == http.MethodGet:
			if fx.jobs != nil {
				fx.jobs(w, r)
				return
			}
			_, _ = io.WriteString(w, checkLogJobsBody)
		case isLog && r.Method == http.MethodGet:
			if fx.log != nil {
				fx.log(w, r, h.blobURL)
				return
			}
			// GitHub's own answer: a redirect to a short-lived signed URL on the
			// blob host. The job id rides into the blob path so a fixture can
			// serve a distinct body per job.
			job := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, apiPrefix+"/actions/jobs/"), "/logs")
			http.Redirect(w, r, h.blobURL+"/blobs/job-"+job+checkLogSigQuery, http.StatusFound)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(apiSrv.Close)

	h.blobURL = blobSrv.URL
	h.client = New(apiSrv.Client(), apiSrv.URL, testToken, "octocat", "hello-world")
	return h
}

// paths returns the API-host request paths, in request order.
func (h *checkLogHarness) paths() []string {
	out := make([]string, len(h.apiReqs))
	for i, r := range h.apiReqs {
		out[i] = r.path
	}
	return out
}

// requested reports whether the API host saw a request for path.
func (h *checkLogHarness) requested(path string) bool {
	for _, r := range h.apiReqs {
		if r.path == path {
			return true
		}
	}
	return false
}

// actionsRequested reports whether ANY Actions-API request was made — how the
// name-resolution failures pin that CheckLog gave up before spending the join
// walk, let alone fetching a log.
func (h *checkLogHarness) actionsRequested() bool {
	for _, r := range h.apiReqs {
		if strings.HasPrefix(r.path, apiPrefix+"/actions/") {
			return true
		}
	}
	return false
}

// TestCheckLog_happyPath pins the whole flow end to end: the fresh pull lookup
// for head.sha, the check-run resolution, the two Actions listings that join the
// matched check run to the job that published it, and the job-log route's
// redirect onto a signed URL on a DIFFERENT host. It asserts the exact ordered
// request sequence AND each hop's query — the head_sha and filter=latest bases
// are what keep the join scoped to this commit's latest attempt — that every API
// hop carried the Bearer credential while the blob hop carried none at all, that
// the served bytes and Attempt (the JOB row's run_attempt, not its run's) come
// back with no fallback recorded, and that the legacy combined-status endpoint
// was NEVER asked: on the check-run path it can contribute nothing and is pure
// cost.
func TestCheckLog_happyPath(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	if string(res.Log) != checkLogBody {
		t.Errorf("log = %q; want %q", res.Log, checkLogBody)
	}
	if res.Attempt != checkLogAttempt {
		t.Errorf("Attempt = %d; want %d (the job row's run_attempt)", res.Attempt, checkLogAttempt)
	}
	if res.FallbackFrom != 0 || res.FallbackStatus != 0 {
		t.Errorf("FallbackFrom/FallbackStatus = %d/%d; want 0/0 — this backend serves the latest attempt only, so it can never have fallen back",
			res.FallbackFrom, res.FallbackStatus)
	}

	wantPaths := []string{
		apiPrefix + "/pulls/72",
		apiPrefix + "/commits/" + checkLogSHA + "/check-runs",
		checkLogRunsPath(),
		checkLogJobsPath(checkLogRunID),
		checkLogLogsPath(checkLogJobID),
	}
	wantQueries := []string{
		"",
		"page=1&per_page=100",
		"head_sha=" + checkLogSHA + "&page=1&per_page=100",
		"filter=latest&page=1&per_page=100",
		"",
	}
	if got := h.paths(); len(got) != len(wantPaths) {
		t.Fatalf("API requests = %v; want %v", got, wantPaths)
	}
	for i := range wantPaths {
		if h.apiReqs[i].path != wantPaths[i] {
			t.Errorf("API request[%d] path = %q; want %q", i, h.apiReqs[i].path, wantPaths[i])
		}
		if h.apiReqs[i].query != wantQueries[i] {
			t.Errorf("API request[%d] (%s) query = %q; want %q", i, h.apiReqs[i].path, h.apiReqs[i].query, wantQueries[i])
		}
		if want := "Bearer " + testToken; h.apiReqs[i].auth != want {
			t.Errorf("API request[%d] (%s) Authorization = %q; want %q", i, h.apiReqs[i].path, h.apiReqs[i].auth, want)
		}
	}
	if len(h.blobReqs) != 1 {
		t.Fatalf("blob host saw %d requests; want exactly 1 (the followed redirect)", len(h.blobReqs))
	}
	if h.blobReqs[0].auth != "" {
		t.Errorf("blob hop Authorization = %q; want no header at all — the forge token must never reach the signed-URL host", h.blobReqs[0].auth)
	}
	if h.requested(checkLogStatusPath()) {
		t.Errorf("legacy status endpoint was requested (%v); want it never asked once a check run matched", h.paths())
	}
}

// TestCheckLog_unknownCheck: a name that no check run and no legacy status row
// carries is tracker.ErrUnknownCheck naming the context. It also pins WHERE the
// legacy surface is read — only here, after the check-run match came up empty,
// which is exactly the request the happy path must not make — and that no
// Actions request is spent on a name that resolves to nothing.
func TestCheckLog_unknownCheck(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{
		checkRuns: `{"total_count":1,"check_runs":[
		  {"id":5,"name":"some-other-check","status":"completed","conclusion":"success","app":{"slug":"github-actions"},"output":{"title":""}}
		]}`,
		statuses: checkLogOtherStatusBody,
	})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if !errors.Is(err, tracker.ErrUnknownCheck) {
		t.Fatalf("err = %v; want ErrUnknownCheck", err)
	}
	if !strings.Contains(err.Error(), checkLogName) {
		t.Errorf("error %q should name the requested check %q", err, checkLogName)
	}
	if !h.requested(checkLogStatusPath()) {
		t.Errorf("API requests = %v; want the legacy status endpoint read once no check run matched", h.paths())
	}
	if h.actionsRequested() {
		t.Errorf("API requests = %v; want no Actions request for a name that matches nothing", h.paths())
	}
}

// TestCheckLog_legacyCommitStatusIsUnsupported: the name matches only a legacy
// combined-status row — an external CI service's, many of which predate Checks —
// whose output GitHub never stored and so cannot serve. That is
// tracker.ErrUnsupported naming the check and its target_url ("not this forge's
// to serve"), never an adapter mismatch and never an empty success, and it costs
// no Actions request.
func TestCheckLog_legacyCommitStatusIsUnsupported(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{
		checkRuns: checkLogNoRunsBody,
		statuses:  checkLogExternalStatusBody,
	})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if !errors.Is(err, tracker.ErrUnsupported) {
		t.Fatalf("err = %v; want ErrUnsupported", err)
	}
	if errors.Is(err, tracker.ErrUnknownCheck) {
		t.Errorf("err = %v; want NOT ErrUnknownCheck — the check exists, its log is just not GitHub's to serve", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, checkLogName) || !strings.Contains(msg, "https://external-ci.example/builds/9") {
		t.Errorf("error %q should name the check and the external target_url the operator must go read", msg)
	}
	if h.actionsRequested() {
		t.Errorf("API requests = %v; want no Actions request for a legacy status row", h.paths())
	}
}

// TestCheckLog_nonActionsAppIsUnsupported: only the GitHub Actions app's check
// runs have a stored job log behind them. Another app's check run — or one with
// no app object at all — is a VERDICT, not a job, so there is nothing to fetch:
// tracker.ErrUnsupported naming the app slug (the absent case in words, not as
// an empty string the reader would have to interpret), decided before any
// Actions request is spent.
func TestCheckLog_nonActionsAppIsUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name      string
		checkRuns string
		wantSlug  string
	}{
		{name: "another GitHub App", checkRuns: checkLogOtherAppBody, wantSlug: `"sonarcloud"`},
		{name: "no app object at all", checkRuns: checkLogNoAppBody, wantSlug: `"(none)"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCheckLogHarness(t, checkLogFixture{checkRuns: tc.checkRuns})

			_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
			if !errors.Is(err, tracker.ErrUnsupported) {
				t.Fatalf("err = %v; want ErrUnsupported", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantSlug) || !strings.Contains(msg, checkLogName) {
				t.Errorf("error %q should name the check %q and the app slug %s", msg, checkLogName, tc.wantSlug)
			}
			if h.actionsRequested() {
				t.Errorf("API requests = %v; want no Actions request for a non-Actions check run", h.paths())
			}
		})
	}
}

// TestCheckLog_ambiguousNameIsUnsupported: two check runs on the head share the
// requested name, and the name is the only selector this seam has — nothing
// tells lab which log was meant. Serving either is the silent wrong-log serve
// this method refuses to risk, so it fails loud with tracker.ErrUnsupported
// naming BOTH check-run ids (the operator's only way to tell the two apart) and
// spends no Actions request.
func TestCheckLog_ambiguousNameIsUnsupported(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{checkRuns: checkLogAmbiguousBody})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if !errors.Is(err, tracker.ErrUnsupported) {
		t.Fatalf("err = %v; want ErrUnsupported", err)
	}
	msg := err.Error()
	for _, id := range []string{strconv.Itoa(checkLogCheckRunID), strconv.Itoa(checkLogOtherCheckRunID)} {
		if !strings.Contains(msg, id) {
			t.Errorf("error %q should name check-run id %s — both ambiguous rows must be identified", msg, id)
		}
	}
	if !strings.Contains(msg, checkLogName) {
		t.Errorf("error %q should name the ambiguous check %q", msg, checkLogName)
	}
	if h.actionsRequested() {
		t.Errorf("API requests = %v; want no Actions request once the name is known to be ambiguous", h.paths())
	}
}

// TestCheckLog_joinsOnCheckRunIDNotJobName is the issue's central "no silent
// wrong-log serve" constraint. Two workflow runs sit on the head: the one walked
// FIRST owns a job whose NAME is exactly the requested check name but whose
// check_run_url points at a different check run, and the one walked SECOND owns
// the job that actually published the matched check run, under an unrelated
// name. A name-matching join would stop at the first job and hand the operator
// another workflow's log with nothing to hint anything went wrong, so the log
// served here MUST be the second run's job's. The second job row also omits
// run_attempt, pinning the documented fallback to its parent workflow run's.
func TestCheckLog_joinsOnCheckRunIDNotJobName(t *testing.T) {
	const (
		decoyRunID    = 111
		decoyJobID    = 700
		matchedRunID  = 222
		matchedJobID  = 800
		matchedAttemp = 4
	)
	h := newCheckLogHarness(t, checkLogFixture{
		runs: func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"total_count":2,"workflow_runs":[
			  {"id":%d,"run_attempt":1},
			  {"id":%d,"run_attempt":%d}
			]}`, decoyRunID, matchedRunID, matchedAttemp)
		},
		jobs: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case checkLogJobsPath(decoyRunID):
				// The requested NAME exactly, a DIFFERENT check-run id.
				_, _ = fmt.Fprintf(w, `{"total_count":1,"jobs":[
				  {"id":%d,"name":%q,"run_attempt":1,"check_run_url":"https://api.github.com/repos/octocat/hello-world/check-runs/%d"}
				]}`, decoyJobID, checkLogName, checkLogOtherCheckRunID)
			case checkLogJobsPath(matchedRunID):
				// An unrelated name, the MATCHING check-run id, and no run_attempt.
				_, _ = fmt.Fprintf(w, `{"total_count":1,"jobs":[
				  {"id":%d,"name":"some other workflow's job","check_run_url":"https://api.github.com/repos/octocat/hello-world/check-runs/%d"}
				]}`, matchedJobID, checkLogCheckRunID)
			default:
				t.Errorf("unexpected jobs request %s", r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}
		},
		blob: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			if !strings.Contains(r.URL.Path, strconv.Itoa(matchedJobID)) {
				t.Errorf("log fetched from blob path %q; want job %d's — the join matched on the job NAME, not the check-run id", r.URL.Path, matchedJobID)
				_, _ = io.WriteString(w, "wrong-job-log")
				return
			}
			_, _ = io.WriteString(w, "matched-job-log")
		},
	})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	if string(res.Log) != "matched-job-log" {
		t.Errorf("log = %q; want %q — the job whose check_run_url carries the matched check-run id", res.Log, "matched-job-log")
	}
	if res.Attempt != matchedAttemp {
		t.Errorf("Attempt = %d; want %d (the job row carries none, so the parent workflow run's stands in)", res.Attempt, matchedAttemp)
	}
	if h.requested(checkLogLogsPath(decoyJobID)) {
		t.Fatalf("the same-named decoy job's log route was requested (%v); the join must be on the check-run id alone", h.paths())
	}
	if !h.requested(checkLogLogsPath(matchedJobID)) {
		t.Errorf("API requests = %v; want the log route of job %d", h.paths(), matchedJobID)
	}
}

// TestCheckLog_noJobPointsBackIsAdapterMismatch: the check run says GitHub
// Actions, yet no job on the head points back at it — what a drifted join looks
// like. That is tracker.ErrLogAdapterMismatch (NOT ErrUnsupported, which would
// tell the operator this forge cannot serve such logs, and NOT ErrNotFound,
// which on this seam means "no such pull request"), the error counts what was
// walked so the reader sees the search was exhaustive, and no log is fetched.
func TestCheckLog_noJobPointsBackIsAdapterMismatch(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{jobs: staticJSON(checkLogStrayJobsBody)})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if !errors.Is(err, tracker.ErrLogAdapterMismatch) {
		t.Fatalf("err = %v; want ErrLogAdapterMismatch", err)
	}
	if errors.Is(err, tracker.ErrUnsupported) || errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("err = %v; want neither ErrUnsupported nor ErrNotFound — an exhausted join is an adapter surprise, not a missing pull or an unsupported forge", err)
	}
	if msg := err.Error(); !strings.Contains(msg, strconv.Itoa(checkLogCheckRunID)) || !strings.Contains(msg, checkLogName) {
		t.Errorf("error %q should name the check %q and check-run id %d", msg, checkLogName, checkLogCheckRunID)
	}
	for _, req := range h.apiReqs {
		if strings.HasSuffix(req.path, "/logs") {
			t.Errorf("log route %q was requested; want the join to fail before any log is fetched", req.path)
		}
	}
}

// TestCheckLog_inFlightJobServesPartialLog is a contract pin, not an accident of
// the route: a still-running job's log route answers 200 with whatever it has
// captured so far, and lab returns those partial bytes with a NIL error. "Not
// finished yet" is never an error on this seam — the fix-the-red loop reads a
// running job's output while it runs.
func TestCheckLog_inFlightJobServesPartialLog(t *testing.T) {
	const partial = "==> build (ubuntu-latest)\nstep 2/7 running\n"
	h := newCheckLogHarness(t, checkLogFixture{
		checkRuns: fmt.Sprintf(`{"total_count":1,"check_runs":[
		  {"id":%d,"name":%q,"status":"in_progress","conclusion":null,"app":{"slug":"github-actions"},"output":{"title":""}}
		]}`, checkLogCheckRunID, checkLogName),
		blob: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, partial)
		},
	})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if err != nil {
		t.Fatalf("CheckLog on an in-flight job: %v; want the partial log and a nil error", err)
	}
	if string(res.Log) != partial {
		t.Errorf("log = %q; want the partial body %q", res.Log, partial)
	}
	if res.Attempt != checkLogAttempt {
		t.Errorf("Attempt = %d; want %d", res.Attempt, checkLogAttempt)
	}
}

// TestCheckLog_foreignRedirectDropsGitHubHeaders: the hop that leaves the API
// host carries the forge token nowhere — and neither the vnd.github Accept nor
// the pinned X-GitHub-Api-Version, which belong to the API and to nothing else.
// The API hop that produced the redirect is asserted to have carried all three,
// so the test cannot pass by dropping headers everywhere.
func TestCheckLog_foreignRedirectDropsGitHubHeaders(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{})

	if _, err := h.client.CheckLog(context.Background(), 72, checkLogName); err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	logHop := h.apiReqs[len(h.apiReqs)-1]
	if logHop.path != checkLogLogsPath(checkLogJobID) {
		t.Fatalf("last API request = %q; want the job-log route %q", logHop.path, checkLogLogsPath(checkLogJobID))
	}
	if logHop.auth != "Bearer "+testToken || logHop.accept != "application/vnd.github+json" || logHop.version != apiVersion {
		t.Fatalf("API log hop headers = auth %q, accept %q, version %q; want the full GitHub trio",
			logHop.auth, logHop.accept, logHop.version)
	}
	if len(h.blobReqs) != 1 {
		t.Fatalf("blob host saw %d requests; want exactly 1", len(h.blobReqs))
	}
	blobHop := h.blobReqs[0]
	if blobHop.auth != "" || blobHop.accept != "" || blobHop.version != "" {
		t.Errorf("blob hop headers = auth %q, accept %q, version %q; want all three absent — the hop left the API host",
			blobHop.auth, blobHop.accept, blobHop.version)
	}
	if !strings.Contains(blobHop.query, checkLogSigMarker) {
		t.Errorf("blob hop query = %q; want the signed URL's query preserved verbatim", blobHop.query)
	}
}

// TestCheckLog_sameHostRedirectKeepsCredential is the other half of the rule: a
// redirect that stays on the API host is still the same authenticated call, so
// the credential must survive it — dropping headers on EVERY hop would break a
// GHE deployment that bounces the log route internally. The Location here is
// path-relative, which is legal and must resolve against the current request.
func TestCheckLog_sameHostRedirectKeepsCredential(t *testing.T) {
	const innerPath = apiPrefix + "/actions/jobs/55667788/logs-blob"
	var innerAuth string
	var innerSeen bool
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/pulls/72":
			_, _ = fmt.Fprintf(w, `{"number":72,"state":"open","merged_at":null,"head":{"ref":"afk/20","sha":%q}}`, checkLogSHA)
		case apiPrefix + "/commits/" + checkLogSHA + "/check-runs":
			_, _ = io.WriteString(w, checkLogCheckRunsBody)
		case checkLogRunsPath():
			_, _ = io.WriteString(w, checkLogRunsBody)
		case checkLogJobsPath(checkLogRunID):
			_, _ = io.WriteString(w, checkLogJobsBody)
		case checkLogLogsPath(checkLogJobID):
			http.Redirect(w, r, innerPath+checkLogSigQuery, http.StatusFound)
		case innerPath:
			innerSeen = true
			innerAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, checkLogBody)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})

	res, err := c.CheckLog(context.Background(), 72, checkLogName)
	if err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	if !innerSeen {
		t.Fatal("the same-host redirect target was never requested")
	}
	if want := "Bearer " + testToken; innerAuth != want {
		t.Errorf("same-host redirect Authorization = %q; want %q — the hop never left the API host", innerAuth, want)
	}
	if string(res.Log) != checkLogBody {
		t.Errorf("log = %q; want %q", res.Log, checkLogBody)
	}
}

// TestCheckLog_redirectWithoutLocationIsAdapterMismatch: a 3xx with nothing to
// follow is a shape lab's adapter cannot act on — a loud
// tracker.ErrLogAdapterMismatch naming the API route and the status, never a
// silent empty log.
func TestCheckLog_redirectWithoutLocationIsAdapterMismatch(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{
		log: func(w http.ResponseWriter, r *http.Request, _ string) {
			w.WriteHeader(http.StatusFound) // 302, no Location header at all
		},
	})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if !errors.Is(err, tracker.ErrLogAdapterMismatch) {
		t.Fatalf("err = %v; want ErrLogAdapterMismatch", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, checkLogLogsPath(checkLogJobID)) || !strings.Contains(msg, "302") {
		t.Errorf("error %q should name the API log route and the status it answered", msg)
	}
	if len(h.blobReqs) != 0 {
		t.Errorf("blob host saw %d requests; want none — there was no Location to follow", len(h.blobReqs))
	}
}

// TestCheckLog_redirectLoopIsAdapterMismatch: a chain that keeps redirecting
// ends after maxLogRedirects hops as a loud tracker.ErrLogAdapterMismatch — the
// adapter bounds the walk instead of looping forever, and the error names the
// cap and the API route while the signed URL stays unmentioned.
func TestCheckLog_redirectLoopIsAdapterMismatch(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{
		blob: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/blobs/again"+checkLogSigQuery, http.StatusFound)
		},
	})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if !errors.Is(err, tracker.ErrLogAdapterMismatch) {
		t.Fatalf("err = %v; want ErrLogAdapterMismatch", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, strconv.Itoa(maxLogRedirects)) || !strings.Contains(msg, checkLogLogsPath(checkLogJobID)) {
		t.Errorf("error %q should name the hop cap and the API log route", msg)
	}
	if strings.Contains(msg, checkLogSigMarker) {
		t.Fatalf("signed blob URL leaked into error: %q", msg)
	}
	if len(h.blobReqs) != maxLogRedirects {
		t.Errorf("blob host saw %d requests; want %d (the hop cap)", len(h.blobReqs), maxLogRedirects)
	}
}

// TestCheckLog_mediaTypesAt200: the blob host — not GitHub's API — sets the
// Content-Type on the served log, and it legitimately says text/plain, says
// application/octet-stream, or omits the header entirely; all three are the log.
// text/html is not: a body lab hands an agent as "the log" must never be a login
// or error page, so that is tracker.ErrLogAdapterMismatch naming the media type
// it refused.
func TestCheckLog_mediaTypesAt200(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		wantErr     bool
	}{
		{name: "text/plain", contentType: "text/plain; charset=utf-8"},
		{name: "application/octet-stream", contentType: "application/octet-stream"},
		{name: "absent Content-Type", contentType: ""},
		{name: "text/html is not a log blob", contentType: "text/html; charset=utf-8", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCheckLogHarness(t, checkLogFixture{
				blob: func(w http.ResponseWriter, r *http.Request) {
					if tc.contentType == "" {
						// nil (not "") suppresses net/http's content sniffing, which
						// would otherwise invent a header the real blob host omits.
						w.Header()["Content-Type"] = nil
					} else {
						w.Header().Set("Content-Type", tc.contentType)
					}
					_, _ = io.WriteString(w, checkLogBody)
				},
			})

			res, err := h.client.CheckLog(context.Background(), 72, checkLogName)
			if tc.wantErr {
				if !errors.Is(err, tracker.ErrLogAdapterMismatch) {
					t.Fatalf("err = %v; want ErrLogAdapterMismatch", err)
				}
				if !strings.Contains(err.Error(), "text/html") {
					t.Errorf("error %q should name the media type it refused", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckLog: %v", err)
			}
			if string(res.Log) != checkLogBody {
				t.Errorf("log = %q; want %q", res.Log, checkLogBody)
			}
		})
	}
}

// TestCheckLog_upstream5xxIsLogUpstreamNotMismatch pins issue #259's attribution
// distinction on this backend: the forge failed to serve a log lab asked for
// correctly, so the reader should retry the forge — NOT debug lab's adapter.
// Both directions are asserted, because a single errors.Is would pass on an
// error that wrapped both.
func TestCheckLog_upstream5xxIsLogUpstreamNotMismatch(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{
		blob: blobStatus(http.StatusBadGateway, "upstream storage unavailable"),
	})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if !errors.Is(err, tracker.ErrLogUpstream) {
		t.Fatalf("err = %v; want ErrLogUpstream", err)
	}
	if errors.Is(err, tracker.ErrLogAdapterMismatch) {
		t.Errorf("err = %v; want NOT ErrLogAdapterMismatch — a forge-side 5xx must not read as lab's adapter having drifted", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "502") || !strings.Contains(msg, checkLogLogsPath(checkLogJobID)) {
		t.Errorf("error %q should name the status and the API log route", msg)
	}
}

// TestCheckLog_logRoute404IsAdapterMismatchNotNotFound: an expired or absent log
// blob must NOT masquerade as tracker.ErrNotFound, which on this seam means "no
// such pull request" — the operator would go looking for the wrong thing. It is
// tracker.ErrLogAdapterMismatch with an honest dual-cause message: most often
// the blob simply expired (or the job never started), and only maybe is it lab's
// adapter having drifted.
func TestCheckLog_logRoute404IsAdapterMismatchNotNotFound(t *testing.T) {
	h := newCheckLogHarness(t, checkLogFixture{
		blob: blobStatus(http.StatusNotFound, "not found"),
	})

	_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if !errors.Is(err, tracker.ErrLogAdapterMismatch) {
		t.Fatalf("err = %v; want ErrLogAdapterMismatch", err)
	}
	if errors.Is(err, tracker.ErrNotFound) {
		t.Errorf("err = %v; want NOT ErrNotFound — on this seam that means the PULL does not exist", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "expire") || !strings.Contains(msg, "adapter") {
		t.Errorf("error %q should name BOTH causes: an expired/absent log blob, and a possibly drifted adapter", msg)
	}
}

// TestCheckLog_rateLimited: a throttled call unwraps to tracker.ErrRateLimited
// wherever the throttle lands — on the opening pull lookup like every other op
// on this client, and on the job-log route, which does its own transport and so
// has to classify a 403 the same way rather than calling it an adapter mismatch.
func TestCheckLog_rateLimited(t *testing.T) {
	t.Run("on the pull lookup", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1720000000")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
		})

		_, err := c.CheckLog(context.Background(), 72, checkLogName)
		if !errors.Is(err, tracker.ErrRateLimited) {
			t.Fatalf("err = %v; want ErrRateLimited", err)
		}
	})

	t.Run("on the log route", func(t *testing.T) {
		h := newCheckLogHarness(t, checkLogFixture{
			log: func(w http.ResponseWriter, r *http.Request, _ string) {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", "1720000000")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
			},
		})

		_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
		if !errors.Is(err, tracker.ErrRateLimited) {
			t.Fatalf("err = %v; want ErrRateLimited", err)
		}
		if errors.Is(err, tracker.ErrLogAdapterMismatch) {
			t.Errorf("err = %v; want NOT ErrLogAdapterMismatch — a throttle is a wait, not a drifted adapter", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "1720000000") {
			t.Errorf("error %q should carry the reset hint", msg)
		}
		if strings.Contains(msg, testToken) {
			t.Fatalf("token leaked into error: %q", msg)
		}
	})
}

// TestCheckLog_unknownPullIsNotFound: CheckLog opens exactly as Checks does, so
// an unknown number fails right there on the pull lookup with
// tracker.ErrNotFound — the seam's "no such pull request" — and no log request
// is attempted.
func TestCheckLog_unknownPullIsNotFound(t *testing.T) {
	var paths []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})

	_, err := c.CheckLog(context.Background(), 999, checkLogName)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
	if len(paths) != 1 || paths[0] != apiPrefix+"/pulls/999" {
		t.Errorf("requests = %v; want only the pull lookup %q", paths, apiPrefix+"/pulls/999")
	}
}

// TestCheckLog_actionsListingsPaginate: both Actions listings are ordinary
// paginated GitHub lists, so the join walks EVERY page — a match on page 2 of a
// run's jobs must be found, not silently missed — and the base query each
// listing rides on (head_sha for the runs, filter=latest for the jobs) has to
// survive onto page 2 as well: a page-2 request that dropped head_sha would list
// the whole repo's runs and a dropped filter=latest would resurrect stale
// attempts' jobs.
func TestCheckLog_actionsListingsPaginate(t *testing.T) {
	const (
		firstRunID  = 111
		secondRunID = 222
	)
	h := newCheckLogHarness(t, checkLogFixture{
		runs: func(w http.ResponseWriter, r *http.Request) {
			switch page := r.URL.Query().Get("page"); page {
			case "1":
				setNextLink(w, r)
				_, _ = fmt.Fprintf(w, `{"total_count":2,"workflow_runs":[{"id":%d,"run_attempt":1}]}`, firstRunID)
			case "2":
				_, _ = fmt.Fprintf(w, `{"total_count":2,"workflow_runs":[{"id":%d,"run_attempt":2}]}`, secondRunID)
			default:
				t.Errorf("unexpected workflow-runs page %q (should have stopped)", page)
				_, _ = io.WriteString(w, `{"total_count":0,"workflow_runs":[]}`)
			}
		},
		jobs: func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			switch {
			case r.URL.Path == checkLogJobsPath(firstRunID) && page == "1":
				_, _ = fmt.Fprintf(w, `{"total_count":1,"jobs":[
				  {"id":701,"name":"unrelated","run_attempt":1,"check_run_url":"https://api.github.com/repos/octocat/hello-world/check-runs/%d"}
				]}`, checkLogOtherCheckRunID)
			case r.URL.Path == checkLogJobsPath(secondRunID) && page == "1":
				setNextLink(w, r)
				_, _ = fmt.Fprintf(w, `{"total_count":2,"jobs":[
				  {"id":702,"name":"also unrelated","run_attempt":%d,"check_run_url":"https://api.github.com/repos/octocat/hello-world/check-runs/%d"}
				]}`, checkLogAttempt, checkLogOtherCheckRunID)
			case r.URL.Path == checkLogJobsPath(secondRunID) && page == "2":
				_, _ = io.WriteString(w, checkLogJobsBody) // the match, on the LAST page
			default:
				t.Errorf("unexpected jobs request %s?%s", r.URL.Path, r.URL.RawQuery)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}
		},
	})

	res, err := h.client.CheckLog(context.Background(), 72, checkLogName)
	if err != nil {
		t.Fatalf("CheckLog: %v", err)
	}
	if string(res.Log) != checkLogBody {
		t.Errorf("log = %q; want %q (the match sits on the last page of the last run's jobs)", res.Log, checkLogBody)
	}

	var gotRuns, gotJobs []string
	for _, req := range h.apiReqs {
		switch {
		case req.path == checkLogRunsPath():
			gotRuns = append(gotRuns, req.query)
		case strings.HasSuffix(req.path, "/jobs"):
			gotJobs = append(gotJobs, req.path+"?"+req.query)
		}
	}
	wantRuns := []string{
		"head_sha=" + checkLogSHA + "&page=1&per_page=100",
		"head_sha=" + checkLogSHA + "&page=2&per_page=100",
	}
	wantJobs := []string{
		checkLogJobsPath(firstRunID) + "?filter=latest&page=1&per_page=100",
		checkLogJobsPath(secondRunID) + "?filter=latest&page=1&per_page=100",
		checkLogJobsPath(secondRunID) + "?filter=latest&page=2&per_page=100",
	}
	if len(gotRuns) != len(wantRuns) {
		t.Fatalf("workflow-runs requests = %v; want %v", gotRuns, wantRuns)
	}
	for i := range wantRuns {
		if gotRuns[i] != wantRuns[i] {
			t.Errorf("workflow-runs request[%d] query = %q; want %q (the head_sha base must survive onto page 2)", i, gotRuns[i], wantRuns[i])
		}
	}
	if len(gotJobs) != len(wantJobs) {
		t.Fatalf("jobs requests = %v; want %v", gotJobs, wantJobs)
	}
	for i := range wantJobs {
		if gotJobs[i] != wantJobs[i] {
			t.Errorf("jobs request[%d] = %q; want %q (the filter=latest base must survive onto page 2)", i, gotJobs[i], wantJobs[i])
		}
	}
}

// TestCheckLog_noCredentialLeakOnErrorPaths sweeps every way this call can fail
// and asserts two things about the returned message: the forge token never
// appears (it lives only in a request header), and neither does the signed blob
// URL — which is itself a credential, so naming it in an error, a log line, or
// anywhere else would hand out read access to the log for as long as it lives.
// The fixture's redirect target carries a distinctive signature marker so the
// assertion has something real to catch.
func TestCheckLog_noCredentialLeakOnErrorPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		fx   checkLogFixture
	}{
		{name: "unknown check", fx: checkLogFixture{checkRuns: checkLogNoRunsBody, statuses: checkLogOtherStatusBody}},
		{name: "legacy commit status", fx: checkLogFixture{checkRuns: checkLogNoRunsBody, statuses: checkLogExternalStatusBody}},
		{name: "non-Actions app", fx: checkLogFixture{checkRuns: checkLogOtherAppBody}},
		{name: "ambiguous name", fx: checkLogFixture{checkRuns: checkLogAmbiguousBody}},
		{name: "join miss", fx: checkLogFixture{jobs: staticJSON(checkLogStrayJobsBody)}},
		{name: "log route 5xx", fx: checkLogFixture{blob: blobStatus(http.StatusInternalServerError, "boom")}},
		{name: "log route 404", fx: checkLogFixture{blob: blobStatus(http.StatusNotFound, "gone")}},
		{name: "log route 418", fx: checkLogFixture{blob: blobStatus(http.StatusTeapot, "surprise")}},
		{
			name: "log route throttled",
			fx: checkLogFixture{log: func(w http.ResponseWriter, r *http.Request, _ string) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"message":"secondary rate limit"}`)
			}},
		},
		{
			name: "200 text/html",
			fx: checkLogFixture{blob: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, "<html><body>sign in</body></html>")
			}},
		},
		{
			name: "redirect without Location",
			fx: checkLogFixture{log: func(w http.ResponseWriter, r *http.Request, _ string) {
				w.WriteHeader(http.StatusFound)
			}},
		},
		{
			name: "redirect loop",
			fx: checkLogFixture{blob: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/blobs/again"+checkLogSigQuery, http.StatusFound)
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCheckLogHarness(t, tc.fx)

			_, err := h.client.CheckLog(context.Background(), 72, checkLogName)
			if err == nil {
				t.Fatal("expected an error from this fixture")
			}
			msg := err.Error()
			if strings.Contains(msg, testToken) {
				t.Errorf("forge token leaked into error: %q", msg)
			}
			if strings.Contains(msg, checkLogSigMarker) {
				t.Errorf("signed blob URL leaked into error: %q", msg)
			}
			if strings.Contains(msg, h.blobURL) {
				t.Errorf("signed-URL host leaked into error: %q", msg)
			}
		})
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
		_, _ = io.WriteString(w, `{"number":100,"state":"open","merged_at":null,"head":{"ref":"afk/63"},
		  "html_url":"https://github.com/octocat/hello-world/pull/100"}`)
	})

	ref, err := c.CreatePull(context.Background(), "afk/63", "main", "Fix thing", "Closes #63")
	if err != nil {
		t.Fatalf("CreatePull: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != apiPrefix+"/pulls" {
		t.Errorf("request = %s %s; want POST %s/pulls", gotMethod, gotPath, apiPrefix)
	}
	for k, want := range map[string]string{"head": "afk/63", "base": "main", "title": "Fix thing", "body": "Closes #63"} {
		if gotBody[k] != want {
			t.Errorf("request body %s = %v; want %q", k, gotBody[k], want)
		}
	}
	want := tracker.PullRef{Number: 100, HeadBranch: "afk/63", State: "open", URL: "https://github.com/octocat/hello-world/pull/100"}
	if ref != want {
		t.Errorf("returned PullRef = %+v; want %+v", ref, want)
	}
}

// --- MergePull -------------------------------------------------------------

// TestMergePull_success drives the open→merge path: GET the pull, then PUT the
// fixed "merge" method to /pulls/{n}/merge; the returned ref reports merged.
func TestMergePull_success(t *testing.T) {
	var mergeMethod, mergePath string
	var mergeBody map[string]any
	putCalled := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"number":42,"state":"open","merged_at":null,"head":{"ref":"afk/42"},
			  "html_url":"https://github.com/octocat/hello-world/pull/42"}`)
		case http.MethodPut:
			putCalled = true
			mergeMethod, mergePath = r.Method, r.URL.Path
			mergeBody = decodeBody(t, r)
			_, _ = io.WriteString(w, `{"sha":"abc123","merged":true,"message":"Pull Request successfully merged"}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	ref, err := c.MergePull(context.Background(), 42)
	if err != nil {
		t.Fatalf("MergePull: %v", err)
	}
	if !putCalled {
		t.Fatal("MergePull did not PUT a merge")
	}
	if mergeMethod != http.MethodPut || mergePath != apiPrefix+"/pulls/42/merge" {
		t.Errorf("merge request = %s %s; want PUT %s/pulls/42/merge", mergeMethod, mergePath, apiPrefix)
	}
	if mergeBody["merge_method"] != "merge" {
		t.Errorf("merge_method = %v; want the fixed \"merge\"", mergeBody["merge_method"])
	}
	// The head branch must survive the merge (acceptance criterion 3): the
	// merge request carries ONLY merge_method — no branch-delete flag.
	if len(mergeBody) != 1 {
		t.Errorf("merge body = %v; want only merge_method (no branch-delete flag)", mergeBody)
	}
	want := tracker.PullRef{Number: 42, HeadBranch: "afk/42", State: tracker.PullMerged, URL: "https://github.com/octocat/hello-world/pull/42"}
	if ref != want {
		t.Errorf("PullRef = %+v; want %+v", ref, want)
	}
}

// TestMergePull_alreadyMergedIsConvergentNoOp: a pull GitHub already reports
// merged (merged_at set) is a no-op success — no merge PUT.
func TestMergePull_alreadyMergedIsConvergentNoOp(t *testing.T) {
	putCalled := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putCalled = true
		}
		_, _ = io.WriteString(w, `{"number":42,"state":"closed","merged_at":"2026-07-08T00:00:00Z","head":{"ref":"afk/42"},
		  "html_url":"https://github.com/octocat/hello-world/pull/42"}`)
	})

	ref, err := c.MergePull(context.Background(), 42)
	if err != nil {
		t.Fatalf("MergePull: %v", err)
	}
	if putCalled {
		t.Fatal("MergePull PUT a merge for an already-merged pull; want a convergent no-op")
	}
	if ref.State != tracker.PullMerged {
		t.Errorf("state = %s; want merged", ref.State)
	}
}

// TestMergePull_rejectedSurfacesGitHubWordsVerbatim: a 405 refusal becomes
// tracker.ErrMergeRejected carrying GitHub's own words — and never the token.
func TestMergePull_rejectedSurfacesGitHubWordsVerbatim(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"number":42,"state":"open","merged_at":null,"head":{"ref":"afk/42"}}`)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"message":"Required status check \"ci\" is expected."}`)
	})

	_, err := c.MergePull(context.Background(), 42)
	if !errors.Is(err, tracker.ErrMergeRejected) {
		t.Fatalf("err = %v, want ErrMergeRejected", err)
	}
	if !strings.Contains(err.Error(), "Required status check") {
		t.Fatalf("err = %q, want GitHub's own words verbatim", err.Error())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestMergePull_notFound: an unknown number's 404 unwraps to ErrNotFound.
func TestMergePull_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})

	_, err := c.MergePull(context.Background(), 999)
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestMergePull_rateLimitedNotRejected: a throttled merge PUT stays
// tracker.ErrRateLimited (a transient upstream throttle), NOT ErrMergeRejected
// (a permanent refusal) — GitHub's secondary limits target mutating requests,
// so the merge PUT is exactly where this bites; the agent must retry, not
// treat a mergeable PR as refused.
func TestMergePull_rateLimitedNotRejected(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"number":42,"state":"open","merged_at":null,"head":{"ref":"afk/42"}}`)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1783500000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit"}`)
	})

	_, err := c.MergePull(context.Background(), 42)
	if !errors.Is(err, tracker.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited (a throttle, not a refusal)", err)
	}
	if errors.Is(err, tracker.ErrMergeRejected) {
		t.Fatalf("throttle mislabeled as a merge refusal: %v", err)
	}
}

// --- Reviews ---------------------------------------------------------------

// TestReviews_stateMappingAndDismissed pins the read: GitHub review states map
// onto lab's Review* vocabulary, DISMISSED sets Dismissed=true with state
// "dismissed", and an unknown state passes through lowercased.
func TestReviews_stateMappingAndDismissed(t *testing.T) {
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `[
		  {"user":{"login":"alice"},"state":"APPROVED","body":"lgtm"},
		  {"user":{"login":"bob"},"state":"CHANGES_REQUESTED","body":"fix this"},
		  {"user":{"login":"carol"},"state":"COMMENTED","body":"nit"},
		  {"user":{"login":"dave"},"state":"DISMISSED","body":"was blocking"},
		  {"user":{"login":"erin"},"state":"PENDING","body":""},
		  {"user":{"login":"frank"},"state":"WEIRD","body":"?"}
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
		{Reviewer: "dave", State: "dismissed", Body: "was blocking", Dismissed: true},
		{Reviewer: "erin", State: "pending"},
		{Reviewer: "frank", State: "weird", Body: "?"},
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
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	if _, err := c.Reviews(context.Background(), 999); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// --- RerequestReview -------------------------------------------------------

// TestRerequestReview_postsChangesRequestedReviewers pins the latest-per-
// reviewer reduction: alice's latest is an approval (drops out), carol's
// changes-request is DISMISSED (skipped), dave's changes-request is followed by
// a PENDING that is skipped (so dave still counts). The POST carries exactly
// [bob, dave] in first-seen order.
func TestRerequestReview_postsChangesRequestedReviewers(t *testing.T) {
	var gotReqMethod, gotReqPath string
	var gotReqBody map[string]any
	postCalled := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = io.WriteString(w, `[
			  {"user":{"login":"alice"},"state":"CHANGES_REQUESTED"},
			  {"user":{"login":"bob"},"state":"CHANGES_REQUESTED"},
			  {"user":{"login":"alice"},"state":"APPROVED"},
			  {"user":{"login":"carol"},"state":"DISMISSED"},
			  {"user":{"login":"dave"},"state":"CHANGES_REQUESTED"},
			  {"user":{"login":"dave"},"state":"PENDING"}
			]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
			postCalled = true
			gotReqMethod, gotReqPath = r.Method, r.URL.Path
			gotReqBody = decodeBody(t, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
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

// TestRerequestReview_noChangesRequestedIsNoOp: with nobody currently
// requesting changes, the re-request is a convergent no-op — no POST, nil error.
func TestRerequestReview_noChangesRequestedIsNoOp(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Fatalf("unexpected POST %s; want a no-op", r.URL.Path)
		}
		_, _ = io.WriteString(w, `[
		  {"user":{"login":"alice"},"state":"APPROVED"},
		  {"user":{"login":"bob"},"state":"COMMENTED"}
		]`)
	})

	if err := c.RerequestReview(context.Background(), 42); err != nil {
		t.Fatalf("RerequestReview no-op: %v", err)
	}
}

// TestRerequestReview_nonVerdictRowsDoNotClearVerdict pins the fold rule that
// only verdict-bearing rows (APPROVED/CHANGES_REQUESTED) update a reviewer's
// latest verdict: alice's CHANGES_REQUESTED followed by a COMMENTED reply
// leaves her still in the POSTed set — GitHub's review decision is likewise
// unaffected by COMMENTED reviews, so a reviewer who replied in the thread is
// still requesting changes.
func TestRerequestReview_nonVerdictRowsDoNotClearVerdict(t *testing.T) {
	var gotReqBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = io.WriteString(w, `[
			  {"user":{"login":"alice"},"state":"CHANGES_REQUESTED"},
			  {"user":{"login":"alice"},"state":"COMMENTED"}
			]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
			gotReqBody = decodeBody(t, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
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
	if len(raw) != 1 || raw[0] != "alice" {
		t.Errorf("reviewers = %v; want [alice] — a COMMENTED reply must not clear the verdict", raw)
	}
}

// TestRerequestReview_refusalSurfacesGitHubWordsVerbatim: a GitHub refusal of
// the /requested_reviewers POST becomes tracker.ErrReviewRejected carrying
// GitHub's own words — never the token.
func TestRerequestReview_refusalSurfacesGitHubWordsVerbatim(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[{"user":{"login":"alice"},"state":"CHANGES_REQUESTED"}]`)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"Review cannot be requested from pull request author"}`)
	})

	err := c.RerequestReview(context.Background(), 42)
	if !errors.Is(err, tracker.ErrReviewRejected) {
		t.Fatalf("err = %v, want ErrReviewRejected", err)
	}
	if !strings.Contains(err.Error(), "cannot be requested from pull request author") {
		t.Fatalf("err = %q, want GitHub's own words verbatim", err.Error())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into error: %q", err.Error())
	}
}

// TestRerequestReview_rateLimitedNotRejected: a throttled /requested_reviewers
// POST stays tracker.ErrRateLimited (a transient upstream throttle), NOT
// ErrReviewRejected — GitHub's secondary limits target mutating requests, so
// the caller must come back later, not treat the pull as un-re-requestable.
func TestRerequestReview_rateLimitedNotRejected(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[{"user":{"login":"alice"},"state":"CHANGES_REQUESTED"}]`)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1783500000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit"}`)
	})

	err := c.RerequestReview(context.Background(), 42)
	if !errors.Is(err, tracker.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited (a throttle, not a refusal)", err)
	}
	if errors.Is(err, tracker.ErrReviewRejected) {
		t.Fatalf("throttle mislabeled as a review refusal: %v", err)
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
// issue-comment number space.
func TestPullComments_mapsSharedIssueCommentEndpoint(t *testing.T) {
	var gotMethod, gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = io.WriteString(w, `[
		  {"body":"first comment","user":{"login":"alice"},"created_at":"2026-03-01T12:00:00Z"},
		  {"body":"second comment","user":{"login":"bob"},"created_at":"2026-03-01T13:00:00Z"}
		]`)
	})

	comments, err := c.PullComments(context.Background(), 62)
	if err != nil {
		t.Fatalf("PullComments: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != apiPrefix+"/issues/62/comments" {
		t.Errorf("request = %s %s; want GET %s/issues/62/comments", gotMethod, gotPath, apiPrefix)
	}
	if len(comments) != 2 || comments[0].Author != "alice" || comments[1].Author != "bob" {
		t.Errorf("comments = %+v", comments)
	}
	if comments[0].Body != "first comment" ||
		!comments[0].CreatedAt.Equal(mustTime(t, "2026-03-01T12:00:00Z")) {
		t.Errorf("comment[0] mismatch: %+v", comments[0])
	}
}

// TestPullComments_paginate: unlike Forgejo, GitHub's per-issue comments
// endpoint paginates with Link, so PullComments must follow it exactly like
// Issue does (a >100-comment thread would otherwise be truncated).
func TestPullComments_paginate(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			setNextLink(w, r)
			_, _ = io.WriteString(w, genCommentsJSON(1, pageLimit))
		case "2":
			_, _ = io.WriteString(w, genCommentsJSON(pageLimit+1, 5))
		default:
			t.Errorf("unexpected comments page %q", r.URL.Query().Get("page"))
			_, _ = io.WriteString(w, `[]`)
		}
	})

	comments, err := c.PullComments(context.Background(), 62)
	if err != nil {
		t.Fatalf("PullComments: %v", err)
	}
	if len(comments) != pageLimit+5 {
		t.Fatalf("got %d comments; want %d (both pages)", len(comments), pageLimit+5)
	}
	if comments[0].Body != "c1" || comments[pageLimit+4].Body != fmt.Sprintf("c%d", pageLimit+5) {
		t.Errorf("comment order lost across pages")
	}
}

func TestPullComments_notFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
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
		_, _ = io.WriteString(w, `{"number":62,"state":"closed"}`)
	})

	if err := c.CloseIssue(context.Background(), 62); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != apiPrefix+"/issues/62" {
		t.Errorf("request = %s %s; want PATCH %s/issues/62", gotMethod, gotPath, apiPrefix)
	}
	if gotBody["state"] != "closed" {
		t.Errorf("request body = %v", gotBody)
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
		absentKeys []string
	}{
		{"title only", tracker.IssueEdit{Title: strPtr("new title")}, []string{"body"}},
		{"body only", tracker.IssueEdit{Body: strPtr("new body")}, []string{"title"}},
		{"both", tracker.IssueEdit{Title: strPtr("t"), Body: strPtr("b")}, nil},
		{"clear body", tracker.IssueEdit{Body: strPtr("")}, []string{"title"}},
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
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})

	_, err := c.EditIssue(context.Background(), 999, tracker.IssueEdit{Title: strPtr("x")})
	if !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// --- labels / triage surface -----------------------------------------------

// labelsJSON is the fixture label set: GitHub emits colors WITHOUT the leading
// '#', and this deliberately arrives unsorted.
const labelsJSON = `[
  {"name":"needs-triage","color":"6b7280","description":"triage me"},
  {"name":"bug","color":"ee0701","description":""},
  {"name":"kind/feature","color":"a2eeef","description":"new behavior"}
]`

// labelFixture answers GET /labels with labelsJSON and delegates everything
// else to next, recording each non-labels request method+path.
func labelFixture(t *testing.T, next http.HandlerFunc) (http.HandlerFunc, *[]string) {
	t.Helper()
	var calls []string
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == apiPrefix+"/labels" {
			_, _ = io.WriteString(w, labelsJSON)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		next(w, r)
	}, &calls
}

func TestLabels_sortedNormalized(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPrefix+"/labels" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, labelsJSON)
	})
	labels, err := c.Labels(context.Background())
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	want := []tracker.Label{
		{Name: "bug", Color: "#ee0701"},
		{Name: "kind/feature", Color: "#a2eeef", Description: "new behavior"},
		{Name: "needs-triage", Color: "#6b7280", Description: "triage me"},
	}
	if len(labels) != len(want) {
		t.Fatalf("Labels = %+v", labels)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("labels[%d] = %+v, want %+v", i, labels[i], want[i])
		}
	}
}

// TestCreateIssue_resolvesLabelNamesStrict: names are verified against the
// repo's set BEFORE the create, and go on the wire AS NAMES (GitHub addresses
// labels by name) — an unknown name aborts before the create reaches GitHub,
// so GitHub's silent label auto-create can never swallow a typo.
func TestCreateIssue_resolvesLabelNamesStrict(t *testing.T) {
	var created map[string]any
	fixture, calls := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		created = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":9,"title":"found a bug","body":"details","state":"open",
		  "labels":[{"name":"bug","color":"ee0701"}],
		  "created_at":"2026-07-06T00:00:00Z","updated_at":"2026-07-06T00:00:00Z"}`)
	})
	c := newTestClient(t, fixture)

	is, err := c.CreateIssue(context.Background(), "found a bug", "details", []string{"bug", "kind/feature"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "POST "+apiPrefix+"/issues" {
		t.Fatalf("github calls = %v, want one POST /issues", *calls)
	}
	if names, _ := created["labels"].([]any); len(names) != 2 || names[0] != "bug" || names[1] != "kind/feature" {
		t.Errorf("wire labels = %v, want the resolved names [bug kind/feature]", created["labels"])
	}
	if is.Number != 9 || len(is.Labels) != 1 || is.Labels[0] != "bug" {
		t.Errorf("created issue = %+v", is)
	}

	// Unknown name: typed error naming it, and the create never leaves.
	_, err = c.CreateIssue(context.Background(), "doomed", "", []string{"bug", "nope"})
	if !errors.Is(err, tracker.ErrUnknownLabel) || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown label err = %v, want ErrUnknownLabel naming it", err)
	}
	if len(*calls) != 1 {
		t.Errorf("github calls after refused create = %v, want no new POST", *calls)
	}
}

func TestAddIssueLabels_namesOnWireStrict(t *testing.T) {
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
		t.Fatalf("github calls = %v, want one POST /issues/7/labels", *calls)
	}
	if names, _ := added["labels"].([]any); len(names) != 2 || names[0] != "needs-triage" || names[1] != "bug" {
		t.Errorf("wire labels = %v, want [needs-triage bug]", added["labels"])
	}

	if err := c.AddIssueLabels(context.Background(), 7, []string{"ghost"}); !errors.Is(err, tracker.ErrUnknownLabel) {
		t.Fatalf("unknown label err = %v, want ErrUnknownLabel", err)
	}
	if len(*calls) != 1 {
		t.Errorf("github calls after refused add = %v, want no new POST", *calls)
	}
}

// TestRemoveIssueLabels_oneDeletePerNameEscaped: one DELETE per label, the
// name path-escaped (kind/feature must not become two path segments).
func TestRemoveIssueLabels_oneDeletePerNameEscaped(t *testing.T) {
	var gotEscaped []string
	fixture, _ := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = append(gotEscaped, r.Method+" "+r.URL.EscapedPath())
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `[]`)
	})
	c := newTestClient(t, fixture)

	if err := c.RemoveIssueLabels(context.Background(), 7, []string{"kind/feature", "bug"}); err != nil {
		t.Fatalf("RemoveIssueLabels: %v", err)
	}
	want := []string{
		"DELETE " + apiPrefix + "/issues/7/labels/kind%2Ffeature",
		"DELETE " + apiPrefix + "/issues/7/labels/bug",
	}
	if len(gotEscaped) != 2 || gotEscaped[0] != want[0] || gotEscaped[1] != want[1] {
		t.Fatalf("delete calls = %v, want %v", gotEscaped, want)
	}

	if err := c.RemoveIssueLabels(context.Background(), 7, []string{"ghost", "bug"}); !errors.Is(err, tracker.ErrUnknownLabel) {
		t.Fatalf("unknown label err = %v, want ErrUnknownLabel", err)
	}
}

func TestEnsureLabel_existingReturnedWithoutCreate(t *testing.T) {
	fixture, calls := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected github call %s %s", r.Method, r.URL.Path)
	})
	c := newTestClient(t, fixture)

	l, err := c.EnsureLabel(context.Background(), "bug", "#123456", "ignored")
	if err != nil {
		t.Fatalf("EnsureLabel: %v", err)
	}
	if l.Name != "bug" || l.Color != "#ee0701" || l.Description != "" {
		t.Errorf("ensured label = %+v, want the existing forge label untouched", l)
	}
	if len(*calls) != 0 {
		t.Errorf("github calls = %v, want none beyond the list", *calls)
	}
}

// TestEnsureLabel_createsAbsentColorWithoutHash: GitHub rejects a '#'-prefixed
// color, so the client strips it (and defaults to the builtin-parity color).
func TestEnsureLabel_createsAbsentColorWithoutHash(t *testing.T) {
	var created map[string]any
	fixture, calls := labelFixture(t, func(w http.ResponseWriter, r *http.Request) {
		created = decodeBody(t, r)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"triage/ready","color":"6b7280","description":"queued"}`)
	})
	c := newTestClient(t, fixture)

	l, err := c.EnsureLabel(context.Background(), "triage/ready", "", "queued")
	if err != nil {
		t.Fatalf("EnsureLabel: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0] != "POST "+apiPrefix+"/labels" {
		t.Fatalf("github calls = %v, want one POST /labels", *calls)
	}
	if created["color"] != strings.TrimPrefix(defaultLabelColor, "#") {
		t.Errorf("create color = %v, want %q (no leading #)", created["color"], strings.TrimPrefix(defaultLabelColor, "#"))
	}
	if l.Name != "triage/ready" || l.Color != "#6b7280" || l.Description != "queued" {
		t.Errorf("ensured label = %+v", l)
	}
}

// TestEnsureLabel_duplicateResolvesByRelist: GitHub answers 422 on a
// duplicate-name create; the client re-lists to the winner's label.
func TestEnsureLabel_duplicateResolvesByRelist(t *testing.T) {
	listCalls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == apiPrefix+"/labels":
			listCalls++
			if listCalls == 1 {
				_, _ = io.WriteString(w, `[]`) // not there yet…
				return
			}
			_, _ = io.WriteString(w, `[{"name":"bug","color":"ee0701","description":"raced"}]`)
		case r.Method == http.MethodPost && r.URL.Path == apiPrefix+"/labels":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"Validation Failed","errors":[{"code":"already_exists"}]}`)
		default:
			t.Errorf("unexpected github call %s %s", r.Method, r.URL.Path)
		}
	})

	l, err := c.EnsureLabel(context.Background(), "bug", "", "")
	if err != nil {
		t.Fatalf("EnsureLabel after duplicate: %v", err)
	}
	if l.Name != "bug" || l.Color != "#ee0701" || l.Description != "raced" {
		t.Errorf("ensured label = %+v, want the winner's label", l)
	}
	if listCalls != 2 {
		t.Errorf("list calls = %d, want 2 (initial + conflict re-list)", listCalls)
	}
}

// --- Error mapping ---------------------------------------------------------

func TestErrorMapping_statusAndNoTokenLeak(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"message":"boom happened","documentation_url":"https://docs.github.com"}`)
			})
			_, err := c.ReadyIssues(context.Background())
			if err == nil {
				t.Fatal("expected an error for non-2xx status")
			}
			msg := err.Error()
			if !strings.Contains(msg, strconv.Itoa(status)) || !strings.Contains(msg, "boom happened") {
				t.Errorf("error %q missing status/body snippet", msg)
			}
			if strings.Contains(msg, testToken) {
				t.Fatalf("token leaked into error: %q", msg)
			}
			if status == http.StatusNotFound && !errors.Is(err, tracker.ErrNotFound) {
				t.Errorf("404 does not unwrap to ErrNotFound: %v", err)
			}
		})
	}
}

// TestRateLimited pins ADR-0015 decision 6: a throttled 403/429 unwraps to
// ErrRateLimited with the reset in the message, while a plain 403 (e.g. the
// token cannot see the repo) does NOT — it would otherwise make the scheduler
// skip forever.
func TestRateLimited(t *testing.T) {
	t.Run("403 remaining=0", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1720000000")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
		})
		_, err := c.ReadyIssues(context.Background())
		if !errors.Is(err, tracker.ErrRateLimited) {
			t.Fatalf("err = %v, want ErrRateLimited", err)
		}
		if !strings.Contains(err.Error(), "1720000000") || strings.Contains(err.Error(), testToken) {
			t.Errorf("error %q should carry the reset and never the token", err)
		}
	})
	t.Run("429 retry-after", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"secondary rate limit"}`)
		})
		_, err := c.Pulls(context.Background())
		if !errors.Is(err, tracker.ErrRateLimited) {
			t.Fatalf("err = %v, want ErrRateLimited", err)
		}
		if !strings.Contains(err.Error(), "60") {
			t.Errorf("error %q should carry the retry-after", err)
		}
	})
	t.Run("plain 403 is not a rate limit", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"Resource not accessible by personal access token"}`)
		})
		_, err := c.ReadyIssues(context.Background())
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, tracker.ErrRateLimited) {
			t.Errorf("plain 403 wrongly classified as a rate limit: %v", err)
		}
	})
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

// setNextLink advertises a rel="next" page pointing at page+1 (GitHub's own
// pagination signal — the client follows it, no empty-page probe).
func setNextLink(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page == 0 {
		page = 1
	}
	w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/x?page=%d>; rel="next", <https://api.github.com/x?page=99>; rel="last"`, page+1))
}

// TestPagination_followsLinkNext: the client walks pages while Link advertises
// rel="next" and stops on the page that omits it — a short-but-nonempty page
// with a next link keeps going, one without stops. (The pagination vehicle is
// the OPEN walk — since issue #176 that is the listing that still rides
// fetchPages; the closed view is a one-request window that ignores Link.)
func TestPagination_followsLinkNext(t *testing.T) {
	var pages []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		switch page {
		case "1":
			setNextLink(w, r)
			_, _ = io.WriteString(w, genIssuesJSON(1, pageLimit))
		case "2":
			setNextLink(w, r) // short page BUT a next link → keep going
			_, _ = io.WriteString(w, genIssuesJSON(pageLimit+1, 3))
		case "3":
			_, _ = io.WriteString(w, genIssuesJSON(pageLimit+4, 2)) // no Link → stop
		default:
			t.Errorf("unexpected page %q (should have stopped)", page)
			_, _ = io.WriteString(w, `[]`)
		}
	})

	issues, err := c.Issues(context.Background(), "open")
	if err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if len(issues) != pageLimit+5 {
		t.Fatalf("got %d issues; want %d (all three pages)", len(issues), pageLimit+5)
	}
	if len(pages) != 3 || pages[0] != "1" || pages[2] != "3" {
		t.Fatalf("requested pages = %v; want [1 2 3]", pages)
	}
	if issues[0].Number != 1 || issues[len(issues)-1].Number != pageLimit+5 {
		t.Errorf("page order not preserved: first=%d last=%d", issues[0].Number, issues[len(issues)-1].Number)
	}
}

// TestPagination_pageCapIsExplicitError: an endpoint whose Link never drops
// rel="next" yields a loud error after maxPages, never an infinite loop.
func TestPagination_pageCapIsExplicitError(t *testing.T) {
	var requests int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > maxPages+1 {
			t.Errorf("more than %d requests; page cap not enforced", maxPages+1)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		setNextLink(w, r) // always advertises another page
		_, _ = io.WriteString(w, genIssuesJSON((requests-1)*pageLimit+1, pageLimit))
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

func TestRequestPathsEscapeOwnerRepo(t *testing.T) {
	var gotEscaped, gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		gotState = r.URL.Query().Get("state")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.Client(), srv.URL, testToken, "evil?owner", "re#po/../x")

	if _, err := c.Issues(context.Background(), "open"); err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if want := "/repos/evil%3Fowner/re%23po%2F..%2Fx/issues"; gotEscaped != want {
		t.Errorf("escaped path = %q; want %q", gotEscaped, want)
	}
	if gotState != "open" {
		t.Errorf("state param = %q; want open (query must survive a hostile owner/repo)", gotState)
	}
}

// --- constructor defaults --------------------------------------------------

func TestNew_trimsTrailingSlashAndDefaultsClient(t *testing.T) {
	c := New(nil, "https://api.github.com/", "tok", "o", "r")
	if c.baseURL != "https://api.github.com" {
		t.Errorf("baseURL = %q; want trailing slash trimmed", c.baseURL)
	}
	if c.httpClient == nil {
		t.Error("nil httpClient should default to a non-nil client")
	}
	if c.timeout != defaultRequestTimeout {
		t.Errorf("timeout = %s; want %s", c.timeout, defaultRequestTimeout)
	}
}
