-- 0022_autoland_rearm — makes autoland escalation terminality PR-scoped and
-- supersedable by an explicit human re-arm (sqlite dialect; issue #188).
--
-- Three changes, one migration, because they are one fix: today's terminality
-- gate is keyed on the claim branch, and afk/<N> claim branches derive from
-- the issue number — so a requeued issue reuses its branch, and a brand-new
-- PR opened on that branch inherits whatever the OLD PR left behind: spent
-- fix/escalate budgets (autoland_attempts, 0015) and, worse, permanent
-- escalated terminality (0014) that was never this PR's to inherit. The gate
-- has to move from "this branch is done" to "this PR is done", and a human
-- has to be able to say "no, try again" for a specific PR without that
-- statement being erased by history.
--
-- (a) runs.pull_number — the PR a run works, set by the three autoland kinds
-- (lander, fix, escalate) whose launch paths already hold the pull number
-- they're validating/re-engaging/escalating. NULL means "not an autoland run,
-- or a row predating this migration" — manual/afk_manual/afk_auto/scheduled
-- runs never touch a PR this way. This mirrors 0020's schedule_id: run
-- identity that must be PERSISTED rather than re-derived, because the
-- escalation-terminality gate has to be able to tell one PR on a reused claim
-- branch from the next one across restarts, and the branch alone can't do
-- that — only the PR number can. Added with a plain ALTER (0019's cred_sig
-- shape) rather than 0020's full-table rebuild: nothing here widens a CHECK
-- or touches an existing constraint, so there is nothing a rebuild would buy.
--
-- (b) autoland_attempts re-keyed from (repo_id, branch, kind) to (repo_id,
-- pull_number, kind) — see 0015 for why the table exists at all (a spawn
-- INTENT counter, burned at the launch chokepoint, not a count of runs rows).
-- branch is GONE: it is exactly the identity that let a new PR inherit an old
-- PR's spent budget, which is the bug this migration fixes. Existing rows
-- carry no PR identity to migrate onto — a branch-keyed attempts row was
-- never told which PR it belonged to, because until now it didn't need to
-- know — so they are dropped outright (DROP TABLE, not an in-place ALTER)
-- rather than invented. A fix loop in flight across the upgrade gets its
-- budget reset once: permissive, not a lie the gate would then trust, and it
-- self-corrects within one loop since the next attempt re-derives the true
-- count against the real bound. Verified nothing else in migrations/
-- REFERENCES autoland_attempts (grep across both dialect trees, clean), so
-- the drop-and-recreate is safe inside goose's ordinary transaction — no
-- PRAGMA foreign_keys dance, no NO TRANSACTION, unlike 0020's runs rebuild.
--
-- (c) autoland_rearms — the durable re-arm record, one row per PR holding the
-- LATEST re-arm moment (INSERT ... ON CONFLICT DO UPDATE from the app side;
-- re-arm is indefinitely repeatable and only the newest gesture matters, so
-- history of earlier re-arms is not worth keeping). This is deliberately a
-- SUPERSESSION record, not an un-escalate: an escalated run row, and any
-- escalation marker comment, is history and is never deleted or rewritten —
-- doing that would falsify the record of what actually happened. Instead the
-- terminality gate becomes relational: terminal iff an escalation signal
-- exists AFTER the last re-arm, comparing an escalate marker comment's
-- CreatedAt or an escalated run's ended_at against rearmed_at. That makes a
-- fresh escalation after a re-arm terminal again automatically — re-arm
-- doesn't "clear" a state the gate then forgets to re-check, it just moves
-- the goalposts the gate was always comparing against — and re-arming stays
-- indefinitely repeatable with no cap and no history to prune.
--
-- rearmed_at is TEXT NOT NULL, matching every other timestamp in this schema
-- (created_at/started_at etc., 0001_baseline.sql onward): internal/store
-- renders every timestamp through one pinned RFC3339Nano-UTC layout, so a
-- native temporal column would be a second, divergent representation of the
-- same fact. No TIMESTAMPTZ.

-- +goose Up
ALTER TABLE runs ADD COLUMN pull_number INTEGER;

DROP TABLE autoland_attempts;

CREATE TABLE autoland_attempts (
    repo_id     TEXT    NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    pull_number INTEGER NOT NULL,
    kind        TEXT    NOT NULL CHECK (kind IN ('fix', 'escalate')),
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
