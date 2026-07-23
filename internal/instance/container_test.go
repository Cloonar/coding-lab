package instance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// recordingCmdRunner is the podmanx.CmdRunner fake for the backstop tests:
// records every invocation, answers scripted output keyed by the space-joined
// command line ("" default), never spawns podman.
type recordingCmdRunner struct {
	mu     sync.Mutex
	calls  [][]string
	script map[string]string
}

func newRecordingCmdRunner() *recordingCmdRunner {
	return &recordingCmdRunner{script: map[string]string{}}
}

func (r *recordingCmdRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	return []byte(r.script[strings.Join(argv, " ")]), nil
}

func (r *recordingCmdRunner) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// The canonical container config the tests wire onto the fixture service
// (internal-package field pokes — the same values cmd/lab would pass
// through instance.Options).
const (
	testPodmanBin  = "podman-test"
	testDevImage   = "docker.io/library/debian:stable-slim"
	testToolsImage = "git.cloonar.com/cloonar/agent-tools@sha256:deadbeef"
)

// enableContainer flips the fixture repo to Runner=container and wires the
// service's container seam: an OK preflight, the recording exec seam, and
// the canonical images. Returns the recorder for backstop assertions.
func (f *fixture) enableContainer(t *testing.T) *recordingCmdRunner {
	t.Helper()
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		Runner: store.Set(store.RunnerContainer),
	}); err != nil {
		t.Fatalf("UpdateRepoSettings(runner=container): %v", err)
	}
	rec := newRecordingCmdRunner()
	f.svc.podmanBin = testPodmanBin
	f.svc.containerImage = testDevImage
	f.svc.containerToolsImages = map[string]string{"claude-code": testToolsImage}
	f.svc.podmanRun = rec.run
	f.svc.containerPreflight = func() (podmanx.Result, bool) { return podmanx.Result{Version: "5.0.0"}, true }
	f.svc.agentSockDir = "/var/lib/lab-test/agent"
	return rec
}

// A container-mode Start produces a pane argv that IS `podman run …` wrapping
// the provider argv verbatim — asserted against podmanx.RunArgv over the
// expected RunSpec, so mounts (worktree/bare/agent-dir/home/runtime),
// --userns=keep-id, --network=pasta, the seeded limits, the tools-image
// mount, -w, and the env split are all pinned in one exact comparison. The
// tmux -e env carries ONLY the secret forward (LAB_TOKEN); the prlimit cap
// is retired for the pane (NoNofileCap).
func TestStart_containerRunner_podmanPaneArgv(t *testing.T) {
	f := newFixture(t)
	f.enableContainer(t)

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	name := "proj~20260608-1530"
	sess, live := f.runner.Session(name)
	if !live {
		t.Fatal("session not live after container Start")
	}
	if !sess.NoNofileCap {
		t.Error("container Start did not pass WithoutNofileCap — the prlimit cap would target the podman client")
	}

	wt := filepath.Join(f.worktreeRoot, "proj-20260608-1530")
	// The container env: LAB_URL rewritten to the mounted socket, HOME
	// re-anchored at the container-side mount, PATH appended (tools bin
	// first, ADR-0051); LAB_TOKEN travels by NAME only, plus TERM.
	wantEnv := []string{
		"LAB_URL=unix:///var/lib/lab-test/agent/agent.sock",
		"HOME=" + podmanx.Home,
		"PATH=" + podmanx.PATH,
	}
	wantForward := []string{"LAB_TOKEN", "TERM"}
	wantArgv := podmanx.RunArgv(podmanx.RunSpec{
		Bin:         testPodmanBin,
		Name:        podmanx.ContainerName(name),
		Image:       testDevImage,
		ToolsImage:  testToolsImage,
		WorktreeDir: wt,
		BareDir:     f.bare(),
		AgentDir:    "/var/lib/lab-test/agent",
		HomeDir:     f.homes.HomePath(run.ID),
		RuntimeDir:  f.homes.RuntimePath(run.ID),
		// The seeded settings defaults (no repo overrides on the fixture).
		Memory:     "8g",
		Pids:       4096,
		Nofile:     16384,
		Env:        wantEnv,
		ForwardEnv: wantForward,
		// The provider argv rides verbatim at the tail — including the
		// injected per-run --settings flag (ADR-0020).
		Argv: f.wantSpawnArgv(name, run.Model, run.Effort, "", run.ID),
	})
	if !slices.Equal(sess.Argv, wantArgv) {
		t.Errorf("container pane argv =\n  %q\nwant\n  %q", sess.Argv, wantArgv)
	}

	// tmux -e carries ONLY the secret forward's value: LAB_TOKEN=…, nothing
	// else — no HOME, no GIT_*, no LAB_URL (those are podman's to deliver).
	if len(sess.ExtraEnv) != 1 || !strings.HasPrefix(sess.ExtraEnv[0], "LAB_TOKEN=lab_run_") {
		t.Errorf("container tmux env = %q, want exactly [LAB_TOKEN=lab_run_…]", sess.ExtraEnv)
	}
	// And the token value appears in NO argv element.
	token := strings.TrimPrefix(sess.ExtraEnv[0], "LAB_TOKEN=")
	for _, a := range sess.Argv {
		if strings.Contains(a, token) {
			t.Errorf("run token value leaked into the pane argv element %q", a)
		}
	}
	if sess.Dir != wt {
		t.Errorf("container pane cwd = %q, want the worktree %q", sess.Dir, wt)
	}
}

// Container limits resolve repo-override ?? settings-row: an explicit repo
// override wins; with none, freshly-written settings rows (not just the
// seeded defaults) are honored.
func TestStart_containerRunner_limitResolution(t *testing.T) {
	sub := func(t *testing.T, prep func(f *fixture), memory string, pids, nofile int) {
		f := newFixture(t)
		f.enableContainer(t)
		prep(f)
		if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		sess, live := f.runner.Session("proj~20260608-1530")
		if !live {
			t.Fatal("session not live")
		}
		argv := strings.Join(sess.Argv, " ")
		for _, want := range []string{
			"--memory " + memory,
			fmt.Sprintf("--pids-limit %d", pids),
			fmt.Sprintf("--ulimit nofile=%d:%d", nofile, nofile),
		} {
			if !strings.Contains(argv, want) {
				t.Errorf("pane argv missing %q:\n  %s", want, argv)
			}
		}
	}

	t.Run("repo overrides win", func(t *testing.T) {
		sub(t, func(f *fixture) {
			mem, pids, nofile := "2g", 99, 123
			if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
				ContainerMemory: store.Set(&mem),
				ContainerPids:   store.Set(&pids),
				ContainerNofile: store.Set(&nofile),
			}); err != nil {
				t.Fatalf("UpdateRepoSettings: %v", err)
			}
		}, "2g", 99, 123)
	})

	t.Run("settings rows are the fallback", func(t *testing.T) {
		sub(t, func(f *fixture) {
			for key, val := range map[string]string{
				store.SettingContainerMemory: "16g",
				store.SettingContainerPids:   "512",
				store.SettingContainerNofile: "2048",
			} {
				if err := f.st.SetSetting(t.Context(), key, val); err != nil {
					t.Fatalf("SetSetting(%s): %v", key, err)
				}
			}
		}, "16g", 512, 2048)
	})
}

// Every container refusal fires BEFORE the claim: no worktree, no branch, no
// run row, no per-run tree, no session — an AFK spec refused this way parks
// nothing. Each carries its actionable message as a *BadRequestError (the
// documented 400 mapping — see refuseContainerSpawn).
func TestStart_containerRefusals(t *testing.T) {
	cases := []struct {
		name    string
		wire    func(f *fixture)
		wantMsg string
	}{
		{
			name:    "unconfigured server",
			wire:    func(f *fixture) { f.svc.containerPreflight = nil },
			wantMsg: "container runner not configured on this server",
		},
		{
			name: "preflight pending",
			wire: func(f *fixture) {
				f.svc.containerPreflight = func() (podmanx.Result, bool) { return podmanx.Result{}, false }
			},
			wantMsg: "container preflight has not finished",
		},
		{
			name: "preflight failed",
			wire: func(f *fixture) {
				f.svc.containerPreflight = func() (podmanx.Result, bool) {
					return podmanx.Result{Failures: []podmanx.Failure{
						{Check: podmanx.CheckPasta, Detail: "pasta not found on PATH", Hint: "install passt (provides pasta)"},
					}}, true
				}
			},
			// The full actionable multi-failure message surfaces verbatim.
			wantMsg: "pasta not found on PATH (install passt (provides pasta))",
		},
		{
			name: "missing tools image for provider",
			wire: func(f *fixture) {
				f.svc.containerToolsImages = map[string]string{}
			},
			wantMsg: "no agent-tools image configured for provider claude-code — set --container-tools-image claude-code=<ref>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.enableContainer(t)
			tc.wire(f)

			_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
			if err == nil {
				t.Fatal("Start succeeded, want a container refusal")
			}
			var bad *BadRequestError
			if !errors.As(err, &bad) {
				t.Errorf("refusal error type = %T (%v), want *BadRequestError (the 400 mapping)", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal = %q, want it to contain %q", err, tc.wantMsg)
			}

			// Refused BEFORE the claim: nothing was created anywhere.
			if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
				t.Error("refused container spawn created a worktree")
			}
			if f.branchExists("lab/20260608-1530") {
				t.Error("refused container spawn created a branch")
			}
			if _, err := f.st.RunBySession(t.Context(), "proj~20260608-1530"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("RunBySession after refusal: %v, want ErrNotFound (no run row)", err)
			}
			if _, live := f.runner.Session("proj~20260608-1530"); live {
				t.Error("refused container spawn left a session")
			}
			if dirExists(f.instancesDir) {
				t.Error("refused container spawn materialized a per-run tree")
			}
		})
	}
}

// Stop of a container-mode run follows the tmux kill with the `podman rm`
// backstop, addressed by the deterministic container name; a host-mode stop
// on an unwired server runs no podman at all (the existing Stop tests cover
// its unchanged behavior — here we only pin that nothing blows up).
func TestStop_containerBackstop(t *testing.T) {
	t.Run("container run: rm with the derived name", func(t *testing.T) {
		f := newFixture(t)
		rec := f.enableContainer(t)

		run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := f.svc.Stop(t.Context(), run.SessionName); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		want := []string{testPodmanBin, "rm", "--force", "--ignore", "--time", "5", podmanx.ContainerName(run.SessionName)}
		calls := rec.recorded()
		if len(calls) != 1 || !slices.Equal(calls[0], want) {
			t.Errorf("podman calls = %q, want exactly [%q]", calls, want)
		}
	})

	t.Run("host run, wiring absent: no podman", func(t *testing.T) {
		f := newFixture(t)
		run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := f.svc.Stop(t.Context(), run.SessionName); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
}

// A container spawn whose tmux Start errors while the session went live (the
// post-spawn recheck race) rolls back like any other — and the rollback's
// session kill carries the same rm backstop.
func TestLaunch_containerRollbackRemovesContainer(t *testing.T) {
	f := newFixture(t)
	rec := f.enableContainer(t)
	name := "proj~20260608-1530"
	f.runner.FailStartLive(name, errors.New("ctx cancelled during recheck"))

	if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID}); err == nil {
		t.Fatal("Start succeeded despite injected spawn failure")
	}
	want := []string{testPodmanBin, "rm", "--force", "--ignore", "--time", "5", podmanx.ContainerName(name)}
	calls := rec.recorded()
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Errorf("rollback podman calls = %q, want exactly [%q]", calls, want)
	}
	if _, live := f.runner.Session(name); live {
		t.Error("rollback left the session live")
	}
}

// The dual-runner seam leaves host mode byte-identical: provider argv (no
// podman), full spawn env via tmux -e (HOME and all), prlimit cap kept.
func TestStart_hostRunner_unchangedByContainerSeam(t *testing.T) {
	f := newFixture(t)

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatal("session not live")
	}
	if sess.NoNofileCap {
		t.Error("host spawn passed WithoutNofileCap — the prlimit cap must stay for host panes")
	}
	if sess.Argv[0] != "claude" {
		t.Errorf("host pane argv starts with %q, want the provider CLI", sess.Argv[0])
	}
	if got := envValue(sess.ExtraEnv, "HOME"); got != f.homes.HomePath(run.ID) {
		t.Errorf("host tmux env HOME = %q, want the per-run home %q", got, f.homes.HomePath(run.ID))
	}
}

// containerEnv's exhaustive contract (pure — see the function doc for the
// secret/non-secret rule it implements).
func TestContainerEnv(t *testing.T) {
	const hostHome = "/state/instances/run_1/home"
	const sockURL = "unix:///state/agent/agent.sock"

	spawnEnv := []string{
		// credEnv: path-valued (the secret is in the materialized files
		// inside the mounted runtime dir) — passes verbatim.
		"GIT_SSH_COMMAND=ssh -i /state/instances/run_1/runtime/cred.run_1.key -o IdentitiesOnly=yes",
		"GIT_ASKPASS=/state/instances/run_1/runtime/cred.run_1.askpass",
		// LAB_URL: replaced with the mounted socket — a TCP agent URL is
		// unreachable through pasta's no-host-route netns.
		"LAB_URL=http://127.0.0.1:8080",
		// LAB_TOKEN: the one secret VALUE — name-only forward.
		"LAB_TOKEN=lab_run_secret",
		// HOME + the provider's home-derived pin: rewritten to the
		// container-side mount.
		"HOME=" + hostHome,
		"CLAUDE_CONFIG_DIR=" + hostHome + "/.claude",
		// Author identity: public, passes verbatim.
		"GIT_AUTHOR_NAME=Dominik",
	}
	env, forward := containerEnv(spawnEnv, hostHome, sockURL)

	wantEnv := []string{
		"GIT_SSH_COMMAND=ssh -i /state/instances/run_1/runtime/cred.run_1.key -o IdentitiesOnly=yes",
		"GIT_ASKPASS=/state/instances/run_1/runtime/cred.run_1.askpass",
		"LAB_URL=" + sockURL,
		"HOME=" + podmanx.Home,
		"CLAUDE_CONFIG_DIR=" + podmanx.Home + "/.claude",
		"GIT_AUTHOR_NAME=Dominik",
		"PATH=" + podmanx.PATH,
	}
	if !slices.Equal(env, wantEnv) {
		t.Errorf("env =\n  %q\nwant\n  %q", env, wantEnv)
	}
	wantForward := []string{"LAB_TOKEN", "TERM"}
	if !slices.Equal(forward, wantForward) {
		t.Errorf("forward = %q, want %q", forward, wantForward)
	}
	// The token VALUE must be nowhere in env.
	for _, kv := range env {
		if strings.Contains(kv, "lab_run_secret") {
			t.Errorf("secret token value leaked into env entry %q", kv)
		}
	}

	// Empty spawn env still yields the PATH pin and the TERM forward.
	env, forward = containerEnv(nil, hostHome, sockURL)
	if !slices.Equal(env, []string{"PATH=" + podmanx.PATH}) {
		t.Errorf("env over empty spawnEnv = %q, want exactly the PATH pin", env)
	}
	if !slices.Equal(forward, []string{"TERM"}) {
		t.Errorf("forward over empty spawnEnv = %q, want [TERM]", forward)
	}
}

// secretForwardEnv extracts exactly the forwarded names' K=V entries — the
// tmux -e payload of a container spawn.
func TestSecretForwardEnv(t *testing.T) {
	spawnEnv := []string{
		"GIT_ASKPASS=/runtime/askpass",
		"LAB_TOKEN=lab_run_secret",
		"HOME=/state/home",
	}
	got := secretForwardEnv(spawnEnv, []string{"LAB_TOKEN", "TERM"})
	if want := []string{"LAB_TOKEN=lab_run_secret"}; !slices.Equal(got, want) {
		t.Errorf("secretForwardEnv = %q, want %q (TERM has no spawnEnv entry; nothing else forwards)", got, want)
	}
	if got := secretForwardEnv(spawnEnv, nil); got != nil {
		t.Errorf("secretForwardEnv with no forwards = %q, want nil", got)
	}
}
