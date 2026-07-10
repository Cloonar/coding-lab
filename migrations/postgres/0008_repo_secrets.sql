-- 0008_repo_secrets — per-repo secret storage (postgres dialect).
-- Issue #104: secrets are named encrypted blobs scoped to one repo, unique by
-- name within the repo (UNIQUE(repo_id, name) → ErrNameTaken), cascade-deleted
-- with their repo. The store only ever sees the encrypted payload (vault
-- nonce||ciphertext); list/metadata reads never select encrypted_value.
-- Identical to the sqlite dialect except encrypted_value: BYTEA here, BLOB
-- there.

-- +goose Up
CREATE TABLE repo_secrets (
    id              TEXT PRIMARY KEY,
    repo_id         TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    encrypted_value BYTEA NOT NULL,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (repo_id, name)
);

-- +goose Down
DROP TABLE repo_secrets;
