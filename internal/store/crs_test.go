package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestCreateCR_perRepoNumberSequence pins the per-repo CR-number counter
// (design §2): numbers start at 1 and advance independently per repo,
// allocated from next_cr_number in the create transaction — and independently
// of the issue counter on the same repo.
func TestCreateCR_perRepoNumberSequence(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repoA := seedRepoForRuns(t, s)
		repoB := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		// Issues on repo A do not advance the CR counter.
		if _, err := s.CreateIssue(ctx, repoA.ID, "an issue", "", now); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}

		for _, want := range []int{1, 2, 3} {
			cr, err := s.CreateCR(ctx, repoA.ID, fmt.Sprintf("A-%d", want), "body A", "afk/1", "main", nil, now)
			if err != nil {
				t.Fatalf("CreateCR A: %v", err)
			}
			if cr.Number != want {
				t.Errorf("repo A CR number = %d, want %d", cr.Number, want)
			}
			if cr.State != CRStateOpen || cr.MergedAt != nil || cr.MergeCommit != nil {
				t.Errorf("new CR = state %q merged_at %v merge_commit %v, want open/nil/nil",
					cr.State, cr.MergedAt, cr.MergeCommit)
			}
			if !cr.CreatedAt.Equal(storedTime(now)) || !cr.UpdatedAt.Equal(storedTime(now)) {
				t.Errorf("timestamps = %v/%v, want %v", cr.CreatedAt, cr.UpdatedAt, storedTime(now))
			}
			if cr.Closes == nil || len(cr.Closes) != 0 {
				t.Errorf("new CR closes = %v, want empty non-nil", cr.Closes)
			}
		}
		// Repo B's counter is independent.
		for _, want := range []int{1, 2} {
			cr, err := s.CreateCR(ctx, repoB.ID, fmt.Sprintf("B-%d", want), "", "afk/2", "main", nil, now)
			if err != nil {
				t.Fatalf("CreateCR B: %v", err)
			}
			if cr.Number != want {
				t.Errorf("repo B CR number = %d, want %d", cr.Number, want)
			}
		}

		if _, err := s.CreateCR(ctx, "repo_00000000000000000000000000000000", "x", "", "afk/1", "main", nil, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("CreateCR on missing repo err = %v, want ErrNotFound", err)
		}
	})
}

// TestCreateCR_closesRows covers the cr_closes persistence: the closes list
// is normalized (deduplicated, non-positive dropped, sorted ascending) and
// read back identically by the detail accessor.
func TestCreateCR_closesRows(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		cr, err := s.CreateCR(ctx, repo.ID, "closer", "Closes #7\n\nFixes #3", "afk/7", "main",
			[]int{7, 3, 7, 0, -1}, now)
		if err != nil {
			t.Fatalf("CreateCR: %v", err)
		}
		if len(cr.Closes) != 2 || cr.Closes[0] != 3 || cr.Closes[1] != 7 {
			t.Errorf("created closes = %v, want [3 7]", cr.Closes)
		}

		got, err := s.CRByRepoNumber(ctx, repo.ID, cr.Number)
		if err != nil {
			t.Fatalf("CRByRepoNumber: %v", err)
		}
		if len(got.Closes) != 2 || got.Closes[0] != 3 || got.Closes[1] != 7 {
			t.Errorf("stored closes = %v, want [3 7]", got.Closes)
		}
		if got.Title != "closer" || got.Body != "Closes #7\n\nFixes #3" ||
			got.HeadBranch != "afk/7" || got.BaseBranch != "main" {
			t.Errorf("stored CR = %+v", got)
		}

		if _, err := s.CRByRepoNumber(ctx, repo.ID, 999); !errors.Is(err, ErrNotFound) {
			t.Errorf("CRByRepoNumber(999) err = %v, want ErrNotFound", err)
		}
	})
}

// TestCRsByRepo_stateFilterAndOrdering covers the list view: state filtering
// across the three stored states plus all, newest-number-first ordering, and
// closes lists carried in the list view (the SPA's closes chips).
func TestCRsByRepo_stateFilterAndOrdering(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		for i := 1; i <= 4; i++ {
			var closes []int
			if i == 1 {
				closes = []int{5, 2}
			}
			if _, err := s.CreateCR(ctx, repo.ID, fmt.Sprintf("t%d", i), "", fmt.Sprintf("afk/%d", i), "main", closes, now); err != nil {
				t.Fatalf("CreateCR #%d: %v", i, err)
			}
		}
		if _, err := s.MergeCR(ctx, repo.ID, 2, "deadbeef", now.Add(time.Minute)); err != nil {
			t.Fatalf("MergeCR #2: %v", err)
		}
		if _, err := s.CloseCR(ctx, repo.ID, 3, now.Add(time.Minute)); err != nil {
			t.Fatalf("CloseCR #3: %v", err)
		}

		// all → [4,3,2,1] newest-number-first, closes carried.
		all, err := s.CRsByRepo(ctx, repo.ID, CRStateAll)
		if err != nil {
			t.Fatalf("CRsByRepo all: %v", err)
		}
		if got := crNumbersOf(all); !equalInts(got, []int{4, 3, 2, 1}) {
			t.Errorf("all numbers = %v, want [4 3 2 1]", got)
		}
		if got := all[3].Closes; !equalInts(got, []int{2, 5}) {
			t.Errorf("CR #1 closes in list = %v, want [2 5]", got)
		}
		if got := all[0].Closes; got == nil || len(got) != 0 {
			t.Errorf("CR #4 closes in list = %v, want empty non-nil", got)
		}

		// open → [4,1].
		open, err := s.CRsByRepo(ctx, repo.ID, CRStateOpen)
		if err != nil {
			t.Fatalf("CRsByRepo open: %v", err)
		}
		if got := crNumbersOf(open); !equalInts(got, []int{4, 1}) {
			t.Errorf("open numbers = %v, want [4 1]", got)
		}

		// merged → [2] with merged_at + merge_commit set.
		merged, err := s.CRsByRepo(ctx, repo.ID, CRStateMerged)
		if err != nil {
			t.Fatalf("CRsByRepo merged: %v", err)
		}
		if got := crNumbersOf(merged); !equalInts(got, []int{2}) {
			t.Errorf("merged numbers = %v, want [2]", got)
		}
		if merged[0].MergedAt == nil || merged[0].MergeCommit == nil || *merged[0].MergeCommit != "deadbeef" {
			t.Errorf("merged CR = merged_at %v merge_commit %v, want set/deadbeef",
				merged[0].MergedAt, merged[0].MergeCommit)
		}

		// closed → [3], closed-unmerged: no merge metadata.
		closed, err := s.CRsByRepo(ctx, repo.ID, CRStateClosed)
		if err != nil {
			t.Fatalf("CRsByRepo closed: %v", err)
		}
		if got := crNumbersOf(closed); !equalInts(got, []int{3}) {
			t.Errorf("closed numbers = %v, want [3]", got)
		}
		if closed[0].MergedAt != nil || closed[0].MergeCommit != nil {
			t.Errorf("closed CR carries merge metadata: %+v", closed[0])
		}

		// Unknown filter is an error.
		if _, err := s.CRsByRepo(ctx, repo.ID, "bogus"); err == nil {
			t.Error("CRsByRepo(bogus) err = nil, want error")
		}
	})
}

// TestMergeCR_transitionGuards pins the open→merged transition: merged_at,
// merge_commit and updated_at stamped; any non-open start state (merged or
// closed) is refused with ErrCRNotOpen; a missing CR is ErrNotFound.
func TestMergeCR_transitionGuards(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		created := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
		mergedAt := created.Add(10 * time.Minute)

		cr, err := s.CreateCR(ctx, repo.ID, "m", "", "afk/1", "main", []int{1}, created)
		if err != nil {
			t.Fatalf("CreateCR: %v", err)
		}
		got, err := s.MergeCR(ctx, repo.ID, cr.Number, "cafe0123", mergedAt)
		if err != nil {
			t.Fatalf("MergeCR: %v", err)
		}
		if got.State != CRStateMerged {
			t.Errorf("state = %q, want merged", got.State)
		}
		if got.MergedAt == nil || !got.MergedAt.Equal(storedTime(mergedAt)) {
			t.Errorf("merged_at = %v, want %v", got.MergedAt, storedTime(mergedAt))
		}
		if got.MergeCommit == nil || *got.MergeCommit != "cafe0123" {
			t.Errorf("merge_commit = %v, want cafe0123", got.MergeCommit)
		}
		if !got.UpdatedAt.Equal(storedTime(mergedAt)) || !got.CreatedAt.Equal(storedTime(created)) {
			t.Errorf("timestamps = created %v updated %v", got.CreatedAt, got.UpdatedAt)
		}
		if !equalInts(got.Closes, []int{1}) {
			t.Errorf("closes = %v, want [1]", got.Closes)
		}

		// Merged is not open: a second merge is refused.
		if _, err := s.MergeCR(ctx, repo.ID, cr.Number, "beef4567", mergedAt); !errors.Is(err, ErrCRNotOpen) {
			t.Errorf("re-merge err = %v, want ErrCRNotOpen", err)
		}
		// … and so is a close of a merged CR.
		if _, err := s.CloseCR(ctx, repo.ID, cr.Number, mergedAt); !errors.Is(err, ErrCRNotOpen) {
			t.Errorf("close merged err = %v, want ErrCRNotOpen", err)
		}
		// The refused transitions left the row untouched.
		after, err := s.CRByRepoNumber(ctx, repo.ID, cr.Number)
		if err != nil {
			t.Fatalf("CRByRepoNumber: %v", err)
		}
		if after.State != CRStateMerged || *after.MergeCommit != "cafe0123" {
			t.Errorf("after refused transitions = state %q commit %v, want merged/cafe0123",
				after.State, after.MergeCommit)
		}

		if _, err := s.MergeCR(ctx, repo.ID, 999, "c", mergedAt); !errors.Is(err, ErrNotFound) {
			t.Errorf("MergeCR(999) err = %v, want ErrNotFound", err)
		}
	})
}

// TestCloseCR_transitionGuards pins the open→closed transition: state and
// updated_at change, merge metadata stays NULL (closed-unmerged), and a
// closed CR refuses both close and merge with ErrCRNotOpen.
func TestCloseCR_transitionGuards(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		created := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
		closedAt := created.Add(5 * time.Minute)

		cr, err := s.CreateCR(ctx, repo.ID, "c", "", "afk/1", "main", nil, created)
		if err != nil {
			t.Fatalf("CreateCR: %v", err)
		}
		got, err := s.CloseCR(ctx, repo.ID, cr.Number, closedAt)
		if err != nil {
			t.Fatalf("CloseCR: %v", err)
		}
		if got.State != CRStateClosed || got.MergedAt != nil || got.MergeCommit != nil {
			t.Errorf("closed CR = state %q merged_at %v merge_commit %v, want closed/nil/nil",
				got.State, got.MergedAt, got.MergeCommit)
		}
		if !got.UpdatedAt.Equal(storedTime(closedAt)) {
			t.Errorf("updated_at = %v, want %v", got.UpdatedAt, storedTime(closedAt))
		}

		// Closed is not open: neither a re-close nor a merge is allowed.
		if _, err := s.CloseCR(ctx, repo.ID, cr.Number, closedAt); !errors.Is(err, ErrCRNotOpen) {
			t.Errorf("re-close err = %v, want ErrCRNotOpen", err)
		}
		if _, err := s.MergeCR(ctx, repo.ID, cr.Number, "c0ffee", closedAt); !errors.Is(err, ErrCRNotOpen) {
			t.Errorf("merge closed err = %v, want ErrCRNotOpen", err)
		}

		if _, err := s.CloseCR(ctx, repo.ID, 999, closedAt); !errors.Is(err, ErrNotFound) {
			t.Errorf("CloseCR(999) err = %v, want ErrNotFound", err)
		}
	})
}

// TestCRDeleteCascade asserts the repo-delete cascade covers CRs and their
// closes rows (FK ON DELETE CASCADE through change_requests to cr_closes) —
// the design §3a foreign_keys(1) guarantee extended to the M6 tables.
func TestCRDeleteCascade(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		cr, err := s.CreateCR(ctx, repo.ID, "doomed", "", "afk/1", "main", []int{1, 2}, now)
		if err != nil {
			t.Fatalf("CreateCR: %v", err)
		}
		if err := s.DeleteRepo(ctx, repo.ID); err != nil {
			t.Fatalf("DeleteRepo: %v", err)
		}

		var crRows, closesRows int
		if err := s.db.QueryRowContext(ctx, s.rebind(
			`SELECT COUNT(*) FROM change_requests WHERE repo_id = ?`), repo.ID).Scan(&crRows); err != nil {
			t.Fatalf("count change_requests: %v", err)
		}
		if err := s.db.QueryRowContext(ctx, s.rebind(
			`SELECT COUNT(*) FROM cr_closes WHERE cr_id = ?`), cr.ID).Scan(&closesRows); err != nil {
			t.Fatalf("count cr_closes: %v", err)
		}
		if crRows != 0 || closesRows != 0 {
			t.Errorf("after repo delete: %d change_requests, %d cr_closes rows, want 0/0", crRows, closesRows)
		}
	})
}

func crNumbersOf(crs []CR) []int {
	out := make([]int, 0, len(crs))
	for _, cr := range crs {
		out = append(out, cr.Number)
	}
	return out
}

// Regression (M6 review): OpenCRByHead finds exactly the open CR on a head —
// the builtin CreatePull duplicate guard's oracle. Merged/closed CRs on the
// same head never match; no match is ErrNotFound.
func TestOpenCRByHead(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		if _, err := s.OpenCRByHead(ctx, repo.ID, "afk/9"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("no CR: err = %v, want ErrNotFound", err)
		}
		first, err := s.CreateCR(ctx, repo.ID, "a", "", "afk/9", "main", nil, now)
		if err != nil {
			t.Fatalf("CreateCR: %v", err)
		}
		got, err := s.OpenCRByHead(ctx, repo.ID, "afk/9")
		if err != nil || got.Number != first.Number {
			t.Fatalf("OpenCRByHead = (%+v, %v), want #%d", got, err, first.Number)
		}
		if _, err := s.CloseCR(ctx, repo.ID, first.Number, now); err != nil {
			t.Fatalf("CloseCR: %v", err)
		}
		if _, err := s.OpenCRByHead(ctx, repo.ID, "afk/9"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("closed CR still matches: err = %v, want ErrNotFound", err)
		}
	})
}
