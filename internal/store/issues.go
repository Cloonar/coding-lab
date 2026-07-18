package store

// Built-in tracker: issue + comment accessors (design §3a). These back the
// builtin.Tracker and the operator API's issue surface for repos whose
// tracker_binding is 'builtin'. Per-repo issue numbers come from the repos
// row's next_issue_number counter, allocated with an UPDATE … RETURNING inside
// the create transaction (design §2 per-repo sequences). Issue state carries a
// closed_at that is stamped on the open→closed transition and cleared on
// reopen; every mutation — including comment and label mutations — bumps
// updated_at (forge parity: Forgejo advances updated_at on comments/labels).
// Comments are authored by an operator or a run (author_kind); a run comment
// carries the originating run_id (FK ON DELETE SET NULL), other kinds none.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// Issue states (design §3a: issues.state CHECK IN (open, closed)).
const (
	IssueStateOpen   = "open"
	IssueStateClosed = "closed"
)

// IssueStateAll is the wildcard state filter for IssuesByRepo — it is not a
// stored value, only "no state predicate".
const IssueStateAll = "all"

// Author kinds (design §3a: issue_comments.author_kind; since 0002 also
// issues.author_kind — issues and comments share the one author vocabulary,
// so the constants keep their original names).
const (
	CommentAuthorOperator = "operator"
	CommentAuthorRun      = "run"
)

// Issue is one row of issues plus its derived collections. Labels holds the
// issue's label names (always loaded by the accessors). Comments is loaded only
// by IssueByRepoNumber (the detail view); list accessors leave it nil and
// report the count via CommentCount instead.
type Issue struct {
	ID           string
	RepoID       string
	Number       int
	Title        string
	Body         string
	State        string  // open|closed
	AuthorKind   string  // operator|run
	RunID        *string // set only for run-authored issues; NULL otherwise
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ClosedAt     *time.Time
	Labels       []string       // label names, sorted
	Comments     []IssueComment // detail view only; nil in list views
	CommentCount int
}

// IssueComment is one row of issue_comments.
type IssueComment struct {
	ID         string
	IssueID    string
	AuthorKind string  // operator|run
	RunID      *string // set only for run-authored comments; NULL otherwise
	Body       string
	CreatedAt  time.Time
}

// issueColumns is the one column list every issue SELECT uses, in scanIssue
// order.
const issueColumns = `id, repo_id, number, title, body, state, author_kind, run_id,
	created_at, updated_at, closed_at`

// CreateIssue allocates the next per-repo issue number and inserts an open
// operator-authored issue. The number comes from next_issue_number incremented
// and returned in the same transaction, so concurrent creates never collide
// (design §2). The returned Issue is what the database holds (state open, no
// labels, no comments). A missing repo maps to ErrNotFound.
func (s *Store) CreateIssue(ctx context.Context, repoID, title, body string, now time.Time) (Issue, error) {
	return s.CreateIssueWithLabels(ctx, repoID, title, body, nil, CommentAuthorOperator, nil, now)
}

// CreateIssueWithLabels is CreateIssue plus the initial label attach in the
// SAME transaction (the operator create-with-labels path and the builtin
// tracker's agent create): if any labelID is missing (e.g. a concurrent label
// delete) the whole create rolls back with ErrNotFound, so a partial failure
// never commits a label-less issue whose retry would allocate a duplicate
// number. The author identity follows the comment rule (0002 shares the
// vocabulary): a run-authored issue requires runID, any other kind must pass
// nil. The returned Issue carries the attached label names, sorted.
func (s *Store) CreateIssueWithLabels(ctx context.Context, repoID, title, body string, labelIDs []string, authorKind string, runID *string, now time.Time) (Issue, error) {
	if authorKind != CommentAuthorOperator && authorKind != CommentAuthorRun {
		return Issue{}, fmt.Errorf("create issue: invalid author kind %q", authorKind)
	}
	if authorKind == CommentAuthorRun && runID == nil {
		return Issue{}, fmt.Errorf("create issue: author kind %q requires a run_id", authorKind)
	}
	if authorKind != CommentAuthorRun && runID != nil {
		return Issue{}, fmt.Errorf("create issue: run_id set for author kind %q", authorKind)
	}
	now = storedTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Issue{}, fmt.Errorf("create issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var number int
	err = tx.QueryRowContext(ctx, s.rebind(
		`UPDATE repos SET next_issue_number = next_issue_number + 1
		 WHERE id = ? RETURNING next_issue_number`), repoID).Scan(&number)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Issue{}, fmt.Errorf("create issue: repo %q: %w", repoID, ErrNotFound)
		}
		return Issue{}, fmt.Errorf("create issue: allocate number: %w", err)
	}

	id := ids.NewID("iss")
	_, err = tx.ExecContext(ctx, s.rebind(
		`INSERT INTO issues (`+issueColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, repoID, number, title, body, IssueStateOpen, authorKind, runID,
		fmtTime(now), fmtTime(now), nil)
	if err != nil {
		if isForeignKeyViolation(err) {
			// The only nullable FK in the insert is run_id: a runID pointing at
			// no run maps to ErrNotFound, like a run comment's.
			return Issue{}, fmt.Errorf("create issue: run %q: %w", *runID, ErrNotFound)
		}
		return Issue{}, fmt.Errorf("create issue: %w", err)
	}

	for _, labelID := range labelIDs {
		_, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO issue_labels (issue_id, label_id) VALUES (?, ?)
			 ON CONFLICT (issue_id, label_id) DO NOTHING`), id, labelID)
		if err != nil {
			if isForeignKeyViolation(err) {
				return Issue{}, fmt.Errorf("create issue: attach label %q: %w", labelID, ErrNotFound)
			}
			return Issue{}, fmt.Errorf("create issue: attach label %q: %w", labelID, err)
		}
	}
	labels := []string{}
	if len(labelIDs) > 0 {
		rows, err := tx.QueryContext(ctx, s.rebind(
			`SELECT l.name FROM issue_labels il
			 JOIN labels l ON l.id = il.label_id
			 WHERE il.issue_id = ? ORDER BY l.name`), id)
		if err != nil {
			return Issue{}, fmt.Errorf("create issue: load label names: %w", err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return Issue{}, fmt.Errorf("create issue: load label names: %w", err)
			}
			labels = append(labels, name)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return Issue{}, fmt.Errorf("create issue: load label names: %w", err)
		}
		_ = rows.Close()
	}

	if err := tx.Commit(); err != nil {
		return Issue{}, fmt.Errorf("create issue: %w", err)
	}
	return Issue{
		ID: id, RepoID: repoID, Number: number, Title: title, Body: body,
		State: IssueStateOpen, AuthorKind: authorKind, RunID: runID,
		CreatedAt: now, UpdatedAt: now,
		Labels: labels,
	}, nil
}

// IssuesByRepo lists a repo's issues newest-number first, filtered by state
// (IssueStateOpen | IssueStateClosed | IssueStateAll). Each issue carries its
// label names and comment count; comments themselves are not loaded (list
// view). An unknown state filter is an error.
func (s *Store) IssuesByRepo(ctx context.Context, repoID, state string) ([]Issue, error) {
	query := `SELECT ` + issueColumns + `,
		(SELECT COUNT(*) FROM issue_comments c WHERE c.issue_id = i.id) AS comment_count
		FROM issues i WHERE i.repo_id = ?`
	args := []any{repoID}
	switch state {
	case IssueStateOpen, IssueStateClosed:
		query += ` AND i.state = ?`
		args = append(args, state)
	case IssueStateAll, "":
		// no state predicate
	default:
		return nil, fmt.Errorf("issues by repo %q: invalid state filter %q", repoID, state)
	}
	query += ` ORDER BY i.number DESC`

	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("issues by repo %q: %w", repoID, err)
	}
	issues := make([]Issue, 0)
	for rows.Next() {
		is, cc, err := scanIssueWithCount(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("issues by repo %q: %w", repoID, err)
		}
		is.CommentCount = cc
		is.Labels = []string{}
		issues = append(issues, is)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("issues by repo %q: %w", repoID, err)
	}
	// Close before the label query: the sqlite store holds a single connection
	// (design §3a), so a second query must not run while these rows are open.
	_ = rows.Close()

	byID, err := s.labelNamesByIssue(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("issues by repo %q: %w", repoID, err)
	}
	for i := range issues {
		if names := byID[issues[i].ID]; names != nil {
			issues[i].Labels = names
		}
	}
	return issues, nil
}

// RecentClosedIssuesByRepo lists the repo's `limit` most recently closed
// issues, newest number first, in the same list shape as IssuesByRepo (label
// names and comment count loaded, comments not). Number-desc approximates
// close recency in this monotonically numbered store: numbers only grow, so
// a higher-numbered closed issue is usually the more recently closed one (a
// reopened-then-reclosed old issue sorts by its number, an accepted
// approximation) — the same recency-window semantic the forge backends
// answer with their recentupdate/updated-desc sorts (issue #176). The
// builtin tracker's closed/all Issues views compose this window onto the
// open set for contract uniformity with the forge backends; the local store
// itself is cheap, so the bound is about the seam contract, not perf.
func (s *Store) RecentClosedIssuesByRepo(ctx context.Context, repoID string, limit int) ([]Issue, error) {
	query := `SELECT ` + issueColumns + `,
		(SELECT COUNT(*) FROM issue_comments c WHERE c.issue_id = i.id) AS comment_count
		FROM issues i WHERE i.repo_id = ? AND i.state = ?
		ORDER BY i.number DESC LIMIT ?`

	rows, err := s.db.QueryContext(ctx, s.rebind(query), repoID, IssueStateClosed, limit)
	if err != nil {
		return nil, fmt.Errorf("recent closed issues by repo %q: %w", repoID, err)
	}
	issues := make([]Issue, 0)
	for rows.Next() {
		is, cc, err := scanIssueWithCount(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("recent closed issues by repo %q: %w", repoID, err)
		}
		is.CommentCount = cc
		is.Labels = []string{}
		issues = append(issues, is)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("recent closed issues by repo %q: %w", repoID, err)
	}
	// Close before the label query: the sqlite store holds a single connection
	// (design §3a), so a second query must not run while these rows are open.
	_ = rows.Close()

	byID, err := s.labelNamesByIssue(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("recent closed issues by repo %q: %w", repoID, err)
	}
	for i := range issues {
		if names := byID[issues[i].ID]; names != nil {
			issues[i].Labels = names
		}
	}
	return issues, nil
}

// IssueByRepoNumber returns one issue with its label names and full comment
// list (the detail view). ErrNotFound when no such (repo, number) exists.
func (s *Store) IssueByRepoNumber(ctx context.Context, repoID string, number int) (Issue, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+issueColumns+` FROM issues WHERE repo_id = ? AND number = ?`),
		repoID, number)
	is, err := scanIssue(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Issue{}, fmt.Errorf("issue %s#%d: %w", repoID, number, ErrNotFound)
		}
		return Issue{}, fmt.Errorf("issue %s#%d: %w", repoID, number, err)
	}
	names, err := s.LabelNamesForIssue(ctx, is.ID)
	if err != nil {
		return Issue{}, fmt.Errorf("issue %s#%d: %w", repoID, number, err)
	}
	is.Labels = names
	comments, err := s.CommentsByIssue(ctx, is.ID)
	if err != nil {
		return Issue{}, fmt.Errorf("issue %s#%d: %w", repoID, number, err)
	}
	is.Comments = comments
	is.CommentCount = len(comments)
	return is, nil
}

// IssueUpdate is a partial issue update: a set State transitions closed_at
// (stamped on close, cleared on reopen). Validation of enum values beyond the
// DB CHECK is here (state must be open|closed).
type IssueUpdate struct {
	Title Opt[string]
	Body  Opt[string]
	State Opt[string] // open|closed
}

// UpdateIssue applies the set fields of u to one issue and returns the full
// updated row (labels + comments). Every applied change bumps updated_at; a
// state change to closed stamps closed_at, a change to open clears it. A
// redundant close (state=closed on an already-closed issue) preserves the
// original closed_at — the stamp marks the open→closed transition, not the
// latest request. An empty update is a no-op that still verifies existence.
// ErrNotFound for a missing (repo, number).
func (s *Store) UpdateIssue(ctx context.Context, repoID string, number int, u IssueUpdate, now time.Time) (Issue, error) {
	now = storedTime(now)

	var (
		sets []string
		args []any
	)
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if u.Title.Set {
		add("title", u.Title.Value)
	}
	if u.Body.Set {
		add("body", u.Body.Value)
	}
	if u.State.Set {
		switch u.State.Value {
		case IssueStateClosed:
			add("state", IssueStateClosed)
			// COALESCE keeps an existing stamp: a redundant close must not
			// move closed_at off the original transition time.
			sets = append(sets, "closed_at = COALESCE(closed_at, ?)")
			args = append(args, fmtTime(now))
		case IssueStateOpen:
			add("state", IssueStateOpen)
			add("closed_at", nil)
		default:
			return Issue{}, fmt.Errorf("update issue %s#%d: invalid state %q", repoID, number, u.State.Value)
		}
	}
	if len(sets) == 0 {
		return s.IssueByRepoNumber(ctx, repoID, number)
	}
	add("updated_at", fmtTime(now))
	args = append(args, repoID, number)

	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE issues SET `+strings.Join(sets, ", ")+` WHERE repo_id = ? AND number = ?`), args...)
	if err != nil {
		return Issue{}, fmt.Errorf("update issue %s#%d: %w", repoID, number, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Issue{}, fmt.Errorf("update issue %s#%d: %w", repoID, number, err)
	}
	if n == 0 {
		return Issue{}, fmt.Errorf("update issue %s#%d: %w", repoID, number, ErrNotFound)
	}
	return s.IssueByRepoNumber(ctx, repoID, number)
}

// CreateIssueComment appends a comment to an issue and bumps the issue's
// updated_at (a comment is an issue mutation, matching forge behavior).
// authorKind must be CommentAuthorOperator or CommentAuthorRun; the run_id
// invariant is enforced here: a run-authored comment requires runID (the M5
// agent path) and any other author kind must pass nil. A missing issue (or a
// runID pointing at no run) maps to ErrNotFound.
func (s *Store) CreateIssueComment(ctx context.Context, issueID, authorKind string, runID *string, body string, now time.Time) (IssueComment, error) {
	if authorKind != CommentAuthorOperator && authorKind != CommentAuthorRun {
		return IssueComment{}, fmt.Errorf("create issue comment: invalid author kind %q", authorKind)
	}
	if authorKind == CommentAuthorRun && runID == nil {
		return IssueComment{}, fmt.Errorf("create issue comment: author kind %q requires a run_id", authorKind)
	}
	if authorKind != CommentAuthorRun && runID != nil {
		return IssueComment{}, fmt.Errorf("create issue comment: run_id set for author kind %q", authorKind)
	}
	now = storedTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IssueComment{}, fmt.Errorf("create issue comment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The updated_at bump doubles as the missing-issue check: zero rows
	// matched means no such issue.
	res, err := tx.ExecContext(ctx, s.rebind(
		`UPDATE issues SET updated_at = ? WHERE id = ?`), fmtTime(now), issueID)
	if err != nil {
		return IssueComment{}, fmt.Errorf("create issue comment: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return IssueComment{}, fmt.Errorf("create issue comment: %w", err)
	} else if n == 0 {
		return IssueComment{}, fmt.Errorf("create issue comment: issue %q: %w", issueID, ErrNotFound)
	}

	id := ids.NewID("cmt")
	_, err = tx.ExecContext(ctx, s.rebind(
		`INSERT INTO issue_comments (id, issue_id, author_kind, run_id, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`),
		id, issueID, authorKind, runID, body, fmtTime(now))
	if err != nil {
		if isForeignKeyViolation(err) {
			return IssueComment{}, fmt.Errorf("create issue comment: %w", ErrNotFound)
		}
		return IssueComment{}, fmt.Errorf("create issue comment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return IssueComment{}, fmt.Errorf("create issue comment: %w", err)
	}
	return IssueComment{
		ID: id, IssueID: issueID, AuthorKind: authorKind, RunID: runID,
		Body: body, CreatedAt: now,
	}, nil
}

// CommentsByIssue returns an issue's comments oldest first (chronological; the
// fixed-width timestamps make lexical order chronological). Ties within one
// stored millisecond order by id — deterministic across reads and backends,
// but arbitrary with respect to insertion order (ids are random; a monotonic
// tiebreak would need a schema change, deferred past M4).
func (s *Store) CommentsByIssue(ctx context.Context, issueID string) ([]IssueComment, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT id, issue_id, author_kind, run_id, body, created_at
		 FROM issue_comments WHERE issue_id = ? ORDER BY created_at, id`), issueID)
	if err != nil {
		return nil, fmt.Errorf("comments by issue %q: %w", issueID, err)
	}
	defer func() { _ = rows.Close() }()

	comments := make([]IssueComment, 0)
	for rows.Next() {
		var (
			c       IssueComment
			runID   sql.NullString
			created string
		)
		if err := rows.Scan(&c.ID, &c.IssueID, &c.AuthorKind, &runID, &c.Body, &created); err != nil {
			return nil, fmt.Errorf("comments by issue %q: %w", issueID, err)
		}
		c.RunID = nullStr(runID)
		if c.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("comments by issue %q: %w", issueID, err)
		}
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("comments by issue %q: %w", issueID, err)
	}
	return comments, nil
}

// labelNamesByIssue returns, for every issue in a repo, its sorted label names
// keyed by issue id. One query — the batch form the list view uses so it never
// issues a per-issue label query while the issues rows are open.
func (s *Store) labelNamesByIssue(ctx context.Context, repoID string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT il.issue_id, l.name
		 FROM issue_labels il
		 JOIN labels l ON l.id = il.label_id
		 JOIN issues i ON i.id = il.issue_id
		 WHERE i.repo_id = ?
		 ORDER BY il.issue_id, l.name`), repoID)
	if err != nil {
		return nil, fmt.Errorf("label names by issue for repo %q: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]string)
	for rows.Next() {
		var issueID, name string
		if err := rows.Scan(&issueID, &name); err != nil {
			return nil, fmt.Errorf("label names by issue for repo %q: %w", repoID, err)
		}
		out[issueID] = append(out[issueID], name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("label names by issue for repo %q: %w", repoID, err)
	}
	return out, nil
}

// scanIssue reads one row in issueColumns order.
func scanIssue(scan func(dest ...any) error) (Issue, error) {
	var (
		is               Issue
		runID            sql.NullString
		created, updated string
		closed           sql.NullString
	)
	if err := scan(&is.ID, &is.RepoID, &is.Number, &is.Title, &is.Body, &is.State,
		&is.AuthorKind, &runID, &created, &updated, &closed); err != nil {
		return Issue{}, err
	}
	is.RunID = nullStr(runID)
	var err error
	if is.CreatedAt, err = parseTime(created); err != nil {
		return Issue{}, err
	}
	if is.UpdatedAt, err = parseTime(updated); err != nil {
		return Issue{}, err
	}
	if is.ClosedAt, err = parseNullTime(closed); err != nil {
		return Issue{}, err
	}
	return is, nil
}

// scanIssueWithCount reads issueColumns followed by a trailing comment_count.
func scanIssueWithCount(scan func(dest ...any) error) (Issue, int, error) {
	var (
		is               Issue
		runID            sql.NullString
		created, updated string
		closed           sql.NullString
		count            int
	)
	if err := scan(&is.ID, &is.RepoID, &is.Number, &is.Title, &is.Body, &is.State,
		&is.AuthorKind, &runID, &created, &updated, &closed, &count); err != nil {
		return Issue{}, 0, err
	}
	is.RunID = nullStr(runID)
	var err error
	if is.CreatedAt, err = parseTime(created); err != nil {
		return Issue{}, 0, err
	}
	if is.UpdatedAt, err = parseTime(updated); err != nil {
		return Issue{}, 0, err
	}
	if is.ClosedAt, err = parseNullTime(closed); err != nil {
		return Issue{}, 0, err
	}
	return is, count, nil
}
