package labctl

// The `secret scan` verb (issue #106): the agent-side half of the git leak
// guard. It collects the outgoing push diff with ONE git invocation and
// streams it to the server-side scan (POST /agent/v1/secrets/scan); the
// server answers with finding NAMES and locations only — secret values never
// cross back, and labctl never reads the vault. The pre-push hook runs
// `labctl secret scan <range>` and blocks the push on ANY nonzero exit, so
// the verb fails CLOSED: findings, a git failure, a transport error, and a
// non-2xx answer all exit 1; a clean pass prints nothing and exits 0.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// errGitFailed marks the scan-request pipe as broken by a git failure: the
// goroutine streaming git's stdout closes the pipe with this error (wrapped
// around git's own words) so an in-flight request cannot complete as a clean
// pass over a truncated diff.
var errGitFailed = errors.New("git log failed")

// runSecretScan implements `labctl secret scan <rev-arg>...`.
//
// Exit contract (DELIBERATELY different from the binary-wide convention — see
// the package doc): 0 is a clean pass and prints NOTHING; 1 is findings OR
// any failure — git, transport, non-2xx — because the pre-push hook blocks on
// any nonzero and a guard that cannot prove the diff clean must fail closed;
// 2 is usage (no rev-args, or missing LAB_URL/LAB_TOKEN).
//
// The rev-args are git REVISION arguments passed through VERBATIM — the hook
// sends `<remoteSha>..<localSha>` or `<localSha> --not --remotes=origin`, so
// they may legitimately start with `--` and are never flag-parsed or
// reordered here.
func runSecretScan(args []string, env Env) int {
	if len(args) == 0 {
		return secretScanUsage(env, "want <rev-arg>... (git revision arguments naming the outgoing commits)")
	}

	// Hand-rolled env read (NOT withClient): usage/env stays 2, but every
	// post-usage failure folds into 1 (fail closed), which withClient's
	// error handling and message shape do not express.
	baseURL := env.Getenv("LAB_URL")
	token := env.Getenv("LAB_TOKEN")
	if baseURL == "" || token == "" {
		return secretScanUsage(env, "LAB_URL and LAB_TOKEN must be set")
	}
	c := &Client{BaseURL: baseURL, Token: token}

	// ONE git invocation collects the outgoing diff as a pure patch stream:
	// `--format=` suppresses commit metadata, `-m` diffs merges against each
	// parent (evil merges stay visible), `--no-ext-diff` keeps
	// repo-configured diff drivers out of the stream. git runs in the
	// current directory — the pre-push hook invokes labctl inside the
	// pushing worktree, so cwd/GIT_DIR are already right.
	gitArgs := append([]string{
		"-c", "core.quotePath=false",
		"log", "--format=", "--no-color", "--no-ext-diff", "-p", "-m",
	}, args...)
	cmd := exec.Command("git", gitArgs...)
	var gitStderr bytes.Buffer
	cmd.Stderr = &gitStderr

	// Stream git's stdout to the server without buffering the whole diff:
	// git writes into an io.Pipe whose read side is the HTTP request body.
	// The goroutine closes the write side when git exits — CloseWithError on
	// failure, so the transport sees a broken body instead of a clean EOF.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	gitDone := make(chan error, 1)
	go func() {
		err := cmd.Run()
		if err != nil {
			_ = pw.CloseWithError(fmt.Errorf("%w: %s", errGitFailed, gitFailureDetail(err, gitStderr.Bytes())))
		} else {
			_ = pw.Close()
		}
		gitDone <- err
	}()

	findings, scanErr := c.SecretScan(pr)
	gitErr := <-gitDone

	// Fail-closed check order. Server/transport first: an early non-2xx (a
	// 413, say) stops the body read and can kill a still-writing git with
	// SIGPIPE — the server's answer, not the confusing SIGPIPE report, is
	// the truth. But a scan error CAUSED by the git failure itself (the
	// CloseWithError above) defers to the git report. Then git: its failure
	// wins even over a 200 — a partial diff scanned as "pass" proves
	// nothing. Findings last; a clean pass is a silent 0.
	if scanErr != nil && !errors.Is(scanErr, errGitFailed) {
		return secretScanFail(env, fmt.Sprintf("scan failed: %v", scanErr))
	}
	if gitErr != nil {
		return secretScanFail(env, "git log failed: "+gitFailureDetail(gitErr, gitStderr.Bytes()))
	}
	if scanErr != nil {
		// A broken pipe without a git failure behind it — unexpected, but
		// the guard did not pass, so it still fails closed.
		return secretScanFail(env, fmt.Sprintf("scan failed: %v", scanErr))
	}
	if len(findings) > 0 {
		for _, f := range findings {
			_, _ = fmt.Fprintf(env.Stderr, "labctl secret scan: secret %s (%s form) found in %s\n", f.Secret, f.Form, f.File)
		}
		_, _ = fmt.Fprintf(env.Stderr, "labctl secret scan: %d finding(s): remove the values from the diff and rewrite the offending commits before pushing\n", len(findings))
		return 1
	}
	return 0
}

// gitFailureDetail renders a one-line git failure: the exec error plus git's
// stderr (newlines collapsed) when it said anything.
func gitFailureDetail(err error, stderr []byte) string {
	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		return err.Error()
	}
	return err.Error() + ": " + strings.ReplaceAll(detail, "\n", "; ")
}

// secretScanFail reports a failed guard: one line on stderr, exit 1. Every
// post-usage failure funnels here — the pre-push hook treats any nonzero as
// "do not push", and a guard that cannot prove the diff clean must not pass.
func secretScanFail(env Env, msg string) int {
	_, _ = fmt.Fprintf(env.Stderr, "labctl secret scan: %s\n", msg)
	return 1
}

// secretScanUsage reports a `secret scan` shape/env error: exit 2 with usage
// on stderr. (Everything after usage passes — findings, git, transport,
// non-2xx — is 1; see runSecretScan.)
func secretScanUsage(env Env, msg string) int {
	_, _ = fmt.Fprintf(env.Stderr, "labctl secret scan: %s\n\n%s", msg, usage)
	return 2
}
