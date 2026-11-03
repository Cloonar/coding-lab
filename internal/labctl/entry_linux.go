//go:build linux

package labctl

// The Linux half of `labctl container-entry` (see entry.go): the real
// privileged operations, and the init loop that runs the provider.
//
// The loop reaps with wait4(-1, WNOHANG) driven by SIGCHLD, so it must be the
// ONLY waiter in the process: the provider is started with os.StartProcess and
// its handle released at once, never Wait()ed, or the two would race for the
// provider's exit status. (That is also why its tests run it in a re-exec'd
// subprocess: in the test binary it would steal other tests' children.)

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// forwardedSignals reach the provider: the stop signal the outer argv pins
// (--stop-signal), whatever tmux's teardown proxies through the podman
// client, and window-size changes.
var forwardedSignals = []os.Signal{
	syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGWINCH,
}

// runContainerEntry is the `labctl container-entry` verb. The hidden probe
// argument (entry_selftest_linux.go) is dispatched before any flag parsing,
// so it never shows up in --help.
func runContainerEntry(args []string, env Env) int {
	if len(args) > 0 && args[0] == selfTestProbeArg {
		return runSelfTestProbe(args[1:], env.Stderr)
	}
	return containerEntry(args, env, realEntrySys(), superviseProvider, realSelfTestChecks)
}

// realEntrySys wires entrySys to the real process and file system. Go's
// syscall.Setgroups/Setresgid/Setresuid apply to every OS thread of the
// process on Linux (they go through the runtime's all-threads path), so no
// thread keeps uid 0 behind the drop. That path refuses in a cgo build;
// labctl ships with CGO_ENABLED=0 (ADR-0065).
func realEntrySys() entrySys {
	return entrySys{
		Root:      "/",
		Getuid:    os.Getuid,
		Geteuid:   os.Geteuid,
		Getgid:    os.Getgid,
		Getegid:   os.Getegid,
		Getgroups: os.Getgroups,
		ReadFile:  os.ReadFile,
		WriteFile: writeFile0644,
		Lchown:    os.Lchown,
		Setgroups: syscall.Setgroups,
		Setresgid: syscall.Setresgid,
		Setresuid: syscall.Setresuid,
	}
}

// writeFile0644 replaces path's content with data and leaves it mode 0644
// whatever mode a pre-existing file had. It writes in place (no rename), so
// a bind-mounted file is written through rather than refused.
func writeFile0644(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// superviseProvider runs argv as the child of an init loop and returns the
// code to exit with: the provider's exit status, 128+signal when a signal
// killed it, or 1 (with the step on stderr) when it could not be started.
//
// It runs after the privilege drop, so argv[0] resolves on PATH as the
// session user. The child gets the environment untouched and this process's
// own stdin/stdout/stderr (never pipes), so the pane's pty stays the
// provider's. When stdin is the controlling terminal and this process is in
// its foreground process group, the provider gets a process group of its own
// and becomes the terminal's foreground group, as catatonit does under
// --init (ADR-0074): keyboard signals and SIGWINCH then reach the provider
// once, from the terminal, instead of twice (directly and forwarded).
//
// The process becomes a child subreaper, so orphans of the provider are
// reparented to it even when it is not PID 1 (as PID 1 it is redundant and
// harmless); SIGCHLD and the forwarded signals are subscribed BEFORE the
// child starts, so no exit and no signal is missed.
func superviseProvider(argv []string, stderr io.Writer) int {
	fail := func(step string, err error) int {
		_, _ = fmt.Fprintf(stderr, "labctl container-entry: %s: %v\n", step, err)
		return 1
	}

	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fail("start provider", err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fail("become child subreaper", err)
	}

	chld := make(chan os.Signal, 1)
	signal.Notify(chld, syscall.SIGCHLD)
	defer signal.Stop(chld)
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, forwardedSignals...)
	defer signal.Stop(sigs)

	proc, err := os.StartProcess(path, argv, &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys:   providerSysProcAttr(),
	})
	if err != nil {
		return fail("start provider", err)
	}
	pid := proc.Pid
	// The handle is never waited on (the loop reaps by pid); releasing it
	// frees the pidfd the runtime may hold for it.
	_ = proc.Release()

	code, err := initLoop(pid, chld, sigs)
	if err != nil {
		return fail("reap", err)
	}
	return code
}

// providerSysProcAttr puts the provider in its own, foreground process group
// when fd 0 is this process's controlling terminal and this process is in
// the terminal's foreground group; otherwise (no terminal, a test, a
// background start) the provider stays in this process's group.
func providerSysProcAttr() *syscall.SysProcAttr {
	fg, err := unix.IoctlGetUint32(0, unix.TIOCGPGRP)
	if err != nil || int(fg) != syscall.Getpgrp() {
		return nil
	}
	// Ctty is the parent's descriptor for Foreground (unlike Setctty).
	return &syscall.SysProcAttr{Foreground: true, Ctty: 0}
}

// initLoop reaps every child and forwards signals to pid until pid itself has
// been reaped, then returns its exit code. Each pass reaps first and blocks
// only afterwards, and SIGCHLD is buffered, so an exit between the reap pass
// and the select still wakes the loop. A signal is forwarded only while pid
// has not been reaped, so it can never reach a recycled pid.
func initLoop(pid int, chld, sigs <-chan os.Signal) (int, error) {
	for {
		code, exited, err := reapChildren(pid)
		if err != nil {
			return 0, err
		}
		if exited {
			return code, nil
		}
		select {
		case <-chld:
		case s := <-sigs:
			if sig, ok := s.(syscall.Signal); ok {
				_ = syscall.Kill(pid, sig)
			}
		}
	}
}

// reapChildren reaps every child that has exited, without blocking, and
// reports whether pid was among them and with which code. It drains all
// zombies on every call, so the pass that reaps pid is also the final one for
// orphans that already exited; orphans still running then die with the
// container (PID 1 is leaving).
func reapChildren(pid int) (code int, exited bool, err error) {
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			if !exited {
				return 0, false, fmt.Errorf("provider pid %d vanished without being reaped", pid)
			}
			return code, true, nil
		case err != nil:
			return 0, false, fmt.Errorf("wait4: %w", err)
		case wpid <= 0:
			return code, exited, nil
		case wpid == pid:
			code, exited = waitStatusCode(ws), true
		}
	}
}

// waitStatusCode is the shell convention for a child's end: its exit status,
// or 128+signal when a signal killed it.
func waitStatusCode(ws syscall.WaitStatus) int {
	switch {
	case ws.Exited():
		return ws.ExitStatus()
	case ws.Signaled():
		return 128 + int(ws.Signal())
	default:
		return 1
	}
}
