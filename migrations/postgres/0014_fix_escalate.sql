-- 0014_fix_escalate — widen runs.kind to admit 'fix' and 'escalate', and
-- runs.outcome to admit 'escalated' (postgres dialect; issue #182 / ADR-0048).
-- A fix run re-engages a rejected claim's PR on the existing head branch,
-- carrying the rejection findings forward; an escalate run is the terminal
-- hand-off once the fix-attempt bound is spent. 'escalate' is a kind of its
-- own (not folded into 'lander') because the reaper's done-signal rule is
-- per-kind and the run's mode must survive a restart. 'escalated' is an
-- outcome of its own (not folded into 'success') because it is the poller's
-- permanent-terminality gate (issue #182): an outcome='escalated' row makes
-- the PR invisible to autoland forever.
--
-- runs_kind_check was explicitly named by 0013's re-ADD, so this re-ADD keeps
-- naming it. The outcome CHECK is still the baseline's auto-generated name
-- (postgres's <table>_<column>_check, confirmed unnamed in 0001_baseline.sql)
-- — this migration is the first to touch it, so it names it explicitly too,
-- the same forward-guessing-avoidance 0013 applied to kind.

-- +goose Up
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('manual', 'afk_manual', 'afk_auto', 'lander', 'fix', 'escalate'));

ALTER TABLE runs DROP CONSTRAINT runs_outcome_check;
ALTER TABLE runs ADD CONSTRAINT runs_outcome_check
    CHECK (outcome IN ('active', 'success', 'death', 'timeout', 'stopped', 'escalated'));

-- +goose Down
-- Fails loudly on any 'fix'/'escalate' kind or 'escalated' outcome row: ADD
-- CONSTRAINT validates existing data, so narrowing under live rows of either
-- kind is not silently lossy.
ALTER TABLE runs DROP CONSTRAINT runs_outcome_check;
ALTER TABLE runs ADD CONSTRAINT runs_outcome_check
    CHECK (outcome IN ('active', 'success', 'death', 'timeout', 'stopped'));

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('manual', 'afk_manual', 'afk_auto', 'lander'));
