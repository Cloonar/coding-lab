package gitx

// pull.go is the gitx surface for pulling the base branch into a run's LIVE
// worktree (the /pull-base flow): PullBase fetches the bare reference repo
// and merges the freshly-fetched origin/<base> into the branch the worktree
// has checked out — fast-forwarding when possible, aborting back to a
// byte-identical worktree on conflict — and SummarizeRange renders the
// digest of what a pull brought in (commit subjects + name-status file
// list). CommitsBehind (worktree.go) is the read-only counterpart that
// decides whether there is anything to pull. Unlike CRMerge this operates on
// a worktree someone is actively working in, so the hard invariant here is:
// a failed pull leaves the worktree exactly as it found it.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// ErrAuthorIdentityRequired marks a pull that would author a merge commit —
// the diverged, non-fast-forwardable path — attempted with an empty
// authorName/authorEmail. A fast-forward moves the ref without committing and
// needs no identity, so the ff-ness (unknowable before the fetch) is what
// decides: only a diverged pull is refused, and only after the fetch but
// BEFORE the merge touches the worktree, leaving it untouched (#151). The
// pull service re-flags this as its own ErrNoAuthorIdentity for the HTTP
// layer; it is the pull-time sibling of the CR-merge author-identity refusal.
var ErrAuthorIdentityRequired = errors.New("pull would author a merge commit but no author identity was given")

// PullResult describes one PullBase outcome. OldHead is the worktree's HEAD
// sha before the pull, NewHead after it. UpToDate means origin/<base> was
// already reachable from HEAD and NO merge was attempted (OldHead ==
// NewHead). FastForward means the merge moved HEAD to origin/<base> without
// creating a merge commit (NewHead is origin/<base>'s sha); when both flags
// are false the pull created a real merge commit with OldHead as ^1 and
// origin/<base> as ^2.
type PullResult struct {
	OldHead     string
	NewHead     string
	UpToDate    bool
	FastForward bool
}

// ConflictError is the typed failure of a PullBase whose merge conflicted:
// Files lists the conflicted paths (sorted), Report carries git's conflict
// report verbatim (merge-ort writes it to STDOUT) — the text the operator
// sees. By the time PullBase returns it the merge has been aborted and the
// worktree is byte-identical to its pre-pull state. It matches
// errors.Is(err, ErrMergeConflict), the same sentinel CRMerge conflicts wrap.
type ConflictError struct {
	Files  []string
	Report string
}

func (ce *ConflictError) Error() string {
	return ErrMergeConflict.Error() + ": " + ce.Report
}

// Is makes errors.Is(err, ErrMergeConflict) true for a *ConflictError, so
// callers classify pull conflicts and CR-merge conflicts with one sentinel.
func (ce *ConflictError) Is(target error) bool { return target == ErrMergeConflict }

// PullBase merges the CURRENT origin/<base> into the branch checked out in
// the run's worktree:
//
//  1. A fail-loud `git fetch origin` on the bare reference repo — the whole
//     point of a pull is origin's present state, never the last sweep's.
//  2. Resolve refs/remotes/origin/<base> (unresolvable after the fetch is an
//     error — same as AddWorktree, there is no fallback base) and the
//     worktree's HEAD.
//  3. If origin/<base> is already an ancestor of HEAD there is nothing to
//     pull: UpToDate=true, no merge attempted, the worktree untouched.
//  4. Otherwise `git merge --no-edit origin/<base>` runs IN the worktree
//     (the bare repo and its worktrees share refs, so the freshly-fetched
//     remote-tracking ref is visible there). Git fast-forwards on its own
//     exactly when HEAD is an ancestor of origin/<base>; that condition is
//     computed up front and reported as FastForward. A fast-forward authors
//     NO commit, so an empty authorName/authorEmail is fine on that path. A
//     DIVERGED pull authors a merge commit as authorName/authorEmail via
//     GIT_AUTHOR_*/GIT_COMMITTER_* entries appended AFTER extraEnv (last
//     duplicate wins) — the repo's configured real identity (D15 measure 5);
//     an empty identity there has nobody to author that commit, so a diverged
//     pull with an empty authorName/authorEmail is refused with
//     ErrAuthorIdentityRequired before the merge runs, the worktree untouched
//     (only the harmless bare-repo fetch happened) — the ff-ness that gates
//     this is unknowable before the fetch (#151).
//
// Failure contract — the worktree ends byte-identical to how the call found
// it: a CONFLICT collects the conflicted paths, aborts the merge under a
// cancellation-immune context, and returns *ConflictError (a failing abort
// is its own loud error naming the stranded worktree). Any other merge
// failure — e.g. git's "would be overwritten by merge" refusal for dirty
// files the merge touches, which fires BEFORE the merge starts — surfaces
// git's message verbatim, with a best-effort abort only if MERGE_HEAD exists
// (a pre-merge refusal leaves nothing to abort, and an unconditional abort
// would itself fail loudly). Uncommitted changes to files the merge does not
// touch survive the pull, exactly as they survive a plain `git merge`.
func (e *Engine) PullBase(ctx context.Context, bareDir, worktreePath, base, authorName, authorEmail string, extraEnv []string) (PullResult, error) {
	if err := e.Fetch(ctx, bareDir, extraEnv); err != nil {
		return PullResult{}, err
	}
	baseRef := "refs/remotes/origin/" + base
	baseSHA, err := e.refCommit(ctx, bareDir, baseRef, extraEnv)
	if err != nil {
		return PullResult{}, fmt.Errorf("base branch origin/%s does not resolve after fetch: %w", base, err)
	}
	out, err := e.run(ctx, worktreePath, extraEnv, "rev-parse", "HEAD")
	if err != nil {
		return PullResult{}, err
	}
	oldHead := strings.TrimSpace(string(out))

	upToDate, err := e.isAncestor(ctx, bareDir, baseSHA, oldHead, extraEnv)
	if err != nil {
		return PullResult{}, err
	}
	if upToDate {
		return PullResult{OldHead: oldHead, NewHead: oldHead, UpToDate: true}, nil
	}
	// Git's own fast-forward condition, computed before the merge so the
	// result can report which shape the pull took.
	ffable, err := e.isAncestor(ctx, bareDir, oldHead, baseSHA, extraEnv)
	if err != nil {
		return PullResult{}, err
	}
	// A fast-forward authors no commit; a diverged pull authors a merge commit
	// under the passed identity. Only the diverged path needs one, and ff-ness
	// is unknowable until here — so refuse an identity-free diverged pull now,
	// after the harmless bare-repo fetch but BEFORE runPullMerge touches the
	// worktree, leaving it untouched (#151).
	if !ffable && (authorName == "" || authorEmail == "") {
		return PullResult{}, ErrAuthorIdentityRequired
	}

	// Author entries come AFTER extraEnv so they win the os/exec
	// last-duplicate rule (the CRMerge identity convention).
	authEnv := append(slices.Clone(extraEnv),
		"GIT_AUTHOR_NAME="+authorName,
		"GIT_AUTHOR_EMAIL="+authorEmail,
		"GIT_COMMITTER_NAME="+authorName,
		"GIT_COMMITTER_EMAIL="+authorEmail,
	)
	if err := e.runPullMerge(ctx, worktreePath, base, authEnv); err != nil {
		return PullResult{}, err
	}
	out, err = e.run(ctx, worktreePath, extraEnv, "rev-parse", "HEAD")
	if err != nil {
		return PullResult{}, err
	}
	return PullResult{OldHead: oldHead, NewHead: strings.TrimSpace(string(out)), FastForward: ffable}, nil
}

// runPullMerge runs `git merge --no-edit origin/<base>` in the worktree,
// capturing BOTH output streams: merge-ort writes its conflict report to
// STDOUT (the runMerge lesson — the stderr-only shape of Engine.run would
// surface a conflict as a blank "exit status 1"), while the dirty-clobber
// refusal goes to stderr. Output carrying a CONFLICT marker takes the
// abort-and-type path (abortConflictedPull); any other failure keeps both
// streams verbatim in the error, aborting first only if a merge actually
// started (MERGE_HEAD exists — a pre-merge refusal has nothing to abort).
func (e *Engine) runPullMerge(ctx context.Context, worktreePath, base string, env []string) error {
	ref := "origin/" + base
	runCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, e.bin, "merge", "--no-edit", ref)
	cmd.Dir = worktreePath
	cmd.Env = append(e.baseEnv(), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("git merge --no-edit %s: timed out after %s", ref, e.timeout)
	}
	report := strings.TrimSpace(string(out))
	if strings.Contains(report, "CONFLICT") {
		return e.abortConflictedPull(ctx, worktreePath, report, env)
	}
	cleanCtx := context.WithoutCancel(ctx)
	if e.refExists(cleanCtx, worktreePath, "MERGE_HEAD", env) {
		_, _ = e.run(cleanCtx, worktreePath, env, "merge", "--abort")
	}
	return fmt.Errorf("git merge --no-edit %s: %v: %s", ref, err, report)
}

// abortConflictedPull restores the worktree after a conflicted pull merge:
// collect the conflicted paths (`git diff --name-only --diff-filter=U`,
// best-effort — the abort must run regardless), then `git merge --abort`,
// both under a cancellation-immune context (a cancelled pull must never
// strand the run's worktree mid-merge). A successful abort returns the typed
// *ConflictError; a FAILING abort is the one case the worktree-untouched
// contract breaks, so it returns its own loud error naming the stranded
// worktree instead of masquerading as an ordinary conflict.
func (e *Engine) abortConflictedPull(ctx context.Context, worktreePath, report string, env []string) error {
	cleanCtx := context.WithoutCancel(ctx)
	var files []string
	if out, err := e.run(cleanCtx, worktreePath, env, "diff", "--name-only", "--diff-filter=U"); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.TrimSpace(line); f != "" {
				files = append(files, f)
			}
		}
	}
	slices.Sort(files)
	if _, err := e.run(cleanCtx, worktreePath, env, "merge", "--abort"); err != nil {
		return fmt.Errorf("pull merge conflicted AND the abort failed — worktree %s is left mid-merge and needs manual repair: %w (conflict report: %s)", worktreePath, err, report)
	}
	return &ConflictError{Files: files, Report: report}
}

// RangeSummary is the digest of a commit range for the pull-base report:
// the newest commit subjects (capped by the caller) with the full commit
// count, and the changed files as name-status entries (capped) with the full
// file count — TotalCommits > len(Subjects) or TotalFiles > len(Files) is
// how a renderer knows to append "… and N more".
type RangeSummary struct {
	Subjects     []string
	TotalCommits int
	Files        []FileChange
	TotalFiles   int
}

// FileChange is one `git diff --name-status` line: Status as git emits it
// ("A", "M", "D", or a scored "R100"/"C75"), and Path — the file's path, or
// "old -> new" for a rename/copy line.
type FileChange struct {
	Status string
	Path   string
}

// SummarizeRange digests the commits in <from>..<to> for the pull-base
// report, running in the worktree (both endpoints of a pull resolve there —
// shas or the shared refs). Subjects come from `git log --format=%s -n
// <maxSubjects>` in git's order (newest first); TotalCommits is the full
// `rev-list --count` of the range. Files come from `git diff --name-status
// <from>..<to>` capped at maxFiles, with TotalFiles counting every line
// before the cap. An empty range is a valid zero summary, never an error.
func (e *Engine) SummarizeRange(ctx context.Context, worktreePath, from, to string, maxSubjects, maxFiles int, extraEnv []string) (RangeSummary, error) {
	var s RangeSummary
	rng := from + ".." + to

	out, err := e.run(ctx, worktreePath, extraEnv, "log", "--format=%s", "-n", strconv.Itoa(maxSubjects), rng)
	if err != nil {
		return RangeSummary{}, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" {
			s.Subjects = append(s.Subjects, line)
		}
	}
	if s.TotalCommits, err = e.revListCount(ctx, worktreePath, rng, extraEnv); err != nil {
		return RangeSummary{}, err
	}

	out, err = e.run(ctx, worktreePath, extraEnv, "diff", "--name-status", rng)
	if err != nil {
		return RangeSummary{}, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			return RangeSummary{}, fmt.Errorf("git diff --name-status %s: unparseable line %q", rng, line)
		}
		s.TotalFiles++
		if len(s.Files) >= maxFiles {
			continue
		}
		fc := FileChange{Status: parts[0], Path: parts[1]}
		if len(parts) >= 3 {
			fc.Path = parts[1] + " -> " + parts[2]
		}
		s.Files = append(s.Files, fc)
	}
	return s, nil
}
