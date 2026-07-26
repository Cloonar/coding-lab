package providercli

// Unit pins for the LoginRunner decorator (issue #206): non-login sessions
// pass through byte-identical, a login Start renders exactly the podman
// argv TestLoginArgvGolden pins (against the fake SessionRunner, so no
// podman ever spawns), every refusal wraps provider.ErrLoginUnavailable
// with the actionable text, and Stop runs the rm backstop plus the scratch
// wipe regardless of how the tmux kill went.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// recordingCmdRunner is the podmanx.CmdRunner fake (the
// instance/container_test.go pattern): records every invocation, answers
// scripted output keyed by the space-joined command line ("" default) and an
// optional scripted error under the same key (nil default, so an unscripted
// command still succeeds — EnsureImage's exists probe needs no setup).
type recordingCmdRunner struct {
	mu     sync.Mutex
	calls  [][]string
	script map[string]string
	errs   map[string]error
}

func newRecordingCmdRunner() *recordingCmdRunner {
	return &recordingCmdRunner{script: map[string]string{}, errs: map[string]error{}}
}

func (r *recordingCmdRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	key := strings.Join(argv, " ")
	return []byte(r.script[key]), r.errs[key]
}

// reset drops the recorded calls, isolating a later phase's podman calls
// (Stop's rm) from an earlier one's (Start's EnsureImage probe).
func (r *recordingCmdRunner) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

func (r *recordingCmdRunner) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// The canonical container config the tests wire up — the same values
// instance/container_test.go pins for the run path.
const (
	testProviderID = "claude-code"
	testPodmanBin  = "podman"
	testDevImage   = "docker.io/library/debian:stable-slim"
	testToolsImage = "git.cloonar.com/cloonar/agent-tools@sha256:deadbeef"
)

// okPreflight returns a Gate-style Preflight closure with a published green
// verdict.
func okPreflight() func() (podmanx.Result, bool) {
	var g podmanx.Gate
	g.Set(podmanx.Result{Version: "5.0.0"})
	return g.Result
}

// fixedLimits stands in for the global container_* settings rows.
func fixedLimits(context.Context) (string, int, int, error) { return "8g", 4096, 16384, nil }

// testConfig builds the canonical Config over t.TempDir paths: a static
// master-store spec (claude-code's shape) and a fresh logins root.
func testConfig(t *testing.T, rec *recordingCmdRunner) Config {
	t.Helper()
	hostDir := filepath.Join(t.TempDir(), "master", ".claude")
	return Config{
		ProviderID: testProviderID,
		PodmanBin:  testPodmanBin,
		Run:        rec.run,
		Preflight:  okPreflight(),
		Image:      testDevImage,
		ToolsImage: testToolsImage,
		Spec: func() provider.MasterStoreSpec {
			return provider.MasterStoreSpec{EnvVar: "CLAUDE_CONFIG_DIR", HostDir: hostDir, HomeSubdir: ".claude"}
		},
		LoginHomeRoot: filepath.Join(t.TempDir(), "logins"),
		Limits:        fixedLimits,
		Logger:        slog.New(slog.DiscardHandler),
	}
}

// assertArgv fails with the first differing element — podmanx's golden
// helper, restated: a DeepEqual message on a 30-element argv is unreadable.
func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q\n got: %q\nwant: %q", i, got[i], want[i], got, want)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("argv length = %d, want %d\n got: %q\nwant: %q", len(got), len(want), got, want)
	}
}

// Non-login sessions pass through byte-identical — argv, dir, env, and the
// caller's own StartOpts — and no podman call is ever made for them.
func TestLoginRunnerPassThrough(t *testing.T) {
	rec := newRecordingCmdRunner()
	fake := tmuxx.NewFake()
	lr := NewLoginRunner(fake, testConfig(t, rec))
	ctx := t.Context()

	argv := []string{"claude", "--model", "opus"}
	env := []string{"LAB_TOKEN=tok"}
	if err := lr.Start(ctx, "myrepo~afk-1", "/wt", argv, env, tmuxx.WithoutNofileCap()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s, ok := fake.Session("myrepo~afk-1")
	if !ok {
		t.Fatal("session not started on the inner runner")
	}
	assertArgv(t, s.Argv, argv)
	assertArgv(t, s.ExtraEnv, env)
	if s.Dir != "/wt" {
		t.Errorf("dir = %q, want /wt", s.Dir)
	}
	if !s.NoNofileCap {
		t.Error("caller's WithoutNofileCap opt was not preserved")
	}

	// A second non-login session WITHOUT the opt proves the decorator does
	// not add it on the pass-through path.
	if err := lr.Start(ctx, "myrepo~afk-2", "/wt", argv, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s, _ := fake.Session("myrepo~afk-2"); s.NoNofileCap {
		t.Error("pass-through Start added WithoutNofileCap")
	}

	if err := lr.SendKeys(ctx, "myrepo~afk-1", "hello", true); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if sent := fake.Sent("myrepo~afk-1"); len(sent) != 1 || sent[0].Text != "hello" || !sent[0].Enter {
		t.Errorf("SendKeys not delegated: %+v", sent)
	}
	if err := lr.Stop(ctx, "myrepo~afk-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if live, _ := lr.IsRunning(ctx, "myrepo~afk-1"); live {
		t.Error("session still live after delegated Stop")
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("non-login sessions made podman calls: %q", calls)
	}
}

// The login Start golden: the fake records exactly the podman argv of
// podmanx's TestLoginArgvGolden shape, WithoutNofileCap rides the Start, a
// stale scratch home is wiped fresh, and the master dir is created.
func TestLoginRunnerStartGolden(t *testing.T) {
	rec := newRecordingCmdRunner()
	cfg := testConfig(t, rec)
	fake := tmuxx.NewFake()
	lr := NewLoginRunner(fake, cfg)

	sess := tmuxx.LoginSessionName(testProviderID) // "lab-login-claude-code"
	hostDir := cfg.Spec().HostDir
	scratch := filepath.Join(cfg.LoginHomeRoot, sess)

	// A crashed prior attempt's leftover must not leak into this one.
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(scratch, "stale.json")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := lr.Start(t.Context(), sess, "/panecwd",
		[]string{"claude", "auth", "login", "--claudeai"}, []string{"EXTRA=1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	want := []string{
		testPodmanBin, "--cgroup-manager=systemd", "run", "--rm", "-it",
		"--name", "labrun-lab-login-claude-code-3e99da",
		"--userns=keep-id",
		"--network=pasta",
		"--memory", "8g",
		"--pids-limit", "4096",
		"--ulimit", "nofile=16384:16384",
		"--mount", "type=image,src=" + testToolsImage + ",dst=/opt/lab",
		"-v", scratch + ":/home/agent",
		"-v", hostDir + ":/home/agent/.claude",
		"-w", "/home/agent",
		"--env", "PATH=" + podmanx.PATH,
		"--env", "HOME=/home/agent",
		"--env", "CLAUDE_CONFIG_DIR=/home/agent/.claude",
		"--env", "TERM",
		testDevImage,
		"claude", "auth", "login", "--claudeai",
	}
	s, ok := fake.Session(sess)
	if !ok {
		t.Fatal("login session not started on the inner runner")
	}
	assertArgv(t, s.Argv, want)
	if !s.NoNofileCap {
		t.Error("login Start must retire the prlimit wrapper (WithoutNofileCap)")
	}
	if s.Dir != "/panecwd" {
		t.Errorf("dir = %q, want the caller's dir passed through", s.Dir)
	}
	assertArgv(t, s.ExtraEnv, []string{"EXTRA=1"})

	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("scratch home missing after Start: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale scratch file survived the per-attempt wipe: %v", err)
	}
	if _, err := os.Stat(hostDir); err != nil {
		t.Errorf("master store dir not created: %v", err)
	}
	// EnsureImage probed the dev image (a local hit — no pull).
	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("podman calls = %q, want only the exists probe", calls)
	}
	assertArgv(t, calls[0], []string{testPodmanBin, "image", "exists", testDevImage})
}

// Every refusal wraps provider.ErrLoginUnavailable, carries the actionable
// text, and never reaches the inner runner.
func TestLoginRunnerStartRefusals(t *testing.T) {
	sess := tmuxx.LoginSessionName(testProviderID)
	cases := []struct {
		name     string
		mutate   func(*Config, *recordingCmdRunner)
		wantText string
	}{
		{
			name: "preflight pending",
			mutate: func(c *Config, _ *recordingCmdRunner) {
				var g podmanx.Gate // nothing published yet
				c.Preflight = g.Result
			},
			wantText: "container preflight has not finished — retry in a moment",
		},
		{
			name: "preflight red",
			mutate: func(c *Config, _ *recordingCmdRunner) {
				var g podmanx.Gate
				g.Set(podmanx.Result{Failures: []podmanx.Failure{
					{Check: podmanx.CheckPasta, Detail: "pasta not found on PATH", Hint: "install passt (provides pasta)"},
				}})
				c.Preflight = g.Result
			},
			wantText: "container preflight failed: pasta: pasta not found on PATH (install passt (provides pasta))",
		},
		{
			name:     "no tools image",
			mutate:   func(c *Config, _ *recordingCmdRunner) { c.ToolsImage = "" },
			wantText: "no agent-tools image configured for provider claude-code — set --container-tools-image claude-code=<ref>",
		},
		{
			name:     "no dev image",
			mutate:   func(c *Config, _ *recordingCmdRunner) { c.Image = "" },
			wantText: "no dev image configured — set --container-image",
		},
		{
			name: "dev image pull failure",
			mutate: func(_ *Config, rec *recordingCmdRunner) {
				rec.errs["podman image exists "+testDevImage] = errors.New("exit status 1")
				rec.errs["podman pull "+testDevImage] = errors.New("manifest unknown")
			},
			wantText: "registry access",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingCmdRunner()
			cfg := testConfig(t, rec)
			tc.mutate(&cfg, rec)
			fake := tmuxx.NewFake()
			lr := NewLoginRunner(fake, cfg)

			err := lr.Start(t.Context(), sess, "/panecwd", []string{"claude", "auth", "login"}, nil)
			if !errors.Is(err, provider.ErrLoginUnavailable) {
				t.Fatalf("err = %v, want it to wrap provider.ErrLoginUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err %q does not carry the actionable text %q", err, tc.wantText)
			}
			if _, live := fake.Session(sess); live {
				t.Error("inner Start was reached despite the refusal")
			}
		})
	}
}

// startLogin is the Stop tests' fixture: a successfully started login
// session, with the recorder cleared so only Stop's podman calls remain.
func startLogin(t *testing.T, lr *LoginRunner, rec *recordingCmdRunner, sess string) {
	t.Helper()
	if err := lr.Start(t.Context(), sess, "/panecwd", []string{"claude", "auth", "login"}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec.reset()
}

// Stop: tmux kill, then ALWAYS the rm backstop and the scratch wipe.
func TestLoginRunnerStop(t *testing.T) {
	rec := newRecordingCmdRunner()
	cfg := testConfig(t, rec)
	fake := tmuxx.NewFake()
	lr := NewLoginRunner(fake, cfg)
	sess := tmuxx.LoginSessionName(testProviderID)
	scratch := filepath.Join(cfg.LoginHomeRoot, sess)
	startLogin(t, lr, rec, sess)

	if err := lr.Stop(t.Context(), sess); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if live, _ := fake.IsRunning(t.Context(), sess); live {
		t.Error("inner session still live after Stop")
	}
	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("podman calls on Stop = %q, want exactly the rm backstop", calls)
	}
	assertArgv(t, calls[0], []string{testPodmanBin, "rm", "--force", "--ignore", "--time", "5", "labrun-lab-login-claude-code-3e99da"})
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch home survived Stop: %v", err)
	}
}

// An rm failure only warns — Stop still returns nil and still wipes the
// scratch home (the boot orphan sweep is the backstop's backstop).
func TestLoginRunnerStopRmFailureOnlyWarns(t *testing.T) {
	rec := newRecordingCmdRunner()
	cfg := testConfig(t, rec)
	fake := tmuxx.NewFake()
	lr := NewLoginRunner(fake, cfg)
	sess := tmuxx.LoginSessionName(testProviderID)
	scratch := filepath.Join(cfg.LoginHomeRoot, sess)
	startLogin(t, lr, rec, sess)
	rec.errs["podman rm --force --ignore --time 5 labrun-lab-login-claude-code-3e99da"] = errors.New("boom")

	if err := lr.Stop(t.Context(), sess); err != nil {
		t.Fatalf("Stop must not fail on an rm error, got: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch home survived Stop: %v", err)
	}
}

// A failing tmux kill is returned — but the backstop and wipe run anyway:
// a half-stopped login must not keep its container or scratch state.
func TestLoginRunnerStopReturnsInnerError(t *testing.T) {
	rec := newRecordingCmdRunner()
	cfg := testConfig(t, rec)
	fake := tmuxx.NewFake()
	lr := NewLoginRunner(fake, cfg)
	sess := tmuxx.LoginSessionName(testProviderID)
	scratch := filepath.Join(cfg.LoginHomeRoot, sess)
	startLogin(t, lr, rec, sess)
	boom := errors.New("tmux kill failed")
	fake.FailStop(sess, boom)

	if err := lr.Stop(t.Context(), sess); !errors.Is(err, boom) {
		t.Fatalf("Stop error = %v, want the inner runner's %v", err, boom)
	}
	calls := rec.recorded()
	if len(calls) != 1 || calls[0][1] != "rm" {
		t.Errorf("rm backstop did not run despite the inner Stop error: %q", calls)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch home survived Stop: %v", err)
	}
}

// A non-login Stop is pure delegation: no rm, no wipe.
func TestLoginRunnerStopNonLogin(t *testing.T) {
	rec := newRecordingCmdRunner()
	fake := tmuxx.NewFake()
	fake.AddLive("myrepo~afk-1")
	lr := NewLoginRunner(fake, testConfig(t, rec))

	if err := lr.Stop(t.Context(), "myrepo~afk-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("non-login Stop made podman calls: %q", calls)
	}
}
