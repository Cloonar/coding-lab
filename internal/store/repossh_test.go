package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestRepoSSHTargets_empty covers the base case a grant-free repo must hit:
// no cached targets returns an empty (non-nil) slice, never an error.
func TestRepoSSHTargets_empty(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

		repo := testRepo("ssh-empty", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}

		targets, err := s.RepoSSHTargets(ctx, repo.ID)
		if err != nil {
			t.Fatalf("RepoSSHTargets: %v", err)
		}
		if targets == nil {
			t.Error("RepoSSHTargets = nil, want empty non-nil slice")
		}
		if len(targets) != 0 {
			t.Errorf("RepoSSHTargets length = %d, want 0", len(targets))
		}
	})
}

// TestAddRepoSSHTarget_upsertIdempotent covers the picker toggle's "on"
// side: a first add inserts, a second add for the same target id updates
// target_name in place (upsert) rather than erroring or duplicating the row.
func TestAddRepoSSHTarget_upsertIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

		repo := testRepo("ssh-upsert", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}

		if err := s.AddRepoSSHTarget(ctx, repo.ID, SSHTarget{ID: "tgt-1", Name: "db-primary"}); err != nil {
			t.Fatalf("first add: %v", err)
		}
		if n := count(t, s, "repo_ssh_targets"); n != 1 {
			t.Fatalf("rows after first add = %d, want 1", n)
		}

		// Re-add the same target id under a renamed target: upsert, not a
		// duplicate row, and the name refreshes.
		if err := s.AddRepoSSHTarget(ctx, repo.ID, SSHTarget{ID: "tgt-1", Name: "db-primary-renamed"}); err != nil {
			t.Fatalf("second add (upsert): %v", err)
		}
		if n := count(t, s, "repo_ssh_targets"); n != 1 {
			t.Fatalf("rows after upsert = %d, want 1", n)
		}
		targets, err := s.RepoSSHTargets(ctx, repo.ID)
		if err != nil {
			t.Fatalf("RepoSSHTargets: %v", err)
		}
		if len(targets) != 1 || targets[0].Name != "db-primary-renamed" {
			t.Errorf("targets after upsert = %v, want [{tgt-1 db-primary-renamed}]", targets)
		}
	})
}

// TestRemoveRepoSSHTarget_idempotent covers the picker toggle's "off" side:
// removing twice is not an error, and removing an absent pair is not an
// error either.
func TestRemoveRepoSSHTarget_idempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)

		repo := testRepo("ssh-remove", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}
		if err := s.AddRepoSSHTarget(ctx, repo.ID, SSHTarget{ID: "tgt-1", Name: "db-primary"}); err != nil {
			t.Fatalf("add: %v", err)
		}

		if err := s.RemoveRepoSSHTarget(ctx, repo.ID, "tgt-1"); err != nil {
			t.Fatalf("first remove: %v", err)
		}
		if err := s.RemoveRepoSSHTarget(ctx, repo.ID, "tgt-1"); err != nil {
			t.Fatalf("second remove (idempotent, absent pair): %v", err)
		}
		if n := count(t, s, "repo_ssh_targets"); n != 0 {
			t.Errorf("rows after double remove = %d, want 0", n)
		}

		// Removing a pair that was never present at all is likewise not an
		// error.
		if err := s.RemoveRepoSSHTarget(ctx, repo.ID, "tgt-never-added"); err != nil {
			t.Errorf("remove never-added target: %v", err)
		}
	})
}

// TestReplaceRepoSSHTargets covers the wholesale-replace semantics: it
// overwrites whatever was cached, orders reads by name then id, collapses
// duplicate ids in the input (last one wins), and an empty slice clears the
// set.
func TestReplaceRepoSSHTargets(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

		repo := testRepo("ssh-replace", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}

		// Seed one target via the toggle path, then replace wholesale with a
		// different set (out of name order, plus a duplicate id where the
		// last occurrence must win) to prove Replace both drops the old row
		// and dedupes the input.
		if err := s.AddRepoSSHTarget(ctx, repo.ID, SSHTarget{ID: "old", Name: "stale-target"}); err != nil {
			t.Fatalf("seed add: %v", err)
		}

		err := s.ReplaceRepoSSHTargets(ctx, repo.ID, []SSHTarget{
			{ID: "tgt-z", Name: "zeta"},
			{ID: "tgt-a", Name: "alpha"},
			{ID: "tgt-a", Name: "alpha-final"}, // duplicate id, last wins
		})
		if err != nil {
			t.Fatalf("ReplaceRepoSSHTargets: %v", err)
		}

		targets, err := s.RepoSSHTargets(ctx, repo.ID)
		if err != nil {
			t.Fatalf("RepoSSHTargets: %v", err)
		}
		want := []SSHTarget{{ID: "tgt-a", Name: "alpha-final"}, {ID: "tgt-z", Name: "zeta"}}
		if !reflect.DeepEqual(targets, want) {
			t.Errorf("targets after replace = %v, want %v", targets, want)
		}

		// Empty slice clears.
		if err := s.ReplaceRepoSSHTargets(ctx, repo.ID, nil); err != nil {
			t.Fatalf("ReplaceRepoSSHTargets(nil): %v", err)
		}
		if n := count(t, s, "repo_ssh_targets"); n != 0 {
			t.Errorf("rows after clearing replace = %d, want 0", n)
		}
	})
}

// TestRepoSSHTargets_cascadeOnRepoDelete mirrors
// TestRepoSecrets_cascadeOnRepoDelete: deleting the repo removes its
// repo_ssh_targets rows via ON DELETE CASCADE.
func TestRepoSSHTargets_cascadeOnRepoDelete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)

		repo := testRepo("ssh-cascade", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}
		if err := s.AddRepoSSHTarget(ctx, repo.ID, SSHTarget{ID: "tgt-1", Name: "db-primary"}); err != nil {
			t.Fatalf("add: %v", err)
		}
		if err := s.AddRepoSSHTarget(ctx, repo.ID, SSHTarget{ID: "tgt-2", Name: "db-replica"}); err != nil {
			t.Fatalf("add: %v", err)
		}
		if n := count(t, s, "repo_ssh_targets"); n != 2 {
			t.Fatalf("rows before delete = %d, want 2", n)
		}

		if err := s.DeleteRepo(ctx, repo.ID); err != nil {
			t.Fatalf("delete repo: %v", err)
		}
		if n := count(t, s, "repo_ssh_targets"); n != 0 {
			t.Errorf("rows after repo delete = %d, want 0", n)
		}
	})
}

// TestRepoSSHTargets_unknownRepoRejected covers ErrNotFound (FOREIGN KEY
// violation mapped) for both write paths when repoID does not reference an
// existing repo, mirroring TestAddRepoImport_unknownRepo.
func TestRepoSSHTargets_unknownRepoRejected(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		const missing = "repo_00000000000000000000000000000000"

		if err := s.AddRepoSSHTarget(ctx, missing, SSHTarget{ID: "tgt-1", Name: "db-primary"}); !errors.Is(err, ErrNotFound) {
			t.Errorf("AddRepoSSHTarget unknown repo err = %v, want ErrNotFound", err)
		}
		if err := s.ReplaceRepoSSHTargets(ctx, missing, []SSHTarget{{ID: "tgt-1", Name: "db-primary"}}); !errors.Is(err, ErrNotFound) {
			t.Errorf("ReplaceRepoSSHTargets unknown repo err = %v, want ErrNotFound", err)
		}
		if n := count(t, s, "repo_ssh_targets"); n != 0 {
			t.Errorf("rows after rejected inserts = %d, want 0", n)
		}
	})
}

// TestRepoSSHTargets_validation covers the empty-repoID/target-ID/target-Name
// rejections shared by the write paths.
func TestRepoSSHTargets_validation(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)

		repo := testRepo("ssh-validate", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}

		if err := s.AddRepoSSHTarget(ctx, "", SSHTarget{ID: "tgt-1", Name: "db-primary"}); err == nil {
			t.Error("AddRepoSSHTarget with empty repo id = nil error, want a rejection")
		}
		if err := s.AddRepoSSHTarget(ctx, repo.ID, SSHTarget{ID: "", Name: "db-primary"}); err == nil {
			t.Error("AddRepoSSHTarget with empty target id = nil error, want a rejection")
		}
		if err := s.AddRepoSSHTarget(ctx, repo.ID, SSHTarget{ID: "tgt-1", Name: ""}); err == nil {
			t.Error("AddRepoSSHTarget with empty target name = nil error, want a rejection")
		}
		if err := s.RemoveRepoSSHTarget(ctx, "", "tgt-1"); err == nil {
			t.Error("RemoveRepoSSHTarget with empty repo id = nil error, want a rejection")
		}
		if err := s.RemoveRepoSSHTarget(ctx, repo.ID, ""); err == nil {
			t.Error("RemoveRepoSSHTarget with empty target id = nil error, want a rejection")
		}
		if err := s.ReplaceRepoSSHTargets(ctx, "", nil); err == nil {
			t.Error("ReplaceRepoSSHTargets with empty repo id = nil error, want a rejection")
		}
		if err := s.ReplaceRepoSSHTargets(ctx, repo.ID, []SSHTarget{{ID: "", Name: "x"}}); err == nil {
			t.Error("ReplaceRepoSSHTargets with empty target id in slice = nil error, want a rejection")
		}
		if err := s.ReplaceRepoSSHTargets(ctx, repo.ID, []SSHTarget{{ID: "tgt-1", Name: ""}}); err == nil {
			t.Error("ReplaceRepoSSHTargets with empty target name in slice = nil error, want a rejection")
		}
		if n := count(t, s, "repo_ssh_targets"); n != 0 {
			t.Errorf("rows after rejected writes = %d, want 0", n)
		}
	})
}
