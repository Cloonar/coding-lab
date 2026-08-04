package claudecode

// The slash-command catalog (issue #51 decision 5): the pinned builtin table
// merged with scans of the worktree's project commands, the seeded skills,
// and the user-level command dir. The builtin rows are a fragile Claude Code
// coupling pinned in internal/compat §10 — extracted from the installed
// 2.1.198 bundle's embedded JS (descriptions/argHints VERBATIM, primary names
// only; aliases are documented in compat, never served) and curated row by
// row: ChatSafe=false marks a command that would strand the TUI in a
// picker/editor/UI lab cannot see (never-scrape means lab could neither
// render nor answer it), or that fights lab's own management of the session
// (auth, lifecycle, transcript identity). The API layer filters on ChatSafe;
// this catalog stays complete and honest either way.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// CommandSpec.Source values (pinned API vocabulary, issue #51 decision 5).
const (
	commandSourceBuiltin = "builtin"
	commandSourceProject = "project"
	commandSourceUser    = "user"
)

// builtinCommands is the pinned builtin table — Claude Code 2.1.198, bundle
// extraction 2026-07-08 (compat §10 documents the method, the per-row
// chat-safe verdicts with reasons, the aliases, and the env-dependent
// description cases where the pinned text is the bundle's default branch).
// Order is the pin: the ten chat-safe rows first in operator-usefulness
// order, then the curated-out rows alphabetically; Commands serves the table
// in exactly this order (builtins never re-sort).
var builtinCommands = []provider.CommandSpec{
	// --- chat-safe: executes inline and returns to the prompt ---------------
	{Name: "clear", Description: "Start a new session with empty context; previous session stays on disk (resumable with /resume)",
		ArgHint: "[name]", Source: commandSourceBuiltin, Role: provider.CommandRoleClear, ChatSafe: true},
	{Name: "compact", Description: "Free up context by summarizing the conversation so far",
		ArgHint: "<optional custom summarization instructions>", Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "context", Description: "Visualize current context usage as a colored grid",
		ArgHint: "[all]", Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "usage", Description: "Show session cost, plan usage, and activity stats",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "status", Description: "Show Claude Code status including version, model, account, API connectivity, and tool statuses",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "export", Description: "Export the current conversation to a file or clipboard",
		ArgHint: "[filename]", Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "release-notes", Description: "View release notes",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "init", Description: "Initialize a new CLAUDE.md file with codebase documentation",
		Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "review", Description: "Review a GitHub pull request; for your working diff use /code-review",
		ArgHint: "[pr number]", Source: commandSourceBuiltin, ChatSafe: true},
	{Name: "security-review", Description: "Complete a security review of the pending changes on the current branch",
		Source: commandSourceBuiltin, ChatSafe: true},
	// --- curated out (ChatSafe=false): reasons per row in compat §10 --------
	{Name: "add-dir", Description: "Add a new working directory", ArgHint: "<path>", Source: commandSourceBuiltin},
	{Name: "agents", Description: "(removed) Ask Claude to create/manage subagents, or edit .claude/agents/", Source: commandSourceBuiltin},
	{Name: "config", Description: "Open settings", ArgHint: "[key=value]", Source: commandSourceBuiltin},
	// doctor's operator-facing text moved to the bundle's `menuDescription`
	// key (its `description` is now a long model-facing prompt); the
	// menuDescription is what the TUI menu shows, so it is what this row
	// pins. Drift found 2026-08-04, present identically on 2.1.220 — compat §10.
	{Name: "doctor", Description: "Health-check your setup and fix issues: installation, unused extensions, duplicated or bloated memory files, slow hooks, updates, permissions", Source: commandSourceBuiltin},
	{Name: "exit", Description: "Exit the CLI", Source: commandSourceBuiltin},
	{Name: "feedback", Description: "Send feedback to Anthropic or report a bug", ArgHint: "[report]", Source: commandSourceBuiltin},
	{Name: "hooks", Description: "View hook configurations for tool events", Source: commandSourceBuiltin},
	{Name: "ide", Description: "Manage IDE integrations and show status", ArgHint: "[open]", Source: commandSourceBuiltin},
	{Name: "install-github-app", Description: "Set up Claude GitHub Actions for a repository", Source: commandSourceBuiltin},
	{Name: "login", Description: "Sign in with your Anthropic account", Source: commandSourceBuiltin},
	{Name: "logout", Description: "Sign out from your Anthropic account", Source: commandSourceBuiltin},
	{Name: "mcp", Description: "Manage MCP servers", ArgHint: "[reconnect <server>|enable|disable [<server>|all]]", Source: commandSourceBuiltin},
	{Name: "memory", Description: "Open a memory file in your editor", Source: commandSourceBuiltin},
	{Name: "model", Description: "Set the AI model for Claude Code", ArgHint: "<model>", Source: commandSourceBuiltin},
	{Name: "permissions", Description: "Manage allow and deny tool permission rules", Source: commandSourceBuiltin},
	{Name: "plan", Description: "Enable plan mode or view the current session plan", ArgHint: "[open|share|<description>]", Source: commandSourceBuiltin},
	{Name: "privacy-settings", Description: "View and update your privacy settings", Source: commandSourceBuiltin},
	{Name: "resume", Description: "Resume a previous conversation", ArgHint: "[conversation id or search term]", Source: commandSourceBuiltin},
	{Name: "rewind", Description: "Restore the code and/or conversation to a previous point", Source: commandSourceBuiltin},
	{Name: "statusline", Description: "Set up Claude Code's status line UI", Source: commandSourceBuiltin},
	{Name: "terminal-setup", Description: "Install Shift+Enter key binding for newlines", Source: commandSourceBuiltin},
	{Name: "upgrade", Description: "Upgrade to Max for higher rate limits and more Opus", Source: commandSourceBuiltin},
	{Name: "usage-credits", Description: "Configure usage credits or request them from your admin when you hit a limit", Source: commandSourceBuiltin},
}

// BuiltinCommands returns a copy of the pinned builtin table, in pinned order
// — exported for the compat pin test (§10).
func BuiltinCommands() []provider.CommandSpec {
	return append([]provider.CommandSpec(nil), builtinCommands...)
}

// Commands implements provider.AgentProvider (issue #51 decision 5): the
// pinned builtin table, then the worktree's project-level commands
// (<worktree>/.claude/commands/*.md) merged with its seeded user-invocable
// skills (<worktree>/.claude/skills/*/SKILL.md — the lab-seeded bundle plus
// anything repo-committed), then the USER-level commands discovered under the
// instance HOME home (<home>/.claude/commands, issue #202). Builtins keep the
// pinned order; the project and user groups sort alphabetically. Scanned
// commands are ChatSafe by construction: a custom command/skill is a prompt
// template — claude runs it as a normal turn, there is no UI to strand.
//
// An empty home contributes NO user-level commands — a run with no per-run
// home never reads the master store's ~/.claude/commands (isolation by
// construction). Missing directories are silently empty (a fresh worktree has
// no commands yet; most instance homes have no .claude/commands); only a real
// I/O failure surfaces as an error. Unparseable frontmatter degrades to an
// empty description, never an error — the name alone is still a servable
// command.
func (p *Provider) Commands(_ context.Context, worktree, home string) ([]provider.CommandSpec, error) {
	out := BuiltinCommands()

	project, err := scanCommandsDir(filepath.Join(worktree, ".claude", "commands"), commandSourceProject)
	if err != nil {
		return nil, err
	}
	skills, err := scanSkillsDir(filepath.Join(worktree, ".claude", "skills"))
	if err != nil {
		return nil, err
	}
	project = append(project, skills...)
	sortCommands(project)
	out = append(out, project...)

	if home == "" {
		return out, nil // no per-run home ⇒ no user-level command discovery
	}
	user, err := scanCommandsDir(userCommandsDirUnder(home), commandSourceUser)
	if err != nil {
		return nil, err
	}
	sortCommands(user)
	return append(out, user...), nil
}

func sortCommands(cmds []provider.CommandSpec) {
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
}

// scanCommandsDir reads a custom-command directory: every *.md file is one
// command named after its basename (claude's own rule), with description and
// argument-hint lifted from YAML frontmatter when trivially present
// (parseFrontmatter). Source is caller-supplied (project|user).
func scanCommandsDir(dir, source string) ([]provider.CommandSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []provider.CommandSpec
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if name == "" {
			continue
		}
		spec := provider.CommandSpec{Name: name, Source: source, ChatSafe: true}
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			fm := parseFrontmatter(b)
			spec.Description = fm.description
			spec.ArgHint = fm.argHint
		}
		out = append(out, spec)
	}
	return out, nil
}

// scanSkillsDir reads a skills tree (<dir>/<skill>/SKILL.md): every skill is
// user-invocable as /<name> unless its frontmatter opts out with
// `user-invocable: false` — the 2.1.198 bundle schema (verbatim): "If false,
// hides the slash command from users; only the model can invoke it via the
// Skill tool." (compat §10). The name comes from the frontmatter `name` key
// (the schema's "slash command name"), falling back to the directory name —
// they coincide across the entire seeded bundle.
func scanSkillsDir(dir string) ([]provider.CommandSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []provider.CommandSpec
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err != nil {
			continue // not a skill dir (or unreadable) — skip, never fail the catalog
		}
		fm := parseFrontmatter(b)
		if !fm.userInvocable {
			continue
		}
		name := fm.name
		if name == "" {
			name = e.Name()
		}
		out = append(out, provider.CommandSpec{
			Name: name, Description: fm.description, ArgHint: fm.argHint,
			Source: commandSourceProject, ChatSafe: true,
		})
	}
	return out, nil
}

// frontmatter is the subset of command/skill YAML frontmatter the catalog
// reads: name, description, argument-hint, user-invocable (default true).
type frontmatter struct {
	name          string
	description   string
	argHint       string
	userInvocable bool
}

// parseFrontmatter extracts the catalog's keys from a leading `---` YAML
// block, deliberately minimal (no YAML dependency): single-line `key: value`
// pairs plus the folded/literal block form (`key: >` / `key: |`) the seeded
// bundle actually uses (assets/skills/caveman), whose indented continuation
// lines join with spaces. Anything fancier degrades to empty values — the
// catalog is autocomplete metadata, not a parser contract.
func parseFrontmatter(b []byte) frontmatter {
	fm := frontmatter{userInvocable: true}
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return fm
	}
	set := func(key, val string) {
		switch key {
		case "name":
			fm.name = val
		case "description":
			fm.description = val
		case "argument-hint":
			fm.argHint = val
		case "user-invocable":
			fm.userInvocable = val != "false"
		}
	}
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "---" || line == "..." {
			break
		}
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue // continuation lines are consumed by the block scan below
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if val == ">" || val == "|" || val == ">-" || val == "|-" {
			// Folded/literal block: join the indented continuation lines.
			var parts []string
			for j := i + 1; j < len(lines); j++ {
				cont := strings.TrimRight(lines[j], "\r")
				if cont == "" || (cont[0] != ' ' && cont[0] != '\t') {
					break
				}
				parts = append(parts, strings.TrimSpace(cont))
			}
			set(key, strings.Join(parts, " "))
			continue
		}
		set(key, strings.Trim(val, `"'`))
	}
	return fm
}
