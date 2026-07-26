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
	CheckPodman      = "podman"           // podman missing, unrunnable, or < 4
	CheckPasta       = "pasta"            // pasta missing (the backend --network=pasta pins)
	CheckSubuid      = "subuid"           // no usable /etc/subuid entry for the service user
	CheckSubgid      = "subgid"           // no usable /etc/subgid entry for the service user
	CheckCgroup2     = "cgroup2"          // unified hierarchy not mounted / not in use
	CheckUserManager = "user-manager"     // the lab user's systemd user manager is unreachable (ADR-0060)
	CheckSpawnProbe  = "spawn-probe"      // a real probe container failed to spawn or its caps did not land
	CheckToolsImage  = "tools-image"      // no agent-tools images configured
	CheckToolsPull   = "tools-image-pull" // a configured agent-tools ref does not resolve
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
	// behind the user-manager check. systemd-logind creates /run/user/<uid>
	// owned by (and writable for) that user exactly while user@<uid>.service
	// runs, so write access to the runtime dir is the reachability signal for
	// the user manager podman's systemd cgroup manager must talk to. Mere
	// existence cannot tell — root, or a stale mount, could leave the dir in
	// place without a live manager behind it.
	Writable func(string) bool
	// Username and UID identify the service user for subuid/subgid
	// matching: shadow(5) keys ranges by either the login name or the
	// numeric uid, so both are accepted. UID also locates the user manager's
	// runtime dir (/run/user/<uid>) for the user-manager check.
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
// be read, every failure found, and any non-fatal warnings (conditions the
// operator should know about that do not block spawns — e.g. running on a
// cached tools image because the registry was unreachable). Zero failures
// means container spawns may proceed.
type Result struct {
	Version  string
	Failures []Failure
	Warnings []string
}

// OK reports whether every check passed.
func (r Result) OK() bool { return len(r.Failures) == 0 }

// HasRetryableFailure reports whether any failure belongs to a check whose
// verdict can change with no host change at all — cmd/lab keys its retry
// loop (issue #220) on this. Two checks qualify:
//
//   - CheckToolsPull: the agent-tools publish job may still be pushing the
//     tag this boot's pull raced, or the registry may be briefly down with
//     no cached image to fall back on.
//   - CheckUserManager: systemd-logind brings user@<uid>.service up for a
//     lingering user asynchronously, and lab.service cannot order After= it
//     (the uid is unknown at nix eval time), so a just-booted host may
//     briefly fail this check and then heal itself.
//
// Every other check reports host state that only a redeploy (and thus a
// fresh boot) changes.
func (r Result) HasRetryableFailure() bool {
	for _, f := range r.Failures {
		if f.Check == CheckToolsPull || f.Check == CheckUserManager {
			return true
		}
	}
	return false
}

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
// service user, cgroup v2 (unified hierarchy) in use, the lab user's systemd
// user manager reachable (ADR-0060 — every container is a transient scope
// under it, so no manager means no spawns), at least one agent-tools image
// configured, every configured agent-tools ref resolvable, and finally a
// REAL spawn probe (probe.go): create+init a probe container through the
// systemd cgroup manager and read the caps back out of its scope cgroup.
// The probe is the no-false-green keystone — the retired cgroup checks
// verified everything EXCEPT the one operation that could fail (the PID
// migration into the container cgroup), and validated green on a host that
// could not spawn a single container. The dev image is deliberately NOT
// checked here (issue #207): it is per-repo-or-global and only the spawn
// knows the repo, so its presence is ensured at spawn (EnsureImage) — a host
// with no global default is a valid deployment where every repo pins its own
// image. Preflight NEVER aborts early — all failures are collected so one
// refusal names everything wrong at once — and it is safe on a host with no
// podman at all: that is just Failures, never a panic. cmd/lab calls this at
// server startup and gates container spawns on Result.OK.
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

	// 4. cgroup v2 (unified hierarchy) in use: the per-scope caps are pure v2
	// machinery (memory.max/pids.max in the scope cgroup), and podman's
	// systemd cgroup manager assumes the unified hierarchy. The 0:: entry
	// confirms this process itself is on it; the path's VALUE feeds nothing
	// anymore — cgroup placement is entirely systemd's job now (ADR-0060),
	// so on success there is nothing to record.
	cgroup2OK := false
	if _, err := d.ReadFile(cgroupFSRoot + "/cgroup.controllers"); err != nil {
		fail(CheckCgroup2, "cgroup v2 (unified hierarchy) not mounted", "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)")
	} else if self, err := d.ReadFile("/proc/self/cgroup"); err != nil {
		fail(CheckCgroup2, fmt.Sprintf("cannot read /proc/self/cgroup: %v", err), "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)")
	} else if _, ok := unifiedCgroupPath(self); !ok {
		fail(CheckCgroup2, "no unified (0::) entry in /proc/self/cgroup", "boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1)")
	} else {
		cgroup2OK = true
	}

	// 5. The lab user's systemd user manager reachable (ADR-0060): every
	// container becomes a transient scope under user@<uid>.service, so an
	// absent manager means podman's systemd cgroup manager has nobody to ask.
	// systemd-logind creates /run/user/<uid> owned by the user exactly while
	// the manager runs — write access to it is the reachability signal (see
	// Deps.Writable). This check can race a fresh boot (logind brings the
	// lingering user's manager up asynchronously and lab cannot After= a
	// uid-templated unit), which is why it is one of the two retryable
	// failures (HasRetryableFailure).
	userManagerOK := false
	runtimeDir := fmt.Sprintf("/run/user/%d", d.UID)
	if !d.Writable(runtimeDir) {
		fail(CheckUserManager,
			fmt.Sprintf("%s is not writable by the service user — user@%d.service is not running (or lingering is not enabled) for %s", runtimeDir, d.UID, subject),
			fmt.Sprintf("enable lingering for the service user (users.users.<user>.linger = true — the NixOS module ships it with container.enable) and check systemctl status user@%d.service; lab retries this check, since a just-booted host may still be bringing the user manager up", d.UID))
	} else {
		userManagerOK = true
	}

	// 6. Agent-tools images configured. Container mode has no default tools
	// image on purpose: the tools injection is lab's own (ADR-0051), but the
	// operator must still point at the built refs, so none configured is
	// "refuse to spawn", not "pick one for them". The DEV image used to be
	// checked here too; issue #207 moved it to the spawn (EnsureImage), where
	// the repo's own image_ref override is known — an unset global default is
	// no longer a preflight failure.
	if len(cfg.ToolsImages) == 0 {
		fail(CheckToolsImage, "no agent-tools images configured", "set --container-tools-image provider=ref")
	}

	// 7. Every configured agent-tools ref resolvable — checked at startup
	// so a bad ref surfaces here and not as a failed spawn hours later.
	// PULL FIRST, deliberately (ADR-0054): the default refs are moving
	// CLI-version tags, and a same-tag re-push (a labctl refresh shipped
	// via workflow_dispatch) must reach the host on its next boot — an
	// exists-first probe would pin the host to whatever it pulled first,
	// forever. An up-to-date tag makes the pull a cheap manifest check.
	// When the pull fails but a cached image exists, the host degrades to
	// the cache with a warning instead of refusing spawns — stale tools
	// beat none while the registry is unreachable. A pull failure with no
	// cache is retryable (HasRetryableFailure): the tag may simply not be
	// published yet when a deploy races the agent-tools publish job.
	// Skipped entirely when podman itself failed check 1: every probe
	// would just re-report the same root cause. Providers iterate in
	// sorted order so failure order — and the tests — are deterministic;
	// usableRefs collects, in that same order, every ref a spawn could use
	// (pulled or cached), because the spawn probe below needs one.
	var usableRefs []string
	if podmanOK {
		for _, provider := range slices.Sorted(maps.Keys(cfg.ToolsImages)) {
			ref := cfg.ToolsImages[provider]
			pullErr := func() error { _, err := d.Run(ctx, cfg.PodmanBin, "pull", ref); return err }()
			if pullErr == nil {
				usableRefs = append(usableRefs, ref)
				continue
			}
			if _, err := d.Run(ctx, cfg.PodmanBin, "image", "exists", ref); err == nil {
				r.Warnings = append(r.Warnings,
					fmt.Sprintf("provider %s: tools image %s: pull failed (%v); using the locally cached image — a moved tag will not reach this host until the registry is reachable again", provider, ref, pullErr))
				usableRefs = append(usableRefs, ref)
				continue
			}
			fail(CheckToolsPull, fmt.Sprintf("provider %s: tools image %s not resolvable: %v", provider, ref, pullErr),
				"check the ref and registry access from this host; the default tags are published by the agent-tools workflow")
		}
	}

	// 8. The spawn probe (probe.go) — the one check that actually spawns.
	// Run only when its prerequisites passed: a broken podman, hierarchy,
	// user manager, or ref inventory is ALREADY reported above, and a probe
	// against a known-broken root cause would only re-report the same thing
	// under a second check id. Skipping adds no failure for exactly that
	// reason.
	if podmanOK && cgroup2OK && userManagerOK && len(usableRefs) > 0 {
		if f := spawnProbe(ctx, cfg.PodmanBin, usableRefs[0], d); f != nil {
			r.Failures = append(r.Failures, *f)
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
