package store

// Run accessors (design §3a; the runs table is lab's instance history and the
// re-adoption oracle). A run row is durable config/history — the DB never
// witnesses liveness (D6): tmux answers "alive", git refs answer "claimed",
// the runs row records what lab spawned and how it ended. The instance service
// creates a row (outcome 'active') at spawn and moves it to a terminal outcome
// at Stop/reap; startup reconciliation re-derives liveness by matching
// outcome='active' rows against live tmux sessions.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Run kinds (design §3a). M3 creates only 'manual'; the two afk kinds are
// written by the M5 engine but recognised here so re-adoption and the Stop
// AFK-seam can classify any row.
const (
	RunKindManual    = "manual"
	RunKindAFKManual = "afk_manual"
	RunKindAFKAuto   = "afk_auto"
)

// Run outcomes (design §3a). 'active' is the only non-terminal value; the
// terminal outcomes are set once, at the Stop/reap/reconcile chokepoint.
const (
	RunOutcomeActive  = "active"
	RunOutcomeSuccess = "success"
	RunOutcomeDeath   = "death"
	RunOutcomeTimeout = "timeout"
	RunOutcomeStopped = "stopped"
)

// Run is one row of runs — every §3a column. Nil pointers are NULL.
type Run struct {
	ID             string
	RepoID         string
	Kind           string // manual|afk_manual|afk_auto
	Provider       string
	IssueNumber    *int
	Branch         string
	WorktreePath   string
	SessionName    string
	Model          string
	Effort         string
	DeepLinkURL    *string
	StartedAt      time.Time
	BudgetDeadline *time.Time // persisted budget clock (D12b)
	EndedAt        *time.Time
	Outcome        string
	FailureReason  *string
}

// runColumns is the one column list every run SELECT/INSERT uses, in the order
// scanRun reads.
const runColumns = `id, repo_id, kind, provider, issue_number, branch,
	worktree_path, session_name, model, effort, deep_link_url, started_at,
	budget_deadline, ended_at, outcome, failure_reason`

// CreateRun inserts r exactly as given (the caller has derived every field:
// session name, branch, worktree path, resolved model/effort). Timestamps are
// normalized to stored precision; the returned Run is what the database holds.
func (s *Store) CreateRun(ctx context.Context, r Run) (Run, error) {
	r.StartedAt = storedTime(r.StartedAt)
	if r.BudgetDeadline != nil {
		t := storedTime(*r.BudgetDeadline)
		r.BudgetDeadline = &t
	}
	if r.EndedAt != nil {
		t := storedTime(*r.EndedAt)
		r.EndedAt = &t
	}
	if r.Outcome == "" {
		r.Outcome = RunOutcomeActive
	}
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO runs (`+runColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.ID, r.RepoID, r.Kind, r.Provider, r.IssueNumber, r.Branch,
		r.WorktreePath, r.SessionName, r.Model, r.Effort, r.DeepLinkURL,
		fmtTime(r.StartedAt), fmtNullTime(r.BudgetDeadline), fmtNullTime(r.EndedAt),
		r.Outcome, r.FailureReason)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Run{}, fmt.Errorf("create run %q: %w", r.SessionName, ErrNotFound)
		}
		return Run{}, fmt.Errorf("create run %q: %w", r.SessionName, err)
	}
	return r, nil
}

// RunBySession returns the ACTIVE run for a session name, or ErrNotFound. A
// session name is reused only after its previous run reaches a terminal
// outcome, so at most one active run ever carries a given name — the row Stop
// and the AFK-seam classification act on.
func (s *Store) RunBySession(ctx context.Context, session string) (Run, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs
		 WHERE session_name = ? AND outcome = ?`), session, RunOutcomeActive)
	r, err := scanRun(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, fmt.Errorf("run by session %q: %w", session, ErrNotFound)
		}
		return Run{}, fmt.Errorf("run by session %q: %w", session, err)
	}
	return r, nil
}

// ActiveRuns lists every outcome='active' run, newest first. This is the
// re-adoption input (matched against live tmux at startup) and the GET
// /instances source (annotated with live/connecting from tmux + the provider).
func (s *Store) ActiveRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+runColumns+` FROM runs WHERE outcome = ? ORDER BY started_at DESC`),
		RunOutcomeActive)
	if err != nil {
		return nil, fmt.Errorf("active runs: %w", err)
	}
	return scanRuns(rows, "active runs")
}

// RunsByRepo lists a repo's runs newest first, capped at limit (<= 0 → no
// cap). The GET /runs history endpoint.
func (s *Store) RunsByRepo(ctx context.Context, repoID string, limit int) ([]Run, error) {
	query := `SELECT ` + runColumns + ` FROM runs WHERE repo_id = ? ORDER BY started_at DESC`
	args := []any{repoID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("runs by repo %q: %w", repoID, err)
	}
	return scanRuns(rows, "runs by repo "+repoID)
}

// EndRun moves an active run to a terminal outcome, stamping ended_at and an
// optional failure reason (empty → NULL). It touches only rows still
// outcome='active', so a double-Stop or a reap racing a Stop cannot overwrite
// the first terminal outcome. ErrNotFound when no active row matched.
func (s *Store) EndRun(ctx context.Context, id, outcome string, endedAt time.Time, failureReason string) error {
	var reason any
	if failureReason != "" {
		reason = failureReason
	}
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE runs SET outcome = ?, ended_at = ?, failure_reason = ?
		 WHERE id = ? AND outcome = ?`),
		outcome, fmtTime(endedAt), reason, id, RunOutcomeActive)
	if err != nil {
		return fmt.Errorf("end run %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("end run %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("end run %q: %w", id, ErrNotFound)
	}
	return nil
}

// UpdateRunDeepLink sets a run's captured deep link. Write-only-on-hit is the
// caller's rule (never persist the generic fallback over a real link); the
// store just writes what it is given.
func (s *Store) UpdateRunDeepLink(ctx context.Context, id, url string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE runs SET deep_link_url = ? WHERE id = ?`), url, id)
	if err != nil {
		return fmt.Errorf("update run %q deep link: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update run %q deep link: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("update run %q deep link: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteRun removes a run row (its run_tokens cascade). This is the Start
// rollback path (§3a: a failure after worktree creation deletes the runs
// row/token), NOT a terminal outcome — a rolled-back start leaves no history.
// Deleting a missing row is not an error (the rollback is best-effort).
func (s *Store) DeleteRun(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM runs WHERE id = ?`), id); err != nil {
		return fmt.Errorf("delete run %q: %w", id, err)
	}
	return nil
}

func scanRuns(rows *sql.Rows, doing string) ([]Run, error) {
	defer func() { _ = rows.Close() }()
	runs := make([]Run, 0)
	for rows.Next() {
		r, err := scanRun(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", doing, err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", doing, err)
	}
	return runs, nil
}

// scanRun reads one row in runColumns order.
func scanRun(scan func(dest ...any) error) (Run, error) {
	var (
		r        Run
		issueN   sql.NullInt64
		deepLink sql.NullString
		started  string
		budget   sql.NullString
		ended    sql.NullString
		failure  sql.NullString
	)
	if err := scan(&r.ID, &r.RepoID, &r.Kind, &r.Provider, &issueN, &r.Branch,
		&r.WorktreePath, &r.SessionName, &r.Model, &r.Effort, &deepLink, &started,
		&budget, &ended, &r.Outcome, &failure); err != nil {
		return Run{}, err
	}
	r.IssueNumber = nullInt(issueN)
	r.DeepLinkURL = nullStr(deepLink)
	r.FailureReason = nullStr(failure)

	var err error
	if r.StartedAt, err = parseTime(started); err != nil {
		return Run{}, err
	}
	if r.BudgetDeadline, err = parseNullTime(budget); err != nil {
		return Run{}, err
	}
	if r.EndedAt, err = parseNullTime(ended); err != nil {
		return Run{}, err
	}
	return r, nil
}
