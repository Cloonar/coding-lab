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

// ActiveAFKRuns lists every outcome='active' run of the two AFK kinds,
// ordered by session name so a reaper tick is deterministic (v0
// trackAFKRuns returned runs sorted by name).
func (s *Store) ActiveAFKRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs
		 WHERE outcome = ? AND kind IN (?, ?)
		 ORDER BY session_name`),
		RunOutcomeActive, RunKindAFKManual, RunKindAFKAuto)
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
