package podmanx

// TestContainerPane_integration drives a REAL podman through the exact
// mechanics production wires up for a containerized run (issue #205,
// internal/instance/launch.go): RunArgv's rendered argv AS a tmux pane
// command (the same composition launch.go builds — RunArgv(spec) fed
// straight into SessionRunner.Start), the mount inventory proven from the
// host side (a write under WorktreeDir and under RuntimeDir must reappear at
// the identical host path), a secret traveling by NAME-ONLY --env forward
// (ForwardEnv) rather than ever appearing in the podman argv, and the
// SIGHUP+--rm / RemoveContainer teardown race (design's stated backstop,
// #205).
//
// It is env-gated behind LAB_TEST_PODMAN=1 (in addition to podman actually
// being on PATH) because, unlike every other test in this package, it is not
// hermetic: it needs an image podman can actually pull (the first run costs a
// real registry round-trip) and a host where ROOTLESS podman genuinely works
// — a subuid/subgid range for the user, cgroup v2, a reachable systemd user
// manager, and pasta — none of which the lab dev sandbox has (no podman at
// all, confirmed absent from the nix devshell) and none of which default CI
// is assumed to provide either. The pane argv now always pins
// --cgroup-manager=systemd (ADR-0060); on a normal dev machine that resolves
// against the developer's OWN user manager — a real login session provides
// exactly what production provisions for the lab user via lingering — so the
// container lands in a libpod-*.scope under user@<uid>.service with no
// lab-specific setup. podmanx.Preflight (preflight.go) is the production
// check for exactly that host readiness (including a real spawn probe); this
// test does not re-verify Preflight, only that RunArgv's argv, once handed
// to a real tmux pane on a host that clears Preflight, actually produces a
// running, mount-correct, cleanly-reapable container.
//
// Run it explicitly on such a host with:
//
//	LAB_TEST_PODMAN=1 nix develop -c go test ./internal/podmanx/ -run TestContainerPane_integration -v
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// podmanIntegrationImage returns the image TestContainerPane_integration
// pulls and runs: LAB_TEST_PODMAN_IMAGE when set, else a small,
// widely-mirrored default that needs no registry auth. It is also used,
// unmodified, as the RunSpec.ToolsImage — see the dodge documented at that
// call site.
func podmanIntegrationImage() string {
	if img := os.Getenv("LAB_TEST_PODMAN_IMAGE"); img != "" {
		return img
	}
	return "docker.io/library/alpine:latest"
}

// TestContainerPane_integrationGate is the tiny always-running unit test for
// the env-parse helper the gated test above builds on: it exercises the
// default-vs-override logic with no podman, tmux, or LAB_TEST_PODMAN
// involved, so a typo in the parsing (as opposed to the environment it reads)
// still gets caught on every run, not just the opted-in one.
func TestContainerPane_integrationGate(t *testing.T) {
	t.Setenv("LAB_TEST_PODMAN_IMAGE", "")
	if got, want := podmanIntegrationImage(), "docker.io/library/alpine:latest"; got != want {
		t.Errorf("podmanIntegrationImage() with unset env = %q, want default %q", got, want)
	}
	t.Setenv("LAB_TEST_PODMAN_IMAGE", "example.test/custom:tag")
	if got, want := podmanIntegrationImage(), "example.test/custom:tag"; got != want {
		t.Errorf("podmanIntegrationImage() with override = %q, want %q", got, want)
	}
}

// waitForFileContent polls path until it has non-empty content, failing t if
// timeout elapses first. A generous timeout is needed here (unlike tmuxx's
// own ~5s helper): the first `podman run` on an image not yet in local
// storage blocks on a real pull before the proof-writing script even starts.
func waitForFileContent(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s never got content within %s", path, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForFileExists polls path until it exists (regardless of content —
// the spool marker is a `touch`, so it is always empty), failing t if
// timeout elapses first.
func waitForFileExists(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s never appeared within %s", path, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// podmanNames runs `podman ps [--all] --filter name=<name> --format
// {{.Names}}` and returns the exact-match lines — podman's name filter is a
// substring/regex match, not an anchor (ListRunContainers's own comment), so
// the exact-match check has to happen client-side here too.
func podmanNames(t *testing.T, all bool, name string) []string {
	t.Helper()
	args := []string{"ps", "--filter", "name=" + name, "--format", "{{.Names}}"}
	if all {
		args = []string{"ps", "--all", "--filter", "name=" + name, "--format", "{{.Names}}"}
	}
	out, err := exec.Command("podman", args...).Output()
	if err != nil {
		t.Fatalf("podman %s: %v", strings.Join(args, " "), err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

func TestContainerPane_integration(t *testing.T) {
	if os.Getenv("LAB_TEST_PODMAN") != "1" {
		t.Skip("LAB_TEST_PODMAN=1 not set: skipping the real-podman container-runner integration test (needs a pullable image and a rootless-podman-capable host — see the package doc comment above)")
	}
	testutil.RequireTool(t, "podman")
	testutil.RequireTool(t, "tmux")
	ctx := t.Context()

	// podman present doesn't mean podman USABLE here (e.g. rootless podman
	// with no configured subuid range fails far less legibly than a clean
	// skip); `podman info` is the cheapest real probe. A Rootless=false
	// answer is still a green light to proceed — logged, not gated on — this
	// test only needs A working podman, rootless or not.
	rootless, err := exec.CommandContext(ctx, "podman", "info", "--format", "{{.Host.Security.Rootless}}").Output()
	if err != nil {
		t.Skipf("podman present but not usable here (`podman info` failed): %v", err)
	}
	t.Logf("podman info: Host.Security.Rootless=%s", strings.TrimSpace(string(rootless)))

	image := podmanIntegrationImage()

	// The mount inventory (RunSpec doc comment): none of these need any
	// lab-specific contents — this test exercises the mount/lifecycle
	// mechanics RunArgv renders, not the instance-home or worktree provider
	// layers above it.
	worktreeDir := t.TempDir()
	bareDir := t.TempDir()
	agentDir := t.TempDir()
	homeDir := t.TempDir()
	runtimeDir := t.TempDir()

	proofPath := filepath.Join(worktreeDir, "proof")
	spoolPath := filepath.Join(runtimeDir, "spool")

	// session is the tmux session name RunArgv's container Name derives
	// from, exactly like launch.go's podmanx.ContainerName(name) call —
	// "~" is production's own session-name separator (ContainerName's doc
	// comment), and the pid suffix keeps repeated local runs from ever
	// colliding on a name a prior crashed run's teardown failed to clear.
	session := fmt.Sprintf("itest~%s-%d", t.Name(), os.Getpid())
	name := ContainerName(session)

	const envMarkerName = "VAR"
	const envMarkerValue = "podmanx-itest-marker"
	const forwardVarName = "ITEST_SECRET"
	secretValue := fmt.Sprintf("podmanx-itest-secret-%d", os.Getpid())

	// The script: write the two env values (one plain --env, one name-only
	// forward) into the worktree-mounted proof file, touch a marker in the
	// runtime-mounted dir, then idle so the assertions below have a live
	// container to inspect. Single-quoted paths match the existing tmuxx
	// integration harness's own sh -c convention (internal/tmuxx/integration_test.go).
	script := fmt.Sprintf(
		`echo "$%s:$%s" > '%s'; touch '%s'; exec sleep 300`,
		envMarkerName, forwardVarName, proofPath, spoolPath,
	)

	spec := RunSpec{
		Bin:  "podman",
		Name: name,
		// ToolsImage: RunArgv always emits the `--mount type=image` flag
		// (there is no way to render RunSpec without one), but this test has
		// no real agent-tools image to point at and doesn't need one — `sh`
		// never reads ToolsDst. The dodge: point ToolsImage at the SAME dev
		// image. Mounting an image at /opt/lab is harmless for a plain `sh`
		// pane, and podman already has the ref pulled for the container image
		// itself, so this costs no extra pull.
		Image:       image,
		ToolsImage:  image,
		WorktreeDir: worktreeDir,
		BareDir:     bareDir,
		AgentDir:    agentDir,
		HomeDir:     homeDir,
		RuntimeDir:  runtimeDir,
		Memory:      "256m",
		Pids:        128,
		Nofile:      1024,
		Env:         []string{envMarkerName + "=" + envMarkerValue},
		ForwardEnv:  []string{forwardVarName},
		Argv:        []string{"sh", "-c", script},
	}
	paneArgv := RunArgv(spec)

	// Private tmux socket, mirroring internal/tmuxx/integration_test.go's
	// testTmux: a fresh server per test run, never the developer's own.
	socket := fmt.Sprintf("lab-itest-podman-%d", os.Getpid())
	tm := tmuxx.New("tmux", tmuxx.WithSocket(socket))

	// t.Cleanup, not deferred: a mid-test t.Fatal still has to leave neither
	// the tmux session nor the container behind. Order is immaterial —
	// RemoveContainer force-removes independently of tmux, and both calls
	// are idempotent — but context.Background() is deliberate: t.Context()
	// is already canceled by the time Cleanup funcs run (its own doc
	// comment), so using it here would make these calls fail to even start.
	t.Cleanup(func() {
		_ = tm.Stop(context.Background(), session)
	})
	t.Cleanup(func() {
		_ = RemoveContainer(context.Background(), ExecRunner(), spec.Bin, name)
	})
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	// WithoutNofileCap: the inner pane command is the podman CLIENT, not the
	// agent — exactly launch.go's container-mode Start call (its own doc
	// comment: capping the client would aim at the wrong process; the
	// container's own --ulimit nofile, set above, governs the agent).
	if err := tm.Start(ctx, session, worktreeDir, paneArgv, []string{forwardVarName + "=" + secretValue}, tmuxx.WithoutNofileCap()); err != nil {
		t.Fatalf("tmux Start: %v", err)
	}

	// Generous wait: first run may still be pulling image.
	gotProof := waitForFileContent(t, proofPath, 120*time.Second)
	if want := envMarkerValue + ":" + secretValue + "\n"; gotProof != want {
		t.Errorf("proof file content = %q, want %q (rw worktree mount + --env + name-only ForwardEnv)", gotProof, want)
	}
	waitForFileExists(t, spoolPath, 15*time.Second)

	if names := podmanNames(t, false, name); !slices.Contains(names, name) {
		t.Errorf("podman ps --filter name=%s = %v; expected it to list the running container", name, names)
	}

	// Teardown: tmux kill-session SIGHUPs the attached podman client, which
	// sig-proxies into the container and --rm reaps it (RunArgv's doc
	// comment on --rm/-it). Stop first, then give the reap its own window
	// before reaching for the RemoveContainer backstop, so a leak on this
	// path is reported rather than silently papered over.
	if err := tm.Stop(ctx, session); err != nil {
		t.Fatalf("tmux Stop: %v", err)
	}

	reaped := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !slices.Contains(podmanNames(t, false, name), name) {
			reaped = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !reaped {
		t.Errorf("container %s still listed in `podman ps` ~15s after tmux Stop: the SIGHUP+--rm self-reap path leaked; RemoveContainer is only meant as the backstop for a CLI that ignores SIGHUP", name)
	}

	// Unconditional and idempotent either way (RemoveContainer's own doc
	// comment): the real backstop path when the reap leaked, a no-op
	// (--ignore) when it didn't.
	if err := RemoveContainer(ctx, ExecRunner(), spec.Bin, name); err != nil {
		t.Errorf("RemoveContainer: %v", err)
	}
	if names := podmanNames(t, true, name); slices.Contains(names, name) {
		t.Errorf("podman ps --all still lists %s after RemoveContainer; expected it fully removed", name)
	}
}
