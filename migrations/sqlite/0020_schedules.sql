-- 0020_schedules — the schedules table, the 'scheduled' run kind, and the
-- runs.schedule_id firing link (sqlite dialect; issue #247 / ADR-0062).
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
-- sqlite cannot ALTER a CHECK constraint, so widening runs.kind to admit
-- 'scheduled' rebuilds the table again — 0014's pattern, reproduced exactly —
-- and the SAME rebuild adds schedule_id, so one copy pays for both changes.
-- run_tokens (CASCADE), issue_comments.run_id and issues.run_id (both SET
-- NULL) all REFERENCE runs and the store's open recipe enforces
-- foreign_keys(1), so the rebuild runs outside goose's transaction with
-- foreign_keys OFF — otherwise DROP TABLE runs would cascade-delete every
-- run_token and NULL every child run_id. With foreign_keys OFF the rename also
-- leaves the children's REFERENCES clauses untouched (they keep naming "runs",
-- which the rename re-points at the rebuilt table). The store holds a single
-- connection (SetMaxOpenConns(1)), so the PRAGMA toggles below are guaranteed
-- to apply to the connection running the statements between them. Column
-- order, defaults, and both indexes reproduce 0014 plus 0019's cred_sig
-- exactly; schedule_id is appended last, where an ALTER would have put it.
-- Existing rows copy with schedule_id NULL — no run predating this migration
-- was ever a firing's.

-- +goose NO TRANSACTION

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

PRAGMA foreign_keys = OFF;

CREATE TABLE runs_new (
    id              TEXT PRIMARY KEY,
    repo_id         TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('manual', 'afk_manual', 'afk_auto', 'lander', 'fix', 'escalate', 'scheduled')),
    provider        TEXT NOT NULL,
    issue_number    INTEGER,
    branch          TEXT NOT NULL,
    worktree_path   TEXT NOT NULL,
    session_name    TEXT NOT NULL,
    model           TEXT NOT NULL,
    effort          TEXT NOT NULL,
    deep_link_url   TEXT,
    started_at      TEXT NOT NULL,
    budget_deadline TEXT, -- persisted budget clock (D12b)
    ended_at        TEXT,
    outcome         TEXT NOT NULL CHECK (outcome IN ('active', 'success', 'death', 'timeout', 'stopped', 'escalated')) DEFAULT 'active',
    failure_reason  TEXT,
    transcript_path TEXT,
    title           TEXT,
    remote          BOOLEAN NOT NULL DEFAULT FALSE,
    cred_sig        TEXT,
    schedule_id     TEXT REFERENCES schedules (id) ON DELETE SET NULL
);

INSERT INTO runs_new (id, repo_id, kind, provider, issue_number, branch,
    worktree_path, session_name, model, effort, deep_link_url, started_at,
    budget_deadline, ended_at, outcome, failure_reason, transcript_path,
    title, remote, cred_sig)
SELECT id, repo_id, kind, provider, issue_number, branch,
    worktree_path, session_name, model, effort, deep_link_url, started_at,
    budget_deadline, ended_at, outcome, failure_reason, transcript_path,
    title, remote, cred_sig
FROM runs;

DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;

CREATE INDEX idx_runs_repo_started ON runs (repo_id, started_at DESC);
CREATE INDEX idx_runs_outcome ON runs (outcome);

PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;

CREATE TABLE runs_new (
    id              TEXT PRIMARY KEY,
    repo_id         TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('manual', 'afk_manual', 'afk_auto', 'lander', 'fix', 'escalate')),
    provider        TEXT NOT NULL,
    issue_number    INTEGER,
    branch          TEXT NOT NULL,
    worktree_path   TEXT NOT NULL,
    session_name    TEXT NOT NULL,
    model           TEXT NOT NULL,
    effort          TEXT NOT NULL,
    deep_link_url   TEXT,
    started_at      TEXT NOT NULL,
    budget_deadline TEXT, -- persisted budget clock (D12b)
    ended_at        TEXT,
    outcome         TEXT NOT NULL CHECK (outcome IN ('active', 'success', 'death', 'timeout', 'stopped', 'escalated')) DEFAULT 'active',
    failure_reason  TEXT,
    transcript_path TEXT,
    title           TEXT,
    remote          BOOLEAN NOT NULL DEFAULT FALSE,
    cred_sig        TEXT
);

-- Fails loudly on any 'scheduled' kind row, and drops schedule_id with the
-- copy: narrowing a CHECK under live data is not silently lossy, matching the
-- dialect's other data-bearing Downs.
INSERT INTO runs_new (id, repo_id, kind, provider, issue_number, branch,
    worktree_path, session_name, model, effort, deep_link_url, started_at,
    budget_deadline, ended_at, outcome, failure_reason, transcript_path,
    title, remote, cred_sig)
SELECT id, repo_id, kind, provider, issue_number, branch,
    worktree_path, session_name, model, effort, deep_link_url, started_at,
    budget_deadline, ended_at, outcome, failure_reason, transcript_path,
    title, remote, cred_sig
FROM runs;

DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;

CREATE INDEX idx_runs_repo_started ON runs (repo_id, started_at DESC);
CREATE INDEX idx_runs_outcome ON runs (outcome);

PRAGMA foreign_keys = ON;

DROP TABLE schedules;
