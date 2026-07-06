package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestCreateIssue_perRepoNumberSequence pins the per-repo issue-number counter
// (design §2): numbers start at 1 and advance independently per repo, allocated
// from next_issue_number in the create transaction.
func TestCreateIssue_perRepoNumberSequence(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repoA := seedRepoForRuns(t, s)
		repoB := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		for _, want := range []int{1, 2, 3} {
			is, err := s.CreateIssue(ctx, repoA.ID, fmt.Sprintf("A-%d", want), "body A", now)
			if err != nil {
				t.Fatalf("CreateIssue A: %v", err)
			}
			if is.Number != want {
				t.Errorf("repo A issue number = %d, want %d", is.Number, want)
			}
			if is.State != IssueStateOpen || is.ClosedAt != nil {
				t.Errorf("new issue = state %q closed_at %v, want open/nil", is.State, is.ClosedAt)
			}
			if !is.CreatedAt.Equal(storedTime(now)) || !is.UpdatedAt.Equal(storedTime(now)) {
				t.Errorf("timestamps = %v/%v, want %v", is.CreatedAt, is.UpdatedAt, storedTime(now))
			}
			if is.Labels == nil || len(is.Labels) != 0 {
				t.Errorf("new issue labels = %v, want empty non-nil", is.Labels)
			}
		}
		// Repo B's counter is independent.
		for _, want := range []int{1, 2} {
			is, err := s.CreateIssue(ctx, repoB.ID, fmt.Sprintf("B-%d", want), "", now)
			if err != nil {
				t.Fatalf("CreateIssue B: %v", err)
			}
			if is.Number != want {
				t.Errorf("repo B issue number = %d, want %d", is.Number, want)
			}
		}

		if _, err := s.CreateIssue(ctx, "repo_00000000000000000000000000000000", "x", "", now); !errors.Is(err, ErrNotFound) {
			t.Errorf("CreateIssue on missing repo err = %v, want ErrNotFound", err)
		}
	})
}

// TestIssuesByRepo_stateFilterOrderingAndCounts covers the list view: state
// filtering, newest-number-first ordering, comment counts, and label names.
func TestIssuesByRepo_stateFilterOrderingAndCounts(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		var created []Issue
		for i := 0; i < 3; i++ {
			is, err := s.CreateIssue(ctx, repo.ID, fmt.Sprintf("t%d", i+1), "", now)
			if err != nil {
				t.Fatalf("CreateIssue: %v", err)
			}
			created = append(created, is)
		}
		// Close #2.
		if _, err := s.UpdateIssue(ctx, repo.ID, 2, IssueUpdate{State: Set(IssueStateClosed)}, now); err != nil {
			t.Fatalf("close #2: %v", err)
		}
		// Two comments on #1.
		for i := 0; i < 2; i++ {
			if _, err := s.CreateIssueComment(ctx, created[0].ID, CommentAuthorOperator, nil, "c", now.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatalf("CreateIssueComment: %v", err)
			}
		}
		// Label ready-for-agent on #3.
		labels, err := s.LabelsByRepo(ctx, repo.ID)
		if err != nil {
			t.Fatalf("LabelsByRepo: %v", err)
		}
		if err := s.AddIssueLabel(ctx, created[2].ID, labelID(t, labels, "ready-for-agent"), now); err != nil {
			t.Fatalf("AddIssueLabel: %v", err)
		}

		// open → [3,1] newest-number-first.
		open, err := s.IssuesByRepo(ctx, repo.ID, IssueStateOpen)
		if err != nil {
			t.Fatalf("IssuesByRepo open: %v", err)
		}
		if gotNums := numbersOf(open); !equalInts(gotNums, []int{3, 1}) {
			t.Errorf("open numbers = %v, want [3 1]", gotNums)
		}
		if open[1].CommentCount != 2 {
			t.Errorf("issue #1 comment count = %d, want 2", open[1].CommentCount)
		}
		if open[1].Comments != nil {
			t.Errorf("list view loaded comments: %v, want nil", open[1].Comments)
		}
		if !equalStrings(open[0].Labels, []string{"ready-for-agent"}) {
			t.Errorf("issue #3 labels = %v, want [ready-for-agent]", open[0].Labels)
		}
		if !equalStrings(open[1].Labels, []string{}) {
			t.Errorf("issue #1 labels = %v, want empty", open[1].Labels)
		}

		// closed → [2].
		closed, err := s.IssuesByRepo(ctx, repo.ID, IssueStateClosed)
		if err != nil {
			t.Fatalf("IssuesByRepo closed: %v", err)
		}
		if gotNums := numbersOf(closed); !equalInts(gotNums, []int{2}) {
			t.Errorf("closed numbers = %v, want [2]", gotNums)
		}

		// all → [3,2,1].
		all, err := s.IssuesByRepo(ctx, repo.ID, IssueStateAll)
		if err != nil {
			t.Fatalf("IssuesByRepo all: %v", err)
		}
		if gotNums := numbersOf(all); !equalInts(gotNums, []int{3, 2, 1}) {
			t.Errorf("all numbers = %v, want [3 2 1]", gotNums)
		}

		if _, err := s.IssuesByRepo(ctx, repo.ID, "bogus"); err == nil {
			t.Error("IssuesByRepo accepted an invalid state filter")
		}
	})
}

// TestIssueByRepoNumber_detailWithCommentsAndLabels covers the detail read.
func TestIssueByRepoNumber_detailWithCommentsAndLabels(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "detail", "the body", now)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if _, err := s.CreateIssueComment(ctx, is.ID, CommentAuthorOperator, nil, "first", now); err != nil {
			t.Fatalf("comment: %v", err)
		}
		labels, _ := s.LabelsByRepo(ctx, repo.ID)
		if err := s.SetIssueLabels(ctx, is.ID, []string{labelID(t, labels, "needs-triage")}, now); err != nil {
			t.Fatalf("SetIssueLabels: %v", err)
		}

		got, err := s.IssueByRepoNumber(ctx, repo.ID, is.Number)
		if err != nil {
			t.Fatalf("IssueByRepoNumber: %v", err)
		}
		if got.Title != "detail" || got.Body != "the body" {
			t.Errorf("detail = %+v", got)
		}
		if len(got.Comments) != 1 || got.Comments[0].Body != "first" {
			t.Errorf("detail comments = %+v, want one 'first'", got.Comments)
		}
		if got.CommentCount != 1 {
			t.Errorf("detail comment count = %d, want 1", got.CommentCount)
		}
		if !equalStrings(got.Labels, []string{"needs-triage"}) {
			t.Errorf("detail labels = %v, want [needs-triage]", got.Labels)
		}

		if _, err := s.IssueByRepoNumber(ctx, repo.ID, 999); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown number err = %v, want ErrNotFound", err)
		}
	})
}

// TestUpdateIssue_stateTransitionsAndBump pins closed_at set/clear and the
// updated_at bump, plus the empty-update no-op and missing/invalid cases.
func TestUpdateIssue_stateTransitionsAndBump(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "orig", "orig body", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}

		// Title + body edit bumps updated_at, leaves state/closed_at.
		t1 := t0.Add(time.Hour)
		up, err := s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{Title: Set("new"), Body: Set("new body")}, t1)
		if err != nil {
			t.Fatalf("UpdateIssue edit: %v", err)
		}
		if up.Title != "new" || up.Body != "new body" {
			t.Errorf("edit = %+v", up)
		}
		if !up.UpdatedAt.Equal(storedTime(t1)) {
			t.Errorf("updated_at = %v, want %v", up.UpdatedAt, storedTime(t1))
		}
		if up.State != IssueStateOpen || up.ClosedAt != nil {
			t.Errorf("edit changed state/closed_at: %q/%v", up.State, up.ClosedAt)
		}

		// Close stamps closed_at.
		t2 := t1.Add(time.Hour)
		up, err = s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{State: Set(IssueStateClosed)}, t2)
		if err != nil {
			t.Fatalf("UpdateIssue close: %v", err)
		}
		if up.State != IssueStateClosed || up.ClosedAt == nil || !up.ClosedAt.Equal(storedTime(t2)) {
			t.Errorf("close = state %q closed_at %v, want closed/%v", up.State, up.ClosedAt, storedTime(t2))
		}

		// Reopen clears closed_at.
		t3 := t2.Add(time.Hour)
		up, err = s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{State: Set(IssueStateOpen)}, t3)
		if err != nil {
			t.Fatalf("UpdateIssue reopen: %v", err)
		}
		if up.State != IssueStateOpen || up.ClosedAt != nil {
			t.Errorf("reopen = state %q closed_at %v, want open/nil", up.State, up.ClosedAt)
		}

		// Empty update is a no-op that leaves updated_at (still t3).
		t4 := t3.Add(time.Hour)
		up, err = s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{}, t4)
		if err != nil {
			t.Fatalf("UpdateIssue empty: %v", err)
		}
		if !up.UpdatedAt.Equal(storedTime(t3)) {
			t.Errorf("empty update bumped updated_at to %v, want %v", up.UpdatedAt, storedTime(t3))
		}

		// Missing issue → ErrNotFound.
		if _, err := s.UpdateIssue(ctx, repo.ID, 999, IssueUpdate{Title: Set("x")}, t4); !errors.Is(err, ErrNotFound) {
			t.Errorf("UpdateIssue missing err = %v, want ErrNotFound", err)
		}
		// Invalid state → a plain error (not ErrNotFound).
		if _, err := s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{State: Set("weird")}, t4); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("UpdateIssue invalid state err = %v, want a validation error", err)
		}
	})
}

// TestUpdateIssue_redundantClosePreservesClosedAt pins that closed_at marks
// the open→closed TRANSITION: a redundant close (stale client, retry) must
// not move the stamp, and a reopen+close cycle stamps fresh.
func TestUpdateIssue_redundantClosePreservesClosedAt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		t0 := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "sticky close", "", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}

		t1 := t0.Add(time.Hour)
		up, err := s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{State: Set(IssueStateClosed)}, t1)
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if up.ClosedAt == nil || !up.ClosedAt.Equal(storedTime(t1)) {
			t.Fatalf("closed_at = %v, want %v", up.ClosedAt, storedTime(t1))
		}

		// Redundant close an hour later: state stays closed, stamp stays t1.
		t2 := t1.Add(time.Hour)
		up, err = s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{State: Set(IssueStateClosed)}, t2)
		if err != nil {
			t.Fatalf("redundant close: %v", err)
		}
		if up.State != IssueStateClosed || up.ClosedAt == nil || !up.ClosedAt.Equal(storedTime(t1)) {
			t.Errorf("redundant close moved closed_at to %v, want it kept at %v", up.ClosedAt, storedTime(t1))
		}

		// Reopen clears; a later close stamps fresh (COALESCE sees NULL again).
		t3 := t2.Add(time.Hour)
		if _, err := s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{State: Set(IssueStateOpen)}, t3); err != nil {
			t.Fatalf("reopen: %v", err)
		}
		t4 := t3.Add(time.Hour)
		up, err = s.UpdateIssue(ctx, repo.ID, is.Number, IssueUpdate{State: Set(IssueStateClosed)}, t4)
		if err != nil {
			t.Fatalf("re-close: %v", err)
		}
		if up.ClosedAt == nil || !up.ClosedAt.Equal(storedTime(t4)) {
			t.Errorf("re-close closed_at = %v, want fresh stamp %v", up.ClosedAt, storedTime(t4))
		}
	})
}

// TestCreateIssueWithLabels_atomicity pins that create + label attach commit
// or roll back as ONE transaction: a missing label leaves no issue behind and
// releases no issue number, and the happy path returns the sorted names.
func TestCreateIssueWithLabels_atomicity(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
		seed, err := s.LabelsByRepo(ctx, repo.ID)
		if err != nil {
			t.Fatalf("LabelsByRepo: %v", err)
		}

		// A vanished label rolls the whole create back.
		_, err = s.CreateIssueWithLabels(ctx, repo.ID, "doomed", "", []string{
			labelID(t, seed, "needs-triage"), "lbl_00000000000000000000000000000000",
		}, CommentAuthorOperator, nil, now)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("CreateIssueWithLabels missing label err = %v, want ErrNotFound", err)
		}
		if issues, err := s.IssuesByRepo(ctx, repo.ID, IssueStateAll); err != nil || len(issues) != 0 {
			t.Fatalf("issues after rolled-back create = %v (err %v), want none", issues, err)
		}

		// The number allocation rolled back too: the next create gets #1, and
		// its labels come back attached and sorted.
		is, err := s.CreateIssueWithLabels(ctx, repo.ID, "labelled", "", []string{
			labelID(t, seed, "ready-for-agent"), labelID(t, seed, "needs-triage"),
		}, CommentAuthorOperator, nil, now)
		if err != nil {
			t.Fatalf("CreateIssueWithLabels: %v", err)
		}
		if is.Number != 1 {
			t.Errorf("number after rollback = %d, want 1 (allocation must roll back)", is.Number)
		}
		if !equalStrings(is.Labels, []string{"needs-triage", "ready-for-agent"}) {
			t.Errorf("labels = %v, want sorted [needs-triage ready-for-agent]", is.Labels)
		}
		got, err := s.IssueByRepoNumber(ctx, repo.ID, is.Number)
		if err != nil {
			t.Fatalf("IssueByRepoNumber: %v", err)
		}
		if !equalStrings(got.Labels, []string{"needs-triage", "ready-for-agent"}) {
			t.Errorf("stored labels = %v", got.Labels)
		}

		// Missing repo still maps to ErrNotFound.
		if _, err := s.CreateIssueWithLabels(ctx, "repo_00000000000000000000000000000000", "x", "", nil,
			CommentAuthorOperator, nil, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing repo err = %v, want ErrNotFound", err)
		}
	})
}

// TestCreateIssue_authorAttribution pins the 0002 author identity on issues:
// CreateIssue defaults to the operator, a run-authored create stores and reads
// back the run identity, and the run/author-kind guard matches the comment
// rule — a contradiction is rejected with nothing persisted.
func TestCreateIssue_authorAttribution(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		op, err := s.CreateIssue(ctx, repo.ID, "by operator", "", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if op.AuthorKind != CommentAuthorOperator || op.RunID != nil {
			t.Errorf("operator issue author = (%q, %v), want (operator, nil)", op.AuthorKind, op.RunID)
		}

		run, err := s.CreateRun(ctx, manualRun(repo.ID, "proj~att", "lab/att", t0))
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		runID := run.ID
		created, err := s.CreateIssueWithLabels(ctx, repo.ID, "by run", "found mid-run", nil,
			CommentAuthorRun, &runID, t0)
		if err != nil {
			t.Fatalf("CreateIssueWithLabels as run: %v", err)
		}
		if created.AuthorKind != CommentAuthorRun || created.RunID == nil || *created.RunID != runID {
			t.Errorf("run issue author = (%q, %v), want (run, %q)", created.AuthorKind, created.RunID, runID)
		}
		got, err := s.IssueByRepoNumber(ctx, repo.ID, created.Number)
		if err != nil {
			t.Fatalf("IssueByRepoNumber: %v", err)
		}
		if got.AuthorKind != CommentAuthorRun || got.RunID == nil || *got.RunID != runID {
			t.Errorf("stored run issue author = (%q, %v), want (run, %q)", got.AuthorKind, got.RunID, runID)
		}

		// The comment guard applies verbatim: contradictory identities are
		// rejected and allocate no issue number.
		if _, err := s.CreateIssueWithLabels(ctx, repo.ID, "x", "", nil, CommentAuthorOperator, &runID, t0); err == nil {
			t.Error("CreateIssueWithLabels accepted operator author with run_id")
		}
		if _, err := s.CreateIssueWithLabels(ctx, repo.ID, "x", "", nil, CommentAuthorRun, nil, t0); err == nil {
			t.Error("CreateIssueWithLabels accepted run author without run_id")
		}
		next, err := s.CreateIssue(ctx, repo.ID, "sequence intact", "", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		if next.Number != created.Number+1 {
			t.Errorf("number after rejected creates = %d, want %d", next.Number, created.Number+1)
		}
	})
}

// TestCreateIssueComment_bumpsIssueUpdatedAt pins the contract that a comment
// is an issue mutation: the issue's updated_at advances with the comment.
func TestCreateIssueComment_bumpsIssueUpdatedAt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "bumpy", "", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		t1 := t0.Add(time.Hour)
		if _, err := s.CreateIssueComment(ctx, is.ID, CommentAuthorOperator, nil, "ping", t1); err != nil {
			t.Fatalf("CreateIssueComment: %v", err)
		}
		got, err := s.IssueByRepoNumber(ctx, repo.ID, is.Number)
		if err != nil {
			t.Fatalf("IssueByRepoNumber: %v", err)
		}
		if !got.UpdatedAt.Equal(storedTime(t1)) {
			t.Errorf("updated_at after comment = %v, want %v", got.UpdatedAt, storedTime(t1))
		}
	})
}

// TestCreateIssueComment_runIDAuthorKindGuard pins the run_id invariant: only
// run-authored comments carry a run_id, and they must carry one.
func TestCreateIssueComment_runIDAuthorKindGuard(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "guarded", "", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		run, err := s.CreateRun(ctx, manualRun(repo.ID, "proj~grd", "lab/grd", t0))
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		runID := run.ID

		// Operator with a run_id → rejected, nothing persisted.
		if _, err := s.CreateIssueComment(ctx, is.ID, CommentAuthorOperator, &runID, "contradiction", t0); err == nil {
			t.Error("CreateIssueComment accepted operator author with run_id")
		}
		// Run author without a run_id → rejected.
		if _, err := s.CreateIssueComment(ctx, is.ID, CommentAuthorRun, nil, "anonymous run", t0); err == nil {
			t.Error("CreateIssueComment accepted run author without run_id")
		}
		comments, err := s.CommentsByIssue(ctx, is.ID)
		if err != nil {
			t.Fatalf("CommentsByIssue: %v", err)
		}
		if len(comments) != 0 {
			t.Errorf("comments after rejected writes = %+v, want none", comments)
		}
	})
}

// TestCommentsByIssue_sameTimestampDeterministicOrder pins the tie order for
// comments sharing one stored millisecond: deterministic across reads (by id),
// even though it is arbitrary with respect to insertion order (documented on
// CommentsByIssue; a monotonic tiebreak needs a schema change, deferred).
func TestCommentsByIssue_sameTimestampDeterministicOrder(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "ties", "", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		for i := 0; i < 6; i++ {
			if _, err := s.CreateIssueComment(ctx, is.ID, CommentAuthorOperator, nil, fmt.Sprintf("c%d", i), t0); err != nil {
				t.Fatalf("comment %d: %v", i, err)
			}
		}

		first, err := s.CommentsByIssue(ctx, is.ID)
		if err != nil {
			t.Fatalf("CommentsByIssue: %v", err)
		}
		if len(first) != 6 {
			t.Fatalf("comments = %d, want 6", len(first))
		}
		for i := 1; i < len(first); i++ {
			if first[i-1].ID > first[i].ID {
				t.Fatalf("tie order not by id: %q before %q", first[i-1].ID, first[i].ID)
			}
		}
		// Re-reads return the identical order.
		for rep := 0; rep < 3; rep++ {
			again, err := s.CommentsByIssue(ctx, is.ID)
			if err != nil {
				t.Fatalf("CommentsByIssue repeat: %v", err)
			}
			for i := range first {
				if again[i].ID != first[i].ID {
					t.Fatalf("read %d reordered ties: pos %d = %q, want %q", rep, i, again[i].ID, first[i].ID)
				}
			}
		}
	})
}

// TestIssueComments_authorKindsAndOrder pins operator + run authorship, the
// optional run_id FK, chronological order, and the error cases.
func TestIssueComments_authorKindsAndOrder(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := seedRepoForRuns(t, s)
		t0 := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

		is, err := s.CreateIssue(ctx, repo.ID, "thread", "", t0)
		if err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		run, err := s.CreateRun(ctx, manualRun(repo.ID, "proj~cmt", "lab/cmt", t0))
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		op, err := s.CreateIssueComment(ctx, is.ID, CommentAuthorOperator, nil, "operator says", t0.Add(time.Second))
		if err != nil {
			t.Fatalf("operator comment: %v", err)
		}
		if op.AuthorKind != CommentAuthorOperator || op.RunID != nil {
			t.Errorf("operator comment = %+v", op)
		}
		runID := run.ID
		rc, err := s.CreateIssueComment(ctx, is.ID, CommentAuthorRun, &runID, "run says", t0.Add(2*time.Second))
		if err != nil {
			t.Fatalf("run comment: %v", err)
		}
		if rc.AuthorKind != CommentAuthorRun || rc.RunID == nil || *rc.RunID != run.ID {
			t.Errorf("run comment = %+v", rc)
		}

		comments, err := s.CommentsByIssue(ctx, is.ID)
		if err != nil {
			t.Fatalf("CommentsByIssue: %v", err)
		}
		if len(comments) != 2 || comments[0].Body != "operator says" || comments[1].Body != "run says" {
			t.Errorf("comments = %+v, want operator then run", comments)
		}

		// Invalid author kind → validation error, no insert.
		if _, err := s.CreateIssueComment(ctx, is.ID, "robot", nil, "no", t0); err == nil {
			t.Error("CreateIssueComment accepted an invalid author kind")
		}
		// Missing issue → ErrNotFound (FK).
		if _, err := s.CreateIssueComment(ctx, "iss_00000000000000000000000000000000", CommentAuthorOperator, nil, "x", t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("comment on missing issue err = %v, want ErrNotFound", err)
		}
	})
}

// --- small test helpers ---

func labelID(t *testing.T, labels []Label, name string) string {
	t.Helper()
	for _, l := range labels {
		if l.Name == name {
			return l.ID
		}
	}
	t.Fatalf("label %q not found among %v", name, labels)
	return ""
}

func numbersOf(issues []Issue) []int {
	out := make([]int, len(issues))
	for i, is := range issues {
		out[i] = is.Number
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
