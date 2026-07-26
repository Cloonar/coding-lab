package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

var fixedNow = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s, err := store.Open(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "lab.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedRepo(t *testing.T, s *store.Store) store.Repo {
	t.Helper()
	r, err := s.CreateRepo(context.Background(), store.Repo{
		ID: ids.NewID("repo"), Name: "proj-" + ids.NewID("x")[:8], RemoteURL: "/tmp/x",
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: fixedNow,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return r
}

// newTracker builds a built-in tracker for repo with a fixed clock so the
// timestamps it stamps (comments, closed_at) are deterministic.
func newTracker(s *store.Store, repoID string) *Tracker {
	tr := New(tracker.BuiltinConfig{Store: s, RepoID: repoID}).(*Tracker)
	tr.now = func() time.Time { return fixedNow }
	return tr
}

func addReadyLabel(t *testing.T, s *store.Store, repoID, issueID string) {
	t.Helper()
	labels, err := s.LabelsByRepo(context.Background(), repoID)
	if err != nil {
		t.Fatalf("LabelsByRepo: %v", err)
	}
	for _, l := range labels {
		if l.Name == tracker.ReadyLabel {
			if err := s.AddIssueLabel(context.Background(), issueID, l.ID, fixedNow); err != nil {
				t.Fatalf("AddIssueLabel: %v", err)
			}
			return
		}
	}
	t.Fatalf("ready label not seeded")
}

func TestBuiltin_ReadyIssues(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	// #1 ready+open, #2 open no label, #3 ready but closed → only #1 qualifies.
	i1, _ := s.CreateIssue(ctx, repo.ID, "ready one", "b1", fixedNow)
	_, _ = s.CreateIssue(ctx, repo.ID, "plain", "b2", fixedNow)
	i3, _ := s.CreateIssue(ctx, repo.ID, "ready but closed", "b3", fixedNow)
	addReadyLabel(t, s, repo.ID, i1.ID)
	addReadyLabel(t, s, repo.ID, i3.ID)
	if _, err := s.UpdateIssue(ctx, repo.ID, i3.Number, store.IssueUpdate{State: store.Set(store.IssueStateClosed)}, fixedNow); err != nil {
		t.Fatalf("close #3: %v", err)
	}

	ready, err := tr.ReadyIssues(ctx)
	if err != nil {
		t.Fatalf("ReadyIssues: %v", err)
	}
	if len(ready) != 1 || ready[0].Number != i1.Number {
		t.Fatalf("ReadyIssues = %+v, want only #%d", ready, i1.Number)
	}
	if !contains(ready[0].Labels, tracker.ReadyLabel) {
		t.Errorf("ready issue labels = %v, want to include %q", ready[0].Labels, tracker.ReadyLabel)
	}
	if ready[0].Comments != nil {
		t.Errorf("ready issue carried comments %v, want nil (list view)", ready[0].Comments)
	}
}

func TestBuiltin_IssuesByState(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	_, _ = s.CreateIssue(ctx, repo.ID, "one", "", fixedNow)
	_, _ = s.CreateIssue(ctx, repo.ID, "two", "", fixedNow)
	if _, err := s.UpdateIssue(ctx, repo.ID, 2, store.IssueUpdate{State: store.Set(store.IssueStateClosed)}, fixedNow); err != nil {
		t.Fatalf("close #2: %v", err)
	}

	open, err := tr.Issues(ctx, tracker.StateOpen)
	if err != nil {
		t.Fatalf("Issues open: %v", err)
	}
	if len(open) != 1 || open[0].Number != 1 || open[0].State != tracker.StateOpen {
		t.Errorf("open issues = %+v, want only #1 open", open)
	}
	closed, err := tr.Issues(ctx, tracker.StateClosed)
	if err != nil {
		t.Fatalf("Issues closed: %v", err)
	}
	if len(closed) != 1 || closed[0].Number != 2 || closed[0].State != tracker.StateClosed {
		t.Errorf("closed issues = %+v, want only #2 closed (the recent-closed window)", closed)
	}
	// all = open set first, then the recent-closed window (issue #176).
	all, err := tr.Issues(ctx, tracker.StateAll)
	if err != nil {
		t.Fatalf("Issues all: %v", err)
	}
	if len(all) != 2 || all[0].Number != 1 || all[1].Number != 2 {
		t.Errorf("all issues = %+v, want [#1 open, #2 closed] open-first", all)
	}
	for _, is := range all {
		if is.Comments != nil {
			t.Errorf("list view issue #%d carried comments, want nil", is.Number)
		}
	}
	if _, err := tr.Issues(ctx, "bogus"); err == nil {
		t.Error("Issues(bogus) err = nil, want the invalid-filter error")
	}
}

func TestBuiltin_IssueWithComments(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	is, _ := s.CreateIssue(ctx, repo.ID, "threaded", "the body", fixedNow)
	run, err := s.CreateRun(ctx, store.Run{
		ID: ids.NewID("run"), RepoID: repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/x", WorktreePath: "/wt/x", SessionName: "proj~x", Model: "opus[1m]",
		Effort: "max", StartedAt: fixedNow, Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := s.CreateIssueComment(ctx, is.ID, store.CommentAuthorOperator, nil, "op note", fixedNow.Add(time.Second)); err != nil {
		t.Fatalf("operator comment: %v", err)
	}
	runID := run.ID
	if _, err := s.CreateIssueComment(ctx, is.ID, store.CommentAuthorRun, &runID, "run note", fixedNow.Add(2*time.Second)); err != nil {
		t.Fatalf("run comment: %v", err)
	}

	got, err := tr.Issue(ctx, is.Number)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got.Title != "threaded" || got.Body != "the body" {
		t.Errorf("issue = %+v", got)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("comments = %d, want 2", len(got.Comments))
	}
	if got.Comments[0].Author != store.CommentAuthorOperator || got.Comments[0].Body != "op note" {
		t.Errorf("first comment = %+v, want operator/op note", got.Comments[0])
	}
	if got.Comments[1].Author != store.CommentAuthorRun || got.Comments[1].Body != "run note" {
		t.Errorf("second comment = %+v, want run/run note", got.Comments[1])
	}

	if _, err := tr.Issue(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Issue(999) err = %v, want ErrNotFound", err)
	}
}

func TestBuiltin_CreateComment_identity(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	is, _ := s.CreateIssue(ctx, repo.ID, "c", "", fixedNow)

	// Default identity is the operator.
	if err := tr.CreateComment(ctx, is.Number, "hello from operator"); err != nil {
		t.Fatalf("CreateComment operator: %v", err)
	}
	// ForRun switches the author to a run (author_kind=run + run_id).
	run, err := s.CreateRun(ctx, store.Run{
		ID: ids.NewID("run"), RepoID: repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/y", WorktreePath: "/wt/y", SessionName: "proj~y", Model: "opus[1m]",
		Effort: "max", StartedAt: fixedNow, Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runTr := tr.ForRun(run.ID)
	if err := runTr.CreateComment(ctx, is.Number, "hello from run"); err != nil {
		t.Fatalf("CreateComment run: %v", err)
	}
	// The base tracker keeps the operator identity (ForRun returns a copy).
	if err := tr.CreateComment(ctx, is.Number, "operator again"); err != nil {
		t.Fatalf("CreateComment operator again: %v", err)
	}

	comments, err := s.CommentsByIssue(ctx, is.ID)
	if err != nil {
		t.Fatalf("CommentsByIssue: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("comments = %d, want 3", len(comments))
	}
	// Assert by body, not position: all three share the fixed clock, so their
	// created_at ties and the id tiebreak makes list order unspecified.
	byBody := map[string]store.IssueComment{}
	for _, c := range comments {
		byBody[c.Body] = c
	}
	if c := byBody["hello from operator"]; c.AuthorKind != store.CommentAuthorOperator || c.RunID != nil {
		t.Errorf("operator comment = %+v, want operator/nil", c)
	}
	if c := byBody["hello from run"]; c.AuthorKind != store.CommentAuthorRun || c.RunID == nil || *c.RunID != run.ID {
		t.Errorf("run comment = %+v, want run/%s", c, run.ID)
	}
	// ForRun returned a copy: the base tracker still authors as the operator.
	if c := byBody["operator again"]; c.AuthorKind != store.CommentAuthorOperator || c.RunID != nil {
		t.Errorf("second operator comment = %+v, want operator/nil (copy did not mutate base)", c)
	}

	// Comment on a missing issue surfaces ErrNotFound.
	if err := tr.CreateComment(ctx, 999, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("CreateComment on missing issue err = %v, want ErrNotFound", err)
	}
}

func TestBuiltin_CloseIssue(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	is, _ := s.CreateIssue(ctx, repo.ID, "closeme", "", fixedNow)
	if err := tr.CloseIssue(ctx, is.Number); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	got, err := s.IssueByRepoNumber(ctx, repo.ID, is.Number)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if got.State != store.IssueStateClosed || got.ClosedAt == nil {
		t.Errorf("after CloseIssue = state %q closed_at %v, want closed/non-nil", got.State, got.ClosedAt)
	}

	if err := tr.CloseIssue(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("CloseIssue(999) err = %v, want ErrNotFound", err)
	}
}

// TestBuiltin_EditIssue pins the title/body patch riding store.UpdateIssue: a
// nil pointer leaves the field untouched, a non-nil one replaces it, a non-nil
// empty Body clears the body — on any issue, open OR closed — and an unknown
// number surfaces store.ErrNotFound. The returned issue is LIST shape (Comments
// nil, CommentsCount from the store).
func TestBuiltin_EditIssue(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	str := func(v string) *string { return &v }
	is, _ := s.CreateIssue(ctx, repo.ID, "old title", "old body", fixedNow)

	// Title only: body untouched.
	got, err := tr.EditIssue(ctx, is.Number, tracker.IssueEdit{Title: str("new title")})
	if err != nil {
		t.Fatalf("EditIssue title: %v", err)
	}
	if got.Title != "new title" || got.Body != "old body" {
		t.Errorf("after title edit = %q/%q, want new title/old body", got.Title, got.Body)
	}
	if got.Comments != nil {
		t.Errorf("returned issue carries a comment thread = %+v, want LIST shape (nil)", got.Comments)
	}

	// Body only: title untouched.
	got, err = tr.EditIssue(ctx, is.Number, tracker.IssueEdit{Body: str("new body")})
	if err != nil {
		t.Fatalf("EditIssue body: %v", err)
	}
	if got.Title != "new title" || got.Body != "new body" {
		t.Errorf("after body edit = %q/%q, want new title/new body", got.Title, got.Body)
	}

	// Both at once.
	got, err = tr.EditIssue(ctx, is.Number, tracker.IssueEdit{Title: str("t2"), Body: str("b2")})
	if err != nil {
		t.Fatalf("EditIssue both: %v", err)
	}
	if got.Title != "t2" || got.Body != "b2" {
		t.Errorf("after both edit = %q/%q, want t2/b2", got.Title, got.Body)
	}

	// Clear body with a non-nil empty string.
	got, err = tr.EditIssue(ctx, is.Number, tracker.IssueEdit{Body: str("")})
	if err != nil {
		t.Fatalf("EditIssue clear body: %v", err)
	}
	if got.Title != "t2" || got.Body != "" {
		t.Errorf("after clear body = %q/%q, want t2/empty", got.Title, got.Body)
	}

	// Editing a CLOSED issue succeeds — no guard on state.
	if err := tr.CloseIssue(ctx, is.Number); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	got, err = tr.EditIssue(ctx, is.Number, tracker.IssueEdit{Title: str("edited while closed")})
	if err != nil {
		t.Fatalf("EditIssue on closed issue: %v", err)
	}
	if got.Title != "edited while closed" || got.State != tracker.StateClosed {
		t.Errorf("after edit-while-closed = %q/%q, want edited title still closed", got.Title, got.State)
	}

	// Unknown number surfaces store.ErrNotFound.
	if _, err := tr.EditIssue(ctx, 999, tracker.IssueEdit{Title: str("x")}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("EditIssue(999) err = %v, want ErrNotFound", err)
	}
}

// TestBuiltin_Pulls_stateMappingAndPRPresent pins the bounded list view
// (issue #176): Pulls returns the open CRs plus the recent-closed window
// (which covers every closed CR here) mapped onto the tracker's PR
// vocabulary, open set first, and tracker.PRPresent over that result treats
// open|merged as done and closed-unmerged (or absent) as no PR — identical
// to a forge repo. (The reaper's own done-signal reads PullsForHead.)
func TestBuiltin_Pulls_stateMappingAndPRPresent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	// #1 stays open, #2 is merged, #3 is closed-unmerged.
	if _, err := s.CreateCR(ctx, repo.ID, "open one", "", "afk/1", "main", nil, fixedNow); err != nil {
		t.Fatalf("CreateCR #1: %v", err)
	}
	if _, err := s.CreateCR(ctx, repo.ID, "merged one", "", "afk/2", "main", nil, fixedNow); err != nil {
		t.Fatalf("CreateCR #2: %v", err)
	}
	if _, err := s.CreateCR(ctx, repo.ID, "closed one", "", "afk/3", "main", nil, fixedNow); err != nil {
		t.Fatalf("CreateCR #3: %v", err)
	}
	if _, err := s.MergeCR(ctx, repo.ID, 2, "abc1234", fixedNow); err != nil {
		t.Fatalf("MergeCR #2: %v", err)
	}
	if _, err := s.CloseCR(ctx, repo.ID, 3, fixedNow); err != nil {
		t.Fatalf("CloseCR #3: %v", err)
	}

	pulls, err := tr.Pulls(ctx)
	if err != nil {
		t.Fatalf("Pulls: %v", err)
	}
	if len(pulls) != 3 {
		t.Fatalf("Pulls = %d refs, want 3 (open + the recent-closed window covers all of them)", len(pulls))
	}
	// Bounded-view order (issue #176): the open set first, then the
	// recent-closed window newest number first.
	if pulls[0].Number != 1 || pulls[1].Number != 3 || pulls[2].Number != 2 {
		t.Errorf("Pulls order = [#%d #%d #%d], want [#1 open, #3, #2] (open first, then closed newest-first)",
			pulls[0].Number, pulls[1].Number, pulls[2].Number)
	}
	byNumber := map[int]tracker.PullRef{}
	for _, p := range pulls {
		byNumber[p.Number] = p
	}
	wantStates := map[int]string{1: tracker.PullOpen, 2: tracker.PullMerged, 3: tracker.PullClosed}
	for n, wantState := range wantStates {
		p, ok := byNumber[n]
		if !ok {
			t.Fatalf("Pulls missing CR #%d", n)
		}
		if p.State != wantState {
			t.Errorf("CR #%d state = %q, want %q", n, p.State, wantState)
		}
		if wantHead := "afk/" + strconv.Itoa(n); p.HeadBranch != wantHead {
			t.Errorf("CR #%d head = %q, want %q", n, p.HeadBranch, wantHead)
		}
		if wantURL := "/repos/" + repo.ID + "/crs/" + strconv.Itoa(n); p.URL != wantURL {
			t.Errorf("CR #%d url = %q, want %q", n, p.URL, wantURL)
		}
	}

	// PRPresent interplay: open and merged CRs are the done-signal, a
	// closed-unmerged CR (or no CR at all) is not.
	for head, want := range map[string]bool{
		"afk/1": true, "afk/2": true, "afk/3": false, "afk/9": false,
	} {
		if got := tracker.PRPresent(pulls, head); got != want {
			t.Errorf("PRPresent(%q) = %v, want %v", head, got, want)
		}
	}
}

// TestBuiltin_Pulls_windowAgesOutOldClosed pins the REAL window at the
// tracker level (issue #176): with tracker.RecentClosedWindow+1 closed CRs,
// Pulls returns the open set plus exactly the window — the oldest closed CR
// (lowest number ≈ least recently closed in the monotonically numbered
// store) ages out, while every open CR survives regardless of the window.
func TestBuiltin_Pulls_windowAgesOutOldClosed(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	// #1..#(window+1) closed, then one open CR on top.
	for i := 1; i <= tracker.RecentClosedWindow+1; i++ {
		if _, err := s.CreateCR(ctx, repo.ID, fmt.Sprintf("closed %d", i), "", fmt.Sprintf("afk/%d", i), "main", nil, fixedNow); err != nil {
			t.Fatalf("CreateCR #%d: %v", i, err)
		}
		if _, err := s.CloseCR(ctx, repo.ID, i, fixedNow); err != nil {
			t.Fatalf("CloseCR #%d: %v", i, err)
		}
	}
	openCR, err := s.CreateCR(ctx, repo.ID, "still open", "", "afk/open", "main", nil, fixedNow)
	if err != nil {
		t.Fatalf("CreateCR open: %v", err)
	}

	pulls, err := tr.Pulls(ctx)
	if err != nil {
		t.Fatalf("Pulls: %v", err)
	}
	if want := 1 + tracker.RecentClosedWindow; len(pulls) != want {
		t.Fatalf("Pulls = %d refs, want %d (1 open + the %d-row window)", len(pulls), want, tracker.RecentClosedWindow)
	}
	if pulls[0].Number != openCR.Number || pulls[0].State != tracker.PullOpen {
		t.Errorf("pulls[0] = %+v, want the open CR #%d first", pulls[0], openCR.Number)
	}
	// The window is newest-number-first and #1 has aged out.
	for i, p := range pulls[1:] {
		if want := tracker.RecentClosedWindow + 1 - i; p.Number != want {
			t.Fatalf("window[%d] = #%d, want #%d (newest closed first)", i, p.Number, want)
		}
	}
	for _, p := range pulls {
		if p.Number == 1 {
			t.Errorf("closed CR #1 still listed; want it aged out of the %d-row window", tracker.RecentClosedWindow)
		}
	}
}

// TestBuiltin_PullsForHead_doneSignalConformance is the shared done-signal
// table (the same cases run against all three backends): PullsForHead only
// enumerates the head+base candidates, and tracker.DonePull projects the same
// verdict it would from a full Pulls() walk. CRs are seeded at the store
// level (bypassing CreatePull's one-open-per-head guard) so the table can
// carry a same-head+base collision and a same-head different-base CR.
func TestBuiltin_PullsForHead_doneSignalConformance(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	// #1 open afk/1→main; #2 merged afk/2→main; #3 closed-unmerged afk/3→main;
	// #4 open afk/1→dev (same head, other base); #5 closed + #6 open afk/5→main
	// (head-reuse collision).
	seedCR := func(title, head, base string) store.CR {
		t.Helper()
		cr, err := s.CreateCR(ctx, repo.ID, title, "", head, base, nil, fixedNow)
		if err != nil {
			t.Fatalf("CreateCR %s: %v", title, err)
		}
		return cr
	}
	seedCR("open one", "afk/1", "main")
	seedCR("merged one", "afk/2", "main")
	seedCR("closed one", "afk/3", "main")
	seedCR("other base", "afk/1", "dev")
	seedCR("closed then reused", "afk/5", "main")
	if _, err := s.MergeCR(ctx, repo.ID, 2, "abc1234", fixedNow); err != nil {
		t.Fatalf("MergeCR #2: %v", err)
	}
	if _, err := s.CloseCR(ctx, repo.ID, 3, fixedNow); err != nil {
		t.Fatalf("CloseCR #3: %v", err)
	}
	if _, err := s.CloseCR(ctx, repo.ID, 5, fixedNow); err != nil {
		t.Fatalf("CloseCR #5: %v", err)
	}
	seedCR("reopened on reused head", "afk/5", "main") // #6, open

	for _, tc := range []struct {
		name        string
		head, base  string
		wantNumbers map[int]bool
		wantDone    bool
		wantDoneNum int
	}{
		{"open CR is the done-signal", "afk/1", "main", map[int]bool{1: true}, true, 1},
		{"merged CR is the done-signal", "afk/2", "main", map[int]bool{2: true}, true, 2},
		{"closed-unmerged only is no done-signal", "afk/3", "main", map[int]bool{3: true}, false, 0},
		{"no CR at all is empty success", "afk/9", "main", map[int]bool{}, false, 0},
		{"CR onto a different base does not match", "afk/1", "dev", map[int]bool{4: true}, true, 4},
		{"head-reuse collision returns both, open wins", "afk/5", "main", map[int]bool{5: true, 6: true}, true, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs, err := tr.PullsForHead(ctx, tc.head, tc.base)
			if err != nil {
				t.Fatalf("PullsForHead: %v", err)
			}
			if refs == nil {
				t.Fatalf("PullsForHead returned nil; want a non-nil slice (empty result is success)")
			}
			if len(refs) != len(tc.wantNumbers) {
				t.Fatalf("got %d refs (%+v); want %d", len(refs), refs, len(tc.wantNumbers))
			}
			for _, r := range refs {
				if !tc.wantNumbers[r.Number] {
					t.Errorf("unexpected ref #%d in %+v", r.Number, refs)
				}
				if r.HeadBranch != tc.head {
					t.Errorf("ref #%d HeadBranch = %q; want the queried %q", r.Number, r.HeadBranch, tc.head)
				}
			}
			done, ok := tracker.DonePull(refs, tc.head)
			if ok != tc.wantDone {
				t.Fatalf("DonePull ok = %v; want %v", ok, tc.wantDone)
			}
			if ok && done.Number != tc.wantDoneNum {
				t.Errorf("DonePull = #%d; want #%d", done.Number, tc.wantDoneNum)
			}
		})
	}
}

// TestBuiltin_Pull pins the single-CR detail read (labctl pr view): the full
// title/body come back with the shared state vocabulary and the CR's
// lab-relative URL; an unknown number surfaces store.ErrNotFound.
func TestBuiltin_Pull(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	body := "card: |\n  kind: capture\n  target: nixos\n\nCloses #1"
	if _, err := tr.CreatePull(ctx, "afk/1", "main", "feat: capture card", body); err != nil {
		t.Fatalf("CreatePull: %v", err)
	}
	if _, err := s.MergeCR(ctx, repo.ID, 1, "abc1234", fixedNow); err != nil {
		t.Fatalf("MergeCR: %v", err)
	}

	pd, err := tr.Pull(ctx, 1)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	want := tracker.PullDetail{
		Number:     1,
		Title:      "feat: capture card",
		Body:       body,
		State:      tracker.PullMerged,
		HeadBranch: "afk/1",
		URL:        "/repos/" + repo.ID + "/crs/1",
	}
	if pd != want {
		t.Errorf("PullDetail = %+v, want %+v", pd, want)
	}

	if _, err := tr.Pull(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Pull(999) err = %v, want ErrNotFound", err)
	}
}

// TestBuiltin_Checks pins the built-in tracker's CI answer: the built-in
// tracker has no CI machinery, so a known CR's checks come back as a non-nil
// empty slice (ChecksState reads that as tracker.ChecksNone, not an error),
// and an unknown number surfaces store.ErrNotFound exactly like Pull.
func TestBuiltin_Checks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	if _, err := tr.CreatePull(ctx, "afk/1", "main", "feat: capture card", "Closes #1"); err != nil {
		t.Fatalf("CreatePull: %v", err)
	}

	checks, err := tr.Checks(ctx, 1)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if checks == nil || len(checks) != 0 {
		t.Errorf("Checks = %#v, want a non-nil empty slice", checks)
	}
	if got := tracker.ChecksState(checks); got != tracker.ChecksNone {
		t.Errorf("ChecksState(Checks(1)) = %q, want %q", got, tracker.ChecksNone)
	}

	if _, err := tr.Checks(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Checks(999) err = %v, want ErrNotFound", err)
	}
}

// TestBuiltin_CreatePull pins the builtin PR create (M6): a CR is persisted
// with head/base/title/body, its closes parsed from the body with the shared
// grammar, and the returned PullRef is the open CR.
func TestBuiltin_CreatePull(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	body := "Does the thing.\n\nFixes #2\nCloses #1\ncloses #2 (dup) — but discloses #9 is not a directive."
	pr, err := tr.CreatePull(ctx, "afk/1", "main", "do the thing", body)
	if err != nil {
		t.Fatalf("CreatePull: %v", err)
	}
	if pr.Number != 1 || pr.HeadBranch != "afk/1" || pr.State != tracker.PullOpen {
		t.Errorf("PullRef = %+v, want #1 afk/1 open", pr)
	}
	if wantURL := "/repos/" + repo.ID + "/crs/1"; pr.URL != wantURL {
		t.Errorf("PullRef url = %q, want %q", pr.URL, wantURL)
	}

	cr, err := s.CRByRepoNumber(ctx, repo.ID, pr.Number)
	if err != nil {
		t.Fatalf("CRByRepoNumber: %v", err)
	}
	if cr.Title != "do the thing" || cr.Body != body || cr.HeadBranch != "afk/1" || cr.BaseBranch != "main" {
		t.Errorf("stored CR = %+v", cr)
	}
	if cr.State != store.CRStateOpen {
		t.Errorf("stored CR state = %q, want open", cr.State)
	}
	// Closes parsed from the body: deduplicated, sorted, boundary-aware
	// ("discloses #9" is no directive).
	if len(cr.Closes) != 2 || cr.Closes[0] != 1 || cr.Closes[1] != 2 {
		t.Errorf("stored CR closes = %v, want [1 2]", cr.Closes)
	}

	// The new CR is immediately the run's done-signal.
	pulls, err := tr.Pulls(ctx)
	if err != nil {
		t.Fatalf("Pulls: %v", err)
	}
	if !tracker.PRPresent(pulls, "afk/1") {
		t.Errorf("PRPresent(afk/1) = false after CreatePull, want true")
	}
}

// TestBuiltin_CreateIssue_identityAndLabels pins the ADR-0014 agent create:
// labels attach at creation under strict name resolution, the issue is
// authored per the tracker's identity (operator by default, the run after
// ForRun), and an unknown label creates nothing — not even an issue number.
func TestBuiltin_CreateIssue_identityAndLabels(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	is, err := tr.CreateIssue(ctx, "found a bug", "details", []string{"needs-triage"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if is.Number != 1 || is.State != tracker.StateOpen || !contains(is.Labels, "needs-triage") {
		t.Errorf("created issue = %+v, want #1 open with needs-triage", is)
	}
	stored, err := s.IssueByRepoNumber(ctx, repo.ID, is.Number)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if stored.AuthorKind != store.CommentAuthorOperator || stored.RunID != nil {
		t.Errorf("default author = (%q, %v), want (operator, nil)", stored.AuthorKind, stored.RunID)
	}

	// ForRun rescopes the create identity, like comments.
	run, err := s.CreateRun(ctx, store.Run{
		ID: ids.NewID("run"), RepoID: repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/z", WorktreePath: "/wt/z", SessionName: "proj~z", Model: "opus[1m]",
		Effort: "max", StartedAt: fixedNow, Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runIs, err := tr.ForRun(run.ID).CreateIssue(ctx, "follow-up", "found mid-run", nil)
	if err != nil {
		t.Fatalf("CreateIssue as run: %v", err)
	}
	stored, err = s.IssueByRepoNumber(ctx, repo.ID, runIs.Number)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if stored.AuthorKind != store.CommentAuthorRun || stored.RunID == nil || *stored.RunID != run.ID {
		t.Errorf("run-created author = (%q, %v), want (run, %q)", stored.AuthorKind, stored.RunID, run.ID)
	}

	// A typo'd label fails loudly and creates nothing.
	_, err = tr.CreateIssue(ctx, "doomed", "", []string{"needs-triage", "nope-label"})
	if !errors.Is(err, tracker.ErrUnknownLabel) || !strings.Contains(err.Error(), "nope-label") {
		t.Fatalf("CreateIssue with unknown label err = %v, want ErrUnknownLabel naming it", err)
	}
	next, err := tr.CreateIssue(ctx, "sequence intact", "", nil)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if next.Number != runIs.Number+1 {
		t.Errorf("number after refused create = %d, want %d (no number leaked)", next.Number, runIs.Number+1)
	}
}

// TestBuiltin_IssueLabelAddRemove pins the label ops: strict resolution
// before anything is applied, idempotent re-add / unattached-remove, and a
// missing issue or unknown label surfacing as the typed errors.
func TestBuiltin_IssueLabelAddRemove(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	is, _ := s.CreateIssue(ctx, repo.ID, "triage me", "", fixedNow)

	if err := tr.AddIssueLabels(ctx, is.Number, []string{"needs-triage", "ready-for-agent"}); err != nil {
		t.Fatalf("AddIssueLabels: %v", err)
	}
	// Idempotent re-add.
	if err := tr.AddIssueLabels(ctx, is.Number, []string{"needs-triage"}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	got, _ := s.IssueByRepoNumber(ctx, repo.ID, is.Number)
	if !contains(got.Labels, "needs-triage") || !contains(got.Labels, "ready-for-agent") || len(got.Labels) != 2 {
		t.Errorf("labels after add = %v, want [needs-triage ready-for-agent]", got.Labels)
	}

	// The strict-resolution failure applies nothing: an unknown name aborts
	// BEFORE the valid one attaches.
	err := tr.AddIssueLabels(ctx, is.Number, []string{"wontfix", "no-such-label"})
	if !errors.Is(err, tracker.ErrUnknownLabel) {
		t.Fatalf("AddIssueLabels unknown err = %v, want ErrUnknownLabel", err)
	}
	got, _ = s.IssueByRepoNumber(ctx, repo.ID, is.Number)
	if contains(got.Labels, "wontfix") {
		t.Errorf("labels after refused add = %v — the valid name must not attach", got.Labels)
	}

	// Remove: triage-role swap (the state-machine move).
	if err := tr.RemoveIssueLabels(ctx, is.Number, []string{"needs-triage"}); err != nil {
		t.Fatalf("RemoveIssueLabels: %v", err)
	}
	// Unattached-but-defined remove is a no-op; unknown name is an error.
	if err := tr.RemoveIssueLabels(ctx, is.Number, []string{"needs-info"}); err != nil {
		t.Fatalf("remove unattached: %v", err)
	}
	if err := tr.RemoveIssueLabels(ctx, is.Number, []string{"no-such-label"}); !errors.Is(err, tracker.ErrUnknownLabel) {
		t.Fatalf("remove unknown err = %v, want ErrUnknownLabel", err)
	}
	got, _ = s.IssueByRepoNumber(ctx, repo.ID, is.Number)
	if len(got.Labels) != 1 || got.Labels[0] != "ready-for-agent" {
		t.Errorf("labels after remove = %v, want [ready-for-agent]", got.Labels)
	}

	// Missing issue surfaces ErrNotFound on both ops.
	if err := tr.AddIssueLabels(ctx, 999, []string{"needs-triage"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AddIssueLabels(999) err = %v, want ErrNotFound", err)
	}
	if err := tr.RemoveIssueLabels(ctx, 999, []string{"needs-triage"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RemoveIssueLabels(999) err = %v, want ErrNotFound", err)
	}
}

// TestBuiltin_LabelsAndEnsure pins the label listing (the five seeded triage
// labels, name-ordered) and the idempotent ensure: absent → created with the
// store default color; present → returned untouched, retry-safe.
func TestBuiltin_LabelsAndEnsure(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	labels, err := tr.Labels(ctx)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	wantSeed := []string{"needs-info", "needs-triage", "ready-for-agent", "ready-for-human", "wontfix"}
	if len(labels) != len(wantSeed) {
		t.Fatalf("Labels = %+v, want the five seeded triage labels", labels)
	}
	for i, want := range wantSeed {
		if labels[i].Name != want {
			t.Errorf("labels[%d] = %q, want %q (name order)", i, labels[i].Name, want)
		}
	}

	// Ensure an absent label: created, empty color takes the store default.
	created, err := tr.EnsureLabel(ctx, "bug", "", "confirmed defect")
	if err != nil {
		t.Fatalf("EnsureLabel new: %v", err)
	}
	if created.Color != store.LabelDefaultColor || created.Description != "confirmed defect" {
		t.Errorf("created label = %+v, want default color + description", created)
	}

	// Ensure again with DIFFERENT color/description: the existing label wins
	// untouched (ensure never updates).
	again, err := tr.EnsureLabel(ctx, "bug", "#ff0000", "changed")
	if err != nil {
		t.Fatalf("EnsureLabel existing: %v", err)
	}
	if again.Color != store.LabelDefaultColor || again.Description != "confirmed defect" {
		t.Errorf("re-ensured label = %+v, want the original untouched", again)
	}
	all, err := s.LabelsByRepo(ctx, repo.ID)
	if err != nil {
		t.Fatalf("LabelsByRepo: %v", err)
	}
	if len(all) != len(wantSeed)+1 {
		t.Errorf("label count after double ensure = %d, want %d (no duplicate)", len(all), len(wantSeed)+1)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// Regression (review): a duplicate CreatePull on a head that already carries
// an OPEN CR is refused with tracker.ErrDuplicateOpenPull naming the existing
// number (forge parity — Forgejo 409s the same agent retry). Once the CR
// leaves the open state, the head is free for a new CR.
func TestBuiltin_CreatePullRefusesDuplicateOpenHead(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	first, err := tr.CreatePull(ctx, "afk/3", "main", "first", "Closes #3")
	if err != nil {
		t.Fatalf("first CreatePull: %v", err)
	}
	_, err = tr.CreatePull(ctx, "afk/3", "main", "retry", "Closes #3")
	if !errors.Is(err, tracker.ErrDuplicateOpenPull) {
		t.Fatalf("duplicate CreatePull err = %v, want ErrDuplicateOpenPull", err)
	}
	if !strings.Contains(err.Error(), "#1") {
		t.Errorf("duplicate error does not name the existing CR: %v", err)
	}
	// Only one CR exists.
	crs, err := s.CRsByRepo(ctx, repo.ID, store.CRStateAll)
	if err != nil {
		t.Fatalf("CRsByRepo: %v", err)
	}
	if len(crs) != 1 {
		t.Fatalf("CR count after refused duplicate = %d, want 1", len(crs))
	}
	// A closed CR releases the head.
	if _, err := s.CloseCR(ctx, repo.ID, first.Number, time.Now()); err != nil {
		t.Fatalf("CloseCR: %v", err)
	}
	if _, err := tr.CreatePull(ctx, "afk/3", "main", "after close", "Closes #3"); err != nil {
		t.Errorf("CreatePull after close: %v", err)
	}
}

// --- MergePull -------------------------------------------------------------

// fakeMerger is a scriptable tracker.CRMerger for the builtin MergePull tests:
// it stands in for the shared crmerge.Service so these tests exercise the
// tracker's error-mapping and convergence logic without a git fixture.
type fakeMerger struct {
	cr    store.CR
	err   error
	calls int
}

func (f *fakeMerger) Merge(_ context.Context, _ string, _ int) (store.CR, error) {
	f.calls++
	return f.cr, f.err
}

// newTrackerWithMerger builds a built-in tracker wired to a CR-merge service.
func newTrackerWithMerger(s *store.Store, repoID string, m tracker.CRMerger) *Tracker {
	tr := New(tracker.BuiltinConfig{Store: s, RepoID: repoID, Merger: m}).(*Tracker)
	tr.now = func() time.Time { return fixedNow }
	return tr
}

// TestBuiltin_MergePull_success: the service merges; the returned PullRef is
// the merged CR.
func TestBuiltin_MergePull_success(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	merged := store.CR{RepoID: repo.ID, Number: 1, HeadBranch: "afk/1", State: store.CRStateMerged}
	tr := newTrackerWithMerger(s, repo.ID, &fakeMerger{cr: merged})

	pr, err := tr.MergePull(ctx, 1)
	if err != nil {
		t.Fatalf("MergePull: %v", err)
	}
	if pr.Number != 1 || pr.State != tracker.PullMerged || pr.HeadBranch != "afk/1" {
		t.Errorf("PullRef = %+v, want #1 afk/1 merged", pr)
	}
}

// TestBuiltin_MergePull_alreadyMergedConverges pins the idempotency contract:
// the service refuses a non-open CR (ErrCRNotOpen, no git run); MergePull
// re-reads the store, sees it already merged, and returns success.
func TestBuiltin_MergePull_alreadyMergedConverges(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	if _, err := s.CreateCR(ctx, repo.ID, "cr", "", "afk/1", "main", nil, fixedNow); err != nil {
		t.Fatalf("CreateCR: %v", err)
	}
	if _, err := s.MergeCR(ctx, repo.ID, 1, "abc1234", fixedNow); err != nil {
		t.Fatalf("MergeCR: %v", err)
	}
	m := &fakeMerger{err: fmt.Errorf("%w (state %q)", store.ErrCRNotOpen, "merged")}
	tr := newTrackerWithMerger(s, repo.ID, m)

	pr, err := tr.MergePull(ctx, 1)
	if err != nil {
		t.Fatalf("MergePull (convergent) err = %v, want success", err)
	}
	if pr.State != tracker.PullMerged || pr.Number != 1 {
		t.Errorf("convergent PullRef = %+v, want #1 merged", pr)
	}
}

// TestBuiltin_MergePull_closedUnmergedRejected: a closed-unmerged CR is NOT a
// convergent success — it is a rejection (no reopen exists).
func TestBuiltin_MergePull_closedUnmergedRejected(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	if _, err := s.CreateCR(ctx, repo.ID, "cr", "", "afk/1", "main", nil, fixedNow); err != nil {
		t.Fatalf("CreateCR: %v", err)
	}
	if _, err := s.CloseCR(ctx, repo.ID, 1, fixedNow); err != nil {
		t.Fatalf("CloseCR: %v", err)
	}
	m := &fakeMerger{err: fmt.Errorf("%w (state %q)", store.ErrCRNotOpen, "closed")}
	tr := newTrackerWithMerger(s, repo.ID, m)

	if _, err := tr.MergePull(ctx, 1); !errors.Is(err, tracker.ErrMergeRejected) {
		t.Fatalf("MergePull on closed CR err = %v, want ErrMergeRejected", err)
	}
}

// TestBuiltin_MergePull_gitRefusalIsRejected: a git refusal (push rejected,
// conflict, head missing) — a typed gitx sentinel — surfaces as
// ErrMergeRejected carrying its own words.
func TestBuiltin_MergePull_gitRefusalIsRejected(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	m := &fakeMerger{err: fmt.Errorf("%w: protected branch main", gitx.ErrPushRejected)}
	tr := newTrackerWithMerger(s, repo.ID, m)

	_, err := tr.MergePull(ctx, 1)
	if !errors.Is(err, tracker.ErrMergeRejected) {
		t.Fatalf("err = %v, want ErrMergeRejected", err)
	}
	if !strings.Contains(err.Error(), "protected branch main") {
		t.Fatalf("err = %q, want the backend's own words verbatim", err.Error())
	}
}

// TestBuiltin_MergePull_internalErrorIsNotRejected: a transient/internal error
// from the service (a DB failure, a credential misconfig) is NOT a merge
// refusal — it stays a raw error so the agent answers 500 rather than a
// permanent, retry-proof 409, and no internal diagnostic leaks as a refusal.
func TestBuiltin_MergePull_internalErrorIsNotRejected(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	m := &fakeMerger{err: errors.New("database is locked")}
	tr := newTrackerWithMerger(s, repo.ID, m)

	_, err := tr.MergePull(ctx, 1)
	if err == nil {
		t.Fatal("MergePull err = nil, want the internal error surfaced")
	}
	if errors.Is(err, tracker.ErrMergeRejected) {
		t.Fatalf("internal error mislabeled as a merge refusal: %v", err)
	}
}

// TestBuiltin_MergePull_notFound: an unknown number stays store.ErrNotFound.
func TestBuiltin_MergePull_notFound(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	m := &fakeMerger{err: fmt.Errorf("cr %s#9: %w", repo.ID, store.ErrNotFound)}
	tr := newTrackerWithMerger(s, repo.ID, m)

	if _, err := tr.MergePull(ctx, 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestBuiltin_MergePull_noServiceWiredFailsLoud: with no merge service wired,
// MergePull fails loud rather than pretending to merge — and it is NOT a merge
// rejection (that would misreport a wiring bug as an unmergeable PR).
func TestBuiltin_MergePull_noServiceWiredFailsLoud(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID) // no Merger

	_, err := tr.MergePull(ctx, 1)
	if err == nil || errors.Is(err, tracker.ErrMergeRejected) {
		t.Fatalf("err = %v, want a plain wiring error (not a merge rejection)", err)
	}
}

// --- Reviews (deferred on the built-in binding) ----------------------------

// TestBuiltin_Reviews_empty: the built-in tracker has no forge review model, so
// Reviews is a harmless empty read for any number, never an error.
func TestBuiltin_Reviews_empty(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	reviews, err := tr.Reviews(ctx, 1)
	if err != nil {
		t.Fatalf("Reviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Errorf("Reviews = %#v, want empty", reviews)
	}
}

// TestBuiltin_ReviewWrites_unsupported: each review-adjacent WRITE verb wraps
// tracker.ErrUnsupported (reviews are forge-observable state a lab-internal CR
// has no model for, and CRs have no comment thread yet), and names the verb
// rather than faking a result.
func TestBuiltin_ReviewWrites_unsupported(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	if err := tr.RerequestReview(ctx, 1); !errors.Is(err, tracker.ErrUnsupported) {
		t.Errorf("RerequestReview err = %v, want ErrUnsupported", err)
	}
	if err := tr.CommentPull(ctx, 1, "body"); !errors.Is(err, tracker.ErrUnsupported) {
		t.Errorf("CommentPull err = %v, want ErrUnsupported", err)
	}
}

// TestBuiltin_PullComments_unsupported: CommentPull's read counterpart wraps
// the same tracker.ErrUnsupported (no PR comment thread to list yet).
func TestBuiltin_PullComments_unsupported(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	if _, err := tr.PullComments(ctx, 1); !errors.Is(err, tracker.ErrUnsupported) {
		t.Errorf("PullComments err = %v, want ErrUnsupported", err)
	}
}

// TestBuiltin_CheckLog_unsupported: a lab-internal CR has no CI behind it and
// so no forge-served job log to proxy — CheckLog wraps tracker.ErrUnsupported.
func TestBuiltin_CheckLog_unsupported(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	repo := seedRepo(t, s)
	tr := newTracker(s, repo.ID)

	if _, err := tr.CheckLog(ctx, 1, "ci/build"); !errors.Is(err, tracker.ErrUnsupported) {
		t.Errorf("CheckLog err = %v, want ErrUnsupported", err)
	}
}
