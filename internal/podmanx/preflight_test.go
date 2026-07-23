package podmanx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// pfFixture is one simulated host: PATH lookups, file contents, and a
// scripted runner. greenFixture returns a host where every check passes;
// each table case mutates exactly the piece it breaks.
type pfFixture struct {
	cfg    PreflightConfig
	paths  map[string]string
	files  map[string]string
	runner *recordingRunner
}

func greenFixture() *pfFixture {
	return &pfFixture{
		cfg: PreflightConfig{
			PodmanBin: "podman",
			Image:     "docker.io/library/debian:stable-slim",
			ToolsImages: map[string]string{
				"claude-code": "reg/agent-tools@sha256:aa",
			},
		},
		paths: map[string]string{
			"podman": "/usr/bin/podman",
			"pasta":  "/usr/bin/pasta",
		},
		files: map[string]string{
			"/etc/subuid":                       "lab:100000:65536\n",
			"/etc/subgid":                       "lab:100000:65536\n",
			"/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory hugetlb pids\n",
			"/proc/self/cgroup":                 "0::/system.slice/lab.service\n",
			"/sys/fs/cgroup/system.slice/lab.service/cgroup.controllers": "memory pids\n",
		},
		runner: &recordingRunner{script: map[string]cmdResult{
			"podman version --format {{.Client.Version}}":   {out: "5.2.3\n"},
			"podman image exists reg/agent-tools@sha256:aa": {},
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
		Run:      f.runner.run,
		Username: "lab",
		UID:      990,
	}
}

func TestPreflight(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*pfFixture)
		wantChecks  []string // Failure.Check values in order; nil means OK
		wantVersion string
		after       func(*testing.T, *pfFixture, Result)
	}{
		{
			name:        "all green",
			mutate:      func(f *pfFixture) {},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if r.Error() != "" {
					t.Errorf("Error() = %q, want empty on OK", r.Error())
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
			name: "delegation missing memory",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup/system.slice/lab.service/cgroup.controllers"] = "cpu pids\n"
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
			name: "unconfigured images",
			mutate: func(f *pfFixture) {
				f.cfg.Image = ""
				f.cfg.ToolsImages = nil
			},
			wantChecks:  []string{CheckImage, CheckToolsImage},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, "--container-image") {
					t.Errorf("image hint %q does not name the flag", r.Failures[0].Hint)
				}
				if !strings.Contains(r.Failures[1].Hint, "--container-tools-image") {
					t.Errorf("tools hint %q does not name the flag", r.Failures[1].Hint)
				}
			},
		},
		{
			name: "tools image absent locally, pull succeeds",
			mutate: func(f *pfFixture) {
				f.runner.script["podman image exists reg/agent-tools@sha256:aa"] = cmdResult{err: errors.New("exit status 1")}
				f.runner.script["podman pull reg/agent-tools@sha256:aa"] = cmdResult{}
			},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				want := [][]string{
					{"podman", "version", "--format", "{{.Client.Version}}"},
					{"podman", "image", "exists", "reg/agent-tools@sha256:aa"},
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
				f.cfg.Image = ""
				f.cfg.ToolsImages = nil
			},
			wantChecks: []string{
				CheckPodman, CheckPasta, CheckSubuid, CheckSubgid,
				CheckCgroup2, CheckImage, CheckToolsImage,
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
			if r.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", r.Version, tt.wantVersion)
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
