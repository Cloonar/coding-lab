package claudecode

// Commands catalog tests (issue #51 decision 5): the merge/order/curation
// behavior over injectable dirs. The builtin TABLE itself (verbatim
// descriptions, chat-safe verdicts, the clear role) is pinned in
// internal/compat (§10) — here we test the machinery around it.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
)

func commandsProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Options{
		ClaudeBin: "claude", ConfigPath: filepath.Join(t.TempDir(), ".claude.json"),
		LoginDir: t.TempDir(),
		Runner:   newFakeRunner(), Bus: events.NewBus(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCommands_mergesBuiltinsProjectSkillsUser(t *testing.T) {
	worktree := t.TempDir()
	home := t.TempDir()

	// Project commands: frontmatter-described, bare, and one with args.
	writeFile(t, filepath.Join(worktree, ".claude", "commands", "deploy.md"),
		"---\ndescription: Deploy the current branch\nargument-hint: <env>\n---\nDo the deploy.\n")
	writeFile(t, filepath.Join(worktree, ".claude", "commands", "bare.md"), "No frontmatter here.\n")
	// Seeded skills: the bundle shape (plain and folded descriptions), one
	// hidden from users, one non-skill dir to skip.
	writeFile(t, filepath.Join(worktree, ".claude", "skills", "triage", "SKILL.md"),
		"---\nname: triage\ndescription: Triage issues through a state machine driven by triage roles.\n---\nbody\n")
	writeFile(t, filepath.Join(worktree, ".claude", "skills", "caveman", "SKILL.md"),
		"---\nname: caveman\ndescription: >\n  Ultra-compressed communication mode. Cuts token usage ~75% by dropping\n  filler.\n---\nbody\n")
	writeFile(t, filepath.Join(worktree, ".claude", "skills", "internal-helper", "SKILL.md"),
		"---\nname: internal-helper\ndescription: model-only\nuser-invocable: false\n---\nbody\n")
	writeFile(t, filepath.Join(worktree, ".claude", "skills", "not-a-skill", "README.md"), "not a skill\n")
	// User-level command, under the instance HOME (issue #202).
	writeFile(t, filepath.Join(userCommandsDirUnder(home), "zz-mine.md"), "---\ndescription: My user command\n---\nbody\n")

	p := commandsProvider(t)
	cmds, err := p.Commands(context.Background(), worktree, home)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}

	// Builtins first, in pinned order, exactly the pinned table.
	builtins := BuiltinCommands()
	if len(cmds) < len(builtins) {
		t.Fatalf("catalog has %d entries; want at least the %d builtins", len(cmds), len(builtins))
	}
	for i, want := range builtins {
		if cmds[i] != want {
			t.Fatalf("builtin %d = %+v; want %+v (pinned order)", i, cmds[i], want)
		}
	}

	// Then the project group (commands + skills merged), alpha within group.
	rest := cmds[len(builtins):]
	wantProject := []string{"bare", "caveman", "deploy", "triage"}
	if len(rest) != len(wantProject)+1 {
		t.Fatalf("non-builtin entries = %+v; want %d project + 1 user", rest, len(wantProject))
	}
	for i, name := range wantProject {
		got := rest[i]
		if got.Name != name || got.Source != "project" || !got.ChatSafe {
			t.Errorf("project entry %d = %+v; want chat-safe project %q", i, got, name)
		}
	}
	if deploy := rest[2]; deploy.Description != "Deploy the current branch" || deploy.ArgHint != "<env>" {
		t.Errorf("deploy = %+v; want frontmatter description + argument-hint", deploy)
	}
	if caveman := rest[1]; caveman.Description != "Ultra-compressed communication mode. Cuts token usage ~75% by dropping filler." {
		t.Errorf("caveman description = %q; want the folded frontmatter joined with spaces", caveman.Description)
	}
	if bare := rest[0]; bare.Description != "" || bare.ArgHint != "" {
		t.Errorf("bare = %+v; want empty metadata (no frontmatter)", bare)
	}

	// User group last. The user-invocable:false skill never appears.
	if user := rest[len(rest)-1]; user.Name != "zz-mine" || user.Source != "user" || user.Description != "My user command" {
		t.Errorf("user entry = %+v; want zz-mine from the user dir", user)
	}
	for _, c := range cmds {
		if c.Name == "internal-helper" {
			t.Error("a user-invocable:false skill leaked into the catalog")
		}
	}
}

// Missing dirs are silently empty: a fresh worktree (no .claude at all) and an
// instance HOME without .claude/commands still get the builtin table.
func TestCommands_missingDirsAreEmpty(t *testing.T) {
	p := commandsProvider(t)
	cmds, err := p.Commands(context.Background(), filepath.Join(t.TempDir(), "fresh"), filepath.Join(t.TempDir(), "no-home"))
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	if len(cmds) != len(BuiltinCommands()) {
		t.Errorf("fresh worktree catalog = %d entries; want just the %d builtins", len(cmds), len(BuiltinCommands()))
	}
}

// An empty home contributes NO user-level commands — a run with no per-run home
// never reads the master store's ~/.claude/commands (issue #202).
func TestCommands_emptyHomeSkipsUserCommands(t *testing.T) {
	worktree := t.TempDir()
	home := t.TempDir()
	writeFile(t, filepath.Join(userCommandsDirUnder(home), "mine.md"), "---\ndescription: user\n---\nbody\n")
	p := commandsProvider(t)

	withHome, err := p.Commands(context.Background(), worktree, home)
	if err != nil {
		t.Fatalf("Commands(home): %v", err)
	}
	homeless, err := p.Commands(context.Background(), worktree, "")
	if err != nil {
		t.Fatalf("Commands(\"\"): %v", err)
	}
	if len(withHome) != len(homeless)+1 {
		t.Errorf("home vs homeless counts = %d vs %d; want exactly one user command dropped when home is empty", len(withHome), len(homeless))
	}
	for _, c := range homeless {
		if c.Source == commandSourceUser {
			t.Errorf("homeless catalog carried a user command %+v; want none", c)
		}
	}
}

// The catalog is complete-and-honest: unsafe builtins ARE returned (the API
// layer filters on ChatSafe) and every scanned entry is ChatSafe.
func TestCommands_chatSafeIsTheCurationSignal(t *testing.T) {
	p := commandsProvider(t)
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

func TestParseFrontmatter_edges(t *testing.T) {
	cases := map[string]struct {
		in   string
		want frontmatter
	}{
		"no frontmatter":  {"just text\n", frontmatter{userInvocable: true}},
		"unclosed block":  {"---\ndescription: x\n", frontmatter{description: "x", userInvocable: true}},
		"quoted value":    {"---\ndescription: \"quoted\"\n---\n", frontmatter{description: "quoted", userInvocable: true}},
		"literal block":   {"---\ndescription: |\n  line one\n  line two\n---\n", frontmatter{description: "line one line two", userInvocable: true}},
		"unknown keys ok": {"---\nallowed-tools: Bash\nname: n\n---\n", frontmatter{name: "n", userInvocable: true}},
		"argument hint":   {"---\nargument-hint: [pr]\n---\n", frontmatter{argHint: "[pr]", userInvocable: true}},
		"not invocable":   {"---\nuser-invocable: false\n---\n", frontmatter{userInvocable: false}},
	}
	for name, c := range cases {
		if got := parseFrontmatter([]byte(c.in)); got != c.want {
			t.Errorf("%s: parseFrontmatter = %+v; want %+v", name, got, c.want)
		}
	}
}
