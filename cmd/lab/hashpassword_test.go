package main

// Pipe-based tests for `lab hash-password` (issue #137). stdin is a real
// *os.File from os.Pipe() (the write end is written then closed, mimicking a
// shell pipe/redirect: term.IsTerminal is false on a pipe, so runHashPassword
// takes the io.ReadAll branch), and stdout/stderr are plain bytes.Buffer.
//
// These run argon2id at the pinned PRODUCTION cost (64MiB, time=3) — a few
// hundred ms per call, not the near-zero a test-only param set would give.
// That's deliberate: the whole point of this subcommand is to mint the exact
// hash the login path verifies against, so the test double never diverges
// from what an operator actually gets.

import (
	"os"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
)

// pipeStdin writes body to an os.Pipe, closes the write end (so the read end
// sees EOF the way a real shell pipe/redirect does), and returns the read
// end for use as runHashPassword's stdin.
func pipeStdin(t *testing.T, body string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(body); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe write end: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestRunHashPasswordPiped: a password piped in with a trailing newline (the
// common shell shape, e.g. `echo password123 | lab hash-password`) hashes
// successfully — exit 0, stdout is exactly one line, the line parses as a
// valid PHC argon2id hash, and it verifies against the plaintext via the
// same VerifyPassword the login path uses. That last check is the issue's
// actual acceptance: the trailing newline must have been stripped, or the
// hash would be of "password123\n" and never verify against "password123".
func TestRunHashPasswordPiped(t *testing.T) {
	stdin := pipeStdin(t, "password123\n")
	var stdout, stderr strings.Builder

	code := runHashPassword(nil, stdin, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one line", out)
	}
	hash := lines[0]
	if err := httpapi.ValidatePasswordHash(hash); err != nil {
		t.Fatalf("ValidatePasswordHash(%q): %v", hash, err)
	}
	ok, err := httpapi.VerifyPassword(hash, "password123")
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(%q, %q) = %v, %v; want true, nil", hash, "password123", ok, err)
	}
}

// TestRunHashPasswordPipedNoTrailingNewline: input with no trailing newline
// at all (e.g. printf without -n's implicit newline, or a file written
// without one) works identically — the newline-stripping is "at most one",
// not "exactly one required".
func TestRunHashPasswordPipedNoTrailingNewline(t *testing.T) {
	stdin := pipeStdin(t, "password123")
	var stdout, stderr strings.Builder

	code := runHashPassword(nil, stdin, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	out := stdout.String()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one line", out)
	}
	hash := lines[0]
	if err := httpapi.ValidatePasswordHash(hash); err != nil {
		t.Fatalf("ValidatePasswordHash(%q): %v", hash, err)
	}
	ok, err := httpapi.VerifyPassword(hash, "password123")
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(%q, %q) = %v, %v; want true, nil", hash, "password123", ok, err)
	}
}

// TestRunHashPasswordTooShort: a <8-char password (the shared
// httpapi.ValidateNewPassword rule) is rejected — nonzero exit, no stdout
// (never print a hash for a rejected password), and a stderr message naming
// the rule.
func TestRunHashPasswordTooShort(t *testing.T) {
	stdin := pipeStdin(t, "short\n")
	var stdout, stderr strings.Builder

	code := runHashPassword(nil, stdin, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("exit code = 0, want nonzero")
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "at least 8") {
		t.Fatalf("stderr = %q, want it to mention \"at least 8\"", stderr.String())
	}
}

// TestRunHashPasswordArgsRejected: hash-password takes no arguments — the
// password is stdin-only (issue #137: argv leaks via shell history and
// ps/procfs). Passing one is a usage error: exit 2, no stdout, and a stderr
// message pointing at stdin.
func TestRunHashPasswordArgsRejected(t *testing.T) {
	stdin := pipeStdin(t, "")
	var stdout, stderr strings.Builder

	code := runHashPassword([]string{"oops"}, stdin, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr = %q", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "stdin") {
		t.Fatalf("stderr = %q, want it to mention \"stdin\"", stderr.String())
	}
}
