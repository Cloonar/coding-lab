package podmanx

// The nested-containers preflight (ADR-0086, issue #115): a static check
// that the tun device a nested run gets is there and usable, and a nested
// spawn probe that runs the entry program's self-test in a container of
// the nested shape.
//
// Gated, and the gate is the caller's: cmd/lab runs NestedPreflight only
// when nested containers are on for some repo or as the global default, so
// a host on which nobody uses them pays nothing and is never refused for
// them. Preflight itself is untouched — its checks are the floor every
// container run needs; these two are what the nested shape adds on top. A
// spawn whose effective knob is on is refused when either reports a
// failure; a spawn whose knob is off never looks at them.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// NestedPreflightConfig is what NestedPreflight needs beyond its Deps.
type NestedPreflightConfig struct {
	PodmanBin string
	// ToolsImage is an agent-tools ref — any one: every agent-tools image
	// carries the same static labctl at /bin/labctl, so the caller passes
	// the one the main preflight found usable. "" skips the probe (see
	// NestedPreflight).
	ToolsImage string
	// SeccompProfile is the path WriteNestedSeccomp returned — the very
	// file a nested run's argv names. "" is a failure, not a skip: a host
	// whose profile could not be written cannot start a nested run, and no
	// other check would say so.
	SeccompProfile string
}

// nestedProbeName is the nested probe's container name: deterministic, so
// the pre-clean reclaims a probe a crash leaked, and NOT labrun-prefixed,
// so the startup orphan sweep can never race it — the reasons of probeName
// (probe.go), and distinct from it so the two probes never address each
// other's container.
const nestedProbeName = "lab-preflight-nested-probe"

// The nested probe's caps. The memory cap matters beyond thrift: with it
// the probe's scope cgroup carries a memory.max, which the self-test's
// cgroup-limits check then asserts the session cannot raise (a write of
// the current value back is refused under nsdelegate) — the probe mirrors
// a run, whose caps are always set. 64 tasks fits the self-test with ample
// room: two Go processes at most at any time (labctl and the one namespace
// probe it re-executes), a dozen threads each, and every thread counts
// against pids.max.
const (
	nestedProbeMemory = "64m"
	nestedProbePids   = "64"
)

// nestedTunHint is CheckNestedTun's operator action: both of its causes —
// the module not loaded (no device node) and a node the service user may
// not open — are fixed on the host, never in lab.
const nestedTunHint = "load the tun module (`modprobe tun`; on NixOS it is available by default, and boot.kernelModules = [ \"tun\" ] loads it at boot) and keep /dev/net/tun at mode 0666 (crw-rw-rw- root root 10,200), as distros ship it — a nested run passes it in with --device so pasta can give nested containers their network"

// nestedProbeHint is CheckNestedProbe's operator action. The probe is what
// proves the cgroup handoff — a writable, namespaced cgroup for the session
// whose caps it still cannot raise, the "no silently absent caps" honesty
// ADR-0052 demanded. The caps hold by the kernel's nsdelegate rule, not by
// file ownership (the session uid owns the limit files under keep-id): the
// host's cgroup2 mount must carry nsdelegate and the run must get its own
// cgroup namespace. So the hint points at the pieces that make it: systemd
// mode, crun, the user manager's delegation, nsdelegate, the private cgroup
// namespace, and an entry program new enough to run.
const nestedProbeHint = "the nested shape needs podman's systemd mode (--systemd=always) to hand the run a writable, namespaced cgroup whose memory.max and pids.max the session still cannot raise: check that the host's cgroup2 mount carries nsdelegate (systemd's default; see /proc/self/mountinfo), that the run gets its own cgroup namespace (podman's cgroupns=private, its default on cgroup v2), that the OCI runtime is crun (engine.runtime in containers.conf), that the lab user's user@.service delegates its cgroup (Delegate=yes, systemd's default for user@.service), and that the agent-tools image carries a labctl with `container-entry` (issue #114); run the self-test by hand as docs/ops.md § Nested containers (\"Checking a host by hand\") shows"

// NestedPreflight runs the two nested-containers checks and returns their
// failures — nil when nested runs may start. Like Preflight it never
// aborts early and keeps "one root cause, one check id":
//
//  1. CheckNestedTun: /dev/net/tun exists and the service user can open it
//     read-write (Deps.AccessRW). The detail tells "missing" (the tun
//     module is not loaded) from "not readable and writable" (its mode).
//  2. CheckNestedProbe (nestedProbe): one real container in the nested
//     shape running `labctl container-entry --self-test`. Skipped — no
//     failure added — when podman is not on PATH or no tools image was
//     given: Preflight's own podman and tools-image checks already name
//     those root causes, and the caller gates every container spawn on
//     Preflight too. Skipped as well when the tun check failed: the probe
//     passes the device with --device, so podman would refuse it and only
//     re-report the tun failure under a second id. An empty
//     SeccompProfile is reported under this id instead of probing: no
//     other check would name it, and every nested spawn would fail on it.
func NestedPreflight(ctx context.Context, cfg NestedPreflightConfig, d Deps) []Failure {
	var failures []Failure

	tunOK := true
	if err := d.AccessRW(devNetTun); err != nil {
		tunOK = false
		var detail string
		switch {
		case errors.Is(err, fs.ErrNotExist):
			detail = fmt.Sprintf("%s does not exist (the tun module is not loaded): %v", devNetTun, err)
		case errors.Is(err, fs.ErrPermission):
			detail = fmt.Sprintf("%s is not readable and writable by the service user: %v", devNetTun, err)
		default:
			detail = fmt.Sprintf("%s cannot be checked: %v", devNetTun, err)
		}
		failures = append(failures, Failure{Check: CheckNestedTun, Detail: detail, Hint: nestedTunHint})
	}

	switch {
	case cfg.SeccompProfile == "":
		failures = append(failures, Failure{
			Check:  CheckNestedProbe,
			Detail: "no nested seccomp profile was written, so a nested run has no profile to pass with --security-opt seccomp=",
			Hint:   "check the lab log for the error writing <state>/containers/nested-seccomp.json and that the state dir is writable by the service user",
		})
	case cfg.ToolsImage == "" || !tunOK:
		// Reported elsewhere: see the doc comment.
	default:
		if _, err := d.LookPath(cfg.PodmanBin); err == nil {
			if f := nestedProbe(ctx, cfg, d); f != nil {
				failures = append(failures, *f)
			}
		}
	}
	return failures
}

// nestedProbeArgv is the probe's `podman run`: the run shapes' global
// manager flag; --rm and the deterministic name; nestedFlags — the very
// list RunArgv renders in a nested run, at the same position (where the
// default shape has --init), so whatever the probe proves holds for the
// run; keep-id as in every run; the probe's caps; then the tools image as
// the rootfs and the self-test.
//
// The tools image is the rootfs, as in the spawn probe (probe.go): it is
// FROM scratch with the static labctl at /bin/labctl, which needs nothing
// else from a rootfs, and the self-test is about the host and the outer
// argv, not about any dev image — so the probe needs no dev image, which
// is per repo and not known at startup. Its only cost is the expected
// `fail: newuidmap:` line — the self-test's one non-fatal check, since
// newuidmap is the dev image's business — which exit 0 tells apart. No
// -it: the probe has no pane, and podman returns the self-test's stdout
// and exit status.
//
// Left out on purpose: the network pins (pasta and the add-host names) —
// the self-test checks nothing network-related, and a broken pasta is
// Preflight's CheckPasta, so including them would report one root cause
// under two ids; --ulimit, the tools image mount and the bind inventory,
// which the self-test does not touch. The probe pins the flags that make a
// run nested, which are the ones this check exists for.
func nestedProbeArgv(cfg NestedPreflightConfig, d Deps) []string {
	args := []string{cfg.PodmanBin, "--cgroup-manager=systemd", "run", "--rm", "--name", nestedProbeName}
	args = append(args, nestedFlags(cfg.SeccompProfile)...)
	return append(args,
		"--userns=keep-id",
		"--memory", nestedProbeMemory,
		"--pids-limit", nestedProbePids,
		cfg.ToolsImage,
		"/bin/labctl", "container-entry", "--self-test",
		"--uid", strconv.Itoa(d.UID),
		"--gid", strconv.Itoa(d.GID),
	)
}

// nestedProbe runs the self-test in a nested-shaped container: pre-clean
// the deterministic name (error ignored — a real problem resurfaces at the
// run), the one `podman run`, and a deferred `rm --force --ignore --time
// 0` in case --rm could not reap (a killed client, a cancelled ctx).
//
// The verdict is the exit status, the self-test's contract (internal/
// labctl/entry_selftest.go): 0 means every fatal check passed, so a
// `fail: newuidmap:` line beside it is expected and ignored. A non-zero
// exit — the self-test's 1, podman's own 125–127, a usage error from a
// labctl too old to know container-entry — is ONE Failure whose detail
// lists every `fail:` line, joined with "; ", or, when the self-test
// printed none (podman refused the container before it ran), podman's
// error with its stderr. ExecRunner returns the stdout a failed command
// did print alongside its ExitError, which is what makes the lines
// available on that path.
//
// Exit 0 with no `ok:` line at all is a failure too: it is not the
// self-test's output, so nothing was proved (the no-false-green rule of
// the spawn probe).
func nestedProbe(ctx context.Context, cfg NestedPreflightConfig, d Deps) *Failure {
	fail := func(detail string) *Failure {
		return &Failure{Check: CheckNestedProbe, Detail: detail, Hint: nestedProbeHint}
	}
	bin := cfg.PodmanBin
	_, _ = d.Run(ctx, bin, "rm", "--force", "--ignore", "--time", "0", nestedProbeName)
	defer func() {
		_, _ = d.Run(ctx, bin, "rm", "--force", "--ignore", "--time", "0", nestedProbeName)
	}()

	argv := nestedProbeArgv(cfg, d)
	out, err := d.Run(ctx, argv[0], argv[1:]...)
	var fails []string
	sawOK := false
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ok: ") {
			sawOK = true
		}
		if strings.HasPrefix(line, "fail: ") {
			fails = append(fails, line)
		}
	}
	if err == nil {
		if !sawOK {
			return fail(fmt.Sprintf("the nested self-test exited 0 but printed no result lines (stdout %q)", strings.TrimSpace(string(out))))
		}
		return nil
	}
	if len(fails) == 0 {
		return fail(fmt.Sprintf("the nested probe container failed before the self-test reported: %v", err))
	}
	// Every line, in the self-test's order — a newuidmap line included: on
	// this path something fatal failed beside it, and it is context.
	return fail(fmt.Sprintf("the nested self-test failed: %s", strings.Join(fails, "; ")))
}
