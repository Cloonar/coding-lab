package providercli

// Unit pins for the container-mode CLIRunner (issue #206): the golden podman
// argv of podmanx's TestCLIArgvGolden shape (pins-then-invocation-env
// order), the Dir/env master-store translation (exact-or-path-prefix, never
// substring), the preflight wait-not-fail, the 125-127 vehicle-vs-CLI exit
// classification the CLIRunner contract hangs on, the post-hoc MaxStdout
// guard, and unique labrun- names. All against a fake execStreams — no
// podman ever spawns.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// recordingExec is the execStreams fake: records each argv, answers one
// scripted (stdout, stderr, err) triple for every call.
type recordingExec struct {
	calls  [][]string
	stdout []byte
	stderr []byte
	err    error
}

func (r *recordingExec) run(_ context.Context, argv []string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	return r.stdout, r.stderr, r.err
}

// testCLI builds a ContainerCLI over the canonical config with the fake
// transport injected and a fast preflight poll.
func testCLI(t *testing.T, rec *recordingCmdRunner, ex *recordingExec) (*ContainerCLI, Config) {
	t.Helper()
	cfg := testConfig(t, rec)
	c := NewContainerCLI(cfg)
	c.execStreams = ex.run
	c.preflightPoll = time.Millisecond
	return c, cfg
}

// cliNameRe pins the ephemeral container-name shape: the labrun- namespace
// (boot-sweep reapable), the cli- marker, the provider, 8 random bytes hex,
// and ContainerName's collision-guard suffix.
var cliNameRe = regexp.MustCompile(`^labrun-cli-claude-code-[0-9a-f]{16}-[0-9a-f]{6}$`)

// The golden CLI argv: NoTTY (no -it), no home bind — the store bind alone —
// and env pins first (PATH, HOME, the config-dir var) with the invocation's
// own entries rewritten onto StoreDst appended AFTER them, so under podman's
// later-wins rule the adapter's pin stays authoritative.
func TestContainerCLIGolden(t *testing.T) {
	ex := &recordingExec{stdout: []byte("ok\n")}
	rec := newRecordingCmdRunner()
	c, cfg := testCLI(t, rec, ex)
	hostDir := cfg.Spec().HostDir

	stdout, stderr, err := c.Run(t.Context(), provider.CLIInvocation{
		Argv: []string{"claude", "auth", "status", "--json"},
		Env:  []string{"CLAUDE_CONFIG_DIR=" + hostDir},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stdout) != "ok\n" || len(stderr) != 0 {
		t.Errorf("streams = %q, %q; want the transport's verbatim", stdout, stderr)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(ex.calls))
	}
	got := ex.calls[0]
	if len(got) < 6 || !cliNameRe.MatchString(got[5]) {
		t.Fatalf("container name %q does not match %v (argv: %q)", got[5], cliNameRe, got)
	}
	want := []string{
		testPodmanBin, "--cgroup-manager=systemd", "run", "--rm",
		"--name", got[5],
		"--userns=keep-id",
		"--network=pasta:--map-guest-addr,none",
		"--add-host", "host.containers.internal:127.0.0.1",
		"--add-host", "host.docker.internal:127.0.0.1",
		"--memory", "8g",
		"--pids-limit", "4096",
		"--ulimit", "nofile=16384:16384",
		"--mount", "type=image,src=" + testToolsImage + ",dst=/opt/lab",
		"-v", hostDir + ":/home/agent/.claude",
		"-w", "/home/agent",
		"--env", "PATH=" + podmanx.PATH,
		"--env", "HOME=/home/agent",
		"--env", "CLAUDE_CONFIG_DIR=/home/agent/.claude", // the pin
		"--env", "CLAUDE_CONFIG_DIR=/home/agent/.claude", // the adapter's own, rewritten, after the pin
		testDevImage,
		"claude", "auth", "status", "--json",
	}
	assertArgv(t, got, want)
	// EnsureImage probed the dev image before the run.
	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("podman transport calls = %q, want only the exists probe", calls)
	}
	assertArgv(t, calls[0], []string{testPodmanBin, "image", "exists", testDevImage})
}

// wAfter extracts the value following -w in argv.
func wAfter(t *testing.T, argv []string) string {
	t.Helper()
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-w" {
			return argv[i+1]
		}
	}
	t.Fatalf("no -w in argv %q", argv)
	return ""
}

// Dir translation: "" → the container home; the master store (exact or a
// subpath) → StoreDst; a sibling merely sharing the string prefix → the
// home fallback, never a bogus store path.
func TestContainerCLIDirTranslation(t *testing.T) {
	rec := newRecordingCmdRunner()
	ex := &recordingExec{}
	c, cfg := testCLI(t, rec, ex)
	hostDir := cfg.Spec().HostDir

	cases := []struct{ dir, want string }{
		{"", "/home/agent"},
		{hostDir, "/home/agent/.claude"},
		{hostDir + "/sub", "/home/agent/.claude/sub"},
		{hostDir + "2/x", "/home/agent"}, // string-prefix sibling: fallback, not rewrite
	}
	for _, tc := range cases {
		ex.calls = nil
		if _, _, err := c.Run(t.Context(), provider.CLIInvocation{Argv: []string{"claude"}, Dir: tc.dir}); err != nil {
			t.Fatalf("Run(Dir=%q): %v", tc.dir, err)
		}
		if got := wAfter(t, ex.calls[0]); got != tc.want {
			t.Errorf("Dir %q → -w %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// Env value rewrite is exact-or-path-prefix only: a sibling dir sharing the
// string prefix and a value containing the store mid-string stay untouched.
func TestContainerCLIEnvRewrite(t *testing.T) {
	rec := newRecordingCmdRunner()
	ex := &recordingExec{}
	c, cfg := testCLI(t, rec, ex)
	hostDir := cfg.Spec().HostDir

	if _, _, err := c.Run(t.Context(), provider.CLIInvocation{
		Argv: []string{"claude"},
		Env: []string{
			"A=" + hostDir,
			"B=" + hostDir + "/sub/f.json",
			"C=" + hostDir + "2/x",
			"D=see " + hostDir + " here",
			"E=plain",
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv := ex.calls[0]
	var envs []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--env" {
			envs = append(envs, argv[i+1])
		}
	}
	assertArgv(t, envs, []string{
		"PATH=" + podmanx.PATH,
		"HOME=/home/agent",
		"CLAUDE_CONFIG_DIR=/home/agent/.claude",
		"A=/home/agent/.claude",
		"B=/home/agent/.claude/sub/f.json",
		"C=" + hostDir + "2/x",
		"D=see " + hostDir + " here",
		"E=plain",
	})
}

// A pending preflight is waited out, not failed: the verdict landing after
// two polls lets the run proceed — the boot-time race (codex's
// construction-time probe, a spawn right after restart) resolves itself.
func TestContainerCLIPreflightWait(t *testing.T) {
	rec := newRecordingCmdRunner()
	ex := &recordingExec{}
	c, _ := testCLI(t, rec, ex)
	polls := 0
	c.cfg.Preflight = func() (podmanx.Result, bool) {
		polls++
		if polls >= 3 {
			return podmanx.Result{Version: "5.0.0"}, true
		}
		return podmanx.Result{}, false
	}
	if _, _, err := c.Run(t.Context(), provider.CLIInvocation{Argv: []string{"claude"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if polls < 3 {
		t.Errorf("preflight polled %d times, want at least 3 (two pending rounds)", polls)
	}
	if len(ex.calls) != 1 {
		t.Errorf("exec calls = %d, want the run to have proceeded", len(ex.calls))
	}
}

// The wait is bounded: past the ceiling the pending error surfaces (and it
// is NOT the login sentinel — CLI callers treat any error as could-not-run).
func TestContainerCLIPreflightCeiling(t *testing.T) {
	rec := newRecordingCmdRunner()
	ex := &recordingExec{}
	c, _ := testCLI(t, rec, ex)
	c.preflightCeiling = 10 * time.Millisecond
	var g podmanx.Gate // never publishes
	c.cfg.Preflight = g.Result

	_, _, err := c.Run(t.Context(), provider.CLIInvocation{Argv: []string{"claude"}})
	if err == nil || !strings.Contains(err.Error(), "container preflight has not finished") {
		t.Fatalf("err = %v, want the pending-preflight text", err)
	}
	if errors.Is(err, provider.ErrLoginUnavailable) {
		t.Error("CLI errors must not carry the login sentinel")
	}
	if len(ex.calls) != 0 {
		t.Error("exec ran despite the pending preflight")
	}
}

// The wait is also ctx-bounded: a cancelled context returns its error
// instead of spinning toward the ceiling.
func TestContainerCLIPreflightCtxCancel(t *testing.T) {
	rec := newRecordingCmdRunner()
	ex := &recordingExec{}
	c, _ := testCLI(t, rec, ex)
	var g podmanx.Gate
	c.cfg.Preflight = g.Result
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, _, err := c.Run(ctx, provider.CLIInvocation{Argv: []string{"claude"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A red verdict and missing image knobs error with the same actionable
// texts as the login refusals — minus the sentinel wrap.
func TestContainerCLIStructuralErrors(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*ContainerCLI)
		wantText string
	}{
		{
			name: "red verdict",
			mutate: func(c *ContainerCLI) {
				var g podmanx.Gate
				g.Set(podmanx.Result{Failures: []podmanx.Failure{
					{Check: podmanx.CheckPodman, Detail: "podman not found on PATH", Hint: "install podman >= 4"},
				}})
				c.cfg.Preflight = g.Result
			},
			wantText: "container preflight failed",
		},
		{
			name:     "no tools image",
			mutate:   func(c *ContainerCLI) { c.cfg.ToolsImage = "" },
			wantText: "no agent-tools image configured for provider claude-code — set --container-tools-image claude-code=<ref>",
		},
		{
			name:     "no dev image",
			mutate:   func(c *ContainerCLI) { c.cfg.Image = "" },
			wantText: "no dev image configured — set --container-image",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingCmdRunner()
			ex := &recordingExec{}
			c, _ := testCLI(t, rec, ex)
			tc.mutate(c)

			_, _, err := c.Run(t.Context(), provider.CLIInvocation{Argv: []string{"claude"}})
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %v, want it to carry %q", err, tc.wantText)
			}
			if errors.Is(err, provider.ErrLoginUnavailable) {
				t.Error("CLI errors must not carry the login sentinel")
			}
			if len(ex.calls) != 0 {
				t.Error("exec ran despite the structural error")
			}
		})
	}
}

// realExitError fabricates a genuine *exec.ExitError with the given code by
// running one — the hostcli_test sh pattern; ProcessState cannot be built
// by hand.
func realExitError(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	testutil.RequireTool(t, "sh")
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("sh -c exit %d returned %v, want an *exec.ExitError", code, err)
	}
	return ee
}

// Exit classification, the load-bearing CLIRunner contract: 125-127 are
// podman's own failure and come back WRAPPED (never a direct ExitError — a
// missing image must not read as a logged-out verdict) with the stderr tail;
// any other exit passes through RAW; a clean exit is nil; a non-exit error
// passes through as-is.
func TestContainerCLIExitClassification(t *testing.T) {
	run := func(t *testing.T, ex *recordingExec) error {
		rec := newRecordingCmdRunner()
		c, _ := testCLI(t, rec, ex)
		_, _, err := c.Run(t.Context(), provider.CLIInvocation{Argv: []string{"claude"}})
		return err
	}

	for _, code := range []int{125, 126, 127} {
		t.Run(fmt.Sprintf("exit %d is podman's, wrapped", code), func(t *testing.T) {
			err := run(t, &recordingExec{err: realExitError(t, code), stderr: []byte("Error: manifest unknown\n")})
			if _, direct := err.(*exec.ExitError); direct {
				t.Fatalf("err = %v is a DIRECT *exec.ExitError; podman's own exit must be wrapped", err)
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("err = %v does not wrap the exit error", err)
			}
			for _, sub := range []string{fmt.Sprintf("podman run failed (exit %d)", code), "manifest unknown"} {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("err %q does not carry %q", err, sub)
				}
			}
		})
	}

	t.Run("exit 1 is the CLI's, raw", func(t *testing.T) {
		want := realExitError(t, 1)
		err := run(t, &recordingExec{err: want})
		if err != want { //nolint:errorlint // the DIRECT identity is the contract under test
			t.Fatalf("err = %v (%T), want the transport's *exec.ExitError untouched", err, err)
		}
	})

	t.Run("exit 0 is nil", func(t *testing.T) {
		if err := run(t, &recordingExec{stdout: []byte("out")}); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})

	t.Run("non-exit errors pass through as-is", func(t *testing.T) {
		boom := errors.New("podman binary missing")
		if err := run(t, &recordingExec{err: boom}); err != boom { //nolint:errorlint // identity, as above
			t.Fatalf("err = %v, want the transport error untouched", err)
		}
	})
}

// MaxStdout is a post-hoc cap: overflow returns the cap-naming error (a
// could-not-run, never a silent truncation), with the streams still handed
// back.
func TestContainerCLIMaxStdout(t *testing.T) {
	rec := newRecordingCmdRunner()
	ex := &recordingExec{stdout: []byte(strings.Repeat("x", 20))}
	c, _ := testCLI(t, rec, ex)

	stdout, _, err := c.Run(t.Context(), provider.CLIInvocation{Argv: []string{"claude"}, MaxStdout: 10})
	if err == nil || !strings.Contains(err.Error(), "stdout exceeds 10 bytes") {
		t.Fatalf("err = %v, want the cap-naming error", err)
	}
	if len(stdout) != 20 {
		t.Errorf("stdout len = %d, want the buffered bytes returned alongside", len(stdout))
	}

	// Under the cap: no error, full stdout.
	ex.stdout = []byte("short")
	if stdout, _, err := c.Run(t.Context(), provider.CLIInvocation{Argv: []string{"claude"}, MaxStdout: 10}); err != nil || string(stdout) != "short" {
		t.Errorf("under-cap Run = %q, %v; want the stdout and nil", stdout, err)
	}
}

// Every invocation mints a fresh labrun- name, so concurrent pokes never
// collide and a wedged container is boot-sweep reapable.
func TestContainerCLIUniqueNames(t *testing.T) {
	rec := newRecordingCmdRunner()
	ex := &recordingExec{}
	c, _ := testCLI(t, rec, ex)
	for range 2 {
		if _, _, err := c.Run(t.Context(), provider.CLIInvocation{Argv: []string{"claude"}}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	a, b := ex.calls[0][5], ex.calls[1][5]
	if a == b {
		t.Errorf("two invocations shared the container name %q", a)
	}
	for _, n := range []string{a, b} {
		if !cliNameRe.MatchString(n) {
			t.Errorf("name %q does not match %v", n, cliNameRe)
		}
	}
}

// An empty Argv is a programmer error, refused before any podman work —
// HostCLI's own guard, mirrored.
func TestContainerCLIEmptyArgv(t *testing.T) {
	rec := newRecordingCmdRunner()
	ex := &recordingExec{}
	c, _ := testCLI(t, rec, ex)
	if _, _, err := c.Run(t.Context(), provider.CLIInvocation{}); err == nil || !strings.Contains(err.Error(), "Argv is empty") {
		t.Fatalf("err = %v, want the empty-Argv guard", err)
	}
}
