-- 0013_lander_run_kind — widen runs.kind to admit 'lander' (sqlite dialect;
-- issue #181 / ADR-0048). A lander run validates a claim's PR on the EXISTING
-- head branch; it is a fourth run kind beside manual/afk_manual/afk_auto.
--
-- sqlite cannot ALTER a CHECK constraint, so the runs table is rebuilt: create
-- the widened table, copy every row, drop the old, rename. run_tokens and
-- issue_comments both REFERENCE runs and the store's open recipe enforces
-- foreign_keys(1), so the rebuild runs outside goose's transaction with
-- foreign_keys OFF — otherwise DROP TABLE runs would cascade-delete every
-- run_token and NULL every issue_comments.run_id. With foreign_keys OFF the
-- rename also leaves the children's REFERENCES clauses untouched (they keep
-- naming "runs", which the rename re-points at the rebuilt table). The store
-- holds a single connection (SetMaxOpenConns(1)), so the PRAGMA toggles below
-- are guaranteed to apply to the connection running the statements between
-- them. Column order, defaults, and both indexes reproduce the baseline plus
-- 0003 (transcript_path), 0009 (title), and 0011 (remote) exactly.

-- +goose NO TRANSACTION

-- +goose Up
PRAGMA foreign_keys = OFF;

CREATE TABLE runs_new (
    id              TEXT PRIMARY KEY,
    repo_id         TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('manual', 'afk_manual', 'afk_auto', 'lander')),
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
    outcome         TEXT NOT NULL CHECK (outcome IN ('active', 'success', 'death', 'timeout', 'stopped')) DEFAULT 'active',
    failure_reason  TEXT,
    transcript_path TEXT,
    title           TEXT,
    remote          BOOLEAN NOT NULL DEFAULT FALSE
);

INSERT INTO runs_new (id, repo_id, kind, provider, issue_number, branch,
    worktree_path, session_name, model, effort, deep_link_url, started_at,
    budget_deadline, ended_at, outcome, failure_reason, transcript_path,
    title, remote)
SELECT id, repo_id, kind, provider, issue_number, branch,
    worktree_path, session_name, model, effort, deep_link_url, started_at,
    budget_deadline, ended_at, outcome, failure_reason, transcript_path,
    title, remote
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
    kind            TEXT NOT NULL CHECK (kind IN ('manual', 'afk_manual', 'afk_auto')),
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
    outcome         TEXT NOT NULL CHECK (outcome IN ('active', 'success', 'death', 'timeout', 'stopped')) DEFAULT 'active',
    failure_reason  TEXT,
    transcript_path TEXT,
    title           TEXT,
    remote          BOOLEAN NOT NULL DEFAULT FALSE
);

-- Fails loudly on any 'lander' row: narrowing a CHECK under live data is not
-- silently lossy, matching the dialect's other data-bearing Downs.
INSERT INTO runs_new (id, repo_id, kind, provider, issue_number, branch,
    worktree_path, session_name, model, effort, deep_link_url, started_at,
    budget_deadline, ended_at, outcome, failure_reason, transcript_path,
    title, remote)
SELECT id, repo_id, kind, provider, issue_number, branch,
    worktree_path, session_name, model, effort, deep_link_url, started_at,
    budget_deadline, ended_at, outcome, failure_reason, transcript_path,
    title, remote
FROM runs;

DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;

CREATE INDEX idx_runs_repo_started ON runs (repo_id, started_at DESC);
CREATE INDEX idx_runs_outcome ON runs (outcome);

PRAGMA foreign_keys = ON;
