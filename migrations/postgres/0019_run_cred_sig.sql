-- 0019_run_cred_sig — the runs.cred_sig adopt-check baseline (postgres
-- dialect; issue #222). Server-owned credential authority: lab's rotation
-- loop injects rotated provider credentials into every live run's instance
-- HOME, and cred_sig is the opaque signature (existence+mtime+size digests —
-- never token contents) lab last injected there. NULL means "never stamped":
-- pre-upgrade rows, or no provider credentials existed at launch. A live home
-- whose CURRENT signature differs from this stamped baseline has
-- self-refreshed and must be adopted back into the master store before any
-- wipe; the column is persisted (not re-derived) so the comparison survives
-- lab restarts, when the startup sweep adopt-checks orphan homes after
-- downtime.

-- +goose Up
ALTER TABLE runs ADD COLUMN cred_sig TEXT;

-- +goose Down
ALTER TABLE runs DROP COLUMN cred_sig;
