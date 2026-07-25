package provider

// The provider-CLI execution seam (issue #206). Every adapter drives its
// agent CLI's machine-level operations — auth status, logout, the
// credential-refresh poke, codex's boot-time catalog probe — through a
// CLIRunner instead of exec'ing directly. The adapter describes WHAT to run
// (a CLIInvocation); the runner decides WHERE it runs: HostCLI below is the
// default (a direct host process, byte-for-byte the behavior the adapters
// had when they exec'd themselves), and a container-mode server injects a
// containerized runner so the host needs no provider CLIs installed. That
// split is what keeps podman knowledge 100% out of the provider packages.
//
// NOT on this seam: the interactive login/instance sessions (they live in
// tmux panes via tmuxx.SessionRunner) and the conformance suite's git/grep
// helpers (test infrastructure, not provider CLIs).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// CLIInvocation is one provider-CLI run: the full argv, the env overrides,
// an optional working directory, and an optional stdout cap. Deliberately
// provider- and runner-neutral — nothing here names a binary, a store
// layout, or an execution vehicle.
type CLIInvocation struct {
	// Argv is the full command, binary first (the adapter's configured bin —
	// a path or a PATH-resolved name). Never empty.
	Argv []string
	// Env is the K=V overrides applied over the base environment with
	// filter-then-append semantics: for each override's key, every existing
	// K= entry is removed, then the override appended. Filtering first is
	// what makes it a genuine override rather than a same-key duplicate —
	// some C-library getenv() implementations resolve the FIRST matching
	// entry in the child's environ array, so a naive append could leave a
	// stray inherited value in effect (the hazard the adapters' former
	// withMasterConfigDir/overrideEnv helpers defended against; this seam
	// generalizes them). nil (or empty) inherits the base environment
	// unchanged.
	Env []string
	// Dir is the working directory; "" inherits the caller's.
	Dir string
	// MaxStdout, when > 0, caps how much stdout is read: exceeding the cap
	// kills the process and Run returns an error naming the cap — a guard
	// against a haywire binary, never a silent truncation (the pinned
	// semantics of codex's catalog probe, the seam's one capped caller).
	// Zero reads stdout unbounded.
	MaxStdout int64
}

// CLIRunner executes provider-CLI invocations (issue #206). Implementations
// exist outside the adapters (HostCLI here; a containerized runner on the
// container-mode server); adapters only ever hold the interface.
//
// Error contract, load-bearing for every caller: an error that is DIRECTLY a
// *exec.ExitError — never wrapped — means the CLI itself ran and exited
// non-zero; stdout/stderr are still valid and callers may parse them
// (codex's definitive logged-out verdict is a direct type assertion on
// exactly this). Any other error means the invocation could not run at all:
// a missing binary, a cancelled context before start, stdout over
// MaxStdout. Implementations MUST preserve that distinction by passing the
// process's exit error through raw.
type CLIRunner interface {
	Run(ctx context.Context, inv CLIInvocation) (stdout, stderr []byte, err error)
}

// HostCLI is the default CLIRunner: the invocation runs as a direct host
// process via os/exec, exactly as the adapters ran their CLIs before this
// seam existed. The zero value is ready to use — it carries no state, so one
// instance may serve any number of concurrent Runs.
type HostCLI struct{}

var _ CLIRunner = (*HostCLI)(nil)

// Run implements CLIRunner over exec.CommandContext. cmd.Env stays nil
// (inherit the parent environment untouched) when inv.Env is empty;
// otherwise it is os.Environ() with inv.Env's filter-then-append overrides
// applied. Stdout and stderr are captured into separate buffers (callers
// that need the pre-seam CombinedOutput view concatenate them — see codex's
// runStatus for why that is safe there). Errors from the process itself
// (Run/Wait) are returned raw so a *exec.ExitError reaches the caller
// unwrapped, per the CLIRunner contract.
func (h *HostCLI) Run(ctx context.Context, inv CLIInvocation) ([]byte, []byte, error) {
	if len(inv.Argv) == 0 {
		return nil, nil, errors.New("provider: CLIInvocation.Argv is empty")
	}
	cmd := exec.CommandContext(ctx, inv.Argv[0], inv.Argv[1:]...)
	if len(inv.Env) > 0 {
		cmd.Env = overrideEnviron(os.Environ(), inv.Env)
	}
	if inv.Dir != "" {
		cmd.Dir = inv.Dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if inv.MaxStdout > 0 {
		return runCapped(cmd, inv.MaxStdout, &stderr)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run() // raw: a non-zero exit surfaces as a direct *exec.ExitError
	return stdout.Bytes(), stderr.Bytes(), err
}

// runCapped is Run's MaxStdout>0 path: stdout is streamed through an
// io.LimitReader sized cap+1 so an overrun is detected by the extra byte,
// then the process is KILLED (never drained) and the cap-naming error
// returned — ported verbatim from codex's pre-seam ProbeModels. A clean
// under-cap run still returns cmd.Wait's error raw, so the direct
// *exec.ExitError contract holds on this path too.
func runCapped(cmd *exec.Cmd, limit int64, stderr *bytes.Buffer) ([]byte, []byte, error) {
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		// Raw: a missing binary must fail fast as the exec lookup error
		// (codex's conformance construction with a "codex-not-invoked" bin
		// relies on it never riding out a probe timeout).
		return nil, nil, err
	}
	out, readErr := io.ReadAll(io.LimitReader(pipe, limit+1))
	if int64(len(out)) > limit {
		_ = cmd.Process.Kill() // stop a haywire binary instead of draining it
		_ = cmd.Wait()
		return out, stderr.Bytes(), fmt.Errorf("stdout exceeds %d bytes", limit)
	}
	if readErr != nil {
		_ = cmd.Wait()
		return out, stderr.Bytes(), fmt.Errorf("read stdout: %w", readErr)
	}
	if err := cmd.Wait(); err != nil {
		return out, stderr.Bytes(), err // raw *exec.ExitError on a non-zero exit
	}
	return out, stderr.Bytes(), nil
}

// overrideEnviron applies overrides ("K=V" entries) onto environ with the
// filter-then-append semantics CLIInvocation.Env documents: every existing
// entry for an overridden key is removed, then the override appended. An
// override with no '=' is treated as a bare key with an empty value —
// callers never pass one, but the split must not panic on it.
func overrideEnviron(environ, overrides []string) []string {
	out := make([]string, 0, len(environ)+len(overrides))
	out = append(out, environ...)
	for _, kv := range overrides {
		key, _, _ := strings.Cut(kv, "=")
		prefix := key + "="
		filtered := out[:0]
		for _, e := range out {
			if !strings.HasPrefix(e, prefix) {
				filtered = append(filtered, e)
			}
		}
		out = append(filtered, kv)
	}
	return out
}
