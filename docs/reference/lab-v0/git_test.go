package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// git_test.go drives realGit against a throwaway repo, the way sessions_test.go
// drives real tmux. It skips if git is unreachable on PATH.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH — skipping integration test")
	}
}

// mustGit runs a git command in dir, failing the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// gitOut runs a git command in dir and returns its trimmed stdout, failing on
// error — for the rev-parse assertions the worktree/fetch tests make.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// newCloneRepo stands up a bare origin (on main) plus a clone with origin/<default>
// resolved like production, returning the clone's path and its enclosing temp dir
// (a worktree parent). The single root commit is pushed to origin so origin/main
// exists. Shared by the Worktrees / Branches / Fetch integration tests.
func newCloneRepo(t *testing.T) (repo, parent string) {
	t.Helper()
	origin := t.TempDir()
	mustGit(t, origin, "init", "-q", "--bare", "-b", "main")
	seed := t.TempDir()
	mustGit(t, seed, "init", "-q", "-b", "main")
	mustGit(t, seed, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root")
	mustGit(t, seed, "remote", "add", "origin", origin)
	mustGit(t, seed, "push", "-q", "origin", "main")
	parent = t.TempDir()
	repo = filepath.Join(parent, "clone")
	mustGit(t, parent, "clone", "-q", origin, repo)
	return repo, parent
}

// Worktrees enumerates every worktree with the branch it has checked out: the
// reference repo's own checkout on the default branch, plus each linked worktree on
// its lab//afk/ branch. A detached worktree carries no branch (Branch ""), which
// reconciliation skips.
func TestRealGitWorktrees(t *testing.T) {
	requireGit(t)
	repo, parent := newCloneRepo(t)
	g := NewGit("git")

	lab := filepath.Join(parent, "wt-lab")
	mustGit(t, repo, "worktree", "add", "-q", "-b", "lab/foo-20260608-1530", lab, "origin/main")
	afk := filepath.Join(parent, "wt-afk")
	mustGit(t, repo, "worktree", "add", "-q", "-b", "afk/7", afk, "origin/main")
	det := filepath.Join(parent, "wt-detached")
	mustGit(t, repo, "worktree", "add", "-q", "--detach", det, "origin/main")

	got, err := g.Worktrees(repo)
	if err != nil {
		t.Fatalf("Worktrees: %v", err)
	}
	byPath := map[string]string{}
	for _, wt := range got {
		byPath[wt.Path] = wt.Branch
	}
	if byPath[repo] != "main" {
		t.Errorf("reference checkout branch = %q; want main", byPath[repo])
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

// Branches lists short names across the lab/ and afk/ namespaces, ignoring other
// branches (a feature branch, the default branch).
func TestRealGitBranches(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root")
	for _, b := range []string{"lab/a-1", "lab/b-2", "afk/7", "afk/12", "feature/x"} {
		mustGit(t, dir, "branch", b)
	}

	got, err := NewGit("git").Branches(dir, "lab/", "afk/")
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	sort.Strings(got)
	want := []string{"afk/12", "afk/7", "lab/a-1", "lab/b-2"} // feature/x + main excluded
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Branches(lab/, afk/) = %v; want %v", got, want)
	}
}

// Fetch advances the reference repo's origin/<default> to published mainline — the
// refresh the runtime sweep does before its merged-check.
func TestRealGitFetch(t *testing.T) {
	requireGit(t)
	repo, _ := newCloneRepo(t)
	// Advance origin/main beyond what the clone fetched at creation, from a second
	// clone so the bare origin moves under the reference repo's feet.
	mover := t.TempDir()
	mustGit(t, mover, "clone", "-q", repoOrigin(t, repo), "m")
	m := filepath.Join(mover, "m")
	mustGit(t, m, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "advance")
	mustGit(t, m, "push", "-q", "origin", "main")

	before := gitOut(t, repo, "rev-parse", "refs/remotes/origin/main")
	if err := NewGit("git").Fetch(repo); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	after := gitOut(t, repo, "rev-parse", "refs/remotes/origin/main")
	if before == after {
		t.Errorf("Fetch did not advance origin/main (still %s); want it updated", before[:8])
	}
}

// repoOrigin returns repo's origin URL, so a test can stand up a second clone of
// the same bare origin.
func repoOrigin(t *testing.T, repo string) string {
	t.Helper()
	return gitOut(t, repo, "remote", "get-url", "origin")
}

// AFKBranches is the claim oracle (ADR-0013): it returns exactly the issue
// numbers under refs/heads/afk/ that parse as afk/<number>, keyed by issue —
// ignoring unrelated branches and a non-numeric afk/* decoy.
func TestRealGitAFKBranches(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root")
	// Three real claim branches, plus decoys that must NOT register.
	for _, b := range []string{"afk/7", "afk/12", "afk/63", "feature/x", "afk/notanumber"} {
		mustGit(t, dir, "branch", b)
	}

	got, err := NewGit("git").AFKBranches(dir)
	if err != nil {
		t.Fatalf("AFKBranches: %v", err)
	}
	want := map[int]bool{7: true, 12: true, 63: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AFKBranches = %v; want %v (only afk/<number> branches, by issue)", got, want)
	}
}

// No afk/ branches at all yields an empty, non-nil set and no error, so selection
// treats every ready issue as claimable.
func TestRealGitAFKBranches_none(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")

	got, err := NewGit("git").AFKBranches(dir)
	if err != nil {
		t.Fatalf("AFKBranches: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("AFKBranches with no afk branches = %v; want an empty, non-nil set", got)
	}
}

// WorktreeDirty and BranchMerged are the two guarded-teardown inputs (ADR-0017),
// exercised here against a real clone with an origin/<default> ref: a branch at
// origin/main is merged, one a commit ahead is not; a fresh worktree is clean
// until a file is written into it.
func TestRealGitWorktreeDirtyAndBranchMerged(t *testing.T) {
	requireGit(t)
	// A bare origin + a clone, so origin/<default> resolves like production.
	origin := t.TempDir()
	mustGit(t, origin, "init", "-q", "--bare", "-b", "main")
	seed := t.TempDir()
	mustGit(t, seed, "init", "-q", "-b", "main")
	mustGit(t, seed, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root")
	mustGit(t, seed, "remote", "add", "origin", origin)
	mustGit(t, seed, "push", "-q", "origin", "main")

	parent := t.TempDir()
	repo := filepath.Join(parent, "clone")
	mustGit(t, parent, "clone", "-q", origin, repo)
	g := NewGit("git")

	// merged: a branch pointing at origin/main is contained in it.
	mustGit(t, repo, "branch", "merged", "origin/main")
	if ok, err := g.BranchMerged(repo, "merged"); err != nil || !ok {
		t.Errorf("BranchMerged(merged) = (%v,%v); want (true,nil)", ok, err)
	}
	// unmerged: a branch with a commit beyond origin/main is not.
	mustGit(t, repo, "checkout", "-q", "-b", "ahead", "origin/main")
	mustGit(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "ahead")
	if ok, err := g.BranchMerged(repo, "ahead"); err != nil || ok {
		t.Errorf("BranchMerged(ahead) = (%v,%v); want (false,nil)", ok, err)
	}

	// A clean worktree reports clean; a written file makes it dirty.
	wt := filepath.Join(parent, "wt")
	mustGit(t, repo, "worktree", "add", "-q", wt, "origin/main")
	if dirty, err := g.WorktreeDirty(wt); err != nil || dirty {
		t.Errorf("WorktreeDirty(clean) = (%v,%v); want (false,nil)", dirty, err)
	}
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirty, err := g.WorktreeDirty(wt); err != nil || !dirty {
		t.Errorf("WorktreeDirty(after write) = (%v,%v); want (true,nil)", dirty, err)
	}
}

// CommitsAhead / UnpushedCount / LastCommitTime are the Parked view's read-only
// stats (ADR-0017 slice 3), exercised here against a real clone with an
// origin/<default> ref. The load-bearing case is the never-pushed/pushed split:
// for a branch with no origin/<branch> ref every ahead commit is unpushed, while a
// pushed-then-advanced branch counts only the commits past its remote tip.
func TestRealGitParkedStats(t *testing.T) {
	requireGit(t)
	repo, parent := newCloneRepo(t)
	g := NewGit("git")

	const branch = "lab/ahead-20260608-1530"
	wt := filepath.Join(parent, "wt-ahead")
	mustGit(t, repo, "worktree", "add", "-q", "-b", branch, wt, "origin/main")
	mustGit(t, wt, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "c1")
	mustGit(t, wt, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "c2")

	// Never pushed: ahead == unpushed == 2 (no origin/<branch>, so every ahead
	// commit is unpushed — the common parked lab/ case).
	if n, err := g.CommitsAhead(repo, branch); err != nil || n != 2 {
		t.Errorf("CommitsAhead(never-pushed) = (%d,%v); want (2,nil)", n, err)
	}
	if n, err := g.UnpushedCount(repo, branch); err != nil || n != 2 {
		t.Errorf("UnpushedCount(never-pushed) = (%d,%v); want (2,nil) — equals ahead", n, err)
	}

	// Push (origin/<branch> = c2), add one more local commit (tip = c3), refresh the
	// reference repo's remote-tracking ref: now 3 ahead of mainline, 1 unpushed.
	mustGit(t, wt, "push", "-q", "-u", "origin", branch)
	mustGit(t, wt, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "c3")
	mustGit(t, repo, "fetch", "-q", "origin")
	if n, err := g.CommitsAhead(repo, branch); err != nil || n != 3 {
		t.Errorf("CommitsAhead(pushed then +1) = (%d,%v); want (3,nil)", n, err)
	}
	if n, err := g.UnpushedCount(repo, branch); err != nil || n != 1 {
		t.Errorf("UnpushedCount(pushed then +1) = (%d,%v); want (1,nil)", n, err)
	}

	// LastCommitTime is the tip's committer date — a fresh commit, so recent.
	ts, err := g.LastCommitTime(repo, branch)
	if err != nil {
		t.Fatalf("LastCommitTime: %v", err)
	}
	if d := time.Since(ts); d < 0 || d > time.Hour {
		t.Errorf("LastCommitTime age = %v; want a recent commit (0..1h)", d)
	}

	// A branch pointing exactly at origin/main is 0 ahead and 0 unpushed.
	const base = "lab/base-20260608-1530"
	mustGit(t, repo, "branch", base, "origin/main")
	if n, err := g.CommitsAhead(repo, base); err != nil || n != 0 {
		t.Errorf("CommitsAhead(at origin/main) = (%d,%v); want (0,nil)", n, err)
	}
	if n, err := g.UnpushedCount(repo, base); err != nil || n != 0 {
		t.Errorf("UnpushedCount(at origin/main) = (%d,%v); want (0,nil)", n, err)
	}
}
