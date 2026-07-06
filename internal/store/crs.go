package store

// Built-in tracker: change-request accessors (design §3a, D11). A change
// request (CR) is the lab-internal pull request for repos whose
// tracker_binding is 'builtin': head branch onto base branch, three-valued
// state (open|merged|closed — merged is distinct from closed-unmerged so the
// reaper's done-signal semantics carry over, see tracker.PRPresent), and the
// issue numbers its body closes persisted as cr_closes rows at create time.
// Per-repo CR numbers come from the repos row's next_cr_number counter,
// allocated with an UPDATE … RETURNING inside the create transaction (design
// §2 per-repo sequences), exactly like issue numbers. State only ever moves
// forward from open (open→merged via MergeCR, open→closed via CloseCR); a
// transition attempted on a non-open CR fails with ErrCRNotOpen so the API
// can answer 409. Reopen is out of scope (roadmap).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// CR states (design §3a: change_requests.state CHECK IN (open, merged,
// closed)). The strings deliberately equal the tracker package's three-valued
// PR vocabulary (tracker.PullOpen/PullMerged/PullClosed) so the built-in
// tracker maps them 1:1.
const (
	CRStateOpen   = "open"
	CRStateMerged = "merged"
	CRStateClosed = "closed"
)

// CRStateAll is the wildcard state filter for CRsByRepo — it is not a stored
// value, only "no state predicate".
const CRStateAll = "all"

// ErrCRNotOpen is the sentinel for a state transition (merge or close)
// attempted on a CR that is not open. The wrapping error carries the CR's
// actual state; the API layer answers 409.
var ErrCRNotOpen = errors.New("change request is not open")

// CR is one row of change_requests plus its cr_closes issue numbers. Closes
// is always loaded (list and detail views alike — the operator list JSON
// carries it) and is sorted ascending, never nil. MergedAt and MergeCommit
// are set together by MergeCR and stay nil otherwise.
type CR struct {
	ID          string
	RepoID      string
	Number      int
	Title       string
	Body        string
	HeadBranch  string
	BaseBranch  string
	State       string // open|merged|closed
	MergedAt    *time.Time
	MergeCommit *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Closes      []int // built-in issue numbers this CR closes, sorted ascending
}

// crColumns is the one column list every CR SELECT uses, in scanCR order.
const crColumns = `id, repo_id, number, title, body, head_branch, base_branch,
	state, merged_at, merge_commit, created_at, updated_at`

// CreateCR allocates the next per-repo CR number and inserts an open change
// request together with its cr_closes rows, all in one transaction. The
// number comes from next_cr_number incremented and returned in the same
// transaction, so concurrent creates never collide (design §2). closesIssues
// is normalized (deduplicated, non-positive numbers dropped, sorted
// ascending) before insert — callers typically pass a tracker.ParseCloses
// result, which is already in that form. A missing repo maps to ErrNotFound.
func (s *Store) CreateCR(ctx context.Context, repoID, title, body, headBranch, baseBranch string, closesIssues []int, now time.Time) (CR, error) {
	now = storedTime(now)
	closes := normalizeCloses(closesIssues)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CR{}, fmt.Errorf("create CR: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var number int
	err = tx.QueryRowContext(ctx, s.rebind(
		`UPDATE repos SET next_cr_number = next_cr_number + 1
		 WHERE id = ? RETURNING next_cr_number`), repoID).Scan(&number)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CR{}, fmt.Errorf("create CR: repo %q: %w", repoID, ErrNotFound)
		}
		return CR{}, fmt.Errorf("create CR: allocate number: %w", err)
	}

	id := ids.NewID("cr")
	_, err = tx.ExecContext(ctx, s.rebind(
		`INSERT INTO change_requests (`+crColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, repoID, number, title, body, headBranch, baseBranch,
		CRStateOpen, nil, nil, fmtTime(now), fmtTime(now))
	if err != nil {
		return CR{}, fmt.Errorf("create CR: %w", err)
	}

	for _, n := range closes {
		_, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO cr_closes (cr_id, issue_number) VALUES (?, ?)`), id, n)
		if err != nil {
			return CR{}, fmt.Errorf("create CR: closes #%d: %w", n, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return CR{}, fmt.Errorf("create CR: %w", err)
	}
	return CR{
		ID: id, RepoID: repoID, Number: number, Title: title, Body: body,
		HeadBranch: headBranch, BaseBranch: baseBranch, State: CRStateOpen,
		CreatedAt: now, UpdatedAt: now, Closes: closes,
	}, nil
}

// CRsByRepo lists a repo's change requests newest-number first, filtered by
// state (CRStateOpen | CRStateMerged | CRStateClosed | CRStateAll). Every CR
// carries its closes list (the operator list view renders closes chips). An
// unknown state filter is an error.
func (s *Store) CRsByRepo(ctx context.Context, repoID, state string) ([]CR, error) {
	query := `SELECT ` + crColumns + ` FROM change_requests WHERE repo_id = ?`
	args := []any{repoID}
	switch state {
	case CRStateOpen, CRStateMerged, CRStateClosed:
		query += ` AND state = ?`
		args = append(args, state)
	case CRStateAll, "":
		// no state predicate
	default:
		return nil, fmt.Errorf("CRs by repo %q: invalid state filter %q", repoID, state)
	}
	query += ` ORDER BY number DESC`

	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("CRs by repo %q: %w", repoID, err)
	}
	crs := make([]CR, 0)
	for rows.Next() {
		cr, err := scanCR(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("CRs by repo %q: %w", repoID, err)
		}
		cr.Closes = []int{}
		crs = append(crs, cr)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("CRs by repo %q: %w", repoID, err)
	}
	// Close before the closes query: the sqlite store holds a single connection
	// (design §3a), so a second query must not run while these rows are open.
	_ = rows.Close()

	byID, err := s.closesByCR(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("CRs by repo %q: %w", repoID, err)
	}
	for i := range crs {
		if nums := byID[crs[i].ID]; nums != nil {
			crs[i].Closes = nums
		}
	}
	return crs, nil
}

// CRByRepoNumber returns one change request with its closes list.
// ErrNotFound when no such (repo, number) exists.
func (s *Store) CRByRepoNumber(ctx context.Context, repoID string, number int) (CR, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+crColumns+` FROM change_requests WHERE repo_id = ? AND number = ?`),
		repoID, number)
	cr, err := scanCR(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CR{}, fmt.Errorf("CR %s#%d: %w", repoID, number, ErrNotFound)
		}
		return CR{}, fmt.Errorf("CR %s#%d: %w", repoID, number, err)
	}
	cr.Closes = []int{}

	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT issue_number FROM cr_closes WHERE cr_id = ? ORDER BY issue_number`), cr.ID)
	if err != nil {
		return CR{}, fmt.Errorf("CR %s#%d: closes: %w", repoID, number, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return CR{}, fmt.Errorf("CR %s#%d: closes: %w", repoID, number, err)
		}
		cr.Closes = append(cr.Closes, n)
	}
	if err := rows.Err(); err != nil {
		return CR{}, fmt.Errorf("CR %s#%d: closes: %w", repoID, number, err)
	}
	return cr, nil
}

// CloseCR transitions an open CR to closed (closed-unmerged: merged_at and
// merge_commit stay NULL) and bumps updated_at. A CR in any other state fails
// with ErrCRNotOpen (wrapped with the actual state — the API's 409 body); a
// missing (repo, number) with ErrNotFound.
func (s *Store) CloseCR(ctx context.Context, repoID string, number int, now time.Time) (CR, error) {
	now = storedTime(now)
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE change_requests SET state = ?, updated_at = ?
		 WHERE repo_id = ? AND number = ? AND state = ?`),
		CRStateClosed, fmtTime(now), repoID, number, CRStateOpen)
	if err != nil {
		return CR{}, fmt.Errorf("close CR %s#%d: %w", repoID, number, err)
	}
	if err := s.crTransitionApplied(ctx, res, repoID, number, "close CR"); err != nil {
		return CR{}, err
	}
	return s.CRByRepoNumber(ctx, repoID, number)
}

// MergeCR transitions an open CR to merged, stamping merged_at and recording
// the merge commit (the FF'd head SHA or the new merge commit — gitx.CRMerge's
// result), and bumps updated_at. A CR in any other state fails with
// ErrCRNotOpen (wrapped with the actual state); a missing (repo, number) with
// ErrNotFound.
func (s *Store) MergeCR(ctx context.Context, repoID string, number int, mergeCommit string, now time.Time) (CR, error) {
	now = storedTime(now)
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE change_requests SET state = ?, merged_at = ?, merge_commit = ?, updated_at = ?
		 WHERE repo_id = ? AND number = ? AND state = ?`),
		CRStateMerged, fmtTime(now), mergeCommit, fmtTime(now), repoID, number, CRStateOpen)
	if err != nil {
		return CR{}, fmt.Errorf("merge CR %s#%d: %w", repoID, number, err)
	}
	if err := s.crTransitionApplied(ctx, res, repoID, number, "merge CR"); err != nil {
		return CR{}, err
	}
	return s.CRByRepoNumber(ctx, repoID, number)
}

// crTransitionApplied resolves a conditional state-transition UPDATE that
// matched zero rows into the right typed error: the CR does not exist
// (ErrNotFound) or it exists but is not open (ErrCRNotOpen, wrapped with the
// actual state). A matched row is success.
func (s *Store) crTransitionApplied(ctx context.Context, res sql.Result, repoID string, number int, doing string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %s#%d: %w", doing, repoID, number, err)
	}
	if n > 0 {
		return nil
	}
	cur, err := s.CRByRepoNumber(ctx, repoID, number)
	switch {
	case err == nil:
		return fmt.Errorf("%s %s#%d: state %q: %w", doing, repoID, number, cur.State, ErrCRNotOpen)
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%s %s#%d: %w", doing, repoID, number, ErrNotFound)
	default:
		// A transient recheck failure (busy DB, canceled ctx) surfaces as
		// itself — mapping it to ErrNotFound would answer a live CR with 404.
		return fmt.Errorf("%s %s#%d: recheck after zero-row transition: %w", doing, repoID, number, err)
	}
}

// OpenCRByHead reports the repo's open change request whose head is branch,
// or ErrNotFound. One head can carry at most one OPEN CR (builtin.CreatePull
// refuses duplicates — forge parity: Forgejo 409s an already-open head), so
// the first match wins deterministically by lowest number.
func (s *Store) OpenCRByHead(ctx context.Context, repoID, headBranch string) (CR, error) {
	var number int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT number FROM change_requests
		 WHERE repo_id = ? AND head_branch = ? AND state = ?
		 ORDER BY number ASC LIMIT 1`), repoID, headBranch, CRStateOpen).Scan(&number)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CR{}, fmt.Errorf("open CR by head %s@%q: %w", repoID, headBranch, ErrNotFound)
		}
		return CR{}, fmt.Errorf("open CR by head %s@%q: %w", repoID, headBranch, err)
	}
	return s.CRByRepoNumber(ctx, repoID, number)
}

// closesByCR returns, for every CR in a repo, its sorted closes issue numbers
// keyed by CR id. One query — the batch form the list view uses so it never
// issues a per-CR query while the change_requests rows are open.
func (s *Store) closesByCR(ctx context.Context, repoID string) (map[string][]int, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT cc.cr_id, cc.issue_number
		 FROM cr_closes cc
		 JOIN change_requests cr ON cr.id = cc.cr_id
		 WHERE cr.repo_id = ?
		 ORDER BY cc.cr_id, cc.issue_number`), repoID)
	if err != nil {
		return nil, fmt.Errorf("closes by CR for repo %q: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]int)
	for rows.Next() {
		var crID string
		var n int
		if err := rows.Scan(&crID, &n); err != nil {
			return nil, fmt.Errorf("closes by CR for repo %q: %w", repoID, err)
		}
		out[crID] = append(out[crID], n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("closes by CR for repo %q: %w", repoID, err)
	}
	return out, nil
}

// normalizeCloses deduplicates, drops non-positive numbers, and sorts
// ascending — the stored (and returned) form of a CR's closes list.
func normalizeCloses(nums []int) []int {
	seen := make(map[int]bool, len(nums))
	out := make([]int, 0, len(nums))
	for _, n := range nums {
		if n < 1 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// scanCR reads one row in crColumns order.
func scanCR(scan func(dest ...any) error) (CR, error) {
	var (
		cr               CR
		merged           sql.NullString
		mergeCommit      sql.NullString
		created, updated string
	)
	if err := scan(&cr.ID, &cr.RepoID, &cr.Number, &cr.Title, &cr.Body,
		&cr.HeadBranch, &cr.BaseBranch, &cr.State,
		&merged, &mergeCommit, &created, &updated); err != nil {
		return CR{}, err
	}
	var err error
	if cr.MergedAt, err = parseNullTime(merged); err != nil {
		return CR{}, err
	}
	cr.MergeCommit = nullStr(mergeCommit)
	if cr.CreatedAt, err = parseTime(created); err != nil {
		return CR{}, err
	}
	if cr.UpdatedAt, err = parseTime(updated); err != nil {
		return CR{}, err
	}
	return cr, nil
}
