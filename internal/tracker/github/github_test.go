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

func TestIssues_stateForwardedAndPRsFiltered(t *testing.T) {
	for _, state := range []string{"open", "closed", "all"} {
		t.Run(state, func(t *testing.T) {
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
			issues, err := c.Issues(context.Background(), state)
			if err != nil {
				t.Fatalf("Issues(%q): %v", state, err)
			}
			if gotState != state {
				t.Errorf("state param = %q; want %q", gotState, state)
			}
			if len(issues) != 1 || issues[0].Number != 3 {
				t.Errorf("issues = %+v; want only #3 (PR filtered)", issues)
			}
		})
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

func TestPulls_stateMappingMergedAtAndHeads(t *testing.T) {
	var gotState string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotState = r.URL.Query().Get("state")
		// merged state derives from merged_at (no `merged` bool on the list).
		_, _ = io.WriteString(w, `[
		  {"number":72,"state":"open","merged_at":null,"head":{"ref":"afk/63"},"html_url":"https://github.com/octocat/hello-world/pull/72"},
		  {"number":40,"state":"closed","merged_at":"2026-01-01T00:00:00Z","head":{"ref":"afk/12"},"html_url":"https://github.com/octocat/hello-world/pull/40"},
		  {"number":9,"state":"closed","merged_at":null,"head":{"ref":"afk/7"},"html_url":"https://github.com/octocat/hello-world/pull/9"}
		]`)
	})

	pulls, err := c.Pulls(context.Background())
	if err != nil {
		t.Fatalf("Pulls: %v", err)
	}
	if gotState != "all" {
		t.Errorf("state param = %q; want all", gotState)
	}
	want := []tracker.PullRef{
		{Number: 72, HeadBranch: "afk/63", State: "open", URL: "https://github.com/octocat/hello-world/pull/72"},
		{Number: 40, HeadBranch: "afk/12", State: "merged", URL: "https://github.com/octocat/hello-world/pull/40"},
		{Number: 9, HeadBranch: "afk/7", State: "closed", URL: "https://github.com/octocat/hello-world/pull/9"},
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
// with a next link keeps going, one without stops.
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

	issues, err := c.Issues(context.Background(), "all")
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

func TestRequestPathsEscapeOwnerRepo(t *testing.T) {
	var gotEscaped, gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		gotState = r.URL.Query().Get("state")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.Client(), srv.URL, testToken, "evil?owner", "re#po/../x")

	if _, err := c.Issues(context.Background(), "all"); err != nil {
		t.Fatalf("Issues: %v", err)
	}
	if want := "/repos/evil%3Fowner/re%23po%2F..%2Fx/issues"; gotEscaped != want {
		t.Errorf("escaped path = %q; want %q", gotEscaped, want)
	}
	if gotState != "all" {
		t.Errorf("state param = %q; want all (query must survive a hostile owner/repo)", gotState)
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
