package codex

// Commands catalog tests (issue #51 decision 5): the pinned static builtin
// table extracted from codex-cli 0.167.0's SlashCommand enum (compat.md §7;
// the 0.133.0 table was a live TUI scrape). Unlike claudecode there is no
// project/user discovery to test — the golden IS the machinery.

import (
	"context"
	"reflect"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// The pinned catalog order (source extraction 2026-11-10): chat-safe rows
// first in operator-usefulness order, curated-out rows alphabetically after.
var wantCommandOrder = []string{
	// chat-safe
	"clear", "compact", "status", "diff", "mcp", "ps", "stop", "pwd", "recap",
	"hooks", "plugins", "debug-config",
	// curated out
	"agents", "approve", "apps", "archive", "btw", "cd", "config", "copy",
	"daemon", "daybreak", "debug-m-drop", "debug-m-update", "delete", "exit",
	"experimental", "export", "feedback", "fork", "goal", "ide", "import",
	"init", "keymap", "logout", "memories", "mention", "model", "new",
	"permissions", "pets", "plan", "presets", "quit", "raw", "rename",
	"resume", "review", "setup-default-sandbox", "side", "skills",
	"statusline", "subagents", "theme", "title", "tui", "usage", "vim",
	"voice", "warnings", "worktree",
}

func TestBuiltinCommands_pinnedGolden(t *testing.T) {
	cmds := BuiltinCommands()
	if len(cmds) != len(wantCommandOrder) {
		t.Fatalf("catalog has %d entries; want %d", len(cmds), len(wantCommandOrder))
	}
	safeCut := 12 // the chat-safe prefix length
	for i, want := range wantCommandOrder {
		c := cmds[i]
		if c.Name != want {
			t.Errorf("cmds[%d].Name = %q; want %q (pinned order)", i, c.Name, want)
		}
		if wantSafe := i < safeCut; c.ChatSafe != wantSafe {
			t.Errorf("cmds[%d] (%s).ChatSafe = %v; want %v", i, c.Name, c.ChatSafe, wantSafe)
		}
		if c.Source != "builtin" {
			t.Errorf("cmds[%d] (%s).Source = %q; want builtin (no project/user discovery)", i, c.Name, c.Source)
		}
		if c.Description == "" {
			t.Errorf("cmds[%d] (%s) has no description; the popup shows one for every row", i, c.Name)
		}
		if wantRole := ""; c.Name == "clear" {
			wantRole = provider.CommandRoleClear
			if c.Role != wantRole {
				t.Errorf("/clear role = %q; want %q (the native clear-context command on 0.167: /new opens a worktree picker in a git repo)", c.Role, wantRole)
			}
		} else if c.Role != "" {
			t.Errorf("cmds[%d] (%s).Role = %q; only /clear carries a role", i, c.Name, c.Role)
		}
	}

	// Spot-pin descriptions verbatim (0.167.0 slash_command.rs description()).
	wantDescriptions := map[string]string{
		"clear":  "clear the terminal and start a new chat",
		"new":    "start a new chat during a conversation",
		"model":  "choose what model and reasoning effort to use",
		"mcp":    "list MCP tools; use /mcp verbose or /mcp login <name>",
		"raw":    "toggle raw scrollback mode for copy-friendly terminal selection",
		"init":   "create an AGENTS.md file with instructions for Codex",
		"status": "show current session configuration and token usage",
		"copy":   "copy the last response or part of it",
	}
	byName := map[string]provider.CommandSpec{}
	for _, c := range cmds {
		byName[c.Name] = c
	}
	for name, want := range wantDescriptions {
		if got := byName[name].Description; got != want {
			t.Errorf("/%s description = %q; want the extracted %q", name, got, want)
		}
	}
}

// Commands is static and worktree-independent — codex discovers nothing
// per-project — and returns a defensive copy.
func TestCommands_staticAndCopied(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	ctx := context.Background()

	a, err := p.Commands(ctx, "/anywhere", "")
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	b, err := p.Commands(ctx, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	if len(a) != len(b) || len(a) != len(BuiltinCommands()) {
		t.Fatalf("catalog varies by worktree/home: %d vs %d entries", len(a), len(b))
	}

	a[0].Name = "mutated"
	if BuiltinCommands()[0].Name != "clear" {
		t.Error("Commands exposed internal catalog storage")
	}
	if fresh, _ := p.Commands(ctx, "/anywhere", ""); fresh[0].Name != "clear" {
		t.Error("a caller's mutation poisoned the next Commands call")
	}
}

// TestCommands_homeIndependence pins the doc comment's home-independence claim
// (issue #202) with DEEP equality, not just a count: codex has no
// project- or user-level command discovery, so the served catalog must be
// byte-for-byte identical for every worktree/home combination, including the
// empty-string edges that mean "no worktree yet" / "no per-run home".
func TestCommands_homeIndependence(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	ctx := context.Background()
	want := BuiltinCommands()

	worktrees := []string{"", "/work/a", t.TempDir()}
	homes := []string{"", "/nonexistent/home", t.TempDir()}
	for _, wt := range worktrees {
		for _, home := range homes {
			got, err := p.Commands(ctx, wt, home)
			if err != nil {
				t.Fatalf("Commands(worktree=%q, home=%q): %v", wt, home, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Commands(worktree=%q, home=%q) = %+v; want the identical builtin table %+v", wt, home, got, want)
			}
		}
	}
}

// The catalog is complete-and-honest: unsafe builtins ARE returned (the API
// layer filters on ChatSafe), with both verdicts present.
func TestCommands_chatSafeIsTheCurationSignal(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	cmds, err := p.Commands(context.Background(), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	var safe, unsafe int
	for _, c := range cmds {
		if c.ChatSafe {
			safe++
		} else {
			unsafe++
		}
	}
	if safe == 0 || unsafe == 0 {
		t.Errorf("catalog safe/unsafe = %d/%d; want both present (honest flags, API filters)", safe, unsafe)
	}
}
