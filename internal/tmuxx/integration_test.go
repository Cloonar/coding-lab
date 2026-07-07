package tmuxx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// Integration tests run against a REAL tmux on a private socket
// (`tmux -L lab-test-<pid>-<n>`, design §11) so they never touch the
// developer's own server. Each test gets its own socket; cleanup kills
// that private server. A long sleep stands in for claude so nothing
// depends on Anthropic auth or a working claude binary. Tests skip when
// tmux (or prlimit, where needed) is not on PATH.

var socketSeq atomic.Int64

// testTmux returns a Tmux bound to a fresh private socket and registers
// a cleanup that kills the private server.
func testTmux(t *testing.T, opts ...Option) *Tmux {
	t.Helper()
	testutil.RequireTool(t, "tmux")
	socket := fmt.Sprintf("lab-test-%d-%d", os.Getpid(), socketSeq.Add(1))
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	return New("tmux", append([]Option{WithSocket(socket)}, opts...)...)
}

// waitForFile polls path until it has content, failing t after ~5s.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	for range 50 {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("file %s never got content", path)
	return ""
}

func TestTmux_lifecycle(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-lifecycle"

	if err := tm.Start(ctx, name, t.TempDir(), []string{"sleep", "600"}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ok, err := tm.IsRunning(ctx, name)
	if err != nil || !ok {
		t.Fatalf("after Start expected running; ok=%v err=%v", ok, err)
	}

	names, err := tm.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slices.Contains(names, name) {
		t.Errorf("List() = %v; expected to contain %q", names, name)
	}

	if err := tm.Stop(ctx, name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ok, err = tm.IsRunning(ctx, name)
	if err != nil || ok {
		t.Fatalf("after Stop expected stopped; ok=%v err=%v", ok, err)
	}
}

func TestTmux_startIsIdempotent(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-idempotent-start"

	if err := tm.Start(ctx, name, t.TempDir(), []string{"sleep", "600"}, nil); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := tm.Start(ctx, name, t.TempDir(), []string{"sleep", "600"}, nil); err != nil {
		t.Fatalf("second Start (should no-op): %v", err)
	}
}

func TestTmux_stopIsIdempotent(t *testing.T) {
	tm := testTmux(t)
	if err := tm.Stop(t.Context(), "lab-test-never-started"); err != nil {
		t.Errorf("Stop on missing session should no-op; got %v", err)
	}
}

func TestTmux_quickFailIsSurfaced(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-quickfail"

	// `false` exits 1 immediately — the 500 ms post-spawn re-check must
	// catch it and surface a loud error.
	err := tm.Start(ctx, name, t.TempDir(), []string{"false"}, nil)
	if err == nil {
		t.Fatalf("expected Start to surface quick-fail; got nil")
	}
	if !strings.Contains(err.Error(), "exited immediately") {
		t.Errorf("expected 'exited immediately' in error; got %q", err)
	}
}

// The login session is a session like any other to the runner — the
// exclusions live in callers (design §4d).
func TestTmux_loginSessionIsOrdinary(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)

	if err := tm.Start(ctx, LoginSession, t.TempDir(), []string{"sleep", "600"}, nil); err != nil {
		t.Fatalf("Start(%s): %v", LoginSession, err)
	}
	names, err := tm.List(ctx)
	if err != nil || !slices.Contains(names, LoginSession) {
		t.Fatalf("List = %v, %v; expected to contain %q", names, err, LoginSession)
	}
	if err := tm.Stop(ctx, LoginSession); err != nil {
		t.Fatalf("Stop(%s): %v", LoginSession, err)
	}
}

// SendKeys into a `cat > file` session: spaces, the literal word
// "Enter", `#`, `=`, and base64url chars must all be delivered verbatim
// as one line — the -l flag stops "Enter"/the space being read as key
// names, and the separate non-literal Enter submits the line.
func TestTmux_sendKeysDeliversLiteralLine(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-sendkeys"
	dir := t.TempDir()
	out := filepath.Join(dir, "got.txt")

	if err := tm.Start(ctx, name, dir, []string{"sh", "-c", "cat > '" + out + "'"}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	const code = "tok-_123 Enter foo#state=bar"
	if err := tm.SendKeys(ctx, name, code, true); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	got := strings.TrimRight(waitForFile(t, out), "\n")
	if got != code {
		t.Errorf("delivered line = %q; want %q", got, code)
	}
}

// enter=false must deliver the text without submitting the line: the
// pane's line discipline holds it until a later Enter arrives, at which
// point both fragments come through as one line.
func TestTmux_sendKeysWithoutEnterDoesNotSubmit(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-sendkeys-noenter"
	dir := t.TempDir()
	out := filepath.Join(dir, "got.txt")

	if err := tm.Start(ctx, name, dir, []string{"sh", "-c", "cat > '" + out + "'"}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tm.SendKeys(ctx, name, "first-half ", false); err != nil {
		t.Fatalf("SendKeys (no enter): %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
		t.Fatalf("line submitted without Enter: %q", b)
	}
	if err := tm.SendKeys(ctx, name, "second-half", true); err != nil {
		t.Fatalf("SendKeys (enter): %v", err)
	}
	got := strings.TrimRight(waitForFile(t, out), "\n")
	if want := "first-half second-half"; got != want {
		t.Errorf("delivered line = %q; want %q", got, want)
	}
}

// CapturePane round-trip: text printed at session start (an OAuth-URL
// stand-in) is recovered from the pane; a missing session errors.
func TestTmux_capturePaneRoundTrip(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-capture"
	const line = "visit: https://claude.com/cai/oauth/authorize?code=true&state=abc"

	if err := tm.Start(ctx, name, t.TempDir(), []string{"sh", "-c", "echo '" + line + "'; sleep 600"}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var pane string
	deadline := time.Now().Add(3 * time.Second)
	for {
		var err error
		pane, err = tm.CapturePane(ctx, name)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		if strings.Contains(pane, line) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(pane, line) {
		t.Errorf("pane capture missing %q; got %q", line, pane)
	}

	if _, err := tm.CapturePane(ctx, "lab-test-no-such-session"); err == nil {
		t.Errorf("CapturePane on missing session: want error")
	}
}

// nofileStandIn returns an argv that records the pane's own soft and
// hard RLIMIT_NOFILE (one per line) into out, then idles — a stand-in
// for an agent that lets the test read back the limits the spawn
// actually applied.
func nofileStandIn(out string) []string {
	return []string{"sh", "-c", "{ ulimit -Sn; ulimit -Hn; } > '" + out + "'; sleep 600"}
}

// readLimits parses the two integers written by nofileStandIn.
func readLimits(t *testing.T, out string) (soft, hard int) {
	t.Helper()
	f := strings.Fields(waitForFile(t, out))
	if len(f) < 2 {
		t.Fatalf("limits file %s reported %q; want two integers", out, f)
	}
	soft, errS := strconv.Atoi(f[0])
	hard, errH := strconv.Atoi(f[1])
	if errS != nil || errH != nil {
		t.Fatalf("limits file %s reported %q; want two integers", out, f)
	}
	return soft, hard
}

// The cap is set on the inner pane command, so it must survive tmux's
// new-session -d double-fork daemonization: the spawned command itself
// reports soft+hard NOFILE equal to the configured cap.
func TestTmux_nofileCapPropagatesThroughDaemonization(t *testing.T) {
	testutil.RequireTool(t, "prlimit")
	ctx := t.Context()
	const cap = 1024
	dir := t.TempDir()
	out := filepath.Join(dir, "limits.txt")
	tm := testTmux(t, WithNofileCap("prlimit", cap))
	const name = "lab-test-nofile-cap"

	if err := tm.Start(ctx, name, dir, nofileStandIn(out), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if soft, hard := readLimits(t, out); soft != cap || hard != cap {
		t.Errorf("spawned session NOFILE soft=%d hard=%d; want both %d", soft, hard, cap)
	}
}

// Two capped sessions on the same server: the second attaches to the
// server the first started, so this proves the cap rides the per-pane
// inner command and isn't bypassed by reusing a running server.
func TestTmux_nofileCapBindsEachSessionOnSharedServer(t *testing.T) {
	testutil.RequireTool(t, "prlimit")
	ctx := t.Context()
	const cap = 1024
	dir := t.TempDir()
	out1 := filepath.Join(dir, "l1.txt")
	out2 := filepath.Join(dir, "l2.txt")
	tm := testTmux(t, WithNofileCap("prlimit", cap))
	name1, name2 := "lab-test-nofile-share-a", "lab-test-nofile-share-b"

	if err := tm.Start(ctx, name1, dir, nofileStandIn(out1), nil); err != nil {
		t.Fatalf("Start %s: %v", name1, err)
	}
	if err := tm.Start(ctx, name2, dir, nofileStandIn(out2), nil); err != nil {
		t.Fatalf("Start %s: %v", name2, err)
	}
	// Both names live under one server — confirms the shared-server
	// premise the limit assertions below then prove the cap survives.
	names, err := tm.List(ctx)
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

// extraEnv entries reach the pane's process environment (the LAB_URL /
// LAB_TOKEN / git-credential injection point, design §7 port notes).
func TestTmux_extraEnvReachesPane(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-extra-env"
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")

	argv := []string{"sh", "-c", `printf '%s' "$LAB_TEST_TOKEN" > '` + out + `'; sleep 600`}
	if err := tm.Start(ctx, name, dir, argv, []string{"LAB_TEST_TOKEN=lab_run_tok123"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := waitForFile(t, out); got != "lab_run_tok123" {
		t.Errorf("pane saw LAB_TEST_TOKEN=%q; want lab_run_tok123", got)
	}
}

// A private socket whose server was never started: List means "no
// sessions", IsRunning means "absent" — neither is an error.
func TestTmux_serverAbsentMeansEmpty(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t) // fresh socket, no server ever started

	names, err := tm.List(ctx)
	if err != nil {
		t.Fatalf("List with no server: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("List with no server = %v; want empty", names)
	}
	ok, err := tm.IsRunning(ctx, "anything")
	if err != nil || ok {
		t.Errorf("IsRunning with no server = %v, %v; want false, nil", ok, err)
	}
}

// SendNamedKeys delivers a named Enter that submits a previously typed
// (unsubmitted) literal line — the general form of SendKeys' second call,
// used by the chat's dialog/interrupt recipes.
func TestTmux_sendNamedKeysSubmitsLine(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-namedkeys"
	dir := t.TempDir()
	out := filepath.Join(dir, "got.txt")

	if err := tm.Start(ctx, name, dir, []string{"sh", "-c", "cat > '" + out + "'"}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tm.SendKeys(ctx, name, "hello", false); err != nil {
		t.Fatalf("SendKeys (no enter): %v", err)
	}
	if err := tm.SendNamedKeys(ctx, name, "Enter"); err != nil {
		t.Fatalf("SendNamedKeys(Enter): %v", err)
	}
	if got := strings.TrimRight(waitForFile(t, out), "\n"); got != "hello" {
		t.Errorf("named Enter did not submit the line: got %q; want %q", got, "hello")
	}
}

// PasteText delivers multi-line text in one bracketed paste. The pane enables
// bracketed-paste mode (as claude's TUI does), so the §6 property under test
// is the real one: tmux frames the text in ESC[200~/ESC[201~, which is what
// makes embedded newlines insert lines instead of submitting turn-by-turn.
func TestTmux_pasteTextDeliversMultiline(t *testing.T) {
	ctx := t.Context()
	tm := testTmux(t)
	const name = "lab-test-paste"
	dir := t.TempDir()
	out := filepath.Join(dir, "got.txt")

	// printf enables mode 2004 on the pane tty; cat then records raw stdin —
	// paste framing included — to the file.
	if err := tm.Start(ctx, name, dir, []string{"sh", "-c", `printf '\033[?2004h'; cat > '` + out + `'`}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := tm.PasteText(ctx, name, "line one\nline two"); err != nil {
		t.Fatalf("PasteText: %v", err)
	}
	if err := tm.SendNamedKeys(ctx, name, "Enter"); err != nil {
		t.Fatalf("SendNamedKeys(Enter): %v", err)
	}
	got := waitForFile(t, out)
	if !strings.Contains(got, "\x1b[200~line one\nline two\x1b[201~") {
		t.Errorf("paste not bracketed: %q; want ESC[200~line one\\nline two ESC[201~", got)
	}
}
