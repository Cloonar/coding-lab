package reposvc

// Read-only imports (issue #261 / ADR-0063): a consumer-declared,
// directional grant — the importing repo's settings list what it reads, and
// only that repo's settings; there is no target-side approval and no
// transitive closure. Mutual imports are legal (a server importing its
// client while the client imports the server is the common case, not a
// cycle to guard against). Self-import and an unknown target are both
// business-rule 400s that belong here rather than in the store, matching
// where every other repo-settings validation lives; the store's own
// self-import guard (AddRepoImport) is a defensive backstop, not the
// primary check. The store's delete-time ImportersError/ErrHasImporters
// guard is deliberately NOT mirrored or bypassed here — force does not
// reach it, since it protects another repo's declared world rather than
// lab's own recoverable state (ADR-0063).

import (
	"context"
	"errors"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// Imports returns the repos repoID has declared read-only imports of
// (store.RepoImports — targets, ordered by name). ErrNotFound if repoID
// itself is unknown.
func (s *Service) Imports(ctx context.Context, repoID string) ([]store.Repo, error) {
	if _, err := s.store.RepoByID(ctx, repoID); err != nil {
		return nil, err
	}
	return s.store.RepoImports(ctx, repoID)
}

// AddImport declares that repoID's instances may read targetID's
// origin/<default> (ADR-0063). Validated, in order, before the store write:
// repoID must exist (ErrNotFound); targetID must not equal repoID (400 —
// a repository cannot import itself); targetID must name a known repo (400,
// not 404 — this is a validation failure of repoID's own imports field, not
// a missing resource of its own). Re-adding an already-declared target
// succeeds (store.AddRepoImport is idempotent). Returns the target's row.
func (s *Service) AddImport(ctx context.Context, repoID, targetID string) (store.Repo, error) {
	if _, err := s.store.RepoByID(ctx, repoID); err != nil {
		return store.Repo{}, err
	}
	if targetID == repoID {
		return store.Repo{}, badRequestf("imports: a repository cannot import itself")
	}
	target, err := s.store.RepoByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Repo{}, badRequestf("imports: unknown target repository %q", targetID)
		}
		return store.Repo{}, err
	}
	if err := s.store.AddRepoImport(ctx, repoID, targetID); err != nil {
		return store.Repo{}, err
	}
	s.publishRepoChanged(repoID)
	return target, nil
}

// RemoveImport retracts a read-only import declaration (idempotent —
// removing an absent pair is not an error, matching store.RemoveRepoImport).
// repoID must still exist; targetID is not otherwise validated, so removing
// a never-valid or already-removed target answers success rather than a
// spurious error.
func (s *Service) RemoveImport(ctx context.Context, repoID, targetID string) error {
	if _, err := s.store.RepoByID(ctx, repoID); err != nil {
		return err
	}
	if err := s.store.RemoveRepoImport(ctx, repoID, targetID); err != nil {
		return err
	}
	s.publishRepoChanged(repoID)
	return nil
}
