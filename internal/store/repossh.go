package store

// Repo SSH target cache accessors (issue #39, Warpgate bastion integration).
// repo_ssh_targets (migration 0023) is a CACHE of Warpgate's role→target
// assignment for a repo — not a lab-side grant model; Warpgate stays the
// source of truth. It exists so the spawn path can decide, without calling
// Warpgate, whether a repo has any SSH grants at all: the fail-closed rule
// only applies to a repo with >=1 assigned target, and that must be
// decidable locally even when Warpgate is unreachable. Like repo_imports it
// is a pure relation table — no surrogate id, (repo_id, target_id) is the
// identity — and cascades with its repo (ON DELETE CASCADE).
//
// Two write granularities, both covered here: AddRepoSSHTarget/
// RemoveRepoSSHTarget are the per-repo picker's single-toggle upsert/delete;
// ReplaceRepoSSHTargets is the wholesale replace used by the picker's own
// listing, a startup heal pass, and a grant-bearing spawn's pre-flight
// check, each reading Warpgate's truth fresh and overwriting the cache to
// match.

import (
	"context"
	"errors"
	"fmt"
)

// SSHTarget is one cached Warpgate SSH target assigned to a repo's role.
type SSHTarget struct {
	ID   string // Warpgate target id
	Name string // Warpgate target name at last sync
}

// validateSSHTarget rejects a target with an empty id or name — both are
// NOT NULL, non-empty columns, and a blank value is never a meaningful
// Warpgate identifier or display name.
func validateSSHTarget(t SSHTarget) error {
	if t.ID == "" {
		return errors.New("empty target id")
	}
	if t.Name == "" {
		return errors.New("empty target name")
	}
	return nil
}

// RepoSSHTargets returns repoID's cached SSH targets ordered by name (then
// id, to break ties deterministically). Empty slice, not nil, when the repo
// has none cached — including an unknown repoID, which is not an error here:
// this is a read, mirroring RepoImports/RepoSecrets' treatment of an unknown
// id as simply having no rows.
func (s *Store) RepoSSHTargets(ctx context.Context, repoID string) ([]SSHTarget, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT target_id, target_name FROM repo_ssh_targets
		 WHERE repo_id = ? ORDER BY target_name, target_id`), repoID)
	if err != nil {
		return nil, fmt.Errorf("repo ssh targets for %q: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	targets := make([]SSHTarget, 0)
	for rows.Next() {
		var t SSHTarget
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, fmt.Errorf("repo ssh targets for %q: %w", repoID, err)
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo ssh targets for %q: %w", repoID, err)
	}
	return targets, nil
}

// ReplaceRepoSSHTargets replaces repoID's whole cached SSH target set with
// exactly targets, in one transaction (delete all, then insert given) — the
// wholesale-replace path used by the picker's listing, a startup heal pass,
// and a grant-bearing spawn's pre-flight read of Warpgate's truth. Duplicate
// target IDs in targets are collapsed, last one in the slice wins (a caller
// building the slice from a Warpgate listing should never produce
// duplicates, but the store does not trust that). An empty or nil targets
// clears the cached set. Any target failing validateSSHTarget rejects the
// whole call before anything is written. A FOREIGN KEY violation (unknown
// repoID) maps to ErrNotFound, mirroring AddRepoImport.
func (s *Store) ReplaceRepoSSHTargets(ctx context.Context, repoID string, targets []SSHTarget) error {
	if repoID == "" {
		return errors.New("replace repo ssh targets: empty repo id")
	}
	for _, t := range targets {
		if err := validateSSHTarget(t); err != nil {
			return fmt.Errorf("replace repo ssh targets for %q: %w", repoID, err)
		}
	}

	// Collapse duplicate target IDs, preserving first-seen order but letting
	// the last occurrence's fields win.
	order := make([]string, 0, len(targets))
	byID := make(map[string]SSHTarget, len(targets))
	for _, t := range targets {
		if _, ok := byID[t.ID]; !ok {
			order = append(order, t.ID)
		}
		byID[t.ID] = t
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace repo ssh targets for %q: %w", repoID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(
		`DELETE FROM repo_ssh_targets WHERE repo_id = ?`), repoID); err != nil {
		return fmt.Errorf("replace repo ssh targets for %q: %w", repoID, err)
	}
	for _, id := range order {
		t := byID[id]
		if _, err := tx.ExecContext(ctx, s.rebind(
			`INSERT INTO repo_ssh_targets (repo_id, target_id, target_name) VALUES (?, ?, ?)`),
			repoID, t.ID, t.Name); err != nil {
			if isForeignKeyViolation(err) {
				return fmt.Errorf("replace repo ssh targets for %q: %w", repoID, ErrNotFound)
			}
			return fmt.Errorf("replace repo ssh targets for %q: %w", repoID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace repo ssh targets for %q: %w", repoID, err)
	}
	return nil
}

// AddRepoSSHTarget upserts one cached target for repoID — the picker
// toggle's "on" side. Idempotent: an existing (repoID, target.ID) pair has
// its target_name refreshed to target.Name rather than erroring, so a
// re-sync that finds the same target under a new Warpgate display name
// still converges. A FOREIGN KEY violation (unknown repoID) maps to
// ErrNotFound, mirroring AddRepoImport.
func (s *Store) AddRepoSSHTarget(ctx context.Context, repoID string, t SSHTarget) error {
	if repoID == "" {
		return errors.New("add repo ssh target: empty repo id")
	}
	if err := validateSSHTarget(t); err != nil {
		return fmt.Errorf("add repo ssh target to %q: %w", repoID, err)
	}
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO repo_ssh_targets (repo_id, target_id, target_name) VALUES (?, ?, ?)
		 ON CONFLICT (repo_id, target_id) DO UPDATE SET target_name = excluded.target_name`),
		repoID, t.ID, t.Name)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("add repo ssh target %q to %q: %w", t.ID, repoID, ErrNotFound)
		}
		return fmt.Errorf("add repo ssh target %q to %q: %w", t.ID, repoID, err)
	}
	return nil
}

// RemoveRepoSSHTarget retracts one cached target for repoID — the picker
// toggle's "off" side. Idempotent: removing an absent pair is not an error.
func (s *Store) RemoveRepoSSHTarget(ctx context.Context, repoID, targetID string) error {
	if repoID == "" {
		return errors.New("remove repo ssh target: empty repo id")
	}
	if targetID == "" {
		return fmt.Errorf("remove repo ssh target from %q: empty target id", repoID)
	}
	_, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM repo_ssh_targets WHERE repo_id = ? AND target_id = ?`), repoID, targetID)
	if err != nil {
		return fmt.Errorf("remove repo ssh target %q from %q: %w", targetID, repoID, err)
	}
	return nil
}
