package codexcompat

// LIVE re-verification probes against the installed codex binary and the
// real $CODEX_HOME (compat.md §Live re-verification). Opt-in via
// LAB_COMPAT_LIVE=1 (skipped otherwise — CI stays hermetic; the parent
// compat package's gating style, no build tags). Both probes are READ-ONLY:
// they never spawn a TUI, never send keys, never write codex state — the §6
// hazard checks and the §3/§4 seeding legs stay a by-hand scratch-session
// job because they mutate state and cost model calls.

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/codex"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// codexSessionsDir resolves the real rollout tree the way the adapter (and
// the codex binary) does: $CODEX_HOME when set, else ~/.codex.
func codexSessionsDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	return filepath.Join(home, ".codex", "sessions")
}

// TestCompat_Live_loginStatusParses re-verifies the §2 status coupling on the
// installed binary: the output must speak the pinned vocabulary ("Logged in"
// / "Not logged in"), the exit code must agree with it, and ParseAuthStatus
// must round-trip both signals to the same verdict.
func TestCompat_Live_loginStatusParses(t *testing.T) {
	if os.Getenv("LAB_COMPAT_LIVE") != "1" {
		t.Skip("set LAB_COMPAT_LIVE=1 to probe the installed codex binary")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex not on PATH: %v", err)
	}

	out, runErr := exec.Command(bin, "login", "status").CombinedOutput()
	exitOK := runErr == nil
	if runErr != nil {
		if _, isExit := runErr.(*exec.ExitError); !isExit {
			t.Fatalf("codex login status did not run: %v", runErr)
		}
	}
	text := string(out)

	loggedIn := strings.Contains(text, "Logged in")
	loggedOut := strings.Contains(text, "Not logged in")
	if loggedIn == loggedOut {
		t.Fatalf("status output speaks neither pinned shape exactly once (compat §2):\nexit ok=%v\n%s", exitOK, text)
	}
	// "Not logged in" contains no "Logged in" substring capital-L, so the two
	// shapes are disjoint; the exit code must agree with the text.
	if loggedIn && !exitOK {
		t.Errorf("output says logged in but exit was non-zero — the §2 exit-code pin broke:\n%s", text)
	}
	if loggedOut && exitOK {
		t.Errorf("output says not logged in but exit was zero — the §2 exit-code pin broke:\n%s", text)
	}

	st := codex.ParseAuthStatus(out, exitOK)
	if st.LoggedIn != (loggedIn && exitOK) {
		t.Errorf("ParseAuthStatus verdict %v disagrees with the live signals (text logged-in=%v, exit ok=%v)",
			st.LoggedIn, loggedIn, exitOK)
	}
	t.Logf("live status: logged_in=%v method=%q exit_ok=%v", st.LoggedIn, st.Method, exitOK)
}

// TestCompat_Live_debugModelsProbe re-verifies the `codex debug models`
// catalog probe (issue #156) against the installed binary: the probe must
// run, parse, and yield a well-formed catalog — non-empty values and labels,
// per-model efforts present, every non-empty DefaultEffort a member of its
// model's own list, and every per-model effort covered by the union (the
// invariants the conformance suite pins on the served catalog). Read-only.
func TestCompat_Live_debugModelsProbe(t *testing.T) {
	if os.Getenv("LAB_COMPAT_LIVE") != "1" {
		t.Skip("set LAB_COMPAT_LIVE=1 to probe the installed codex binary")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex not on PATH: %v", err)
	}

	models, efforts, err := codex.ProbeModels(context.Background(), &provider.HostCLI{}, bin)
	if err != nil {
		t.Fatalf("ProbeModels: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("ProbeModels returned no models; want at least one listed model")
	}
	union := make(map[string]bool, len(efforts))
	for _, e := range efforts {
		if e.Value == "" || e.Label == "" {
			t.Errorf("union effort %+v has an empty value or label", e)
		}
		union[e.Value] = true
	}
	for _, m := range models {
		if m.Value == "" || m.Label == "" {
			t.Errorf("model %+v has an empty value or label", m.Option)
		}
		if len(m.Efforts) == 0 {
			t.Errorf("model %q has no efforts", m.Value)
		}
		if m.DefaultEffort != "" && !provider.HasOption(m.Efforts, m.DefaultEffort) {
			t.Errorf("model %q DefaultEffort %q is not in its own effort list %+v", m.Value, m.DefaultEffort, m.Efforts)
		}
		for _, e := range m.Efforts {
			if !union[e.Value] {
				t.Errorf("model %q effort %q missing from the union catalog %+v", m.Value, e.Value, efforts)
			}
		}
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Value)
	}
	t.Logf("live probe: %d models %v, %d union efforts %v", len(models), names, len(efforts), efforts)
}

// TestCompat_Live_locateTranscript re-verifies the §5 location coupling
// against the real $CODEX_HOME/sessions date tree: pick a genuine rollout's
// session_meta.cwd, then drive the production adapter (codex.New with its
// real path defaults) through LocateTranscript + ReadChat and assert
// the located file belongs to that cwd and parses. Read-only.
func TestCompat_Live_locateTranscript(t *testing.T) {
	if os.Getenv("LAB_COMPAT_LIVE") != "1" {
		t.Skip("set LAB_COMPAT_LIVE=1 to probe the real codex sessions tree")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex not on PATH: %v", err)
	}
	sessions := codexSessionsDir(t)
	if _, err := os.Stat(sessions); err != nil {
		t.Skipf("no sessions tree at %s: %v", sessions, err)
	}

	// Find any real rollout's cwd (first parseable session_meta wins).
	var cwd string
	_ = filepath.WalkDir(sessions, func(path string, d fs.DirEntry, err error) error {
		if err != nil || cwd != "" || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		cwd = rolloutMetaCwd(path)
		return nil
	})
	if cwd == "" {
		t.Skip("no rollout with a parseable session_meta found in the real tree")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	// Production construction: path fields empty so the adapter's OWN default
	// resolution ($CODEX_HOME, else ~/.codex) is what gets exercised. The
	// runner is never touched by the transcript surface.
	prov, err := codex.New(codex.Options{
		LoginDir: home,
		Runner:   tmuxx.NewFake(),
		Bus:      events.NewBus(),
	})
	if err != nil {
		t.Fatalf("codex.New: %v", err)
	}

	// The instance HOME whose <home>/.codex/sessions tree holds the rollout
	// (issue #202): the real user home in the default no-$CODEX_HOME case.
	located, err := prov.LocateTranscript(context.Background(), "unused-session-name", cwd, home)
	if err != nil {
		t.Fatalf("LocateTranscript(%q): %v", cwd, err)
	}
	if located == "" {
		t.Fatalf("LocateTranscript(%q) found nothing, but the tree holds a rollout for that cwd", cwd)
	}
	// The newest-first match may be a DIFFERENT (newer) rollout for the same
	// cwd — assert identity by the located file's own session_meta.
	if got := rolloutMetaCwd(located); got != cwd {
		t.Errorf("located %s has cwd %q; want %q", located, got, cwd)
	}
	// runID/runtimeDir empty: codex's ReadChat is a pure rollout fold with no
	// live-signal channel (issue #92 / ADR-0037), so the transcript-only read
	// IS the production read.
	chat, err := prov.ReadChat("", "", located)
	if err != nil {
		t.Fatalf("ReadChat(%s): %v", located, err)
	}
	t.Logf("live locate: cwd=%q → %s (%d messages, state %s)", cwd, located, len(chat.Messages), chat.State)
}

// rolloutMetaCwd reads only the first line of a rollout and returns its
// session_meta payload.cwd, "" on any miss — the same first-line contract
// LocateTranscript pins (compat §5).
func rolloutMetaCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // session_meta carries multi-KB base_instructions
	if !sc.Scan() {
		return ""
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "session_meta" {
		return ""
	}
	return rec.Payload.Cwd
}
