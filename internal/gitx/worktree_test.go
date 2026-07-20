package gitx

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// Integration tests for the M3 worktree lifecycle against real git
// (design §11), transcribed from v0 git_test.go / handlers_test.go via the
// git-worktrees port-spec — but on the rewrite's production repo shape: a
// lab-owned BARE reference clone (D8) instead of v0's non-bare checkout.

// wtFixture is one hermetic repo world: a non-bare fixture origin (on
// main), the lab-owned bare reference clone of it, and a worktrees root.
type wtFixture struct {
	home, origin, bare, wtRoot string
	env                        []string
	eng                        *Engine
}

func newWtFixture(t *testing.T) *wtFixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeOrigin(t, home, "main", 2)
	bare := filepath.Join(t.TempDir(), "repo.git")
	env := testutil.HermeticGitEnv(home)
	eng := New("git")
	if err := eng.CloneBare(t.Context(), "file://"+origin, bare, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	return &wtFixture{
		home:   home,
		origin: origin,
		bare:   bare,
		wtRoot: filepath.Join(t.TempDir(), "worktrees"),
		env:    env,
		eng:    eng,
	}
}

// branchExists reports whether refs/heads/<branch> resolves in the bare
// reference clone — non-fatal, for keep-vs-delete assertions.
func (f *wtFixture) branchExists(branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = f.bare
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(f.home)...)
	return cmd.Run() == nil
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// AddWorktree forks from FRESHLY-FETCHED origin/<default> — the origin
// advances after the clone, and the new worktree must sit on the advanced
// tip (fetch-first is load-bearing). The rollback helpers RemoveWorktree +
// DeleteBranch then restore the exact pre-start state as two separate ops.
func TestAddWorktree_realGitFreshForkAndRollbackHelpers(t *testing.T) {
	f := newWtFixture(t)

	// Advance the origin under the clone's feet.
	if err := os.WriteFile(filepath.Join(f.origin, "advance.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	gitCmd(t, f.home, f.origin, "add", ".")
	gitCmd(t, f.home, f.origin, "commit", "-q", "-m", "advance")
	wantSHA := gitCmd(t, f.home, f.origin, "rev-parse", "main")

	const branch = "lab/20260608-1530"
	wt := filepath.Join(f.wtRoot, WorktreeDir("repo", "20260608-1530"))
	if err := f.eng.AddWorktree(t.Context(), f.bare, wt, branch, "main", f.env); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if got := gitCmd(t, f.home, wt, "rev-parse", "HEAD"); got != wantSHA {
		t.Errorf("worktree HEAD = %s; want the freshly-fetched origin tip %s", got, wantSHA)
	}
	if got := gitCmd(t, f.home, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
		t.Errorf("worktree on %q; want refs/heads/%s", got, branch)
	}
	if !f.branchExists(branch) {
		t.Fatalf("branch %s not created in the reference clone", branch)
	}

	// Rollback contract: the two helpers are separate ops and restore the
	// pre-start state.
	if err := f.eng.RemoveWorktree(t.Context(), f.bare, wt, f.env); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if dirExists(wt) {
		t.Errorf("worktree dir %s still exists after RemoveWorktree", wt)
	}
	if !f.branchExists(branch) {
		t.Errorf("RemoveWorktree deleted branch %s; branch deletion is the separate deliberate step", branch)
	}
	if err := f.eng.DeleteBranch(t.Context(), f.bare, branch, f.env); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if f.branchExists(branch) {
		t.Errorf("branch %s still exists after DeleteBranch", branch)
	}
}

// A repo with no usable origin fails AddWorktree at the fetch, loudly, with
// git's stderr verbatim — and creates NOTHING (no worktree, no branch), so
// a failure before worktree creation has nothing to roll back. There is no
// fallback base by design.
func TestAddWorktree_failsLoudWithoutUsableOrigin(t *testing.T) {
	f := newWtFixture(t)
	gitCmd(t, f.home, f.bare, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	const branch = "lab/20260608-1530"
	wt := filepath.Join(f.wtRoot, "repo-20260608-1530")
	err := f.eng.AddWorktree(t.Context(), f.bare, wt, branch, "main", f.env)
	if err == nil {
		t.Fatal("AddWorktree with a missing origin succeeded, want fail-loud error")
	}
	if !strings.Contains(err.Error(), "does not appear to be a git repository") {
		t.Errorf("AddWorktree error does not surface git stderr verbatim: %v", err)
	}
	if dirExists(wt) {
		t.Errorf("failed AddWorktree left worktree dir %s behind", wt)
	}
	if f.branchExists(branch) {
		t.Errorf("failed AddWorktree left branch %s behind", branch)
	}
}

// AddWorktreeExisting is the lander's adopt-branch checkout (issue #181):
// the matrix covers the fresh-local-branch fork from origin/<branch>, the
// existing-local-branch checkout, the checked-out-elsewhere refusal, the
// missing-remote-branch refusal, and the hard alignment of a diverged local
// branch back to origin/<branch>.
func TestAddWorktreeExisting_realGitMatrix(t *testing.T) {
	const branch = "afk/7"

	// pushBranch publishes branch on the fixture origin with one commit past
	// main and returns its tip SHA — the PR-head shape a lander adopts.
	pushBranch := func(t *testing.T, f *wtFixture) string {
		t.Helper()
		gitCmd(t, f.home, f.origin, "branch", branch, "main")
		gitCmd(t, f.home, f.origin, "checkout", "-q", branch)
		gitCmd(t, f.home, f.origin, "commit", "-q", "--allow-empty", "-m", "claim work")
		sha := gitCmd(t, f.home, f.origin, "rev-parse", branch)
		gitCmd(t, f.home, f.origin, "checkout", "-q", "main")
		return sha
	}

	t.Run("no local branch forks from origin and aligns", func(t *testing.T) {
		f := newWtFixture(t)
		want := pushBranch(t, f) // pushed AFTER the clone — the fetch must see it
		wt := filepath.Join(f.wtRoot, "repo-lander-7")
		if err := f.eng.AddWorktreeExisting(t.Context(), f.bare, wt, branch, f.env); err != nil {
			t.Fatalf("AddWorktreeExisting: %v", err)
		}
		if got := gitCmd(t, f.home, wt, "rev-parse", "HEAD"); got != want {
			t.Errorf("worktree HEAD = %s; want the freshly-fetched origin/%s tip %s", got, branch, want)
		}
		if got := gitCmd(t, f.home, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
			t.Errorf("worktree on %q; want refs/heads/%s", got, branch)
		}
		if !f.branchExists(branch) {
			t.Errorf("local branch %s not created", branch)
		}
	})

	t.Run("existing local branch is checked out where it stands", func(t *testing.T) {
		f := newWtFixture(t)
		want := pushBranch(t, f)
		gitCmd(t, f.home, f.bare, "fetch", "-q", "origin")
		gitCmd(t, f.home, f.bare, "branch", branch, "origin/"+branch)
		wt := filepath.Join(f.wtRoot, "repo-lander-7")
		if err := f.eng.AddWorktreeExisting(t.Context(), f.bare, wt, branch, f.env); err != nil {
			t.Fatalf("AddWorktreeExisting: %v", err)
		}
		if got := gitCmd(t, f.home, wt, "rev-parse", "HEAD"); got != want {
			t.Errorf("worktree HEAD = %s; want origin/%s tip %s", got, branch, want)
		}
		if got := gitCmd(t, f.home, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
			t.Errorf("worktree on %q; want refs/heads/%s", got, branch)
		}
	})

	t.Run("branch checked out elsewhere is a clear error", func(t *testing.T) {
		f := newWtFixture(t)
		pushBranch(t, f)
		gitCmd(t, f.home, f.bare, "fetch", "-q", "origin")
		parked := filepath.Join(f.wtRoot, "repo-7")
		gitCmd(t, f.home, f.bare, "worktree", "add", "-q", "--track", "-b", branch, parked, "origin/"+branch)
		wt := filepath.Join(f.wtRoot, "repo-lander-7")
		err := f.eng.AddWorktreeExisting(t.Context(), f.bare, wt, branch, f.env)
		if err == nil {
			t.Fatal("AddWorktreeExisting adopted a branch another worktree has checked out")
		}
		if !strings.Contains(err.Error(), "already checked out at "+parked) {
			t.Errorf("error = %v; want the parked worktree named", err)
		}
		if dirExists(wt) {
			t.Errorf("failed adopt left worktree dir %s behind", wt)
		}
	})

	t.Run("missing remote branch is a clear error", func(t *testing.T) {
		f := newWtFixture(t)
		wt := filepath.Join(f.wtRoot, "repo-lander-7")
		err := f.eng.AddWorktreeExisting(t.Context(), f.bare, wt, branch, f.env)
		if err == nil {
			t.Fatal("AddWorktreeExisting succeeded without origin/" + branch)
		}
		if !strings.Contains(err.Error(), "origin/"+branch+" does not exist after fetch") {
			t.Errorf("error = %v; want the missing origin/%s named", err, branch)
		}
		if dirExists(wt) || f.branchExists(branch) {
			t.Error("failed adopt left a worktree or branch behind")
		}
	})

	t.Run("local branch ahead of origin is hard-reset to origin", func(t *testing.T) {
		f := newWtFixture(t)
		want := pushBranch(t, f)
		gitCmd(t, f.home, f.bare, "fetch", "-q", "origin")
		// The local branch drifts one commit past origin/<branch> (a stale
		// parked claim): the lander must validate what the forge sees, so the
		// adopt hard-resets it back.
		drift := filepath.Join(f.wtRoot, "repo-drift")
		gitCmd(t, f.home, f.bare, "worktree", "add", "-q", "--track", "-b", branch, drift, "origin/"+branch)
		gitCmd(t, f.home, drift, "commit", "-q", "--allow-empty", "-m", "local drift")
		gitCmd(t, f.home, f.bare, "worktree", "remove", "--force", drift)

		wt := filepath.Join(f.wtRoot, "repo-lander-7")
		if err := f.eng.AddWorktreeExisting(t.Context(), f.bare, wt, branch, f.env); err != nil {
			t.Fatalf("AddWorktreeExisting: %v", err)
		}
		if got := gitCmd(t, f.home, wt, "rev-parse", "HEAD"); got != want {
			t.Errorf("worktree HEAD = %s; want origin/%s tip %s (local drift must be reset away)", got, branch, want)
		}
	})
}

// BranchMerged's exit-code trichotomy: exit 0 → (true,nil); exit 1 →
// (false,nil), a definite "keep", not an error; any other failure (here an
// unresolvable ref, exit 128) → (false, err) the caller treats
// conservatively.
func TestBranchMerged_trichotomy(t *testing.T) {
	f := newWtFixture(t)
	gitCmd(t, f.home, f.bare, "branch", "merged", "origin/main")
	if ok, err := f.eng.BranchMerged(t.Context(), f.bare, "merged", "main", f.env); err != nil || !ok {
		t.Errorf("BranchMerged(merged) = (%v,%v); want (true,nil)", ok, err)
	}

	wt := filepath.Join(f.wtRoot, "repo-ahead")
	gitCmd(t, f.home, f.bare, "worktree", "add", "-q", "-b", "lab/ahead", wt, "origin/main")
	gitCmd(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "ahead")
	if ok, err := f.eng.BranchMerged(t.Context(), f.bare, "lab/ahead", "main", f.env); err != nil || ok {
		t.Errorf("BranchMerged(ahead) = (%v,%v); want (false,nil)", ok, err)
	}

	ok, err := f.eng.BranchMerged(t.Context(), f.bare, "nosuchbranch", "main", f.env)
	if err == nil || ok {
		t.Errorf("BranchMerged(missing ref) = (%v,%v); want (false, error)", ok, err)
	}
	if err != nil && !strings.Contains(err.Error(), "merge-base --is-ancestor") {
		t.Errorf("BranchMerged error = %v; want the merge-base argv in the message", err)
	}
}

// A worktree freshly forked from origin/<default> reads CLEAN and MERGED —
// the race-guard rationale: the merged/dirty checks can never protect an
// in-flight Start, only ownership (the starting-set ∪ live sessions) can.
func TestFreshForkReadsCleanAndMerged(t *testing.T) {
	f := newWtFixture(t)
	const branch = "lab/fresh-20260608-1530"
	wt := filepath.Join(f.wtRoot, "repo-fresh-20260608-1530")
	if err := f.eng.AddWorktree(t.Context(), f.bare, wt, branch, "main", f.env); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if dirty, err := f.eng.WorktreeDirty(t.Context(), wt, f.env); err != nil || dirty {
		t.Errorf("WorktreeDirty(fresh fork) = (%v,%v); want (false,nil)", dirty, err)
	}
	if merged, err := f.eng.BranchMerged(t.Context(), f.bare, branch, "main", f.env); err != nil || !merged {
		t.Errorf("BranchMerged(fresh fork) = (%v,%v); want (true,nil) — a fresh fork trivially reads merged", merged, err)
	}
}

// The guarded-teardown decision table end-to-end against real repos
// (port-spec §2.1 end-to-end cells): the four dirty/merged rows, the
// conservative keep-everything on an unreadable status, and the
// skip-branch-delete-when-worktree-removal-fails rule. TeardownGuarded
// never fails the caller; its return reports what actually happened.
func TestTeardownGuarded_realGitMatrix(t *testing.T) {
	const branch = "lab/x-20260608-1530"

	// setup returns a fixture with a worktree on branch, forked fresh (so
	// clean+merged by default); rows mutate from there.
	setup := func(t *testing.T) (*wtFixture, string) {
		f := newWtFixture(t)
		wt := filepath.Join(f.wtRoot, "repo-x-20260608-1530")
		if err := f.eng.AddWorktree(t.Context(), f.bare, wt, branch, "main", f.env); err != nil {
			t.Fatalf("AddWorktree: %v", err)
		}
		return f, wt
	}

	t.Run("clean and merged removes both", func(t *testing.T) {
		f, wt := setup(t)
		act := f.eng.TeardownGuarded(t.Context(), discardLogger(), f.bare, wt, branch, "main", f.env)
		if !act.RemoveWorktree || !act.DeleteBranch {
			t.Errorf("action = %+v; want both performed", act)
		}
		if dirExists(wt) {
			t.Errorf("worktree %s still exists; want removed", wt)
		}
		if f.branchExists(branch) {
			t.Errorf("branch %s still exists; want deleted", branch)
		}
	})

	t.Run("clean and unmerged removes worktree keeps branch", func(t *testing.T) {
		f, wt := setup(t)
		gitCmd(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "unmerged work")
		act := f.eng.TeardownGuarded(t.Context(), discardLogger(), f.bare, wt, branch, "main", f.env)
		if !act.RemoveWorktree || act.DeleteBranch {
			t.Errorf("action = %+v; want worktree removed, branch kept", act)
		}
		if dirExists(wt) {
			t.Errorf("worktree %s still exists; want removed", wt)
		}
		if !f.branchExists(branch) {
			t.Errorf("branch %s deleted; unmerged commits must survive", branch)
		}
	})

	t.Run("dirty unmerged keeps both", func(t *testing.T) {
		f, wt := setup(t)
		gitCmd(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "unmerged work")
		if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		act := f.eng.TeardownGuarded(t.Context(), discardLogger(), f.bare, wt, branch, "main", f.env)
		if act.RemoveWorktree || act.DeleteBranch {
			t.Errorf("action = %+v; want nothing performed", act)
		}
		if !dirExists(wt) || !f.branchExists(branch) {
			t.Error("dirty teardown destroyed worktree or branch; want both kept")
		}
	})

	t.Run("dirty wins even when merged", func(t *testing.T) {
		f, wt := setup(t)
		// Untracked file only: the branch still reads merged, but dirty is
		// checked first and the merged check is skipped entirely.
		if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		act := f.eng.TeardownGuarded(t.Context(), discardLogger(), f.bare, wt, branch, "main", f.env)
		if act.RemoveWorktree || act.DeleteBranch {
			t.Errorf("action = %+v; want nothing performed", act)
		}
		if !dirExists(wt) || !f.branchExists(branch) {
			t.Error("dirty teardown destroyed worktree or branch; want both kept")
		}
	})

	t.Run("unreadable status keeps everything", func(t *testing.T) {
		f, wt := setup(t)
		// Nuke the worktree dir out from under git: status fails, and the
		// conservative rule keeps worktree record AND branch untouched.
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		act := f.eng.TeardownGuarded(t.Context(), discardLogger(), f.bare, wt, branch, "main", f.env)
		if act.RemoveWorktree || act.DeleteBranch {
			t.Errorf("action = %+v; want nothing performed on a status error", act)
		}
		if !f.branchExists(branch) {
			t.Errorf("branch %s deleted despite an unreadable status; want kept", branch)
		}
	})

	t.Run("remove failure skips branch delete", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root — permission-based failure injection does not apply")
		}
		f, wt := setup(t)
		// A read-only worktrees root makes `git worktree remove` fail on a
		// clean+merged tree; the branch delete must then be SKIPPED.
		if err := os.Chmod(f.wtRoot, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(f.wtRoot, 0o755) })
		act := f.eng.TeardownGuarded(t.Context(), discardLogger(), f.bare, wt, branch, "main", f.env)
		if act.RemoveWorktree || act.DeleteBranch {
			t.Errorf("action = %+v; want nothing performed when removal fails", act)
		}
		if !f.branchExists(branch) {
			t.Errorf("branch %s deleted after a failed worktree removal; want kept", branch)
		}
	})
}

// Worktrees parses `git worktree list --porcelain` on real output: the
// bare reference clone's own row carries Branch "" (skipped by
// reconciliation), linked worktrees carry their short branch names, and a
// detached worktree is present with Branch "".
func TestWorktrees_realGitParser(t *testing.T) {
	f := newWtFixture(t)
	lab := filepath.Join(f.wtRoot, "repo-foo-20260608-1530")
	gitCmd(t, f.home, f.bare, "worktree", "add", "-q", "-b", "lab/foo-20260608-1530", lab, "origin/main")
	afk := filepath.Join(f.wtRoot, "repo-7")
	gitCmd(t, f.home, f.bare, "worktree", "add", "-q", "-b", "afk/7", afk, "origin/main")
	det := filepath.Join(f.wtRoot, "repo-detached")
	gitCmd(t, f.home, f.bare, "worktree", "add", "-q", "--detach", det, "origin/main")

	got, err := f.eng.Worktrees(t.Context(), f.bare, f.env)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	byPath := map[string]string{}
	for _, wt := range got {
		byPath[wt.Path] = wt.Branch
	}
	if b, ok := byPath[f.bare]; !ok || b != "" {
		t.Errorf("bare reference entry = (%q, present=%v); want present with an empty branch", b, ok)
	}
	if byPath[lab] != "lab/foo-20260608-1530" {
		t.Errorf("lab worktree branch = %q; want lab/foo-20260608-1530", byPath[lab])
	}
	if byPath[afk] != "afk/7" {
		t.Errorf("afk worktree branch = %q; want afk/7", byPath[afk])
	}
	if b, ok := byPath[det]; !ok || b != "" {
		t.Errorf("detached worktree = (%q, present=%v); want present with an empty branch", b, ok)
	}
}

// Branches enumerates raw short names under prefix globs (design §4a: the
// glob is mandatory so dash prefixes like "issue-" work); ManagedBranches
// filters them through the strict per-repo pattern classification.
func TestBranchesAndManagedBranches_realGit(t *testing.T) {
	f := newWtFixture(t)
	for _, b := range []string{"lab/a-1", "lab/b-2", "afk/7", "afk/12", "feature/x"} {
		gitCmd(t, f.home, f.bare, "branch", b)
	}

	got, err := f.eng.Branches(t.Context(), f.bare, f.env, "lab/", "afk/")
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	sort.Strings(got)
	want := []string{"afk/12", "afk/7", "lab/a-1", "lab/b-2"} // feature/x + main excluded
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Branches(lab/, afk/) = %v; want %v", got, want)
	}

	// No matches → nil slice, no error.
	if none, err := f.eng.Branches(t.Context(), f.bare, f.env, "nope/"); err != nil || len(none) != 0 {
		t.Errorf("Branches(nope/) = (%v,%v); want (nil,nil)", none, err)
	}

	// Decoys: a non-rendering under the afk prefix and the incogni-shaped
	// namespaces.
	for _, b := range []string{"afk/notanumber", "issue-3", "issue-x", "wip/z"} {
		gitCmd(t, f.home, f.bare, "branch", b)
	}

	// The dash-prefix glob: a bare "refs/heads/issue-" would match nothing.
	got, err = f.eng.Branches(t.Context(), f.bare, f.env, "issue-")
	if err != nil {
		t.Fatalf("Branches(issue-): %v", err)
	}
	sort.Strings(got)
	if want := []string{"issue-3", "issue-x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Branches(issue-) = %v; want %v (raw, unparsed)", got, want)
	}

	// ManagedBranches: strict inverse parse drops afk/notanumber and
	// issue-x; the manual prefix is a plain namespace.
	got, err = f.eng.ManagedBranches(t.Context(), f.bare, "afk/<N>", "lab/", f.env)
	if err != nil {
		t.Fatalf("ManagedBranches(afk/<N>, lab/): %v", err)
	}
	sort.Strings(got)
	if want := []string{"afk/12", "afk/7", "lab/a-1", "lab/b-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ManagedBranches(afk/<N>, lab/) = %v; want %v", got, want)
	}
	got, err = f.eng.ManagedBranches(t.Context(), f.bare, "issue-<N>", "wip/", f.env)
	if err != nil {
		t.Fatalf("ManagedBranches(issue-<N>, wip/): %v", err)
	}
	sort.Strings(got)
	if want := []string{"issue-3", "wip/z"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ManagedBranches(issue-<N>, wip/) = %v; want %v", got, want)
	}
}

// CommitsAhead / UnpushedCount matrix (v0 TestRealGitParkedStats): the
// load-bearing case is the never-pushed/pushed split — a branch with no
// origin/<branch> ref counts every ahead commit as unpushed, while a
// pushed-then-advanced branch counts only the commits past its remote tip.
func TestCommitsAheadUnpushed_realGitMatrix(t *testing.T) {
	f := newWtFixture(t)

	const branch = "lab/ahead-20260608-1530"
	wt := filepath.Join(f.wtRoot, "repo-ahead-20260608-1530")
	gitCmd(t, f.home, f.bare, "worktree", "add", "-q", "-b", branch, wt, "origin/main")
	gitCmd(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "c1")
	gitCmd(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "c2")

	// Never pushed: ahead == unpushed == 2 (no origin/<branch>, so every
	// ahead commit is unpushed — the common parked case).
	if n, err := f.eng.CommitsAhead(t.Context(), f.bare, branch, "main", f.env); err != nil || n != 2 {
		t.Errorf("CommitsAhead(never-pushed) = (%d,%v); want (2,nil)", n, err)
	}
	if n, err := f.eng.UnpushedCount(t.Context(), f.bare, branch, "main", f.env); err != nil || n != 2 {
		t.Errorf("UnpushedCount(never-pushed) = (%d,%v); want (2,nil) — equals ahead", n, err)
	}

	// Push (origin/<branch> = c2), add one more local commit (tip = c3),
	// refresh the remote-tracking ref: now 3 ahead of mainline, 1 unpushed.
	gitCmd(t, f.home, wt, "push", "-q", "-u", "origin", branch)
	gitCmd(t, f.home, wt, "commit", "-q", "--allow-empty", "-m", "c3")
	gitCmd(t, f.home, f.bare, "fetch", "-q", "origin")
	if n, err := f.eng.CommitsAhead(t.Context(), f.bare, branch, "main", f.env); err != nil || n != 3 {
		t.Errorf("CommitsAhead(pushed then +1) = (%d,%v); want (3,nil)", n, err)
	}
	if n, err := f.eng.UnpushedCount(t.Context(), f.bare, branch, "main", f.env); err != nil || n != 1 {
		t.Errorf("UnpushedCount(pushed then +1) = (%d,%v); want (1,nil)", n, err)
	}

	// A branch pointing exactly at origin/main is 0 ahead and 0 unpushed
	// (an empty rev-list range prints "0" — plain zero, never an error).
	const base = "lab/base-20260608-1530"
	gitCmd(t, f.home, f.bare, "branch", base, "origin/main")
	if n, err := f.eng.CommitsAhead(t.Context(), f.bare, base, "main", f.env); err != nil || n != 0 {
		t.Errorf("CommitsAhead(at origin/main) = (%d,%v); want (0,nil)", n, err)
	}
	if n, err := f.eng.UnpushedCount(t.Context(), f.bare, base, "main", f.env); err != nil || n != 0 {
		t.Errorf("UnpushedCount(at origin/main) = (%d,%v); want (0,nil)", n, err)
	}
}
