package codex

// The slash-command catalog (issue #51 decision 5): a curated STATIC builtin
// table — codex discovers skills only globally ($CODEX_HOME/skills) and repo
// .codex dirs are not a command source, so there is no project/user command
// discovery. The rows are a fragile Codex coupling: names, descriptions and
// order are EXACTLY codex-cli 0.167.0's SlashCommand enum
// (codex-rs/tui/src/slash_command.rs at tag rust-v0.167.0 — the popup renders
// the enum in declaration order; source extraction 2026-11-10, the 0.133.0
// table was a live tmux scrape) filtered by is_visible() for a Linux release
// build, and curated row by row: ChatSafe=false marks a command that would
// strand the TUI in a picker/overlay lab cannot see (never-scrape means lab
// could neither render nor answer it), that the popup hides behind a
// feature/auth gate lab cannot assume (a hidden command typed into the
// composer is NOT dispatched as a command), or that fights lab's own
// management of the session (auth, lifecycle, cwd, transcript identity,
// composer/rendering modes the reply and interrupt recipes assume). The API
// layer filters on ChatSafe; this catalog stays complete and honest either
// way.

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// commandSourceBuiltin is the only Source codex serves (pinned API
// vocabulary, issue #51 decision 5) — no project/user discovery.
const commandSourceBuiltin = "builtin"

// builtinCommands is the pinned builtin table — codex-cli 0.167.0, source
// extraction 2026-11-10 (compat.md §7). Order is the pin: the chat-safe rows
// first in operator-usefulness order, then the curated-out rows
// alphabetically; Commands serves the table in exactly this order.
var builtinCommands = []provider.CommandSpec{
	// --- chat-safe: executes inline and returns to the prompt ---------------
	{Name: "clear", Description: "clear the terminal and start a new chat",
		Source: commandSourceBuiltin, Role: provider.CommandRoleClear, ChatSafe: true}, // AppEvent::ClearUi → start_fresh_session(ThreadStartSource::Clear): a fresh thread, a fresh rollout file, no picker — the clear/epoch identity (0.133 bound the role to /new, which now opens a worktree picker inside any git repo)
	{Name: "compact", Description: "summarize conversation to prevent hitting the context limit",
		Source: commandSourceBuiltin, ChatSafe: true}, // inline; same rollout file
	{Name: "status", Description: "show current session configuration and token usage",
		Source: commandSourceBuiltin, ChatSafe: true}, // inline panel
	{Name: "diff", Description: "show git diff (including untracked files)",
		Source: commandSourceBuiltin, ChatSafe: true}, // inline
	{Name: "mcp", Description: "list MCP tools; use /mcp verbose or /mcp login <name>",
		Source: commandSourceBuiltin, ChatSafe: true}, // bare /mcp is an inline list (ToolsAndAuthOnly); the /mcp login variant is the operator's choice to type
	{Name: "ps", Description: "list background terminals",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "stop", Description: "stop all background terminals",
		Source: commandSourceBuiltin, ChatSafe: true}, // inline action, returns to the prompt — a legitimate operator brake
	{Name: "pwd", Description: "show the current working directory",
		Source: commandSourceBuiltin, ChatSafe: true}, // inline info message (new in 0.167)
	{Name: "recap", Description: "summarize the current conversation now",
		Source: commandSourceBuiltin, ChatSafe: true}, // AppEvent::GenerateRecap: a model turn rendered inline, like /compact; refused inline while a turn runs (new in 0.167)
	{Name: "hooks", Description: "view and manage lifecycle hooks",
		Source: commandSourceBuiltin, ChatSafe: true}, // add_hooks_output: an inline listing on 0.167 (0.133 curated it out as a management overlay)
	{Name: "plugins", Description: "browse plugins",
		Source: commandSourceBuiltin, ChatSafe: true}, // add_plugins_output: an inline listing on 0.167, gated on the `plugins` feature which is stable+on (0.133 curated it out as a browser overlay)
	{Name: "debug-config", Description: "show config layers and requirement sources for debugging",
		Source: commandSourceBuiltin, ChatSafe: true}, // add_debug_config_output: inline (new in 0.167)
	// --- curated out: picker/overlay, feature-gated, or fights lab's session management (alphabetical)
	{Name: "agents", Description: "open the agent command center", Source: commandSourceBuiltin},                                       // overview overlay (replaces 0.133's /agent)
	{Name: "approve", Description: "approve one retry of a recent auto-review denial", Source: commandSourceBuiltin},                   // approval overlay; never-ask posture keeps these out anyway
	{Name: "apps", Description: "manage apps", Source: commandSourceBuiltin},                                                           // inline listing, but popup-gated on connectors_enabled — hidden commands are not dispatched
	{Name: "archive", Description: "archive this session", Source: commandSourceBuiltin},                                               // confirmation picker; session-ending
	{Name: "btw", Description: "start a side conversation in an ephemeral fork", Source: commandSourceBuiltin},                         // side-conversation UI (alias-shaped sibling of /side)
	{Name: "cd", Description: "change the current working directory", Source: commandSourceBuiltin},                                    // moves the session out of the lab worktree
	{Name: "config", Description: "configure Codex preferences", Source: commandSourceBuiltin},                                         // config panel overlay
	{Name: "copy", Description: "copy the last response or part of it", Source: commandSourceBuiltin},                                  // show_copy_picker: a picker on 0.167 (0.133's /copy was output-only and chat-safe)
	{Name: "daemon", Description: "Manage the local background server", Source: commandSourceBuiltin},                                  // daemon menu; lab spawns with --no-daemon
	{Name: "daybreak", Description: "turn Daybreak on or off", Source: commandSourceBuiltin},                                           // popup-gated on account eligibility; a toggle the recipes do not model
	{Name: "debug-m-drop", Description: "DO NOT USE", Source: commandSourceBuiltin},                                                    // upstream-marked debugging command
	{Name: "debug-m-update", Description: "DO NOT USE", Source: commandSourceBuiltin},                                                  // upstream-marked debugging command
	{Name: "delete", Description: "permanently delete this session", Source: commandSourceBuiltin},                                     // confirmation picker; destroys the transcript lab reads
	{Name: "exit", Description: "exit Codex", Source: commandSourceBuiltin},                                                            // session-ending — kills the session lab manages
	{Name: "experimental", Description: "toggle experimental features", Source: commandSourceBuiltin},                                  // toggle overlay
	{Name: "export", Description: "export the conversation as markdown", Source: commandSourceBuiltin},                                 // export popup
	{Name: "feedback", Description: "send logs to maintainers", Source: commandSourceBuiltin},                                          // external submit flow
	{Name: "fork", Description: "fork the current chat", Source: commandSourceBuiltin},                                                 // worktree picker in a git repo; forks transcript identity under lab
	{Name: "goal", Description: "set or view the goal for a long-running task", Source: commandSourceBuiltin},                          // popup-gated on the goals feature; unverified overlay behavior
	{Name: "ide", Description: "include current selection, open files, and other context from your IDE", Source: commandSourceBuiltin}, // needs an IDE attach lab has no surface for
	{Name: "import", Description: "import setup, this project, and recent chats from Claude Code", Source: commandSourceBuiltin},       // migration overlay
	{Name: "init", Description: "create an AGENTS.md file with instructions for Codex", Source: commandSourceBuiltin},                  // submits a turn that writes a repo-tracked AGENTS.md (ADR-0035 violation from inside the TUI)
	{Name: "keymap", Description: "remap TUI shortcuts", Source: commandSourceBuiltin},                                                 // overlay; remaps keys the recipes assume
	{Name: "logout", Description: "log out of Codex", Source: commandSourceBuiltin},                                                    // machine-level auth is lab's own surface
	{Name: "memories", Description: "configure memory use and generation", Source: commandSourceBuiltin},                               // config overlay
	{Name: "mention", Description: "mention a file", Source: commandSourceBuiltin},                                                     // inserts "@" and opens the file picker
	{Name: "model", Description: "choose what model and reasoning effort to use", Source: commandSourceBuiltin},                        // picker
	{Name: "new", Description: "start a new chat during a conversation", Source: commandSourceBuiltin},                                 // show_session_checkout_picker: inside any git repo (every lab worktree) a "Where should the new conversation run?" picker; /clear carries the clear role instead
	{Name: "permissions", Description: "choose what Codex is allowed to do", Source: commandSourceBuiltin},                             // picker; fights the pinned spawn posture
	{Name: "pets", Description: "choose or hide the terminal pet", Source: commandSourceBuiltin},                                       // picker
	{Name: "plan", Description: "switch to Plan mode", Source: commandSourceBuiltin},                                                   // popup-gated on collaboration modes; mode switch ending in an approval flow lab cannot see
	{Name: "presets", Description: "run or save reusable prompts and slash commands", Source: commandSourceBuiltin},                    // quick-dial menu
	{Name: "quit", Description: "exit Codex", Source: commandSourceBuiltin},                                                            // session-ending (alias-shaped sibling of /exit)
	{Name: "raw", Description: "toggle raw scrollback mode for copy-friendly terminal selection", Source: commandSourceBuiltin},        // rendering-mode toggle the recipes/pane scrapes assume off
	{Name: "rename", Description: "rename the current thread", Source: commandSourceBuiltin},                                           // input overlay
	{Name: "resume", Description: "resume a saved chat", Source: commandSourceBuiltin},                                                 // picker; swaps transcript identity under lab
	{Name: "review", Description: "review my current changes and find issues", Source: commandSourceBuiltin},                           // review picker overlay
	{Name: "setup-default-sandbox", Description: "set up elevated agent sandbox", Source: commandSourceBuiltin},                        // Windows-only setup flow; popup hides it on Linux
	{Name: "side", Description: "start a side conversation in an ephemeral fork", Source: commandSourceBuiltin},                        // side-conversation UI
	{Name: "skills", Description: "use skills to improve how Codex performs specific tasks", Source: commandSourceBuiltin},             // picker
	{Name: "statusline", Description: "configure which items appear in the status line", Source: commandSourceBuiltin},                 // config overlay
	{Name: "subagents", Description: "switch between this session's subagents", Source: commandSourceBuiltin},                          // agent picker
	{Name: "theme", Description: "choose a syntax highlighting theme", Source: commandSourceBuiltin},                                   // picker
	{Name: "title", Description: "configure which items appear in the terminal title", Source: commandSourceBuiltin},                   // config overlay
	{Name: "tui", Description: "choose the TUI mode for the next launch", Source: commandSourceBuiltin},                                // picker
	{Name: "usage", Description: "view account usage or use a usage limit reset", Source: commandSourceBuiltin},                        // usage menu; popup-gated on ChatGPT-backend auth
	{Name: "vim", Description: "toggle Vim mode for the composer", Source: commandSourceBuiltin},                                       // composer-mode toggle; breaks the paste+Enter reply recipe
	{Name: "voice", Description: "start or stop voice; use /voice settings to choose a voice", Source: commandSourceBuiltin},           // popup-gated on realtime availability; audio surface lab has none of
	{Name: "warnings", Description: "view retained warnings and diagnostic details", Source: commandSourceBuiltin},                     // warnings overlay
	{Name: "worktree", Description: "start or continue a conversation in a new worktree", Source: commandSourceBuiltin},                // worktree picker; moves the session out of the lab worktree
}

// BuiltinCommands returns a copy of the pinned builtin table, in pinned
// order — exported for the compat pin test.
func BuiltinCommands() []provider.CommandSpec {
	return append([]provider.CommandSpec(nil), builtinCommands...)
}

// Commands implements provider.AgentProvider (issue #51 decision 5): the
// pinned builtin table, worktree- AND home-independent — codex (0.133 and
// 0.167 alike) has no project- or user-level command discovery (skills load only from the global
// codex home's skills dir, and a repo's .codex dir is not a command source), so
// there is nothing under the instance HOME to scan and the catalog is the same
// for every session. worktree and home (issue #202) are accepted for the seam
// and unused.
func (p *Provider) Commands(_ context.Context, _, _ string) ([]provider.CommandSpec, error) {
	return BuiltinCommands(), nil
}
