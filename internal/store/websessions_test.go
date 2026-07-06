package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

func TestWebSessionLifecycle(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		u, err := s.CreateUser(ctx, "admin", "hash")
		if err != nil {
			t.Fatalf("create user: %v", err)
		}

		created := time.Date(2026, 7, 5, 8, 0, 0, 123_000_000, time.UTC)
		ws := WebSession{
			ID:        ids.HashToken("cookie-token"),
			UserID:    u.ID,
			CreatedAt: created,
			ExpiresAt: created.Add(7 * 24 * time.Hour),
			Remember:  true,
		}
		if err := s.CreateWebSession(ctx, ws); err != nil {
			t.Fatalf("create session: %v", err)
		}

		got, err := s.WebSession(ctx, ws.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if got.UserID != u.ID || !got.Remember {
			t.Errorf("got %+v, want user %s remember=true", got, u.ID)
		}
		if !got.CreatedAt.Equal(created) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created)
		}
		if !got.ExpiresAt.Equal(ws.ExpiresAt) {
			t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, ws.ExpiresAt)
		}
		if got.LastSeenAt != nil {
			t.Errorf("LastSeenAt = %v, want nil", got.LastSeenAt)
		}

		seen := created.Add(time.Hour)
		if err := s.TouchWebSession(ctx, ws.ID, seen); err != nil {
			t.Fatalf("touch: %v", err)
		}
		got, err = s.WebSession(ctx, ws.ID)
		if err != nil {
			t.Fatalf("get after touch: %v", err)
		}
		if got.LastSeenAt == nil || !got.LastSeenAt.Equal(seen) {
			t.Errorf("LastSeenAt after touch = %v, want %v", got.LastSeenAt, seen)
		}

		if err := s.TouchWebSession(ctx, "missing", seen); !errors.Is(err, ErrNotFound) {
			t.Errorf("touch missing = %v, want ErrNotFound", err)
		}

		if err := s.DeleteWebSession(ctx, ws.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.WebSession(ctx, ws.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("get after delete = %v, want ErrNotFound", err)
		}
		// Idempotent: deleting again is not an error.
		if err := s.DeleteWebSession(ctx, ws.ID); err != nil {
			t.Errorf("second delete = %v, want nil", err)
		}
	})
}

func TestDeleteExpiredWebSessions(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		u, err := s.CreateUser(ctx, "admin", "hash")
		if err != nil {
			t.Fatalf("create user: %v", err)
		}

		now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
		mk := func(id string, expires time.Time) {
			t.Helper()
			if err := s.CreateWebSession(ctx, WebSession{
				ID: id, UserID: u.ID, CreatedAt: now.Add(-time.Hour), ExpiresAt: expires,
			}); err != nil {
				t.Fatalf("create session %s: %v", id, err)
			}
		}
		mk("expired", now.Add(-time.Minute))
		mk("boundary", now) // expires_at == now is expired (<=)
		mk("live", now.Add(time.Minute))

		n, err := s.DeleteExpiredWebSessions(ctx, now)
		if err != nil {
			t.Fatalf("delete expired: %v", err)
		}
		if n != 2 {
			t.Errorf("deleted = %d, want 2", n)
		}
		if _, err := s.WebSession(ctx, "live"); err != nil {
			t.Errorf("live session was deleted: %v", err)
		}
		if _, err := s.WebSession(ctx, "expired"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expired session survived: %v", err)
		}
	})
}

// TestFKCascadeUserDeletesSessions proves ON DELETE CASCADE actually fires —
// on sqlite this is the witness that the foreign_keys pragma is on (design
// §3a: without it every cascade silently no-ops).
func TestFKCascadeUserDeletesSessions(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		u, err := s.CreateUser(ctx, "admin", "hash")
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
		if err := s.CreateWebSession(ctx, WebSession{
			ID: "sess", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}

		if _, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM users WHERE id = ?`), u.ID); err != nil {
			t.Fatalf("delete user: %v", err)
		}

		if _, err := s.WebSession(ctx, "sess"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("web session survived user delete (err=%v) — foreign_keys pragma not in force?", err)
		}
	})
}

// TestFKCascadeRepoGraph exercises the whole 0001 dependency graph: deleting
// a repo must take runs, run_tokens, issues, comments, labels, issue_labels,
// change requests and cr_closes with it.
func TestFKCascadeRepoGraph(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := "2026-07-05T12:00:00.000Z"
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := s.db.ExecContext(ctx, s.rebind(query), args...); err != nil {
				t.Fatalf("exec %s: %v", query, err)
			}
		}

		exec(`INSERT INTO repos (id, name, remote_url, tracker_binding, forge_kind, afk_branch_pattern, manual_branch_prefix, created_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"repo_1", "demo", "ssh://git@example.com/demo.git", "builtin", "none", "afk/<N>", "lab/", now)
		exec(`INSERT INTO runs (id, repo_id, kind, provider, branch, worktree_path, session_name, model, effort, started_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"run_1", "repo_1", "afk_auto", "claude-code", "afk/7", "/wt/demo-7", "demo~afk-auto-7", "opus[1m]", "max", now)
		exec(`INSERT INTO run_tokens (id, run_id, token_hash, created_at) VALUES (?, ?, ?, ?)`,
			"rtok_1", "run_1", "hashhash", now)
		exec(`INSERT INTO issues (id, repo_id, number, title, state, created_at, updated_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"iss_1", "repo_1", 7, "fix it", "open", now, now)
		exec(`INSERT INTO issue_comments (id, issue_id, author_kind, run_id, body, created_at)
		      VALUES (?, ?, ?, ?, ?, ?)`,
			"cmt_1", "iss_1", "run", "run_1", "done", now)
		exec(`INSERT INTO labels (id, repo_id, name) VALUES (?, ?, ?)`,
			"lbl_1", "repo_1", "ready-for-agent")
		exec(`INSERT INTO issue_labels (issue_id, label_id) VALUES (?, ?)`,
			"iss_1", "lbl_1")
		exec(`INSERT INTO change_requests (id, repo_id, number, title, head_branch, base_branch, state, created_at, updated_at)
		      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"cr_1", "repo_1", 1, "resolve #7", "afk/7", "main", "open", now, now)
		exec(`INSERT INTO cr_closes (cr_id, issue_number) VALUES (?, ?)`,
			"cr_1", 7)

		exec(`DELETE FROM repos WHERE id = ?`, "repo_1")

		for _, table := range []string{
			"repos", "runs", "run_tokens", "issues", "issue_comments",
			"labels", "issue_labels", "change_requests", "cr_closes",
		} {
			if n := count(t, s, table); n != 0 {
				t.Errorf("%s has %d rows after repo delete, want 0", table, n)
			}
		}
	})
}
