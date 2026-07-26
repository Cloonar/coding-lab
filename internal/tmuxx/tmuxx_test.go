package tmuxx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newSessionArgs is pure (no tmux), so the cap wrapping is asserted
// directly on the argv (transcribed from v0
// TestSessions_newSessionArgsNofileCap; the argv arrives complete from
// the provider — session name and --effort/--model already rendered).
func TestNewSessionArgs_nofileCap(t *testing.T) {
	argv := []string{"claude", "--remote-control", "proj-x", "--effort", "max", "--model", "opus[1m]"}

	capped := New("tmux", WithNofileCap("prlimit", 4096))
	got := capped.newSessionArgs("proj-x", "/work/x", argv, nil, startConfig{})
	want := []string{
		"new-session", "-d", "-s", "proj-x", "-c", "/work/x",
		"prlimit", "--nofile=4096:4096", "--",
		"claude", "--remote-control", "proj-x",
		"--effort", "max", "--model", "opus[1m]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("capped newSessionArgs =\n  %q\nwant\n  %q", got, want)
	}

	// Zero cap (the default) spawns bare — no wrapper, just the argv.
	bare := New("tmux")
	got = bare.newSessionArgs("proj-x", "/work/x", argv, nil, startConfig{})
	want = []string{
		"new-session", "-d", "-s", "proj-x", "-c", "/work/x",
		"claude", "--remote-control", "proj-x",
		"--effort", "max", "--model", "opus[1m]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("bare newSessionArgs =\n  %q\nwant\n  %q", got, want)
	}
}

// WithoutNofileCap retires the prlimit wrapper for one Start even on a
// capped runner (issue #205): a container pane's inner command is the podman
// client, whose agent is bounded by the container's own --ulimit nofile —
// the host-side wrapper would cap the wrong process.
func TestNewSessionArgs_withoutNofileCap(t *testing.T) {
	argv := []string{"podman", "run", "--rm", "-it", "img", "claude"}
	capped := New("tmux", WithNofileCap("prlimit", 4096))

	var cfg startConfig
	WithoutNofileCap()(&cfg)
	got := capped.newSessionArgs("proj-x", "/work/x", argv, nil, cfg)
	want := []string{
		"new-session", "-d", "-s", "proj-x", "-c", "/work/x",
		"podman", "run", "--rm", "-it", "img", "claude",
	}
	if !slices.Equal(got, want) {
		t.Errorf("uncapped newSessionArgs =\n  %q\nwant\n  %q", got, want)
	}
}

// extraEnv entries become `-e KEY=VALUE` flags on new-session — after the
// target/dir flags, BEFORE the prlimit wrapper and inner command, so the
// environment applies to the pane while the token value stays out of the
// pane command's argv (and so out of `ps` output).
func TestNewSessionArgs_extraEnvFlags(t *testing.T) {
	argv := []string{"claude", "--remote-control", "repo~20260608-1530"}
	env := []string{"LAB_URL=http://127.0.0.1:8080", "LAB_TOKEN=lab_run_secret"}

	tm := New("tmux", WithNofileCap("prlimit", 1024))
	got := tm.newSessionArgs("repo~20260608-1530", "/work/wt", argv, env, startConfig{})
	want := []string{
		"new-session", "-d", "-s", "repo~20260608-1530", "-c", "/work/wt",
		"-e", "LAB_URL=http://127.0.0.1:8080",
		"-e", "LAB_TOKEN=lab_run_secret",
		"prlimit", "--nofile=1024:1024", "--",
		"claude", "--remote-control", "repo~20260608-1530",
	}
	if !slices.Equal(got, want) {
		t.Errorf("newSessionArgs with env =\n  %q\nwant\n  %q", got, want)
	}
}

// nofile <= 0 disables the cap entirely (v0 `-session-nofile` 0-disables).
func TestNofileCapArgv_zeroAndNegativeDisable(t *testing.T) {
	for _, n := range []int{0, -1} {
		tm := New("tmux", WithNofileCap("prlimit", n))
		if got := tm.nofileCapArgv(); got != nil {
			t.Errorf("nofile=%d: nofileCapArgv() = %q; want nil", n, got)
		}
	}
	tm := New("tmux", WithNofileCap("prlimit", 16384))
	want := []string{"prlimit", "--nofile=16384:16384", "--"}
	if got := tm.nofileCapArgv(); !slices.Equal(got, want) {
		t.Errorf("nofileCapArgv() = %q; want %q", got, want)
	}
}

// The private-socket option prefixes every tmux invocation with -L.
func TestCmd_privateSocketFlag(t *testing.T) {
	tm := New("tmux", WithSocket("lab-test-sock"))
	cmd := tm.cmd(t.Context(), "has-session", "-t", "=x")
	want := []string{"tmux", "-L", "lab-test-sock", "has-session", "-t", "=x"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd args = %q; want %q", cmd.Args, want)
	}

	bare := New("tmux")
	cmd = bare.cmd(t.Context(), "list-sessions", "-F", "#{session_name}")
	want = []string{"tmux", "list-sessions", "-F", "#{session_name}"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("cmd args without socket = %q; want %q", cmd.Args, want)
	}
}

// isBaselineVar is the pass-through allow-list: the fixed names plus any
// LC_* locale category, and nothing else (secrets like LAB_DB, tmux's own
// TMUX, git's GIT_* all fall through).
func TestIsBaselineVar(t *testing.T) {
	for _, name := range []string{
		"PATH", "HOME", "TERM", "TERMINFO_DIRS",
		"LANG", "LANGUAGE", "LOCALE_ARCHIVE",
		"LC_ALL", "LC_CTYPE", "LC_TIME",
		// The rootless podman client's runtime-root anchor (issue #205).
		"XDG_RUNTIME_DIR",
	} {
		if !isBaselineVar(name) {
			t.Errorf("isBaselineVar(%q) = false; want true", name)
		}
	}
	for _, name := range []string{
		"LAB_DB", "LAB_URL", "LAB_TOKEN", "TMUX", "SSH_AUTH_SOCK",
		"GIT_AUTHOR_NAME", "PATHOLOGICAL", "LC", "XLC_ALL", "",
	} {
		if isBaselineVar(name) {
			t.Errorf("isBaselineVar(%q) = true; want false", name)
		}
	}
}

// baselineEnv forwards ONLY the present baseline vars from the lab process
// env: a planted secret is dropped, the locale set survives, and a baseline
// var that isn't actually set is not fabricated.
func TestBaselineEnv_filtersProcessEnv(t *testing.T) {
	t.Setenv("LAB_DB", "postgres://user:secret@db/lab")
	t.Setenv("LC_ALL", "C.UTF-8")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("TERMINFO_DIRS", "/nix/store/xxx-terminfo/share/terminfo")
	t.Setenv("PATH", "/nix/store/bin:/bin")
	t.Setenv("HOME", "/home/lab")
	// LANGUAGE is deliberately left unset to prove absent vars aren't invented.

	got := envNames(baselineEnv())
	for _, name := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TERMINFO_DIRS"} {
		if !got[name] {
			t.Errorf("baselineEnv() missing baseline var %q; got %v", name, got)
		}
	}
	if got["LAB_DB"] {
		t.Errorf("baselineEnv() leaked secret LAB_DB; got %v", got)
	}
	if got["LANGUAGE"] {
		t.Errorf("baselineEnv() fabricated unset LANGUAGE; got %v", got)
	}
}

// cmd pins Env to the baseline: a secret in the lab process env never reaches
// the tmux client (and so never seeds the server's global env / any pane),
// while the baseline survives. A non-nil Env is the whole point — a nil
// cmd.Env means "inherit os.Environ() wholesale", which was the #204 leak.
func TestCmd_scrubsEnvToBaseline(t *testing.T) {
	t.Setenv("LAB_DB", "postgres://user:secret@db/lab")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
	t.Setenv("LC_ALL", "C.UTF-8")
	t.Setenv("PATH", "/nix/store/bin:/bin")
	t.Setenv("HOME", "/home/lab")

	cmd := New("tmux").cmd(t.Context(), "has-session", "-t", "=x")
	if cmd.Env == nil {
		t.Fatal("cmd.Env is nil — the pane would inherit the whole lab process env (#204)")
	}
	got := envNames(cmd.Env)
	for _, name := range []string{"PATH", "HOME", "LC_ALL"} {
		if !got[name] {
			t.Errorf("cmd.Env missing baseline var %q; got %v", name, got)
		}
	}
	for _, name := range []string{"LAB_DB", "TMUX"} {
		if got[name] {
			t.Errorf("cmd.Env leaked non-baseline var %q; got %v", name, got)
		}
	}
}

// isEnvName screens continuation lines of a multi-line show-environment value
// (which can't be real variable names) out of the legacy global-env scrub.
func TestIsEnvName(t *testing.T) {
	for _, s := range []string{"PATH", "LAB_DB", "_hidden", "A1_B2", "X"} {
		if !isEnvName(s) {
			t.Errorf("isEnvName(%q) = false; want true", s)
		}
	}
	for _, s := range []string{"", "1PATH", "bad.name", "has space", "a=b", "-flag", "a-b"} {
		if isEnvName(s) {
			t.Errorf("isEnvName(%q) = true; want false", s)
		}
	}
}

// globalEnvOps is pure, so the reconciliation matrix is pinned directly:
// a stale baseline value and a missing baseline var are (re-)set, a matching
// value produces no operation at all, a non-baseline var is unset, and a
// baseline var the server carries but the desired baseline lacks is left
// alone in BOTH directions (a TERM the server picked up must survive).
// Staged-removal markers, continuation-shaped lines, and syntactically
// invalid names are screened out.
func TestGlobalEnvOps(t *testing.T) {
	desired := []string{
		"PATH=/nix/bin:/bin",
		"XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
	}
	shown := strings.Join([]string{
		"PATH=/nix/bin:/bin",                      // matches desired: no op
		"XDG_RUNTIME_DIR=/run/lab",                // stale (the retired ADR-0057 value): re-pinned
		"TERM=screen-256color",                    // baseline, absent from desired: untouched
		"LAB_DB=postgres://user:secret@db",        // non-baseline: unset
		"-STAGED",                                 // staged-removal marker: ignored
		"continuation line of a multi-line value", // no NAME= shape: ignored
		"bad.name=x",                              // invalid env name (a continuation lookalike): ignored
	}, "\n") + "\n"

	set, unset := globalEnvOps(shown, desired)
	wantSet := []string{
		"XDG_RUNTIME_DIR=/run/user/1000",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
	}
	if !slices.Equal(set, wantSet) {
		t.Errorf("set = %q, want %q", set, wantSet)
	}
	if want := []string{"LAB_DB"}; !slices.Equal(unset, want) {
		t.Errorf("unset = %q, want %q", unset, want)
	}

	// A server already mirroring the baseline yields no operations — the
	// happy path syncGlobalEnv turns into a single show-environment call.
	set, unset = globalEnvOps(strings.Join(desired, "\n")+"\n", desired)
	if len(set)+len(unset) != 0 {
		t.Errorf("in-sync server: set = %q, unset = %q, want none", set, unset)
	}
}

// fakeTmuxBin writes a stand-in tmux: every invocation's args are appended
// (space-joined, one line each) to the returned log, and show-environment
// answers the canned global env. Lets the syncGlobalEnv tests observe the
// exact client calls without a real tmux server.
func fakeTmuxBin(t *testing.T, showOut string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "tmux")
	log = filepath.Join(dir, "calls.log")
	show := filepath.Join(dir, "show.out")
	if err := os.WriteFile(show, []byte(showOut), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + log + "\"\n" +
		"if [ \"$1\" = show-environment ]; then cat \"" + show + "\"; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// loggedCalls reads the fake's log: one space-joined argv per line.
func loggedCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("reading fake tmux log: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// A server whose global env already mirrors lab's baseline costs exactly one
// show-environment call — no set-environment at all (the cheap happy path
// every Start pays).
func TestSyncGlobalEnv_inSyncServerIsOneCall(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	bin, log := fakeTmuxBin(t, strings.Join(baselineEnv(), "\n")+"\n")

	if err := New(bin).syncGlobalEnv(t.Context()); err != nil {
		t.Fatalf("syncGlobalEnv: %v", err)
	}
	calls := loggedCalls(t, log)
	if len(calls) != 1 || calls[0] != "show-environment -g" {
		t.Errorf("calls = %q, want exactly [\"show-environment -g\"]", calls)
	}
}

// The ADR-0060 adoption case: a surviving server carries the retired
// layout's XDG_RUNTIME_DIR, no DBUS address, and a pre-#204 secret. One
// chained set-environment call re-pins the stale value, sets the missing
// one, and unsets the secret — while matching baseline vars produce no
// operation.
func TestSyncGlobalEnv_repinsStaleServer(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")

	// The server global env: every current baseline entry verbatim, except
	// XDG_RUNTIME_DIR stale and DBUS_SESSION_BUS_ADDRESS absent; plus a
	// leaked secret.
	var shown []string
	for _, kv := range baselineEnv() {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "XDG_RUNTIME_DIR":
			shown = append(shown, "XDG_RUNTIME_DIR=/run/lab")
		case "DBUS_SESSION_BUS_ADDRESS":
			// a pre-fix server never had it
		default:
			shown = append(shown, kv)
		}
	}
	shown = append(shown, "LAB_DB=postgres://user:secret@db/lab")
	bin, log := fakeTmuxBin(t, strings.Join(shown, "\n")+"\n")

	if err := New(bin).syncGlobalEnv(t.Context()); err != nil {
		t.Fatalf("syncGlobalEnv: %v", err)
	}
	calls := loggedCalls(t, log)
	if len(calls) != 2 {
		t.Fatalf("calls = %q, want the show and one chained set call", calls)
	}
	for _, want := range []string{
		"set-environment -g XDG_RUNTIME_DIR /run/user/1000",
		"set-environment -g DBUS_SESSION_BUS_ADDRESS unix:path=/run/user/1000/bus",
		"set-environment -g -u LAB_DB",
	} {
		if !strings.Contains(calls[1], want) {
			t.Errorf("chained call %q missing %q", calls[1], want)
		}
	}
	// Matching vars produced no operations: exactly the three above.
	if got := strings.Count(calls[1], "set-environment"); got != 3 {
		t.Errorf("chained call carries %d operations, want 3: %q", got, calls[1])
	}
}

// envNames indexes a KEY=VALUE slice by name for presence assertions.
func envNames(env []string) map[string]bool {
	m := make(map[string]bool, len(env))
	for _, kv := range env {
		if name, _, ok := strings.Cut(kv, "="); ok {
			m[name] = true
		}
	}
	return m
}
