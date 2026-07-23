package podmanx

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Check identifiers, one per way Preflight can fail. Stable strings, not an
// enum: they end up in the spawn-refusal message and in logs, and the tests
// key on them.
const (
	CheckPodman     = "podman"            // podman missing, unrunnable, or < 4
	CheckPasta      = "pasta"             // pasta missing (the backend --network=pasta pins)
	CheckSubuid     = "subuid"            // no usable /etc/subuid entry for the service user
	CheckSubgid     = "subgid"            // no usable /etc/subgid entry for the service user
	CheckCgroup2    = "cgroup2"           // unified hierarchy not mounted / not in use
	CheckDelegation = "cgroup-delegation" // memory/pids not delegated to our cgroup
	CheckToolsImage = "tools-image"       // no agent-tools images configured
	CheckToolsPull  = "tools-image-pull"  // a configured agent-tools ref does not resolve
)

// PreflightConfig is the container-mode configuration Preflight validates —
// the config.Config fields of #205, decoupled from that struct so this
// package needs no config import and tests build it literally.
type PreflightConfig struct {
	PodmanBin   string
	ToolsImages map[string]string // provider id -> agent-tools ref, digest-pinned (ADR-0051)
}

// Deps are Preflight's probes into the host, each injectable so the whole
// check matrix runs against fakes — a test must be able to simulate a host
// with no podman, a v1 cgroup, or a missing subuid entry without owning
// such a machine.
type Deps struct {
	LookPath func(string) (string, error)
	ReadFile func(string) ([]byte, error)
	Run      CmdRunner
	// Writable reports whether the service user can write path — the probe
	// behind the cgroup-delegation check. systemd's Delegate=yes chowns the
	// delegated cgroup directory to the unit's user so it can create the
	// child cgroups a container needs; a non-delegated cgroup stays
	// root-owned. Read access (ReadFile of cgroup.controllers) cannot tell
	// the two apart — both are world-readable — so delegation is detected by
	// write access to the cgroup directory itself.
	Writable func(string) bool
	// Username and UID identify the service user for subuid/subgid
	// matching: shadow(5) keys ranges by either the login name or the
	// numeric uid, so both are accepted.
	Username string
	UID      int
}

// RealDeps returns the production probes: exec.LookPath, os.ReadFile, an
// ExecRunner, and the current process's identity. A failed user.Current()
// leaves Username "" — the subuid check then matches on the numeric UID
// string alone, which /etc/subuid equally accepts — rather than making
// preflight construction fallible over a lookup that is only one of two
// accepted match keys.
func RealDeps() Deps {
	d := Deps{
		LookPath: exec.LookPath,
		ReadFile: os.ReadFile,
		Run:      ExecRunner(),
		Writable: func(path string) bool { return unix.Access(path, unix.W_OK) == nil },
		UID:      os.Getuid(),
	}
	if u, err := user.Current(); err == nil {
		d.Username = u.Username
	}
	return d
}

// Failure is one failed preflight check: a stable identifier, what was
// observed, and the operator action that fixes it. Hints are load-bearing —
// the whole point of preflight is that a refused spawn tells the operator
// exactly what to run, not just that the host "isn't ready".
type Failure struct {
	Check  string
	Detail string
	Hint   string
}

// Result is a full preflight pass: the podman client version when it could
// be read, and every failure found. Zero failures means container spawns
// may proceed.
type Result struct {
	Version  string
	Failures []Failure
}

// OK reports whether every check passed.
func (r Result) OK() bool { return len(r.Failures) == 0 }

// Error renders every failure into one actionable message for the spawn
// refusal, "" when OK. All failures in one string, deliberately: surfacing
// only the first would make the operator fix the host one restart at a
// time.
func (r Result) Error() string {
	if r.OK() {
		return ""
	}
	parts := make([]string, len(r.Failures))
	for i, f := range r.Failures {
		parts[i] = fmt.Sprintf("%s: %s (%s)", f.Check, f.Detail, f.Hint)
	}
	return "container preflight failed: " + strings.Join(parts, "; ")
}

// Preflight verifies the host can actually run containerized sessions
// (#205): podman >= 4 present, pasta present, subuid/subgid ranges for the
// service user, cgroup v2 with memory+pids delegated (the --memory and
// --pids-limit caps silently do nothing without it), at least one agent-tools
// image configured, and every configured agent-tools ref resolvable. The dev
// image is deliberately NOT checked here (issue #207): it is per-repo-or-
// global and only the spawn knows the repo, so its presence is ensured at
// spawn (EnsureImage) — a host with no global default is a valid deployment
// where every repo pins its own image. Preflight NEVER aborts early — all
// failures are collected so one refusal names everything wrong at once — and
// it is safe on a host with no podman at all: that is just Failures, never a
// panic. A later task calls this at server startup and gates container spawns
// on Result.OK.
func Preflight(ctx context.Context, cfg PreflightConfig, d Deps) Result {
	var r Result
	fail := func(check, detail, hint string) {
		r.Failures = append(r.Failures, Failure{Check: check, Detail: detail, Hint: hint})
	}

	// 1. podman present and modern enough. --userns=keep-id combined with
	// --network=pasta and image mounts is podman >= 4 territory; a 3.x
	// client would fail at spawn time with a far less legible error.
	podmanOK := false
	if _, err := d.LookPath(cfg.PodmanBin); err != nil {
		fail(CheckPodman, fmt.Sprintf("%s not found on PATH", cfg.PodmanBin), "install podman >= 4")
	} else if out, err := d.Run(ctx, cfg.PodmanBin, "version", "--format", "{{.Client.Version}}"); err != nil {
		fail(CheckPodman, fmt.Sprintf("%s version: %v", cfg.PodmanBin, err), "install podman >= 4")
	} else {
		v := strings.TrimSpace(string(out))
		r.Version = v
		if major, ok := versionMajor(v); !ok {
			fail(CheckPodman, fmt.Sprintf("cannot parse podman version %q", v), "install podman >= 4")
		} else if major < 4 {
			fail(CheckPodman, fmt.Sprintf("podman %s is too old", v), fmt.Sprintf("found podman %s; install podman >= 4", v))
		} else {
			podmanOK = true
		}
	}

	// 2. pasta present: the argv pins --network=pasta (not slirp4netns), so
	// its absence means every container spawn dies at start.
	if _, err := d.LookPath("pasta"); err != nil {
		fail(CheckPasta, "pasta not found on PATH", "install passt (provides pasta)")
	}

	// 3. subuid/subgid ranges: rootless podman cannot set up the user
	// namespace (which --userns=keep-id requires) without a subordinate id
	// range for the service user. Both files checked independently so the
	// failure names exactly the one that is missing.
	subject := d.Username
	if subject == "" {
		subject = strconv.Itoa(d.UID)
	}
	subHint := fmt.Sprintf("add %q to /etc/subuid and /etc/subgid (usermod --add-subuids/--add-subgids)", subject+":100000:65536")
	for _, c := range []struct{ check, file string }{
		{CheckSubuid, "/etc/subuid"},
		{CheckSubgid, "/etc/subgid"},
	} {
		data, err := d.ReadFile(c.file)
		if err != nil {
			fail(c.check, fmt.Sprintf("cannot read %s: %v", c.file, err), subHint)
			continue
		}
		if !hasSubIDEntry(data, d.Username, d.UID) {
			fail(c.check, fmt.Sprintf("%s has no entry for %s", c.file, subject), subHint)
		}
	}

	// 4. cgroup v2 with memory+pids delegated TO lab's own cgroup. The pane
	// argv pins --cgroups=split, so the container's payload cgroup is a child
	// of lab's own cgroup (<lab's cgroup>/libpod-payload-<id>), not podman's
	// rootless default of user.slice — which means THIS cgroup, the one
	// /proc/self/cgroup names, is exactly where --memory/--pids-limit take
	// effect. Two conditions must both hold or the caps are silently absent
	// (worse than failing, since #205's whole point is a bounded blast
	// radius):
	//
	//   (a) memory and pids are available in this cgroup — i.e. the parent
	//       delegated them inward (cgroup.controllers lists them); and
	//   (b) this cgroup is actually delegated to the lab user, so lab can
	//       create the child cgroups --cgroups=split needs and enable the
	//       controllers on them.
	//
	// (a) alone is a false green: cgroup.controllers reflects what the PARENT
	// enabled in its subtree_control, so a non-delegated lab.service still
	// shows "memory pids" there while lab cannot create a single limited
	// child (Delegate=no leaves the cgroup dir root-owned). And subtree_control
	// is not a substitute — a correctly delegated but idle cgroup has it empty
	// until the first child is created. So (b) is checked by write access to
	// the cgroup directory, the durable signal systemd's Delegate=yes leaves.
	const cgroupRoot = "/sys/fs/cgroup"
	delegHint := "the lab service's cgroup lacks memory/pids delegation — set Delegate=yes on the systemd unit"
	if _, err := d.ReadFile(cgroupRoot + "/cgroup.controllers"); err != nil {
		fail(CheckCgroup2, "cgroup v2 (unified hierarchy) not mounted", "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)")
	} else if self, err := d.ReadFile("/proc/self/cgroup"); err != nil {
		fail(CheckCgroup2, fmt.Sprintf("cannot read /proc/self/cgroup: %v", err), "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)")
	} else if path, ok := unifiedCgroupPath(self); !ok {
		fail(CheckCgroup2, "no unified (0::) entry in /proc/self/cgroup", "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)")
	} else {
		cgroupDir := cgroupRoot + strings.TrimSuffix(path, "/")
		ctrlFile := cgroupDir + "/cgroup.controllers"
		if ctrl, err := d.ReadFile(ctrlFile); err != nil {
			fail(CheckDelegation, fmt.Sprintf("cannot read %s: %v", ctrlFile, err), delegHint)
		} else if have := strings.Fields(string(ctrl)); !slices.Contains(have, "memory") || !slices.Contains(have, "pids") {
			fail(CheckDelegation, fmt.Sprintf("cgroup %s delegates only %q — memory and pids are required", path, strings.TrimSpace(string(ctrl))), delegHint)
		} else if d.Writable != nil && !d.Writable(cgroupDir) {
			fail(CheckDelegation, fmt.Sprintf("cgroup %s is not delegated to the lab user (not writable) — its --memory/--pids-limit caps would be silently absent", path), delegHint)
		}
	}

	// 5. Agent-tools images configured. Container mode has no default tools
	// image on purpose: the tools injection is lab's own (ADR-0051), but the
	// operator must still point at the built refs, so none configured is
	// "refuse to spawn", not "pick one for them". The DEV image used to be
	// checked here too; issue #207 moved it to the spawn (EnsureImage), where
	// the repo's own image_ref override is known — an unset global default is
	// no longer a preflight failure.
	if len(cfg.ToolsImages) == 0 {
		fail(CheckToolsImage, "no agent-tools images configured", "set --container-tools-image provider=ref")
	}

	// 6. Every configured agent-tools ref resolvable — checked at startup
	// so a bad digest surfaces here and not as a failed spawn hours later.
	// `image exists` is the cheap local probe; a miss attempts one pull
	// (the ref is digest-pinned, so a successful pull is the permanent
	// fix). Skipped entirely when podman itself failed check 1: every probe
	// would just re-report the same root cause. Providers iterate in sorted
	// order so failure order — and the tests — are deterministic.
	if podmanOK {
		for _, provider := range slices.Sorted(maps.Keys(cfg.ToolsImages)) {
			ref := cfg.ToolsImages[provider]
			if _, err := d.Run(ctx, cfg.PodmanBin, "image", "exists", ref); err == nil {
				continue
			}
			if _, err := d.Run(ctx, cfg.PodmanBin, "pull", ref); err != nil {
				fail(CheckToolsPull, fmt.Sprintf("provider %s: tools image %s not resolvable: %v", provider, ref, err),
					"check the ref (@sha256-pinned per ADR-0051) and registry access from this host")
			}
		}
	}

	return r
}

// versionMajor parses the major component of a "4.9.3"-style version.
// Anything before the first '.' must be a non-negative integer; podman's
// --format {{.Client.Version}} emits nothing fancier.
func versionMajor(v string) (int, bool) {
	head, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(head)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// hasSubIDEntry reports whether a subuid/subgid file (name:start:count
// lines, shadow(5)) grants a positive-count range to username or to the
// numeric uid — shadow tools write either key. An empty username matches
// nothing (never the empty first field of a malformed line); malformed
// lines are skipped, not errors, matching how the shadow tools themselves
// read these files.
func hasSubIDEntry(data []byte, username string, uid int) bool {
	uidStr := strconv.Itoa(uid)
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			continue
		}
		if parts[0] != uidStr && (username == "" || parts[0] != username) {
			continue
		}
		if count, err := strconv.Atoi(parts[2]); err == nil && count > 0 {
			return true
		}
	}
	return false
}

// unifiedCgroupPath extracts the cgroup-v2 path from /proc/self/cgroup —
// the "0::<path>" line the unified hierarchy always writes. Its absence
// (v1-only lines like "4:memory:/…") means the process is not on the
// unified hierarchy even if one is mounted somewhere.
func unifiedCgroupPath(data []byte) (string, bool) {
	for line := range strings.Lines(string(data)) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return p, true
		}
	}
	return "", false
}
