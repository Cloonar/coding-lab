-- 0012_autoland_settings — the four per-repo Autoland knobs (sqlite dialect;
-- issue #181 / ADR-0048). Autoland is opt-in and default OFF: autoland_enabled
-- starts FALSE, so no repo gains a lander merely from this migration.
-- max_fix_attempts bounds fix-run SPAWNS (never rejection verdicts — a dead
-- fix run posts no comment, so counting verdicts would let it respawn
-- forever), default 2. auto_merge gates a clean PASS: on merges directly, off
-- downgrades to pr approve for a human to merge; default TRUE. lander_provider
-- is nullable — NULL means the lander run inherits this repo's own Provider
-- chain (a fix run inherits its authoring run's provider instead; ADR-0048).
-- The engine that reads these ships in a later issue; this migration only
-- adds the columns.

-- +goose Up
ALTER TABLE repos ADD COLUMN autoland_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE repos ADD COLUMN max_fix_attempts INTEGER NOT NULL DEFAULT 2;
ALTER TABLE repos ADD COLUMN auto_merge BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE repos ADD COLUMN lander_provider TEXT;

-- +goose Down
ALTER TABLE repos DROP COLUMN lander_provider;
ALTER TABLE repos DROP COLUMN auto_merge;
ALTER TABLE repos DROP COLUMN max_fix_attempts;
ALTER TABLE repos DROP COLUMN autoland_enabled;
