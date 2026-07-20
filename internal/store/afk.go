package store

// AFK-engine accessors (design §3a; brief D12). The reaper enumerates active
// AFK runs from the runs table (the persisted budget clock, D12b — never an
// in-memory map), the scheduler asks for a repo's live auto run (one auto run
// per repo), and the reap chokepoint is the ONLY run-lifecycle writer of
// repos.consecutive_failures (three-strikes pause; a human Reset is the other
// writer, via ResetRepoFailures from the API layer).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ActiveAFKRuns lists every outcome='active' run of the unattended kinds —
// the two AFK kinds plus lander, fix, and escalate (the reaper owns all four
// autoland kinds' classification too, issue #182) — ordered by session name
// so a reaper tick is deterministic (v0 trackAFKRuns returned runs sorted by
// name).
func (s *Store) ActiveAFKRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs
		 WHERE outcome = ? AND kind IN (?, ?, ?, ?, ?)
		 ORDER BY session_name`),
		RunOutcomeActive, RunKindAFKManual, RunKindAFKAuto, RunKindLander,
		RunKindFix, RunKindEscalate)
	if err != nil {
		return nil, fmt.Errorf("active afk runs: %w", err)
	}
	return scanRuns(rows, "active afk runs")
}

// ActiveRunsByRepo lists a repo's outcome='active' runs (all kinds), newest
// first.
func (s *Store) ActiveRunsByRepo(ctx context.Context, repoID string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs
		 WHERE outcome = ? AND repo_id = ?
		 ORDER BY started_at DESC`),
		RunOutcomeActive, repoID)
	if err != nil {
		return nil, fmt.Errorf("active runs by repo %q: %w", repoID, err)
	}
	return scanRuns(rows, "active runs by repo "+repoID)
}

// ActiveAutoRunForRepo returns the repo's live (outcome='active') afk_auto
// run, if any — the scheduler's serial-per-repo gate re-checked inside the
// locked claim path. found=false with a nil error means no auto run is in
// flight.
func (s *Store) ActiveAutoRunForRepo(ctx context.Context, repoID string) (Run, bool, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs
		 WHERE outcome = ? AND repo_id = ? AND kind = ?
		 ORDER BY started_at DESC LIMIT 1`),
		RunOutcomeActive, repoID, RunKindAFKAuto)
	r, err := scanRun(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, false, nil
		}
		return Run{}, false, fmt.Errorf("active auto run for repo %q: %w", repoID, err)
	}
	return r, true, nil
}

// ActiveRunOnBranch reports whether any outcome='active' run — ANY kind —
// works branch in the repo: the autoland poller's runs-store gate (issue
// #181). The authoring AFK run still idling on its claim and a lander already
// validating it both suppress a lander spawn; the store answers with one count
// query, never a client-side scan. No index covers this predicate —
// idx_runs_outcome is on outcome alone and 'active' is its hot value, so it
// narrows almost nothing; the working set of active runs is small enough that
// the scan is cheap. Revisit if that stops being true.
func (s *Store) ActiveRunOnBranch(ctx context.Context, repoID, branch string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT COUNT(*) FROM runs
		 WHERE outcome = ? AND repo_id = ? AND branch = ?`),
		RunOutcomeActive, repoID, branch).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("active run on branch %q for repo %q: %w", branch, repoID, err)
	}
	return n > 0, nil
}

// AutolandAttempts reads the spawn-intent counter for (repoID, branch, kind)
// — the fix-forward loop's attempt bounds (issue #182 / ADR-0048). Absent row
// means zero, the most permissive value, so a repo that has never spawned the
// kind needs no seeding.
//
// This, NOT a count of runs rows, is the bounds' source of truth. A runs row
// records a spawn that reached a live session: Start's rollback deletes it on
// every failure after CreateRun, and the failures before CreateRun (a stale
// worktree at the fix label, a seeding failure, a secrets read) never write
// one. Counting rows therefore lets a deterministically-failing launch retry
// every tick forever, never burning an attempt and never reaching escalation.
// RecordAutolandAttempt burns the attempt at the launch chokepoint instead.
func (s *Store) AutolandAttempts(ctx context.Context, repoID, branch, kind string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT attempts FROM autoland_attempts
		 WHERE repo_id = ? AND branch = ? AND kind = ?`),
		repoID, branch, kind).Scan(&n)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("autoland %s attempts for branch %q in repo %q: %w", kind, branch, repoID, err)
	}
	return n, nil
}

// RecordAutolandAttempt burns one attempt for (repoID, branch, kind) — called
// once per launch the autoland pass actually reaches the launch pad with, for
// the bounded kinds only (issue #182 / ADR-0048). Idempotent by upsert, never
// by run identity: two attempts on one branch are two attempts, which is the
// whole point of bounding intents rather than surviving rows.
func (s *Store) RecordAutolandAttempt(ctx context.Context, repoID, branch, kind string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO autoland_attempts (repo_id, branch, kind, attempts)
		 VALUES (?, ?, ?, 1)
		 ON CONFLICT (repo_id, branch, kind)
		 DO UPDATE SET attempts = autoland_attempts.attempts + 1`),
		repoID, branch, kind)
	if err != nil {
		return fmt.Errorf("record autoland %s attempt for branch %q in repo %q: %w", kind, branch, repoID, err)
	}
	return nil
}

// AuthoringRunForBranch returns the latest afk_manual/afk_auto run for
// (repoID, branch) — the run that authored the claim, whose provider/model/
// effort a fix run inherits (issue #182 / ADR-0048). found=false with a nil
// error when no authoring run exists. Any outcome: the authoring run is
// normally already success-reaped by the time a fix run spawns (its PR
// exists), so outcome is deliberately not filtered.
func (s *Store) AuthoringRunForBranch(ctx context.Context, repoID, branch string) (Run, bool, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs
		 WHERE repo_id = ? AND branch = ? AND kind IN (?, ?)
		 ORDER BY started_at DESC LIMIT 1`),
		repoID, branch, RunKindAFKManual, RunKindAFKAuto)
	r, err := scanRun(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, false, nil
		}
		return Run{}, false, fmt.Errorf("authoring run for branch %q in repo %q: %w", branch, repoID, err)
	}
	return r, true, nil
}

// EscalatedRunOnBranch reports whether an outcome='escalated' run exists for
// (repoID, branch) — the poller's permanent-terminality gate (issue #182 /
// ADR-0048). An escalated-outcome run makes the PR invisible to autoland
// forever, even if the escalate marker comment is later deleted from the
// forge: this store row, not the comment, is the durable terminal signal.
func (s *Store) EscalatedRunOnBranch(ctx context.Context, repoID, branch string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT COUNT(*) FROM runs
		 WHERE outcome = ? AND repo_id = ? AND branch = ?`),
		RunOutcomeEscalated, repoID, branch).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("escalated run on branch %q for repo %q: %w", branch, repoID, err)
	}
	return n > 0, nil
}

// IncrementRepoFailures adds one to a repo's consecutive-failure counter and
// returns the new value — the reap chokepoint's death/timeout accounting
// (three-strikes pause: the AFK scheduler compares the counter against its
// threshold; the store just counts). ErrNotFound for a missing repo.
func (s *Store) IncrementRepoFailures(ctx context.Context, repoID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`UPDATE repos SET consecutive_failures = consecutive_failures + 1
		 WHERE id = ? RETURNING consecutive_failures`), repoID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("increment failures for repo %q: %w", repoID, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("increment failures for repo %q: %w", repoID, err)
	}
	return n, nil
}

// ResetRepoFailures zeroes a repo's consecutive-failure counter — the success
// reap re-arming a three-strikes pause, and the human POST reset (the only
// un-pause, ADR-0007: never automatic). It reports whether the counter
// actually changed (a no-op reset on an already-zero counter is not a
// change), so callers publish repo.changed only on real transitions. A
// missing repo is indistinguishable from an already-zero counter here;
// callers that must 404 verify the repo first.
func (s *Store) ResetRepoFailures(ctx context.Context, repoID string) (changed bool, err error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE repos SET consecutive_failures = 0
		 WHERE id = ? AND consecutive_failures <> 0`), repoID)
	if err != nil {
		return false, fmt.Errorf("reset failures for repo %q: %w", repoID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reset failures for repo %q: %w", repoID, err)
	}
	return n > 0, nil
}
