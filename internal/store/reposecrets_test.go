package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

func TestValidSecretName(t *testing.T) {
	valid := []string{"API_KEY", "A", "X9_Y"}
	for _, name := range valid {
		if !ValidSecretName(name) {
			t.Errorf("ValidSecretName(%q) = false, want true", name)
		}
	}
	invalid := []string{"", "lower", "9X", "WITH-DASH", "WITH SPACE", "_X"}
	for _, name := range invalid {
		if ValidSecretName(name) {
			t.Errorf("ValidSecretName(%q) = true, want false", name)
		}
	}
}

func TestRepoSecretsCRUD(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 10, 10, 0, 0, 123_456_789, time.UTC)

		repo := testRepo("alpha", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}

		// Create + list: metadata only, no value field to check, ordered by name.
		zKey, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, "Z_KEY", "last alphabetically", []byte("blob-z"), now)
		if err != nil {
			t.Fatalf("create Z_KEY: %v", err)
		}
		if want := now.Truncate(time.Millisecond); !zKey.CreatedAt.Equal(want) || !zKey.UpdatedAt.Equal(want) {
			t.Errorf("timestamps = %v/%v, want both %v", zKey.CreatedAt, zKey.UpdatedAt, want)
		}
		aKey, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, "A_KEY", "first alphabetically", []byte("blob-a"), now)
		if err != nil {
			t.Fatalf("create A_KEY: %v", err)
		}

		secrets, err := s.RepoSecrets(ctx, repo.ID)
		if err != nil {
			t.Fatalf("RepoSecrets: %v", err)
		}
		if len(secrets) != 2 {
			t.Fatalf("RepoSecrets length = %d, want 2", len(secrets))
		}
		if secrets[0].Name != "A_KEY" || secrets[1].Name != "Z_KEY" {
			t.Errorf("order = [%s, %s], want [A_KEY, Z_KEY]", secrets[0].Name, secrets[1].Name)
		}
		if secrets[0].Description != "first alphabetically" {
			t.Errorf("A_KEY description = %q", secrets[0].Description)
		}
		if secrets[0].RepoID != repo.ID {
			t.Errorf("A_KEY repo id = %q, want %q", secrets[0].RepoID, repo.ID)
		}

		// RepoSecretByID.
		got, err := s.RepoSecretByID(ctx, aKey.ID)
		if err != nil {
			t.Fatalf("RepoSecretByID: %v", err)
		}
		if got.Name != "A_KEY" {
			t.Errorf("RepoSecretByID name = %q, want A_KEY", got.Name)
		}
		if _, err := s.RepoSecretByID(ctx, "sec_00000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
			t.Errorf("RepoSecretByID unknown err = %v, want ErrNotFound", err)
		}

		// Name validation.
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, "not valid", "", []byte("x"), now); !errors.Is(err, ErrInvalidSecretName) {
			t.Errorf("invalid name err = %v, want ErrInvalidSecretName", err)
		}

		// Duplicate name, same repo → ErrNameTaken.
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, "A_KEY", "", []byte("y"), now); !errors.Is(err, ErrNameTaken) {
			t.Errorf("duplicate name err = %v, want ErrNameTaken", err)
		}

		// Same name, different repo → OK.
		repo2 := testRepo("beta", now)
		if _, err := s.CreateRepo(ctx, repo2); err != nil {
			t.Fatalf("create repo2: %v", err)
		}
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo2.ID, "A_KEY", "", []byte("z"), now); err != nil {
			t.Errorf("create A_KEY in repo2: %v", err)
		}

		// Nonexistent repo → ErrNotFound.
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), "repo_00000000000000000000000000000000", "B_KEY", "", []byte("x"), now); !errors.Is(err, ErrNotFound) {
			t.Errorf("create with missing repo err = %v, want ErrNotFound", err)
		}

		// Rotate: blob changes, UpdatedAt bumps, CreatedAt unchanged.
		later := now.Add(time.Hour)
		rotated, err := s.RotateRepoSecret(ctx, aKey.ID, []byte("blob-a-rotated"), later)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		if !rotated.CreatedAt.Equal(aKey.CreatedAt) {
			t.Errorf("rotate CreatedAt = %v, want unchanged %v", rotated.CreatedAt, aKey.CreatedAt)
		}
		if want := later.Truncate(time.Millisecond); !rotated.UpdatedAt.Equal(want) {
			t.Errorf("rotate UpdatedAt = %v, want %v", rotated.UpdatedAt, want)
		}
		values, err := s.RepoSecretValues(ctx, repo.ID, []string{"A_KEY"})
		if err != nil {
			t.Fatalf("RepoSecretValues after rotate: %v", err)
		}
		if !bytes.Equal(values["A_KEY"], []byte("blob-a-rotated")) {
			t.Errorf("A_KEY value after rotate = %q, want blob-a-rotated", values["A_KEY"])
		}

		// Rotate missing id → ErrNotFound.
		if _, err := s.RotateRepoSecret(ctx, "sec_00000000000000000000000000000000", []byte("x"), later); !errors.Is(err, ErrNotFound) {
			t.Errorf("rotate missing err = %v, want ErrNotFound", err)
		}

		// Delete: gone afterwards; delete missing → ErrNotFound.
		if err := s.DeleteRepoSecret(ctx, zKey.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.RepoSecretByID(ctx, zKey.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("RepoSecretByID after delete err = %v, want ErrNotFound", err)
		}
		if err := s.DeleteRepoSecret(ctx, zKey.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete missing err = %v, want ErrNotFound", err)
		}
	})
}

// TestRepoSecrets_cascadeOnRepoDelete pins the FK cascade: deleting a repo
// removes its repo_secrets rows.
func TestRepoSecrets_cascadeOnRepoDelete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC)

		repo := testRepo("gamma", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, "CASCADE_KEY", "", []byte("x"), now); err != nil {
			t.Fatalf("create secret: %v", err)
		}
		if n := count(t, s, "repo_secrets"); n != 1 {
			t.Fatalf("repo_secrets rows = %d, want 1", n)
		}

		if err := s.DeleteRepo(ctx, repo.ID); err != nil {
			t.Fatalf("delete repo: %v", err)
		}
		secrets, err := s.RepoSecrets(ctx, repo.ID)
		if err != nil {
			t.Fatalf("RepoSecrets after repo delete: %v", err)
		}
		if len(secrets) != 0 {
			t.Errorf("RepoSecrets after repo delete = %v, want none", secrets)
		}
		if n := count(t, s, "repo_secrets"); n != 0 {
			t.Errorf("repo_secrets rows after cascade = %d, want 0", n)
		}
	})
}

// TestRepoSecretValues covers subset fetch, missing names, and cross-repo
// isolation: names belonging to another repo are never returned.
func TestRepoSecretValues(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

		repoA := testRepo("delta", now)
		if _, err := s.CreateRepo(ctx, repoA); err != nil {
			t.Fatalf("create repoA: %v", err)
		}
		repoB := testRepo("epsilon", now)
		if _, err := s.CreateRepo(ctx, repoB); err != nil {
			t.Fatalf("create repoB: %v", err)
		}

		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repoA.ID, "ONE", "", []byte("one-value"), now); err != nil {
			t.Fatalf("create ONE: %v", err)
		}
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repoA.ID, "TWO", "", []byte("two-value"), now); err != nil {
			t.Fatalf("create TWO: %v", err)
		}
		// Same name in the other repo, different value — must not leak into
		// repoA's fetch.
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repoB.ID, "ONE", "", []byte("other-repo-one"), now); err != nil {
			t.Fatalf("create repoB ONE: %v", err)
		}

		// Empty names slice -> empty map, no query.
		values, err := s.RepoSecretValues(ctx, repoA.ID, nil)
		if err != nil {
			t.Fatalf("RepoSecretValues empty: %v", err)
		}
		if len(values) != 0 {
			t.Errorf("RepoSecretValues(nil) = %v, want empty map", values)
		}

		// Subset fetch with a missing name mixed in.
		values, err = s.RepoSecretValues(ctx, repoA.ID, []string{"ONE", "MISSING"})
		if err != nil {
			t.Fatalf("RepoSecretValues subset: %v", err)
		}
		if len(values) != 1 {
			t.Fatalf("RepoSecretValues subset length = %d, want 1", len(values))
		}
		if !bytes.Equal(values["ONE"], []byte("one-value")) {
			t.Errorf("ONE value = %q, want one-value", values["ONE"])
		}
		if _, ok := values["MISSING"]; ok {
			t.Errorf("RepoSecretValues returned a value for MISSING")
		}
		if _, ok := values["TWO"]; ok {
			t.Errorf("RepoSecretValues returned unrequested name TWO")
		}

		// Cross-repo isolation: repoB's "ONE" must never satisfy repoA's request.
		if !bytes.Equal(values["ONE"], []byte("one-value")) {
			t.Errorf("repoA ONE leaked repoB's value: %q", values["ONE"])
		}
		valuesB, err := s.RepoSecretValues(ctx, repoB.ID, []string{"ONE"})
		if err != nil {
			t.Fatalf("RepoSecretValues repoB: %v", err)
		}
		if !bytes.Equal(valuesB["ONE"], []byte("other-repo-one")) {
			t.Errorf("repoB ONE = %q, want other-repo-one", valuesB["ONE"])
		}
	})
}

// TestAllRepoSecretValues covers the unfiltered fetch AllRepoSecretValues
// feeds the tailer's redactor build: every blob in a repo, keyed by name,
// and an empty map (not an error) for a repo with none.
func TestAllRepoSecretValues(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)

		repo := testRepo("zeta", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}

		// No secrets yet: empty map, not nil, not an error.
		values, err := s.AllRepoSecretValues(ctx, repo.ID)
		if err != nil {
			t.Fatalf("AllRepoSecretValues empty repo: %v", err)
		}
		if len(values) != 0 {
			t.Errorf("AllRepoSecretValues(empty repo) = %v, want empty map", values)
		}

		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, "ONE", "", []byte("one-value"), now); err != nil {
			t.Fatalf("create ONE: %v", err)
		}
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, "TWO", "", []byte("two-value"), now); err != nil {
			t.Fatalf("create TWO: %v", err)
		}

		// Another repo's secret must never appear in this repo's fetch.
		other := testRepo("eta", now)
		if _, err := s.CreateRepo(ctx, other); err != nil {
			t.Fatalf("create other repo: %v", err)
		}
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), other.ID, "THREE", "", []byte("three-value"), now); err != nil {
			t.Fatalf("create THREE in other repo: %v", err)
		}

		values, err = s.AllRepoSecretValues(ctx, repo.ID)
		if err != nil {
			t.Fatalf("AllRepoSecretValues: %v", err)
		}
		if len(values) != 2 {
			t.Fatalf("AllRepoSecretValues length = %d, want 2", len(values))
		}
		if !bytes.Equal(values["ONE"], []byte("one-value")) {
			t.Errorf("ONE value = %q, want one-value", values["ONE"])
		}
		if !bytes.Equal(values["TWO"], []byte("two-value")) {
			t.Errorf("TWO value = %q, want two-value", values["TWO"])
		}
		if _, ok := values["THREE"]; ok {
			t.Errorf("AllRepoSecretValues leaked another repo's secret THREE")
		}
	})
}

// TestMarkRepoSecretExposed covers the sticky first-writer-wins semantics:
// the first Mark flips unset -> exposed and returns true; a second Mark
// (same or different run) leaves the original exposure untouched and
// returns false; an unknown name returns (false, nil), not ErrNotFound.
func TestMarkRepoSecretExposed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)

		repo := testRepo("theta", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}
		if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, "LEAKY", "", []byte("blob"), now); err != nil {
			t.Fatalf("create secret: %v", err)
		}

		// Unknown name: false, nil — never ErrNotFound (concurrent-delete race).
		flipped, err := s.MarkRepoSecretExposed(ctx, repo.ID, "MISSING", "run_first", now)
		if err != nil {
			t.Fatalf("MarkRepoSecretExposed unknown name: %v", err)
		}
		if flipped {
			t.Error("MarkRepoSecretExposed(unknown name) = true, want false")
		}

		// First Mark: flips unset -> exposed.
		flipped, err = s.MarkRepoSecretExposed(ctx, repo.ID, "LEAKY", "run_first", now)
		if err != nil {
			t.Fatalf("MarkRepoSecretExposed first: %v", err)
		}
		if !flipped {
			t.Error("MarkRepoSecretExposed first call = false, want true")
		}

		secrets, err := s.RepoSecrets(ctx, repo.ID)
		if err != nil {
			t.Fatalf("RepoSecrets: %v", err)
		}
		if len(secrets) != 1 {
			t.Fatalf("RepoSecrets length = %d, want 1", len(secrets))
		}
		leaky := secrets[0]
		if leaky.ExposedRunID == nil || *leaky.ExposedRunID != "run_first" {
			t.Errorf("ExposedRunID = %v, want run_first", leaky.ExposedRunID)
		}
		if leaky.ExposedAt == nil || !leaky.ExposedAt.Equal(now.Truncate(time.Millisecond)) {
			t.Errorf("ExposedAt = %v, want %v", leaky.ExposedAt, now)
		}

		// Second Mark, different run and later time: sticky — original
		// run/timestamp untouched, returns false.
		later := now.Add(time.Hour)
		flipped, err = s.MarkRepoSecretExposed(ctx, repo.ID, "LEAKY", "run_second", later)
		if err != nil {
			t.Fatalf("MarkRepoSecretExposed second: %v", err)
		}
		if flipped {
			t.Error("MarkRepoSecretExposed second call = true, want false (sticky)")
		}
		secrets, err = s.RepoSecrets(ctx, repo.ID)
		if err != nil {
			t.Fatalf("RepoSecrets after second mark: %v", err)
		}
		leaky = secrets[0]
		if leaky.ExposedRunID == nil || *leaky.ExposedRunID != "run_first" {
			t.Errorf("ExposedRunID after second mark = %v, want unchanged run_first", leaky.ExposedRunID)
		}
		if leaky.ExposedAt == nil || !leaky.ExposedAt.Equal(now.Truncate(time.Millisecond)) {
			t.Errorf("ExposedAt after second mark = %v, want unchanged %v", leaky.ExposedAt, now)
		}

		// Rotate clears both columns; Mark can flip unset -> exposed again.
		rotated, err := s.RotateRepoSecret(ctx, leaky.ID, []byte("blob-rotated"), later)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		if rotated.ExposedRunID != nil || rotated.ExposedAt != nil {
			t.Errorf("rotated ExposedRunID/ExposedAt = %v/%v, want both nil", rotated.ExposedRunID, rotated.ExposedAt)
		}

		afterRotate := later.Add(time.Hour)
		flipped, err = s.MarkRepoSecretExposed(ctx, repo.ID, "LEAKY", "run_third", afterRotate)
		if err != nil {
			t.Fatalf("MarkRepoSecretExposed after rotate: %v", err)
		}
		if !flipped {
			t.Error("MarkRepoSecretExposed after rotate = false, want true (new edge)")
		}
	})
}

// TestExposedSecretNamesForRun covers the run-scoped filter, name ordering,
// and the empty-slice result for a run that has exposed nothing.
func TestExposedSecretNamesForRun(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)

		repo := testRepo("iota", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create repo: %v", err)
		}
		for _, name := range []string{"Z_KEY", "A_KEY", "M_KEY", "UNEXPOSED"} {
			if _, err := s.CreateRepoSecret(ctx, ids.NewID("sec"), repo.ID, name, "", []byte("blob-"+name), now); err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
		}

		// A run that has exposed nothing: empty slice, not nil, not an error.
		names, err := s.ExposedSecretNamesForRun(ctx, "run_clean")
		if err != nil {
			t.Fatalf("ExposedSecretNamesForRun clean run: %v", err)
		}
		if len(names) != 0 {
			t.Errorf("ExposedSecretNamesForRun(clean run) = %v, want empty", names)
		}

		if _, err := s.MarkRepoSecretExposed(ctx, repo.ID, "Z_KEY", "run_leaky", now); err != nil {
			t.Fatalf("mark Z_KEY: %v", err)
		}
		if _, err := s.MarkRepoSecretExposed(ctx, repo.ID, "A_KEY", "run_leaky", now); err != nil {
			t.Fatalf("mark A_KEY: %v", err)
		}
		// A different run's exposure must not leak into run_leaky's list.
		if _, err := s.MarkRepoSecretExposed(ctx, repo.ID, "M_KEY", "run_other", now); err != nil {
			t.Fatalf("mark M_KEY: %v", err)
		}

		names, err = s.ExposedSecretNamesForRun(ctx, "run_leaky")
		if err != nil {
			t.Fatalf("ExposedSecretNamesForRun: %v", err)
		}
		if len(names) != 2 || names[0] != "A_KEY" || names[1] != "Z_KEY" {
			t.Errorf("ExposedSecretNamesForRun(run_leaky) = %v, want [A_KEY Z_KEY]", names)
		}

		namesOther, err := s.ExposedSecretNamesForRun(ctx, "run_other")
		if err != nil {
			t.Fatalf("ExposedSecretNamesForRun run_other: %v", err)
		}
		if len(namesOther) != 1 || namesOther[0] != "M_KEY" {
			t.Errorf("ExposedSecretNamesForRun(run_other) = %v, want [M_KEY]", namesOther)
		}
	})
}
