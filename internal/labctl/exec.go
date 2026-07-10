package labctl

// The `secret` verb (issue #104): the agent-side surface over the run's
// per-repo secrets. `secret list` renders name + description (never values);
// `secret exec <NAME...> -- <cmd> [args...]` fetches the named values at exec
// time and runs cmd with each injected as $NAME in the CHILD's environment
// ONLY. Values are never placed in argv, never exported into labctl's own
// process env (os.Setenv), never written to disk, and never printed or logged
// — they live for one exec, in cmd.Env, and nowhere else. Rotation is live:
// the fetch happens per invocation, so nothing is ever cached.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// runSecret dispatches `labctl secret list|exec|scan`.
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
	case "scan":
		return runSecretScan(args[1:], env)
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
	cmd.Stdout = env.Stdout
	cmd.Stderr = env.Stderr

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// The child ran and exited non-zero: pass its code through. A
			// signal death reports ExitCode() == -1; map that to 1 so the
			// caller always sees a real, non-negative code.
			if code := ee.ExitCode(); code >= 0 {
				return code
			}
			return 1
		}
		// The child never started (command not found / not executable).
		_, _ = fmt.Fprintf(env.Stderr, "labctl secret exec: %v\n", err)
		return 1
	}
	return 0
}

// secretExecUsage reports a `secret exec` shape/env error: exit 2 with usage
// on stderr. (An API error or a failed exec is 1; the child's own code passes
// through — see runSecretExec.)
func secretExecUsage(env Env, msg string) int {
	_, _ = fmt.Fprintf(env.Stderr, "labctl secret exec: %s\n\n%s", msg, usage)
	return 2
}
