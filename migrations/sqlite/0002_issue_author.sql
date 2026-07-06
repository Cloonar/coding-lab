-- 0002_issue_author — issue author attribution (sqlite dialect).
-- Issues gain the same author identity issue_comments carry (ADR-0014): the
-- agent API's issue create attributes builtin issues to the originating run,
-- so agent-authored tracker content is distinguishable from operator-authored.
-- Existing rows were all operator-created (the only create path before the
-- agent surface), so the backfill default is 'operator'.

-- +goose Up
ALTER TABLE issues ADD COLUMN author_kind TEXT NOT NULL DEFAULT 'operator' CHECK (author_kind IN ('operator', 'run'));
ALTER TABLE issues ADD COLUMN run_id TEXT REFERENCES runs (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE issues DROP COLUMN run_id;
ALTER TABLE issues DROP COLUMN author_kind;
