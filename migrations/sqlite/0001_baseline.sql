-- 0001_baseline — full schema per design §3a (sqlite dialect).
-- Timestamps are TEXT, fixed-width UTC (store fmtTime); booleans are BOOLEAN;
-- IDs are app-generated TEXT (internal/ids), except web_sessions.id which is
-- the sha256 hex of the cookie token.

-- +goose Up
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

CREATE TABLE web_sessions (
    id           TEXT PRIMARY KEY, -- sha256 hex of the cookie token, not a prefixed ID
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    remember     BOOLEAN NOT NULL DEFAULT FALSE,
    last_seen_at TEXT
);

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL,
    last_used_at TEXT
);

CREATE TABLE credentials (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    kind              TEXT NOT NULL CHECK (kind IN ('ssh_key', 'https_token', 'forge_token')),
    encrypted_payload BLOB NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

CREATE TABLE repos (
    id                     TEXT PRIMARY KEY,
    name                   TEXT NOT NULL UNIQUE, -- sanitized
    remote_url             TEXT NOT NULL,
    credential_id          TEXT REFERENCES credentials (id),       -- git credential: ssh_key | https_token only (app-enforced)
    forge_credential_id    TEXT REFERENCES credentials (id),       -- forge_token only; required when tracker_binding = 'forge'
    tracker_binding        TEXT NOT NULL CHECK (tracker_binding IN ('forge', 'builtin')),
    forge_kind             TEXT NOT NULL CHECK (forge_kind IN ('forgejo', 'github', 'none')),
    default_branch         TEXT NOT NULL DEFAULT 'main',
    provider               TEXT NOT NULL DEFAULT 'claude-code',
    model_default          TEXT,
    effort_default         TEXT,
    incogni                BOOLEAN NOT NULL DEFAULT FALSE,
    git_author_name        TEXT,
    git_author_email       TEXT,
    afk_branch_pattern     TEXT NOT NULL, -- 'afk/<N>'; incogni default 'issue-<N>'
    manual_branch_prefix   TEXT NOT NULL, -- 'lab/'; incogni default 'wip/'
    afk_auto_enabled       BOOLEAN NOT NULL DEFAULT FALSE,
    consecutive_failures   INTEGER NOT NULL DEFAULT 0,
    budget_minutes         INTEGER, -- override; NULL -> settings
    max_instances_override INTEGER, -- per-repo cap override; NULL -> settings
    clone_status           TEXT NOT NULL CHECK (clone_status IN ('cloning', 'ready', 'error')) DEFAULT 'cloning',
    clone_error            TEXT,
    next_issue_number      INTEGER NOT NULL DEFAULT 0,
    next_cr_number         INTEGER NOT NULL DEFAULT 0,
    created_at             TEXT NOT NULL,
    last_opened_at         TEXT
);

CREATE TABLE runs (
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
    failure_reason  TEXT
);

CREATE INDEX idx_runs_repo_started ON runs (repo_id, started_at DESC);
CREATE INDEX idx_runs_outcome ON runs (outcome);

CREATE TABLE run_tokens (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TEXT, -- AFK: budget_deadline + 30min; manual: NULL
    created_at TEXT NOT NULL
);

CREATE TABLE issues (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    number     INTEGER NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL CHECK (state IN ('open', 'closed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    closed_at  TEXT,
    UNIQUE (repo_id, number)
);

CREATE TABLE issue_comments (
    id          TEXT PRIMARY KEY,
    issue_id    TEXT NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    author_kind TEXT NOT NULL CHECK (author_kind IN ('operator', 'run')),
    run_id      TEXT REFERENCES runs (id) ON DELETE SET NULL,
    body        TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE TABLE labels (
    id          TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    color       TEXT NOT NULL DEFAULT '#6b7280',
    description TEXT NOT NULL DEFAULT '',
    UNIQUE (repo_id, name)
);

CREATE TABLE issue_labels (
    issue_id TEXT NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    label_id TEXT NOT NULL REFERENCES labels (id) ON DELETE CASCADE,
    PRIMARY KEY (issue_id, label_id)
);

CREATE TABLE change_requests (
    id           TEXT PRIMARY KEY,
    repo_id      TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    number       INTEGER NOT NULL,
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',
    head_branch  TEXT NOT NULL,
    base_branch  TEXT NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('open', 'merged', 'closed')),
    merged_at    TEXT,
    merge_commit TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE (repo_id, number)
);

CREATE TABLE cr_closes (
    cr_id        TEXT NOT NULL REFERENCES change_requests (id) ON DELETE CASCADE,
    issue_number INTEGER NOT NULL,
    PRIMARY KEY (cr_id, issue_number)
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- +goose Down
DROP TABLE settings;
DROP TABLE cr_closes;
DROP TABLE change_requests;
DROP TABLE issue_labels;
DROP TABLE labels;
DROP TABLE issue_comments;
DROP TABLE issues;
DROP TABLE run_tokens;
DROP TABLE runs;
DROP TABLE repos;
DROP TABLE credentials;
DROP TABLE api_tokens;
DROP TABLE web_sessions;
DROP TABLE users;
