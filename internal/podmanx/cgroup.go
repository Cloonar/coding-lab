package podmanx

// The container cgroup layout (ADR-0059, issue #237).
//
// Every container lands under a payload cgroup pinned by --cgroup-parent so
// its --memory/--pids-limit caps bind to a subtree lab owns and preflight
// verified (#205's no-false-green rule). The hard part is WHERE that subtree
// lives: a cgroup that delegates controllers may hold no processes of its own
// (cgroup v2's no-internal-process rule), yet systemd spawns a service's main
// PID DIRECTLY into that service's own cgroup root.
//
// ADR-0058 tried to dodge that with Delegate=yes + DelegateSubgroup=main on
// lab.service itself, expecting systemd to spawn lab into a proc-free main/
// subgroup and leave the unit root free to carry the payload controllers. It
// does NOT work: DelegateSubgroup= only redirects CONTROL processes, not the
// main ExecStart spawn — src/core/execute.c exec_params_needs_control_subcgroup
// requires EXEC_CGROUP_DELEGATE|EXEC_CONTROL_CGROUP|EXEC_IS_CONTROL all set,
// and the main process is not a control process. systemd clone3(CLONE_INTO_
// CGROUP)s the new main PID into the unit cgroup ROOT and sd-executor only
// migrates itself into main/ afterwards; when the root already delegates
// controllers the kernel refuses that spawn with EBUSY, and systemd does NOT
// retry it (posix_spawn_wrapper retries only ENOTSUP/EPERM). With
// KillMode=process keeping container survivors populating and re-arming the
// root across restarts, EVERY lab.service restart after any container had run
// died with "Failed to spawn executor: Device or resource busy" (the
// 2026-07-26 dev-new incident, issue #237). Any design that keeps controllers
// enabled on lab.service's OWN cgroup root across restarts is dead.
//
// ADR-0059 moves the delegation OUT of lab.service entirely, into a separate,
// never-restarted "holder" unit (lab-payload.service):
//
//	/sys/fs/cgroup/system.slice/
//	├── lab.service/                 plain, UNDELEGATED — lab, tmux, all host
//	│                                processes; its subtree_control MUST stay
//	│                                empty forever (else the next restart EBUSYs)
//	└── lab-payload.service/         holder: delegated root (Delegate=yes,
//	    │                            User=lab), NEVER restarted; subtree_control
//	    │                            carries +memory +pids (lab preflight enables)
//	    ├── main/                    holder's own sleep (DelegateSubgroup=main
//	    │                            keeps the delegated root proc-free)
//	    └── payload/                 every container via --cgroup-parent
//	        └── libpod-*/            per-container leaves; the caps bind here
//
// lab.service never delegates, so systemd's spawn into its cgroup root is
// always a legal spawn into a proc-only cgroup — restart-safe forever. The
// holder owns the delegation and is never restarted (a holder restart with
// surviving containers would hit the very same EBUSY trap the whole design
// avoids — hence restartIfChanged=false), so its delegated root, and the
// containers' caps under it, survive across every lab restart.
// DelegateSubgroup=main works on the holder because the holder's sleep is
// spawned once and never re-attached under an armed root.
//
// setupCgroups establishes the payload subtree inside the holder at preflight
// (holder delegated + writable, +memory +pids enabled on its root, payload/
// created and confirmed to carry those controllers). Verify re-checks, before
// EVERY container spawn, the two invariants whose violation is silent and
// catastrophic: lab.service's own subtree_control must stay empty (something
// re-armed the trap → the next restart EBUSYs) and the payload cgroup must
// still carry memory+pids (the holder was restarted/stopped and lost its
// delegation → the caps would silently not bind). The nix module's
// lab-cgroup-hygiene oneshot clears any stray delegation under lab.service
// before each start as the last line of defense.

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// cgroupFSRoot is where the unified hierarchy is mounted.
const cgroupFSRoot = "/sys/fs/cgroup"

// holderCgroup is the lab-payload.service holder unit's delegated cgroup root,
// as a cgroupfs path (ADR-0059). Hardcoded, not derived from lab's own cgroup:
// the delegation deliberately lives OUTSIDE lab.service so no lab restart ever
// spawns into a delegating cgroup. The holder is pinned to system.slice by the
// nix module and never restarted.
const holderCgroup = "/system.slice/lab-payload.service"

// payloadCgroup is the container subtree under the holder — RunSpec.CgroupParent's
// value, the same subtree preflight verifies delegation on. Its name is the
// counterpart of the holder's DelegateSubgroup=main.
const payloadCgroup = holderCgroup + "/payload"

// holderMainSubgroup is where stray processes found at the holder root are
// adopted (systemd < 254 ignoring DelegateSubgroup, or a start-up race): the
// holder root must be proc-free before its controllers can be enabled.
const holderMainSubgroup = "main"

// Cgroups is the established container cgroup layout (ADR-0059), published on
// the preflight Result so every spawn site reads one source: Parent becomes
// the argv's --cgroup-parent, Verify is the pre-spawn restart-safety guard.
// Built by setupCgroups; a hand-built value with an empty LabDir (tests,
// degenerate wiring) verifies vacuously — only Preflight-produced Results
// carry the full layout.
type Cgroups struct {
	// Parent is the payload cgroup as a cgroupfs path
	// ("/system.slice/lab-payload.service/payload") — RunSpec.CgroupParent's value.
	Parent string
	// LabDir is lab's OWN cgroup directory under /sys/fs/cgroup (from
	// /proc/self/cgroup) — the plain, undelegated lab.service cgroup whose
	// cgroup.subtree_control must stay empty forever, else the next lab
	// restart EBUSYs.
	LabDir string
	// PayloadDir is the payload cgroup directory under /sys/fs/cgroup, inside
	// the holder — the subtree the container caps bind to, which must keep
	// carrying memory+pids across the holder's lifetime.
	PayloadDir string

	// readFile is Verify's probe, injected by setupCgroups (Deps.ReadFile);
	// nil falls back to os.ReadFile.
	readFile func(string) ([]byte, error)
}

// Verify is the restart-safety guard (ADR-0059), run at preflight and again
// before EVERY container spawn: it re-checks the two invariants whose
// violation is both silent and catastrophic. Detecting them at spawn time
// turns "the next deploy bricks the service" (or "the caps silently stopped
// binding") into an immediate, actionable refusal. Nil receiver or empty
// LabDir (a layout never established — hand-built Results) verifies vacuously.
func (c *Cgroups) Verify() error {
	if c == nil || c.LabDir == "" {
		return nil
	}
	readFile := c.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	// (a) lab.service's own cgroup must delegate nothing. systemd spawns
	// lab's main PID directly here on every start; a non-empty subtree_control
	// means something re-armed the trap (a podman heuristic, a stray delegate)
	// and the NEXT restart would fail with EBUSY. Nothing may ever delegate
	// under lab.service.
	stc := c.LabDir + "/cgroup.subtree_control"
	b, err := readFile(stc)
	if err != nil {
		return fmt.Errorf("reading %s: %w", stc, err)
	}
	if ctrl := strings.TrimSpace(string(b)); ctrl != "" {
		return fmt.Errorf("lab's own cgroup %s delegates controllers (%q) — the next lab restart would fail with EBUSY (systemd spawns lab's main PID directly here, and cgroup v2 forbids processes in a delegating cgroup); nothing may delegate under lab.service. Restarting lab clears it (lab-cgroup-hygiene)", c.LabDir, ctrl)
	}
	// (b) the payload cgroup must still carry both caps' controllers. If the
	// lab-payload.service holder was restarted or stopped, its fresh cgroup
	// lost the delegation preflight established — and --memory/--pids-limit
	// would then silently not bind (the #205 no-false-green lesson).
	ctrlFile := c.PayloadDir + "/cgroup.controllers"
	b, err = readFile(ctrlFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", ctrlFile, err)
	}
	if have := strings.Fields(string(b)); !slices.Contains(have, "memory") || !slices.Contains(have, "pids") {
		return fmt.Errorf("the payload cgroup %s carries only %q — the lab-payload.service holder lost its memory/pids delegation (restarted or stopped), so the container caps would silently not bind; restarting the lab service re-establishes the payload delegation", c.PayloadDir, strings.TrimSpace(string(b)))
	}
	return nil
}

// setupCgroups establishes the ADR-0059 layout inside the lab-payload.service
// holder and returns it, or the preflight Failure that explains what the host
// is missing. ownPath (the unified-hierarchy path from /proc/self/cgroup)
// feeds ONLY LabDir now — the payload parent is hardcoded under the holder,
// never derived from lab's own cgroup:
//
//  1. Holder delegated + writable: systemd's Delegate=yes + User=lab chowns
//     the holder's cgroup dir (and its cgroup.procs/subtree_control) to the
//     lab user, so write access IS the delegation signal — and a missing dir
//     (holder absent or not running) fails the same probe.
//  2. Adopt strays: any process still sitting at the holder ROOT (systemd
//     < 254 ignoring DelegateSubgroup, or a start-up race) is moved into the
//     holder's main/ subgroup so the root is proc-free — best-effort per pid
//     (a race with an exit is benign; if adoption doesn't stick, step 3 fails
//     loudly, since the kernel refuses controller delegation on a populated
//     cgroup).
//  3. Enable +memory +pids on the holder root's subtree_control so the
//     payload subtree can carry the --memory/--pids-limit caps.
//  4. Create the payload cgroup and confirm the controllers actually arrived
//     in it — the no-false-green posture aimed at the subtree containers land in.
func setupCgroups(ownPath string, d Deps) (*Cgroups, *Failure) {
	ownPath = strings.TrimSuffix(ownPath, "/")
	holderDir := cgroupFSRoot + holderCgroup
	payloadDir := cgroupFSRoot + payloadCgroup
	c := &Cgroups{
		Parent:     payloadCgroup,
		LabDir:     cgroupFSRoot + ownPath,
		PayloadDir: payloadDir,
		readFile:   d.ReadFile,
	}

	// Step 1. Write access to the holder root is the delegation signal.
	holderHint := "the lab-payload.service holder unit must be running with Delegate=yes + DelegateSubgroup=main and the lab service user (the NixOS module ships it with container.enable) — check systemctl status lab-payload.service"
	if d.Writable != nil && !d.Writable(holderDir) {
		return nil, &Failure{Check: CheckHolder, Detail: fmt.Sprintf("the holder cgroup %s is not delegated to the lab user (the lab-payload.service holder is missing or not running)", holderDir), Hint: holderHint}
	}

	// Step 2. Adopt strays into the holder's main/ subgroup. Only touched when
	// the holder root actually holds processes — in the healthy case
	// DelegateSubgroup=main already keeps the sleep in main/ and the root
	// proc-free, so systemd owns main/ and this is a no-op.
	if procs, err := d.ReadFile(holderDir + "/cgroup.procs"); err == nil && strings.TrimSpace(string(procs)) != "" {
		mainDir := holderDir + "/" + holderMainSubgroup
		_ = d.Mkdir(mainDir, 0o755) // MkdirAll-shaped; an existing dir is success
		for line := range strings.Lines(string(procs)) {
			if pid := strings.TrimSpace(line); pid != "" {
				_ = d.WriteFile(mainDir+"/cgroup.procs", []byte(pid), 0)
			}
		}
	}

	// Steps 3 & 4. Delegation into the payload subtree — the false-green check
	// retargeted at the subtree containers really land in.
	delegHint := "the lab-payload.service holder is not delegating memory/pids into the payload cgroup — restart the lab service to re-establish it (check systemctl status lab-payload.service)"
	if err := d.WriteFile(holderDir+"/cgroup.subtree_control", []byte("+memory +pids"), 0); err != nil {
		return nil, &Failure{Check: CheckDelegation, Detail: fmt.Sprintf("cannot enable memory/pids on %s: %v", holderCgroup, err), Hint: delegHint}
	}
	if err := d.Mkdir(payloadDir, 0o755); err != nil {
		return nil, &Failure{Check: CheckDelegation, Detail: fmt.Sprintf("cannot create payload cgroup %s: %v", payloadDir, err), Hint: delegHint}
	}
	ctrlFile := payloadDir + "/cgroup.controllers"
	if ctrl, err := d.ReadFile(ctrlFile); err != nil {
		return nil, &Failure{Check: CheckDelegation, Detail: fmt.Sprintf("cannot read %s: %v", ctrlFile, err), Hint: delegHint}
	} else if have := strings.Fields(string(ctrl)); !slices.Contains(have, "memory") || !slices.Contains(have, "pids") {
		return nil, &Failure{Check: CheckDelegation, Detail: fmt.Sprintf("payload cgroup %s carries only %q — memory and pids are required for the container caps", c.Parent, strings.TrimSpace(string(ctrl))), Hint: delegHint}
	}
	return c, nil
}
