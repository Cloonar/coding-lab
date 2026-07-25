package podmanx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
)

// pfFixture is one simulated host: PATH lookups, file contents, and a
// scripted runner. greenFixture returns a host where every check passes;
// each table case mutates exactly the piece it breaks.
type pfFixture struct {
	cfg      PreflightConfig
	paths    map[string]string
	files    map[string]string
	writable map[string]bool // paths the service user can write (cgroup delegation)
	runner   *recordingRunner

	// setupCgroups' write seams: every accepted write/mkdir is recorded for
	// assertions; writeErr scripts a per-path failure.
	cgWrites []cgWrite
	mkdirs   []string
	writeErr map[string]error
}

type cgWrite struct{ path, data string }

func greenFixture() *pfFixture {
	return &pfFixture{
		cfg: PreflightConfig{
			PodmanBin: "podman",
			ToolsImages: map[string]string{
				"claude-code": "reg/agent-tools@sha256:aa",
			},
		},
		paths: map[string]string{
			"podman": "/usr/bin/podman",
			"pasta":  "/usr/bin/pasta",
		},
		// The ADR-0058 layout: lab attached in the main/ subgroup
		// (DelegateSubgroup=main), the delegated root process-free, main/
		// delegating nothing, and the payload cgroup carrying memory+pids
		// once setupCgroups enables them on the root.
		files: map[string]string{
			"/etc/subuid":                       "lab:100000:65536\n",
			"/etc/subgid":                       "lab:100000:65536\n",
			"/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory hugetlb pids\n",
			"/proc/self/cgroup":                 "0::/system.slice/lab.service/main\n",
			"/sys/fs/cgroup/system.slice/lab.service/cgroup.procs":                "",
			"/sys/fs/cgroup/system.slice/lab.service/main/cgroup.subtree_control": "\n",
			"/sys/fs/cgroup/system.slice/lab.service/payload/cgroup.controllers":  "memory pids\n",
		},
		// Delegate=yes chowns the delegated unit root to the service user; a
		// non-delegated host leaves it root-owned (unwritable).
		writable: map[string]bool{
			"/sys/fs/cgroup/system.slice/lab.service": true,
		},
		runner: &recordingRunner{script: map[string]cmdResult{
			"podman version --format {{.Client.Version}}": {out: "5.2.3\n"},
			// Pull-first (ADR-0054): even a locally-present tools image is
			// re-pulled so a moved tag reaches the host.
			"podman pull reg/agent-tools@sha256:aa": {},
		}},
	}
}

func (f *pfFixture) deps() Deps {
	return Deps{
		LookPath: func(name string) (string, error) {
			if p, ok := f.paths[name]; ok {
				return p, nil
			}
			return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
		},
		ReadFile: func(path string) ([]byte, error) {
			if s, ok := f.files[path]; ok {
				return []byte(s), nil
			}
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		},
		Writable: func(path string) bool { return f.writable[path] },
		WriteFile: func(path string, data []byte, _ os.FileMode) error {
			if err := f.writeErr[path]; err != nil {
				return err
			}
			f.cgWrites = append(f.cgWrites, cgWrite{path, string(data)})
			// Emulate the kernel's pid-move semantics: writing a pid into a
			// cgroup.procs file removes it from whichever cgroup listed it,
			// so a post-adoption re-read of the root sees it gone.
			if strings.HasSuffix(path, "/cgroup.procs") {
				pid := strings.TrimSpace(string(data))
				for name, content := range f.files {
					if name == path || !strings.HasSuffix(name, "/cgroup.procs") {
						continue
					}
					var kept []string
					for line := range strings.Lines(content) {
						if l := strings.TrimSpace(line); l != "" && l != pid {
							kept = append(kept, l)
						}
					}
					f.files[name] = strings.Join(kept, "\n")
				}
			}
			return nil
		},
		Mkdir: func(path string, _ os.FileMode) error {
			f.mkdirs = append(f.mkdirs, path)
			return nil
		},
		Run:      f.runner.run,
		Username: "lab",
		UID:      990,
	}
}

func TestPreflight(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*pfFixture)
		wantChecks   []string // Failure.Check values in order; nil means OK
		wantVersion  string
		wantWarnings int
		after        func(*testing.T, *pfFixture, Result)
	}{
		{
			name:        "all green",
			mutate:      func(f *pfFixture) {},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if r.Error() != "" {
					t.Errorf("Error() = %q, want empty on OK", r.Error())
				}
				// The established ADR-0058 layout rides the Result: the
				// payload cgroup as --cgroup-parent, the controllers enabled
				// on the delegated root, the payload cgroup created.
				if got, want := r.CgroupParent(), "/system.slice/lab.service/payload"; got != want {
					t.Errorf("CgroupParent() = %q, want %q", got, want)
				}
				wantWrite := cgWrite{"/sys/fs/cgroup/system.slice/lab.service/cgroup.subtree_control", "+memory +pids"}
				if !slices.Contains(f.cgWrites, wantWrite) {
					t.Errorf("cgroup writes %+v missing the controller enable %+v", f.cgWrites, wantWrite)
				}
				if !slices.Contains(f.mkdirs, "/sys/fs/cgroup/system.slice/lab.service/payload") {
					t.Errorf("mkdirs %q missing the payload cgroup", f.mkdirs)
				}
			},
		},
		{
			name: "podman missing skips the version and image probes",
			mutate: func(f *pfFixture) {
				delete(f.paths, "podman")
			},
			wantChecks: []string{CheckPodman},
			after: func(t *testing.T, f *pfFixture, r Result) {
				if len(f.runner.calls) != 0 {
					t.Errorf("commands ran despite podman missing: %q", f.runner.calls)
				}
			},
		},
		{
			name: "podman 3.x is too old and skips the image probes",
			mutate: func(f *pfFixture) {
				f.runner.script["podman version --format {{.Client.Version}}"] = cmdResult{out: "3.4.7\n"}
			},
			wantChecks:  []string{CheckPodman},
			wantVersion: "3.4.7",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, "3.4.7") {
					t.Errorf("hint %q does not name the found version", r.Failures[0].Hint)
				}
				if len(f.runner.calls) != 1 {
					t.Errorf("calls = %q, want only the version probe", f.runner.calls)
				}
			},
		},
		{
			name: "pasta missing",
			mutate: func(f *pfFixture) {
				delete(f.paths, "pasta")
			},
			wantChecks:  []string{CheckPasta},
			wantVersion: "5.2.3",
		},
		{
			name: "subuid entry for another user only",
			mutate: func(f *pfFixture) {
				f.files["/etc/subuid"] = "someoneelse:100000:65536\n"
			},
			wantChecks:  []string{CheckSubuid},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, `"lab:100000:65536"`) {
					t.Errorf("hint %q does not spell the entry to add", r.Failures[0].Hint)
				}
			},
		},
		{
			name: "uid-keyed subuid and subgid entries accepted",
			mutate: func(f *pfFixture) {
				f.files["/etc/subuid"] = "990:100000:65536\n"
				f.files["/etc/subgid"] = "990:100000:65536\n"
			},
			wantVersion: "5.2.3",
		},
		{
			name: "zero-count entry does not satisfy subgid",
			mutate: func(f *pfFixture) {
				f.files["/etc/subgid"] = "lab:100000:0\n"
			},
			wantChecks:  []string{CheckSubgid},
			wantVersion: "5.2.3",
		},
		{
			name: "cgroup v1 host: controllers file absent",
			mutate: func(f *pfFixture) {
				delete(f.files, "/sys/fs/cgroup/cgroup.controllers")
			},
			wantChecks:  []string{CheckCgroup2},
			wantVersion: "5.2.3",
		},
		{
			name: "no unified entry in /proc/self/cgroup",
			mutate: func(f *pfFixture) {
				f.files["/proc/self/cgroup"] = "4:memory:/legacy\n"
			},
			wantChecks:  []string{CheckCgroup2},
			wantVersion: "5.2.3",
		},
		{
			// The controllers never arrived in the payload cgroup — the
			// no-false-green posture retargeted at the subtree containers
			// actually land in (ADR-0058).
			name: "payload cgroup missing memory",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup/system.slice/lab.service/payload/cgroup.controllers"] = "cpu pids\n"
			},
			wantChecks:  []string{CheckDelegation},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, "Delegate=yes") {
					t.Errorf("hint %q does not name the systemd fix", r.Failures[0].Hint)
				}
			},
		},
		{
			// Delegate=no leaves the unit root root-owned: lab cannot enable
			// the payload controllers or create the payload cgroup — the caps
			// would be silently absent (the #205 false green).
			name: "unit root not delegated (not writable)",
			mutate: func(f *pfFixture) {
				delete(f.writable, "/sys/fs/cgroup/system.slice/lab.service")
			},
			wantChecks:  []string{CheckCgroupLayout},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Detail, "not writable") {
					t.Errorf("detail %q does not explain the delegation failure", r.Failures[0].Detail)
				}
				if !strings.Contains(r.Failures[0].Hint, "Delegate=yes") {
					t.Errorf("hint %q does not name the systemd fix", r.Failures[0].Hint)
				}
			},
		},
		{
			// Pre-ADR-0058 layout: lab attached at the unit cgroup root
			// itself (no DelegateSubgroup). Refused — the root could never
			// delegate the payload controllers without re-arming the
			// restart-EBUSY trap.
			name: "lab at the unit cgroup root (no DelegateSubgroup)",
			mutate: func(f *pfFixture) {
				f.files["/proc/self/cgroup"] = "0::/system.slice/lab.service\n"
			},
			wantChecks:  []string{CheckCgroupLayout},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, "DelegateSubgroup=main") {
					t.Errorf("hint %q does not name DelegateSubgroup", r.Failures[0].Hint)
				}
			},
		},
		{
			// A tmux server surviving from a pre-ADR-0058 deploy still sits
			// at the delegated root: setupCgroups adopts it into lab's own
			// cgroup (the root must be process-free before controllers can be
			// enabled on it), and the fixture's kernel-faithful pid-move
			// leaves the root empty for the guard.
			name: "stray root pids adopted into lab's cgroup",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup/system.slice/lab.service/cgroup.procs"] = "4242\n"
			},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				want := cgWrite{"/sys/fs/cgroup/system.slice/lab.service/main/cgroup.procs", "4242"}
				if !slices.Contains(f.cgWrites, want) {
					t.Errorf("cgroup writes %+v missing the stray adoption %+v", f.cgWrites, want)
				}
			},
		},
		{
			name: "enabling the payload controllers fails",
			mutate: func(f *pfFixture) {
				f.writeErr = map[string]error{
					"/sys/fs/cgroup/system.slice/lab.service/cgroup.subtree_control": errors.New("device or resource busy"),
				}
			},
			wantChecks:  []string{CheckDelegation},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Detail, "cannot enable memory/pids") {
					t.Errorf("detail %q does not name the failed enable", r.Failures[0].Detail)
				}
			},
		},
		{
			// The restart-safety guard at boot: lab's own cgroup delegating
			// controllers is exactly the state that wedged the 2026-07-25
			// restart — refuse loudly instead of arming the trap again.
			name: "restart guard: lab's own cgroup delegates controllers",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup/system.slice/lab.service/main/cgroup.subtree_control"] = "memory pids\n"
			},
			wantChecks:  []string{CheckRestartSafety},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, "lab-cgroup-hygiene") {
					t.Errorf("hint %q does not name the hygiene unit", r.Failures[0].Hint)
				}
				if r.Cgroups != nil {
					t.Errorf("Cgroups = %+v, want nil on a guard failure", r.Cgroups)
				}
			},
		},
		{
			// A stray root pid whose adoption did not stick (the write
			// failed) must fail the guard — the root holding processes is the
			// other half of the restart trap.
			name: "restart guard: unadoptable stray at the delegated root",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup/system.slice/lab.service/cgroup.procs"] = "4242\n"
				f.writeErr = map[string]error{
					"/sys/fs/cgroup/system.slice/lab.service/main/cgroup.procs": errors.New("permission denied"),
				}
			},
			wantChecks:  []string{CheckRestartSafety},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Detail, "4242") {
					t.Errorf("detail %q does not name the stray pid", r.Failures[0].Detail)
				}
			},
		},
		{
			// issue #207: the dev image is no longer a preflight concern (it is
			// per-repo-or-global, ensured at spawn), so ONLY a missing tools
			// image fails here — an unset global dev image is fine, proven both
			// by the green fixture carrying no dev image and by this check
			// producing exactly one failure, the tools one.
			name: "unconfigured tools image (unset dev image is not a failure)",
			mutate: func(f *pfFixture) {
				f.cfg.ToolsImages = nil
			},
			wantChecks:  []string{CheckToolsImage},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, "--container-tools-image") {
					t.Errorf("tools hint %q does not name the flag", r.Failures[0].Hint)
				}
				for _, fa := range r.Failures {
					if fa.Check == "image" {
						t.Errorf("unset dev image produced a preflight failure %+v — #207 moved that check to the spawn", fa)
					}
				}
			},
		},
		{
			// Pull-first (ADR-0054): the pull happens unconditionally — no
			// `image exists` short-circuit that would pin the host to the
			// first digest a moving tag ever resolved to.
			name: "tools image pull runs even with no local probe first",
			mutate: func(f *pfFixture) {
				f.runner.script["podman pull reg/agent-tools@sha256:aa"] = cmdResult{}
			},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				want := [][]string{
					{"podman", "version", "--format", "{{.Client.Version}}"},
					{"podman", "pull", "reg/agent-tools@sha256:aa"},
				}
				if len(f.runner.calls) != len(want) {
					t.Fatalf("calls = %q, want %q", f.runner.calls, want)
				}
				for i := range want {
					assertArgv(t, f.runner.calls[i], want[i])
				}
			},
		},
		{
			// Registry down but the image is cached: degrade to the cache
			// with a warning instead of refusing spawns — stale tools beat
			// none.
			name: "tools pull fails with cached image: OK plus warning",
			mutate: func(f *pfFixture) {
				f.runner.script["podman pull reg/agent-tools@sha256:aa"] = cmdResult{err: errors.New("connection refused")}
				f.runner.script["podman image exists reg/agent-tools@sha256:aa"] = cmdResult{}
			},
			wantVersion:  "5.2.3",
			wantWarnings: 1,
			after: func(t *testing.T, f *pfFixture, r Result) {
				if w := r.Warnings[0]; !strings.Contains(w, "reg/agent-tools@sha256:aa") || !strings.Contains(w, "cached") {
					t.Errorf("warning %q does not name the ref and the cached fallback", w)
				}
			},
		},
		{
			name: "tools image pull fails, providers reported in sorted order",
			mutate: func(f *pfFixture) {
				f.cfg.ToolsImages = map[string]string{
					"codex":       "reg/agent-tools@sha256:bb",
					"claude-code": "reg/agent-tools@sha256:aa",
				}
				f.runner.script["podman image exists reg/agent-tools@sha256:aa"] = cmdResult{err: errors.New("exit status 1")}
				f.runner.script["podman pull reg/agent-tools@sha256:aa"] = cmdResult{err: errors.New("manifest unknown")}
				f.runner.script["podman image exists reg/agent-tools@sha256:bb"] = cmdResult{err: errors.New("exit status 1")}
				f.runner.script["podman pull reg/agent-tools@sha256:bb"] = cmdResult{err: errors.New("manifest unknown")}
			},
			wantChecks:  []string{CheckToolsPull, CheckToolsPull},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if d := r.Failures[0].Detail; !strings.Contains(d, "claude-code") || !strings.Contains(d, "reg/agent-tools@sha256:aa") {
					t.Errorf("failure[0] detail %q does not name provider and ref", d)
				}
				if d := r.Failures[1].Detail; !strings.Contains(d, "codex") || !strings.Contains(d, "reg/agent-tools@sha256:bb") {
					t.Errorf("failure[1] detail %q does not name provider and ref", d)
				}
			},
		},
		{
			name: "bare host collects every failure at once",
			mutate: func(f *pfFixture) {
				f.paths = map[string]string{}
				f.files = map[string]string{}
				f.cfg.ToolsImages = nil
			},
			wantChecks: []string{
				CheckPodman, CheckPasta, CheckSubuid, CheckSubgid,
				CheckCgroup2, CheckToolsImage,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := greenFixture()
			tt.mutate(f)
			r := Preflight(context.Background(), f.cfg, f.deps())

			var gotChecks []string
			for _, fa := range r.Failures {
				gotChecks = append(gotChecks, fa.Check)
			}
			if !slices.Equal(gotChecks, tt.wantChecks) {
				t.Fatalf("failure checks = %q, want %q (failures: %+v)", gotChecks, tt.wantChecks, r.Failures)
			}
			if r.OK() != (len(tt.wantChecks) == 0) {
				t.Errorf("OK() = %v with failures %+v", r.OK(), r.Failures)
			}
			// HasPullFailure keys cmd/lab's retry loop (issue #220): true
			// exactly when a tools-image pull failure is among the failures.
			if got, want := r.HasPullFailure(), slices.Contains(gotChecks, CheckToolsPull); got != want {
				t.Errorf("HasPullFailure() = %v, want %v (failures: %+v)", got, want, r.Failures)
			}
			if r.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", r.Version, tt.wantVersion)
			}
			if len(r.Warnings) != tt.wantWarnings {
				t.Errorf("Warnings = %q, want %d of them", r.Warnings, tt.wantWarnings)
			}
			// Error() must mention every failure — a spawn refusal that
			// names only the first would hide the rest.
			if !r.OK() {
				msg := r.Error()
				for _, fa := range r.Failures {
					if !strings.Contains(msg, fa.Check) || !strings.Contains(msg, fa.Hint) {
						t.Errorf("Error() %q omits failure %+v", msg, fa)
					}
				}
			}
			if tt.after != nil {
				tt.after(t, f, r)
			}
		})
	}
}
