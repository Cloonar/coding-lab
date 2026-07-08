package claudecode

// Transcript location (registry cwd-match → slug/sessionId path) and the
// state-derivation edges the compat fixture doesn't cover. The full JSONL →
// schema mapping is pinned in internal/compat against a captured fixture.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

func chatProvider(t *testing.T, registryDir, projectsDir string) *Provider {
	t.Helper()
	p, err := New(Options{
		ClaudeBin: "claude", ConfigPath: filepath.Join(t.TempDir(), ".claude.json"),
		RegistryDir: registryDir, ProjectsDir: projectsDir, LoginDir: t.TempDir(),
		Runner: tmuxx.NewFake(), Bus: events.NewBus(),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestLocateTranscript_registryMatch(t *testing.T) {
	registryDir := t.TempDir()
	projectsDir := t.TempDir()
	worktree := "/home/op/state/worktrees/proj-manual-1"
	sessionID := "abcd-1234"

	// A live registry entry (this test process) whose cwd is the worktree.
	writeJSONFile(t, filepath.Join(registryDir, "1.json"), map[string]any{
		"pid": os.Getpid(), "cwd": worktree, "startedAt": 1, "sessionId": sessionID,
		"bridgeSessionId": "session_x",
	})
	// The transcript file at the slug path.
	dir := filepath.Join(projectsDir, SlugForDir(worktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := chatProvider(t, registryDir, projectsDir)
	got, err := p.LocateTranscript(context.Background(), "proj~manual-1", worktree)
	if err != nil {
		t.Fatalf("LocateTranscript: %v", err)
	}
	if got != path {
		t.Errorf("LocateTranscript = %q; want %q", got, path)
	}

	// A worktree with no registry entry misses cleanly (no error).
	got, err = p.LocateTranscript(context.Background(), "proj~other", "/no/such/worktree")
	if err != nil || got != "" {
		t.Errorf("miss = (%q, %v); want (\"\", nil)", got, err)
	}
}

func TestReadTranscript_goneFile(t *testing.T) {
	p := chatProvider(t, t.TempDir(), t.TempDir())
	if _, err := p.ReadTranscript(filepath.Join(t.TempDir(), "absent.jsonl")); err != provider.ErrTranscriptGone {
		t.Errorf("ReadTranscript(absent) err = %v; want ErrTranscriptGone", err)
	}
}

func TestParseTranscript_stateEdges(t *testing.T) {
	cases := map[string]struct {
		lines string
		want  string
	}{
		"empty":      {"", provider.StateIdle},
		"user waits": {line(`{"type":"user","message":{"role":"user","content":"go"}}`), provider.StateWorking},
		"assistant ended": {
			line(`{"type":"user","message":{"role":"user","content":"go"}}`) +
				line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`),
			provider.StateNeedsInput,
		},
		"tool running": {
			line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","id":"t1","input":{"command":"ls"}}]}}`),
			provider.StateWorking,
		},
		// A /clear rotates to a fresh transcript whose only tail is the local
		// slash-command echo; it is non-conversational, so the tail is empty and
		// the run reads idle — not stuck in `working` (issue #45).
		"clear echo idles": {
			line(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}`),
			provider.StateIdle,
		},
		// Tag order varies (compat.md §5), so an echo that leads with
		// <command-message> is also non-conversational — pins that OR arm.
		"command-message-led echo idles": {
			line(`{"type":"user","message":{"role":"user","content":"<command-message>rewind</command-message>\n  <command-name>/rewind</command-name>"}}`),
			provider.StateIdle,
		},
		// Local-command output is likewise non-conversational.
		"command output idles": {
			line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>ok</local-command-stdout>"}}`),
			provider.StateIdle,
		},
	}
	for name, c := range cases {
		got, err := ParseTranscript(strings.NewReader(c.lines))
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", name, err)
		}
		if got.State != c.want {
			t.Errorf("%s: state = %q; want %q", name, got.State, c.want)
		}
	}
}

// A local slash-command echo (/clear, /rewind, …) and its captured output are
// non-conversational breadcrumbs: they must not render as user bubbles (no raw
// <command-…> / <local-command-stdout> tags in the chat) and must not drive
// conversational state. A genuine plain-text reply after the echo is
// unaffected — it still renders and derives `working`, preserving the
// Send→Interrupt morph (ADR-0022). Issue #45.
func TestParseTranscript_localCommandEcho(t *testing.T) {
	echo := line(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}`) +
		line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>Conversation cleared</local-command-stdout>"}}`)

	// Echo (and output) alone: no messages, idle state — the stuck-Interrupt case.
	got, err := ParseTranscript(strings.NewReader(echo))
	if err != nil {
		t.Fatalf("ParseTranscript(echo): %v", err)
	}
	if len(got.Messages) != 0 {
		t.Errorf("echo rendered %d messages; want 0 (no raw <command-…> bubble)", len(got.Messages))
	}
	if got.State != provider.StateIdle {
		t.Errorf("echo state = %q; want %q", got.State, provider.StateIdle)
	}

	// Echo then a genuine reply: only the reply renders, and it still derives working.
	got, err = ParseTranscript(strings.NewReader(echo +
		line(`{"type":"user","message":{"role":"user","content":"now do the thing"}}`)))
	if err != nil {
		t.Fatalf("ParseTranscript(echo+reply): %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Text != "now do the thing" {
		t.Fatalf("echo+reply messages = %+v; want just the genuine reply", got.Messages)
	}
	if got.State != provider.StateWorking {
		t.Errorf("echo+reply state = %q; want %q (Send→Interrupt morph preserved)", got.State, provider.StateWorking)
	}
}

// A line beyond the scanner cap must surface as an error — silently stopping
// mid-file would serve a truncated chat (and a backwards-moving cursor) on
// every subsequent reparse.
func TestParseTranscript_oversizeLineErrors(t *testing.T) {
	huge := line(`{"type":"user","message":{"role":"user","content":"` + strings.Repeat("x", 9*1024*1024) + `"}}`)
	if _, err := ParseTranscript(strings.NewReader(huge)); err == nil {
		t.Fatal("ParseTranscript(9MB line) = nil error; want a scan error")
	}
}

func line(s string) string { return s + "\n" }

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
