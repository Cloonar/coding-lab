package providercli

// TestLoginRunner_integration and TestContainerCLI_integration drive REAL
// podman + tmux through the production composition (issue #206): a
// LoginRunner over a real tmuxx.Tmux on a private socket spawning the
// containerized login pane — the OAuth-style URL scraped through the
// runner's CapturePane, a write through the rw master-store bind appearing
// at the HOST path (the issue's core criterion), and Stop reaping both the
// container and the scratch home — and a ContainerCLI round-trip proving
// stream separation and the direct-ExitError contract against a real
// ephemeral container.
//
// Env-gated behind LAB_TEST_PODMAN=1 exactly like podmanx's integration
// test and for the same reason: these are not hermetic — they need a
// pullable image and a host where rootless podman genuinely works (subuid/
// subgid, cgroup v2 delegation, pasta), which neither the lab dev sandbox
// nor default CI provides. Run them explicitly on such a host with:
//
//	LAB_TEST_PODMAN=1 nix develop -c go test ./internal/providercli/ -run _integration -v
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

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// integrationImage returns the image the gated tests pull and run:
// LAB_TEST_PODMAN_IMAGE when set, else a small, widely-mirrored default
// needing no registry auth. Used for BOTH Image and ToolsImage — podmanx's
// documented dodge: `--mount type=image` accepts any image, and `sh` never
// reads /opt/lab, so the tools mount is harmless and costs no extra pull.
func integrationImage() string {
	if img := os.Getenv("LAB_TEST_PODMAN_IMAGE"); img != "" {
		return img
	}
	return "docker.io/library/alpine:latest"
}

// TestIntegrationImageGate is the always-running unit test for the
// env-parse helper above (podmanx's TestContainerPane_integrationGate
// pattern): a typo in the default-vs-override logic is caught on every run,
// not just the opted-in one.
func TestIntegrationImageGate(t *testing.T) {
	t.Setenv("LAB_TEST_PODMAN_IMAGE", "")
	if got, want := integrationImage(), "docker.io/library/alpine:latest"; got != want {
		t.Errorf("integrationImage() with unset env = %q, want default %q", got, want)
	}
	t.Setenv("LAB_TEST_PODMAN_IMAGE", "example.test/custom:tag")
	if got, want := integrationImage(), "example.test/custom:tag"; got != want {
		t.Errorf("integrationImage() with override = %q, want %q", got, want)
	}
}

// requirePodman gates on the env opt-in, the tool presence, and podman
// actually being usable (`podman info` — rootless podman with no subuid
// range fails far less legibly than a clean skip). Rootless=false is still
// a green light: logged, not gated on.
func requirePodman(t *testing.T) {
	t.Helper()
	if os.Getenv("LAB_TEST_PODMAN") != "1" {
		t.Skip("LAB_TEST_PODMAN=1 not set: skipping the real-podman provider-login integration test (needs a pullable image and a rootless-podman-capable host — see the file comment above)")
	}
	testutil.RequireTool(t, "podman")
	testutil.RequireTool(t, "tmux")
	rootless, err := exec.CommandContext(t.Context(), "podman", "info", "--format", "{{.Host.Security.Rootless}}").Output()
	if err != nil {
		t.Skipf("podman present but not usable here (`podman info` failed): %v", err)
	}
	t.Logf("podman info: Host.Security.Rootless=%s", strings.TrimSpace(string(rootless)))
}

// integrationConfig assembles the production-shaped Config over test dirs: a
// green preflight verdict, the shared test image for both refs, a master
// store the fake provider addresses via LAB_TEST_STORE_DIR, and small limits.
func integrationConfig(image, master, loginRoot string) Config {
	var g podmanx.Gate
	g.Set(podmanx.Result{})
	return Config{
		ProviderID: "itest",
		PodmanBin:  "podman",
		Preflight:  g.Result,
		Image:      image,
		ToolsImage: image,
		Spec: func() provider.MasterStoreSpec {
			return provider.MasterStoreSpec{EnvVar: "LAB_TEST_STORE_DIR", HostDir: master, HomeSubdir: ".fake"}
		},
		LoginHomeRoot: loginRoot,
		Limits: func(context.Context) (string, int, int, error) {
			return "256m", 128, 1024, nil
		},
	}
}

// podmanNames lists the exact-match `podman ps [--all]` names — podman's
// name filter is a substring match, so exactness is checked client-side
// (ListRunContainers's own caveat).
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
		if line = strings.TrimSpace(line); line == name {
			names = append(names, line)
		}
	}
	return names
}

func TestLoginRunner_integration(t *testing.T) {
	requirePodman(t)
	ctx := t.Context()

	image := integrationImage()
	master := t.TempDir()
	loginRoot := t.TempDir()
	cfg := integrationConfig(image, master, loginRoot)

	session := tmuxx.LoginSessionName("itest")
	name := podmanx.ContainerName(session)
	scratch := filepath.Join(loginRoot, session)

	// Private tmux socket — a fresh server per run, never the developer's.
	socket := fmt.Sprintf("lab-itest-providercli-%d", os.Getpid())
	lr := NewLoginRunner(tmuxx.New("tmux", tmuxx.WithSocket(socket)), cfg)

	// t.Cleanup, not defer: a mid-test t.Fatal must still leave nothing
	// behind, and t.Context() is already cancelled when these run.
	t.Cleanup(func() { _ = lr.Stop(context.Background(), session) })
	t.Cleanup(func() {
		_ = podmanx.RemoveContainer(context.Background(), podmanx.ExecRunner(), "podman", name)
	})
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	// The fake login CLI: print an OAuth-style URL (the scrape target),
	// prove the rw store bind by touching a marker THROUGH the pinned
	// config-dir var, then idle like a real TUI awaiting the browser flow.
	const url = "https://example.com/oauth/authorize?code=itest"
	argv := []string{"sh", "-c",
		`echo ` + url + `; touch "$LAB_TEST_STORE_DIR/marker"; sleep 30`}
	// EnsureImage inside Start pulls the image synchronously, so the pane's
	// own `podman run` never blocks on a pull and the polls below are quick.
	if err := lr.Start(ctx, session, master, argv, nil); err != nil {
		t.Fatalf("LoginRunner.Start: %v", err)
	}

	// The URL is scraped THROUGH the runner, as the adapters scrape it.
	deadline := time.Now().Add(60 * time.Second)
	for {
		pane, err := lr.CapturePane(ctx, session)
		if err == nil && strings.Contains(pane, url) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane never showed %q within 60s (last err %v, pane %q)", url, err, pane)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The core criterion: the container's write through the store mount
	// appears in the HOST master dir.
	marker := filepath.Join(master, "marker")
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker %s never appeared within 60s: the rw master-store bind did not reach the host", marker)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("scratch home %s missing during the attempt: %v", scratch, err)
	}

	// Stop reaps: the tmux SIGHUP + --rm path with the rm backstop behind
	// it, so `podman ps --all` must clear, and the scratch home is wiped.
	if err := lr.Stop(ctx, session); err != nil {
		t.Fatalf("LoginRunner.Stop: %v", err)
	}
	reapDeadline := time.Now().Add(15 * time.Second)
	for len(podmanNames(t, true, name)) > 0 {
		if time.Now().After(reapDeadline) {
			t.Fatalf("container %s still listed ~15s after Stop", name)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch home %s survived Stop: %v", scratch, err)
	}
}

func TestContainerCLI_integration(t *testing.T) {
	requirePodman(t)
	ctx := t.Context()

	image := integrationImage()
	cli := NewContainerCLI(integrationConfig(image, t.TempDir(), t.TempDir()))

	// Stream separation through a real ephemeral container. stdout is the
	// container's alone; stderr may carry podman's own warnings alongside
	// the script's line, so it is a contains-check.
	stdout, stderr, err := cli.Run(ctx, provider.CLIInvocation{
		Argv: []string{"sh", "-c", "echo out; echo err >&2"},
	})
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr)
	}
	if string(stdout) != "out\n" {
		t.Errorf("stdout = %q, want %q", stdout, "out\n")
	}
	if !strings.Contains(string(stderr), "err\n") {
		t.Errorf("stderr = %q, want it to carry %q", stderr, "err\n")
	}

	// The inner CLI's own exit passes through as a DIRECT *exec.ExitError —
	// the codex logged-out verdict's contract, held across the container.
	_, _, err = cli.Run(ctx, provider.CLIInvocation{Argv: []string{"sh", "-c", "exit 1"}})
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("err = %v (%T), want a DIRECT *exec.ExitError", err, err)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1", ee.ExitCode())
	}

	// No labrun- CLI containers linger: --rm reaped both ephemeral runs.
	out, psErr := exec.Command("podman", "ps", "--all", "--filter", "name=labrun-cli-itest-", "--format", "{{.Names}}").Output()
	if psErr != nil {
		t.Fatalf("podman ps: %v", psErr)
	}
	if names := strings.TrimSpace(string(out)); names != "" && slices.ContainsFunc(strings.Split(names, "\n"), func(n string) bool {
		return strings.HasPrefix(strings.TrimSpace(n), "labrun-cli-itest-")
	}) {
		t.Errorf("ephemeral CLI containers lingered after --rm: %q", names)
	}
}
