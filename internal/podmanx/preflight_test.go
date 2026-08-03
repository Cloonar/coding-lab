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

// The green fixture's host constants: the service uid, its user-manager
// runtime dir (the user-manager check's probe target), and the transient
// scope podman's systemd cgroup manager places the probe container in. The
// scope path deliberately carries a /container leaf below the scope — crun's
// real shape — so the green path exercises spawnProbe's normalization down
// to the libpod-*.scope component, where the cap files actually live.
const (
	testUID       = 990
	testRunUser   = "/run/user/990"
	probeScopeDir = "/user.slice/user-990.slice/user@990.service/user.slice/libpod-abc123.scope"

	// The pasta capability probe (check 2), keyed exactly as the runner sees
	// it. The option precedes --version deliberately: reversed, --version
	// would exit zero on every passt ever built and the check would be a
	// guaranteed false green.
	pastaProbe = "pasta --map-guest-addr none --version"

	probeRm      = "podman rm --force --ignore --time 0 lab-preflight-probe"
	probeCreate  = "podman --cgroup-manager=systemd create --name lab-preflight-probe --memory 64m --pids-limit 16 reg/agent-tools@sha256:aa /bin/labctl"
	probeInit    = "podman init lab-preflight-probe"
	probeInspect = "podman inspect --format {{.State.Pid}} {{.State.CgroupPath}} lab-preflight-probe"
)

// pfFixture is one simulated host: PATH lookups, file contents, writable
// paths, and a scripted runner. greenFixture returns a host where every
// check passes — including the spawn probe's full podman conversation —
// and each table case mutates exactly the piece it breaks.
type pfFixture struct {
	cfg      PreflightConfig
	paths    map[string]string
	files    map[string]string
	writable map[string]bool // paths the service user can write (the user-manager probe)
	runner   *recordingRunner
}

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
		files: map[string]string{
			"/etc/subuid":                       "lab:100000:65536\n",
			"/etc/subgid":                       "lab:100000:65536\n",
			"/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory hugetlb pids\n",
			"/proc/self/cgroup":                 "0::/system.slice/lab.service\n",
			// The probe's cap files, at the SCOPE — not at the /container
			// leaf inspect reports — proving the caps landed as per-scope
			// properties (MemoryMax/TasksMax, ADR-0060).
			"/sys/fs/cgroup" + probeScopeDir + "/memory.max": "67108864\n",
			"/sys/fs/cgroup" + probeScopeDir + "/pids.max":   "16\n",
		},
		// systemd-logind creates /run/user/<uid> writable for the user
		// exactly while user@<uid>.service runs — the user-manager
		// reachability signal.
		writable: map[string]bool{testRunUser: true},
		runner: &recordingRunner{script: map[string]cmdResult{
			"podman version --format {{.Client.Version}}": {out: "5.2.3\n"},
			// pasta's capability probe: the option comes BEFORE --version, so
			// a passt too old for --map-guest-addr fails getopt_long before
			// --version can exit zero. A modern passt parses "none", then
			// prints its version blob to stdout and exits zero.
			pastaProbe: {out: "pasta 2024_11_27.a999ffe\n"},
			// Pull-first (ADR-0054): even a locally-present tools image is
			// re-pulled so a moved tag reaches the host.
			"podman pull reg/agent-tools@sha256:aa": {},
			// The spawn probe's conversation; one rm script answers both the
			// pre-clean and the deferred cleanup.
			probeRm:      {},
			probeCreate:  {},
			probeInit:    {},
			probeInspect: {out: "12345 " + probeScopeDir + "/container\n"},
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
		Run:      f.runner.run,
		Username: "lab",
		UID:      testUID,
	}
}

// probeCalls returns every recorded command that names the probe container —
// empty when the probe was (correctly) never attempted.
func (f *pfFixture) probeCalls() [][]string {
	var out [][]string
	for _, c := range f.runner.calls {
		if slices.Contains(c, probeName) {
			out = append(out, c)
		}
	}
	return out
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
				// The exact command sequence, pinned end to end: the podman
				// version probe, pasta's capability probe (check 2, hence
				// before the pull), then the spawn probe's pre-clean, create
				// (with the manager flag and both caps), init, inspect, and
				// the deferred cleanup — cleanup runs on success too, so a
				// green preflight leaves no probe container behind.
				want := [][]string{
					{"podman", "version", "--format", "{{.Client.Version}}"},
					{"pasta", "--map-guest-addr", "none", "--version"},
					{"podman", "pull", "reg/agent-tools@sha256:aa"},
					{"podman", "rm", "--force", "--ignore", "--time", "0", probeName},
					{"podman", "--cgroup-manager=systemd", "create", "--name", probeName,
						"--memory", "64m", "--pids-limit", "16", "reg/agent-tools@sha256:aa", "/bin/labctl"},
					{"podman", "init", probeName},
					{"podman", "inspect", "--format", "{{.State.Pid}} {{.State.CgroupPath}}", probeName},
					{"podman", "rm", "--force", "--ignore", "--time", "0", probeName},
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
			name: "podman missing skips the version, image and probe calls",
			mutate: func(f *pfFixture) {
				delete(f.paths, "podman")
			},
			wantChecks: []string{CheckPodman},
			after: func(t *testing.T, f *pfFixture, r Result) {
				// Not a single podman invocation: the binary is not there, so
				// running it could only produce a second report of one root
				// cause. pasta's own probe is unrelated and still runs — a
				// broken podman says nothing about the host's passt.
				for _, c := range f.runner.calls {
					if c[0] == "podman" {
						t.Errorf("podman commands ran despite podman missing: %q", f.runner.calls)
						break
					}
				}
			},
		},
		{
			name: "podman 3.x is too old and skips the image and probe calls",
			mutate: func(f *pfFixture) {
				f.runner.script["podman version --format {{.Client.Version}}"] = cmdResult{out: "3.4.7\n"}
			},
			wantChecks:  []string{CheckPodman},
			wantVersion: "3.4.7",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Hint, "3.4.7") {
					t.Errorf("hint %q does not name the found version", r.Failures[0].Hint)
				}
				// Only the two capability probes of checks 1 and 2 — no pull,
				// no spawn probe: everything downstream of a too-old podman
				// would just re-report it.
				want := [][]string{
					{"podman", "version", "--format", "{{.Client.Version}}"},
					{"pasta", "--map-guest-addr", "none", "--version"},
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
			// A host with no pasta at all is blamed exactly once. The
			// capability probe is NOT attempted: exec'ing a binary that is not
			// on PATH could only fail, and that second failure would land
			// under the same check id — two entries, one root cause, which is
			// precisely what the collect-everything contract forbids.
			name: "pasta missing",
			mutate: func(f *pfFixture) {
				delete(f.paths, "pasta")
			},
			wantChecks:  []string{CheckPasta},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				for _, c := range f.runner.calls {
					if c[0] == "pasta" {
						t.Errorf("capability probe ran despite pasta missing: %q", c)
					}
				}
				if h := r.Failures[0].Hint; !strings.Contains(h, "install passt") {
					t.Errorf("hint %q does not tell the operator to install passt", h)
				}
			},
		},
		{
			// pasta is installed but predates --map-guest-addr entirely
			// (before passt 2024_08_21). getopt_long returns '?' for the
			// unknown option before --version can exit, so the probe exits
			// non-zero — which is the whole reason the option precedes
			// --version. Without this check the host would pass preflight and
			// then fail EVERY container at start: podman's back-compat retry
			// for --map-guest-addr fires only for the default podman itself
			// appended, never for the user-supplied one in the run argv.
			name: "pasta too old: option unknown",
			mutate: func(f *pfFixture) {
				f.runner.script[pastaProbe] = cmdResult{err: errors.New("exit status 1: unrecognized option '--map-guest-addr'")}
			},
			wantChecks:  []string{CheckPasta},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				// The operator action, not just the diagnosis: an old passt is
				// host state, so the hint must name the upgrade and the
				// release that makes it enough.
				if h := r.Failures[0].Hint; !strings.Contains(h, "passt") || !strings.Contains(h, "2025_04_15") {
					t.Errorf("hint %q does not name the passt upgrade and the release that makes the option usable", h)
				}
				// ExecRunner folds pasta's stderr into the error, so the
				// detail quotes pasta's own words rather than paraphrasing.
				d := r.Failures[0].Detail
				if !strings.Contains(d, "unrecognized option '--map-guest-addr'") {
					t.Errorf("detail %q does not carry pasta's own explanation", d)
				}
				if !strings.Contains(d, "--map-guest-addr") {
					t.Errorf("detail %q does not name the rejected option", d)
				}
				// Nothing a retry can heal: only a new passt on the host can.
				if r.HasRetryableFailure() {
					t.Errorf("HasRetryableFailure() = true; an old passt is host state no retry changes")
				}
			},
		},
		{
			// The subtle half of the floor, and the reason the probe passes
			// the VALUE rather than reading a version: passt 2024_08_21
			// through 2025_03_20 recognize --map-guest-addr but reject "none"
			// — conf_nat() fell through its own literal branch into inet_pton
			// and died. Such a host answers `pasta --help` with a line
			// advertising the option, and any version gate keyed on "when was
			// --map-guest-addr added" calls it green; only running the exact
			// option/value pair the run argv uses tells the truth.
			name: "pasta recognizes --map-guest-addr but rejects none",
			mutate: func(f *pfFixture) {
				f.runner.script[pastaProbe] = cmdResult{err: errors.New("exit status 1: Invalid address to remap to host: none")}
			},
			wantChecks:  []string{CheckPasta},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if d := r.Failures[0].Detail; !strings.Contains(d, "Invalid address to remap to host: none") {
					t.Errorf("detail %q does not carry pasta's own explanation", d)
				}
				if h := r.Failures[0].Hint; !strings.Contains(h, "2025_04_15") {
					t.Errorf("hint %q does not name the release that makes the option usable", h)
				}
			},
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
			name: "cgroup v1 host: controllers file absent, probe skipped",
			mutate: func(f *pfFixture) {
				delete(f.files, "/sys/fs/cgroup/cgroup.controllers")
			},
			wantChecks:  []string{CheckCgroup2},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if pc := f.probeCalls(); pc != nil {
					t.Errorf("probe attempted on a cgroup-v1 host: %q", pc)
				}
			},
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
			// The user manager is unreachable — user@<uid>.service down, or
			// lingering never enabled. Without it podman's systemd cgroup
			// manager has nobody to ask for a scope, so the probe is not even
			// attempted: its failure would only re-report this root cause.
			name: "user manager runtime dir not writable",
			mutate: func(f *pfFixture) {
				delete(f.writable, testRunUser)
			},
			wantChecks:  []string{CheckUserManager},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if d := r.Failures[0].Detail; !strings.Contains(d, testRunUser) || !strings.Contains(d, "user@990.service") {
					t.Errorf("detail %q does not name the runtime dir and the user unit", d)
				}
				if h := r.Failures[0].Hint; !strings.Contains(h, "linger") || !strings.Contains(h, "user@990.service") {
					t.Errorf("hint %q does not name lingering and the user unit", h)
				}
				if !r.HasRetryableFailure() {
					t.Errorf("HasRetryableFailure() = false; the user manager can race a fresh boot and must be retried")
				}
				if pc := f.probeCalls(); pc != nil {
					t.Errorf("probe attempted despite an unreachable user manager: %q", pc)
				}
			},
		},
		{
			// The probe container cannot even be created — e.g. podman cannot
			// reach the user manager's bus. Podman's own stderr (folded into
			// the error by ExecRunner) must surface in the Detail, and the
			// deferred cleanup must still run.
			name: "probe create fails",
			mutate: func(f *pfFixture) {
				f.runner.script[probeCreate] = cmdResult{err: errors.New("exit status 125: cannot connect to user manager bus")}
			},
			wantChecks:  []string{CheckSpawnProbe},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				if !strings.Contains(r.Failures[0].Detail, "cannot connect to user manager bus") {
					t.Errorf("detail %q does not quote podman's stderr", r.Failures[0].Detail)
				}
				last := f.runner.calls[len(f.runner.calls)-1]
				assertArgv(t, last, []string{"podman", "rm", "--force", "--ignore", "--time", "0", probeName})
				for _, c := range f.runner.calls {
					if c[1] == "init" {
						t.Errorf("init ran after a failed create: %q", f.runner.calls)
					}
				}
			},
		},
		{
			// The container spawned but NOT into a libpod-*.scope: the
			// systemd cgroup manager did not engage (e.g. a cgroupfs
			// fallback). The caps would land somewhere unverified — a
			// failure even though the container "works".
			name: "probe lands outside a transient scope",
			mutate: func(f *pfFixture) {
				f.runner.script[probeInspect] = cmdResult{out: "12345 /user.slice/user-990.slice/somewhere/else\n"}
			},
			wantChecks:  []string{CheckSpawnProbe},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				d := r.Failures[0].Detail
				if !strings.Contains(d, "/user.slice/user-990.slice/somewhere/else") || !strings.Contains(d, "transient scope") {
					t.Errorf("detail %q does not quote the observed path and name the missing scope", d)
				}
			},
		},
		{
			// The no-false-green assertion itself: the scope exists but the
			// memory cap did not land (memory.max still "max").
			name: "probe memory.max mismatch",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup"+probeScopeDir+"/memory.max"] = "max\n"
			},
			wantChecks:  []string{CheckSpawnProbe},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				d := r.Failures[0].Detail
				if !strings.Contains(d, `"max"`) || !strings.Contains(d, `"67108864"`) || !strings.Contains(d, "memory.max") {
					t.Errorf("detail %q does not quote got/want for memory.max", d)
				}
			},
		},
		{
			name: "probe pids.max mismatch",
			mutate: func(f *pfFixture) {
				f.files["/sys/fs/cgroup"+probeScopeDir+"/pids.max"] = "max\n"
			},
			wantChecks:  []string{CheckSpawnProbe},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				d := r.Failures[0].Detail
				if !strings.Contains(d, `"max"`) || !strings.Contains(d, `"16"`) || !strings.Contains(d, "pids.max") {
					t.Errorf("detail %q does not quote got/want for pids.max", d)
				}
			},
		},
		{
			// Older podmans report State.CgroupPath empty on a created-but-
			// not-started container; the probe then reads the scope out of
			// /proc/<pid>/cgroup's 0:: entry instead. Green end-to-end here
			// proves the fallback resolves the same scope (the cap files only
			// exist at probeScopeDir).
			name: "inspect without CgroupPath falls back to /proc/<pid>/cgroup",
			mutate: func(f *pfFixture) {
				f.runner.script[probeInspect] = cmdResult{out: "12345 \n"}
				f.files["/proc/12345/cgroup"] = "0::" + probeScopeDir + "/container\n"
			},
			wantVersion: "5.2.3",
		},
		{
			// issue #207: the dev image is no longer a preflight concern (it is
			// per-repo-or-global, ensured at spawn), so ONLY a missing tools
			// image fails here — an unset global dev image is fine, proven both
			// by the green fixture carrying no dev image and by this check
			// producing exactly one failure, the tools one. With no configured
			// ref there is nothing to probe with either — and no spurious
			// spawn-probe failure on top of the root cause.
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
				if pc := f.probeCalls(); pc != nil {
					t.Errorf("probe attempted with no usable tools ref: %q", pc)
				}
			},
		},
		{
			// Pull-first (ADR-0054): the pull happens unconditionally — no
			// `image exists` short-circuit that would pin the host to the
			// first digest a moving tag ever resolved to.
			name:        "tools image pull runs even with no local probe first",
			mutate:      func(f *pfFixture) {},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				want := [][]string{
					{"podman", "version", "--format", "{{.Client.Version}}"},
					{"pasta", "--map-guest-addr", "none", "--version"},
					{"podman", "pull", "reg/agent-tools@sha256:aa"},
				}
				if len(f.runner.calls) < len(want) {
					t.Fatalf("calls = %q, want at least %q", f.runner.calls, want)
				}
				for i := range want {
					assertArgv(t, f.runner.calls[i], want[i])
				}
			},
		},
		{
			// Registry down but the image is cached: degrade to the cache
			// with a warning instead of refusing spawns — stale tools beat
			// none. The cached ref counts as USABLE, so the spawn probe still
			// runs against it.
			name: "tools pull fails with cached image: OK plus warning, probe runs",
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
				if f.probeCalls() == nil {
					t.Errorf("probe not attempted despite a usable (cached) tools ref")
				}
			},
		},
		{
			name: "tools image pull fails, providers reported in sorted order, probe skipped",
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
				if pc := f.probeCalls(); pc != nil {
					t.Errorf("probe attempted with no usable tools ref: %q", pc)
				}
			},
		},
		{
			// The probe spawns with the FIRST usable ref in provider-sorted
			// order: an unusable earlier provider (pull failed, no cache) is
			// reported and skipped over, not allowed to starve the probe.
			name: "probe uses the first usable ref in provider order",
			mutate: func(f *pfFixture) {
				f.cfg.ToolsImages = map[string]string{
					"claude-code": "reg/agent-tools@sha256:aa",
					"codex":       "reg/agent-tools@sha256:bb",
				}
				f.runner.script["podman pull reg/agent-tools@sha256:aa"] = cmdResult{err: errors.New("manifest unknown")}
				f.runner.script["podman image exists reg/agent-tools@sha256:aa"] = cmdResult{err: errors.New("exit status 1")}
				f.runner.script["podman pull reg/agent-tools@sha256:bb"] = cmdResult{}
				f.runner.script["podman --cgroup-manager=systemd create --name lab-preflight-probe --memory 64m --pids-limit 16 reg/agent-tools@sha256:bb /bin/labctl"] = cmdResult{}
			},
			wantChecks:  []string{CheckToolsPull},
			wantVersion: "5.2.3",
			after: func(t *testing.T, f *pfFixture, r Result) {
				for _, c := range f.probeCalls() {
					if slices.Contains(c, "create") && !slices.Contains(c, "reg/agent-tools@sha256:bb") {
						t.Errorf("probe create %q did not use the first USABLE ref", c)
					}
				}
			},
		},
		{
			name: "bare host collects every failure at once",
			mutate: func(f *pfFixture) {
				f.paths = map[string]string{}
				f.files = map[string]string{}
				f.writable = map[string]bool{}
				f.cfg.ToolsImages = nil
			},
			// Every root cause, once each — and NO spawn-probe failure: the
			// probe is skipped when its prerequisites already failed, so a
			// bare host is not additionally blamed for a probe it could
			// never run.
			wantChecks: []string{
				CheckPodman, CheckPasta, CheckSubuid, CheckSubgid,
				CheckCgroup2, CheckUserManager, CheckToolsImage,
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
			// HasRetryableFailure keys cmd/lab's retry loop (issue #220):
			// true exactly when a tools-pull or user-manager failure is
			// among the failures — everything else (subuid, cgroup2, …) is
			// host state a retry cannot change.
			wantRetry := slices.Contains(gotChecks, CheckToolsPull) || slices.Contains(gotChecks, CheckUserManager)
			if got := r.HasRetryableFailure(); got != wantRetry {
				t.Errorf("HasRetryableFailure() = %v, want %v (failures: %+v)", got, wantRetry, r.Failures)
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
