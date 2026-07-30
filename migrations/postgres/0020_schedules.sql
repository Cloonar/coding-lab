-- 0020_schedules — the schedules table, the 'scheduled' run kind, and the
-- runs.schedule_id firing link (postgres dialect; issue #247 / ADR-0062).
--
-- A Schedule is the per-repo cadence object: a name unique within its repo, a
-- cron expression, a freeform prompt, an ordered selection of built-in flows,
-- an enabled flag, optional knob overrides, and its own consecutive-failure
-- counter with a paused state. When the one spawn pass (ADR-0049) crosses a
-- cron match the Schedule fires a scheduled run — an AFK-run sibling with no
-- issue, no claim, and no done-signal, terminated by its budget clock alone.
--
-- The firing links its run by id rather than by parsing a label grammar: the
-- skip-on-overlap rule and the per-Schedule failure counter both need to
-- attribute a run to its Schedule, and a label parse would be a second source
-- of truth for a fact this column carries directly.
--
-- Why each nullable is nullable — the inherit grammar the rest of the schema
-- already speaks:
--   budget_minutes NULL      the 30-minute scheduled-run default (repos.
--                            budget_minutes' precedent: NULL defers to the
--                            layer above rather than duplicating the number).
--   model NULL               inherit the AFK-default layering (ADR-0021/0030
--   effort NULL              skip-layer resolution: repo AFK override, then
--   provider NULL            the globals, then the provider's own default).
--                            A stored override a provider switch orphaned
--                            must degrade a firing to the inherited chain,
--                            never break every firing at 06:00 forever.
--   last_fired_at NULL       never fired. It is the startup missed-slot log's
--                            only input; no-catch-up (ADR-0062) means a stale
--                            value can never replay a backlog.
--   runs.schedule_id NULL    this run is not a firing's — and ON DELETE SET
--                            NULL so deleting a Schedule never kills its live
--                            run: the run keeps its budget clock and reaps
--                            normally, it just loses its parent link.
--
-- cadence and flows carry no CHECK: the five-field cron grammar lives in
-- internal/cronx and the flow catalog is versioned with the binary, so a DB
-- constraint would be a second, always-stale source of truth. App-side enum
-- validation for a new table is the repos.runner precedent (0017).
--
-- Divergence from the sqlite dialect, which is otherwise column-for-column
-- identical: sqlite cannot ALTER a CHECK constraint, so there the whole runs
-- table is rebuilt (0014's pattern) to widen kind and add schedule_id in one
-- copy, outside goose's transaction with foreign_keys OFF. Postgres swaps the
-- constraint in place and adds the column with a plain ALTER, inside the
-- normal migration transaction — no rebuild, no PRAGMA dance, and no risk to
-- the tables referencing runs. runs_kind_check has been explicitly named since
-- 0013's re-ADD, so this re-ADD keeps naming it.

-- +goose Up
CREATE TABLE schedules (
    id                   TEXT PRIMARY KEY,
    repo_id              TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name                 TEXT NOT NULL,
    cadence              TEXT NOT NULL,             -- cron expression, validated app-side
    prompt               TEXT NOT NULL DEFAULT '',
    flows                TEXT NOT NULL DEFAULT '[]', -- JSON array of flow keys, validated app-side
    enabled              BOOLEAN NOT NULL DEFAULT TRUE,
    budget_minutes       INTEGER,                   -- NULL = the 30-minute default
    model                TEXT,                      -- NULL = inherit the AFK-default layering
    effort               TEXT,                      -- NULL = inherit
    provider             TEXT,                      -- NULL = inherit
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    paused               BOOLEAN NOT NULL DEFAULT FALSE,
    last_fired_at        TEXT,                      -- NULL until the first firing
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL,
    UNIQUE (repo_id, name)
);

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('manual', 'afk_manual', 'afk_auto', 'lander', 'fix', 'escalate', 'scheduled'));

ALTER TABLE runs ADD COLUMN schedule_id TEXT REFERENCES schedules (id) ON DELETE SET NULL;

-- +goose Down
-- Narrowing fails loudly on any 'scheduled' kind row: ADD CONSTRAINT validates
-- existing data, so the reverse is not silently lossy. Dropping schedule_id
-- first also drops the FK that would otherwise block DROP TABLE schedules.
ALTER TABLE runs DROP COLUMN schedule_id;

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('manual', 'afk_manual', 'afk_auto', 'lander', 'fix', 'escalate'));

DROP TABLE schedules;
