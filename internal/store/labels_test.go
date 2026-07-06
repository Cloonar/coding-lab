package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLabelsCRUD covers the label surface: the seeded triage set, color
// defaulting, the per-repo UNIQUE-name typed error, and update/delete not-found.
func TestLabelsCRUD(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)

		// The five triage labels are seeded at repo creation, sorted by name.
		labels, err := s.LabelsByRepo(ctx, repo.ID)
		if err != nil {
			t.Fatalf("LabelsByRepo: %v", err)
		}
		wantSeed := []string{"needs-info", "needs-triage", "ready-for-agent", "ready-for-human", "wontfix"}
		if got := labelNames(labels); !equalStrings(got, wantSeed) {
			t.Fatalf("seeded labels = %v, want %v", got, wantSeed)
		}

		// CreateLabel with empty color defaults to LabelDefaultColor.
		bug, err := s.CreateLabel(ctx, repo.ID, "bug", "", "defects")
		if err != nil {
			t.Fatalf("CreateLabel: %v", err)
		}
		if bug.Color != LabelDefaultColor || bug.Description != "defects" {
			t.Errorf("created label = %+v, want default color", bug)
		}

		// Duplicate name (new or seeded) → ErrNameTaken.
		if _, err := s.CreateLabel(ctx, repo.ID, "bug", "#000000", ""); !errors.Is(err, ErrNameTaken) {
			t.Errorf("duplicate name err = %v, want ErrNameTaken", err)
		}
		if _, err := s.CreateLabel(ctx, repo.ID, "wontfix", "#000000", ""); !errors.Is(err, ErrNameTaken) {
			t.Errorf("duplicate seeded name err = %v, want ErrNameTaken", err)
		}

		// Same name in a different repo is fine (uniqueness is per-repo).
		repo2 := seedRepoForRuns(t, s)
		if _, err := s.CreateLabel(ctx, repo2.ID, "bug", "", ""); err != nil {
			t.Errorf("CreateLabel same name other repo: %v", err)
		}

		// CreateLabel on a missing repo → ErrNotFound.
		if _, err := s.CreateLabel(ctx, "repo_00000000000000000000000000000000", "x", "", ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("CreateLabel missing repo err = %v, want ErrNotFound", err)
		}

		// UpdateLabel color + description.
		up, err := s.UpdateLabel(ctx, bug.ID, LabelUpdate{Color: Set("#111111"), Description: Set("bugs!")})
		if err != nil {
			t.Fatalf("UpdateLabel: %v", err)
		}
		if up.Color != "#111111" || up.Description != "bugs!" || up.Name != "bug" {
			t.Errorf("updated label = %+v", up)
		}
		// Rename onto an existing name → ErrNameTaken.
		if _, err := s.UpdateLabel(ctx, bug.ID, LabelUpdate{Name: Set("wontfix")}); !errors.Is(err, ErrNameTaken) {
			t.Errorf("rename collision err = %v, want ErrNameTaken", err)
		}
		// UpdateLabel missing id → ErrNotFound.
		if _, err := s.UpdateLabel(ctx, "lbl_00000000000000000000000000000000", LabelUpdate{Color: Set("#222222")}); !errors.Is(err, ErrNotFound) {
			t.Errorf("update missing label err = %v, want ErrNotFound", err)
		}

		// DeleteLabel removes it; a second delete → ErrNotFound.
		if err := s.DeleteLabel(ctx, bug.ID); err != nil {
			t.Fatalf("DeleteLabel: %v", err)
		}
		if err := s.DeleteLabel(ctx, bug.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete missing label err = %v, want ErrNotFound", err)
		}
	})
}

// TestIssueLabels covers the issue↔label join: replace, add/remove idempotency,
// sorted name read, and the FK not-found cases.
func TestIssueLabels(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "labelled", "", now)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		seed, _ := s.LabelsByRepo(ctx, repo.ID)
		lReady := labelID(t, seed, "ready-for-agent")
		lTriage := labelID(t, seed, "needs-triage")

		// Empty set → empty names.
		names, err := s.LabelNamesForIssue(ctx, is.ID)
		if err != nil {
			t.Fatalf("LabelNamesForIssue: %v", err)
		}
		if len(names) != 0 {
			t.Errorf("initial labels = %v, want none", names)
		}

		// SetIssueLabels replaces; names come back sorted.
		if err := s.SetIssueLabels(ctx, is.ID, []string{lReady, lTriage}, now); err != nil {
			t.Fatalf("SetIssueLabels: %v", err)
		}
		names, _ = s.LabelNamesForIssue(ctx, is.ID)
		if !equalStrings(names, []string{"needs-triage", "ready-for-agent"}) {
			t.Errorf("labels = %v, want sorted [needs-triage ready-for-agent]", names)
		}

		// Replace with a smaller set.
		if err := s.SetIssueLabels(ctx, is.ID, []string{lReady}, now); err != nil {
			t.Fatalf("SetIssueLabels replace: %v", err)
		}
		names, _ = s.LabelNamesForIssue(ctx, is.ID)
		if !equalStrings(names, []string{"ready-for-agent"}) {
			t.Errorf("labels after replace = %v, want [ready-for-agent]", names)
		}

		// AddIssueLabel is idempotent.
		if err := s.AddIssueLabel(ctx, is.ID, lTriage, now); err != nil {
			t.Fatalf("AddIssueLabel: %v", err)
		}
		if err := s.AddIssueLabel(ctx, is.ID, lTriage, now); err != nil {
			t.Fatalf("AddIssueLabel idempotent: %v", err)
		}
		names, _ = s.LabelNamesForIssue(ctx, is.ID)
		if !equalStrings(names, []string{"needs-triage", "ready-for-agent"}) {
			t.Errorf("labels after add = %v", names)
		}

		// RemoveIssueLabel; removing again is a no-op.
		if err := s.RemoveIssueLabel(ctx, is.ID, lTriage, now); err != nil {
			t.Fatalf("RemoveIssueLabel: %v", err)
		}
		if err := s.RemoveIssueLabel(ctx, is.ID, lTriage, now); err != nil {
			t.Fatalf("RemoveIssueLabel again: %v", err)
		}
		names, _ = s.LabelNamesForIssue(ctx, is.ID)
		if !equalStrings(names, []string{"ready-for-agent"}) {
			t.Errorf("labels after remove = %v, want [ready-for-agent]", names)
		}

		// Unknown label id → ErrNotFound (FK).
		if err := s.SetIssueLabels(ctx, is.ID, []string{"lbl_00000000000000000000000000000000"}, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetIssueLabels unknown label err = %v, want ErrNotFound", err)
		}
		// Unknown issue id (non-empty set) → ErrNotFound (FK).
		if err := s.SetIssueLabels(ctx, "iss_00000000000000000000000000000000", []string{lReady}, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetIssueLabels unknown issue err = %v, want ErrNotFound", err)
		}
		// Unknown issue id with an EMPTY set → still ErrNotFound (the doc
		// contract; the FK never fires for zero inserts, so the accessor must
		// verify the issue itself).
		if err := s.SetIssueLabels(ctx, "iss_00000000000000000000000000000000", nil, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetIssueLabels missing issue empty set err = %v, want ErrNotFound", err)
		}
		if err := s.SetIssueLabels(ctx, "iss_00000000000000000000000000000000", []string{}, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("SetIssueLabels missing issue empty slice err = %v, want ErrNotFound", err)
		}
	})
}

// TestIssueLabelMutations_bumpUpdatedAt pins that label mutations are issue
// mutations: Set/Add/Remove bump the issue's updated_at, while the idempotent
// no-ops (re-add of an existing pair, remove of an absent pair) do not.
func TestIssueLabelMutations_bumpUpdatedAt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "bump", "", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		seed, _ := s.LabelsByRepo(ctx, repo.ID)
		lReady := labelID(t, seed, "ready-for-agent")
		lTriage := labelID(t, seed, "needs-triage")

		updatedAt := func() time.Time {
			t.Helper()
			got, err := s.IssueByRepoNumber(ctx, repo.ID, is.Number)
			if err != nil {
				t.Fatalf("IssueByRepoNumber: %v", err)
			}
			return got.UpdatedAt
		}

		t1 := t0.Add(time.Hour)
		if err := s.SetIssueLabels(ctx, is.ID, []string{lReady}, t1); err != nil {
			t.Fatalf("SetIssueLabels: %v", err)
		}
		if got := updatedAt(); !got.Equal(storedTime(t1)) {
			t.Errorf("updated_at after SetIssueLabels = %v, want %v", got, storedTime(t1))
		}

		t2 := t1.Add(time.Hour)
		if err := s.AddIssueLabel(ctx, is.ID, lTriage, t2); err != nil {
			t.Fatalf("AddIssueLabel: %v", err)
		}
		if got := updatedAt(); !got.Equal(storedTime(t2)) {
			t.Errorf("updated_at after AddIssueLabel = %v, want %v", got, storedTime(t2))
		}
		// Idempotent re-add is not a mutation: no bump.
		t3 := t2.Add(time.Hour)
		if err := s.AddIssueLabel(ctx, is.ID, lTriage, t3); err != nil {
			t.Fatalf("AddIssueLabel idempotent: %v", err)
		}
		if got := updatedAt(); !got.Equal(storedTime(t2)) {
			t.Errorf("updated_at after idempotent add = %v, want unchanged %v", got, storedTime(t2))
		}

		t4 := t3.Add(time.Hour)
		if err := s.RemoveIssueLabel(ctx, is.ID, lTriage, t4); err != nil {
			t.Fatalf("RemoveIssueLabel: %v", err)
		}
		if got := updatedAt(); !got.Equal(storedTime(t4)) {
			t.Errorf("updated_at after RemoveIssueLabel = %v, want %v", got, storedTime(t4))
		}
		// Removing an absent pair is a no-op: no bump.
		t5 := t4.Add(time.Hour)
		if err := s.RemoveIssueLabel(ctx, is.ID, lTriage, t5); err != nil {
			t.Fatalf("RemoveIssueLabel absent: %v", err)
		}
		if got := updatedAt(); !got.Equal(storedTime(t4)) {
			t.Errorf("updated_at after absent remove = %v, want unchanged %v", got, storedTime(t4))
		}
	})
}

// TestDeleteLabel_cascadesIssueLabels pins the FK cascade: dropping a label
// removes its issue_labels rows (design §3a; enforced by the sqlite recipe).
func TestDeleteLabel_cascadesIssueLabels(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "x", "", now)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		lbl, err := s.CreateLabel(ctx, repo.ID, "temp", "", "")
		if err != nil {
			t.Fatalf("CreateLabel: %v", err)
		}
		if err := s.SetIssueLabels(ctx, is.ID, []string{lbl.ID}, now); err != nil {
			t.Fatalf("SetIssueLabels: %v", err)
		}
		if n := count(t, s, "issue_labels"); n != 1 {
			t.Fatalf("issue_labels rows = %d, want 1", n)
		}

		if err := s.DeleteLabel(ctx, lbl.ID); err != nil {
			t.Fatalf("DeleteLabel: %v", err)
		}
		if n := count(t, s, "issue_labels"); n != 0 {
			t.Errorf("issue_labels rows after delete = %d, want 0 (cascade)", n)
		}
		names, _ := s.LabelNamesForIssue(ctx, is.ID)
		if len(names) != 0 {
			t.Errorf("issue labels after label delete = %v, want none", names)
		}
	})
}

func labelNames(labels []Label) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = l.Name
	}
	return out
}
