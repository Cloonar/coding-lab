-- 0015_autoland_attempts — durable per-(repo, branch, kind) spawn-intent
-- counters for the bounded autoland kinds (sqlite dialect; issue #182 /
-- ADR-0048).
--
-- Why a table of its own rather than counting runs rows: the runs row is NOT
-- a record of a spawn INTENT, it is a record of a spawn that reached a live
-- session. Start's rollback deletes the row on every failure after CreateRun
-- (instance/launch.go), and the failures BEFORE CreateRun — a stale worktree
-- at the fix label, a seeding failure, a secrets read — never write one at
-- all. Counting runs rows therefore lets a deterministically-failing launch
-- retry every tick without ever burning an attempt or reaching escalation,
-- which is the exact unbounded respawn the bound exists to prevent. This
-- counter is incremented at the launch chokepoint instead, so an attempt is
-- burned by the ATTEMPT, whatever the launch does next.
--
-- Both bounded kinds share the table: 'fix' bounds against the repo's
-- max_fix_attempts (0012), 'escalate' against afk.MaxEscalateAttempts. The
-- escalate arm needs a bound of its own because an escalate run that dies
-- before posting its marker leaves no 'escalated' outcome row, so the poller
-- re-derives the identical rejected-at-bound state next tick — and escalate
-- is excluded from the three-strikes counter, so nothing else brakes it.
--
-- Rows are keyed on branch, matching the poller's other per-branch gates, and
-- cascade with the repo. Not counted per PR: the claim branch is the loop's
-- identity, and the PR number is not known at every gate that reads this.

-- +goose Up
CREATE TABLE autoland_attempts (
    repo_id  TEXT    NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    branch   TEXT    NOT NULL,
    kind     TEXT    NOT NULL CHECK (kind IN ('fix', 'escalate')),
    attempts INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_id, branch, kind)
);

-- +goose Down
DROP TABLE autoland_attempts;
