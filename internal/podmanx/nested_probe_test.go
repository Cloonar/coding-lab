package podmanx

import (
	"context"
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
)

const (
	nestedSeccomp = "/var/lib/lab/containers/nested-seccomp.json"
	nestedTools   = "reg/agent-tools@sha256:aa"
	nestedRm      = "podman rm --force --ignore --time 0 lab-preflight-nested-probe"
)

// nestedProbeWant is the probe's exact argv: the manager flag, --rm and
// the name, nestedFlags at --init's slot (as in a nested run), keep-id,
// the caps, the tools image as rootfs, and the self-test for the service
// uid:gid.
var nestedProbeWant = []string{
	"podman", "--cgroup-manager=systemd", "run", "--rm", "--name", "lab-preflight-nested-probe",
	"--systemd=always",
	"--stop-signal", "SIGTERM",
	"--user", "0",
	"--security-opt", "unmask=ALL",
	"--security-opt", "seccomp=" + nestedSeccomp,
	"--device", "/dev/net/tun",
	"-v", "/sys:/sys",
	"--userns=keep-id",
	"--memory", "64m",
	"--pids-limit", "64",
	nestedTools,
	"/bin/labctl", "container-entry", "--self-test", "--uid", "990", "--gid", "991",
}

// selfTestGreen is the self-test's stdout on a healthy host, as the probe
// sees it from the tools rootfs: every fatal check ok, newuidmap failing
// (the tools image has none) — exit 0.
const selfTestGreen = `ok: cgroup-child
ok: cgroup-limits
ok: procfs-mount
ok: uts-hostname
ok: dev-net-tun
ok: sys-rw
fail: newuidmap: exec: "newuidmap": executable file not found in $PATH
`

func nestedFixture() (*pfFixture, NestedPreflightConfig) {
	f := greenFixture()
	f.runner.script = map[string]cmdResult{
		nestedRm:                           {},
		strings.Join(nestedProbeWant, " "): {out: selfTestGreen},
	}
	return f, NestedPreflightConfig{PodmanBin: "podman", ToolsImage: nestedTools, SeccompProfile: nestedSeccomp}
}

func TestNestedPreflight(t *testing.T) {
	probeKey := strings.Join(nestedProbeWant, " ")
	tests := []struct {
		name       string
		mutate     func(*pfFixture, *NestedPreflightConfig)
		wantChecks []string
		after      func(*testing.T, *pfFixture, []Failure)
	}{
		{
			// The green path, pinned end to end: pre-clean, the one run,
			// the deferred cleanup. The probe argv carries nestedFlags
			// verbatim — the same list a nested run renders. The newuidmap
			// fail line beside exit 0 is expected and not reported.
			name:   "all green, newuidmap line ignored on exit 0",
			mutate: func(*pfFixture, *NestedPreflightConfig) {},
			after: func(t *testing.T, f *pfFixture, _ []Failure) {
				want := [][]string{
					{"podman", "rm", "--force", "--ignore", "--time", "0", nestedProbeName},
					nestedProbeWant,
					{"podman", "rm", "--force", "--ignore", "--time", "0", nestedProbeName},
				}
				if len(f.runner.calls) != len(want) {
					t.Fatalf("calls = %q, want %q", f.runner.calls, want)
				}
				for i := range want {
					assertArgv(t, f.runner.calls[i], want[i])
				}
				flags := nestedFlags(nestedSeccomp)
				if i := slices.Index(nestedProbeWant, flags[0]); i < 0 || !slices.Equal(nestedProbeWant[i:i+len(flags)], flags) {
					t.Errorf("probe argv does not carry nestedFlags as one block")
				}
			},
		},
		{
			name: "all green without the newuidmap line",
			mutate: func(f *pfFixture, _ *NestedPreflightConfig) {
				f.runner.script[probeKey] = cmdResult{out: strings.Replace(selfTestGreen, `fail: newuidmap: exec: "newuidmap": executable file not found in $PATH`, "ok: newuidmap", 1)}
			},
		},
		{
			name: "tun missing: detail says missing, probe skipped",
			mutate: func(f *pfFixture, _ *NestedPreflightConfig) {
				f.accessRW["/dev/net/tun"] = syscall.ENOENT
			},
			wantChecks: []string{CheckNestedTun},
			after: func(t *testing.T, f *pfFixture, fs []Failure) {
				if d := fs[0].Detail; !strings.Contains(d, "/dev/net/tun") || !strings.Contains(d, "does not exist") || !strings.Contains(d, "no such file") {
					t.Errorf("detail %q does not say the device is missing", d)
				}
				if h := fs[0].Hint; !strings.Contains(h, "modprobe tun") || !strings.Contains(h, "0666") {
					t.Errorf("hint %q does not name loading the module and the device mode", h)
				}
				// The probe would only be refused at --device: one root
				// cause, one check id.
				if len(f.runner.calls) != 0 {
					t.Errorf("probe ran despite a missing tun device: %q", f.runner.calls)
				}
			},
		},
		{
			name: "tun not readable and writable",
			mutate: func(f *pfFixture, _ *NestedPreflightConfig) {
				f.accessRW["/dev/net/tun"] = syscall.EACCES
			},
			wantChecks: []string{CheckNestedTun},
			after: func(t *testing.T, f *pfFixture, fs []Failure) {
				if d := fs[0].Detail; !strings.Contains(d, "not readable and writable") || !strings.Contains(d, "permission denied") {
					t.Errorf("detail %q does not say the device is not usable", d)
				}
			},
		},
		{
			// The self-test ran and failed: every fail: line, in order, in
			// one failure — the newuidmap one included as context. The
			// stdout arrives beside the ExitError, as ExecRunner returns it.
			name: "self-test fails: detail carries every fail line",
			mutate: func(f *pfFixture, _ *NestedPreflightConfig) {
				f.runner.script[probeKey] = cmdResult{
					out: "ok: cgroup-child\nfail: cgroup-limits: /sys/fs/cgroup/memory.max is writable\nok: procfs-mount\nok: uts-hostname\nok: dev-net-tun\nfail: sys-rw: /sys is mounted read-only\nfail: newuidmap: not found\n",
					err: errors.New("exit status 1"),
				}
			},
			wantChecks: []string{CheckNestedProbe},
			after: func(t *testing.T, f *pfFixture, fs []Failure) {
				want := "fail: cgroup-limits: /sys/fs/cgroup/memory.max is writable; fail: sys-rw: /sys is mounted read-only; fail: newuidmap: not found"
				if d := fs[0].Detail; !strings.Contains(d, want) {
					t.Errorf("detail %q does not carry the fail lines %q", d, want)
				}
				if h := fs[0].Hint; !strings.Contains(h, "--systemd=always") || !strings.Contains(h, "crun") || !strings.Contains(h, "docs/ops.md") || !strings.Contains(h, "Nested containers") {
					t.Errorf("hint %q is not actionable", h)
				}
				last := f.runner.calls[len(f.runner.calls)-1]
				assertArgv(t, last, []string{"podman", "rm", "--force", "--ignore", "--time", "0", nestedProbeName})
			},
		},
		{
			// A failed setup is the self-test's single line.
			name: "self-test setup fails",
			mutate: func(f *pfFixture, _ *NestedPreflightConfig) {
				f.runner.script[probeKey] = cmdResult{
					out: "fail: setup: chown cgroup: /sys/fs/cgroup/cgroup.procs: read-only file system\n",
					err: errors.New("exit status 1"),
				}
			},
			wantChecks: []string{CheckNestedProbe},
			after: func(t *testing.T, f *pfFixture, fs []Failure) {
				if d := fs[0].Detail; !strings.Contains(d, "fail: setup: chown cgroup: /sys/fs/cgroup/cgroup.procs: read-only file system") {
					t.Errorf("detail %q does not carry the setup line", d)
				}
			},
		},
		{
			// podman refused the container before the self-test ran: no
			// stdout, so the detail is podman's own error (stderr folded in
			// by ExecRunner).
			name: "podman error with no stdout",
			mutate: func(f *pfFixture, _ *NestedPreflightConfig) {
				f.runner.script[probeKey] = cmdResult{err: errors.New("exit status 126: Error: crun: open `/proc/sys`: Operation not permitted")}
			},
			wantChecks: []string{CheckNestedProbe},
			after: func(t *testing.T, f *pfFixture, fs []Failure) {
				if d := fs[0].Detail; !strings.Contains(d, "crun: open `/proc/sys`: Operation not permitted") || !strings.Contains(d, "before the self-test") {
					t.Errorf("detail %q does not quote podman's error", d)
				}
			},
		},
		{
			// Exit 0 without the self-test's output proves nothing.
			name: "exit 0 with no result lines",
			mutate: func(f *pfFixture, _ *NestedPreflightConfig) {
				f.runner.script[probeKey] = cmdResult{out: ""}
			},
			wantChecks: []string{CheckNestedProbe},
		},
		{
			// Preflight's CheckPodman names this root cause; the nested
			// probe adds nothing on top.
			name: "podman missing skips the probe",
			mutate: func(f *pfFixture, _ *NestedPreflightConfig) {
				delete(f.paths, "podman")
			},
			after: func(t *testing.T, f *pfFixture, _ []Failure) {
				if len(f.runner.calls) != 0 {
					t.Errorf("probe ran without podman: %q", f.runner.calls)
				}
			},
		},
		{
			name: "no tools image skips the probe",
			mutate: func(_ *pfFixture, c *NestedPreflightConfig) {
				c.ToolsImage = ""
			},
			after: func(t *testing.T, f *pfFixture, _ []Failure) {
				if len(f.runner.calls) != 0 {
					t.Errorf("probe ran without a tools image: %q", f.runner.calls)
				}
			},
		},
		{
			// Nothing else would name a missing profile, and every nested
			// spawn would fail on it.
			name: "no seccomp profile is a probe failure",
			mutate: func(_ *pfFixture, c *NestedPreflightConfig) {
				c.SeccompProfile = ""
			},
			wantChecks: []string{CheckNestedProbe},
			after: func(t *testing.T, f *pfFixture, fs []Failure) {
				if !strings.Contains(fs[0].Detail, "seccomp") || !strings.Contains(fs[0].Hint, "nested-seccomp.json") {
					t.Errorf("failure %+v does not name the missing profile", fs[0])
				}
				if len(f.runner.calls) != 0 {
					t.Errorf("probe ran without a profile: %q", f.runner.calls)
				}
			},
		},
		{
			name: "missing tun and missing profile: both reported once",
			mutate: func(f *pfFixture, c *NestedPreflightConfig) {
				delete(f.accessRW, "/dev/net/tun")
				c.SeccompProfile = ""
			},
			wantChecks: []string{CheckNestedTun, CheckNestedProbe},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, cfg := nestedFixture()
			tt.mutate(f, &cfg)
			got := NestedPreflight(context.Background(), cfg, f.deps())
			var checks []string
			for _, fa := range got {
				checks = append(checks, fa.Check)
				if fa.Hint == "" {
					t.Errorf("failure %+v has no hint", fa)
				}
			}
			if !slices.Equal(checks, tt.wantChecks) {
				t.Fatalf("failure checks = %q, want %q (failures: %+v)", checks, tt.wantChecks, got)
			}
			if tt.after != nil {
				tt.after(t, f, got)
			}
		})
	}
}

// TestPreflightDoesNotRunNestedChecks: the gate is the caller's —
// Preflight itself never touches the tun device nor runs the nested probe,
// so a host nobody uses nested containers on is never refused for them.
func TestPreflightDoesNotRunNestedChecks(t *testing.T) {
	f := greenFixture()
	f.accessRW = map[string]error{} // no /dev/net/tun at all
	if r := Preflight(context.Background(), f.cfg, f.deps()); !r.OK() {
		t.Fatalf("Preflight failed on a host without /dev/net/tun: %+v", r.Failures)
	}
	for _, c := range f.runner.calls {
		if slices.Contains(c, nestedProbeName) {
			t.Errorf("Preflight ran the nested probe: %q", c)
		}
	}
}
