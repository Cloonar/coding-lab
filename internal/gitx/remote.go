package gitx

import (
	"context"
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
// other non-clone op.
func (e *Engine) Fetch(ctx context.Context, bareDir string, extraEnv []string) error {
	if _, err := e.run(ctx, bareDir, extraEnv, "fetch", "origin"); err != nil {
		return err
	}
	_, _ = e.run(ctx, bareDir, extraEnv, "remote", "set-head", "origin", "--auto")
	return nil
}
