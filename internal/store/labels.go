package store

// Built-in tracker: label accessors (design §3a). Labels are per-repo, unique
// by name within a repo (UNIQUE(repo_id, name) → ErrNameTaken). The five triage
// labels are seeded at repo creation (SeedRepoLabels, in repos.go); these
// accessors are the operator's label-CRUD surface plus the issue↔label join
// (issue_labels) the built-in tracker reads. Deleting a label cascades its
// issue_labels rows (FK ON DELETE CASCADE, enforced by the sqlite open recipe).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// LabelDefaultColor is the fallback swatch when CreateLabel is given no color
// (design §3a: labels.color DEFAULT).
const LabelDefaultColor = "#6b7280"

// Label is one row of labels.
type Label struct {
	ID          string
	RepoID      string
	Name        string
	Color       string
	Description string
}

// labelColumns is the one column list every label SELECT uses, in scanLabel
// order.
const labelColumns = `id, repo_id, name, color, description`

// LabelsByRepo lists a repo's labels ordered by name.
func (s *Store) LabelsByRepo(ctx context.Context, repoID string) ([]Label, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+labelColumns+` FROM labels WHERE repo_id = ? ORDER BY name`), repoID)
	if err != nil {
		return nil, fmt.Errorf("labels by repo %q: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	labels := make([]Label, 0)
	for rows.Next() {
		l, err := scanLabel(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("labels by repo %q: %w", repoID, err)
		}
		labels = append(labels, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("labels by repo %q: %w", repoID, err)
	}
	return labels, nil
}

// LabelByID returns one label, or ErrNotFound.
func (s *Store) LabelByID(ctx context.Context, id string) (Label, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+labelColumns+` FROM labels WHERE id = ?`), id)
	l, err := scanLabel(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Label{}, fmt.Errorf("label %q: %w", id, ErrNotFound)
		}
		return Label{}, fmt.Errorf("label %q: %w", id, err)
	}
	return l, nil
}

// CreateLabel adds a label to a repo. An empty color defaults to
// LabelDefaultColor. A duplicate name within the repo maps to ErrNameTaken; a
// missing repo to ErrNotFound.
func (s *Store) CreateLabel(ctx context.Context, repoID, name, color, description string) (Label, error) {
	if color == "" {
		color = LabelDefaultColor
	}
	id := ids.NewID("lbl")
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO labels (id, repo_id, name, color, description)
		 VALUES (?, ?, ?, ?, ?)`),
		id, repoID, name, color, description)
	if err != nil {
		if isUniqueViolation(err) {
			return Label{}, fmt.Errorf("create label %q: %w", name, ErrNameTaken)
		}
		if isForeignKeyViolation(err) {
			return Label{}, fmt.Errorf("create label %q: repo %q: %w", name, repoID, ErrNotFound)
		}
		return Label{}, fmt.Errorf("create label %q: %w", name, err)
	}
	return Label{ID: id, RepoID: repoID, Name: name, Color: color, Description: description}, nil
}

// LabelUpdate is a partial label update.
type LabelUpdate struct {
	Name        Opt[string]
	Color       Opt[string]
	Description Opt[string]
}

// UpdateLabel applies the set fields of u and returns the updated row. An empty
// update is a no-op that still verifies existence. A rename collision maps to
// ErrNameTaken; a missing id to ErrNotFound.
func (s *Store) UpdateLabel(ctx context.Context, id string, u LabelUpdate) (Label, error) {
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
	if u.Color.Set {
		add("color", u.Color.Value)
	}
	if u.Description.Set {
		add("description", u.Description.Value)
	}
	if len(sets) == 0 {
		return s.LabelByID(ctx, id)
	}
	args = append(args, id)

	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE labels SET `+strings.Join(sets, ", ")+` WHERE id = ?`), args...)
	if err != nil {
		if isUniqueViolation(err) {
			return Label{}, fmt.Errorf("update label %q: %w", id, ErrNameTaken)
		}
		return Label{}, fmt.Errorf("update label %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Label{}, fmt.Errorf("update label %q: %w", id, err)
	}
	if n == 0 {
		return Label{}, fmt.Errorf("update label %q: %w", id, ErrNotFound)
	}
	return s.LabelByID(ctx, id)
}

// DeleteLabel removes a label; its issue_labels rows cascade (FK ON DELETE
// CASCADE). ErrNotFound for a missing id.
func (s *Store) DeleteLabel(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM labels WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete label %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete label %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete label %q: %w", id, ErrNotFound)
	}
	return nil
}

// SetIssueLabels replaces an issue's label set with exactly labelIDs (the
// operator PUT /labels semantics) and bumps the issue's updated_at (a label
// mutation is an issue mutation). Runs in a transaction: verify + bump, clear,
// then insert. A missing issue or label maps to ErrNotFound — including a
// missing issue with an empty label set.
func (s *Store) SetIssueLabels(ctx context.Context, issueID string, labelIDs []string, now time.Time) error {
	now = storedTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set issue labels for %q: %w", issueID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// The updated_at bump doubles as the missing-issue check (the INSERT
	// loop's FK never fires for an empty set): zero rows matched → ErrNotFound.
	res, err := tx.ExecContext(ctx, s.rebind(
		`UPDATE issues SET updated_at = ? WHERE id = ?`), fmtTime(now), issueID)
	if err != nil {
		return fmt.Errorf("set issue labels for %q: %w", issueID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set issue labels for %q: %w", issueID, err)
	} else if n == 0 {
		return fmt.Errorf("set issue labels for %q: %w", issueID, ErrNotFound)
	}

	if _, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM issue_labels WHERE issue_id = ?`), issueID); err != nil {
		return fmt.Errorf("set issue labels for %q: %w", issueID, err)
	}
	for _, labelID := range labelIDs {
		_, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO issue_labels (issue_id, label_id) VALUES (?, ?)
			 ON CONFLICT (issue_id, label_id) DO NOTHING`), issueID, labelID)
		if err != nil {
			if isForeignKeyViolation(err) {
				return fmt.Errorf("set issue labels for %q: %w", issueID, ErrNotFound)
			}
			return fmt.Errorf("set issue labels for %q: %w", issueID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set issue labels for %q: %w", issueID, err)
	}
	return nil
}

// AddIssueLabel attaches one label to an issue (idempotent — an existing pair
// is left untouched) and bumps the issue's updated_at when the pair was
// actually inserted (an idempotent no-op is not a mutation). A missing issue
// or label maps to ErrNotFound.
func (s *Store) AddIssueLabel(ctx context.Context, issueID, labelID string, now time.Time) error {
	now = storedTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("add issue label %q to %q: %w", labelID, issueID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO issue_labels (issue_id, label_id) VALUES (?, ?)
		 ON CONFLICT (issue_id, label_id) DO NOTHING`), issueID, labelID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("add issue label %q to %q: %w", labelID, issueID, ErrNotFound)
		}
		return fmt.Errorf("add issue label %q to %q: %w", labelID, issueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("add issue label %q to %q: %w", labelID, issueID, err)
	}
	if n > 0 {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`UPDATE issues SET updated_at = ? WHERE id = ?`), fmtTime(now), issueID); err != nil {
			return fmt.Errorf("add issue label %q to %q: %w", labelID, issueID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("add issue label %q to %q: %w", labelID, issueID, err)
	}
	return nil
}

// RemoveIssueLabel detaches one label from an issue (idempotent — removing an
// absent pair is not an error) and bumps the issue's updated_at when a pair
// was actually removed.
func (s *Store) RemoveIssueLabel(ctx context.Context, issueID, labelID string, now time.Time) error {
	now = storedTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("remove issue label %q from %q: %w", labelID, issueID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM issue_labels WHERE issue_id = ? AND label_id = ?`),
		issueID, labelID)
	if err != nil {
		return fmt.Errorf("remove issue label %q from %q: %w", labelID, issueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove issue label %q from %q: %w", labelID, issueID, err)
	}
	if n > 0 {
		if _, err := tx.ExecContext(ctx, s.rebind(
			`UPDATE issues SET updated_at = ? WHERE id = ?`), fmtTime(now), issueID); err != nil {
			return fmt.Errorf("remove issue label %q from %q: %w", labelID, issueID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("remove issue label %q from %q: %w", labelID, issueID, err)
	}
	return nil
}

// LabelNamesForIssue returns an issue's label names sorted by name.
func (s *Store) LabelNamesForIssue(ctx context.Context, issueID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT l.name FROM issue_labels il
		 JOIN labels l ON l.id = il.label_id
		 WHERE il.issue_id = ? ORDER BY l.name`), issueID)
	if err != nil {
		return nil, fmt.Errorf("label names for issue %q: %w", issueID, err)
	}
	defer func() { _ = rows.Close() }()

	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("label names for issue %q: %w", issueID, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("label names for issue %q: %w", issueID, err)
	}
	return names, nil
}

// scanLabel reads one row in labelColumns order.
func scanLabel(scan func(dest ...any) error) (Label, error) {
	var l Label
	if err := scan(&l.ID, &l.RepoID, &l.Name, &l.Color, &l.Description); err != nil {
		return Label{}, err
	}
	return l, nil
}
