package agentapi

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
)

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// testFixture is a migrated sqlite store plus a raw connection to the same
// file for seeding repos/runs/run_tokens/issues at exact column values.
type testFixture struct {
	st  *store.Store
	db  *sql.DB
	now time.Time
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lab.db")
	st, err := store.Open(context.Background(), "sqlite:"+path, logx.New(io.Discard))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return &testFixture{
		st:  st,
		db:  db,
		now: time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
	}
}

func (f *testFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatalf("seed exec: %v\n%s", err, query)
	}
}

func (f *testFixture) seedRepo(t *testing.T, id string) {
	f.seedRepoBinding(t, id, "builtin", "none")
}

func (f *testFixture) seedRepoBinding(t *testing.T, id, binding, forgeKind string) {
	f.seedRepoIncogni(t, id, binding, forgeKind, false)
}

func (f *testFixture) seedRepoIncogni(t *testing.T, id, binding, forgeKind string, incogni bool) {
	t.Helper()
	f.exec(t, `INSERT INTO repos
		(id, name, remote_url, tracker_binding, forge_kind, default_branch, afk_branch_pattern, manual_branch_prefix, incogni, created_at)
		VALUES (?, ?, ?, ?, ?, 'main', 'afk/<N>', 'lab/', ?, ?)`,
		id, "repo-"+id, "https://example.invalid/r.git", binding, forgeKind, incogni, f.now.Format(timeFormat))
}

// seedRun inserts the canonical AFK run: kind afk_auto, issue 7, branch afk/7.
func (f *testFixture) seedRun(t *testing.T, id, repoID, outcome string) {
	f.seedRunKind(t, id, repoID, "afk_auto", outcome, intp(7), "afk/7")
}

func (f *testFixture) seedRunKind(t *testing.T, id, repoID, kind, outcome string, issue *int, branch string) {
	t.Helper()
	var issueVal any
	if issue != nil {
		issueVal = *issue
	}
	f.exec(t, `INSERT INTO runs
		(id, repo_id, kind, provider, issue_number, branch, worktree_path, session_name, model, effort, started_at, outcome)
		VALUES (?, ?, ?, 'claude-code', ?, ?, '/tmp/wt', ?, 'opus[1m]', 'max', ?, ?)`,
		id, repoID, kind, issueVal, branch, "sess-"+id, f.now.Format(timeFormat), outcome)
}

// seedIssue inserts a builtin-tracker issue with an explicit number.
func (f *testFixture) seedIssue(t *testing.T, id, repoID string, number int, title, body string) {
	t.Helper()
	f.exec(t, `INSERT INTO issues (id, repo_id, number, title, body, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'open', ?, ?)`,
		id, repoID, number, title, body, f.now.Format(timeFormat), f.now.Format(timeFormat))
}

// seedLabel inserts a repo label (the fixture bypasses repo-create label
// seeding, so tests define exactly the labels they need).
func (f *testFixture) seedLabel(t *testing.T, id, repoID, name string) {
	t.Helper()
	f.exec(t, `INSERT INTO labels (id, repo_id, name, color, description)
		VALUES (?, ?, ?, '#6b7280', '')`, id, repoID, name)
}

// seedToken inserts a run token and returns its plaintext.
func (f *testFixture) seedToken(t *testing.T, runID string, expiresAt *time.Time) string {
	token, hash := ids.NewToken("run")
	var exp any
	if expiresAt != nil {
		exp = expiresAt.UTC().Format(timeFormat)
	}
	f.exec(t, `INSERT INTO run_tokens (id, run_id, token_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		ids.NewID("rtok"), runID, hash, exp, f.now.Format(timeFormat))
	return token
}

func intp(n int) *int { return &n }

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// resolverFunc adapts a function to TrackerResolver.
type resolverFunc func(ctx context.Context, repo store.Repo) (tracker.Tracker, error)

func (f resolverFunc) TrackerFor(ctx context.Context, repo store.Repo) (tracker.Tracker, error) {
	return f(ctx, repo)
}

// builtinResolver mirrors what *tracker.Registry does for builtin-bound
// repos: a store-backed tracker scoped to the repo.
func builtinResolver(st *store.Store) TrackerResolver {
	return resolverFunc(func(_ context.Context, repo store.Repo) (tracker.Tracker, error) {
		return builtin.New(tracker.BuiltinConfig{Store: st, RepoID: repo.ID}), nil
	})
}

func (f *testFixture) server() *Server {
	return New(f.st, builtinResolver(f.st), nil, discard(), func() time.Time { return f.now })
}

func TestRunTokenAuthMatrix(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix the frobnicator", "It wobbles.")
	f.seedRun(t, "run_active", "repo_a", "active")
	f.seedRun(t, "run_stopped", "repo_a", "stopped")
	f.seedRun(t, "run_dead", "repo_a", "death")

	future := f.now.Add(time.Hour)
	past := f.now.Add(-time.Second)

	activeNoExpiry := f.seedToken(t, "run_active", nil)
	activeFuture := f.seedToken(t, "run_active", &future)
	activeExpired := f.seedToken(t, "run_active", &past)
	stoppedToken := f.seedToken(t, "run_stopped", nil)
	deadToken := f.seedToken(t, "run_dead", &future)
	unknownToken, _ := ids.NewToken("run")

	handler := f.server().Handler()

	tests := []struct {
		name       string
		authz      string
		wantStatus int
		wantBody   string
	}{
		{"active run, no expiry", "Bearer " + activeNoExpiry, http.StatusOK, `"number":7`},
		{"active run, future expiry", "Bearer " + activeFuture, http.StatusOK, `"number":7`},
		{"active run, expired token", "Bearer " + activeExpired, http.StatusUnauthorized, "invalid run token"},
		{"stopped run", "Bearer " + stoppedToken, http.StatusUnauthorized, "invalid run token"},
		{"dead run, unexpired token", "Bearer " + deadToken, http.StatusUnauthorized, "invalid run token"},
		{"unknown token", "Bearer " + unknownToken, http.StatusUnauthorized, "invalid run token"},
		{"missing header", "", http.StatusUnauthorized, "run token required"},
		{"wrong scheme", "Basic abc", http.StatusUnauthorized, "run token required"},
		{"wrong prefix", "Bearer lab_pat_abcdef", http.StatusUnauthorized, "run token required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/agent/v1/issue", nil)
			if tt.authz != "" {
				req.Header.Set("Authorization", tt.authz)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content-type = %q, want JSON", ct)
			}
			if !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", rr.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestAllRoutesAreMountedBehindAuth(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix it", "body")
	f.seedLabel(t, "lbl_nt", "repo_a", "needs-triage")
	f.seedRun(t, "run_active", "repo_a", "active")
	token := f.seedToken(t, "run_active", nil)
	past := f.now.Add(-time.Second)
	expired := f.seedToken(t, "run_active", &past)

	handler := f.server().Handler()

	routes := []struct {
		method, path, body string
		wantStatus         int
	}{
		{"GET", "/agent/v1/issue", "", http.StatusOK},
		{"GET", "/agent/v1/issues", "", http.StatusOK},
		{"GET", "/agent/v1/issues/7", "", http.StatusOK},
		{"POST", "/agent/v1/issues/7/comments", `{"body":"hi"}`, http.StatusCreated},
		{"POST", "/agent/v1/issues", `{"title":"t","body":"b"}`, http.StatusCreated},
		{"POST", "/agent/v1/issues/7/labels", `{"labels":["needs-triage"]}`, http.StatusOK},
		{"DELETE", "/agent/v1/issues/7/labels", `{"labels":["needs-triage"]}`, http.StatusOK},
		{"POST", "/agent/v1/issues/7/close", "", http.StatusOK},
		{"GET", "/agent/v1/labels", "", http.StatusOK},
		{"POST", "/agent/v1/labels", `{"name":"bug"}`, http.StatusOK},
		{"POST", "/agent/v1/prs", `{"title":"t","body":"b"}`, http.StatusCreated}, // builtin → change request (M6)
		{"GET", "/agent/v1/prs/1/checks", "", http.StatusOK},                      // CR #1 created by the row above → 200 state none
	}
	for _, rt := range routes {
		// Without a token: 401.
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token: status = %d, want 401", rt.method, rt.path, rr.Code)
		}

		// With an expired token: the same opaque 401 (§3a rule on every route).
		req = httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		req.Header.Set("Authorization", "Bearer "+expired)
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with expired token: status = %d, want 401", rt.method, rt.path, rr.Code)
		}

		// With a valid token: the real handler.
		req = httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != rt.wantStatus {
			t.Fatalf("%s %s with token: status = %d, want %d (body %s)", rt.method, rt.path, rr.Code, rt.wantStatus, rr.Body.String())
		}
	}

	// Unknown agent path with a valid token: JSON 404.
	req := httptest.NewRequest("GET", "/agent/v1/nope", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "not found") {
		t.Fatalf("unknown path: status = %d, body = %q", rr.Code, rr.Body.String())
	}
}

func TestRunTokenContextCarriesRunScope(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_active", "repo_a", "active")
	token := f.seedToken(t, "run_active", nil)

	s := f.server()
	var got store.RunTokenInfo
	var ok bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = RunFromContext(r.Context())
	})
	req := httptest.NewRequest("GET", "/agent/v1/issue", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	s.AuthMiddleware(inner).ServeHTTP(httptest.NewRecorder(), req)

	if !ok {
		t.Fatal("no run info in context")
	}
	if got.RunID != "run_active" || got.RepoID != "repo_a" || got.Outcome != "active" {
		t.Fatalf("run info = %+v", got)
	}
	if got.IssueNumber == nil || *got.IssueNumber != 7 {
		t.Fatalf("issue number = %v, want 7", got.IssueNumber)
	}
	if got.Kind != "afk_auto" || got.Branch != "afk/7" {
		t.Fatalf("kind/branch = %q/%q, want afk_auto/afk/7", got.Kind, got.Branch)
	}
}
