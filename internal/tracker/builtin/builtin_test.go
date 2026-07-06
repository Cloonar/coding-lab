package builtin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
		Provider: "claude-code", AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
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
	all, err := tr.Issues(ctx, tracker.StateAll)
	if err != nil {
		t.Fatalf("Issues all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("all issues = %d, want 2", len(all))
	}
	for _, is := range all {
		if is.Comments != nil {
			t.Errorf("list view issue #%d carried comments, want nil", is.Number)
		}
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

// TestBuiltin_Pulls_stateMappingAndPRPresent pins the builtin done-signal
// (M6): Pulls returns every CR across ALL states mapped onto the tracker's PR
// vocabulary, and tracker.PRPresent over that result treats open|merged as
// done and closed-unmerged (or absent) as no PR — identical to a forge repo.
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
		t.Fatalf("Pulls = %d refs, want 3 (all states)", len(pulls))
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
