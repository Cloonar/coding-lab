package instance

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// Nested containers at the spawn (issue #115 / ADR-0086): the container
// gate's nested stages as Launch refuses them, the nested pane argv and the
// worktree notes of a nested run, and the host runner's indifference to it.

// fakeNested is the NestedRuntime seam: a scripted verdict, the two paths,
// and a count of kicks.
type fakeNested struct {
	failures []podmanx.Failure
	done     bool
	seccomp  string
	archives string
	kicks    atomic.Int32
}

func (n *fakeNested) Verdict() ([]podmanx.Failure, bool) { return n.failures, n.done }
func (n *fakeNested) Kick()                              { n.kicks.Add(1) }
func (n *fakeNested) SeccompPath() string                { return n.seccomp }
func (n *fakeNested) ImageArchives() string              { return n.archives }

// nestedTunFailure is a representative failed nested check.
var nestedTunFailure = podmanx.Failure{
	Check:  podmanx.CheckNestedTun,
	Detail: "/dev/net/tun does not exist (the tun module is not loaded)",
	Hint:   "load the tun module",
}

const (
	testNestedSeccomp = "/var/lib/lab-test/containers/nested-seccomp.json"
	testImageArchives = "/var/lib/lab-test/containers/image-archives"
	testServiceUID    = 990
	testServiceGID    = 991
)

// setSetting writes one settings row.
func (f *fixture) setSetting(key, value string) {
	f.t.Helper()
	if err := f.st.SetSetting(f.t.Context(), key, value); err != nil {
		f.t.Fatalf("SetSetting(%s): %v", key, err)
	}
}

// pinNested pins the fixture repo's Nested containers knob.
func (f *fixture) pinNested(on bool) {
	f.t.Helper()
	if _, err := f.st.UpdateRepoSettings(f.t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		NestedContainers: store.Set(&on),
	}); err != nil {
		f.t.Fatalf("UpdateRepoSettings(nested_containers): %v", err)
	}
}

// wireNested wires a green nested runtime and pins the service ids.
func (f *fixture) wireNested(imageArchives string) *fakeNested {
	n := &fakeNested{done: true, seccomp: testNestedSeccomp, archives: imageArchives}
	f.svc.nested = n
	f.svc.serviceUID, f.svc.serviceGID = testServiceUID, testServiceGID
	return n
}

// startedPane starts a run on the fixture repo and returns its pane argv
// and seeded context file.
func (f *fixture) startedPane(t *testing.T) (argv []string, notes string, runID string) {
	t.Helper()
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatal("session not live after Start")
	}
	return sess.Argv, contextFile(t, filepath.Join(f.worktreeRoot, "proj-20260608-1530")), run.ID
}

// wantContainerArgv is the pane argv TestStart_containerRunner_podmanPaneArgv
// pins, with nested as the RunSpec's Nested.
func (f *fixture) wantContainerArgv(runID string, nested *podmanx.NestedSpec) []string {
	name := "proj~20260608-1530"
	run, err := f.st.RunBySession(f.t.Context(), name)
	if err != nil {
		f.t.Fatalf("RunBySession: %v", err)
	}
	return podmanx.RunArgv(podmanx.RunSpec{
		Bin:         testPodmanBin,
		Name:        podmanx.ContainerName(name),
		Image:       testDevImage,
		ToolsImage:  testToolsImage,
		WorktreeDir: filepath.Join(f.worktreeRoot, "proj-20260608-1530"),
		BareDir:     f.bare(),
		AgentDir:    "/var/lib/lab-test/agent",
		HomeDir:     f.homes.HomePath(runID),
		RuntimeDir:  f.homes.RuntimePath(runID),
		Memory:      "8g",
		Pids:        4096,
		Nofile:      16384,
		Env: []string{
			"LAB_URL=unix:///var/lib/lab-test/agent/agent.sock",
			"HOME=" + podmanx.Home,
			"PATH=" + podmanx.PATH,
		},
		ForwardEnv: []string{"LAB_TOKEN", "TERM"},
		Argv:       f.wantSpawnArgv(name, run.Model, run.Effort, "", runID),
		Nested:     nested,
	})
}

// A container run whose effective knob is on gets the nested pane argv — the
// same RunSpec as a plain run plus Nested with the runtime's seccomp profile
// and image archives directory and the service ids — and the "## Nested
// containers" notes section. Both the pin and the inherited default turn it
// on. With no archives directory the bind and LAB_IMAGE_ARCHIVES are left
// out. The retired graphroot contract (issue #124) never renders.
func TestStart_nestedRun_paneArgvAndNotes(t *testing.T) {
	cases := []struct {
		name     string
		archives string
		on       func(f *fixture)
	}{
		{"pinned on, with the image archives", testImageArchives, func(f *fixture) { f.pinNested(true) }},
		{"inherited on, without the image archives", "", func(f *fixture) {
			f.setSetting(store.SettingNestedContainersDefault, "true")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.enableContainer(t)
			n := f.wireNested(tc.archives)
			tc.on(f)

			argv, notes, runID := f.startedPane(t)
			want := f.wantContainerArgv(runID, &podmanx.NestedSpec{
				SeccompProfile: testNestedSeccomp, ImageArchives: tc.archives, UID: testServiceUID, GID: testServiceGID,
			})
			if !slices.Equal(argv, want) {
				t.Errorf("nested pane argv =\n  %q\nwant\n  %q", argv, want)
			}
			joined := strings.Join(argv, " ")
			for _, sub := range []string{
				"--systemd=always", "--user 0", "--security-opt seccomp=" + testNestedSeccomp,
				"--device /dev/net/tun", podmanx.EntryBin + " container-entry --uid 990 --gid 991 -- ",
			} {
				if !strings.Contains(joined, sub) {
					t.Errorf("nested pane argv lacks %q:\n  %s", sub, joined)
				}
			}
			if slices.Contains(argv, "--init") {
				t.Error("nested pane argv carries --init")
			}
			bind := testImageArchives + ":" + testImageArchives + ":ro"
			if got := slices.Contains(argv, bind); got != (tc.archives != "") {
				t.Errorf("image archives bind present = %v, want %v", got, tc.archives != "")
			}
			env := "LAB_IMAGE_ARCHIVES=" + testImageArchives
			if got := slices.Contains(argv, env); got != (tc.archives != "") {
				t.Errorf("%s present = %v, want %v", env, got, tc.archives != "")
			}
			if strings.Contains(joined, "LAB_SHARED_STORE") || strings.Contains(joined, "shared-store") {
				t.Errorf("nested pane argv carries the retired store:\n  %s", joined)
			}
			if !strings.Contains(notes, "## Nested containers") {
				t.Errorf("nested run's notes lack the Nested containers section:\n%s", notes)
			}
			if n.kicks.Load() != 0 {
				t.Error("a spawn past a published verdict kicked the nested preflight")
			}
		})
	}
}

// A container run whose knob is off keeps today's argv byte for byte and
// gets no notes section — off by the seeded default, and off by a pin that
// beats a global on. A wired nested runtime changes nothing for it.
func TestStart_nestedOff_unchanged(t *testing.T) {
	cases := []struct {
		name string
		off  func(f *fixture)
	}{
		{"seeded default off", func(*fixture) {}},
		{"pinned off under a global on", func(f *fixture) {
			f.setSetting(store.SettingNestedContainersDefault, "true")
			f.pinNested(false)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.enableContainer(t)
			f.wireNested(testImageArchives)
			tc.off(f)

			argv, notes, runID := f.startedPane(t)
			if want := f.wantContainerArgv(runID, nil); !slices.Equal(argv, want) {
				t.Errorf("pane argv =\n  %q\nwant today's shape\n  %q", argv, want)
			}
			if strings.Contains(notes, "Nested containers") {
				t.Errorf("a run that is not nested got the Nested containers section:\n%s", notes)
			}
		})
	}
}

// Every nested refusal fires BEFORE the claim — nothing created, no podman
// call (the dev image is not even probed) — as a *BadRequestError carrying
// its actionable text. A pending verdict also kicks the nested preflight, so
// the retry finds one.
func TestStart_nestedRefusals(t *testing.T) {
	cases := []struct {
		name      string
		wire      func(f *fixture) *fakeNested
		wantMsg   []string
		wantKicks int32
	}{
		{
			name: "pending: refused and kicked",
			wire: func(f *fixture) *fakeNested {
				f.pinNested(true)
				n := f.wireNested(testImageArchives)
				n.done = false
				return n
			},
			wantMsg:   []string{"nested containers preflight has not finished — retry in a moment"},
			wantKicks: 1,
		},
		{
			name: "failed: refused with every failure",
			wire: func(f *fixture) *fakeNested {
				f.pinNested(true)
				n := f.wireNested(testImageArchives)
				n.failures = []podmanx.Failure{nestedTunFailure,
					{Check: podmanx.CheckNestedProbe, Detail: "the nested self-test failed: fail: cgroup: read-only", Hint: "check crun"}}
				return n
			},
			wantMsg: []string{"nested containers preflight failed: " +
				"nested-tun: /dev/net/tun does not exist (the tun module is not loaded) (load the tun module); " +
				"nested-probe: the nested self-test failed: fail: cgroup: read-only (check crun)"},
		},
		{
			name: "unavailable: on, but no nested runtime wired",
			wire: func(f *fixture) *fakeNested {
				f.setSetting(store.SettingNestedContainersDefault, "true")
				f.svc.nested = nil
				return nil
			},
			wantMsg: []string{"nested containers are on for repo proj", "no nested containers support", "Settings → Runner"},
		},
		{
			name: "unresolvable inherited default",
			wire: func(f *fixture) *fakeNested {
				f.setSetting(store.SettingNestedContainersDefault, "maybe")
				return f.wireNested(testImageArchives)
			},
			wantMsg: []string{"cannot resolve Nested containers for repo proj", `holds "maybe"`, "Settings → Runner"},
		},
		{
			name: "unreadable inherited default",
			wire: func(f *fixture) *fakeNested {
				n := f.wireNested(testImageArchives)
				f.breakSettingRead(f.t, store.SettingNestedContainersDefault)
				return n
			},
			wantMsg: []string{"nested_containers_default", "could not be read", "integer overflow"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			rec := f.enableContainer(t)
			n := tc.wire(f)

			_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
			if err == nil {
				t.Fatal("Start succeeded, want a nested refusal")
			}
			var bad *BadRequestError
			if !errors.As(err, &bad) {
				t.Errorf("refusal type = %T (%v), want *BadRequestError", err, err)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}
			if n != nil && n.kicks.Load() != tc.wantKicks {
				t.Errorf("kicks = %d, want %d", n.kicks.Load(), tc.wantKicks)
			}
			if calls := rec.recorded(); len(calls) != 0 {
				t.Errorf("refused spawn issued podman calls %q, want none", calls)
			}
			f.assertNothingClaimed(t)
		})
	}
}

// A host-runner spawn never reads the Nested containers knob: a garbage —
// even an unreadable — nested_containers_default, or a repo pinned on with
// no nested runtime, leaves it spawning the plain provider argv.
func TestStart_hostRunner_ignoresNested(t *testing.T) {
	cases := []struct {
		name string
		wire func(f *fixture)
	}{
		{"garbage default", func(f *fixture) { f.setSetting(store.SettingNestedContainersDefault, "maybe") }},
		{"unreadable default", func(f *fixture) { f.breakSettingRead(f.t, store.SettingNestedContainersDefault) }},
		{"pinned on, no runtime", func(f *fixture) { f.pinNested(true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			n := &fakeNested{}
			f.svc.nested = n
			tc.wire(f)
			if tc.name == "pinned on, no runtime" {
				f.svc.nested = nil
			}

			argv, notes, _ := f.startedPane(t)
			if argv[0] != "claude" {
				t.Errorf("host pane argv starts with %q, want the provider CLI", argv[0])
			}
			if strings.Contains(notes, "Nested containers") {
				t.Error("a host run got the Nested containers section")
			}
			if n.kicks.Load() != 0 {
				t.Error("a host spawn kicked the nested preflight")
			}
		})
	}
}

// assertNothingClaimed: no worktree, branch, run row, session or per-run tree.
func (f *fixture) assertNothingClaimed(t *testing.T) {
	t.Helper()
	if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
		t.Error("refused spawn created a worktree")
	}
	if f.branchExists("lab/20260608-1530") {
		t.Error("refused spawn created a branch")
	}
	if _, err := f.st.RunBySession(t.Context(), "proj~20260608-1530"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RunBySession after refusal: %v, want ErrNotFound (no run row)", err)
	}
	if _, live := f.runner.Session("proj~20260608-1530"); live {
		t.Error("refused spawn left a session")
	}
	if _, err := os.Stat(f.instancesDir); err == nil {
		t.Error("refused spawn materialized a per-run tree")
	}
}
