-- 0011_remote_control — remote-control as a layered spawn knob (sqlite dialect).
-- Issue #163 turns claude's hardcoded --remote-control into a lab-level knob
-- that layers exactly like provider/model/effort (ADR-0021): per-spawn pick →
-- repos.remote_default → the global spawn_remote_default, with the AFK-only
-- repos.afk_remote_default (→ spawn_remote_default_afk) consulted FIRST for
-- unattended runs. Both repo columns are NULLABLE, and that nullability is the
-- whole design: unlike every knob so far, FALSE is a LEGAL value here, so the
-- empty-string "unset" sentinel the TEXT knobs rely on has no boolean
-- equivalent — NULL is the only honest way to spell inherit (the Go fields are
-- *bool, never bare bool).
--
-- runs.remote is NOT NULL: it is the RESOLVED value, stamped at launch, and it
-- must be a real column (not deferred to a generic options bag) because
-- deep-link capture arms at boot RE-ADOPTION as well as at Start — after a
-- restart lab has only the runs row to tell it whether a live session is remote.
--
-- The backfill is mandatory, not cosmetic: every run that exists today was
-- launched with --remote-control, and their claude.ai "Open" deep links must
-- keep working after the migration — so UPDATE runs SET remote = TRUE records
-- how they were actually spawned. (sqlite cannot add a NOT NULL column to a
-- non-empty table without a DEFAULT; DEFAULT FALSE satisfies the ALTER and the
-- explicit UPDATE then states the truth for the existing rows. New rows are
-- stamped by the resolver, so the default is never the value that matters.)
--
-- BOOLEAN follows the baseline's bool convention (repos.incogni). The matching
-- global settings keys (spawn_remote_default / spawn_remote_default_afk) are
-- plain settings rows and need no schema change.

-- +goose Up
ALTER TABLE repos ADD COLUMN remote_default BOOLEAN;
ALTER TABLE repos ADD COLUMN afk_remote_default BOOLEAN;
ALTER TABLE runs ADD COLUMN remote BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE runs SET remote = TRUE;

-- +goose Down
ALTER TABLE runs DROP COLUMN remote;
ALTER TABLE repos DROP COLUMN afk_remote_default;
ALTER TABLE repos DROP COLUMN remote_default;
