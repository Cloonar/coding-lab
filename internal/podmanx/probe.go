package podmanx

// The preflight spawn probe (ADR-0060, issue #239).
//
// Under --cgroup-manager=systemd every container is a transient scope,
// libpod-<id>.scope, under the lab user's systemd user manager
// (user@<uid>.service, kept alive by lingering). The one operation that can
// still fail on a misconfigured host is the attach itself: conmon+crun ask
// the user manager for the scope, and the manager forwards the cgroup
// migration to PID 1 — which is precisely why this design escapes cgroup
// v2's delegation containment rule (an unprivileged process migrating a PID
// needs write access to the COMMON ANCESTOR's cgroup.procs; system.slice is
// root-owned, so every hand-placed layout ADR-0058/0059 built EACCESed at
// `podman create` no matter how correctly lab delegated its own subtree —
// the 2026-07-26 dev-new incident).
//
// The retired preflights verified controllers, ownership and delegation, yet
// never performed an actual PID migration — the single operation that
// failed. That is a textbook false green (issue #205's rule), so the check
// is now the real thing: create a probe container, init it (the OCI create —
// the attach), and read the caps back out of the scope's own cgroup files.
// Nothing is inferred that can be observed.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// cgroupFSRoot is where the unified hierarchy is mounted; the probe resolves
// the scope's cgroup files under it.
const cgroupFSRoot = "/sys/fs/cgroup"

// probeName deliberately does NOT carry the labrun- prefix: the startup
// orphan sweep (ListRunContainers consumers) removes every unaccounted
// labrun-* container and could race a probe mid-flight, turning a green host
// into a flaky spawn-probe failure. A leaked probe (crash between create and
// cleanup) is instead reclaimed by the next preflight's own pre-clean — the
// name is deterministic, so "the previous probe" is always addressable.
const probeName = "lab-preflight-probe"

// The probe's caps: small enough to cost nothing, distinctive enough that a
// default value could never be mistaken for them. probeMemoryBytes is 64m as
// the kernel renders it in memory.max.
const (
	probeMemory      = "64m"
	probeMemoryBytes = "67108864"
	probePids        = "16"
)

// spawnProbe is the preflight's final, load-bearing check: a REAL container
// spawn through the systemd cgroup manager, asserting the caps verifiably
// landed in the transient scope's cgroup. Steps, all through d.Run / d.ReadFile
// so the test fixture fakes the host:
//
//  1. Pre-clean the deterministic name (error ignored — a real problem
//     resurfaces at create).
//  2. `create` with the run shapes' manager flag and the probe caps. The
//     command is /bin/labctl — the static binary every agent-tools image
//     carries — but it is a PLACEHOLDER argv that is never executed (see
//     step 3), so the tools images' "never run as a container" contract is
//     preserved in spirit: no process in them ever execs.
//  3. `init`: the OCI create. conmon+crun ask the user manager to create
//     libpod-<id>.scope and PLACE the container's init process in it, paused
//     before exec — exactly the unprivileged cross-cgroup attach that failed
//     in the incident, and exactly what the old preflight never exercised.
//  4. `inspect` for the pid and cgroup path; prefer CgroupPath, fall back to
//     /proc/<pid>/cgroup's 0:: entry. Normalize to the libpod-*.scope
//     component (crun may report a child cgroup inside the scope, e.g.
//     …scope/container; the caps live on the scope itself). A path WITHOUT
//     such a component means the systemd manager did not engage — podman
//     fell back to cgroupfs placement — which is a failure even if the
//     container "runs": the caps would land somewhere unverified.
//  5. Read memory.max and pids.max out of the scope cgroup and compare —
//     the no-false-green assertion.
//  6. Cleanup, deferred, success or failure (error ignored; --ignore makes
//     the already-gone case a no-op).
//
// Every failure is one CheckSpawnProbe Failure whose Detail carries the step
// and podman's own stderr (ExecRunner folds stderr into the error).
func spawnProbe(ctx context.Context, bin, ref string, d Deps) *Failure {
	hint := fmt.Sprintf("check that user@%d.service is running and that podman can reach the user manager (XDG_RUNTIME_DIR=/run/user/%d, DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus) — lab sets both at startup", d.UID, d.UID, d.UID)
	fail := func(detail string) *Failure {
		return &Failure{Check: CheckSpawnProbe, Detail: detail, Hint: hint}
	}

	_, _ = d.Run(ctx, bin, "rm", "--force", "--ignore", "--time", "0", probeName)
	defer func() {
		_, _ = d.Run(ctx, bin, "rm", "--force", "--ignore", "--time", "0", probeName)
	}()

	if _, err := d.Run(ctx, bin, "--cgroup-manager=systemd", "create", "--name", probeName,
		"--memory", probeMemory, "--pids-limit", probePids, ref, "/bin/labctl"); err != nil {
		return fail(fmt.Sprintf("probe container create failed: %v", err))
	}
	if _, err := d.Run(ctx, bin, "init", probeName); err != nil {
		return fail(fmt.Sprintf("probe container init failed (the user manager refused to create or populate the container's transient scope): %v", err))
	}

	out, err := d.Run(ctx, bin, "inspect", "--format", "{{.State.Pid}} {{.State.CgroupPath}}", probeName)
	if err != nil {
		return fail(fmt.Sprintf("probe container inspect failed: %v", err))
	}
	pidStr, cgPath, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if cgPath == "" {
		pid, perr := strconv.Atoi(pidStr)
		if perr != nil || pid <= 0 {
			return fail(fmt.Sprintf("probe container inspect reported neither a cgroup path nor a live pid (%q)", strings.TrimSpace(string(out))))
		}
		procCg := fmt.Sprintf("/proc/%d/cgroup", pid)
		b, err := d.ReadFile(procCg)
		if err != nil {
			return fail(fmt.Sprintf("reading %s: %v", procCg, err))
		}
		p, ok := unifiedCgroupPath(b)
		if !ok {
			return fail(fmt.Sprintf("no unified (0::) entry in %s", procCg))
		}
		cgPath = p
	}
	scopePath, ok := scopeCgroupPath(cgPath)
	if !ok {
		return fail(fmt.Sprintf("probe container's cgroup %q has no libpod-*.scope component — it did not land in a transient scope, so the systemd cgroup manager did not engage (e.g. podman fell back to cgroupfs placement)", cgPath))
	}
	for _, c := range []struct{ file, want string }{
		{"memory.max", probeMemoryBytes},
		{"pids.max", probePids},
	} {
		path := cgroupFSRoot + scopePath + "/" + c.file
		b, err := d.ReadFile(path)
		if err != nil {
			return fail(fmt.Sprintf("reading %s: %v", path, err))
		}
		if got := strings.TrimSpace(string(b)); got != c.want {
			return fail(fmt.Sprintf("%s = %q, want %q — the probe's cap did not land in its scope cgroup", path, got, c.want))
		}
	}
	return nil
}

// scopeCgroupPath truncates a container's cgroup path immediately after its
// libpod-*.scope component — the transient scope the caps are set on. crun
// may report a deeper leaf inside the scope; anything below it is the
// runtime's own business. No such component means no scope: (,false).
func scopeCgroupPath(path string) (string, bool) {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, "libpod-") && strings.HasSuffix(p, ".scope") {
			return strings.Join(parts[:i+1], "/"), true
		}
	}
	return "", false
}
