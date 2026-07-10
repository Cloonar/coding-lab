package codex

// The slash-command catalog (issue #51 decision 5): a curated STATIC builtin
// table — codex discovers skills only globally ($CODEX_HOME/skills) and repo
// .codex dirs are not a command source, so there is no project/user command
// discovery. The rows are a fragile Codex coupling scraped VERBATIM from the
// live 0.133.0 TUI slash popup (issue #87, tmux scrape 2026-07-10; names,
// descriptions, and order exactly as rendered) and curated row by row:
// ChatSafe=false marks a command that would strand the TUI in a
// picker/overlay lab cannot see (never-scrape means lab could neither render
// nor answer it), or that fights lab's own management of the session (auth,
// lifecycle, transcript identity, composer/rendering modes the reply and
// interrupt recipes assume). The API layer filters on ChatSafe; this catalog
// stays complete and honest either way.

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// commandSourceBuiltin is the only Source codex serves (pinned API
// vocabulary, issue #51 decision 5) — no project/user discovery.
const commandSourceBuiltin = "builtin"

// builtinCommands is the pinned builtin table — codex-cli 0.133.0, live TUI
// scrape 2026-07-10. Order is the pin: the chat-safe rows first in
// operator-usefulness order, then the curated-out rows alphabetically;
// Commands serves the table in exactly this order.
var builtinCommands = []provider.CommandSpec{
	// --- chat-safe: executes inline and returns to the prompt ---------------
	{Name: "new", Description: "start a new chat during a conversation",
		Source: commandSourceBuiltin, Role: provider.CommandRoleClear, ChatSafe: true}, // clears context; rotates the rollout file (the clear/epoch identity)
	{Name: "clear", Description: "clear the terminal and start a new chat",
		Source: commandSourceBuiltin, ChatSafe: true}, // also rotates to a fresh chat; /new carries the clear role
	{Name: "compact", Description: "summarize conversation to prevent hitting the context limit",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "status", Description: "show current session configuration and token usage",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "diff", Description: "show git diff (including untracked files)",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "mcp", Description: "list configured MCP tools; use /mcp verbose for details",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "ps", Description: "list background terminals",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "stop", Description: "stop all background terminals",
		Source: commandSourceBuiltin, ChatSafe: true}, // inline action, returns to the prompt — a legitimate operator brake
	{Name: "copy", Description: "copy last response as markdown",
		Source: commandSourceBuiltin, ChatSafe: true}, // output-only (OSC52 clipboard); harmless headless
	// --- curated out (ChatSafe=false), alphabetically; reason per row -------
	{Name: "agent", Description: "switch the active agent thread", Source: commandSourceBuiltin},                                       // thread picker
	{Name: "approve", Description: "approve one retry of a recent auto-review denial", Source: commandSourceBuiltin},                   // approval overlay; never-ask posture keeps these out anyway
	{Name: "exit", Description: "exit Codex", Source: commandSourceBuiltin},                                                            // session-ending
	{Name: "experimental", Description: "toggle experimental features", Source: commandSourceBuiltin},                                  // toggle overlay
	{Name: "feedback", Description: "send logs to maintainers", Source: commandSourceBuiltin},                                          // external submit flow
	{Name: "fork", Description: "fork the current chat", Source: commandSourceBuiltin},                                                 // picker; forks transcript identity under lab
	{Name: "goal", Description: "set or view the goal for a long-running task", Source: commandSourceBuiltin},                          // unverified overlay behavior — curated out until live-checked
	{Name: "hooks", Description: "view and manage lifecycle hooks", Source: commandSourceBuiltin},                                      // management overlay
	{Name: "ide", Description: "include current selection, open files, and other context from your IDE", Source: commandSourceBuiltin}, // needs an IDE attach lab has no surface for
	{Name: "init", Description: "create an AGENTS.md file with instructions for Codex", Source: commandSourceBuiltin},                  // writes a repo-tracked AGENTS.md
	{Name: "keymap", Description: "remap TUI shortcuts", Source: commandSourceBuiltin},                                                 // overlay; remaps keys the recipes assume
	{Name: "logout", Description: "log out of Codex", Source: commandSourceBuiltin},                                                    // machine-level auth is lab's own surface
	{Name: "memories", Description: "configure memory use and generation", Source: commandSourceBuiltin},                               // config overlay
	{Name: "mention", Description: "mention a file", Source: commandSourceBuiltin},                                                     // file picker
	{Name: "model", Description: "choose what model and reasoning effort to use", Source: commandSourceBuiltin},                        // picker
	{Name: "permissions", Description: "choose what Codex is allowed to do", Source: commandSourceBuiltin},                             // picker; fights the pinned spawn posture
	{Name: "personality", Description: "choose a communication style for Codex", Source: commandSourceBuiltin},                         // picker
	{Name: "pets", Description: "choose or hide the terminal pet", Source: commandSourceBuiltin},                                       // picker
	{Name: "plan", Description: "switch to Plan mode", Source: commandSourceBuiltin},                                                   // mode switch ending in an approval flow lab cannot see
	{Name: "plugins", Description: "browse plugins", Source: commandSourceBuiltin},                                                     // browser overlay
	{Name: "raw", Description: "toggle raw scrollback mode for copy-friendly terminal selection", Source: commandSourceBuiltin},        // rendering-mode toggle the recipes/pane scrapes assume off
	{Name: "rename", Description: "rename the current thread", Source: commandSourceBuiltin},                                           // input overlay
	{Name: "resume", Description: "resume a saved chat", Source: commandSourceBuiltin},                                                 // picker; swaps transcript identity under lab
	{Name: "review", Description: "review my current changes and find issues", Source: commandSourceBuiltin},                           // review picker overlay
	{Name: "side", Description: "start a side conversation in an ephemeral fork", Source: commandSourceBuiltin},                        // side-conversation UI
	{Name: "skills", Description: "use skills to improve how Codex performs specific tasks", Source: commandSourceBuiltin},             // picker
	{Name: "statusline", Description: "configure which items appear in the status line", Source: commandSourceBuiltin},                 // config overlay
	{Name: "subagents", Description: "switch the active agent thread", Source: commandSourceBuiltin},                                   // thread picker (alias-shaped sibling of /agent)
	{Name: "theme", Description: "choose a syntax highlighting theme", Source: commandSourceBuiltin},                                   // picker
	{Name: "title", Description: "configure which items appear in the terminal title", Source: commandSourceBuiltin},                   // config overlay
	{Name: "vim", Description: "toggle Vim mode for the composer", Source: commandSourceBuiltin},                                       // composer-mode toggle; breaks the paste+Enter reply recipe
}

// BuiltinCommands returns a copy of the pinned builtin table, in pinned
// order — exported for the compat pin test.
func BuiltinCommands() []provider.CommandSpec {
	return append([]provider.CommandSpec(nil), builtinCommands...)
}

// Commands implements provider.AgentProvider (issue #51 decision 5): the
// pinned builtin table, worktree-independent — codex 0.133 has no project-
// or user-level command discovery (skills load only from the global
// $CODEX_HOME/skills, and a repo's .codex dir is not a command source), so
// the catalog is the same for every session.
func (p *Provider) Commands(_ context.Context, _ string) ([]provider.CommandSpec, error) {
	return BuiltinCommands(), nil
}
