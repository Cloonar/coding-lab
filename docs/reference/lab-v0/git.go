package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Git is the seam over the `git` CLI for lab's worktree-per-instance model
// (ADR-0017): read a project's origin URL (Forgejo detection), enumerate the
// local afk/<N> claim branches, create an isolated worktree on a fresh branch off
// freshly-fetched origin/<default>, ask whether a worktree is dirty or a branch
// is merged (the guarded-teardown inputs), remove a worktree, and delete a
// branch. The real implementation shells out; tests substitute a fake.
//
// AFKBranches is the AFK claim oracle (ADR-0013): an AFK run's claim *is* its
// local afk/<N> branch — a durable ref on disk that survives a lab restart and
// that no triage/human tracker-label edit can clobber — so issue selection skips
// any issue that already has one. lab therefore writes no claim label at all.
//
// WorktreeDirty + BranchMerged are the two facts the guarded teardown weighs
// (decideTeardown): a dirty worktree is kept whole; a clean one's worktree is
// removed and its branch deleted only when already merged into origin/<default>.
//
// RemoveWorktree and DeleteBranch are separate on purpose: keeping a worktree
// while keeping its branch, or removing the worktree while keeping the branch,
// are both real teardown outcomes — a parked/dirty run, or a clean-but-unmerged
// one.
//
// Fetch, Worktrees, and Branches drive the cleanup sweeps (ADR-0017 slice 2):
// Fetch refreshes origin/<default> so the runtime sweep's merged-check is current;
// Worktrees enumerates every worktree (with its branch) so a worktree with no live
// session can be spotted as an orphan; Branches lists the lab/ and afk/ branches
// so a merged one left behind after its run — including the afk/<N> branch lab used
// to keep forever — can be GC'd.
type Git interface {
	OriginURL(dir string) (string, error)
	AFKBranches(repoDir string) (map[int]bool, error)
	AddWorktree(repoDir, path, branch string) error
	RemoveWorktree(repoDir, path string) error
	DeleteBranch(repoDir, branch string) error
	WorktreeDirty(path string) (bool, error)
	BranchMerged(repoDir, branch string) (bool, error)
	Fetch(repoDir string) error
	Worktrees(repoDir string) ([]Worktree, error)
	Branches(repoDir string, prefixes ...string) ([]string, error)

	// CommitsAhead, UnpushedCount, and LastCommitTime are the Parked view's
	// read-only per-entry stats (ADR-0017 slice 3): how far a parked branch is
	// ahead of mainline, how many of those commits a Discard would destroy
	// unrecoverably (never pushed), and how old its tip is. None mutate anything —
	// they only feed the view the user weighs before discarding.
	CommitsAhead(repoDir, branch string) (int, error)
	UnpushedCount(repoDir, branch string) (int, error)
	LastCommitTime(repoDir, branch string) (time.Time, error)
}

// Worktree is one entry of `git worktree list`: the on-disk path and the branch
// it has checked out (short name, e.g. "lab/foo-20260608-1530" or "afk/7"). Branch
// is "" for a detached or bare worktree — including the reference repo's own
// checkout on the default branch — which reconciliation ignores (it only ever acts
// on lab/ and afk/ worktrees, never a human's checkout).
type Worktree struct {
	Path   string
	Branch string
}

// gitTimeout bounds every git subprocess (see realGit.run). It exists for the one
// network op — the `fetch origin` inside AddWorktree on a synchronous Start — so a
// stalled remote fails the request loudly instead of hanging it forever
// (ADR-0017). Local ops (status, merge-base, worktree, branch) finish in
// milliseconds and never approach it.
const gitTimeout = 60 * time.Second

type realGit struct {
	bin     string
	timeout time.Duration
}

func NewGit(bin string) *realGit { return &realGit{bin: bin, timeout: gitTimeout} }

func (g *realGit) run(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), g.timeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (g *realGit) OriginURL(dir string) (string, error) {
	out, err := g.run(dir, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// AFKBranches returns the set of issue numbers that have a local afk/<N> branch
// — lab's durable, restart-safe AFK claim record (ADR-0013). A single read-only
// `git for-each-ref` over the refs/heads/afk/ namespace; each match is parsed
// back to its issue number with parseAFKBranch, so a stray branch under the
// prefix that isn't afk/<number> is ignored. No afk/ branches yields an empty
// (non-nil) set, never an error.
func (g *realGit) AFKBranches(repoDir string) (map[int]bool, error) {
	out, err := g.run(repoDir, "for-each-ref", "--format", "%(refname:short)", "refs/heads/"+afkBranchPrefix)
	if err != nil {
		return nil, err
	}
	claimed := map[int]bool{}
	for _, name := range strings.Split(string(out), "\n") {
		if n, ok := parseAFKBranch(strings.TrimSpace(name)); ok {
			claimed[n] = true
		}
	}
	return claimed, nil
}

// Branches lists the short names of every local branch under the given refs/heads/
// prefixes (e.g. "lab/", "afk/") — a read-only `git for-each-ref`, the same pattern
// AFKBranches uses, but returning raw names across both namespaces so the cleanup
// sweeps can weigh each lab/ and afk/ branch (ADR-0017). No matching branches
// yields an empty (nil) slice and no error. Unlike AFKBranches it does not parse
// the names: the caller decides what each one means.
func (g *realGit) Branches(repoDir string, prefixes ...string) ([]string, error) {
	args := []string{"for-each-ref", "--format", "%(refname:short)"}
	for _, p := range prefixes {
		args = append(args, "refs/heads/"+p)
	}
	out, err := g.run(repoDir, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// AddWorktree creates a worktree at path on a new branch, forked from the
// freshly-fetched default branch of origin — NOT repoDir's current HEAD, so an
// instance always starts from published mainline regardless of what the main
// checkout happens to be sitting on. The worktrees parent directory is created
// if missing. The `fetch origin` is the one network op; gitTimeout bounds it so a
// stalled remote fails Start loudly instead of hanging it (ADR-0017). A repo with
// no usable origin (no remote, or origin/<default> unresolvable) fails here, by
// design — there is no fallback base.
func (g *realGit) AddWorktree(repoDir, path, branch string) error {
	if _, err := g.run(repoDir, "fetch", "origin"); err != nil {
		return err
	}
	// Refresh origin/HEAD so the default-branch lookup below is accurate; this
	// is best-effort (a clone may lack the ref until queried).
	_, _ = g.run(repoDir, "remote", "set-head", "origin", "--auto")
	base := g.defaultBranch(repoDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir worktrees root: %w", err)
	}
	_, err := g.run(repoDir, "worktree", "add", "-b", branch, path, "origin/"+base)
	return err
}

// defaultBranch returns origin's default branch (e.g. "main"), falling back to
// "main" when origin/HEAD isn't resolvable.
func (g *realGit) defaultBranch(repoDir string) string {
	out, err := g.run(repoDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "main"
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
}

// Fetch refreshes origin into the reference repo so a subsequent BranchMerged sees
// current published mainline — the network op the runtime sweep does before its
// merged-check (ADR-0017). It mirrors AddWorktree's fetch (the same network op,
// the same gitTimeout bound, the same best-effort origin/HEAD refresh so the
// default-branch lookup stays accurate), but stands alone so the sweep can refresh
// without creating a worktree. Callers treat it as best-effort: a fetch failure
// just leaves the merged-check reading the last-known origin ref.
func (g *realGit) Fetch(repoDir string) error {
	if _, err := g.run(repoDir, "fetch", "origin"); err != nil {
		return err
	}
	_, _ = g.run(repoDir, "remote", "set-head", "origin", "--auto")
	return nil
}

// WorktreeDirty reports whether the worktree at path has uncommitted changes —
// tracked or untracked — a non-empty `git status --porcelain`. Run with the
// worktree as cwd so it reflects that tree's own index, not the reference repo's.
// A dirty worktree is kept whole by the guarded teardown so unsaved work is never
// destroyed (ADR-0017).
func (g *realGit) WorktreeDirty(path string) (bool, error) {
	out, err := g.run(path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// BranchMerged reports whether branch is contained in origin's default branch —
// every commit already on published mainline, so deleting the branch loses
// nothing. `git merge-base --is-ancestor <branch> origin/<default>` exits 0 when
// branch is an ancestor (merged, or a clean never-advanced fork point), 1 when it
// carries unmerged commits, and >1 on a real error (e.g. an unresolvable ref) —
// distinguished here so a clean-but-unmerged branch (exit 1) is a definite "keep"
// and a broken lookup surfaces as an error the caller treats conservatively. The
// comparison is against the LOCAL origin/<default> ref; the runtime sweep refreshes
// it with Fetch before its merged-check (ADR-0017 slice 2), while startup
// reconciliation reads the last-known ref (refreshed at the most recent Start/sweep).
func (g *realGit) BranchMerged(repoDir, branch string) (bool, error) {
	base := "origin/" + g.defaultBranch(repoDir)
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.bin, "merge-base", "--is-ancestor", branch, base)
	cmd.Dir = repoDir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil // definitively not an ancestor — unmerged commits
	}
	if ctx.Err() == context.DeadlineExceeded {
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: timed out after %s", branch, base, g.timeout)
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", branch, base, err)
}

// RemoveWorktree removes the worktree at path. --force handles a tree that still
// has changes git considers worth guarding (the guarded teardown only calls this
// for a clean worktree, but a success-path AFK reap may carry committed-but-
// unpushed work). The branch is left intact, so the branch and any pushed commits
// survive the removal — DeleteBranch is the separate, deliberate step.
func (g *realGit) RemoveWorktree(repoDir, path string) error {
	_, err := g.run(repoDir, "worktree", "remove", "--force", path)
	return err
}

// Worktrees enumerates the reference repo's worktrees with the branch each has
// checked out, parsed from `git worktree list --porcelain` (ADR-0017). The blank
// line between porcelain records is implicit: a new "worktree " line flushes the
// previous record. A detached or bare worktree (including the reference repo's own
// checkout when it is on the default branch, and the first/bare entry) carries no
// "branch " line, so its Branch stays "" and reconciliation skips it — lab only
// acts on lab/ and afk/ worktrees, never a human's checkout.
func (g *realGit) Worktrees(repoDir string) ([]Worktree, error) {
	out, err := g.run(repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var (
		wts  []Worktree
		cur  Worktree
		have bool
	)
	flush := func() {
		if have {
			wts = append(wts, cur)
		}
		cur, have = Worktree{}, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.Path = strings.TrimPrefix(line, "worktree ")
			have = true
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return wts, nil
}

// DeleteBranch force-deletes a local branch (-D, since a rolled-back start's
// branch may carry no upstream and an unmerged branch is only ever deleted on the
// caller's explicit decision). The guarded teardown calls this only for a clean,
// already-merged branch; a failed start's rollback calls it to restore the exact
// pre-start state.
func (g *realGit) DeleteBranch(repoDir, branch string) error {
	_, err := g.run(repoDir, "branch", "-D", branch)
	return err
}

// --- Parked view read-only stats (ADR-0017 slice 3) -------------------------

// revListCount runs `git rev-list --count <range>` in repoDir and parses the
// integer it prints — the shared primitive behind CommitsAhead and UnpushedCount.
// An empty range (no commits) prints "0", so the common fully-merged / fully-
// pushed case is a plain 0, never an error.
func (g *realGit) revListCount(repoDir, rng string) (int, error) {
	out, err := g.run(repoDir, "rev-list", "--count", rng)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count %s: parse %q: %w", rng, s, err)
	}
	return n, nil
}

// CommitsAhead counts the commits a branch carries beyond origin's default branch
// — `git rev-list --count origin/<default>..<branch>` — the "N ahead of mainline"
// the Parked view shows per entry. The base is the LOCAL origin/<default> ref (the
// one BranchMerged reads); a stale ref just yields a stale count until the next
// sweep Fetch refreshes it, which the lazy view tolerates.
func (g *realGit) CommitsAhead(repoDir, branch string) (int, error) {
	return g.revListCount(repoDir, "origin/"+g.defaultBranch(repoDir)+".."+branch)
}

// UnpushedCount counts the commits on branch not yet on its origin counterpart —
// the commits a Discard would destroy unrecoverably, so the view can warn before
// nuking them. When origin/<branch> exists it is `origin/<branch>..<branch>`; a
// never-pushed branch (no such remote ref — every manual lab/<label> that never
// opened a PR) has no upstream to diff against, so every commit it carries beyond
// origin/<default> is unpushed, which is exactly its commits-ahead count. Reporting
// that keeps the unpushed warning meaningful for the common parked case instead of
// erroring on the missing ref.
func (g *realGit) UnpushedCount(repoDir, branch string) (int, error) {
	base := "origin/" + branch
	if !g.refExists(repoDir, "refs/remotes/"+base) {
		base = "origin/" + g.defaultBranch(repoDir)
	}
	return g.revListCount(repoDir, base+".."+branch)
}

// refExists reports whether ref resolves in repoDir — a quiet `git rev-parse
// --verify`, exit 0 when present. UnpushedCount uses it to tell a pushed branch
// (origin/<branch> present) from a never-pushed one.
func (g *realGit) refExists(repoDir, ref string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.bin, "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

// LastCommitTime returns branch's tip commit time (committer date) for the Parked
// view's age column — `git log -1 --format=%ct <branch>`, parsed from Unix
// seconds. The caller turns it into a human age against its own (pinnable) clock,
// so the age formatting stays testable without pinning git's output.
func (g *realGit) LastCommitTime(repoDir, branch string) (time.Time, error) {
	out, err := g.run(repoDir, "log", "-1", "--format=%ct", branch)
	if err != nil {
		return time.Time{}, err
	}
	s := strings.TrimSpace(string(out))
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("git log -1 --format=%%ct %s: parse %q: %w", branch, s, err)
	}
	return time.Unix(secs, 0), nil
}
