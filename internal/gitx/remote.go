package gitx

import (
	"context"
	"errors"
	"strings"
)

// DefaultBranch resolves the remote's default branch for a bare reference
// repo, for the repos.default_branch column at clone time. A bare clone
// never has refs/remotes/origin/HEAD set, so the v0 symbolic-ref lookup
// does not apply; the order here is:
//
//  1. `git ls-remote --symref origin HEAD` — asks the remote itself
//     (network; runs with extraEnv for credentialed remotes), parsing the
//     "ref: refs/heads/<branch>\tHEAD" line.
//  2. The bare repo's own HEAD symref (`git symbolic-ref HEAD` →
//     refs/heads/<branch>) — git points it at the remote's default branch
//     at clone time, so this answers offline.
//  3. "main" (the v0 fallback rule).
//
// It never fails: any error just falls through to the next tier.
func (e *Engine) DefaultBranch(ctx context.Context, bareDir string, extraEnv []string) string {
	if out, err := e.run(ctx, bareDir, extraEnv, "ls-remote", "--symref", "origin", "HEAD"); err == nil {
		for line := range strings.Lines(string(out)) {
			rest, ok := strings.CutPrefix(line, "ref: refs/heads/")
			if !ok {
				continue
			}
			if branch, _, ok := strings.Cut(rest, "\t"); ok && branch != "" {
				return branch
			}
		}
	}
	if branch, ok := e.HeadBranch(ctx, bareDir, nil); ok {
		return branch
	}
	return "main"
}

// HeadBranch returns the branch the repo's own HEAD symref points at
// (`git symbolic-ref HEAD` → refs/heads/<branch>), or ok=false when HEAD
// is detached or unreadable. For a bare reference clone this is the
// remote's default branch as git recorded it at clone time — it answers
// offline (DefaultBranch tier 2; also the startup-heal re-derivation).
func (e *Engine) HeadBranch(ctx context.Context, gitDir string, extraEnv []string) (string, bool) {
	out, err := e.run(ctx, gitDir, extraEnv, "symbolic-ref", "HEAD")
	if err != nil {
		return "", false
	}
	ref := strings.TrimSpace(string(out))
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if branch == "" || branch == ref {
		return "", false
	}
	return branch, true
}

// Fetch refreshes the reference repo's remote-tracking refs — `git fetch
// origin` with extraEnv (fail-loud, stderr verbatim in the error), then a
// best-effort `git remote set-head origin --auto` so origin/HEAD tracks a
// default-branch change on the remote. Bounded by gitTimeout like every
// other non-clone op. The outcome of the fetch itself is reported to the
// fetch observer when ctx is attributed (AttributeFetch, observe.go).
func (e *Engine) Fetch(ctx context.Context, bareDir string, extraEnv []string) error {
	_, err := e.run(ctx, bareDir, extraEnv, "fetch", "origin")
	e.reportFetch(ctx, err)
	if err != nil {
		return err
	}
	_, _ = e.run(ctx, bareDir, extraEnv, "remote", "set-head", "origin", "--auto")
	return nil
}

// DeleteRemoteBranch deletes refs/heads/<branch> on the bare repo's ORIGIN —
// the merge-time head delete of ADR-0081 (issue #90), run from the bare
// reference clone with extraEnv carrying the credential the vault
// materialized. It never touches the LOCAL refs/heads/<branch>: the local
// branch lifecycle stays with guarded teardown and the sweep.
//
// The contract is "absent on origin afterwards", not "this call removed it":
// nil means refs/heads/<branch> does not exist on origin once DeleteRemoteBranch
// returns — whether the push deleted it now, a previous (convergent) attempt
// already did, or the branch was never pushed at all. Detecting the
// already-absent case does NOT parse git's "remote ref does not exist" text
// (porcelain prose that differs across git versions and transports, and a
// false match would hide a real refusal); instead, when the deletion push
// fails, a `git ls-remote origin refs/heads/<branch>` asks origin directly
// and its output is matched on the EXACT ref name (ls-remote patterns match
// on path tails, so any listed line is not proof by itself). Absent there →
// nil; still listed, or the ls-remote itself failing (origin unreachable) →
// the PUSH's error, which carries git's own
// stderr verbatim (an origin hook decline wraps ErrPushRejected, like the
// merge push — see pushOrigin). Git anonymizes credentials in the URLs it
// prints, and the credential itself lives in a materialized helper file, so
// the error text is safe to surface.
//
// The pre-push leak guard lab installs in the bare repo skips deletions (an
// all-zero local sha pushes no commits), so the guard never blocks this.
// A successful deletion push also removes refs/remotes/origin/<branch> (the
// clone's standard fetch refspec maps it, and git updates tracking refs on
// push); on the already-absent path a lingering tracking ref is dropped
// best-effort so the bare clone does not keep advertising a branch origin no
// longer has (UnpushedCount and the guard's --not --remotes=origin read it).
func (e *Engine) DeleteRemoteBranch(ctx context.Context, bareDir, branch string, extraEnv []string) error {
	if branch == "" {
		return errors.New("delete remote branch: empty branch name")
	}
	ref := "refs/heads/" + branch
	pushErr := e.pushOrigin(ctx, bareDir, ":"+ref, extraEnv)
	if pushErr == nil {
		return nil
	}
	present, lsErr := e.remoteRefExists(ctx, bareDir, ref, extraEnv)
	if lsErr != nil || present {
		return pushErr
	}
	_, _ = e.run(ctx, bareDir, extraEnv, "update-ref", "-d", "refs/remotes/origin/"+branch)
	return nil
}

// remoteRefExists reports whether origin advertises exactly ref — `git
// ls-remote origin <ref>` with an exact match on the listed name. A failed
// ls-remote (network, auth) is an error, never a "no".
func (e *Engine) remoteRefExists(ctx context.Context, bareDir, ref string, extraEnv []string) (bool, error) {
	out, err := e.run(ctx, bareDir, extraEnv, "ls-remote", "origin", ref)
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(string(out)) {
		if _, name, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "\t"); ok && name == ref {
			return true, nil
		}
	}
	return false, nil
}
