package store

// Minimal PAT accessors needed by the operator-API Bearer auth path (M1).
// PAT CRUD (list, delete, UI) lands in M5; only create/lookup/touch live
// here for now.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// APIToken is one row of api_tokens. The token itself is never stored —
// only its sha256 hex in TokenHash.
type APIToken struct {
	ID         string
	UserID     string
	Name       string
	TokenHash  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// CreateAPIToken inserts a new PAT row for tokenHash (sha256 hex of a
// `lab_pat_…` token), stamped with the store clock.
func (s *Store) CreateAPIToken(ctx context.Context, userID, name, tokenHash string) (APIToken, error) {
	tok := APIToken{
		ID:        ids.NewID("tok"),
		UserID:    userID,
		Name:      name,
		TokenHash: tokenHash,
		CreatedAt: storedTime(s.Now()),
	}
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO api_tokens (id, user_id, name, token_hash, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, NULL)`),
		tok.ID, tok.UserID, tok.Name, tok.TokenHash, fmtTime(tok.CreatedAt))
	if err != nil {
		return APIToken{}, fmt.Errorf("create api token %q: %w", name, err)
	}
	return tok, nil
}

// APITokenByHash returns the PAT with the given sha256 hex, or ErrNotFound.
func (s *Store) APITokenByHash(ctx context.Context, tokenHash string) (APIToken, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT id, user_id, name, token_hash, created_at, last_used_at
		 FROM api_tokens WHERE token_hash = ?`), tokenHash)
	var (
		tok      APIToken
		created  string
		lastUsed sql.NullString
	)
	if err := row.Scan(&tok.ID, &tok.UserID, &tok.Name, &tok.TokenHash, &created, &lastUsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIToken{}, fmt.Errorf("api token by hash: %w", ErrNotFound)
		}
		return APIToken{}, fmt.Errorf("api token by hash: %w", err)
	}
	var err error
	if tok.CreatedAt, err = parseTime(created); err != nil {
		return APIToken{}, fmt.Errorf("api token by hash: %w", err)
	}
	if tok.LastUsedAt, err = parseNullTime(lastUsed); err != nil {
		return APIToken{}, fmt.Errorf("api token by hash: %w", err)
	}
	return tok, nil
}

// TouchAPIToken updates last_used_at; ErrNotFound when the token is gone.
func (s *Store) TouchAPIToken(ctx context.Context, id string, usedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`), fmtTime(usedAt), id)
	if err != nil {
		return fmt.Errorf("touch api token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("touch api token: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("touch api token: %w", ErrNotFound)
	}
	return nil
}
