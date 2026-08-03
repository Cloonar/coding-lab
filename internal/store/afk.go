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
	"time"
)

// ActiveAFKRuns lists every outcome='active' run of the unattended kinds —
// the two AFK kinds plus lander, fix, and escalate (the reaper owns all four
// autoland kinds' classification too, issue #182) and scheduled (a Schedule's
// firing, issue #247 / ADR-0062: unattended and reaper-owned like the rest,
// so it must be in this list or the reaper never sees the run and its budget
// clock never fires) — ordered by session name so a reaper tick is
// deterministic (v0 trackAFKRuns returned runs sorted by name).
func (s *Store) ActiveAFKRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs
		 WHERE outcome = ? AND kind IN (?, ?, ?, ?, ?, ?)
		 ORDER BY session_name`),
		RunOutcomeActive, RunKindAFKManual, RunKindAFKAuto, RunKindLander,
		RunKindFix, RunKindEscalate, RunKindScheduled)
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

// ActiveRunForSchedule returns the live (outcome='active') run scheduleID
// fired, or ErrNotFound when the Schedule has nothing in flight — the
// skip-on-overlap gate (issue #247 / ADR-0062: a firing that comes due while
// the previous run is still live is consumed and logged, never queued). It
// matches on runs.schedule_id, the durable firing link, so a restart between
// launch and the next pass cannot lose the attribution. Newest first for the
// pathological case of two live rows (a Schedule deleted and recreated onto a
// run's link cannot happen — ON DELETE SET NULL clears it — but the LIMIT
// keeps the reader total either way).
func (s *Store) ActiveRunForSchedule(ctx context.Context, scheduleID string) (Run, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs
		 WHERE outcome = ? AND schedule_id = ?
		 ORDER BY started_at DESC LIMIT 1`),
		RunOutcomeActive, scheduleID)
	r, err := scanRun(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, fmt.Errorf("active run for schedule %q: %w", scheduleID, ErrNotFound)
		}
		return Run{}, fmt.Errorf("active run for schedule %q: %w", scheduleID, err)
	}
	return r, nil
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

// AutolandAttempts reads the spawn-intent counter for (repoID, pullNumber,
// kind) — the fix-forward loop's attempt bounds (issue #182 / ADR-0048).
// Absent row means zero, the most permissive value, so a PR that has never
// spawned the kind needs no seeding.
//
// This, NOT a count of runs rows, is the bounds' source of truth. A runs row
// records a spawn that reached a live session: Start's rollback deletes it on
// every failure after CreateRun, and the failures before CreateRun (a stale
// worktree at the fix label, a seeding failure, a secrets read) never write
// one. Counting rows therefore lets a deterministically-failing launch retry
// every tick forever, never burning an attempt and never reaching escalation.
// RecordAutolandAttempt burns the attempt at the launch chokepoint instead.
//
// The key is the PR, not the claim branch (issue #188 / migration 0022):
// afk/<N> claim branches derive from the ISSUE number, so discarding an
// escalated run and letting a fresh AFK run re-claim the issue reuses the
// branch — and the brand-new PR opened on it inherited the discarded PR's
// spent budget, starting life out of fix attempts without ever having been
// validated once. Only the pull number tells one PR on a reused branch from
// the next.
func (s *Store) AutolandAttempts(ctx context.Context, repoID string, pullNumber int, kind string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT attempts FROM autoland_attempts
		 WHERE repo_id = ? AND pull_number = ? AND kind = ?`),
		repoID, pullNumber, kind).Scan(&n)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("autoland %s attempts for pull %d in repo %q: %w", kind, pullNumber, repoID, err)
	}
	return n, nil
}

// RecordAutolandAttempt burns one attempt for (repoID, pullNumber, kind) —
// called once per launch the autoland pass actually reaches the launch pad
// with, for the bounded kinds only (issue #182 / ADR-0048). Idempotent by
// upsert, never by run identity: two attempts on one PR are two attempts,
// which is the whole point of bounding intents rather than surviving rows.
// Keyed on the pull rather than the claim branch for the reason
// AutolandAttempts spells out (issue #188): a requeued issue reuses its
// afk/<N> branch, and the new PR must not be born owing the old PR's budget.
func (s *Store) RecordAutolandAttempt(ctx context.Context, repoID string, pullNumber int, kind string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO autoland_attempts (repo_id, pull_number, kind, attempts)
		 VALUES (?, ?, ?, 1)
		 ON CONFLICT (repo_id, pull_number, kind)
		 DO UPDATE SET attempts = autoland_attempts.attempts + 1`),
		repoID, pullNumber, kind)
	if err != nil {
		return fmt.Errorf("record autoland %s attempt for pull %d in repo %q: %w", kind, pullNumber, repoID, err)
	}
	return nil
}

// EscalatedRunForPull returns the moment autoland's terminality was recorded
// for (repoID, pullNumber) — the newest outcome='escalated' run's ended_at,
// falling back to started_at for a row that somehow reached the outcome
// without one — and ok=false when no such run exists (issue #188 / ADR-0048's
// amendment).
//
// It returns a TIME, not a bool, because terminality stopped being permanent.
// An escalated run says "as of these N attempts, agents could not finish
// this", and a human re-arm (RearmPull) is new information that supersedes the
// statement; the gate is therefore relational — terminal iff an escalation
// signal exists AFTER the last re-arm — and the caller needs this instant to
// compare against PullRearmedAt. A bool would throw away the only fact the
// comparison needs. The escalated row itself is never deleted or rewritten:
// supersession, not erasure, because the row is the record of what happened.
//
// No branch parameter: (repo_id, pull_number) already identifies the PR within
// the repo, and a redundant branch predicate would only add a way for the two
// to disagree — which is precisely the failure this issue is fixing.
//
// The newest row is computed in GO, never with ORDER BY ... LIMIT 1, and that
// is deliberate. Two reasons, both worth the extra scan (there are at most
// MaxEscalateAttempts escalated rows per PR, so it is trivially cheap):
//
//   - The instant we want is COALESCE(ended_at, started_at), so ordering must
//     use the coalesce too. Ordering by ended_at while selecting the fallback
//     — the natural way to write it — silently picks the wrong row whenever a
//     later-started run ended earlier, which the interleaving of concurrent
//     escalate attempts makes entirely ordinary.
//   - A lexicographic ORDER BY over these columns is only correct because
//     store.go's pinned timeFormat is fixed-width UTC ("…T15:04:05.000Z"), an
//     invariant one careless writer away from breaking: a raw INSERT rendered
//     with time.RFC3339Nano drops trailing zeros, and "…:00Z" then sorts AFTER
//     "…:00.500Z" — later text for an earlier instant. Parsing with the
//     store's own parser and taking the max over time.Time depends on no such
//     invariant.
//
// Upgrade path, stated so it is not mistaken for an oversight: a run row
// predating migration 0022 has pull_number NULL and can therefore never match
// this query (SQL equality against NULL is never true). That is safe. ADR-0048
// writes outcome='escalated' ONLY for an escalate run whose escalate marker
// comment actually landed, so every pre-0022 escalated PR still carries that
// marker and stays terminal through the comment half of the poller's gate. The
// only PRs that lose terminality across the upgrade are the ones whose human
// deliberately deleted the marker — itself a statement of intent, and the
// same statement a re-arm would have made explicitly.
func (s *Store) EscalatedRunForPull(ctx context.Context, repoID string, pullNumber int) (time.Time, bool, error) {
	doing := fmt.Sprintf("escalated run for pull %d in repo %q", pullNumber, repoID)
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT started_at, ended_at FROM runs
		 WHERE outcome = ? AND repo_id = ? AND pull_number = ?`),
		RunOutcomeEscalated, repoID, pullNumber)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s: %w", doing, err)
	}
	defer func() { _ = rows.Close() }()

	var newest time.Time
	found := false
	for rows.Next() {
		var (
			started string
			ended   sql.NullString
		)
		if err := rows.Scan(&started, &ended); err != nil {
			return time.Time{}, false, fmt.Errorf("%s: %w", doing, err)
		}
		at, err := parseTime(started)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("%s: %w", doing, err)
		}
		if ended.Valid {
			if at, err = parseTime(ended.String); err != nil {
				return time.Time{}, false, fmt.Errorf("%s: %w", doing, err)
			}
		}
		if !found || at.After(newest) {
			newest, found = at, true
		}
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, false, fmt.Errorf("%s: %w", doing, err)
	}
	return newest, found, nil
}

// PullRearmedAt returns the moment (repoID, pullNumber) was last re-armed; a
// zero Time means never (issue #188 / ADR-0048's amendment). It is the right
// half of the terminality comparison: the poller folds an escalation signal —
// EscalatedRunForPull's instant, or an escalate marker comment's CreatedAt —
// as terminal only when that instant is AFTER this one.
//
// An absent row is not an error, mirroring AutolandAttempts' absent-row-means-
// zero rule: "never re-armed" is the overwhelmingly common state and the zero
// Time answers it exactly — every real escalation instant is after it, so the
// comparison keeps working with no special case at the call site.
func (s *Store) PullRearmedAt(ctx context.Context, repoID string, pullNumber int) (time.Time, error) {
	var at string
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT rearmed_at FROM autoland_rearms
		 WHERE repo_id = ? AND pull_number = ?`),
		repoID, pullNumber).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, fmt.Errorf("rearm moment for pull %d in repo %q: %w", pullNumber, repoID, err)
	}
	t, err := parseTime(at)
	if err != nil {
		return time.Time{}, fmt.Errorf("rearm moment for pull %d in repo %q: %w", pullNumber, repoID, err)
	}
	return t, nil
}

// RearmPull records a human re-arm of (repoID, pullNumber) at instant at and
// zeroes the PR's fix and escalate attempt budgets, atomically (issue #188 /
// ADR-0048's amendment). at is normalized through the store's
// storedTime/fmtTime path before it is written, exactly as CreateRun does, so
// PullRearmedAt hands back precisely the instant the database holds rather
// than a value that differs from the caller's by sub-millisecond noise the
// comparison would then have to tolerate.
//
// The two halves are ONE transaction because either alone is a bug that looks
// like a silent no-op. Clearing terminality while leaving the fix budget spent
// means the very next lander rejection escalates again immediately — the
// worst possible outcome, since to the human it reads as "the re-arm did not
// work" rather than "the re-arm worked and the agents failed again". Zeroing
// the budget without the supersession record leaves the PR invisible to the
// poller, so the restored budget is never spent at all.
//
// Upsert rather than insert: re-arm is indefinitely repeatable and only the
// newest gesture matters, so the table keeps one row per PR holding the LATEST
// moment (migration 0022) — earlier re-arms are superseded by the same
// relation the gate itself uses, and there is no history to prune.
//
// Zeroing is a DELETE, not an UPDATE ... SET attempts = 0: an absent row
// already means zero (AutolandAttempts), so deleting IS zeroing, and it needs
// no per-kind enumeration that a future third bounded kind would silently fall
// out of.
//
// A repoID that does not exist maps to ErrNotFound via the autoland_rearms FK
// — the package's convention for a write against a vanished parent (CreateRun,
// AddIssueLabel). Defensive only: the httpapi caller has already loaded the
// repo to authorize the action, so this is the vanished-between-check-and-write
// race, not the ordinary path.
func (s *Store) RearmPull(ctx context.Context, repoID string, pullNumber int, at time.Time) error {
	at = storedTime(at)
	doing := fmt.Sprintf("rearm pull %d in repo %q", pullNumber, repoID)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", doing, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO autoland_rearms (repo_id, pull_number, rearmed_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT (repo_id, pull_number)
		 DO UPDATE SET rearmed_at = ?`),
		repoID, pullNumber, fmtTime(at), fmtTime(at)); err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%s: %w", doing, ErrNotFound)
		}
		return fmt.Errorf("%s: %w", doing, err)
	}

	if _, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM autoland_attempts WHERE repo_id = ? AND pull_number = ?`),
		repoID, pullNumber); err != nil {
		return fmt.Errorf("%s: %w", doing, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", doing, err)
	}
	return nil
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
