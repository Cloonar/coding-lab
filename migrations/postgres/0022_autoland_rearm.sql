-- 0022_autoland_rearm — makes autoland escalation terminality PR-scoped and
-- supersedable by an explicit human re-arm (postgres dialect; issue #188).
-- See the sqlite dialect of this migration for the full rationale; in short:
-- afk/<N> claim branches derive from the issue number, so a requeued issue
-- reuses its branch and a brand-new PR on it was inheriting the OLD PR's
-- spent autoland_attempts budget and even its permanent escalated
-- terminality — the gate has to move from branch-scoped to PR-scoped, and a
-- human has to be able to re-arm a specific PR without erasing history.
--
-- (a) runs.pull_number: the PR a run works, set by the three autoland kinds
-- (lander, fix, escalate). NULL means not an autoland run, or a row
-- predating this migration. Persisted rather than re-derived — 0020's
-- schedule_id precedent — because the gate must tell one PR on a reused
-- branch from the next across restarts. Plain ALTER (0019's cred_sig shape):
-- nothing here touches an existing CHECK, so no rebuild is warranted.
--
-- (b) autoland_attempts re-keyed from (repo_id, branch, kind) to (repo_id,
-- pull_number, kind); branch is gone, it was the bug. Existing rows carry no
-- PR identity to migrate onto, so DROP TABLE + CREATE TABLE rather than an
-- in-place ALTER; a fix loop in flight across the upgrade gets its budget
-- reset once, which is permissive and self-corrects within one loop.
-- Confirmed by grep that nothing in migrations/ REFERENCES autoland_attempts,
-- so the drop is safe inside the ordinary transaction. The kind CHECK gets an
-- explicit CONSTRAINT name (autoland_attempts_kind_check) instead of letting
-- postgres auto-generate one — 0013/0014/0020's naming discipline — so a
-- future migration can DROP/re-ADD it by name instead of having to guess.
--
-- (c) autoland_rearms: one row per PR, upserted (INSERT ... ON CONFLICT DO
-- UPDATE, app side) to the LATEST re-arm moment. A supersession record, not
-- an un-escalate — escalated run rows and marker comments stay history,
-- untouched — so the gate reads "terminal iff an escalation signal exists
-- AFTER the last re-arm" (an escalate marker's CreatedAt or an escalated
-- run's ended_at, compared against rearmed_at). A fresh escalation after a
-- re-arm is terminal again, indefinitely repeatable, with no history to keep
-- since only the newest re-arm matters.
--
-- rearmed_at is TEXT NOT NULL, not TIMESTAMPTZ: every timestamp in this
-- schema (created_at/started_at, 0001_baseline.sql onward) is TEXT rendered
-- through internal/store's single pinned RFC3339Nano-UTC layout, and a native
-- temporal column would be a second, divergent representation of the same
-- fact.

-- +goose Up
ALTER TABLE runs ADD COLUMN pull_number INTEGER;

DROP TABLE autoland_attempts;

CREATE TABLE autoland_attempts (
    repo_id     TEXT    NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    pull_number INTEGER NOT NULL,
    kind        TEXT    NOT NULL CONSTRAINT autoland_attempts_kind_check CHECK (kind IN ('fix', 'escalate')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, pull_number, kind)
);

CREATE TABLE autoland_rearms (
    repo_id     TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    pull_number INTEGER NOT NULL,
    rearmed_at  TEXT NOT NULL,
    PRIMARY KEY (repo_id, pull_number)
);

-- +goose Down
DROP TABLE autoland_rearms;

DROP TABLE autoland_attempts;

CREATE TABLE autoland_attempts (
    repo_id  TEXT    NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    branch   TEXT    NOT NULL,
    kind     TEXT    NOT NULL CHECK (kind IN ('fix', 'escalate')),
    attempts INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, branch, kind)
);

ALTER TABLE runs DROP COLUMN pull_number;
