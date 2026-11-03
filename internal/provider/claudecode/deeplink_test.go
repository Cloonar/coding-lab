package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRegistryEntry drops one <name>.json into dir the way claude's
// session registry does (the subset of fields lab reads).
func writeRegistryEntry(t *testing.T, dir, name string, e RegistryEntry) {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadPID returns the pid of a process that has already exited, for the
// stale-registry-file cases (claude only cleans its file on graceful
// exit).
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// ownDomain renders the pidDomain a 2.1.284 CLI running in THIS test
// process's pid namespace would write (compat §2:
// linux:<machine-id>:pid:[<inode>]). The machine-id half is deliberately
// made up: only the namespace token decides, because a container carries its
// own /etc/machine-id.
func ownDomain(t *testing.T) string {
	t.Helper()
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Skipf("/proc/self/ns/pid unreadable: %v", err)
	}
	return "linux:ffffffffffffffffffffffffffffffff:" + ns
}

// foreignDomain is a pidDomain naming a pid namespace that is not this test
// process's — what a container run's claude writes, seen from the host.
const foreignDomain = "linux:ea8e7833ea8e7833ea8e7833ea8e7833:pid:[1]"

// Transcribed v0 contract: registry_test.go TestBridgeURLForDir
// (complete, 8 subtests).
func TestBridgeURLForDir(t *testing.T) {
	alive := os.Getpid()

	t.Run("matches cwd and normalises cse_ to session_", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_abc123",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_abc123"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
		if got := bridgeURLForDir(dir, "/work/other"); got != "" {
			t.Errorf("bridgeURLForDir for unmatched cwd = %q; want empty", got)
		}
	})

	t.Run("registry's session_ form passes through unchanged", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "session_xyz",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_xyz"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
	})

	t.Run("no bridge session yet means no URL", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1,
		})
		if got := bridgeURLForDir(dir, "/work/a"); got != "" {
			t.Errorf("bridgeURLForDir before bridge connect = %q; want empty", got)
		}
	})

	t.Run("stale file of a dead process is ignored", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: deadPID(t), Cwd: "/work/a", StartedAt: 9, BridgeSessionID: "cse_stale", PidDomain: ownDomain(t),
		})
		writeRegistryEntry(t, dir, "2.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_live",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_live"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q (dead pid must lose)", got, want)
		}
	})

	t.Run("newest startedAt wins on a reused worktree", func(t *testing.T) {
		dir := t.TempDir()
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_old",
		})
		writeRegistryEntry(t, dir, "2.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 2, BridgeSessionID: "cse_new",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_new"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
	})

	t.Run("malformed file is skipped, valid sibling still found", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeRegistryEntry(t, dir, "1.json", RegistryEntry{
			PID: alive, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_ok",
		})
		if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_ok"; got != want {
			t.Errorf("bridgeURLForDir = %q; want %q", got, want)
		}
	})

	t.Run("missing registry dir is a quiet miss", func(t *testing.T) {
		if got := bridgeURLForDir(filepath.Join(t.TempDir(), "nope"), "/work/a"); got != "" {
			t.Errorf("bridgeURLForDir on missing dir = %q; want empty", got)
		}
	})
}

// Liveness is decided per entry by pidDomain (issue: nested runs read as an
// empty chat). A container run's claude writes its in-container pid — 2 under
// --init, 9 or so under the nested-containers entry program — which on the
// host is a kernel thread or nothing at all; probing it answers about a
// different process. So an entry in another pid namespace is live regardless
// of its pid, an entry in lab's own namespace is probed as before, and an
// entry without a parsable pidDomain (an older CLI) is live: absent means
// "cannot check", never "dead".
func TestEntryAlive_byPidDomain(t *testing.T) {
	self, dead := ownDomain(t), deadPID(t)
	ns, _ := pidNamespaceOf(self)
	cases := []struct {
		name   string
		e      RegistryEntry
		self   string
		alive  bool
		reason string
	}{
		{"own namespace, live pid", RegistryEntry{PID: os.Getpid(), PidDomain: self}, ns, true, "a running CLI on the host"},
		{"own namespace, dead pid", RegistryEntry{PID: dead, PidDomain: self}, ns, false, "a SIGKILLed host CLI's leftover must lose"},
		{"foreign namespace, dead host pid", RegistryEntry{PID: dead, PidDomain: foreignDomain}, ns, true, "a container pid means nothing on the host"},
		{"foreign namespace, pid absent on host", RegistryEntry{PID: 1 << 30, PidDomain: foreignDomain}, ns, true, "nested runs: pid 9 does not exist on the host"},
		{"foreign namespace, host machine-id", RegistryEntry{PID: dead, PidDomain: "linux:ffffffffffffffffffffffffffffffff:pid:[1]"}, ns, true, "only the namespace token decides, not the machine id"},
		{"no pidDomain, dead pid", RegistryEntry{PID: dead}, ns, true, "an older CLI writes no pidDomain: cannot check, not dead"},
		{"unparsable pidDomain, dead pid", RegistryEntry{PID: dead, PidDomain: "windows:whatever"}, ns, true, "unparsable is cannot check"},
		{"self unknown, dead pid", RegistryEntry{PID: dead, PidDomain: foreignDomain}, "", false, "/proc/self/ns/pid unreadable: probe every entry as before"},
		{"self unknown, live pid", RegistryEntry{PID: os.Getpid()}, "", true, "/proc/self/ns/pid unreadable: probe every entry as before"},
	}
	for _, tc := range cases {
		if got := entryAlive(tc.e, tc.self); got != tc.alive {
			t.Errorf("%s: entryAlive(%+v, %q) = %v; want %v (%s)", tc.name, tc.e, tc.self, got, tc.alive, tc.reason)
		}
	}
}

func TestPidNamespaceOf(t *testing.T) {
	cases := map[string]struct {
		ns string
		ok bool
	}{
		"linux:92c69de892c69de892c69de892c69de8:pid:[4026531836]": {"pid:[4026531836]", true},
		"linux:ea8e7833ea8e7833ea8e7833ea8e7833:pid:[4026533686]": {"pid:[4026533686]", true},
		"":                         {"", false},
		"linux:abc":                {"", false},
		"linux:abc:pid:[":          {"", false},
		"linux:abc:pid:4026531836": {"", false},
	}
	for in, want := range cases {
		ns, ok := pidNamespaceOf(in)
		if ns != want.ns || ok != want.ok {
			t.Errorf("pidNamespaceOf(%q) = (%q, %v); want (%q, %v)", in, ns, ok, want.ns, want.ok)
		}
	}
	// The resolved self token has the same spelling the CLI embeds, so the two
	// compare equal for a host-runner entry.
	if self := selfPidNamespace(); self != "" {
		if ns, ok := pidNamespaceOf("linux:0123:" + self); !ok || ns != self {
			t.Errorf("own token %q does not round-trip through pidNamespaceOf: (%q, %v)", self, ns, ok)
		}
	}
}

// The registry pass selects a foreign-namespace entry for the deep link
// whatever its pid, and two such entries resolve to the newest startedAt —
// the dead-CLI signal in a container is tmux liveness, not the pid.
func TestBridgeURLForDir_foreignNamespace(t *testing.T) {
	dir := t.TempDir()
	writeRegistryEntry(t, dir, "9.json", RegistryEntry{
		PID: 1 << 30, Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_older", PidDomain: foreignDomain,
	})
	writeRegistryEntry(t, dir, "12.json", RegistryEntry{
		PID: deadPID(t), Cwd: "/work/a", StartedAt: 2, BridgeSessionID: "cse_newer", PidDomain: foreignDomain,
	})
	if got, want := bridgeURLForDir(dir, "/work/a"), "https://claude.ai/code/session_newer"; got != want {
		t.Errorf("bridgeURLForDir = %q; want %q (foreign-namespace entries are live; newest startedAt wins)", got, want)
	}
}

// Transcribed v0 contract: the bridge connects seconds after spawn, so
// the capture must keep polling until the registry entry appears — a
// one-shot read at spawn time would always miss.
func TestCaptureBridgeURL_findsLateEntry(t *testing.T) {
	dir := t.TempDir()
	got := make(chan string, 1)
	go func() { got <- captureBridgeURL(context.Background(), dir, "/work/a", 5*time.Second) }()
	time.Sleep(300 * time.Millisecond)
	writeRegistryEntry(t, dir, "1.json", RegistryEntry{
		PID: os.Getpid(), Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_late",
	})
	if g, want := <-got, "https://claude.ai/code/session_late"; g != want {
		t.Errorf("captureBridgeURL = %q; want %q", g, want)
	}
}

func TestCaptureBridgeURL_timesOut(t *testing.T) {
	if got := captureBridgeURL(context.Background(), t.TempDir(), "/work/a", 300*time.Millisecond); got != "" {
		t.Errorf("captureBridgeURL with no entry = %q; want empty", got)
	}
}

func TestCaptureDeepLink_hit(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	// The session registry lives under the run's private HOME (issue #202).
	home := t.TempDir()
	registryDir := registryDirUnder(home)
	if err := os.MkdirAll(registryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRegistryEntry(t, registryDir, "1.json", RegistryEntry{
		PID: os.Getpid(), Cwd: "/work/a", StartedAt: 1, BridgeSessionID: "cse_abc",
	})
	got, err := p.CaptureDeepLink(context.Background(), "repo~x-1", "/work/a", home)
	if err != nil {
		t.Fatalf("CaptureDeepLink: %v", err)
	}
	if want := "https://claude.ai/code/session_abc"; got != want {
		t.Errorf("CaptureDeepLink = %q; want %q", got, want)
	}
}

// An empty home is a QUIET miss — a run with no per-run home has no instance
// registry, and lab never falls back to the master store (issue #202). No loud
// capture-miss warning fires (that is reserved for a real registry poll that
// timed out).
func TestCaptureDeepLink_emptyHomeQuietMiss(t *testing.T) {
	var logBuf bytes.Buffer
	p, _ := testProvider(t, newFakeRunner())
	p.log = slog.New(slog.NewTextHandler(&logBuf, nil))

	got, err := p.CaptureDeepLink(context.Background(), "repo~x-1", "/work/a", "")
	if err != nil || got != "" {
		t.Errorf("homeless capture = (%q, %v); want (\"\", nil)", got, err)
	}
	if strings.Contains(logBuf.String(), "deep-link capture missed") {
		t.Errorf("empty-home capture logged a capture-miss warning; want a quiet miss. log = %q", logBuf.String())
	}
}

// ADR-0017 / brief §11.2: a miss returns "" (the generic fallback is surfaced
// through FallbackOpen, never capture) AND logs loudly (v0 was silent).
func TestCaptureDeepLink_missReturnsEmptyAndWarnsLoudly(t *testing.T) {
	var logBuf bytes.Buffer
	run := newFakeRunner()
	p, _ := testProvider(t, run)
	p.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	p.bridgeTimeout = 100 * time.Millisecond

	got, err := p.CaptureDeepLink(context.Background(), "repo~x-1", "/work/nowhere", t.TempDir())
	if err != nil {
		t.Fatalf("CaptureDeepLink: %v", err)
	}
	if got != "" {
		t.Errorf("CaptureDeepLink on miss = %q; want \"\" (fallback is FallbackOpen's job)", got)
	}
	if !strings.Contains(logBuf.String(), "deep-link capture missed") {
		t.Errorf("no loud warning logged on capture miss; log = %q", logBuf.String())
	}
}

// FallbackOpen owns the generic claude.ai open affordance (URL + v0 tooltip),
// the provider-owned metadata the SPA renders when no exact link was captured.
func TestFallbackOpen(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	fo := p.FallbackOpen()
	if fo.URL != GenericDeepLink {
		t.Errorf("FallbackOpen URL = %q; want %q", fo.URL, GenericDeepLink)
	}
	if fo.Title != genericLinkTitle {
		t.Errorf("FallbackOpen Title = %q; want %q", fo.Title, genericLinkTitle)
	}
}

// Idempotence: while a capture is in flight the session reads as
// "connecting" and a second CaptureDeepLink is an immediate no-op; the
// state clears when the capture ends.
func TestCaptureDeepLink_idempotentPerSessionAndConnecting(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	p.bridgeTimeout = 600 * time.Millisecond
	const name = "repo~x-1"
	home := t.TempDir()

	done := make(chan string, 1)
	go func() {
		url, _ := p.CaptureDeepLink(context.Background(), name, "/work/a", home)
		done <- url
	}()

	// Wait for the in-flight mark.
	deadline := time.Now().Add(2 * time.Second)
	for !p.Connecting(name) {
		if time.Now().After(deadline) {
			t.Fatal("capture never marked connecting")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	got, err := p.CaptureDeepLink(context.Background(), name, "/work/a", home)
	if err != nil {
		t.Fatalf("second CaptureDeepLink: %v", err)
	}
	if got != "" {
		t.Errorf("second CaptureDeepLink while in flight = %q; want \"\" (no-op)", got)
	}
	if e := time.Since(start); e > 300*time.Millisecond {
		t.Errorf("second CaptureDeepLink took %v; want an immediate no-op", e)
	}

	<-done
	if p.Connecting(name) {
		t.Error("Connecting still true after capture finished")
	}
}
