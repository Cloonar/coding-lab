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
)

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// testFixture is a migrated sqlite store plus a raw connection to the same
// file for seeding repos/runs/run_tokens (no store accessors exist for
// those yet — they land with the M3/M5 services).
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
	f.exec(t, `INSERT INTO repos
		(id, name, remote_url, tracker_binding, forge_kind, afk_branch_pattern, manual_branch_prefix, created_at)
		VALUES (?, ?, ?, 'builtin', 'none', 'afk/<N>', 'lab/', ?)`,
		id, "repo-"+id, "https://example.invalid/r.git", f.now.Format(timeFormat))
}

func (f *testFixture) seedRun(t *testing.T, id, repoID, outcome string) {
	f.exec(t, `INSERT INTO runs
		(id, repo_id, kind, provider, issue_number, branch, worktree_path, session_name, model, effort, started_at, outcome)
		VALUES (?, ?, 'afk_auto', 'claude-code', 7, 'afk/7', '/tmp/wt', ?, 'opus[1m]', 'max', ?, ?)`,
		id, repoID, "sess-"+id, f.now.Format(timeFormat), outcome)
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

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestRunTokenAuthMatrix(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
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

	handler := New(f.st, discard(), func() time.Time { return f.now }).Handler()

	tests := []struct {
		name       string
		authz      string
		wantStatus int
		wantError  string
	}{
		{"active run, no expiry", "Bearer " + activeNoExpiry, http.StatusNotImplemented, "agent API lands in M5"},
		{"active run, future expiry", "Bearer " + activeFuture, http.StatusNotImplemented, "agent API lands in M5"},
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
			if !strings.Contains(rr.Body.String(), tt.wantError) {
				t.Fatalf("body = %q, want it to contain %q", rr.Body.String(), tt.wantError)
			}
		})
	}
}

func TestAllRoutesAreMountedBehindAuth(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_active", "repo_a", "active")
	token := f.seedToken(t, "run_active", nil)

	handler := New(f.st, discard(), func() time.Time { return f.now }).Handler()

	routes := []struct{ method, path string }{
		{"GET", "/agent/v1/issue"},
		{"GET", "/agent/v1/issues"},
		{"GET", "/agent/v1/issues/7"},
		{"POST", "/agent/v1/issues/7/comments"},
		{"POST", "/agent/v1/prs"},
	}
	for _, rt := range routes {
		// Without a token: 401.
		req := httptest.NewRequest(rt.method, rt.path, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token: status = %d, want 401", rt.method, rt.path, rr.Code)
		}

		// With a valid token: the M5 stub.
		req = httptest.NewRequest(rt.method, rt.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s with token: status = %d, want 501 (body %s)", rt.method, rt.path, rr.Code, rr.Body.String())
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

	s := New(f.st, discard(), func() time.Time { return f.now })
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
}
