-- 0010_repo_secret_exposure — sticky exposed-flag for repo secrets (postgres dialect).
-- Issue #108: a secret value appearing in a run's transcript marks the
-- secret "exposed" — which run, when — sticky until the secret is rotated
-- (RotateRepoSecret clears both columns; rotation is the remediation, so the
-- flag's lifecycle rides the rotate action rather than getting its own
-- clear-exposure endpoint). exposed_run_id deliberately carries no FK: the
-- exposure record is forensic and must survive independently of the run
-- row's own lifecycle — a run can later be pruned without erasing the fact
-- that it once exposed a secret. exposed_at is TEXT, matching the
-- created_at/updated_at convention 0008 established for this table. Identical
-- to the sqlite dialect — unlike encrypted_value, no column here differs
-- between dialects.

-- +goose Up
ALTER TABLE repo_secrets ADD COLUMN exposed_run_id TEXT;
ALTER TABLE repo_secrets ADD COLUMN exposed_at TEXT;

-- +goose Down
ALTER TABLE repo_secrets DROP COLUMN exposed_at;
ALTER TABLE repo_secrets DROP COLUMN exposed_run_id;
