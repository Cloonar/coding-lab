package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WebSession is one row of web_sessions. ID is the sha256 hex of the cookie
// token (the lookup key), never a prefixed ID and never the token itself.
type WebSession struct {
	ID         string
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Remember   bool
	LastSeenAt *time.Time
}

// CreateWebSession inserts ws. Timestamps are stored at millisecond
// precision; LastSeenAt may be nil.
func (s *Store) CreateWebSession(ctx context.Context, ws WebSession) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO web_sessions (id, user_id, created_at, expires_at, remember, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?)`),
		ws.ID, ws.UserID, fmtTime(ws.CreatedAt), fmtTime(ws.ExpiresAt), ws.Remember, fmtNullTime(ws.LastSeenAt))
	if err != nil {
		return fmt.Errorf("create web session: %w", err)
	}
	return nil
}

// WebSession returns the session with the given id (sha256 hex of the cookie
// token), or ErrNotFound. Expiry is the caller's check: the row is returned
// as stored.
func (s *Store) WebSession(ctx context.Context, id string) (WebSession, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT id, user_id, created_at, expires_at, remember, last_seen_at
		 FROM web_sessions WHERE id = ?`), id)
	var (
		ws               WebSession
		created, expires string
		lastSeen         sql.NullString
	)
	if err := row.Scan(&ws.ID, &ws.UserID, &created, &expires, &ws.Remember, &lastSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WebSession{}, fmt.Errorf("web session: %w", ErrNotFound)
		}
		return WebSession{}, fmt.Errorf("web session: %w", err)
	}
	var err error
	if ws.CreatedAt, err = parseTime(created); err != nil {
		return WebSession{}, fmt.Errorf("web session: %w", err)
	}
	if ws.ExpiresAt, err = parseTime(expires); err != nil {
		return WebSession{}, fmt.Errorf("web session: %w", err)
	}
	if ws.LastSeenAt, err = parseNullTime(lastSeen); err != nil {
		return WebSession{}, fmt.Errorf("web session: %w", err)
	}
	return ws, nil
}

// DeleteWebSession removes the session. Deleting a missing session is not an
// error (logout is idempotent).
func (s *Store) DeleteWebSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM web_sessions WHERE id = ?`), id); err != nil {
		return fmt.Errorf("delete web session: %w", err)
	}
	return nil
}

// TouchWebSession updates last_seen_at; ErrNotFound when the session does
// not exist.
func (s *Store) TouchWebSession(ctx context.Context, id string, lastSeen time.Time) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE web_sessions SET last_seen_at = ? WHERE id = ?`), fmtTime(lastSeen), id)
	if err != nil {
		return fmt.Errorf("touch web session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("touch web session: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("touch web session: %w", ErrNotFound)
	}
	return nil
}

// DeleteExpiredWebSessions removes every session with expires_at <= now and
// reports how many were deleted. The comparison is lexicographic on the
// fixed-width timestamp encoding.
func (s *Store) DeleteExpiredWebSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM web_sessions WHERE expires_at <= ?`), fmtTime(now))
	if err != nil {
		return 0, fmt.Errorf("delete expired web sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired web sessions: %w", err)
	}
	return n, nil
}
