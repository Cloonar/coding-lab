//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// prctl(2) constants for the ambient set (linux/prctl.h). Not in package
// syscall, and golang.org/x/sys is not worth a dependency for two integers.
const (
	prCapAmbient         = 47 // PR_CAP_AMBIENT
	prCapAmbientClearAll = 4  // PR_CAP_AMBIENT_CLEAR_ALL
)

// clearAmbientCaps clears the ambient set of every thread of the process and
// returns the mask it held before (0 = nothing to do, no syscall made).
//
// Every thread, via syscall.AllThreadsSyscall, because Linux capabilities
// are per-thread: a plain prctl would clear only the OS thread this goroutine
// happens to run on, and a child forked later from any other runtime thread
// would still inherit the set. AllThreadsSyscall refuses (ENOTSUP) in a cgo
// build, where the runtime does not own every thread; lab ships with
// CGO_ENABLED=0 (nix/package.nix), so that only bites a local cgo build, and
// the caller logs it.
func clearAmbientCaps() (uint64, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("reading /proc/self/status: %w", err)
	}
	amb, err := parseCapAmb(string(status))
	if err != nil {
		return 0, err
	}
	if amb == 0 {
		return 0, nil
	}
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0); errno != 0 {
		return amb, fmt.Errorf("prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL): %w", errno)
	}
	return amb, nil
}
