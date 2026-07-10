package store

// Repo secret accessors (design §12, issue #104 foundation slice). Secrets
// are per-repo named encrypted blobs, unique by name within a repo
// (UNIQUE(repo_id, name) → ErrNameTaken), cascade-deleted with their repo.
// Like credentials, the store only ever sees the encrypted payload (vault
// nonce||ciphertext); it never imports internal/vault and never decrypts.
// Metadata reads (RepoSecrets, RepoSecretByID) never select encrypted_value —
// only RepoSecretValues does, and it returns raw blobs keyed by name, never a
// RepoSecret struct, so the blob can never leak through the metadata type.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// validSecretNameRE is the shell/env-safe name grammar for repo secrets: a
// leading uppercase letter, followed by any run of uppercase letters,
// digits, or underscores. See ValidSecretName.
var validSecretNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ValidSecretName reports whether name matches ^[A-Z][A-Z0-9_]*$ — the
// grammar for names materialized as environment variables in a session.
func ValidSecretName(name string) bool {
	return validSecretNameRE.MatchString(name)
}

// RepoSecret is one row of repo_secrets, metadata only — no value field.
// List and single-item reads never carry the encrypted blob; only
// RepoSecretValues does, keyed by name rather than embedded in this struct.
type RepoSecret struct {
	ID          string
	RepoID      string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// repoSecretColumns is the metadata column list every RepoSecret SELECT
// uses, in scanRepoSecret order. encrypted_value is deliberately absent —
// metadata paths never select it.
const repoSecretColumns = `id, repo_id, name, description, created_at, updated_at`

// CreateRepoSecret inserts a repo secret row. The caller supplies the sec_
// id (generated before encryption) and the clock; encryptedValue is an
// already-encrypted blob (vault nonce||ciphertext) — the store never sees
// plaintext. Returns ErrInvalidSecretName if name fails ValidSecretName,
// ErrNameTaken on a (repo_id, name) collision, and ErrNotFound if repoID
// does not reference an existing repo.
func (s *Store) CreateRepoSecret(ctx context.Context, id, repoID, name, description string, encryptedValue []byte, now time.Time) (RepoSecret, error) {
	if !ValidSecretName(name) {
		return RepoSecret{}, fmt.Errorf("create repo secret %q: %w", name, ErrInvalidSecretName)
	}
	rs := RepoSecret{
		ID:          id,
		RepoID:      repoID,
		Name:        name,
		Description: description,
		CreatedAt:   storedTime(now),
		UpdatedAt:   storedTime(now),
	}
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO repo_secrets (id, repo_id, name, description, encrypted_value, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`),
		rs.ID, rs.RepoID, rs.Name, rs.Description, encryptedValue, fmtTime(rs.CreatedAt), fmtTime(rs.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return RepoSecret{}, fmt.Errorf("create repo secret %q: %w", name, ErrNameTaken)
		}
		if isForeignKeyViolation(err) {
			return RepoSecret{}, fmt.Errorf("create repo secret %q: repo %q: %w", name, repoID, ErrNotFound)
		}
		return RepoSecret{}, fmt.Errorf("create repo secret %q: %w", name, err)
	}
	return rs, nil
}

// RepoSecrets lists a repo's secrets as metadata (no value) ordered by name.
func (s *Store) RepoSecrets(ctx context.Context, repoID string) ([]RepoSecret, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+repoSecretColumns+` FROM repo_secrets WHERE repo_id = ? ORDER BY name`), repoID)
	if err != nil {
		return nil, fmt.Errorf("repo secrets for %q: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	secrets := make([]RepoSecret, 0)
	for rows.Next() {
		rs, err := scanRepoSecret(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo secrets for %q: %w", repoID, err)
		}
		secrets = append(secrets, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo secrets for %q: %w", repoID, err)
	}
	return secrets, nil
}

// RepoSecretByID returns one secret's metadata (no value), or ErrNotFound.
// Used by the httpapi cross-repo guard: callers compare the returned RepoID
// against the request's repo to reject cross-repo access.
func (s *Store) RepoSecretByID(ctx context.Context, id string) (RepoSecret, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+repoSecretColumns+` FROM repo_secrets WHERE id = ?`), id)
	rs, err := scanRepoSecret(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RepoSecret{}, fmt.Errorf("repo secret %q: %w", id, ErrNotFound)
		}
		return RepoSecret{}, fmt.Errorf("repo secret %q: %w", id, err)
	}
	return rs, nil
}

// RotateRepoSecret replaces a secret's encrypted value and bumps updated_at,
// returning fresh metadata. The old ciphertext is never read back. ErrNotFound
// if id does not exist.
func (s *Store) RotateRepoSecret(ctx context.Context, id string, encryptedValue []byte, now time.Time) (RepoSecret, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE repo_secrets SET encrypted_value = ?, updated_at = ? WHERE id = ?`),
		encryptedValue, fmtTime(storedTime(now)), id)
	if err != nil {
		return RepoSecret{}, fmt.Errorf("rotate repo secret %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return RepoSecret{}, fmt.Errorf("rotate repo secret %q: %w", id, err)
	}
	if n == 0 {
		return RepoSecret{}, fmt.Errorf("rotate repo secret %q: %w", id, ErrNotFound)
	}
	return s.RepoSecretByID(ctx, id)
}

// DeleteRepoSecret removes a secret immediately (no reference-count guard —
// unlike credentials, secrets are not shared). ErrNotFound if id does not
// exist.
func (s *Store) DeleteRepoSecret(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM repo_secrets WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete repo secret %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete repo secret %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete repo secret %q: %w", id, ErrNotFound)
	}
	return nil
}

// RepoSecretValues returns the encrypted blobs of the requested names that
// exist in repoID, keyed by name. Names not found in this repo (unknown, or
// belonging to a different repo) are simply absent from the map — callers
// decide how to treat a miss. An empty names slice returns an empty map
// without a query.
func (s *Store) RepoSecretValues(ctx context.Context, repoID string, names []string) (map[string][]byte, error) {
	values := make(map[string][]byte, len(names))
	if len(names) == 0 {
		return values, nil
	}

	placeholders := make([]string, len(names))
	args := make([]any, 0, len(names)+1)
	args = append(args, repoID)
	for i, name := range names {
		placeholders[i] = "?"
		args = append(args, name)
	}

	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT name, encrypted_value FROM repo_secrets
		 WHERE repo_id = ? AND name IN (`+strings.Join(placeholders, ", ")+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("repo secret values for %q: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			name  string
			value []byte
		)
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("repo secret values for %q: %w", repoID, err)
		}
		values[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo secret values for %q: %w", repoID, err)
	}
	return values, nil
}

// scanRepoSecret reads one row in repoSecretColumns order.
func scanRepoSecret(scan func(dest ...any) error) (RepoSecret, error) {
	var (
		rs               RepoSecret
		created, updated string
	)
	if err := scan(&rs.ID, &rs.RepoID, &rs.Name, &rs.Description, &created, &updated); err != nil {
		return RepoSecret{}, err
	}
	var err error
	if rs.CreatedAt, err = parseTime(created); err != nil {
		return RepoSecret{}, err
	}
	if rs.UpdatedAt, err = parseTime(updated); err != nil {
		return RepoSecret{}, err
	}
	return rs, nil
}
