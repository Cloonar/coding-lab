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
