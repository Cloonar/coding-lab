package seeder

import (
	"bytes"
	"fmt"
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
	ContextFileName:      "CLAUDE.local.md",
	SkillsDir:            ".claude/skills",
	NativeSkillDiscovery: true,
	ExcludeEntries:       []string{".claude/", "CLAUDE.local.md"},
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

// testSecrets is the repo secret metadata fixture (issue #104) used by the
// Secrets-section tests: already name-sorted, one with a description and one
// without, so both bullet shapes get exercised. Metadata only, as the store
// itself only ever hands the seeder — no value field exists on store.RepoSecret.
var testSecrets = []store.RepoSecret{
	{Name: "API_KEY", Description: "Widget API token"},
	{Name: "DEPLOY_TOKEN"},
}

// testImports is the read-only-imports fixture (issue #261) used by the
// Read-only-imports-section tests: two snapshots, already ordered by name as
// the real caller (the launch path, another agent's slice) is documented to
// hand them, with realistic absolute snapshot paths under an instance's state
// dir.
var testImports = []ImportRef{
	{Name: "api-server", Path: "/var/lib/lab/state/instances/run_x/imports/api-server", Commit: "1234567890ab"},
	{Name: "proto-defs", Path: "/var/lib/lab/state/instances/run_x/imports/proto-defs", Commit: "abcdef012345"},
}

// testGatewayServices is the granted-service fixture (issue #24 / ADR-0067):
// the services this repo's OneCLI agent identity holds grants for, in the
// order the caller resolved them — deliberately NOT alphabetical, so the
// tests pin that the render preserves the caller's order instead of imposing
// one of its own. Names only, because a GatewayRef carries nothing else: the
// seeder never sees a proxy URL, a proxy token, or a secret value.
var testGatewayServices = []string{"stripe", "api.github.com"}

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
	body, err := renderContextFile(forgeRepo, claudeGoldenMeta, nil, nil, nil)
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

// readGolden reads a byte-identity golden from testdata. These files were
// captured from HEAD before issue #79 (the native-discovery byte-identity
// contract) — never regenerate or edit them.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// byteDiff is a compact, diff-worthy mismatch report: lengths plus the full got
// when it is short enough to eyeball, else the first differing offset.
func byteDiff(got, want []byte) string {
	if len(got) < 512 {
		return fmt.Sprintf("got %d bytes, want %d bytes; got:\n%s", len(got), len(want), got)
	}
	off := 0
	for off < len(got) && off < len(want) && got[off] == want[off] {
		off++
	}
	return fmt.Sprintf("got %d bytes, want %d bytes; first differ at offset %d", len(got), len(want), off)
}

// Byte-identity acceptance for claude's native-discovery shape (issue #79 /
// ADR-0035): because claude declares NativeSkillDiscovery: true, renderContextFile
// appends NO skills index, so its output must equal the pre-change goldens
// captured from HEAD byte-for-byte. A failure means the template render drifted —
// the native path must never gain a byte the goldens do not have. The
// strings.Contains guard makes the "no index for native discovery" promise
// self-documenting beyond the raw byte compare.
func TestRenderContextFile_claudeGoldenByteIdentity(t *testing.T) {
	cases := []struct {
		name   string
		repo   store.Repo
		golden string
	}{
		{"builtin", builtinRepo, "contextfile-claude-builtin.golden"},
		{"forge", forgeRepo, "contextfile-claude-forge.golden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderContextFile(tc.repo, claudeGoldenMeta, nil, nil, nil)
			if err != nil {
				t.Fatalf("renderContextFile: %v", err)
			}
			if want := readGolden(t, tc.golden); !bytes.Equal(got, want) {
				t.Errorf("render != %s\n%s", tc.golden, byteDiff(got, want))
			}
			if strings.Contains(string(got), "## Seeded skills") {
				t.Error("native-discovery render contains a `## Seeded skills` section; want none")
			}
			if strings.Contains(string(got), "## Secrets") {
				t.Error("zero-secrets render contains a `## Secrets` section; want none")
			}
		})
	}
}

// The full seed path (not just the in-memory render) writes the claude golden to
// disk byte-for-byte — one binding is enough for the on-disk variant.
func TestSeedWorkspace_onDiskMatchesGolden(t *testing.T) {
	wt, _ := newWorktree(t)
	seed(t, wt, builtinRepo)
	got, err := os.ReadFile(filepath.Join(wt, "CLAUDE.local.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, "contextfile-claude-builtin.golden"); !bytes.Equal(got, want) {
		t.Errorf("on-disk CLAUDE.local.md != golden\n%s", byteDiff(got, want))
	}
}

// codexMeta is a FAKE non-native-discovery provider's declaration (issue #79 /
// ADR-0035): a codex-shaped agent reads AGENTS.local.md but does not discover
// .codex/skills on its own, so the generic seeder must append the generated
// skills index for it. Not a real provider — a shape to exercise the non-native
// path end to end without importing any concrete adapter.
var codexMeta = provider.SeedMeta{
	ContextFileName:      "AGENTS.local.md",
	SkillsDir:            ".codex/skills",
	NativeSkillDiscovery: false,
	ExcludeEntries:       []string{".codex/", "AGENTS.local.md"},
}

// Non-native acceptance: for a provider that seeds skills but does not discover
// them natively, the bundle lands under its own skills dir AND the context file
// grows a generated index — one bullet per bundled skill, each pointing at the
// correct per-provider seeded path — with everything still hidden from git status.
func TestSeedWorkspace_nonNativeAppendsSkillsIndex(t *testing.T) {
	wt, env := newWorktree(t)
	if err := New().SeedWorkspace(wt, forgeRepo, codexMeta, Opts{}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}

	// The bundle landed under the codex-shaped skills dir (spot-check tdd).
	if _, err := os.Stat(filepath.Join(wt, ".codex", "skills", "tdd", "SKILL.md")); err != nil {
		t.Errorf("skills bundle not seeded under .codex/skills: %v", err)
	}

	got := readFile(t, filepath.Join(wt, "AGENTS.local.md"))
	if !strings.Contains(got, "## Seeded skills") {
		t.Fatal("AGENTS.local.md missing the `## Seeded skills` heading")
	}
	// The index is APPENDED, not replacing — the base sections survive.
	if !strings.Contains(got, "labctl issue view [n]") {
		t.Error("appended index clobbered the base labctl vocabulary section")
	}

	// Every top-level bundle directory gets exactly one bullet pointing at its
	// correct per-provider seeded path — walked from the bundle, no hardcoded list.
	bundle, err := fs.Sub(assets.Skills, "skills")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := fs.ReadDir(bundle, ".")
	if err != nil {
		t.Fatal(err)
	}
	nDirs := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		nDirs++
		want := "read `.codex/skills/" + d.Name() + "/SKILL.md`"
		if !strings.Contains(got, want) {
			t.Errorf("index missing per-provider path for skill %q: want substring %q", d.Name(), want)
		}
	}
	if n := strings.Count(got, "- **"); n != nDirs {
		t.Errorf("index has %d bullet lines; want one per bundle skill (%d)", n, nDirs)
	}

	// The codex-shaped exclude entries cover the codex-shaped names too — a fully
	// seeded worktree is clean.
	if out := git(t, env, "-C", wt, "status", "--porcelain"); out != "" {
		t.Errorf("git status after non-native seed = %q; want empty", out)
	}
}

// A NativeSkillDiscovery: false provider with an EMPTY SkillsDir seeds no bundle,
// so there is nothing to index — renderContextFile must append no section. (The
// native-discovery-WITH-skills absence is asserted in the golden test above.)
func TestRenderContextFile_noIndexWithoutSkillsDir(t *testing.T) {
	meta := provider.SeedMeta{ContextFileName: "AGENTS.local.md", NativeSkillDiscovery: false}
	got, err := renderContextFile(builtinRepo, meta, nil, nil, nil)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	if strings.Contains(string(got), "## Seeded skills") {
		t.Error("empty-SkillsDir render contains a skills index; want none")
	}
}

// Zero secrets — both nil and an explicit empty slice — must render
// byte-identical to the claude golden (issue #104): the new Opts.Secrets /
// renderContextFile parameter is a pure addition, never a behavior change for
// a secret-less repo. Reuses the same golden the pre-#104 byte-identity test
// pins, so any accidental "## Secrets" leak on the empty path fails here too.
func TestRenderContextFile_zeroSecretsByteIdentical(t *testing.T) {
	want := readGolden(t, "contextfile-claude-builtin.golden")
	for name, secrets := range map[string][]store.RepoSecret{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := renderContextFile(builtinRepo, claudeGoldenMeta, secrets, nil, nil)
			if err != nil {
				t.Fatalf("renderContextFile: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("render with %s secrets != golden\n%s", name, byteDiff(got, want))
			}
		})
	}
}

// With secrets, a claude-shaped (native-discovery) render carries the full
// Secrets section: the heading, the usage norm, the single-quote/double-quote
// quoting pattern (both the correct form and the trap), one bullet per
// secret (name + description, or just the name when description is empty),
// and the `labctl secret list` pointer. Byte-pinned against a NEW golden
// (contextfile-claude-builtin-secrets.golden) captured for this shape.
func TestRenderContextFile_withSecrets(t *testing.T) {
	got, err := renderContextFile(builtinRepo, claudeGoldenMeta, testSecrets, nil, nil)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	if want := readGolden(t, "contextfile-claude-builtin-secrets.golden"); !bytes.Equal(got, want) {
		t.Errorf("render != contextfile-claude-builtin-secrets.golden\n%s", byteDiff(got, want))
	}

	gotStr := string(got)
	wantContains := []string{
		"## Secrets",
		"never write one into a file, a commit, an issue, a\nPR body, or a chat reply",
		"labctl secret exec\n<NAME...> -- <cmd>",
		"correct: labctl secret exec API_KEY -- sh -c 'curl -H \"Authorization: Bearer $API_KEY\" https://example.com'",
		"trap:    labctl secret exec API_KEY -- sh -c \"curl -H \\\"Authorization: Bearer $API_KEY\\\" https://example.com\"",
		"Single-quote the child command",
		"`API_KEY` — Widget API token",
		"`DEPLOY_TOKEN`",
		"`labctl secret list` shows the live inventory",
	}
	for _, want := range wantContains {
		if !strings.Contains(gotStr, want) {
			t.Errorf("Secrets section missing %q", want)
		}
	}
}

// Zero imports — nil, an explicit empty slice, with no secrets, and with
// secrets present — must render byte-identical to the corresponding claude
// golden (issue #261): the new Opts.Imports / renderContextFile parameter is
// a pure addition, never a behavior change for an import-less repo. Mirrors
// TestRenderContextFile_zeroSecretsByteIdentical's proof shape.
func TestRenderContextFile_zeroImportsByteIdentical(t *testing.T) {
	cases := []struct {
		name    string
		secrets []store.RepoSecret
		golden  string
	}{
		{"noSecrets", nil, "contextfile-claude-builtin.golden"},
		{"withSecrets", testSecrets, "contextfile-claude-builtin-secrets.golden"},
	}
	for _, tc := range cases {
		want := readGolden(t, tc.golden)
		for name, imports := range map[string][]ImportRef{
			"nil":   nil,
			"empty": {},
		} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				got, err := renderContextFile(builtinRepo, claudeGoldenMeta, tc.secrets, nil, imports)
				if err != nil {
					t.Fatalf("renderContextFile: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("render with %s imports != %s\n%s", name, tc.golden, byteDiff(got, want))
				}
			})
		}
	}
}

// With imports, a claude-shaped (native-discovery) render carries the full
// Read-only imports section: the heading, each import's name/path/commit
// bullet, and the never-to-be-edited norm. Byte-pinned against a NEW golden
// (contextfile-claude-builtin-imports.golden) captured for this shape, the
// same way TestRenderContextFile_withSecrets pins the secrets shape.
func TestRenderContextFile_withImports(t *testing.T) {
	got, err := renderContextFile(builtinRepo, claudeGoldenMeta, nil, nil, testImports)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	if want := readGolden(t, "contextfile-claude-builtin-imports.golden"); !bytes.Equal(got, want) {
		t.Errorf("render != contextfile-claude-builtin-imports.golden\n%s", byteDiff(got, want))
	}

	gotStr := string(got)
	wantContains := []string{
		"## Read-only imports",
		"api-server",
		"/var/lib/lab/state/instances/run_x/imports/api-server",
		"1234567890ab",
		"proto-defs",
		"/var/lib/lab/state/instances/run_x/imports/proto-defs",
		"abcdef012345",
		"never to be edited or committed",
	}
	for _, want := range wantContains {
		if !strings.Contains(gotStr, want) {
			t.Errorf("Read-only imports section missing %q", want)
		}
	}
}

// Non-native-discovery meta (the fake codex shape) with secrets: BOTH the
// Secrets section and the generated skills index appear, and the Secrets
// section comes FIRST — repo-driven content before the provider-driven tail
// (the append order documented on appendSecretsSection).
func TestRenderContextFile_nonNativeWithSecretsBothSectionsOrdered(t *testing.T) {
	got, err := renderContextFile(forgeRepo, codexMeta, testSecrets, nil, nil)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	gotStr := string(got)

	secretsIdx := strings.Index(gotStr, "## Secrets")
	skillsIdx := strings.Index(gotStr, "## Seeded skills")
	if secretsIdx == -1 {
		t.Fatal("missing `## Secrets` section")
	}
	if skillsIdx == -1 {
		t.Fatal("missing `## Seeded skills` section")
	}
	if secretsIdx >= skillsIdx {
		t.Errorf("Secrets section (offset %d) must precede Seeded skills (offset %d)", secretsIdx, skillsIdx)
	}
	if !strings.Contains(gotStr, "`API_KEY` — Widget API token") {
		t.Error("non-native render missing secret inventory bullet")
	}
}

// Non-native-discovery meta with BOTH secrets and imports: all three sections
// appear, IN ORDER — template body, then Secrets, then Read-only imports,
// then Seeded skills — repo-driven content (secrets, then imports) before the
// provider-driven tail (mirrors TestRenderContextFile_nonNativeWithSecretsBothSectionsOrdered,
// extended with the imports section per the ordering documented on
// appendSecretsSection and appendImportsSection).
func TestRenderContextFile_nonNativeWithSecretsAndImportsOrdered(t *testing.T) {
	got, err := renderContextFile(forgeRepo, codexMeta, testSecrets, nil, testImports)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	gotStr := string(got)

	bodyIdx := strings.Index(gotStr, "## Tracker binding")
	secretsIdx := strings.Index(gotStr, "## Secrets")
	importsIdx := strings.Index(gotStr, "## Read-only imports")
	skillsIdx := strings.Index(gotStr, "## Seeded skills")
	for name, idx := range map[string]int{
		"template body (## Tracker binding)": bodyIdx,
		"## Secrets":                         secretsIdx,
		"## Read-only imports":               importsIdx,
		"## Seeded skills":                   skillsIdx,
	} {
		if idx == -1 {
			t.Fatalf("missing %s section", name)
		}
	}
	if bodyIdx >= secretsIdx || secretsIdx >= importsIdx || importsIdx >= skillsIdx {
		t.Errorf("sections out of order: body=%d secrets=%d imports=%d skills=%d", bodyIdx, secretsIdx, importsIdx, skillsIdx)
	}
}

// The full seed path writes the Secrets section to disk when Opts.Secrets is
// non-empty (mirrors TestSeedWorkspace_onDiskMatchesGolden's mechanics).
func TestSeedWorkspace_onDiskWithSecretsMatchesGolden(t *testing.T) {
	wt, _ := newWorktree(t)
	if err := New().SeedWorkspace(wt, builtinRepo, claudeGoldenMeta, Opts{Secrets: testSecrets}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "CLAUDE.local.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, "contextfile-claude-builtin-secrets.golden"); !bytes.Equal(got, want) {
		t.Errorf("on-disk CLAUDE.local.md with secrets != golden\n%s", byteDiff(got, want))
	}
}

// The full seed path writes the Read-only imports section to disk when
// Opts.Imports is non-empty (mirrors TestSeedWorkspace_onDiskWithSecretsMatchesGolden's
// mechanics, issue #261).
func TestSeedWorkspace_onDiskWithImportsMatchesGolden(t *testing.T) {
	wt, _ := newWorktree(t)
	if err := New().SeedWorkspace(wt, builtinRepo, claudeGoldenMeta, Opts{Imports: testImports}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "CLAUDE.local.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, "contextfile-claude-builtin-imports.golden"); !bytes.Equal(got, want) {
		t.Errorf("on-disk CLAUDE.local.md with imports != golden\n%s", byteDiff(got, want))
	}
}

// The gateway is opt-in and OFF by default: with Opts.Gateway nil the render
// is byte-for-byte what it was before issue #24 — for a secret-less repo AND
// for one with repo_secrets rows, whose LEGACY Secrets section must still
// render exactly as it did. That is issue #24's "with OneCLI unconfigured,
// spawn behaves exactly as today" acceptance criterion at the seeder's
// boundary, pinned against the untouched goldens rather than against prose.
func TestRenderContextFile_nilGatewayByteIdentical(t *testing.T) {
	cases := []struct {
		name    string
		secrets []store.RepoSecret
		golden  string
	}{
		{"noSecrets", nil, "contextfile-claude-builtin.golden"},
		{"withSecrets", testSecrets, "contextfile-claude-builtin-secrets.golden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderContextFile(builtinRepo, claudeGoldenMeta, tc.secrets, nil, nil)
			if err != nil {
				t.Fatalf("renderContextFile: %v", err)
			}
			if want := readGolden(t, tc.golden); !bytes.Equal(got, want) {
				t.Errorf("nil-Gateway render != %s\n%s", tc.golden, byteDiff(got, want))
			}
		})
	}
}

// With a gateway, a claude-shaped (native-discovery) render carries the
// GATEWAY Secrets section: the heading, the no-value-in-the-environment
// property, the placeholder-auth norm, one bullet per granted service IN THE
// CALLER'S ORDER, the proxy-ignoring-tool caveat, and the surviving
// server-side push scan (issue #106). Byte-pinned against a NEW golden
// (contextfile-claude-builtin-gateway.golden), the same way
// TestRenderContextFile_withSecrets pins the legacy shape — and asserting the
// legacy teaching is gone, which is an explicit issue #24 acceptance
// criterion, not a nicety.
func TestRenderContextFile_withGateway(t *testing.T) {
	got, err := renderContextFile(builtinRepo, claudeGoldenMeta, nil, &GatewayRef{Services: testGatewayServices}, nil)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	if want := readGolden(t, "contextfile-claude-builtin-gateway.golden"); !bytes.Equal(got, want) {
		t.Errorf("render != contextfile-claude-builtin-gateway.golden\n%s", byteDiff(got, want))
	}

	gotStr := string(got)
	wantContains := []string{
		"## Secrets",
		"injected by lab's credential gateway at the\nnetwork layer",
		"The environment holds NO secret value",
		"(`HTTPS_PROXY`)",
		"send a placeholder — the gateway replaces",
		"its absence is not a misconfiguration",
		"Services granted to this repository:",
		"- `stripe`",
		"- `api.github.com`",
		"a tool that ignores proxy environment variables bypasses",
		"401/403 from the real API",
		"the environment never held a real key",
		"scanned server-side for known secret values",
	}
	for _, want := range wantContains {
		if !strings.Contains(gotStr, want) {
			t.Errorf("gateway Secrets section missing %q", want)
		}
	}
	// The legacy teaching is GONE — not merely deprioritized.
	for _, gone := range []string{"labctl secret exec", "labctl secret list", "operator-managed secrets", "Quote the child command"} {
		if strings.Contains(gotStr, gone) {
			t.Errorf("gateway render still carries the legacy secrets teaching %q", gone)
		}
	}
	// Caller order, not the seeder's: stripe was handed first even though it
	// sorts last.
	if first, second := strings.Index(gotStr, "- `stripe`"), strings.Index(gotStr, "- `api.github.com`"); first >= second {
		t.Errorf("granted services rendered out of caller order: stripe at %d, api.github.com at %d", first, second)
	}
}

// A gateway with NO grants yet still renders the section — the norm is the
// same whether or not anything is attached — but with the nothing-granted
// line in place of an inventory, so a 401 from an ungranted service reads as
// "an operator has not attached it in OneCLI's dashboard" rather than as a
// break the run should try to fix from inside.
func TestRenderContextFile_gatewayWithoutServices(t *testing.T) {
	for name, services := range map[string][]string{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := renderContextFile(builtinRepo, claudeGoldenMeta, nil, &GatewayRef{Services: services}, nil)
			if err != nil {
				t.Fatalf("renderContextFile: %v", err)
			}
			gotStr := string(got)
			if !strings.Contains(gotStr, "## Secrets") {
				t.Fatal("no-grants gateway render dropped the `## Secrets` section")
			}
			for _, want := range []string{
				"No services are granted to this repository yet",
				"an operator attaches\ngrants in OneCLI's dashboard",
				"not something this run can fix from inside",
			} {
				if !strings.Contains(gotStr, want) {
					t.Errorf("no-grants gateway render missing %q", want)
				}
			}
			if strings.Contains(gotStr, "Services granted to this repository:") {
				t.Error("no-grants gateway render still announces an inventory")
			}
			if n := strings.Count(gotStr, "\n- "); n != 0 {
				t.Errorf("no-grants gateway render has %d bullet line(s); want none", n)
			}
		})
	}
}

// The gateway section REPLACES the legacy one, never joins it: with
// Opts.Gateway set AND repo_secrets rows present, the file carries exactly
// ONE `## Secrets` heading, it is the gateway one, and neither the legacy
// prose nor the legacy inventory survives. Two headings would be two
// contradictory norms in one file (see appendGatewaySecretsSection), and the
// repo_secrets rows keep existing until #27 — so this is the case that has to
// be pinned, not assumed.
func TestRenderContextFile_gatewayReplacesLegacySecrets(t *testing.T) {
	got, err := renderContextFile(builtinRepo, claudeGoldenMeta, testSecrets, &GatewayRef{Services: testGatewayServices}, nil)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	gotStr := string(got)
	if n := strings.Count(gotStr, "## Secrets"); n != 1 {
		t.Errorf("gateway + repo secrets render has %d `## Secrets` headings; want exactly 1", n)
	}
	if !strings.Contains(gotStr, "injected by lab's credential gateway") {
		t.Error("the surviving `## Secrets` section is not the gateway one")
	}
	for _, gone := range []string{"labctl secret exec", "labctl secret list", "API_KEY", "DEPLOY_TOKEN", "This repository's secrets:"} {
		if strings.Contains(gotStr, gone) {
			t.Errorf("legacy Secrets section leaked into a gateway render: %q", gone)
		}
	}
	// The gateway golden is the whole answer here: the repo's secrets change
	// nothing about the render once a gateway is wired.
	if want := readGolden(t, "contextfile-claude-builtin-gateway.golden"); !bytes.Equal(got, want) {
		t.Errorf("gateway + repo secrets render != the gateway golden\n%s", byteDiff(got, want))
	}
}

// Non-native-discovery meta with gateway + imports: the gateway section
// occupies exactly the slot the legacy one did — template body, then Secrets,
// then Read-only imports, then Seeded skills — so replacing the section's
// CONTENT never reshuffles the file (mirrors
// TestRenderContextFile_nonNativeWithSecretsAndImportsOrdered).
func TestRenderContextFile_nonNativeWithGatewayAndImportsOrdered(t *testing.T) {
	got, err := renderContextFile(forgeRepo, codexMeta, nil, &GatewayRef{Services: testGatewayServices}, testImports)
	if err != nil {
		t.Fatalf("renderContextFile: %v", err)
	}
	gotStr := string(got)

	bodyIdx := strings.Index(gotStr, "## Tracker binding")
	secretsIdx := strings.Index(gotStr, "## Secrets")
	importsIdx := strings.Index(gotStr, "## Read-only imports")
	skillsIdx := strings.Index(gotStr, "## Seeded skills")
	for name, idx := range map[string]int{
		"template body (## Tracker binding)": bodyIdx,
		"## Secrets":                         secretsIdx,
		"## Read-only imports":               importsIdx,
		"## Seeded skills":                   skillsIdx,
	} {
		if idx == -1 {
			t.Fatalf("missing %s section", name)
		}
	}
	if bodyIdx >= secretsIdx || secretsIdx >= importsIdx || importsIdx >= skillsIdx {
		t.Errorf("sections out of order: body=%d secrets=%d imports=%d skills=%d", bodyIdx, secretsIdx, importsIdx, skillsIdx)
	}
	if !strings.Contains(gotStr, "- `stripe`") {
		t.Error("non-native render missing the granted-service inventory bullet")
	}
	if strings.Contains(gotStr, "labctl secret exec") {
		t.Error("non-native gateway render still carries the legacy `labctl secret exec` teaching")
	}
}

// The full seed path writes the gateway Secrets section to disk when
// Opts.Gateway is non-nil (mirrors TestSeedWorkspace_onDiskWithSecretsMatchesGolden's
// mechanics, issue #24) — the field the launch path will populate reaches the
// file, not just renderContextFile's parameter.
func TestSeedWorkspace_onDiskWithGatewayMatchesGolden(t *testing.T) {
	wt, _ := newWorktree(t)
	opts := Opts{Gateway: &GatewayRef{Services: testGatewayServices}}
	if err := New().SeedWorkspace(wt, builtinRepo, claudeGoldenMeta, opts); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "CLAUDE.local.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, "contextfile-claude-builtin-gateway.golden"); !bytes.Equal(got, want) {
		t.Errorf("on-disk CLAUDE.local.md with a gateway != golden\n%s", byteDiff(got, want))
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
