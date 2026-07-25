package podmanx

// The container cgroup layout (ADR-0058). Containers used to ride
// `--cgroups=split`, which nests the payload cgroup inside lab's OWN cgroup
// and makes podman enable memory/pids in that cgroup's subtree_control. On
// cgroup v2 a cgroup with controllers enabled in subtree_control may not
// hold processes directly (the "no internal processes" rule) — and systemd
// attaches the service's new main PID DIRECTLY into that cgroup on every
// start. With KillMode=process keeping containers alive across restarts,
// the first restart after any container ran died with "Failed to spawn
// executor: Device or resource busy" (the 2026-07-25 dev-new outage).
//
// The fix restructures the tree so the cgroup systemd attaches into never
// delegates controllers:
//
//	lab.service/          delegated root (Delegate=yes) — NO processes,
//	│                     subtree_control carries +memory +pids
//	├── main/             lab + tmux + every host process (systemd's
//	│                     DelegateSubgroup=main attach point) — NO
//	│                     controllers delegated below it, ever
//	└── payload/          every container, via --cgroup-parent — NO
//	    └── libpod-*/     processes directly in payload/ itself
//
// main/ holds processes but never delegates; payload/ delegates but never
// holds processes directly. Both sides of the internal-process rule stay
// satisfied across any restart, and podman never needs its move-everything-
// into-a-"runtime"-subgroup workaround (which is what armed the old trap
// one nesting level per service generation). setupCgroups establishes the
// layout at preflight, Verify re-checks it before every container spawn,
// and the nix module's lab-cgroup-hygiene oneshot clears a re-armed main/
// before each service start as the last line of defense.

import (
	"fmt"
	"os"
	pathpkg "path"
	"slices"
	"strings"
)

// cgroupFSRoot is where the unified hierarchy is mounted.
const cgroupFSRoot = "/sys/fs/cgroup"

// payloadSubgroup is the container subtree's name under the delegated unit
// root — the counterpart of the module's DelegateSubgroup=main.
const payloadSubgroup = "payload"

// Cgroups is the established container cgroup layout (ADR-0058), published
// on the preflight Result so every spawn site reads one source: Parent
// becomes the argv's --cgroup-parent, Verify is the pre-spawn restart-safety
// guard. Built by setupCgroups; a hand-built value with an empty AttachDir
// (tests, degenerate wiring) verifies vacuously — only Preflight-produced
// Results carry the full layout.
type Cgroups struct {
	// Parent is the payload cgroup as a cgroupfs path (e.g.
	// "/system.slice/lab.service/payload") — RunSpec.CgroupParent's value.
	Parent string
	// AttachDir is lab's own cgroup directory under /sys/fs/cgroup — the
	// cgroup systemd attaches the service's main PID into on every start.
	AttachDir string
	// RootDir is AttachDir's parent: the delegated unit root whose
	// subtree_control carries the payload controllers and whose cgroup.procs
	// must stay empty.
	RootDir string

	// readFile is Verify's probe, injected by setupCgroups (Deps.ReadFile);
	// nil falls back to os.ReadFile.
	readFile func(string) ([]byte, error)
}

// Verify is the restart-safety guard (ADR-0058), run at preflight and again
// before EVERY container spawn: it re-checks the two invariants whose
// violation would make the NEXT service restart fail with EBUSY — the attach
// cgroup must not delegate controllers (podman's runtime-subgroup workaround
// re-enables them if it ever fires again, e.g. after a podman upgrade), and
// the delegated root must hold no processes directly. Detecting the re-armed
// trap at spawn time turns "the next deploy bricks the service" into an
// immediate, actionable refusal. Nil receiver or empty AttachDir (a layout
// never established — hand-built Results) verifies vacuously.
func (c *Cgroups) Verify() error {
	if c == nil || c.AttachDir == "" {
		return nil
	}
	readFile := c.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	stc := c.AttachDir + "/cgroup.subtree_control"
	b, err := readFile(stc)
	if err != nil {
		return fmt.Errorf("reading %s: %w", stc, err)
	}
	if ctrl := strings.TrimSpace(string(b)); ctrl != "" {
		return fmt.Errorf("lab's own cgroup %s delegates controllers (%q) — the next service restart would fail (cgroup v2 forbids processes in a delegating cgroup); restarting lab clears it (lab-cgroup-hygiene)", c.AttachDir, ctrl)
	}
	procs := c.RootDir + "/cgroup.procs"
	b, err = readFile(procs)
	if err != nil {
		return fmt.Errorf("reading %s: %w", procs, err)
	}
	if strays := strings.TrimSpace(string(b)); strays != "" {
		return fmt.Errorf("the delegated cgroup root %s holds processes (pids %s) — the next service restart would fail; they must live in a subgroup", c.RootDir, strings.Join(strings.Fields(strays), ","))
	}
	return nil
}

// setupCgroups establishes the ADR-0058 layout from inside lab's own cgroup
// (ownPath, the unified-hierarchy path from /proc/self/cgroup) and returns
// it, or the preflight Failure that explains what the host is missing:
//
//  1. Layout: lab must run in a delegated SUBGROUP — ownPath's parent is the
//     delegated unit root, detected by write access (the same durable signal
//     the old delegation check used). Running at the root itself (no
//     DelegateSubgroup) is refused: the root could then never delegate the
//     payload controllers without re-arming the restart trap.
//  2. Adopt strays: any process still sitting at the root (a tmux server
//     surviving from a pre-ADR-0058 deploy) is moved into lab's own cgroup —
//     best-effort per pid (races with exits are benign; Verify re-checks the
//     result and fails loud if adoption didn't stick).
//  3. Enable +memory +pids on the root's subtree_control so the payload
//     subtree can carry the --memory/--pids-limit caps.
//  4. Create the payload cgroup and confirm the controllers actually arrived
//     in it — the no-false-green posture of the old check, retargeted at the
//     subtree containers now really land in.
func setupCgroups(ownPath string, d Deps) (*Cgroups, *Failure) {
	ownPath = strings.TrimSuffix(ownPath, "/")
	rootPath := pathpkg.Dir(ownPath)
	c := &Cgroups{
		Parent:    rootPath + "/" + payloadSubgroup,
		AttachDir: cgroupFSRoot + ownPath,
		RootDir:   cgroupFSRoot + rootPath,
		readFile:  d.ReadFile,
	}
	layoutHint := "run lab with systemd Delegate=yes AND DelegateSubgroup=main — the delegated cgroup root must stay free of processes or service restarts fail (ADR-0058)"
	if rootPath == "/" || rootPath == "." {
		return nil, &Failure{Check: CheckCgroupLayout, Detail: fmt.Sprintf("lab runs in cgroup %s, directly under the hierarchy root", ownPath), Hint: layoutHint}
	}
	if d.Writable != nil && !d.Writable(c.RootDir) {
		return nil, &Failure{Check: CheckCgroupLayout, Detail: fmt.Sprintf("cgroup %s is not delegated to the lab user (parent %s not writable)", ownPath, rootPath), Hint: layoutHint}
	}

	// Adopt strays (step 2). The root's cgroup.procs is unreadable only on
	// layouts step 1 already refused, so a read error here just leaves
	// adoption to Verify's loud failure.
	if procs, err := d.ReadFile(c.RootDir + "/cgroup.procs"); err == nil {
		for line := range strings.Lines(string(procs)) {
			if pid := strings.TrimSpace(line); pid != "" {
				_ = d.WriteFile(c.AttachDir+"/cgroup.procs", []byte(pid), 0)
			}
		}
	}

	delegHint := "the lab service's cgroup lacks memory/pids delegation — set Delegate=yes (and DelegateSubgroup=main) on the systemd unit"
	if err := d.WriteFile(c.RootDir+"/cgroup.subtree_control", []byte("+memory +pids"), 0); err != nil {
		return nil, &Failure{Check: CheckDelegation, Detail: fmt.Sprintf("cannot enable memory/pids on %s: %v", rootPath, err), Hint: delegHint}
	}
	payloadDir := c.RootDir + "/" + payloadSubgroup
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
