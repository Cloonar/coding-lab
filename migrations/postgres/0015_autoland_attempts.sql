-- 0015_autoland_attempts — durable per-(repo, branch, kind) spawn-intent
-- counters for the bounded autoland kinds (postgres dialect; issue #182 /
-- ADR-0048). See the sqlite dialect of this migration for the full rationale;
-- in short: a runs row records a spawn that REACHED a live session (Start's
-- rollback deletes it on every post-CreateRun failure, and pre-CreateRun
-- failures never write one), so counting runs rows lets a deterministically
-- failing launch retry forever without burning an attempt. This counter is
-- incremented at the launch chokepoint, so the ATTEMPT burns it.
--
-- 'fix' bounds against repos.max_fix_attempts (0012); 'escalate' against
-- afk.MaxEscalateAttempts, which the escalate arm needs because an escalate
-- run that dies before posting its marker leaves no 'escalated' outcome row
-- and is excluded from the three-strikes counter.

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
