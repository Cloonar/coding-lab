package store

// PAT accessors: create/lookup/touch feed the operator-API Bearer auth path
// (M1); list and delete back the tokens CRUD surface (D7, M5). The token
// itself is never stored — auth resolves by sha256 hex on every request, so
// deleting a row revokes the token immediately.

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

// APITokensByUser lists a user's PATs, newest first (the tokens UI order).
func (s *Store) APITokensByUser(ctx context.Context, userID string) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT id, user_id, name, token_hash, created_at, last_used_at
		 FROM api_tokens WHERE user_id = ?
		 ORDER BY created_at DESC, id`), userID)
	if err != nil {
		return nil, fmt.Errorf("api tokens by user: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var toks []APIToken
	for rows.Next() {
		var (
			tok      APIToken
			created  string
			lastUsed sql.NullString
		)
		if err := rows.Scan(&tok.ID, &tok.UserID, &tok.Name, &tok.TokenHash, &created, &lastUsed); err != nil {
			return nil, fmt.Errorf("api tokens by user: %w", err)
		}
		if tok.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("api tokens by user: %w", err)
		}
		if tok.LastUsedAt, err = parseNullTime(lastUsed); err != nil {
			return nil, fmt.Errorf("api tokens by user: %w", err)
		}
		toks = append(toks, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api tokens by user: %w", err)
	}
	return toks, nil
}

// DeleteAPIToken removes one of userID's PATs by id — revocation is immediate
// because the Bearer path looks the hash up per request. ErrNotFound when id
// names none of the user's tokens (another user's token is invisible here).
func (s *Store) DeleteAPIToken(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`), id, userID)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete api token: %w", ErrNotFound)
	}
	return nil
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
