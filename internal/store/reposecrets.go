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
// ExposedRunID/ExposedAt are the sticky exposure flag (issue #108): both nil
// means never exposed since creation or since the last rotation; both set
// records which run's transcript first surfaced the value and when. The pair
// only ever move together — see MarkRepoSecretExposed and RotateRepoSecret.
type RepoSecret struct {
	ID          string
	RepoID      string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time

	ExposedRunID *string    // run whose transcript first exposed this secret's value, sticky until rotated
	ExposedAt    *time.Time // when that exposure was recorded
}

// repoSecretColumns is the metadata column list every RepoSecret SELECT
// uses, in scanRepoSecret order. encrypted_value is deliberately absent —
// metadata paths never select it.
const repoSecretColumns = `id, repo_id, name, description, created_at, updated_at, exposed_run_id, exposed_at`

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
// returning fresh metadata. The old ciphertext is never read back. Rotation
// is the remediation for an exposed secret (issue #108): this also clears
// exposed_run_id/exposed_at, so the exposed-flag's lifecycle rides the
// rotate action rather than needing a dedicated clear-exposure call — a
// later transcript exposure can MarkRepoSecretExposed again, a fresh edge
// for a fresh push notification. ErrNotFound if id does not exist.
func (s *Store) RotateRepoSecret(ctx context.Context, id string, encryptedValue []byte, now time.Time) (RepoSecret, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE repo_secrets SET encrypted_value = ?, updated_at = ?, exposed_run_id = NULL, exposed_at = NULL WHERE id = ?`),
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

// MarkRepoSecretExposed records that runID's transcript exposed the secret
// named name in repoID, as of now. The flag is sticky and first-writer-wins:
// the single UPDATE only touches a row whose exposed_at is still NULL, which
// doubles as the compare-and-set the caller needs. It returns true iff THIS
// call is the one that flipped unset -> exposed — the caller uses that edge
// to send exactly one push notification per exposure, not one per matching
// transcript line. An already-exposed secret is left untouched (the original
// run/timestamp survive) and returns (false, nil); a later exposure never
// overwrites the first exposer. An unknown (repoID, name) pair also returns
// (false, nil), never ErrNotFound: the tailer races a concurrent secret
// delete/rotate, and a secret that has vanished (or already been rotated
// away) by the time we try to mark it is simply no longer exposable, not an
// error worth surfacing. See RotateRepoSecret for how the flag clears.
func (s *Store) MarkRepoSecretExposed(ctx context.Context, repoID, name, runID string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE repo_secrets SET exposed_run_id = ?, exposed_at = ?
		 WHERE repo_id = ? AND name = ? AND exposed_at IS NULL`),
		runID, fmtTime(storedTime(now)), repoID, name)
	if err != nil {
		return false, fmt.Errorf("mark repo secret exposed %q/%q: %w", repoID, name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark repo secret exposed %q/%q: %w", repoID, name, err)
	}
	return n == 1, nil
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

// AllRepoSecretValues returns every encrypted blob belonging to repoID,
// keyed by name — like RepoSecretValues but unfiltered, for callers that
// need the repo's whole secret set rather than a caller-chosen subset. This
// feeds the tailer's redactor build: it scans a run's transcript against
// every live secret value, not just the ones a particular caller names in
// advance. An empty map, not an error, when the repo has no secrets.
func (s *Store) AllRepoSecretValues(ctx context.Context, repoID string) (map[string][]byte, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT name, encrypted_value FROM repo_secrets WHERE repo_id = ?`), repoID)
	if err != nil {
		return nil, fmt.Errorf("all repo secret values for %q: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	values := make(map[string][]byte)
	for rows.Next() {
		var (
			name  string
			value []byte
		)
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("all repo secret values for %q: %w", repoID, err)
		}
		values[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("all repo secret values for %q: %w", repoID, err)
	}
	return values, nil
}

// ExposedSecretNamesForRun returns the names of secrets currently exposed by
// run runID (exposed_run_id = runID), ordered by name. Feeds the run chat
// header badge that warns an operator which secrets this run has leaked into
// its transcript. Empty slice, not nil, when the run has exposed none.
func (s *Store) ExposedSecretNamesForRun(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT name FROM repo_secrets WHERE exposed_run_id = ? ORDER BY name`), runID)
	if err != nil {
		return nil, fmt.Errorf("exposed secret names for run %q: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("exposed secret names for run %q: %w", runID, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("exposed secret names for run %q: %w", runID, err)
	}
	return names, nil
}

// scanRepoSecret reads one row in repoSecretColumns order.
func scanRepoSecret(scan func(dest ...any) error) (RepoSecret, error) {
	var (
		rs               RepoSecret
		created, updated string
		exposedRunID     sql.NullString
		exposedAt        sql.NullString
	)
	if err := scan(&rs.ID, &rs.RepoID, &rs.Name, &rs.Description, &created, &updated, &exposedRunID, &exposedAt); err != nil {
		return RepoSecret{}, err
	}
	var err error
	if rs.CreatedAt, err = parseTime(created); err != nil {
		return RepoSecret{}, err
	}
	if rs.UpdatedAt, err = parseTime(updated); err != nil {
		return RepoSecret{}, err
	}
	rs.ExposedRunID = nullStr(exposedRunID)
	if rs.ExposedAt, err = parseNullTime(exposedAt); err != nil {
		return RepoSecret{}, err
	}
	return rs, nil
}
