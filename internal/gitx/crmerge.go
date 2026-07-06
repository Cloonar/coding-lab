package gitx

// Change requests are lab's internal PRs (CONTEXT.md vocabulary: "change
// request", never "merge request"); crmerge.go is the M6 gitx surface for
// them (brief §9 "Change-request merge"): CRDiff renders the reviewable
// unified diff, CRMerge lands the CR's head branch on the base branch of the
// repo's REAL origin. Both operate on the bare reference repo. The head is
// the LOCAL refs/heads/<head> the run's worktree created (the guarded
// teardown keeps an unmerged branch, so it survives the reap); the base is
// the remote-tracking refs/remotes/origin/<base> — lab's published-mainline
// convention everywhere (AddWorktree fork base, BranchMerged, CommitsAhead).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// crDiffMaxBytes bounds the diff text CRDiff returns (~1MiB): the CR detail
// endpoint computes the diff live per request and ships it to a phone, so a
// pathological diff must not balloon the payload or the process. The
// truncated return value is the marker — the API forwards it as
// diff_truncated and the SPA renders the truncation notice; no in-band
// marker is appended to the diff text itself.
const crDiffMaxBytes = 1 << 20

// ErrPushRejected marks a push that origin refused: a pre-receive/update
// hook decline (protected branch; M7's incogni guard) or a non-fast-forward
// race with someone else pushing to the base branch. The wrapped message
// carries git's stderr verbatim (the v0 property) — that text IS the
// operator-facing 409 body, so it must survive intact. Anything else that
// breaks a push (unreachable remote, bad credential) stays an ordinary
// shaped run error, NOT an ErrPushRejected — a network failure is not a
// rejection and must not read like one.
var ErrPushRejected = errors.New("push rejected by origin")

// ErrHeadMissing marks a CR head branch that no longer resolves as a local
// branch in the bare repo — e.g. discarded from Parked after the CR was
// filed. The merge route 409s on it; the CR detail route omits the diff
// with a note instead of failing the whole page.
var ErrHeadMissing = errors.New("change-request head branch missing")

// ErrMergeConflict marks a non-fast-forward merge whose head conflicts with
// the current base. git's conflict report (merge-ort writes it to STDOUT,
// not stderr) is wrapped verbatim — that text names the conflicted files and
// is exactly what the operator needs in the 409 body.
var ErrMergeConflict = errors.New("merge conflict with the base branch")

// CRDiff returns the unified diff of the change request head over its merge
// base with the base branch — `git diff origin/<base>...refs/heads/<head>`
// in the bare repo. The three-dot range IS merge-base semantics (diff from
// merge-base(base, head) to head), so base-side changes landed after the
// fork never pollute the review. -M turns on rename detection; --no-color
// and --no-ext-diff keep the output plain text regardless of any
// /etc/gitconfig production may read (baseEnv deliberately allows it).
//
// CRDiff does NOT fetch: like CommitsAhead and BranchMerged it reads the
// LOCAL origin/<base> ref, and freshness is the caller's business — a CR
// detail view is a hot read path and must not pay a network round-trip.
//
// Output is bounded by crDiffMaxBytes: beyond it the diff is cut at the last
// line boundary inside the bound, truncated=true is returned, and git is
// killed rather than drained (a truncated diff is a SUCCESS — the bound is
// the contract). A head branch that does not resolve returns ErrHeadMissing.
func (e *Engine) CRDiff(ctx context.Context, bareDir, base, head string, extraEnv []string) (diff string, truncated bool, err error) {
	headSHA, err := e.headCommit(ctx, bareDir, head, extraEnv)
	if err != nil {
		return "", false, err
	}
	baseRef := "refs/remotes/origin/" + base
	if _, err := e.refCommit(ctx, bareDir, baseRef, extraEnv); err != nil {
		return "", false, fmt.Errorf("base branch origin/%s does not resolve: %w", base, err)
	}

	// Diff the RESOLVED sha, not the ref name: a concurrent Discard deleting
	// the head branch between the resolve above and the exec below would make
	// a by-name diff die with "bad revision"; the sha keeps resolving (the
	// objects outlive the ref) and the review stays exactly the head that was
	// verified.
	args := []string{"diff", "-M", "--no-color", "--no-ext-diff", baseRef + "..." + headSHA, "--"}
	joined := strings.Join(args, " ")
	runCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, e.bin, args...)
	cmd.Dir = bareDir
	cmd.Env = append(e.baseEnv(), extraEnv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, fmt.Errorf("git %s: %w", joined, err)
	}
	if err := cmd.Start(); err != nil {
		return "", false, fmt.Errorf("git %s: %w", joined, err)
	}

	buf, readErr := io.ReadAll(io.LimitReader(stdout, crDiffMaxBytes+1))
	if len(buf) > crDiffMaxBytes {
		truncated = true
		if cut := bytes.LastIndexByte(buf[:crDiffMaxBytes], '\n'); cut >= 0 {
			buf = buf[:cut+1]
		} else {
			buf = buf[:crDiffMaxBytes]
		}
		cancel() // kill git — the rest of a pathological diff is never read
	}
	waitErr := cmd.Wait()
	if truncated {
		return string(buf), true, nil
	}
	if waitErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return "", false, fmt.Errorf("git %s: timed out after %s", joined, e.timeout)
		}
		return "", false, fmt.Errorf("git %s: %v: %s", joined, waitErr, strings.TrimSpace(stderr.String()))
	}
	if readErr != nil {
		return "", false, fmt.Errorf("git %s: read diff: %w", joined, readErr)
	}
	return string(buf), false, nil
}

// CRMerge lands the change request's head branch on the base branch of the
// repo's REAL origin (extraEnv carries the credential the vault
// materialized) and returns the resulting tip of origin/<base>:
//
//  1. Resolve refs/heads/<head> (missing → ErrHeadMissing), then a
//     fail-loud `git fetch origin` so the ancestry check and merge base see
//     origin's CURRENT state, not the last sweep's — building on a stale
//     base would only convert freshness into avoidable non-ff rejections.
//  2. Ancestry check `merge-base --is-ancestor origin/<base> <head>`:
//     when base is an ancestor of head the merge is a pure fast-forward —
//     `git push origin <headSHA>:refs/heads/<base>`, no commit created, the
//     head sha is the merge commit (brief §9).
//  3. Otherwise a merge commit is built in a TEMPORARY worktree of the bare
//     repo: `worktree add --detach` at origin/<base> → `merge --no-ff -m
//     <message> <headSHA>` → `push origin HEAD:refs/heads/<base>`. The
//     worktree is ALWAYS cleaned up, error paths included. The commit is
//     authored and committed as authorName/authorEmail via
//     GIT_AUTHOR_*/GIT_COMMITTER_* entries appended AFTER extraEnv (last
//     duplicate wins) — the repo's configured REAL identity, never a bot
//     (D15 measure 5).
//  4. A push origin refuses surfaces as ErrPushRejected with the stderr
//     verbatim; then a final fail-loud `git fetch origin` so
//     refs/remotes/origin/<base> reflects the merge — the guarded-teardown
//     merged-check reads local origin refs, and without the refresh the
//     head branch would look unmerged (kept) until the next sweep. If that
//     fetch fails the push HAS landed; the error names the pushed sha and a
//     retry converges (below).
//
// Concurrency invariant — what gitx does and does not guarantee: CRMerge is
// CONVERGENT, not mutually exclusive. Re-merging a head already contained
// in origin/<base> is a no-op at every step (is-ancestor → push of an
// already-present sha, or `merge --no-ff` answering "Already up to date"
// with no new commit and an up-to-date push) and returns the sha
// origin/<base> already carries — no duplicate merge commit, no ref
// movement. Two truly concurrent CRMerge calls race at origin: one wins,
// the loser's push is a non-fast-forward rejection (ErrPushRejected).
// Preventing a double-merge of the same CR is the service layer's job —
// the store's open-state guard on MergeCR (409 on non-open) is the real
// protection; gitx only guarantees the git side stays consistent either way.
func (e *Engine) CRMerge(ctx context.Context, bareDir, base, head, message, authorName, authorEmail string, extraEnv []string) (string, error) {
	headSHA, err := e.headCommit(ctx, bareDir, head, extraEnv)
	if err != nil {
		return "", err
	}
	if err := e.Fetch(ctx, bareDir, extraEnv); err != nil {
		return "", err
	}
	baseRef := "refs/remotes/origin/" + base
	if _, err := e.refCommit(ctx, bareDir, baseRef, extraEnv); err != nil {
		return "", fmt.Errorf("base branch origin/%s does not resolve after fetch: %w", base, err)
	}

	ffable, err := e.isAncestor(ctx, bareDir, baseRef, headSHA, extraEnv)
	if err != nil {
		return "", err
	}
	mergeCommit := headSHA
	if ffable {
		if err := e.pushOrigin(ctx, bareDir, headSHA+":refs/heads/"+base, extraEnv); err != nil {
			return "", err
		}
	} else {
		if mergeCommit, err = e.mergeInTempWorktree(ctx, bareDir, baseRef, headSHA, base, message, authorName, authorEmail, extraEnv); err != nil {
			return "", err
		}
	}
	if err := e.Fetch(ctx, bareDir, extraEnv); err != nil {
		return "", fmt.Errorf("merge pushed to origin/%s as %s but the refresh fetch failed (retrying the merge converges): %w", base, mergeCommit, err)
	}
	return mergeCommit, nil
}

// mergeInTempWorktree builds the non-fast-forward merge commit: a throwaway
// detached worktree of the bare repo at baseRef, `git merge --no-ff` of the
// head sha with the given message and author identity, then a push of the
// worktree's HEAD to origin's base branch. The worktree lives in a fresh
// MkdirTemp dir (concurrent merges never collide) and the deferred cleanup
// runs on every exit path with a cancellation-immune context: `worktree
// remove --force`, falling back to RemoveAll + `worktree prune` so neither
// the directory nor a stale admin entry outlives the call.
//
// An already-merged head makes `merge --no-ff` answer "Already up to date"
// (git creates a forced merge commit only when there is something to
// merge): HEAD stays at baseRef, the push is an up-to-date no-op, and the
// returned sha is the existing tip — the convergent-retry half of the
// CRMerge concurrency invariant. A merge CONFLICT surfaces git's stderr
// verbatim as an ordinary error; the conflicted worktree is torn down by
// the same deferred cleanup.
func (e *Engine) mergeInTempWorktree(ctx context.Context, bareDir, baseRef, headSHA, base, message, authorName, authorEmail string, extraEnv []string) (string, error) {
	tmp, err := os.MkdirTemp("", "lab-cr-merge-")
	if err != nil {
		return "", fmt.Errorf("mkdir temp merge worktree: %w", err)
	}
	defer func() {
		cleanCtx := context.WithoutCancel(ctx)
		if _, rmErr := e.run(cleanCtx, bareDir, extraEnv, "worktree", "remove", "--force", tmp); rmErr != nil {
			_ = os.RemoveAll(tmp)
			_, _ = e.run(cleanCtx, bareDir, extraEnv, "worktree", "prune")
		}
	}()

	if _, err := e.run(ctx, bareDir, extraEnv, "worktree", "add", "--detach", tmp, baseRef); err != nil {
		return "", err
	}
	// Author entries come AFTER extraEnv so they win the os/exec
	// last-duplicate rule — the merge commit carries the repo's configured
	// real identity even when the ambient env (or a hermetic test env)
	// carries its own GIT_AUTHOR_*/GIT_COMMITTER_*.
	authEnv := append(slices.Clone(extraEnv),
		"GIT_AUTHOR_NAME="+authorName,
		"GIT_AUTHOR_EMAIL="+authorEmail,
		"GIT_COMMITTER_NAME="+authorName,
		"GIT_COMMITTER_EMAIL="+authorEmail,
	)
	if err := e.runMerge(ctx, tmp, authEnv, message, headSHA); err != nil {
		return "", err
	}
	out, err := e.run(ctx, tmp, extraEnv, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	mergeCommit := strings.TrimSpace(string(out))
	if err := e.pushOrigin(ctx, tmp, "HEAD:refs/heads/"+base, extraEnv); err != nil {
		return "", err
	}
	return mergeCommit, nil
}

// runMerge runs `git merge --no-ff` in the temp worktree, capturing BOTH
// output streams: merge-ort (git ≥2.34) writes its whole conflict report —
// "Auto-merging …", "CONFLICT (content): …", "Automatic merge failed …" — to
// STDOUT, so the stderr-only shape of Engine.run would surface a conflict as
// a blank "exit status 1". Output carrying a CONFLICT marker wraps
// ErrMergeConflict with the report verbatim (the operator-facing 409 body);
// any other failure keeps both streams in the error.
func (e *Engine) runMerge(ctx context.Context, dir string, env []string, message, headSHA string) error {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "merge", "--no-ff", "-m", message, headSHA)
	cmd.Dir = dir
	cmd.Env = append(e.baseEnv(), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("git merge --no-ff %s: timed out after %s", headSHA, e.timeout)
	}
	report := strings.TrimSpace(string(out))
	if strings.Contains(report, "CONFLICT") {
		return fmt.Errorf("%w: %s", ErrMergeConflict, report)
	}
	return fmt.Errorf("git merge --no-ff %s: %v: %s", headSHA, err, report)
}

// pushOrigin runs `git push origin <refspec>` in dir, classifying failures:
// stderr carrying git's rejection markers ("! [rejected]" for a
// non-fast-forward, "! [remote rejected]" for a hook decline) wraps
// ErrPushRejected with the stderr verbatim — the operator needs the hook's
// own words ("protected branch …") in the 409 body. Every other failure
// keeps the standard run error shape.
func (e *Engine) pushOrigin(ctx context.Context, dir, refspec string, extraEnv []string) error {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "push", "origin", refspec)
	cmd.Dir = dir
	cmd.Env = append(e.baseEnv(), extraEnv...)
	_, err := cmd.Output()
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("git push origin %s: timed out after %s", refspec, e.timeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		stderr := strings.TrimSpace(string(ee.Stderr))
		if strings.Contains(stderr, "[rejected]") || strings.Contains(stderr, "[remote rejected]") {
			return fmt.Errorf("%w: %s", ErrPushRejected, stderr)
		}
		return fmt.Errorf("git push origin %s: %v: %s", refspec, err, stderr)
	}
	return fmt.Errorf("git push origin %s: %w", refspec, err)
}

// headCommit resolves the CR head branch to its commit sha — strictly the
// LOCAL refs/heads/<head> (a CR's head is the branch the run's worktree
// created in the bare repo; remote-tracking refs never qualify). Only a
// DEFINITIVE miss — rev-parse --verify --quiet exiting 1 — is ErrHeadMissing,
// the typed signal the merge route 409s on and the detail route turns into a
// diff-omitted note. A timeout, cancellation, or broken repo (exit 128) is a
// real error and must never masquerade as "branch missing" (the same exit-
// code discrimination as isAncestor/BranchMerged).
func (e *Engine) headCommit(ctx context.Context, bareDir, head string, extraEnv []string) (string, error) {
	ref := "refs/heads/" + head
	ctxT, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctxT, e.bin, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = bareDir
	cmd.Env = append(e.baseEnv(), extraEnv...)
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	if ctxT.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git rev-parse --verify %s: timed out after %s", ref, e.timeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return "", fmt.Errorf("%w: %s", ErrHeadMissing, ref)
	}
	if errors.As(err, &ee) {
		return "", fmt.Errorf("git rev-parse --verify %s: %v: %s", ref, err, strings.TrimSpace(string(ee.Stderr)))
	}
	return "", fmt.Errorf("git rev-parse --verify %s: %w", ref, err)
}

// refCommit resolves ref to a commit sha via `git rev-parse --verify
// --quiet <ref>^{commit}` — one call that both verifies existence and
// yields the sha.
func (e *Engine) refCommit(ctx context.Context, bareDir, ref string, extraEnv []string) (string, error) {
	out, err := e.run(ctx, bareDir, extraEnv, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isAncestor reports whether anc is an ancestor of desc (`git merge-base
// --is-ancestor`, exit 0 = yes, exit 1 = a definite no, anything else a
// real error) — the same exit-code discrimination as BranchMerged, kept
// separate because BranchMerged's contract is pinned to branch/default
// names while the merge path works on resolved refs.
func (e *Engine) isAncestor(ctx context.Context, dir, anc, desc string, extraEnv []string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "merge-base", "--is-ancestor", anc, desc)
	cmd.Dir = dir
	cmd.Env = append(e.baseEnv(), extraEnv...)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil // definitively not an ancestor
	}
	if ctx.Err() == context.DeadlineExceeded {
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: timed out after %s", anc, desc, e.timeout)
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", anc, desc, err)
}
