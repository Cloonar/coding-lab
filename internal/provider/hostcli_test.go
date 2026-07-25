package provider_test

// Unit pins for the default host CLIRunner (issue #206). HostCLI is the
// runner every adapter falls back to when no containerized runner is
// injected, so its semantics — env override filter-then-append, inherit on
// nil Env, working-dir application, separated streams, the DIRECT
// *exec.ExitError passthrough, and the MaxStdout kill — are exactly the
// behaviors the adapters' stub-script suites observe end to end. The stubs
// here follow the fakeClaude/fakeCodex sh-script pattern.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// fakeCLI writes an executable sh script standing in for a provider binary
// and returns its path (the adapter suites' fakeClaude/fakeCodex pattern).
func fakeCLI(t *testing.T, script string) string {
	t.Helper()
	testutil.RequireTool(t, "sh")
	path := filepath.Join(t.TempDir(), "provider-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Stdout and stderr land in separate buffers — the seam's replacement for
// the adapters' per-call cmd.Stdout/cmd.Stderr wiring.
func TestHostCLI_separatesStdoutAndStderr(t *testing.T) {
	bin := fakeCLI(t, `echo out-line
echo err-line 1>&2`)
	stdout, stderr, err := (&provider.HostCLI{}).Run(context.Background(), provider.CLIInvocation{Argv: []string{bin}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stdout) != "out-line\n" {
		t.Errorf("stdout = %q; want %q", stdout, "out-line\n")
	}
	if string(stderr) != "err-line\n" {
		t.Errorf("stderr = %q; want %q", stderr, "err-line\n")
	}
}

// Env overrides are filter-then-append: a pre-existing entry for the same
// key is REMOVED (never left as a first-match-winning duplicate) and
// unrelated inherited entries pass through — the generalized contract behind
// the adapters' former withMasterConfigDir/overrideEnv helpers.
func TestHostCLI_envOverrideFilterThenAppend(t *testing.T) {
	t.Setenv("LAB_HOSTCLI_VAR", "stray-inherited")
	t.Setenv("LAB_HOSTCLI_KEEP", "kept")
	bin := fakeCLI(t, `echo "n=$(env | grep -c '^LAB_HOSTCLI_VAR=')"
echo "v=$LAB_HOSTCLI_VAR"
echo "keep=$LAB_HOSTCLI_KEEP"`)
	stdout, _, err := (&provider.HostCLI{}).Run(context.Background(), provider.CLIInvocation{
		Argv: []string{bin},
		Env:  []string{"LAB_HOSTCLI_VAR=pinned"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := string(stdout)
	if !strings.Contains(got, "n=1\n") {
		t.Errorf("child saw multiple (or zero) LAB_HOSTCLI_VAR entries:\n%s", got)
	}
	if !strings.Contains(got, "v=pinned\n") {
		t.Errorf("child LAB_HOSTCLI_VAR is not the override:\n%s", got)
	}
	if !strings.Contains(got, "keep=kept\n") {
		t.Errorf("unrelated inherited env entry was not preserved:\n%s", got)
	}
}

// A nil Env leaves cmd.Env nil, so the child inherits the parent environment
// unchanged.
func TestHostCLI_nilEnvInherits(t *testing.T) {
	t.Setenv("LAB_HOSTCLI_VAR", "inherited")
	bin := fakeCLI(t, `echo "v=$LAB_HOSTCLI_VAR"`)
	stdout, _, err := (&provider.HostCLI{}).Run(context.Background(), provider.CLIInvocation{Argv: []string{bin}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stdout) != "v=inherited\n" {
		t.Errorf("stdout = %q; want the inherited value", stdout)
	}
}

// A non-empty Dir becomes the child's working directory.
func TestHostCLI_dirApplied(t *testing.T) {
	dir := t.TempDir()
	bin := fakeCLI(t, `pwd -P`)
	stdout, _, err := (&provider.HostCLI{}).Run(context.Background(), provider.CLIInvocation{
		Argv: []string{bin},
		Dir:  dir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if got := strings.TrimSpace(string(stdout)); got != want {
		t.Errorf("child cwd = %q; want %q", got, want)
	}
}

// A non-zero exit surfaces as a DIRECT *exec.ExitError — never wrapped —
// with stdout/stderr still valid: the contract codex's definitive logged-out
// verdict type-asserts on.
func TestHostCLI_exitErrorPassesThroughDirect(t *testing.T) {
	bin := fakeCLI(t, `echo partial-out
echo the-stderr 1>&2
exit 3`)
	stdout, stderr, err := (&provider.HostCLI{}).Run(context.Background(), provider.CLIInvocation{Argv: []string{bin}})
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("err = %v (%T); want a DIRECT *exec.ExitError", err, err)
	}
	if ee.ExitCode() != 3 {
		t.Errorf("exit code = %d; want 3", ee.ExitCode())
	}
	if string(stdout) != "partial-out\n" {
		t.Errorf("stdout = %q; want it valid alongside the exit error", stdout)
	}
	if string(stderr) != "the-stderr\n" {
		t.Errorf("stderr = %q; want it valid alongside the exit error", stderr)
	}
}

// A missing binary is a could-not-run error, NOT an *exec.ExitError — the
// other half of the CLIRunner error contract.
func TestHostCLI_missingBinaryIsNotExitError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-cli")
	_, _, err := (&provider.HostCLI{}).Run(context.Background(), provider.CLIInvocation{Argv: []string{missing}})
	if err == nil {
		t.Fatal("Run with a missing binary = nil error; want an error")
	}
	if _, isExit := err.(*exec.ExitError); isExit {
		t.Errorf("err = %v is an *exec.ExitError; a missing binary never ran, so it must not read as a CLI exit", err)
	}
}

// An empty Argv is a programmer error, reported before anything runs.
func TestHostCLI_emptyArgvErrors(t *testing.T) {
	if _, _, err := (&provider.HostCLI{}).Run(context.Background(), provider.CLIInvocation{}); err == nil {
		t.Error("Run with an empty Argv = nil error; want a programmer error")
	}
}

// A run under the cap is untouched by MaxStdout, and a non-zero exit on the
// capped path still returns the direct *exec.ExitError with output intact —
// the capped and uncapped paths share one error contract.
func TestHostCLI_maxStdoutUnderCap(t *testing.T) {
	bin := fakeCLI(t, `echo small
echo capped-stderr 1>&2
exit 2`)
	stdout, stderr, err := (&provider.HostCLI{}).Run(context.Background(), provider.CLIInvocation{
		Argv:      []string{bin},
		MaxStdout: 1 << 20,
	})
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("err = %v (%T); want a DIRECT *exec.ExitError on the capped path too", err, err)
	}
	if ee.ExitCode() != 2 {
		t.Errorf("exit code = %d; want 2", ee.ExitCode())
	}
	if string(stdout) != "small\n" {
		t.Errorf("stdout = %q; want %q", stdout, "small\n")
	}
	if string(stderr) != "capped-stderr\n" {
		t.Errorf("stderr = %q; want it captured on the capped path", stderr)
	}
}

// Exceeding MaxStdout KILLS the process (a haywire binary is never drained)
// and returns a cap-naming error that is NOT an *exec.ExitError — the codex
// catalog probe's pinned overflow semantics, hoisted onto the seam.
func TestHostCLI_maxStdoutKillsAndErrors(t *testing.T) {
	// An endless emitter: only the kill can end it — a hang here means the
	// overflow path failed, so bound the whole run defensively.
	bin := fakeCLI(t, `while :; do echo xxxxxxxxxxxxxxxx; done`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := (&provider.HostCLI{}).Run(ctx, provider.CLIInvocation{
		Argv:      []string{bin},
		MaxStdout: 1024,
	})
	if err == nil {
		t.Fatal("Run over MaxStdout = nil error; want the cap error")
	}
	if _, isExit := err.(*exec.ExitError); isExit {
		t.Fatalf("err = %v is an *exec.ExitError; the cap overflow must read as could-not-run, never as a CLI exit verdict", err)
	}
	if !strings.Contains(err.Error(), "1024") {
		t.Errorf("err = %q; want it to name the cap", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("overflow took %v; want a prompt kill, not a drain", elapsed)
	}
}
