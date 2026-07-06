package tmuxx

import (
	"slices"
	"testing"
)

// newSessionArgs is pure (no tmux), so the cap wrapping is asserted
// directly on the argv (transcribed from v0
// TestSessions_newSessionArgsNofileCap; the argv arrives complete from
// the provider — session name and --effort/--model already rendered).
func TestNewSessionArgs_nofileCap(t *testing.T) {
	argv := []string{"claude", "--remote-control", "proj-x", "--effort", "max", "--model", "opus[1m]"}

	capped := New("tmux", WithNofileCap("prlimit", 4096))
	got := capped.newSessionArgs("proj-x", "/work/x", argv, nil)
	want := []string{
		"new-session", "-d", "-s", "proj-x", "-c", "/work/x",
		"prlimit", "--nofile=4096:4096", "--",
		"claude", "--remote-control", "proj-x",
		"--effort", "max", "--model", "opus[1m]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("capped newSessionArgs =\n  %q\nwant\n  %q", got, want)
	}

	// Zero cap (the default) spawns bare — no wrapper, just the argv.
	bare := New("tmux")
	got = bare.newSessionArgs("proj-x", "/work/x", argv, nil)
	want = []string{
		"new-session", "-d", "-s", "proj-x", "-c", "/work/x",
		"claude", "--remote-control", "proj-x",
		"--effort", "max", "--model", "opus[1m]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("bare newSessionArgs =\n  %q\nwant\n  %q", got, want)
	}
}

// extraEnv entries become `-e KEY=VALUE` flags on new-session — after the
// target/dir flags, BEFORE the prlimit wrapper and inner command, so the
// environment applies to the pane while the token value stays out of the
// pane command's argv (and so out of `ps` output).
func TestNewSessionArgs_extraEnvFlags(t *testing.T) {
	argv := []string{"claude", "--remote-control", "repo~20260608-1530"}
	env := []string{"LAB_URL=http://127.0.0.1:8080", "LAB_TOKEN=lab_run_secret"}

	tm := New("tmux", WithNofileCap("prlimit", 1024))
	got := tm.newSessionArgs("repo~20260608-1530", "/work/wt", argv, env)
	want := []string{
		"new-session", "-d", "-s", "repo~20260608-1530", "-c", "/work/wt",
		"-e", "LAB_URL=http://127.0.0.1:8080",
		"-e", "LAB_TOKEN=lab_run_secret",
		"prlimit", "--nofile=1024:1024", "--",
		"claude", "--remote-control", "repo~20260608-1530",
	}
	if !slices.Equal(got, want) {
		t.Errorf("newSessionArgs with env =\n  %q\nwant\n  %q", got, want)
	}
}

// nofile <= 0 disables the cap entirely (v0 `-session-nofile` 0-disables).
func TestNofileCapArgv_zeroAndNegativeDisable(t *testing.T) {
	for _, n := range []int{0, -1} {
		tm := New("tmux", WithNofileCap("prlimit", n))
		if got := tm.nofileCapArgv(); got != nil {
			t.Errorf("nofile=%d: nofileCapArgv() = %q; want nil", n, got)
		}
	}
	tm := New("tmux", WithNofileCap("prlimit", 16384))
	want := []string{"prlimit", "--nofile=16384:16384", "--"}
	if got := tm.nofileCapArgv(); !slices.Equal(got, want) {
		t.Errorf("nofileCapArgv() = %q; want %q", got, want)
	}
}

// The private-socket option prefixes every tmux invocation with -L.
func TestCmd_privateSocketFlag(t *testing.T) {
	tm := New("tmux", WithSocket("lab-test-sock"))
	cmd := tm.cmd(t.Context(), "has-session", "-t", "=x")
	want := []string{"tmux", "-L", "lab-test-sock", "has-session", "-t", "=x"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd args = %q; want %q", cmd.Args, want)
	}

	bare := New("tmux")
	cmd = bare.cmd(t.Context(), "list-sessions", "-F", "#{session_name}")
	want = []string{"tmux", "list-sessions", "-F", "#{session_name}"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd args without socket = %q; want %q", cmd.Args, want)
	}
}
