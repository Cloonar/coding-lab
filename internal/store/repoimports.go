package store

// Repo import accessors (issue #261, "read-only imports"): a repo declares
// other repos its instances may read. repo_imports (migration 0021) is a
// pure relation table like issue_labels — no surrogate id, the (repo_id,
// target_repo_id) pair is the identity. repo_id is the importing (consumer)
// repo and cascades with it; target_repo_id has no delete action, so
// store.DeleteRepo carries the friendly named-importers refusal (see
// ImportersError in errors.go) and this file's bare FK is only a race
// backstop. Self-import and unknown-target validation live at the API layer
// (migration 0021's header); AddRepoImport still defends against the
// nonsense row defensively, short of a full DB CHECK.

import (
	"context"
	"fmt"
	"strings"
)

// qualifiedRepoColumns is repoColumns with every column qualified under the
// "r." alias used by the JOINs below (repos AS r joined against
// repo_imports). SELECT * against a JOIN needs every column disambiguated by
// table even where nothing collides today, so a SELECT built on a JOIN
// always qualifies rather than relying on repo_imports staying columnless.
var qualifiedRepoColumns = qualifyColumns(repoColumns, "r")

// qualifyColumns prefixes every column in a comma-separated column list
// (such as repoColumns) with "<alias>.". Column names never contain commas
// or embedded whitespace, so splitting on "," and trimming is exact even
// though repoColumns itself is wrapped across several lines.
func qualifyColumns(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// AddRepoImport declares that repoID's instances may read targetID
// (idempotent — an existing pair is left untouched). A self-import
// (repoID == targetID) is refused before the query even runs: the API layer
// rejects it with a 400 first, but the store must never persist the nonsense
// row regardless of caller. A FOREIGN KEY violation (either id unknown) maps
// to ErrNotFound.
func (s *Store) AddRepoImport(ctx context.Context, repoID, targetID string) error {
	if repoID == targetID {
		return fmt.Errorf("add repo import %q -> %q: a repo cannot import itself", repoID, targetID)
	}
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO repo_imports (repo_id, target_repo_id) VALUES (?, ?)
		 ON CONFLICT (repo_id, target_repo_id) DO NOTHING`),
		repoID, targetID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("add repo import %q -> %q: %w", repoID, targetID, ErrNotFound)
		}
		return fmt.Errorf("add repo import %q -> %q: %w", repoID, targetID, err)
	}
	return nil
}

// RemoveRepoImport retracts a read-only import declaration (idempotent — an
// absent pair is not an error).
func (s *Store) RemoveRepoImport(ctx context.Context, repoID, targetID string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM repo_imports WHERE repo_id = ? AND target_repo_id = ?`),
		repoID, targetID)
	if err != nil {
		return fmt.Errorf("remove repo import %q -> %q: %w", repoID, targetID, err)
	}
	return nil
}

// RepoImports returns the full rows of the repos repoID has declared
// read-only imports of, ordered by name.
func (s *Store) RepoImports(ctx context.Context, repoID string) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+qualifiedRepoColumns+` FROM repos r
		 JOIN repo_imports i ON i.target_repo_id = r.id
		 WHERE i.repo_id = ? ORDER BY r.name`), repoID)
	if err != nil {
		return nil, fmt.Errorf("repo imports for %q: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	repos := make([]Repo, 0)
	for rows.Next() {
		r, err := scanRepo(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo imports for %q: %w", repoID, err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo imports for %q: %w", repoID, err)
	}
	return repos, nil
}

// RepoImporters returns the full rows of the repos that have declared
// targetID a read-only import — the reverse direction of RepoImports — used
// by DeleteRepo's confirmation prompt on the API side, ordered by name.
func (s *Store) RepoImporters(ctx context.Context, targetID string) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT `+qualifiedRepoColumns+` FROM repos r
		 JOIN repo_imports i ON i.repo_id = r.id
		 WHERE i.target_repo_id = ? ORDER BY r.name`), targetID)
	if err != nil {
		return nil, fmt.Errorf("repo importers of %q: %w", targetID, err)
	}
	defer func() { _ = rows.Close() }()

	repos := make([]Repo, 0)
	for rows.Next() {
		r, err := scanRepo(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo importers of %q: %w", targetID, err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo importers of %q: %w", targetID, err)
	}
	return repos, nil
}
