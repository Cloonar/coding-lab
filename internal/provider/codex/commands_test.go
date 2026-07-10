package codex

// Commands catalog tests (issue #51 decision 5): the pinned static builtin
// table scraped from the live 0.133.0 TUI (issue #87). Unlike claudecode
// there is no project/user discovery to test — the golden IS the machinery.

import (
	"context"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// The pinned catalog order (scrape 2026-07-10): chat-safe rows first in
// operator-usefulness order, curated-out rows alphabetically after.
var wantCommandOrder = []string{
	// chat-safe
	"new", "clear", "compact", "status", "diff", "mcp", "ps", "stop", "copy",
	// curated out
	"agent", "approve", "exit", "experimental", "feedback", "fork", "goal",
	"hooks", "ide", "init", "keymap", "logout", "memories", "mention", "model",
	"permissions", "personality", "pets", "plan", "plugins", "raw", "rename",
	"resume", "review", "side", "skills", "statusline", "subagents", "theme",
	"title", "vim",
}

func TestBuiltinCommands_pinnedGolden(t *testing.T) {
	cmds := BuiltinCommands()
	if len(cmds) != len(wantCommandOrder) {
		t.Fatalf("catalog has %d entries; want %d", len(cmds), len(wantCommandOrder))
	}
	safeCut := 9 // the chat-safe prefix length
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
		if wantRole := ""; c.Name == "new" {
			wantRole = provider.CommandRoleClear
			if c.Role != wantRole {
				t.Errorf("/new role = %q; want %q (the native clear-context command)", c.Role, wantRole)
			}
		} else if c.Role != "" {
			t.Errorf("cmds[%d] (%s).Role = %q; only /new carries a role", i, c.Name, c.Role)
		}
	}

	// Spot-pin scraped descriptions verbatim (0.133.0 popup text).
	wantDescriptions := map[string]string{
		"new":    "start a new chat during a conversation",
		"model":  "choose what model and reasoning effort to use",
		"mcp":    "list configured MCP tools; use /mcp verbose for details",
		"raw":    "toggle raw scrollback mode for copy-friendly terminal selection",
		"init":   "create an AGENTS.md file with instructions for Codex",
		"status": "show current session configuration and token usage",
	}
	byName := map[string]provider.CommandSpec{}
	for _, c := range cmds {
		byName[c.Name] = c
	}
	for name, want := range wantDescriptions {
		if got := byName[name].Description; got != want {
			t.Errorf("/%s description = %q; want the scraped %q", name, got, want)
		}
	}
}

// Commands is static and worktree-independent — codex 0.133 discovers
// nothing per-project — and returns a defensive copy.
func TestCommands_staticAndCopied(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	ctx := context.Background()

	a, err := p.Commands(ctx, "/anywhere")
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	b, err := p.Commands(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	if len(a) != len(b) || len(a) != len(BuiltinCommands()) {
		t.Fatalf("catalog varies by worktree: %d vs %d entries", len(a), len(b))
	}

	a[0].Name = "mutated"
	if BuiltinCommands()[0].Name != "new" {
		t.Error("Commands exposed internal catalog storage")
	}
	if fresh, _ := p.Commands(ctx, "/anywhere"); fresh[0].Name != "new" {
		t.Error("a caller's mutation poisoned the next Commands call")
	}
}

// The catalog is complete-and-honest: unsafe builtins ARE returned (the API
// layer filters on ChatSafe), with both verdicts present.
func TestCommands_chatSafeIsTheCurationSignal(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	cmds, err := p.Commands(context.Background(), t.TempDir())
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
