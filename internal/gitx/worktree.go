package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The worktree lifecycle (port of v0 git.go — the git-worktrees port-spec is
// the behavioral contract): every instance runs in its own worktree on its
// own branch, forked from freshly-fetched origin/<default> of the lab-owned
// bare reference clone — never the reference repo's HEAD, never a shared
// tree. RemoveWorktree and DeleteBranch are separate on purpose:
// keep-worktree/keep-branch and remove-worktree/keep-branch are both real
// teardown outcomes. All destructive decisions route through the one
// guarded rule in teardown.go.

// Worktree is one entry of `git worktree list`: the on-disk path and the
// branch it has checked out (short name, e.g. "lab/foo-20260608-1530" or
// "afk/7"). Branch is "" for a detached or bare entry — including the bare
// reference repo's own row — which reconciliation ignores (it only ever
// acts on managed branches, never a human's checkout).
type Worktree struct {
	Path   string
	Branch string
}

// AddWorktree creates a worktree at path on new branch, forked from the
// freshly-fetched default branch of origin — NOT the reference repo's
// current HEAD. Exact sequence (v0-pinned): `git fetch origin` fail-loud →
// `git remote set-head origin --auto` best-effort → MkdirAll of the
// worktrees parent → `git worktree add -b <branch> <path> origin/<base>`.
// A repo with no usable origin (no remote, or origin/<base> unresolvable)
// fails here by design — there is NO fallback base. The fetch is the one
// network op; gitTimeout bounds it so a stalled remote fails a synchronous
// Start loudly instead of hanging it. Failure creates nothing the caller
// must roll back; any later Start-sequence failure rolls back with the
// separate RemoveWorktree + DeleteBranch helpers.
func (e *Engine) AddWorktree(ctx context.Context, bareDir, path, branch, base string, extraEnv []string) error {
	if err := e.Fetch(ctx, bareDir, extraEnv); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir worktrees root: %w", err)
	}
	_, err := e.run(ctx, bareDir, extraEnv, "worktree", "add", "-b", branch, path, "origin/"+base)
	return err
}

// AddWorktreeExisting creates a DETACHED worktree at path on origin/<branch>
// — the lander run's adopt checkout (issue #181 / ADR-0048), the one
// deliberate counterpart to AddWorktree's fresh fork. Same fail-loud fetch,
// then: origin/<branch> must exist (a vanished PR head is a clear error, not
// a fallback), and the worktree is checked out DETACHED at that remote tip.
//
// Detached is load-bearing, not incidental. The lander only needs the local
// branch REF for its one committing step, and it pushes that explicitly
// (HEAD:refs/heads/<branch>), so never holding the ref buys two properties:
//
//   - A parked claim worktree — an AFK run whose dirty teardown kept its
//     worktree, or an operator Stop — no longer blocks the adopt. Both want
//     the same afk/<N>; only one may hold it, and git refuses the second.
//     Claiming it here wedged the poller into a hot un-backed-off retry
//     (spawn → adopt fails → log → respawn next tick, forever).
//   - Committed-but-unpushed work on the claim branch survives. Checking the
//     branch out and hard-resetting it to origin/<branch> moved the REF, so a
//     success-path reap that parked unpushed commits (see RemoveWorktree)
//     lost them silently. A detached checkout cannot move a branch.
//
// The lander still validates exactly what the forge sees — that was the point
// of the reset, and origin/<branch> is what it now checks out directly.
func (e *Engine) AddWorktreeExisting(ctx context.Context, bareDir, path, branch string, extraEnv []string) error {
	if err := e.Fetch(ctx, bareDir, extraEnv); err != nil {
		return err
	}
	remote := "origin/" + branch
	if !e.refExists(ctx, bareDir, "refs/remotes/"+remote, extraEnv) {
		return fmt.Errorf("adopt branch %q: %s does not exist after fetch", branch, remote)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir worktrees root: %w", err)
	}
	if _, err := e.run(ctx, bareDir, extraEnv, "worktree", "add", "--detach", path, remote); err != nil {
		return err
	}
	return nil
}

// RemoveWorktree removes the worktree at path (`git worktree remove
// --force`). --force handles a tree that still has changes git considers
// worth guarding: the guarded teardown only reaches it clean, but a
// success-path reap may carry committed-but-unpushed work, and a Start
// rollback must always win. The branch is left intact — DeleteBranch is
// the separate, deliberate step.
func (e *Engine) RemoveWorktree(ctx context.Context, bareDir, path string, extraEnv []string) error {
	_, err := e.run(ctx, bareDir, extraEnv, "worktree", "remove", "--force", path)
	return err
}

// DeleteBranch force-deletes a local branch (`git branch -D`, since a
// rolled-back Start's branch has no upstream and an unmerged branch is only
// ever deleted on the caller's explicit decision). The guarded teardown
// calls this only for a clean, already-merged branch; a failed Start's
// rollback calls it to restore the exact pre-Start state.
func (e *Engine) DeleteBranch(ctx context.Context, bareDir, branch string, extraEnv []string) error {
	_, err := e.run(ctx, bareDir, extraEnv, "branch", "-D", branch)
	return err
}

// WorktreeDirty reports whether the worktree at path has uncommitted
// changes — tracked OR untracked — a non-empty `git status --porcelain`.
// Run with the worktree as cwd so it reflects that tree's own index, not
// the reference repo's. A dirty worktree is kept whole by the guarded
// teardown so unsaved work is never destroyed.
func (e *Engine) WorktreeDirty(ctx context.Context, path string, extraEnv []string) (bool, error) {
	out, err := e.run(ctx, path, extraEnv, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// BranchMerged reports whether branch is contained in the LOCAL
// origin/<defaultBranch> ref — every commit already on published mainline,
// so deleting the branch loses nothing. `git merge-base --is-ancestor`
// exits 0 when branch is an ancestor (merged, or a clean never-advanced
// fork point — see the starting-set guard), 1 when it carries unmerged
// commits (a definite "keep", not an error), and anything else is a real
// error the caller treats conservatively. Freshness of the local ref is
// the caller's business: the runtime sweep Fetches first, startup
// reconciliation reads the last-known ref.
func (e *Engine) BranchMerged(ctx context.Context, bareDir, branch, defaultBranch string, extraEnv []string) (bool, error) {
	base := "origin/" + defaultBranch
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "merge-base", "--is-ancestor", branch, base)
	cmd.Dir = bareDir
	cmd.Env = append(e.baseEnv(), extraEnv...)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil // definitively not an ancestor — unmerged commits
	}
	if ctx.Err() == context.DeadlineExceeded {
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: timed out after %s", branch, base, e.timeout)
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", branch, base, err)
}

// Worktrees enumerates the reference repo's worktrees with the branch each
// has checked out, parsed from `git worktree list --porcelain`. The blank
// line between porcelain records is implicit: a new "worktree " line
// flushes the previous record. A detached or bare entry (the bare
// reference repo itself is always the first row) carries no "branch "
// line, so its Branch stays "" and reconciliation skips it.
func (e *Engine) Worktrees(ctx context.Context, bareDir string, extraEnv []string) ([]Worktree, error) {
	out, err := e.run(ctx, bareDir, extraEnv, "worktree", "list", "--porcelain")
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

// Branches lists the short names of every local branch under the given
// refs/heads/ prefixes (e.g. "lab/", "afk/", "issue-") — one read-only
// `git for-each-ref` with a glob per prefix (design §4a: the glob is
// mandatory — a bare "refs/heads/issue-" pattern matches nothing). Names
// are returned raw and unparsed, in git's deterministic refname order; the
// caller decides what each one means. No matches yields a nil slice and no
// error.
func (e *Engine) Branches(ctx context.Context, bareDir string, extraEnv []string, prefixes ...string) ([]string, error) {
	args := []string{"for-each-ref", "--format", "%(refname:short)"}
	for _, p := range prefixes {
		args = append(args, "refs/heads/"+p+"*")
	}
	out, err := e.run(ctx, bareDir, extraEnv, args...)
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

// ManagedBranch reports whether branch belongs to lab under the repo's two
// configured patterns: an exact rendering of afkPattern (strict inverse
// parse — a stray "afk/notanumber" under the prefix is NOT managed, v0
// parseAFKBranch's ignore-non-matching rule) or any branch under the
// literal manualPrefix. Reconciliation and the sweep act ONLY on managed
// branches — never a human's branch or the default-branch checkout.
// Nothing outside per-repo config may assume the literal "afk/" or "lab/".
func ManagedBranch(afkPattern, manualPrefix, branch string) bool {
	if _, ok := ParseBranch(afkPattern, branch); ok {
		return true
	}
	return branch != "" && strings.HasPrefix(branch, manualPrefix)
}

// ManagedBranches lists the repo's managed branches: everything under the
// afk pattern's prefix and the manual prefix, filtered through
// ManagedBranch (so a non-rendering under the afk prefix never registers).
// Git's deterministic refname order is preserved for the sweeps.
func (e *Engine) ManagedBranches(ctx context.Context, bareDir, afkPattern, manualPrefix string, extraEnv []string) ([]string, error) {
	names, err := e.Branches(ctx, bareDir, extraEnv, PatternPrefix(afkPattern), manualPrefix)
	if err != nil {
		return nil, err
	}
	managed := names[:0]
	for _, name := range names {
		if ManagedBranch(afkPattern, manualPrefix, name) {
			managed = append(managed, name)
		}
	}
	if len(managed) == 0 {
		return nil, nil
	}
	return managed, nil
}

// CommitsAhead counts the commits branch carries beyond the LOCAL
// origin/<defaultBranch> ref — `git rev-list --count origin/<def>..<br>` —
// the "N ahead of mainline" the Parked view shows per entry. A stale ref
// just yields a stale count until the next sweep Fetch refreshes it.
func (e *Engine) CommitsAhead(ctx context.Context, bareDir, branch, defaultBranch string, extraEnv []string) (int, error) {
	return e.revListCount(ctx, bareDir, "origin/"+defaultBranch+".."+branch, extraEnv)
}

// CommitsBehind counts the commits the LOCAL origin/<defaultBranch> ref
// carries that branch does not — `git rev-list --count <br>..origin/<def>` —
// the "N behind mainline" mirror of CommitsAhead, the number the pull-base
// flow shows before offering to pull. Like CommitsAhead it reads the local
// remote-tracking ref and never fetches: freshness is the caller's business,
// and a stale ref just yields a stale count until the next Fetch refreshes
// it.
func (e *Engine) CommitsBehind(ctx context.Context, bareDir, branch, defaultBranch string, extraEnv []string) (int, error) {
	return e.revListCount(ctx, bareDir, branch+"..origin/"+defaultBranch, extraEnv)
}

// UnpushedCount counts the commits on branch not yet on its origin
// counterpart — the commits a Discard would destroy unrecoverably. When
// refs/remotes/origin/<branch> resolves it is `origin/<br>..<br>`; a
// never-pushed branch has no upstream to diff against, so every commit it
// carries beyond origin/<defaultBranch> is unpushed — exactly its
// commits-ahead count — which keeps the warning meaningful for the common
// parked case instead of erroring on the missing ref.
func (e *Engine) UnpushedCount(ctx context.Context, bareDir, branch, defaultBranch string, extraEnv []string) (int, error) {
	base := "origin/" + branch
	if !e.refExists(ctx, bareDir, "refs/remotes/"+base, extraEnv) {
		base = "origin/" + defaultBranch
	}
	return e.revListCount(ctx, bareDir, base+".."+branch, extraEnv)
}

// revListCount runs `git rev-list --count <range>` and parses the integer
// it prints — the shared primitive behind CommitsAhead and UnpushedCount.
// An empty range prints "0", so the fully-merged / fully-pushed case is a
// plain 0, never an error.
func (e *Engine) revListCount(ctx context.Context, bareDir, rng string, extraEnv []string) (int, error) {
	out, err := e.run(ctx, bareDir, extraEnv, "rev-list", "--count", rng)
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

// refExists reports whether ref resolves in bareDir — a quiet `git
// rev-parse --verify`. UnpushedCount uses it to tell a pushed branch
// (origin/<branch> present) from a never-pushed one.
func (e *Engine) refExists(ctx context.Context, bareDir, ref string, extraEnv []string) bool {
	_, err := e.run(ctx, bareDir, extraEnv, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}
