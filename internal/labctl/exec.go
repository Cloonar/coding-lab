package labctl

// The `secret` verb (issue #104): the agent-side surface over the run's
// per-repo secrets. `secret list` renders name + description (never values);
// `secret exec <NAME...> -- <cmd> [args...]` fetches the named values at exec
// time and runs cmd with each injected as $NAME in the CHILD's environment
// ONLY. Values are never placed in argv, never exported into labctl's own
// process env (os.Setenv), never written to disk, and never printed or logged
// — they live for one exec, in cmd.Env, and nowhere else. Rotation is live:
// the fetch happens per invocation, so nothing is ever cached.
//
// Broker output redaction (issue #105): the child's stdout and stderr are
// each piped through their own secrets.Redactor, built from a Matcher over
// exactly the values injected for THIS exec, so every occurrence — in the
// exact plaintext or any of its base64/hex/URL-encoded forms — reaches
// env.Stdout/env.Stderr only as the literal token "[REDACTED:NAME]". A
// contained redaction hit is deliberately quiet: no operator alert, no log
// line, and the plaintext value appears nowhere in labctl's own output (grill
// decision, issue #105). Because the redactors are not *os.File, os/exec runs
// the child on pipes rather than inheriting the caller's stdout/stderr
// directly — never a TTY. That is acceptable and intentional: agents don't
// run interactive commands through the broker. Each Redactor also holds back
// a small trailing window of its stream (bounded by the longest derived
// pattern), so the final bytes of a stream only reach the destination once
// Flush runs at process exit — never mid-stream.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"git.cloonar.com/Cloonar/coding-lab/internal/secrets"
)

// runSecret dispatches `labctl secret list|exec`.
func runSecret(args []string, env Env) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "list":
		return runSecretList(args[1:], env)
	case "exec":
		return runSecretExec(args[1:], env)
	default:
		_, _ = fmt.Fprintf(env.Stderr, "labctl secret: unknown subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}

// runSecretList renders one `NAME<TAB>description` row per secret (mirrors
// `label list`). Metadata only — the value never crosses this surface.
func runSecretList(args []string, env Env) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintln(env.Stderr, "labctl secret list: too many arguments")
		return 2
	}
	return withClient(env, "secret list", func(c *Client) error {
		secrets, err := c.SecretList()
		if err != nil {
			return err
		}
		for _, s := range secrets {
			_, _ = fmt.Fprintf(env.Stdout, "%s\t%s\n", s.Name, s.Description)
		}
		return nil
	})
}

// runSecretExec implements `labctl secret exec <NAME...> -- <cmd> [args...]`.
//
// Exit contract (DELIBERATELY different from the binary-wide convention — see
// the package doc): the child's exit code passes through VERBATIM, so 0/1/2 or
// any other code may be the child's own. Only a pre-exec failure is labctl's:
// a bad shape (no `--`, no NAME before it, no command after it) or missing
// LAB_URL/LAB_TOKEN is 2 (usage, printed to stderr); an API error fetching the
// values, or a command that fails to START (not found / not executable), is 1.
// A child killed by a signal (ExitCode() == -1) maps to 1.
//
// The fetched values feed ONLY cmd.Env — they are never placed in argv, never
// exported via os.Setenv, never printed, and never logged.
//
// The child's stdout and stderr each run through their own secrets.Redactor
// (issue #105), so any of the fetched values reaching either stream — in any
// derived form — is replaced with "[REDACTED:NAME]" before it reaches
// env.Stdout/env.Stderr; a hit is quiet by design (no log, no alert). A
// Matcher/Redactor is not safe for concurrent use, and os/exec copies stdout
// and stderr from separate goroutines, so this builds one Matcher and one
// Redactor per stream. cmd.Run waits for both copy goroutines to finish before
// returning, so by the time it returns there is nothing left to feed — but
// each Redactor still holds back a small trailing window that only Flush
// releases. Flush runs on BOTH redactors unconditionally, even when cmd.Run
// already failed and even when the first Flush errors, so a child that prints
// part of a secret and then exits non-zero still gets its tail redacted and
// emitted rather than silently dropped.
func runSecretExec(args []string, env Env) int {
	// Split on the first `--`: names before it, the command (and its args)
	// after it. All three parts are required.
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return secretExecUsage(env, "want <NAME...> -- <cmd> [args...] (the -- separator is required)")
	}
	names := args[:sep]
	cmdArgv := args[sep+1:]
	if len(names) == 0 {
		return secretExecUsage(env, "want at least one secret NAME before --")
	}
	if len(cmdArgv) == 0 {
		return secretExecUsage(env, "want a command after --")
	}

	// Hand-rolled env read (NOT withClient): this verb's exit contract keeps 2
	// for usage/env and 1 for API/transport, and passes the child's code
	// through untouched.
	baseURL := env.Getenv("LAB_URL")
	token := env.Getenv("LAB_TOKEN")
	if baseURL == "" || token == "" {
		return secretExecUsage(env, "LAB_URL and LAB_TOKEN must be set")
	}
	c := &Client{BaseURL: baseURL, Token: token}

	values, err := c.SecretValues(names)
	if err != nil {
		// The API error names missing secrets, never values.
		_, _ = fmt.Fprintf(env.Stderr, "labctl secret exec: %v\n", err)
		return 1
	}

	cmd := exec.Command(cmdArgv[0], cmdArgv[1:]...)
	// Inherit the current env, then append each secret. os/exec gives the last
	// duplicate key the win, so a NAME already present in the environment is
	// overridden by the secret value — the values exist ONLY here.
	cmd.Env = os.Environ()
	for _, name := range names {
		cmd.Env = append(cmd.Env, name+"="+values[name])
	}
	cmd.Stdin = os.Stdin

	// One Matcher/Redactor per stream: neither type is safe for concurrent
	// use, and os/exec copies stdout and stderr from separate goroutines.
	// Assigning a non-*os.File Writer here is what makes os/exec run the
	// child on pipes instead of handing it env.Stdout/env.Stderr directly —
	// deliberate, see the file header.
	stdoutRedactor := secrets.NewRedactor(env.Stdout, secrets.NewMatcher(values))
	stderrRedactor := secrets.NewRedactor(env.Stderr, secrets.NewMatcher(values))
	cmd.Stdout = stdoutRedactor
	cmd.Stderr = stderrRedactor

	runErr := cmd.Run()

	// Flush BOTH redactors unconditionally — even if the first errors, even
	// if cmd.Run itself failed — so a stream's held-back tail is never
	// dropped: a child that prints a secret and then exits non-zero must
	// still get that tail redacted and emitted.
	stdoutFlushErr := stdoutRedactor.Flush()
	stderrFlushErr := stderrRedactor.Flush()

	var ee *exec.ExitError
	if runErr == nil || errors.As(runErr, &ee) {
		// The child ran to completion — either cleanly or with its own exit
		// code, which passes through VERBATIM. A signal death reports
		// ExitCode() == -1; map that to 1 so the caller always sees a real,
		// non-negative code.
		code := 0
		if ee != nil {
			if c := ee.ExitCode(); c >= 0 {
				code = c
			} else {
				code = 1
			}
		}
		flushErr := stdoutFlushErr
		if flushErr == nil {
			flushErr = stderrFlushErr
		}
		if flushErr != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl secret exec: %v\n", flushErr)
			if code == 0 {
				return 1
			}
			return code
		}
		return code
	}

	// The child never started (command not found / not executable), or an
	// output copy failed. Either way runErr already names the failure — the
	// Flush calls above return that same poisoning error, so do not print it
	// again.
	_, _ = fmt.Fprintf(env.Stderr, "labctl secret exec: %v\n", runErr)
	return 1
}

// secretExecUsage reports a `secret exec` shape/env error: exit 2 with usage
// on stderr. (An API error or a failed exec is 1; the child's own code passes
// through — see runSecretExec.)
func secretExecUsage(env Env, msg string) int {
	_, _ = fmt.Fprintf(env.Stderr, "labctl secret exec: %s\n\n%s", msg, usage)
	return 2
}
