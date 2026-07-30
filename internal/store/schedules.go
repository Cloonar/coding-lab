package store

// Schedule accessors (issue #247 / ADR-0062). A Schedule is the per-repo
// cadence object: a name unique within its repo (UNIQUE(repo_id, name) →
// ErrNameTaken), a cron expression, a freeform prompt, an ordered selection of
// built-in flow keys, an enabled flag, optional budget/model/effort/provider
// overrides, and its own consecutive-failure counter with a paused state.
// Rows cascade-delete with their repo.
//
// The store persists what it is given: cron grammar (internal/cronx), flow-key
// membership, and provider validity are all checked by the caller before a
// write — the table carries no CHECK for either, the repos.runner precedent.
//
// Two clocks meet here on purpose. created_at/updated_at are the operator's
// edit history: CreateSchedule takes them from the caller (the reposecrets
// style) and UpdateSchedule stamps updated_at from s.Now. last_fired_at and
// the failure/pause columns are engine bookkeeping, written by the dedicated
// verbs below, and none of them touch updated_at — a Schedule that fires every
// morning must not read as edited every morning.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Schedule is one row of schedules. Nil pointers are NULL.
type Schedule struct {
	ID      string
	RepoID  string
	Name    string // unique within the repo
	Cadence string // five-field cron expression, validated app-side
	Prompt  string // freeform seed text; the flow blocks are appended at spawn
	// Flows are the selected flow-catalog keys, persisted as a JSON array in a
	// TEXT column. Empty is legal and means a pure-prompt Schedule; the order
	// stored here is the operator's, but composition follows the catalog's
	// order, so two Schedules picking the same flows brief identically.
	Flows   []string
	Enabled bool
	// BudgetMinutes is the per-Schedule budget override; nil = the 30-minute
	// scheduled-run default. Budget expiry is a scheduled run's COMPLETION, not
	// a timeout-failure, so this is the only termination knob there is.
	BudgetMinutes *int
	// Model, Effort and Provider are the topmost rung of the AFK-default
	// layering (ADR-0021/0030). Nil = inherit the rung below; a set-but-stale
	// value degrades the firing to the inherited chain rather than breaking
	// every firing forever (skip-layer, not strict).
	Model    *string
	Effort   *string
	Provider *string
	// ConsecutiveFailures and Paused are the per-Schedule three-strikes state.
	// It is deliberately independent of the repo's AFK counter: a broken
	// Schedule must never pause unrelated AFK work, and a healthy Schedule's
	// completions must never clear strikes that AFK runs earned.
	ConsecutiveFailures int
	Paused              bool
	LastFiredAt         *time.Time // nil until the first firing
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// scheduleColumns is the one column list every schedule SELECT/INSERT uses, in
// the order scanSchedule reads.
const scheduleColumns = `id, repo_id, name, cadence, prompt, flows, enabled,
	budget_minutes, model, effort, provider, consecutive_failures, paused,
	last_fired_at, created_at, updated_at`

// CreateSchedule inserts sc exactly as given: the caller supplies the sched_
// id and both timestamps, which are normalized to stored precision, and the
// returned Schedule is what the database now holds. ErrNameTaken on a
// (repo_id, name) collision; ErrNotFound when repo_id names no repo.
func (s *Store) CreateSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	sc.CreatedAt = storedTime(sc.CreatedAt)
	sc.UpdatedAt = storedTime(sc.UpdatedAt)
	if sc.LastFiredAt != nil {
		t := storedTime(*sc.LastFiredAt)
		sc.LastFiredAt = &t
	}
	sc.Flows = normalizeFlows(sc.Flows)

	flows, err := marshalFlows(sc.Flows)
	if err != nil {
		return Schedule{}, fmt.Errorf("create schedule %q: %w", sc.Name, err)
	}
	_, err = s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO schedules (`+scheduleColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		sc.ID, sc.RepoID, sc.Name, sc.Cadence, sc.Prompt, flows, sc.Enabled,
		sc.BudgetMinutes, sc.Model, sc.Effort, sc.Provider,
		sc.ConsecutiveFailures, sc.Paused, fmtNullTime(sc.LastFiredAt),
		fmtTime(sc.CreatedAt), fmtTime(sc.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return Schedule{}, fmt.Errorf("create schedule %q: %w", sc.Name, ErrNameTaken)
		}
		if isForeignKeyViolation(err) {
			return Schedule{}, fmt.Errorf("create schedule %q: repo %q: %w", sc.Name, sc.RepoID, ErrNotFound)
		}
		return Schedule{}, fmt.Errorf("create schedule %q: %w", sc.Name, err)
	}
	return sc, nil
}

// SchedulesByRepo lists a repo's Schedules ordered by name — the repo
// settings section's listing. Empty slice, not nil, for a repo with none.
func (s *Store) SchedulesByRepo(ctx context.Context, repoID string) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+scheduleColumns+` FROM schedules WHERE repo_id = ? ORDER BY name`), repoID)
	if err != nil {
		return nil, fmt.Errorf("schedules for repo %q: %w", repoID, err)
	}
	return scanSchedules(rows, "schedules for repo "+repoID)
}

// ScheduleByID returns one Schedule, or ErrNotFound. The httpapi cross-repo
// guard compares the returned RepoID against the request's repo.
func (s *Store) ScheduleByID(ctx context.Context, id string) (Schedule, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`), id)
	sc, err := scanSchedule(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Schedule{}, fmt.Errorf("schedule %q: %w", id, ErrNotFound)
		}
		return Schedule{}, fmt.Errorf("schedule %q: %w", id, err)
	}
	return sc, nil
}

// EnabledSchedules lists every Schedule the producer may consider this pass —
// enabled and not paused, across all repos, ordered by (repo_id, name) so a
// pass is deterministic. This is the producer's ONE listing per pass: the
// due-ness decision is made in memory against each row's cadence, never with a
// per-Schedule query. Both flags are filtered here rather than by the caller
// because a paused Schedule is exactly a Schedule the engine must not see.
func (s *Store) EnabledSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+scheduleColumns+` FROM schedules
		 WHERE enabled = ? AND paused = ?
		 ORDER BY repo_id, name`), true, false)
	if err != nil {
		return nil, fmt.Errorf("enabled schedules: %w", err)
	}
	return scanSchedules(rows, "enabled schedules")
}

// ScheduleUpdate names every column an operator edit may touch. Absent on
// purpose: consecutive_failures, paused and last_fired_at, which are engine
// bookkeeping with their own verbs (IncrementScheduleFailures,
// ResetScheduleFailures, SetSchedulePaused, PauseScheduleIfStruck,
// ReenableSchedule, MarkScheduleFired) — an edit form must not be able to
// forge a firing or silently clear a three-strikes pause.
type ScheduleUpdate struct {
	Name          Opt[string]
	Cadence       Opt[string]
	Prompt        Opt[string]
	Flows         Opt[[]string] // nil or empty both store "[]" (a pure-prompt Schedule)
	Enabled       Opt[bool]
	BudgetMinutes Opt[*int]    // nil clears (NULL = the 30-minute default)
	Model         Opt[*string] // nil clears (NULL = inherit the AFK-default layering)
	Effort        Opt[*string] // nil clears (NULL = inherit)
	Provider      Opt[*string] // nil clears (NULL = inherit)
}

// UpdateSchedule applies the set fields of u to one Schedule and returns the
// updated row, stamping updated_at from s.Now. An empty update is a no-op that
// still verifies existence (and leaves updated_at where it was — nothing
// changed, so nothing was edited). ErrNotFound for a missing id; ErrNameTaken
// on a rename collision within the repo.
func (s *Store) UpdateSchedule(ctx context.Context, id string, u ScheduleUpdate) (Schedule, error) {
	var (
		sets []string
		args []any
	)
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if u.Name.Set {
		add("name", u.Name.Value)
	}
	if u.Cadence.Set {
		add("cadence", u.Cadence.Value)
	}
	if u.Prompt.Set {
		add("prompt", u.Prompt.Value)
	}
	if u.Flows.Set {
		v, err := marshalFlows(u.Flows.Value)
		if err != nil {
			return Schedule{}, fmt.Errorf("update schedule %q: %w", id, err)
		}
		add("flows", v)
	}
	if u.Enabled.Set {
		add("enabled", u.Enabled.Value)
	}
	if u.BudgetMinutes.Set {
		add("budget_minutes", u.BudgetMinutes.Value)
	}
	if u.Model.Set {
		add("model", u.Model.Value)
	}
	if u.Effort.Set {
		add("effort", u.Effort.Value)
	}
	if u.Provider.Set {
		add("provider", u.Provider.Value)
	}
	if len(sets) == 0 {
		return s.ScheduleByID(ctx, id)
	}
	add("updated_at", fmtTime(storedTime(s.Now())))
	args = append(args, id)

	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE schedules SET `+strings.Join(sets, ", ")+` WHERE id = ?`), args...)
	if err != nil {
		if isUniqueViolation(err) {
			return Schedule{}, fmt.Errorf("update schedule %q: %w", id, ErrNameTaken)
		}
		return Schedule{}, fmt.Errorf("update schedule %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Schedule{}, fmt.Errorf("update schedule %q: %w", id, err)
	}
	if n == 0 {
		return Schedule{}, fmt.Errorf("update schedule %q: %w", id, ErrNotFound)
	}
	return s.ScheduleByID(ctx, id)
}

// DeleteSchedule removes a Schedule. Its live run, if any, survives with
// runs.schedule_id set to NULL (ON DELETE SET NULL): deleting the cadence must
// never kill work already in flight. ErrNotFound if id does not exist.
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM schedules WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete schedule %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete schedule %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete schedule %q: %w", id, ErrNotFound)
	}
	return nil
}

// MarkScheduleFired stamps last_fired_at at the launch chokepoint — and only
// that column. updated_at stays put: a firing is the engine acting on the
// Schedule, not an operator editing it, and the startup missed-slot log reads
// last_fired_at alone. ErrNotFound when no row matched.
func (s *Store) MarkScheduleFired(ctx context.Context, id string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE schedules SET last_fired_at = ? WHERE id = ?`),
		fmtTime(storedTime(now)), id)
	if err != nil {
		return fmt.Errorf("mark schedule %q fired: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark schedule %q fired: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("mark schedule %q fired: %w", id, ErrNotFound)
	}
	return nil
}

// IncrementScheduleFailures adds one to a Schedule's consecutive-failure
// counter and returns the new value — the reap chokepoint's accounting for a
// session death before budget expiry. The caller compares the result against
// the three-strikes threshold and pauses; the store just counts. This never
// touches the repo's own counter (ADR-0062: the two measure different things).
// ErrNotFound for a missing Schedule.
func (s *Store) IncrementScheduleFailures(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`UPDATE schedules SET consecutive_failures = consecutive_failures + 1
		 WHERE id = ? RETURNING consecutive_failures`), id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("increment failures for schedule %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("increment failures for schedule %q: %w", id, err)
	}
	return n, nil
}

// ResetScheduleFailures zeroes a Schedule's consecutive-failure counter — a
// budget-expiry completion re-arming the three-strikes bound. It reports
// whether the counter actually changed, so callers publish a change event only
// on a real transition. A missing Schedule is indistinguishable from an
// already-zero counter here; callers that must 404 verify existence first.
func (s *Store) ResetScheduleFailures(ctx context.Context, id string) (changed bool, err error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE schedules SET consecutive_failures = 0
		 WHERE id = ? AND consecutive_failures <> 0`), id)
	if err != nil {
		return false, fmt.Errorf("reset failures for schedule %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reset failures for schedule %q: %w", id, err)
	}
	return n > 0, nil
}

// SetSchedulePaused writes the paused flag and reports whether it changed —
// the edge the pause push notification rides, so "paused after 3 failures"
// sends exactly once no matter how many times the reap path re-asserts it.
// The counter is left alone: pausing is the consequence of the strikes, not
// their erasure. See ReenableSchedule for the human un-pause.
func (s *Store) SetSchedulePaused(ctx context.Context, id string, paused bool) (changed bool, err error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE schedules SET paused = ? WHERE id = ? AND paused <> ?`),
		paused, id, paused)
	if err != nil {
		return false, fmt.Errorf("set schedule %q paused: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set schedule %q paused: %w", id, err)
	}
	return n > 0, nil
}

// PauseScheduleIfStruck pauses a Schedule only while its strikes still clear
// threshold, in ONE statement — the reap chokepoint's pause verb. Increment,
// threshold comparison, and pause are otherwise separate statements, and a
// human ReenableSchedule landing between them would be silently reverted
// into paused-with-zero-strikes; here the WHERE re-reads the counter
// atomically with the write, so the re-enable wins instead. changed reports
// the paused edge exactly like SetSchedulePaused (the pause push's trigger);
// an already-paused, re-enabled, or missing Schedule is a plain no-op. The
// counter is left alone: pausing is the consequence of the strikes, not
// their erasure.
func (s *Store) PauseScheduleIfStruck(ctx context.Context, id string, threshold int) (changed bool, err error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE schedules SET paused = ?
		 WHERE id = ? AND paused = ? AND consecutive_failures >= ?`),
		true, id, false, threshold)
	if err != nil {
		return false, fmt.Errorf("pause struck schedule %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("pause struck schedule %q: %w", id, err)
	}
	return n > 0, nil
}

// ReenableSchedule clears the pause AND zeroes the counter in one statement,
// returning the fresh row — the human re-enable action from the UI (never
// automatic, ADR-0007's rule for the repo pause, applied to the Schedule's own
// strikes). One statement because the two are one decision: re-enabling
// without resetting would re-pause on the very next failure, and the operator
// asked for a fresh three strikes. ErrNotFound for a missing Schedule.
func (s *Store) ReenableSchedule(ctx context.Context, id string) (Schedule, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE schedules SET paused = ?, consecutive_failures = 0 WHERE id = ?`),
		false, id)
	if err != nil {
		return Schedule{}, fmt.Errorf("reenable schedule %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Schedule{}, fmt.Errorf("reenable schedule %q: %w", id, err)
	}
	if n == 0 {
		return Schedule{}, fmt.Errorf("reenable schedule %q: %w", id, ErrNotFound)
	}
	return s.ScheduleByID(ctx, id)
}

func scanSchedules(rows *sql.Rows, doing string) ([]Schedule, error) {
	defer func() { _ = rows.Close() }()
	schedules := make([]Schedule, 0)
	for rows.Next() {
		sc, err := scanSchedule(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", doing, err)
		}
		schedules = append(schedules, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", doing, err)
	}
	return schedules, nil
}

// scanSchedule reads one row in scheduleColumns order.
func scanSchedule(scan func(dest ...any) error) (Schedule, error) {
	var (
		sc                      Schedule
		flows                   sql.NullString
		budget                  sql.NullInt64
		model, effort, provider sql.NullString
		lastFired               sql.NullString
		created, updated        string
	)
	if err := scan(&sc.ID, &sc.RepoID, &sc.Name, &sc.Cadence, &sc.Prompt, &flows,
		&sc.Enabled, &budget, &model, &effort, &provider,
		&sc.ConsecutiveFailures, &sc.Paused, &lastFired,
		&created, &updated); err != nil {
		return Schedule{}, err
	}
	sc.Flows = unmarshalFlows(flows)
	sc.BudgetMinutes = nullInt(budget)
	sc.Model = nullStr(model)
	sc.Effort = nullStr(effort)
	sc.Provider = nullStr(provider)

	var err error
	if sc.LastFiredAt, err = parseNullTime(lastFired); err != nil {
		return Schedule{}, err
	}
	if sc.CreatedAt, err = parseTime(created); err != nil {
		return Schedule{}, err
	}
	if sc.UpdatedAt, err = parseTime(updated); err != nil {
		return Schedule{}, err
	}
	return sc, nil
}

// normalizeFlows is what a flow selection becomes after a database round trip:
// never nil, so a created Schedule compares equal to the row read back.
func normalizeFlows(flows []string) []string {
	if len(flows) == 0 {
		return []string{}
	}
	return flows
}

// marshalFlows renders a flow selection for the flows TEXT column. Nil and
// empty both become "[]", the column's default and the pure-prompt Schedule's
// value: unlike repos.afk_options there is no inherit layer here, so "no
// flows" needs exactly one spelling and NULL is not it.
func marshalFlows(flows []string) (string, error) {
	if len(flows) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(flows)
	if err != nil {
		return "", fmt.Errorf("marshal flows: %w", err)
	}
	return string(b), nil
}

// unmarshalFlows parses the flows column defensively: NULL, blank, and
// anything that is not a JSON array of strings all read back as no flows
// rather than failing the row. This is the deliberate opposite of
// unmarshalOptions' loud parse. A Schedule whose flow list was corrupted (a
// hand-edited row, a rolled-back catalog) must still list, edit, pause and
// delete from the UI; briefing a firing with no flow block degrades one run,
// while an unreadable row would strand the operator with no way to fix it.
// Unknown keys survive the round trip untouched — the catalog, not the store,
// decides what a key means, and a key this binary does not know may be one a
// newer one does.
func unmarshalFlows(ns sql.NullString) []string {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return []string{}
	}
	var flows []string
	if err := json.Unmarshal([]byte(ns.String), &flows); err != nil {
		return []string{}
	}
	return normalizeFlows(flows)
}
