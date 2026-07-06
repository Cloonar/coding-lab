package store

// Credential accessors (design §3a/§6). The store only ever sees the
// encrypted payload (vault nonce||ciphertext); plaintext handling lives in
// internal/vault. Payloads are write-only from the API's point of view:
// list/PATCH responses carry metadata, never payload bytes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Credential kinds (design §3a). ssh_key and https_token are GIT credentials
// (repos.credential_id); forge_token is the tracker credential
// (repos.forge_credential_id), server-side only — it must never reach a
// session's env or materialized files.
const (
	CredentialKindSSHKey     = "ssh_key"
	CredentialKindHTTPSToken = "https_token"
	CredentialKindForgeToken = "forge_token"
)

// Credential is one row of credentials, including the encrypted payload.
// Only server-internal callers (vault materialization, kind checks) receive
// it; API responses are built from CredentialMeta or the metadata fields.
type Credential struct {
	ID               string
	Name             string
	Kind             string
	EncryptedPayload []byte // AES-256-GCM nonce||ciphertext (vault format)
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CredentialMeta is a credential without its payload, plus the number of
// repos referencing it via either FK column (credential_id OR
// forge_credential_id). The API renders `referenced` as Referenced > 0.
type CredentialMeta struct {
	ID         string
	Name       string
	Kind       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Referenced int // count of referencing repos
}

// CreateCredential inserts a credential row. The caller supplies the cred_
// id (generated before encryption) and the clock. A UNIQUE name violation
// maps to ErrNameTaken.
func (s *Store) CreateCredential(ctx context.Context, id, name, kind string, encryptedPayload []byte, now time.Time) (Credential, error) {
	c := Credential{
		ID:               id,
		Name:             name,
		Kind:             kind,
		EncryptedPayload: encryptedPayload,
		CreatedAt:        storedTime(now),
		UpdatedAt:        storedTime(now),
	}
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO credentials (id, name, kind, encrypted_payload, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`),
		c.ID, c.Name, c.Kind, c.EncryptedPayload, fmtTime(c.CreatedAt), fmtTime(c.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return Credential{}, fmt.Errorf("create credential %q: %w", name, ErrNameTaken)
		}
		return Credential{}, fmt.Errorf("create credential %q: %w", name, err)
	}
	return c, nil
}

// Credentials lists every credential as metadata (no payload) with its
// referencing-repo count. The single LEFT JOIN matches a repo when EITHER FK
// column points at the credential, so each referencing repo counts exactly
// once even when both its columns do.
func (s *Store) Credentials(ctx context.Context) ([]CredentialMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.name, c.kind, c.created_at, c.updated_at, COUNT(r.id)
		 FROM credentials c
		 LEFT JOIN repos r ON r.credential_id = c.id OR r.forge_credential_id = c.id
		 GROUP BY c.id, c.name, c.kind, c.created_at, c.updated_at
		 ORDER BY c.name`)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	metas := make([]CredentialMeta, 0)
	for rows.Next() {
		var (
			m                CredentialMeta
			created, updated string
		)
		if err := rows.Scan(&m.ID, &m.Name, &m.Kind, &created, &updated, &m.Referenced); err != nil {
			return nil, fmt.Errorf("list credentials: %w", err)
		}
		if m.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("list credentials: %w", err)
		}
		if m.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, fmt.Errorf("list credentials: %w", err)
		}
		metas = append(metas, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	return metas, nil
}

// CredentialByID returns the credential with the given id — encrypted
// payload included — or ErrNotFound.
func (s *Store) CredentialByID(ctx context.Context, id string) (Credential, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT id, name, kind, encrypted_payload, created_at, updated_at
		 FROM credentials WHERE id = ?`), id)
	var (
		c                Credential
		created, updated string
	)
	if err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.EncryptedPayload, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Credential{}, fmt.Errorf("credential by id %q: %w", id, ErrNotFound)
		}
		return Credential{}, fmt.Errorf("credential by id %q: %w", id, err)
	}
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return Credential{}, fmt.Errorf("credential by id %q: %w", id, err)
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return Credential{}, fmt.Errorf("credential by id %q: %w", id, err)
	}
	return c, nil
}

// UpdateCredential renames and/or rotates a credential. A nil name leaves
// the name unchanged; a nil encryptedPayload leaves the payload unchanged
// (rotation writes a fresh ciphertext — the old payload is never read back).
// updated_at is stamped either way. ErrNotFound when the id does not exist;
// ErrNameTaken on a rename collision. The kind is immutable.
func (s *Store) UpdateCredential(ctx context.Context, id string, name *string, encryptedPayload []byte, now time.Time) error {
	sets := []string{"updated_at = ?"}
	args := []any{fmtTime(now)}
	if name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *name)
	}
	if encryptedPayload != nil {
		sets = append(sets, "encrypted_payload = ?")
		args = append(args, encryptedPayload)
	}
	args = append(args, id)

	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE credentials SET `+strings.Join(sets, ", ")+` WHERE id = ?`), args...)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("update credential %q: %w", id, ErrNameTaken)
		}
		return fmt.Errorf("update credential %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update credential %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("update credential %q: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteCredential removes a credential unless any repo still references it
// via credential_id OR forge_credential_id, in which case it returns a
// *ReferencedError (matches ErrReferenced) carrying the referencing-repo
// count. The check and the delete run in one transaction: on sqlite the
// single-writer connection serializes them; on postgres a concurrent
// reference insert makes the DELETE itself fail on the FK, which is also
// mapped to ErrReferenced — the row can never be deleted while referenced.
func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete credential %q: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	var refs int
	if err := tx.QueryRowContext(ctx, s.rebind(
		`SELECT COUNT(*) FROM repos WHERE credential_id = ? OR forge_credential_id = ?`),
		id, id).Scan(&refs); err != nil {
		return fmt.Errorf("delete credential %q: count references: %w", id, err)
	}
	if refs > 0 {
		return fmt.Errorf("delete credential %q: %w", id, &ReferencedError{Repos: refs})
	}

	res, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM credentials WHERE id = ?`), id)
	if err != nil {
		if isForeignKeyViolation(err) {
			// Lost a race with a repo insert after the count (postgres,
			// read-committed): the FK held the line. Report referenced with
			// the best count available.
			return fmt.Errorf("delete credential %q: %w", id, &ReferencedError{Repos: 1})
		}
		return fmt.Errorf("delete credential %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete credential %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete credential %q: %w", id, ErrNotFound)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete credential %q: %w", id, err)
	}
	return nil
}
