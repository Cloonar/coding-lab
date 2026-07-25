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
		// The ADR-0059 layout: lab runs in a plain, UNDELEGATED lab.service
		// cgroup (its subtree_control empty), while the lab-payload.service
		// holder carries the delegation — proc-free at its root
		// (DelegateSubgroup=main keeps the sleep in main/), and its payload
		// cgroup carries memory+pids once setupCgroups enables them on the
		// holder root.
		files: map[string]string{
			"/etc/subuid":                       "lab:100000:65536\n",
			"/etc/subgid":                       "lab:100000:65536\n",
			"/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory hugetlb pids\n",
			"/proc/self/cgroup":                 "0::/system.slice/lab.service\n",
			"/sys/fs/cgroup/system.slice/lab.service/cgroup.subtree_control":             "\n",
			"/sys/fs/cgroup/system.slice/lab-payload.service/cgroup.procs":               "",
			"/sys/fs/cgroup/system.slice/lab-payload.service/payload/cgroup.controllers": "memory pids\n",
		},
		// Delegate=yes + User=lab chowns the holder's delegated root to the
		// service user; a missing or non-delegated holder leaves it root-owned
		// (unwritable).
		writable: map[string]bool{
			"/sys/fs/cgroup/system.slice/lab-payload.service": true,
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
				// The established ADR-0059 layout rides the Result: the payload
				// cgroup as --cgroup-parent, the controllers enabled on the
				// holder root, the payload cgroup created under the holder.
				if got, want := r.CgroupParent(), "/system.slice/lab-payload.service/payload"; got != want {
					t.Errorf("CgroupParent() = %q, want %q", got, want)
				}
				wantWrite := cgWrite{"/sys/fs/cgroup/system.slice/lab-payload.service/cgroup.subtree_control", "+memory +pids"}
				if !slices.Contains(f.cgWrites, wantWrite) {
					t.Errorf("cgroup writes %+v missing the controller enable %+v", f.cgWrites, wantWrite)
				}
				if !slices.Contains(f.mkdirs, "/sys/fs/cgroup/system.slice/lab-payload.service/payload") {
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
			// actually land in (ADR-0059).
			name: "payload cgroup missing memory",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup/system.slice/lab-payload.service/payload/cgroup.controllers"] = "cpu pids\n"
			},
			wantChecks:  []string{CheckDelegation},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, "lab-payload.service") {
					t.Errorf("hint %q does not name the holder unit", r.Failures[0].Hint)
				}
			},
		},
		{
			// The holder is missing or not delegated (Delegate=no, or
			// lab-payload.service not running): its root stays root-owned, so
			// lab cannot enable the payload controllers or create the payload
			// cgroup — the caps would be silently absent (the #205 false green).
			name: "holder not delegated (not writable)",
			mutate: func(f *pfFixture) {
				delete(f.writable, "/sys/fs/cgroup/system.slice/lab-payload.service")
			},
			wantChecks:  []string{CheckHolder},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Detail, "lab-payload.service") {
					t.Errorf("detail %q does not name the holder unit", r.Failures[0].Detail)
				}
				if !strings.Contains(r.Failures[0].Hint, "lab-payload.service") {
					t.Errorf("hint %q does not name the holder unit", r.Failures[0].Hint)
				}
			},
		},
		{
			// A process still sitting at the holder root (systemd < 254
			// ignoring DelegateSubgroup, or a start-up race): setupCgroups
			// adopts it into the holder's main/ subgroup so the root is
			// proc-free before its controllers are enabled.
			name: "stray holder-root pids adopted into main/",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup/system.slice/lab-payload.service/cgroup.procs"] = "4242\n"
			},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				want := cgWrite{"/sys/fs/cgroup/system.slice/lab-payload.service/main/cgroup.procs", "4242"}
				if !slices.Contains(f.cgWrites, want) {
					t.Errorf("cgroup writes %+v missing the stray adoption %+v", f.cgWrites, want)
				}
				if !slices.Contains(f.mkdirs, "/sys/fs/cgroup/system.slice/lab-payload.service/main") {
					t.Errorf("mkdirs %q missing the holder main/ subgroup", f.mkdirs)
				}
			},
		},
		{
			name: "enabling the payload controllers fails",
			mutate: func(f *pfFixture) {
				f.writeErr = map[string]error{
					"/sys/fs/cgroup/system.slice/lab-payload.service/cgroup.subtree_control": errors.New("device or resource busy"),
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
			// The restart-safety guard at boot: lab's OWN cgroup delegating
			// controllers is exactly the state that wedged the 2026-07-26
			// restart — refuse loudly instead of arming the trap again. Named
			// in issue #237's acceptance criteria.
			name: "restart guard: lab's own cgroup delegates controllers",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup/system.slice/lab.service/cgroup.subtree_control"] = "memory\n"
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

// TestCgroupsVerify drives the per-spawn restart-safety guard (ADR-0059)
// directly over an injected readFile: the two invariants it re-checks before
// every spawn, and the vacuous forms hand-built Results rely on. The
// re-armed-trap case is also reached through Preflight above; the
// holder-lost-delegation case is Verify-only (setupCgroups would have failed
// first at boot — it only appears when the holder is restarted AFTER a green
// preflight, which is exactly what the per-spawn guard exists to catch).
func TestCgroupsVerify(t *testing.T) {
	const labDir = "/sys/fs/cgroup/system.slice/lab.service"
	const payloadDir = "/sys/fs/cgroup/system.slice/lab-payload.service/payload"
	read := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if s, ok := m[p]; ok {
				return []byte(s), nil
			}
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
	}
	cg := func(m map[string]string) *Cgroups {
		return &Cgroups{Parent: payloadDir, LabDir: labDir, PayloadDir: payloadDir, readFile: read(m)}
	}

	t.Run("green layout verifies OK", func(t *testing.T) {
		err := cg(map[string]string{
			labDir + "/cgroup.subtree_control": "\n",
			payloadDir + "/cgroup.controllers": "memory pids\n",
		}).Verify()
		if err != nil {
			t.Errorf("Verify() = %v, want nil on the clean layout", err)
		}
	})

	t.Run("re-armed trap: lab's own subtree_control non-empty", func(t *testing.T) {
		err := cg(map[string]string{
			labDir + "/cgroup.subtree_control": "memory pids\n",
			payloadDir + "/cgroup.controllers": "memory pids\n",
		}).Verify()
		if err == nil || !strings.Contains(err.Error(), labDir) {
			t.Errorf("Verify() = %v, want an error naming %s", err, labDir)
		}
	})

	t.Run("holder lost delegation: payload controllers missing pids", func(t *testing.T) {
		err := cg(map[string]string{
			labDir + "/cgroup.subtree_control": "\n",
			payloadDir + "/cgroup.controllers": "memory\n",
		}).Verify()
		if err == nil || !strings.Contains(err.Error(), "lab-payload.service") {
			t.Errorf("Verify() = %v, want an error naming lab-payload.service", err)
		}
	})

	// Vacuous forms: a nil receiver and an empty LabDir (hand-built Results,
	// degenerate wiring) verify OK with no probe at all.
	t.Run("nil receiver verifies OK", func(t *testing.T) {
		var c *Cgroups
		if err := c.Verify(); err != nil {
			t.Errorf("nil Verify() = %v, want nil", err)
		}
	})
	t.Run("empty LabDir verifies OK", func(t *testing.T) {
		if err := (&Cgroups{Parent: payloadDir}).Verify(); err != nil {
			t.Errorf("empty-LabDir Verify() = %v, want nil", err)
		}
	})
}
