//go:build linux

package labctl

// Tests for container-entry's init loop (superviseProvider) with REAL
// processes. No root is needed: the child-subreaper flag makes orphans
// reparent to the supervising process even though it is not PID 1.
//
// Every test runs the loop in a re-exec'd copy of the test binary
// (TestEntryHelperProcess), never in the test process itself: the loop
// reaps with wait4(-1) and sets the subreaper flag for the whole process, so
// in-process it would steal the children other tests start with os/exec and
// adopt their orphans. The subprocess also lets the forwarding tests send
// real signals to a real process.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const entryHelperEnv = "LABCTL_ENTRY_HELPER"

// TestEntryHelperProcess is not a test: re-exec'd with entryHelperEnv set,
// it runs superviseProvider on the argv after `--` and exits with its code.
func TestEntryHelperProcess(t *testing.T) {
	if os.Getenv(entryHelperEnv) != "1" {
		return
	}
	argv := os.Args
	for i, a := range os.Args {
		if a == "--" {
			argv = os.Args[i+1:]
			break
		}
	}
	os.Exit(superviseProvider(argv, os.Stderr))
}

// supervisedCmd builds the helper subprocess supervising argv.
func supervisedCmd(t *testing.T, argv ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestEntryHelperProcess$", "--"}, argv...)...)
	cmd.Env = append(os.Environ(), entryHelperEnv+"=1")
	return cmd
}

// runSupervised runs argv under the loop to completion and returns the
// loop's exit code and output.
func runSupervised(t *testing.T, argv ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := supervisedCmd(t, argv...)
	var out, errw strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errw
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("helper: %v", err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errw.String()
}

// TestSuperviseRelaysExitStatus: the provider's exit status passes through
// verbatim, and a signal death becomes 128+signal.
func TestSuperviseRelaysExitStatus(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   int
	}{
		{"exit 0", "exit 0", 0},
		{"exit 7", "exit 7", 7},
		{"exit 2 is the child's, not usage", "exit 2", 2},
		{"killed by SIGKILL", "kill -KILL $$", 128 + 9},
		{"killed by SIGTERM", "kill -TERM $$", 128 + 15},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runSupervised(t, "sh", "-c", tc.script)
			if code != tc.want {
				t.Errorf("code %d, want %d (stderr %q)", code, tc.want, stderr)
			}
			if stderr != "" {
				t.Errorf("stderr %q, want none", stderr)
			}
		})
	}
}

// TestSuperviseStartFailure: a provider that cannot be resolved is a named
// step failure, exit 1.
func TestSuperviseStartFailure(t *testing.T) {
	code, _, stderr := runSupervised(t, "labctl-no-such-provider")
	want := "labctl container-entry: start provider: exec: \"labctl-no-such-provider\": executable file not found in $PATH\n"
	if code != 1 || stderr != want {
		t.Errorf("code %d stderr %q, want 1 and %q", code, stderr, want)
	}
}

// TestSuperviseInheritsEnvAndStdio: the provider sees the environment
// untouched and writes to the loop's own stdout and stderr.
func TestSuperviseInheritsEnvAndStdio(t *testing.T) {
	cmd := supervisedCmd(t, "sh", "-c", `echo "out:$LABCTL_ENTRY_PROBE"; echo "err:$LABCTL_ENTRY_PROBE" >&2`)
	cmd.Env = append(cmd.Env, "LABCTL_ENTRY_PROBE=s3cr3t-by-name")
	var out, errw strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errw
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v (stderr %q)", err, errw.String())
	}
	if out.String() != "out:s3cr3t-by-name\n" || errw.String() != "err:s3cr3t-by-name\n" {
		t.Errorf("stdout %q stderr %q", out.String(), errw.String())
	}
}

// TestSuperviseForwardsSignals: each forwarded signal sent to the loop
// reaches the provider, whose trap turns it into exit 42 — relayed.
func TestSuperviseForwardsSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH} {
		t.Run(unix.SignalName(sig), func(t *testing.T) {
			// The child prints "ready" once its trap is set. It is started
			// only after the loop subscribed to the signals, so "ready"
			// also means the loop will catch what the test sends.
			script := fmt.Sprintf("trap 'exit 42' %d; echo ready; while :; do sleep 0.02; done", int(sig))
			cmd := supervisedCmd(t, "sh", "-c", script)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var errw strings.Builder
			cmd.Stderr = &errw
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != "ready\n" {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("waiting for ready: %q, %v (stderr %q)", line, err, errw.String())
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, stdout)
			_ = cmd.Wait()
			if code := cmd.ProcessState.ExitCode(); code != 42 {
				t.Errorf("code %d, want 42 from the provider's trap (state %v, stderr %q)", code, cmd.ProcessState, errw.String())
			}
		})
	}
}

// TestSuperviseReapsOrphans: an orphan that exits while the provider is
// still running is reaped by the loop, not left a zombie. The provider
// starts a grandchild through a shell that exits at once (so it reparents to
// the loop), waits until well after the grandchild has exited, and reports
// whether its /proc entry — which a zombie keeps — is gone.
func TestSuperviseReapsOrphans(t *testing.T) {
	script := `g=$(sh -c 'sleep 0.1 >/dev/null 2>&1 & echo $!')
sleep 0.8
if [ -e /proc/$g ]; then echo "zombie $g: $(cat /proc/$g/stat)"; exit 9; fi
exit 3`
	code, stdout, stderr := runSupervised(t, "sh", "-c", script)
	if code != 3 {
		t.Errorf("code %d, want 3 (orphan reaped); stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestSuperviseDoesNotWaitForOrphans: the loop leaves as soon as the
// provider is reaped; an orphan still running does not hold it up.
func TestSuperviseDoesNotWaitForOrphans(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "orphan")
	script := fmt.Sprintf(`sleep 2 >/dev/null 2>&1 & echo $! > %s; exit 5`, pidFile)
	start := time.Now()
	code, _, stderr := runSupervised(t, "sh", "-c", script)
	if code != 5 {
		t.Errorf("code %d, want 5 (stderr %q)", code, stderr)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("loop took %v: it waited for the orphan", d)
	}
	if b, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// TestSuperviseForegroundOnTTY: when the loop runs on a controlling
// terminal in its foreground group, the provider gets its own process group
// and becomes the terminal's foreground group (catatonit's behaviour under
// --init); without a terminal it stays in the loop's group.
func TestSuperviseForegroundOnTTY(t *testing.T) {
	// /proc/<pid>/stat fields: 1 pid, 5 pgrp, 8 tpgid (comm "(sh)" has no
	// spaces).
	const report = `set -- $(cat /proc/$$/stat); echo "$1 $5 $8"; set -- $(cat /proc/$PPID/stat); echo "$5"`

	t.Run("no tty", func(t *testing.T) {
		code, stdout, stderr := runSupervised(t, "sh", "-c", report)
		if code != 0 {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
		lines := strings.Fields(stdout)
		if len(lines) != 4 || lines[1] != lines[3] {
			t.Errorf("provider pid/pgrp/tpgid + loop pgrp = %q: want the provider in the loop's group", stdout)
		}
	})

	t.Run("controlling tty", func(t *testing.T) {
		master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			t.Skipf("no pty: %v", err)
		}
		defer func() { _ = master.Close() }()
		if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
			t.Skipf("unlockpt: %v", err)
		}
		n, err := unix.IoctlGetUint32(int(master.Fd()), unix.TIOCGPTN)
		if err != nil {
			t.Skipf("ptsname: %v", err)
		}
		tty, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			t.Skipf("open pts: %v", err)
		}
		defer func() { _ = tty.Close() }()
		go func() { _, _ = io.Copy(io.Discard, master) }()

		out := filepath.Join(t.TempDir(), "report")
		cmd := supervisedCmd(t, "sh", "-c", "{ "+report+"; } > "+out)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		// A new session with the pty as its controlling terminal: the
		// loop is then the terminal's foreground group, as in a pane.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		if err := cmd.Run(); err != nil {
			t.Fatalf("helper: %v", err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(string(b))
		if len(f) != 4 || f[0] != f[1] || f[0] != f[2] || f[3] == f[1] {
			t.Errorf("provider pid/pgrp/tpgid + loop pgrp = %q: want the provider leading its own, foreground group", string(b))
		}
	})
}
