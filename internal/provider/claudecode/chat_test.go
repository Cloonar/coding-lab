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
		// slash-command echo; it renders (issue #51 decision 2) but stays
		// state-neutral, so the run reads idle — not stuck in `working`
		// (issue #45).
		"clear echo idles": {
			line(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}`),
			provider.StateIdle,
		},
		// Tag order varies (compat.md §5), so an echo that leads with
		// <command-message> is also recognised — pins that OR arm.
		"command-message-led echo idles": {
			line(`{"type":"user","message":{"role":"user","content":"<command-message>rewind</command-message>\n  <command-name>/rewind</command-name>"}}`),
			provider.StateIdle,
		},
		// Local-command output is likewise state-neutral.
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

// A local slash-command echo (/clear, /rewind, …) renders as a plain user
// text message showing the command line, and its captured output as a
// lifecycle message (issue #51 decision 2 reversed issue #45's rendering
// half) — the raw <command-…> / <local-command-stdout> tags never leak into
// the chat, and a genuine plain-text reply after the echo is unaffected.
func TestParseTranscript_localCommandEchoRenders(t *testing.T) {
	echo := line(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}`) +
		line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>Conversation cleared</local-command-stdout>"}}`)

	got, err := ParseTranscript(strings.NewReader(echo))
	if err != nil {
		t.Fatalf("ParseTranscript(echo): %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("echo rendered %d messages; want the command line + its output", len(got.Messages))
	}
	if m := got.Messages[0]; m.Kind != provider.MessageText || m.Role != "user" || m.Text != "/clear" {
		t.Errorf("echo msg0 = %+v; want user text \"/clear\"", m)
	}
	if m := got.Messages[1]; m.Kind != provider.MessageLifecycle || m.Text != "Conversation cleared" || m.Error {
		t.Errorf("echo msg1 = %+v; want a non-error lifecycle with the stdout", m)
	}

	// A command with args echoes as one "/name args" line; an empty stdout is
	// dropped entirely.
	got, err = ParseTranscript(strings.NewReader(
		line(`{"type":"user","message":{"role":"user","content":"<command-name>/foo</command-name>\n  <command-message>foo</command-message>\n  <command-args>bar baz</command-args>"}}`) +
			line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout></local-command-stdout>"}}`)))
	if err != nil {
		t.Fatalf("ParseTranscript(args echo): %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Text != "/foo bar baz" {
		t.Fatalf("args echo messages = %+v; want just \"/foo bar baz\"", got.Messages)
	}

	// Echo then a genuine reply: the reply still renders and derives working
	// (the Send→Interrupt morph, ADR-0022).
	got, err = ParseTranscript(strings.NewReader(echo +
		line(`{"type":"user","message":{"role":"user","content":"now do the thing"}}`)))
	if err != nil {
		t.Fatalf("ParseTranscript(echo+reply): %v", err)
	}
	if n := len(got.Messages); n != 3 || got.Messages[2].Text != "now do the thing" {
		t.Fatalf("echo+reply messages = %+v; want echo, stdout, reply", got.Messages)
	}
	if got.State != provider.StateWorking {
		t.Errorf("echo+reply state = %q; want %q (Send→Interrupt morph preserved)", got.State, provider.StateWorking)
	}
}

// THE issue-#45 / issue-#51 regression pin: command echoes are excluded from
// state derivation even though they now render. The echo formerly drove
// deriveState to `working`, which stranded the composer on a pulsing
// Interrupt with no Send after /clear (the stuck-composer root cause,
// ADR-0022 / issue #45); issue #51 decision 2 made echoes visible but kept
// them state-neutral. Two edges: a transcript whose tail is ONLY an echo
// (+stdout) after real turns keeps the pre-echo state, and a fresh
// post-/clear transcript containing ONLY the echo derives idle.
func TestParseTranscript_commandEchoNeverDrivesState(t *testing.T) {
	echoTail := line(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}`) +
		line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>cleared</local-command-stdout>"}}`)

	// Real turns, then only the echo: the assistant's ended turn still decides
	// the state — needs_input, exactly as if the echo were absent.
	turns := line(`{"type":"user","message":{"role":"user","content":"go"}}`) +
		line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
	got, err := ParseTranscript(strings.NewReader(turns + echoTail))
	if err != nil {
		t.Fatalf("ParseTranscript(turns+echo): %v", err)
	}
	if got.State != provider.StateNeedsInput {
		t.Errorf("turns+echo state = %q; want %q (echo must not override the pre-echo state)", got.State, provider.StateNeedsInput)
	}

	// A fresh post-/clear transcript contains ONLY the echo: idle, never
	// `working` — the composer shows Send, not the stuck Interrupt.
	got, err = ParseTranscript(strings.NewReader(echoTail))
	if err != nil {
		t.Fatalf("ParseTranscript(echo only): %v", err)
	}
	if got.State != provider.StateIdle {
		t.Errorf("fresh post-/clear state = %q; want %q (the stuck-composer case)", got.State, provider.StateIdle)
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
