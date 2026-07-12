package gitx

// Integration tests for the pull-base surface (PullBase, CommitsBehind,
// SummarizeRange) against real git, mirroring the crmerge_test topology: a
// BARE origin (the repo's real remote), a work clone that drives origin's
// main forward (the "someone else pushed" actor), the lab bare reference
// clone the Engine operates on — plus, new here, a LIVE run worktree created
// via AddWorktree that the pull merges into. The hard property under test is
// the failure contract: every failing pull leaves the worktree byte-identical
// to how it found it.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

const (
	pullAuthorName  = "Pull Author"
	pullAuthorEmail = "pull@example.invalid"
	pullAuthorID    = pullAuthorName + "|" + pullAuthorEmail + "|" + pullAuthorName + "|" + pullAuthorEmail
	pullBranch      = "afk/9"
)

type pullFixture struct {
	t      *testing.T
	home   string
	origin string // bare push target — the repo's REAL remote
	work   string // working clone that advances origin's main
	bare   string // lab bare reference clone (the Engine's bareDir)
	wt     string // the run's live worktree on pullBranch
	env    []string
	eng    *Engine
}

func newPullFixture(t *testing.T) *pullFixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	env := testutil.HermeticGitEnv(home)

	work := makeOrigin(t, home, "main", 2) // f0.txt, f1.txt
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitCmd(t, home, "", "init", "-q", "--bare", "-b", "main", origin)
	gitCmd(t, home, work, "remote", "add", "origin", origin)
	gitCmd(t, home, work, "push", "-q", "origin", "main")

	bare := filepath.Join(t.TempDir(), "repo.git")
	eng := New("git")
	if err := eng.CloneBare(t.Context(), origin, bare, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	wt := filepath.Join(t.TempDir(), "run-wt")
	if err := eng.AddWorktree(t.Context(), bare, wt, pullBranch, "main", env); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	return &pullFixture{t: t, home: home, origin: origin, work: work, bare: bare, wt: wt, env: env, eng: eng}
}

// advanceOrigin commits file=content in the work clone and pushes it to
// origin's main — base movement the bare reference clone has NOT fetched.
func (f *pullFixture) advanceOrigin(file, content string) string {
	t := f.t
	t.Helper()
	writeFileT(t, filepath.Join(f.work, file), content)
	gitCmd(t, f.home, f.work, "add", "-A")
	gitCmd(t, f.home, f.work, "commit", "-q", "-m", "advance "+file)
	gitCmd(t, f.home, f.work, "push", "-q", "origin", "main")
	return gitCmd(t, f.home, f.work, "rev-parse", "HEAD")
}

// commitWorktree commits file=content on the run's branch in the live
// worktree and returns the new HEAD sha.
func (f *pullFixture) commitWorktree(file, content, msg string) string {
	t := f.t
	t.Helper()
	writeFileT(t, filepath.Join(f.wt, file), content)
	gitCmd(t, f.home, f.wt, "add", "-A")
	gitCmd(t, f.home, f.wt, "commit", "-q", "-m", msg)
	return gitCmd(t, f.home, f.wt, "rev-parse", "HEAD")
}

func (f *pullFixture) worktreeHead() string {
	f.t.Helper()
	return gitCmd(f.t, f.home, f.wt, "rev-parse", "HEAD")
}

// mergeHeadExists reports whether the worktree is mid-merge (MERGE_HEAD
// resolves) — checked without gitCmd because a missing MERGE_HEAD exits 1
// and must not fail the test.
func (f *pullFixture) mergeHeadExists() bool {
	f.t.Helper()
	cmd := exec.Command("git", "rev-parse", "-q", "--verify", "MERGE_HEAD")
	cmd.Dir = f.wt
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(f.home)...)
	return cmd.Run() == nil
}

// requireClean asserts `git status --porcelain` is empty in the worktree.
func (f *pullFixture) requireClean(when string) {
	f.t.Helper()
	if out := gitCmd(f.t, f.home, f.wt, "status", "--porcelain"); out != "" {
		f.t.Errorf("worktree not clean %s:\n%s", when, out)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestCommitsBehind_readsLocalRefOnly(t *testing.T) {
	f := newPullFixture(t)

	if n, err := f.eng.CommitsBehind(t.Context(), f.bare, pullBranch, "main", f.env); err != nil || n != 0 {
		t.Errorf("CommitsBehind fresh fork = (%d, %v), want (0, nil)", n, err)
	}

	f.advanceOrigin("behind1.txt", "one\n")
	f.advanceOrigin("behind2.txt", "two\n")

	// No fetch yet: the LOCAL remote-tracking ref is stale, so the count must
	// still be 0 — CommitsBehind never fetches by itself.
	if n, err := f.eng.CommitsBehind(t.Context(), f.bare, pullBranch, "main", f.env); err != nil || n != 0 {
		t.Errorf("CommitsBehind before fetch = (%d, %v), want (0, nil): it must not fetch by itself", n, err)
	}

	if err := f.eng.Fetch(t.Context(), f.bare, f.env); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n, err := f.eng.CommitsBehind(t.Context(), f.bare, pullBranch, "main", f.env); err != nil || n != 2 {
		t.Errorf("CommitsBehind after fetch = (%d, %v), want (2, nil)", n, err)
	}
}

func TestPullBase_fastForward(t *testing.T) {
	f := newPullFixture(t)
	baseSHA := f.advanceOrigin("base.txt", "base content\n")
	oldHead := f.worktreeHead()

	res, err := f.eng.PullBase(t.Context(), f.bare, f.wt, "main", pullAuthorName, pullAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if res.UpToDate {
		t.Error("UpToDate = true for a worktree behind origin")
	}
	if !res.FastForward {
		t.Error("FastForward = false for a pure fast-forward pull")
	}
	if res.OldHead != oldHead {
		t.Errorf("OldHead = %s, want %s", res.OldHead, oldHead)
	}
	if res.NewHead == res.OldHead {
		t.Error("NewHead == OldHead after a fast-forward pull")
	}
	// No merge commit: the new HEAD IS origin/main's sha.
	if res.NewHead != baseSHA {
		t.Errorf("NewHead = %s, want origin/main %s (a fast-forward must not create a commit)", res.NewHead, baseSHA)
	}
	if h := f.worktreeHead(); h != baseSHA {
		t.Errorf("worktree HEAD = %s, want %s", h, baseSHA)
	}
	if got := readFileT(t, filepath.Join(f.wt, "base.txt")); got != "base content\n" {
		t.Errorf("pulled file content = %q, want %q", got, "base content\n")
	}
	f.requireClean("after fast-forward pull")
}

func TestPullBase_mergeCommit(t *testing.T) {
	f := newPullFixture(t)
	localSHA := f.commitWorktree("local.txt", "local\n", "local work")
	baseSHA := f.advanceOrigin("base.txt", "base\n") // disjoint files → clean merge

	res, err := f.eng.PullBase(t.Context(), f.bare, f.wt, "main", pullAuthorName, pullAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	if res.UpToDate || res.FastForward {
		t.Errorf("UpToDate=%v FastForward=%v for a diverged pull, want false/false", res.UpToDate, res.FastForward)
	}
	if res.OldHead != localSHA {
		t.Errorf("OldHead = %s, want %s", res.OldHead, localSHA)
	}
	if res.NewHead == localSHA || res.NewHead == baseSHA {
		t.Fatalf("NewHead = %s, want a fresh merge commit (local %s, base %s)", res.NewHead, localSHA, baseSHA)
	}
	// Parent order: ^1 = the branch merged into (the old worktree HEAD),
	// ^2 = the freshly-fetched origin/main.
	if p1 := gitCmd(t, f.home, f.wt, "rev-parse", res.NewHead+"^1"); p1 != localSHA {
		t.Errorf("merge commit ^1 = %s, want old head %s", p1, localSHA)
	}
	if p2 := gitCmd(t, f.home, f.wt, "rev-parse", res.NewHead+"^2"); p2 != baseSHA {
		t.Errorf("merge commit ^2 = %s, want origin base %s", p2, baseSHA)
	}
	// The passed real identity must win over the ambient GIT_AUTHOR_*/
	// GIT_COMMITTER_* (HermeticGitEnv sets lab-test).
	if id := gitCmd(t, f.home, f.wt, "log", "-1", "--format=%an|%ae|%cn|%ce", res.NewHead); id != pullAuthorID {
		t.Errorf("merge commit identity = %q, want %q", id, pullAuthorID)
	}
	if got := readFileT(t, filepath.Join(f.wt, "base.txt")); got != "base\n" {
		t.Errorf("pulled file content = %q, want %q", got, "base\n")
	}
	if got := readFileT(t, filepath.Join(f.wt, "local.txt")); got != "local\n" {
		t.Errorf("local file content = %q, want %q", got, "local\n")
	}
	f.requireClean("after merge pull")
}

func TestPullBase_upToDate(t *testing.T) {
	f := newPullFixture(t)

	// Fresh fork: origin has not moved.
	res, err := f.eng.PullBase(t.Context(), f.bare, f.wt, "main", pullAuthorName, pullAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("PullBase: %v", err)
	}
	head := f.worktreeHead()
	if !res.UpToDate || res.FastForward {
		t.Errorf("result = %+v, want UpToDate=true FastForward=false", res)
	}
	if res.OldHead != head || res.NewHead != head {
		t.Errorf("OldHead/NewHead = %s/%s, want both %s (no commit created)", res.OldHead, res.NewHead, head)
	}

	// Worktree AHEAD of an unmoved origin is still up to date: the base is
	// reachable from HEAD, so no merge is attempted.
	aheadSHA := f.commitWorktree("ahead.txt", "ahead\n", "ahead work")
	res, err = f.eng.PullBase(t.Context(), f.bare, f.wt, "main", pullAuthorName, pullAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("PullBase (ahead): %v", err)
	}
	if !res.UpToDate || res.NewHead != aheadSHA {
		t.Errorf("ahead result = %+v, want UpToDate=true NewHead=%s", res, aheadSHA)
	}
	if h := f.worktreeHead(); h != aheadSHA {
		t.Errorf("worktree HEAD = %s after up-to-date pull, want unchanged %s", h, aheadSHA)
	}
}

func TestPullBase_conflictAbortsClean(t *testing.T) {
	f := newPullFixture(t)
	localSHA := f.commitWorktree("f0.txt", "local version\n", "local change to f0")
	f.advanceOrigin("f0.txt", "base version\n") // same file → conflict

	_, err := f.eng.PullBase(t.Context(), f.bare, f.wt, "main", pullAuthorName, pullAuthorEmail, f.env)
	if err == nil {
		t.Fatal("conflicting PullBase succeeded, want error")
	}
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("err = %v, want ErrMergeConflict", err)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ConflictError", err)
	}
	if len(ce.Files) != 1 || ce.Files[0] != "f0.txt" {
		t.Errorf("ConflictError.Files = %v, want [f0.txt]", ce.Files)
	}
	if !strings.Contains(ce.Report, "CONFLICT") {
		t.Errorf("ConflictError.Report does not carry git's conflict report: %q", ce.Report)
	}

	// The worktree-untouched contract: HEAD unchanged, no merge in progress,
	// the conflicted file restored byte-identical, status clean.
	if h := f.worktreeHead(); h != localSHA {
		t.Errorf("worktree HEAD = %s after aborted conflict, want unchanged %s", h, localSHA)
	}
	if f.mergeHeadExists() {
		t.Error("MERGE_HEAD still exists after the conflict abort")
	}
	if got := readFileT(t, filepath.Join(f.wt, "f0.txt")); got != "local version\n" {
		t.Errorf("f0.txt = %q after aborted conflict, want restored %q", got, "local version\n")
	}
	f.requireClean("after aborted conflict")
}

func TestPullBase_dirtyClobberRefusal(t *testing.T) {
	f := newPullFixture(t)
	localSHA := f.commitWorktree("local.txt", "local\n", "local work")  // diverge → real merge path
	writeFileT(t, filepath.Join(f.wt, "f0.txt"), "dirty uncommitted\n") // dirty file the merge touches
	f.advanceOrigin("f0.txt", "base version\n")

	_, err := f.eng.PullBase(t.Context(), f.bare, f.wt, "main", pullAuthorName, pullAuthorEmail, f.env)
	if err == nil {
		t.Fatal("PullBase over a dirty file the merge touches succeeded, want git's refusal")
	}
	if errors.Is(err, ErrMergeConflict) {
		t.Errorf("dirty-clobber refusal misclassified as merge conflict: %v", err)
	}
	// Git refuses BEFORE the merge starts; its message must survive verbatim.
	if !strings.Contains(err.Error(), "overwritten by merge") {
		t.Errorf("refusal error does not carry git's message verbatim: %v", err)
	}
	if h := f.worktreeHead(); h != localSHA {
		t.Errorf("worktree HEAD = %s after refusal, want unchanged %s", h, localSHA)
	}
	if f.mergeHeadExists() {
		t.Error("MERGE_HEAD exists after a pre-merge refusal")
	}
	if got := readFileT(t, filepath.Join(f.wt, "f0.txt")); got != "dirty uncommitted\n" {
		t.Errorf("dirty modification = %q after refusal, want preserved %q", got, "dirty uncommitted\n")
	}
}

func TestPullBase_dirtyUnrelatedFileSurvives(t *testing.T) {
	f := newPullFixture(t)
	writeFileT(t, filepath.Join(f.wt, "f1.txt"), "dirty but unrelated\n") // merge does not touch f1.txt
	baseSHA := f.advanceOrigin("base.txt", "base\n")

	res, err := f.eng.PullBase(t.Context(), f.bare, f.wt, "main", pullAuthorName, pullAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("PullBase with an unrelated dirty file: %v", err)
	}
	if res.NewHead != baseSHA {
		t.Errorf("NewHead = %s, want %s", res.NewHead, baseSHA)
	}
	if got := readFileT(t, filepath.Join(f.wt, "f1.txt")); got != "dirty but unrelated\n" {
		t.Errorf("unrelated dirty file = %q after pull, want preserved %q", got, "dirty but unrelated\n")
	}
}

func TestPullBase_fetchFailureLeavesWorktreeUntouched(t *testing.T) {
	f := newPullFixture(t)
	head := f.worktreeHead()
	gitCmd(t, f.home, f.bare, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	_, err := f.eng.PullBase(t.Context(), f.bare, f.wt, "main", pullAuthorName, pullAuthorEmail, f.env)
	if err == nil {
		t.Fatal("PullBase with a broken origin succeeded, want fetch error")
	}
	if !strings.Contains(err.Error(), "does not appear to be a git repository") {
		t.Errorf("fetch failure does not surface git stderr: %v", err)
	}
	if h := f.worktreeHead(); h != head {
		t.Errorf("worktree HEAD = %s after failed fetch, want unchanged %s", h, head)
	}
	f.requireClean("after failed fetch")
}

func TestSummarizeRange_capsAndRename(t *testing.T) {
	f := newPullFixture(t)
	from := f.worktreeHead()
	f.commitWorktree("a.txt", "a\n", "subject one")
	f.commitWorktree("f0.txt", "changed\n", "subject two")
	gitCmd(t, f.home, f.wt, "mv", "f1.txt", "renamed.txt")
	gitCmd(t, f.home, f.wt, "commit", "-q", "-m", "subject three")
	to := f.worktreeHead()

	// Subjects capped at 2 of 3, newest first; files capped at 2 of 3.
	s, err := f.eng.SummarizeRange(t.Context(), f.wt, from, to, 2, 2, f.env)
	if err != nil {
		t.Fatalf("SummarizeRange: %v", err)
	}
	if want := []string{"subject three", "subject two"}; len(s.Subjects) != 2 || s.Subjects[0] != want[0] || s.Subjects[1] != want[1] {
		t.Errorf("Subjects = %v, want %v", s.Subjects, want)
	}
	if s.TotalCommits != 3 {
		t.Errorf("TotalCommits = %d, want 3", s.TotalCommits)
	}
	if len(s.Files) != 2 {
		t.Errorf("len(Files) = %d, want 2 (capped)", len(s.Files))
	}
	if s.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want 3 (counted before capping)", s.TotalFiles)
	}

	// Uncapped: full name-status parse including the rename line.
	s, err = f.eng.SummarizeRange(t.Context(), f.wt, from, to, 10, 10, f.env)
	if err != nil {
		t.Fatalf("SummarizeRange (uncapped): %v", err)
	}
	if s.TotalCommits != 3 || len(s.Subjects) != 3 {
		t.Errorf("uncapped Subjects/TotalCommits = %d/%d, want 3/3", len(s.Subjects), s.TotalCommits)
	}
	if s.TotalFiles != 3 || len(s.Files) != 3 {
		t.Fatalf("uncapped Files/TotalFiles = %d/%d, want 3/3", len(s.Files), s.TotalFiles)
	}
	byPath := map[string]string{}
	for _, fc := range s.Files {
		byPath[fc.Path] = fc.Status
	}
	if byPath["a.txt"] != "A" {
		t.Errorf("a.txt status = %q, want A (files: %v)", byPath["a.txt"], s.Files)
	}
	if byPath["f0.txt"] != "M" {
		t.Errorf("f0.txt status = %q, want M (files: %v)", byPath["f0.txt"], s.Files)
	}
	if byPath["f1.txt -> renamed.txt"] != "R100" {
		t.Errorf("rename entry = %v, want {R100 f1.txt -> renamed.txt}", s.Files)
	}
}
