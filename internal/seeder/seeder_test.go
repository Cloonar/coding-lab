package seeder

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/assets"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// builtinRepo/forgeRepo are the two binding shapes the context file renders.
var (
	builtinRepo = store.Repo{Name: "proj", TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none"}
	forgeRepo   = store.Repo{Name: "proj", TrackerBinding: store.TrackerBindingForge, ForgeKind: "forgejo"}
)

// claudeGoldenMeta is claude-code's seed declaration, duplicated here as a
// GOLDEN LITERAL (issue #51 decision 8 byte-identity acceptance). The seeder is
// generic — these tests prove that GIVEN today's claude shapes it produces
// today's exact output. The seeder must NOT import claudecode, so the values
// are inlined; a drift in claudecode.SeedMeta() is caught separately by
// claudecode's own seedmeta_test.go, which pins the real declaration to the
// same literals. Any divergence between the two fails one side or the other.
var claudeGoldenMeta = provider.SeedMeta{
	ContextFileName: "CLAUDE.local.md",
	SkillsDir:       ".claude/skills",
	ExcludeEntries:  []string{".claude/", "CLAUDE.local.md"},
	SeededPathPatterns: []string{
		`^\.claude/skills/`,
		`^\.claude/settings\.local\.json$`,
		`^CLAUDE\.local\.md$`,
	},
	ScrubPatterns: []string{
		`co-authored-by:[[:space:]]*claude`,
		`co-authored-by:.*<[^>]*@anthropic\.com>`,
		`generated with.*claude`,
		`claude-session:`,
	},
}

// newWorktree builds a REAL linked worktree (bare-style main checkout +
// `git worktree add`), the shape every Launch seeds: its .git is a gitdir
// pointer whose commondir leads back to the shared git dir, so the exclude
// write must land there for git status to honor it.
func newWorktree(t *testing.T) (wt string, env []string) {
	t.Helper()
	testutil.RequireTool(t, "git")
	root := t.TempDir()
	env = append(os.Environ(), testutil.HermeticGitEnv(root)...)
	repo := filepath.Join(root, "repo")
	git(t, env, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, env, "-C", repo, "add", "f.txt")
	git(t, env, "-C", repo, "commit", "-q", "-m", "init")
	wt = filepath.Join(root, "wt")
	git(t, env, "-C", repo, "worktree", "add", "-q", "-b", "lab/x-1", wt)
	return wt, env
}

func git(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func seed(t *testing.T, wt string, repo store.Repo) {
	t.Helper()
	if err := New().SeedWorkspace(wt, repo, claudeGoldenMeta, Opts{}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Every file of the embedded bundle lands byte-identical under
// <worktree>/.claude/skills/.
func TestSeedWorkspace_skillsBundleLandsVerbatim(t *testing.T) {
	wt, _ := newWorktree(t)
	seed(t, wt, builtinRepo)

	bundle, err := fs.Sub(assets.Skills, "skills")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	err = fs.WalkDir(bundle, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		want, err := fs.ReadFile(bundle, p)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(wt, ".claude", "skills", filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("seeded skill missing: %v", err)
			return nil
		}
		if string(got) != string(want) {
			t.Errorf("seeded %s differs from the embedded bundle", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking bundle: %v", err)
	}
	if files == 0 {
		t.Fatal("embedded skills bundle is empty — go:embed all:skills broken")
	}
	// Spot-check the bundle carries the expected skills at all. land-pr is the
	// cloonar-local skill (not from upstream) — guard it against an upstream bump
	// that replaces the mattpocock dirs wholesale and forgets to keep it.
	for _, name := range []string{"tdd", "triage", "to-issues", "land-pr"} {
		if _, err := os.Stat(filepath.Join(wt, ".claude", "skills", name, "SKILL.md")); err != nil {
			t.Errorf("skill %s not seeded: %v", name, err)
		}
	}
}

// CLAUDE.local.md carries one golden-ish assertion per pinned section: the
// binding line, the labctl vocabulary + provided env, the five-label table
// with meanings, and the supersedes-tea/gh note.
func TestSeedWorkspace_claudeLocalSections(t *testing.T) {
	wt, _ := newWorktree(t)
	seed(t, wt, builtinRepo)
	got := readFile(t, filepath.Join(wt, "CLAUDE.local.md"))

	wantContains := []string{
		// Tracker binding + what it means (builtin → change request, lab UI).
		"tracker binding is **builtin**",
		"**change request**",
		// labctl vocabulary (§8.3, incl. the ADR-0014 triage set) + provided
		// credentials.
		"labctl issue view [n]",
		"labctl issue list",
		"labctl issue create --title T --body B [--labels a,b]",
		"labctl issue comment <n> <body>",
		"labctl issue label add <n> <a,b>",
		"labctl issue label remove <n> <a,b>",
		"labctl issue close <n>",
		"labctl label list",
		"labctl label create --name N [--color C --description D]",
		"labctl pr create --title T --body B",
		"`LAB_URL` and `LAB_TOKEN` are already provided",
		"Closes #<n>",
		// Five-triage-label table with meanings.
		"`needs-triage`",
		"`needs-info`",
		"`ready-for-agent`",
		"`ready-for-human`",
		"`wontfix`",
		"Fully specified, ready for an AFK agent",
		// The explicit supersedes note.
		"supersedes any committed `tea` or `gh` instructions",
	}
	for _, want := range wantContains {
		if !strings.Contains(got, want) {
			t.Errorf("CLAUDE.local.md missing %q", want)
		}
	}
}

func TestRenderContextFile_forgeBinding(t *testing.T) {
	body, err := renderContextFile(forgeRepo)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "tracker binding is **forge** (forgejo)") {
		t.Errorf("forge render missing the binding line:\n%s", got)
	}
	if strings.Contains(got, "**change request**") {
		t.Error("forge render mentions change requests — that is the builtin binding's object")
	}
}

// The exclude entries land in the SHARED git dir (commondir resolution), git
// status shows NOTHING in a fully seeded worktree, and re-seeding twice more
// neither duplicates exclude lines nor dirties anything.
func TestSeedWorkspace_excludedFromStatusAndDeduped(t *testing.T) {
	wt, env := newWorktree(t)
	for range 3 {
		seed(t, wt, builtinRepo)
	}

	if out := git(t, env, "-C", wt, "status", "--porcelain"); out != "" {
		t.Errorf("git status after seeding = %q; want empty (everything lab seeds is excluded)", out)
	}
	path, err := GitExcludePath(wt)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(readFile(t, path), "\n")
	for _, entry := range claudeGoldenMeta.ExcludeEntries {
		n := 0
		for _, l := range lines {
			if l == entry {
				n++
			}
		}
		if n != 1 {
			t.Errorf("exclude entry %q appears %d times after 3 seeds; want exactly 1", entry, n)
		}
	}
}

// Re-seeding overwrites lab's files (healing drift) but never deletes files
// a user added under the seeded tree.
func TestSeedWorkspace_reseedOverwritesLabFilesKeepsUserFiles(t *testing.T) {
	wt, _ := newWorktree(t)
	seed(t, wt, builtinRepo)

	labFile := filepath.Join(wt, ".claude", "skills", "tdd", "SKILL.md")
	if err := os.WriteFile(labFile, []byte("drifted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(wt, ".claude", "skills", "my-own", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(userFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userFile, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	seed(t, wt, builtinRepo)

	want, err := assets.Skills.ReadFile("skills/tdd/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, labFile); got != string(want) {
		t.Error("re-seed did not restore lab's drifted skill file to the embedded content")
	}
	if got := readFile(t, userFile); got != "mine\n" {
		t.Errorf("re-seed touched a user-added file: %q", got)
	}
}

// A dir without .git fails loud BEFORE anything is written (excludes run
// first, and they need the git dir).
func TestSeedWorkspace_missingGitFailsLoudAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := New().SeedWorkspace(dir, builtinRepo, claudeGoldenMeta, Opts{}); err == nil {
		t.Fatal("SeedWorkspace on a dir with no .git succeeded; want an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.local.md")); err == nil {
		t.Error("CLAUDE.local.md written despite the seed failing")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); err == nil {
		t.Error(".claude/ created despite the seed failing")
	}
}

// Empty-field semantics (issue #51 decision 8): a provider that declares no
// skills dir and no context file gets neither — but its exclude entries still
// apply. seedSkills/seedContextFile skip on empty, so a minimal provider is a
// no-op beyond excludes rather than a crash or a stray .claude/.
func TestSeedWorkspace_emptyMetaSkipsSkillsAndContextFile(t *testing.T) {
	wt, _ := newWorktree(t)
	meta := provider.SeedMeta{ExcludeEntries: []string{"scratch/"}}
	if err := New().SeedWorkspace(wt, builtinRepo, meta, Opts{}); err != nil {
		t.Fatalf("SeedWorkspace with empty skills/context meta: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude")); err == nil {
		t.Error(".claude/ created for a provider declaring no skills dir")
	}
	if _, err := os.Stat(filepath.Join(wt, "CLAUDE.local.md")); err == nil {
		t.Error("context file created for a provider declaring no context name")
	}
	// The declared exclude entry still lands.
	path, err := GitExcludePath(wt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "scratch/") {
		t.Error("declared exclude entry not applied")
	}
}

// Even a provider that declares NO exclude entries still fails loud on a
// non-git dir before writing anything: EnsureExcludes resolves the git dir up
// front regardless of entry count, preserving the write-nothing-on-bad-dir
// contract independent of the meta.
func TestSeedWorkspace_emptyExcludesStillFailsLoudOnMissingGit(t *testing.T) {
	dir := t.TempDir()
	meta := provider.SeedMeta{SkillsDir: ".claude/skills", ContextFileName: "CLAUDE.local.md"}
	if err := New().SeedWorkspace(dir, builtinRepo, meta, Opts{}); err == nil {
		t.Fatal("SeedWorkspace on a non-git dir with empty ExcludeEntries succeeded; want an error")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); err == nil {
		t.Error(".claude/ created despite the seed failing on a non-git dir")
	}
}
