package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Sessions tests shell out to a real tmux. They use a long sleep in place of
// claude so they don't depend on Anthropic auth or a working claude binary.
// They skip if tmux is unreachable on PATH.

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH — skipping integration test")
	}
}

func requirePrlimit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Skip("prlimit not on PATH — skipping nofile-cap integration test")
	}
}

func TestSessions_lifecycle(t *testing.T) {
	requireTmux(t)
	name := "lab-test-lifecycle"
	// sh -c, not bare `sleep`: Start appends the resolved --effort/--model flags,
	// which a bare `sleep` would reject (sh -c ignores the trailing args).
	s := NewSessions("tmux", []string{"sh", "-c", "sleep 600"})
	t.Cleanup(func() { _ = s.Stop(name) })

	if err := s.Start(name, t.TempDir()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ok, err := s.IsRunning(name)
	if err != nil || !ok {
		t.Fatalf("after Start expected running; ok=%v err=%v", ok, err)
	}

	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Contains(names, name) {
		t.Errorf("List() = %v; expected to contain %q", names, name)
	}

	if err := s.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ok, err = s.IsRunning(name)
	if err != nil || ok {
		t.Fatalf("after Stop expected stopped; ok=%v err=%v", ok, err)
	}
}

// A project directory whose name contains a "." must still produce a fully
// manageable session. tmux rewrites "." (and ":") to "_" in a session name at
// creation (session_check_name) AND reads a bare "." in a target as the
// window.pane separator — so a name like "foo.bar" gets stored as "foo_bar" yet
// every has-session / capture-pane / send-keys looks it up as "foo.bar" and
// fails with "can't find pane: bar". lab avoids this by never emitting "." from
// sessionName; this drives the session name through that sanitiser and asserts
// the start→lookup→stop round-trip the scan path depends on. Against a
// dot-preserving sessionName the test fails at Start with "exited immediately"
// (the post-spawn IsRunning re-check can't find the rewritten name).
func TestSessions_dottedProjectNameRoundTrips(t *testing.T) {
	requireTmux(t)
	name := sessionName("foo.bar")
	// sh -c so the appended --effort/--model flags (Start now resolves them) are
	// tolerated; a bare `sleep` would exit immediately on them.
	s := NewSessions("tmux", []string{"sh", "-c", "sleep 600"})
	t.Cleanup(func() { _ = s.Stop(name) })

	if err := s.Start(name, t.TempDir()); err != nil {
		t.Fatalf("Start(%q): %v", name, err)
	}
	ok, err := s.IsRunning(name)
	if err != nil || !ok {
		t.Fatalf("after Start expected running; ok=%v err=%v", ok, err)
	}
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Contains(names, name) {
		t.Errorf("List() = %v; expected to contain %q (from sessionName(%q))", names, name, "foo.bar")
	}
	if err := s.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if ok, _ := s.IsRunning(name); ok {
		t.Errorf("after Stop expected stopped")
	}
}

func TestSessions_startIsIdempotent(t *testing.T) {
	requireTmux(t)
	name := "lab-test-idempotent-start"
	// sh -c so the appended --effort/--model flags are tolerated (see lifecycle test).
	s := NewSessions("tmux", []string{"sh", "-c", "sleep 600"})
	t.Cleanup(func() { _ = s.Stop(name) })

	if err := s.Start(name, t.TempDir()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := s.Start(name, t.TempDir()); err != nil {
		t.Fatalf("second Start (should no-op): %v", err)
	}
}

func TestSessions_stopIsIdempotent(t *testing.T) {
	requireTmux(t)
	s := NewSessions("tmux", []string{"sleep", "600"})
	if err := s.Stop("lab-test-never-started"); err != nil {
		t.Errorf("Stop on missing session should no-op; got %v", err)
	}
}

func TestSessions_quickFailIsSurfaced(t *testing.T) {
	requireTmux(t)
	name := "lab-test-quickfail"
	// `false` exits 1 immediately — the post-spawn check should catch it.
	s := NewSessions("tmux", []string{"false"})
	t.Cleanup(func() { _ = s.Stop(name) })

	err := s.Start(name, t.TempDir())
	if err == nil {
		t.Fatalf("expected Start to surface quick-fail; got nil")
	}
	if !strings.Contains(err.Error(), "exited immediately") {
		t.Errorf("expected 'exited immediately' in error; got %q", err)
	}
}

func TestExtractOAuthURL(t *testing.T) {
	// The real authorize link from `claude auth login` (claude 2.1.150): it lives
	// on claude.com under a /cai/ path prefix — not claude.ai/oauth. The encoded
	// redirect_uri embeds "%2Foauth%2Fcode%2Fcallback", which must NOT be mistaken
	// for the real "/oauth/" segment, so this case also guards greedy matching.
	const realLine = "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=3paXVTV6FPSlOHej-t8CPB3Azy7demaQ-wTe13tPw6E&code_challenge_method=S256&state=84xH4rqCmCJzD1NFDMGF8YMkafUHjFOIhzfLXzd8ERQ"
	const realURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=3paXVTV6FPSlOHej-t8CPB3Azy7demaQ-wTe13tPw6E&code_challenge_method=S256&state=84xH4rqCmCJzD1NFDMGF8YMkafUHjFOIhzfLXzd8ERQ"
	for _, tc := range []struct{ in, want string }{
		{realLine, realURL},
		{"https://claude.ai/oauth/authorize?code=abc#state=xyz", "https://claude.ai/oauth/authorize?code=abc#state=xyz"},
		{"Use the url below to sign in https://claude.com/cai/oauth/authorize?a=b.", "https://claude.com/cai/oauth/authorize?a=b"},
		{"wraps in (https://claude.ai/oauth/foo)", "https://claude.ai/oauth/foo"},
		{"https://claude.ai/code/session_abc", ""}, // /code is not an oauth link
		{"no url here", ""},
	} {
		if got := extractOAuthURL(tc.in); got != tc.want {
			t.Errorf("extractOAuthURL(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestSessions_captureOAuthURLFromPane(t *testing.T) {
	requireTmux(t)
	name := "lab-test-oauth-url"
	// Stand-in for `claude auth login`: prints the authorize URL the way the real
	// flow does (claude.com under a /cai/ prefix), then idles so capture-pane
	// finds it in the buffer.
	s := NewSessions("tmux", []string{"sh", "-c", "echo 'visit: https://claude.com/cai/oauth/authorize?code=true&state=abc'; sleep 600"})
	t.Cleanup(func() { _ = s.Stop(name) })

	if err := s.Start(name, t.TempDir()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := s.CaptureOAuthURL(name, 3*time.Second)
	want := "https://claude.com/cai/oauth/authorize?code=true&state=abc"
	if got != want {
		t.Errorf("CaptureOAuthURL(%q) = %q; want %q", name, got, want)
	}
}

func TestSessions_sendKeysDeliversLiteralLine(t *testing.T) {
	requireTmux(t)
	name := "lab-test-sendkeys"
	dir := t.TempDir()
	out := filepath.Join(dir, "got.txt")
	// Read exactly one line, write it verbatim, then idle. (No "%s" in the argv:
	// StartCommand substitutes the session name for any "%s".)
	s := NewSessions("tmux", []string{"sh", "-c", "IFS= read -r line; echo \"$line\" > '" + out + "'; sleep 600"})
	t.Cleanup(func() { _ = s.Stop(name) })

	if err := s.Start(name, dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Spaces, the literal word "Enter", and code#state base64url chars must all
	// survive — the -l flag is what stops "Enter"/the space being read as keys.
	code := "tok-_123 Enter foo#state=bar"
	if err := s.SendKeys(name, code); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	var got string
	for i := 0; i < 30; i++ {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			got = strings.TrimRight(string(b), "\n")
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got != code {
		t.Errorf("delivered line = %q; want %q", got, code)
	}
}

// newSessionArgs is pure (no tmux), so the cap wrapping is asserted directly on
// the argv: prlimit prefix present and "%s" substituted when a cap is set, bare
// substituted argv when it is 0.
func TestSessions_newSessionArgsNofileCap(t *testing.T) {
	// The base argv no longer carries --model/--effort: baseStartArgv appends them
	// fresh from spawnConfig (here unwired, so the documented defaults), so the cap
	// wrapping is asserted around the substituted argv + those resolved flags.
	argv := []string{"claude", "--remote-control", "%s"}

	capped := NewSessions("tmux", argv)
	capped.prlimitBin = "prlimit"
	capped.nofile = 4096
	got := capped.newSessionArgs("proj-x", "/work/x", capped.baseStartArgv())
	want := []string{
		"new-session", "-d", "-s", "proj-x", "-c", "/work/x",
		"prlimit", "--nofile=4096:4096", "--",
		"claude", "--remote-control", "proj-x",
		"--effort", defaultSpawnEffort, "--model", defaultSpawnModel,
	}
	if !slices.Equal(got, want) {
		t.Errorf("capped newSessionArgs =\n  %q\nwant\n  %q", got, want)
	}

	// Zero cap (the default) spawns bare — no wrapper, just the substituted argv
	// plus the resolved default model/effort flags.
	bare := NewSessions("tmux", argv)
	got = bare.newSessionArgs("proj-x", "/work/x", bare.baseStartArgv())
	want = []string{
		"new-session", "-d", "-s", "proj-x", "-c", "/work/x",
		"claude", "--remote-control", "proj-x",
		"--effort", defaultSpawnEffort, "--model", defaultSpawnModel,
	}
	if !slices.Equal(got, want) {
		t.Errorf("bare newSessionArgs =\n  %q\nwant\n  %q", got, want)
	}
}

// baseStartArgv reads the model + effort FRESH from the injected spawnConfig at
// each call (spawn time), not at construction — so a change to the persisted
// global setting governs the next spawn with no restart, and the flags land after
// the base argv (before any caller-appended prompt). A nil accessor falls back to
// the documented defaults.
func TestSessions_baseStartArgvResolvesModelEffortDynamically(t *testing.T) {
	s := NewSessions("tmux", []string{"claude", "--remote-control", "%s"})

	// Unwired: the documented defaults.
	want := []string{"claude", "--remote-control", "%s", "--effort", defaultSpawnEffort, "--model", defaultSpawnModel}
	if got := s.baseStartArgv(); !slices.Equal(got, want) {
		t.Errorf("nil spawnConfig baseStartArgv = %q; want %q", got, want)
	}

	// Wired to a mutable cell: a later change is reflected on the NEXT call, proving
	// the read is at spawn time, not construction.
	model, effort := "sonnet", "high"
	s.spawnConfig = func() (string, string) { return model, effort }
	want = []string{"claude", "--remote-control", "%s", "--effort", "high", "--model", "sonnet"}
	if got := s.baseStartArgv(); !slices.Equal(got, want) {
		t.Errorf("wired baseStartArgv = %q; want %q", got, want)
	}
	model, effort = "fable", "low"
	want = []string{"claude", "--remote-control", "%s", "--effort", "low", "--model", "fable"}
	if got := s.baseStartArgv(); !slices.Equal(got, want) {
		t.Errorf("baseStartArgv after change = %q; want %q (must re-read at spawn time)", got, want)
	}

	// Each call returns a fresh slice, so a caller appending (an AFK seed prompt)
	// can't mutate the shared startCmd's backing array.
	a := s.baseStartArgv()
	_ = append(a, "seed prompt")
	if got := s.baseStartArgv(); !slices.Equal(got, want) {
		t.Errorf("baseStartArgv mutated by a caller's append: %q", got)
	}
}

// nofileStandIn returns an argv that records the pane's own soft and hard
// RLIMIT_NOFILE (one per line) into out, then idles — a stand-in for an agent
// that lets the test read back the limits the spawn actually applied.
func nofileStandIn(out string) []string {
	return []string{"sh", "-c", "{ ulimit -Sn; ulimit -Hn; } > '" + out + "'; sleep 600"}
}

// readLimits polls out (written by nofileStandIn) for the two integers the pane
// recorded, returning soft and hard RLIMIT_NOFILE.
func readLimits(t *testing.T, out string) (soft, hard int) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if b, err := os.ReadFile(out); err == nil {
			if f := strings.Fields(string(b)); len(f) >= 2 {
				s, errS := strconv.Atoi(f[0])
				h, errH := strconv.Atoi(f[1])
				if errS == nil && errH == nil {
					return s, h
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("limits file %s never reported two integers", out)
	return 0, 0
}

// The cap is set on the inner pane command, so it must survive tmux's
// new-session -d double-fork daemonization: the spawned agent itself reports
// soft+hard NOFILE equal to the configured cap. A small cap is fine —
// propagation is value-independent.
func TestSessions_nofileCapPropagatesThroughDaemonization(t *testing.T) {
	requireTmux(t)
	requirePrlimit(t)
	const cap = 256
	dir := t.TempDir()
	out := filepath.Join(dir, "limits.txt")
	s := NewSessions("tmux", nofileStandIn(out))
	s.prlimitBin = "prlimit"
	s.nofile = cap
	name := "lab-test-nofile-cap"
	t.Cleanup(func() { _ = s.Stop(name) })

	if err := s.Start(name, dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if soft, hard := readLimits(t, out); soft != cap || hard != cap {
		t.Errorf("spawned session NOFILE soft=%d hard=%d; want both %d", soft, hard, cap)
	}
}

// Two capped sessions on the same shared tmux server: the second attaches to the
// server the first started, so this proves the cap rides the per-pane inner
// command and isn't bypassed by reusing a running server.
func TestSessions_nofileCapBindsEachSessionOnSharedServer(t *testing.T) {
	requireTmux(t)
	requirePrlimit(t)
	const cap = 256
	dir := t.TempDir()
	out1 := filepath.Join(dir, "l1.txt")
	out2 := filepath.Join(dir, "l2.txt")
	mk := func(out string) *Sessions {
		s := NewSessions("tmux", nofileStandIn(out))
		s.prlimitBin = "prlimit"
		s.nofile = cap
		return s
	}
	s1, s2 := mk(out1), mk(out2)
	name1, name2 := "lab-test-nofile-share-a", "lab-test-nofile-share-b"
	t.Cleanup(func() { _ = s1.Stop(name1); _ = s2.Stop(name2) })

	if err := s1.Start(name1, dir); err != nil {
		t.Fatalf("Start %s: %v", name1, err)
	}
	if err := s2.Start(name2, dir); err != nil {
		t.Fatalf("Start %s: %v", name2, err)
	}
	// Both names live under one server — confirms the shared-server premise the
	// limit assertions below then prove the cap survives.
	names, err := s1.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Contains(names, name1) || !slices.Contains(names, name2) {
		t.Fatalf("expected both sessions on one server; List() = %v", names)
	}
	for _, tc := range []struct{ name, out string }{{name1, out1}, {name2, out2}} {
		if soft, hard := readLimits(t, tc.out); soft != cap || hard != cap {
			t.Errorf("%s NOFILE soft=%d hard=%d; want both %d", tc.name, soft, hard, cap)
		}
	}
}
