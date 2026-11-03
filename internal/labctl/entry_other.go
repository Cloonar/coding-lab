//go:build !linux

package labctl

import "fmt"

// runContainerEntry refuses off Linux: container-entry is the PID 1 of a
// Linux container (cgroup v2, setresuid, a child subreaper), and labctl's
// release builds are Linux-only (ADR-0065). The argument handling and the
// setup steps in entry.go still compile and test everywhere.
func runContainerEntry(_ []string, env Env) int {
	_, _ = fmt.Fprintln(env.Stderr, "labctl container-entry: only supported on Linux")
	return 1
}
