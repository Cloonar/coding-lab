// Package gitx is lab's GitEngine — the single seam over the git CLI
// (design §1). M2 scope: bare reference-repo clones with live progress
// (CloneBare), default-branch detection (DefaultBranch), remote-tracking
// ref refresh (Fetch), repo-name sanitization (SanitizeRepoName /
// NameFromURL, the v0 scanner rules), and the per-repo branch-pattern
// grammar (design §4a). M3 adds the worktree lifecycle ported from v0
// git.go (worktree.go: AddWorktree with the fail-loud fetch and no
// fallback base, the separate RemoveWorktree/DeleteBranch rollback
// helpers, the dirty/merged/parked queries), the guarded teardown rule
// (teardown.go: decideTeardown verbatim + TeardownGuarded), and the
// naming algebra (session.go: <repoName>~<label> sessions, dash-joined
// worktree dirs, minute-bumped manual labels) — all on the same Engine
// plumbing (run, baseEnv, gitTimeout).
//
// Every git subprocess runs with a minimal base environment, never an
// os.Environ() passthrough: credentialed operations receive exactly the
// entries the vault materializes (GIT_SSH_COMMAND for ssh_key credentials,
// GIT_ASKPASS for https_token — design §6) via extraEnv. Git command
// failures surface stderr verbatim to the caller (v0 property).
package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds every bounded git subprocess (design §2: git ops get a
// 60s timeout). It exists for the network ops — ls-remote and fetch against
// a stalled remote must fail loudly instead of hanging forever; local ops
// finish in milliseconds and never approach it. Clones are exempt: CloneBare
// has no timeout and is cancelled via its context only.
const gitTimeout = 60 * time.Second

// Engine runs git subprocesses for lab: reference-repo clones and fetches
// in M2, the full worktree lifecycle from M3 on. The zero value is not
// usable; construct with New.
type Engine struct {
	bin     string        // git binary path
	timeout time.Duration // per-call bound for run(); clones are exempt

	// now and progressEvery exist for the clone-progress throttle and are
	// overridden in tests (design §2: no time.Now() inside decision logic).
	now           func() time.Time
	progressEvery time.Duration
}

// New returns an Engine shelling out to the given git binary (usually just
// "git", resolved via PATH).
func New(bin string) *Engine {
	return &Engine{
		bin:           bin,
		timeout:       gitTimeout,
		now:           time.Now,
		progressEvery: cloneProgressInterval,
	}
}

// baseEnv is the minimal environment every git subprocess starts from:
// PATH (git spawns ssh and its remote helpers), HOME (ssh and git both
// want one; the hermetic test env overrides it via extraEnv),
// LC_ALL=C (stable, parseable output), and GIT_TERMINAL_PROMPT=0 (a
// credentialed op that would prompt fails loudly instead of hanging).
//
// It deliberately does NOT set GIT_CONFIG_NOSYSTEM: production may
// legitimately read /etc/gitconfig. Tests append
// testutil.HermeticGitEnv(dir) as extraEnv — extraEnv entries come after
// the base, and os/exec gives the last duplicate key the win, so HOME,
// GIT_CONFIG_GLOBAL, and GIT_CONFIG_NOSYSTEM are hermetic there.
func (e *Engine) baseEnv() []string {
	env := []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0"}
	for _, k := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// ProbeBareRepo reports whether dir is itself a git directory: `git
// --git-dir <dir> rev-parse --git-dir` with the engine's minimal base env.
// The explicit --git-dir anchors the probe — rev-parse never walks up to an
// ancestor repository's .git, and a GIT_DIR inherited from the daemon
// environment is ignored — so an empty or garbage leftover dir probes
// false (the design §3a healing probe). extraEnv follows the run contract.
func (e *Engine) ProbeBareRepo(ctx context.Context, dir string, extraEnv []string) bool {
	_, err := e.run(ctx, "", extraEnv, "--git-dir", dir, "rev-parse", "--git-dir")
	return err == nil
}

// run executes one bounded git subprocess in dir (empty dir = inherit cwd),
// with extraEnv appended to the minimal base env, and returns its stdout.
// Error shaping is the v0 contract: deadline → "git <args>: timed out after
// <timeout>"; exit error → "git <args>: <err>: <trimmed stderr>" (stderr
// verbatim); anything else → "git <args>: <err>".
func (e *Engine) run(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(e.baseEnv(), extraEnv...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return out, fmt.Errorf("git %s: timed out after %s", strings.Join(args, " "), e.timeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// PinHooksPath sets the bare repo's LOCAL core.hooksPath to the absolute
// <bareDir>/hooks directory. Local scope out-precedences a global/system
// core.hooksPath (husky and friends set one in ~/.gitconfig), which would
// otherwise silently route past the incogni guard; the path must be absolute
// because a relative "hooks" resolves against the worktree the pre-push runs
// in, not the git dir. Idempotent.
func (e *Engine) PinHooksPath(ctx context.Context, bareDir string, extraEnv []string) error {
	abs, err := filepath.Abs(filepath.Join(bareDir, "hooks"))
	if err != nil {
		return fmt.Errorf("hooks path: %w", err)
	}
	if _, err := e.run(ctx, bareDir, extraEnv, "config", "--local", "core.hooksPath", abs); err != nil {
		return err
	}
	return nil
}

// UnpinHooksPath clears the local core.hooksPath the incogni guard set, so a
// non-incogni repo falls back to git's default hook resolution. A missing key
// (exit 5) is not an error.
func (e *Engine) UnpinHooksPath(ctx context.Context, bareDir string, extraEnv []string) error {
	if _, err := e.run(ctx, bareDir, extraEnv, "config", "--local", "--unset", "core.hooksPath"); err != nil {
		if strings.Contains(err.Error(), "exit status 5") {
			return nil // key was not set
		}
		return err
	}
	return nil
}
