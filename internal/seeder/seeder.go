// Package seeder owns lab-side workspace seeding beyond the provider's trust
// step (design §1; brief D13): on EVERY spawn — incogni or not — it copies
// the embedded skills bundle into the provider's skills dir, writes a
// generated context file at the worktree root (tracker binding, labctl
// vocabulary, triage-label table, the labctl-supersedes-tea/gh note — plus, for
// a provider that declares NativeSkillDiscovery: false with a non-empty
// SkillsDir, a generated index of the seeded skills so an agent that only reads
// context files still finds the playbooks on demand, issue #79 / ADR-0035), and
// lists everything lab seeds in .git/info/exclude — never .gitignore (D15 §9
// measure 6) — so a seeded worktree's own git status shows nothing.
//
// It is GENERIC (issue #51 decision 8): every claude-specific shape it used to
// hardcode — the ".claude/skills" dir, the "CLAUDE.local.md" name, the exclude
// entries, and the incogni hook's scrub/seeded-path patterns — now flows in
// as provider.SeedMeta from the call site that holds the resolved provider
// (instance.Launch) or resolves it from the repo row (reposvc, for the
// incogni hook). The seeder still OWNS the lab content: the skills bundle
// bytes and the context-file template are lab's; only their destination
// name/dir comes from the provider. This keeps the split coherent — the
// provider's own SeedWorkspace seeds its trust/MCP/attribution grants, the
// seeder seeds what lab layers on top of any agent CLI.
//
// instance.Launch calls SeedWorkspace right after the provider's
// SeedWorkspace and before the tmux spawn; any failure aborts the Start
// through the existing rollback (worktree + branch + credential files gone,
// nothing stranded).
package seeder

import (
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// Seeder is the lab-side workspace seeder. Construct with New; it carries no
// state — everything it writes derives from the embedded bundle, the repo row,
// and the provider.SeedMeta passed per call.
type Seeder struct{}

// New returns a Seeder.
func New() *Seeder { return &Seeder{} }

// ImportRef describes one read-only import for the context file: a snapshot
// of another lab repo materialized outside the worktree at spawn.
type ImportRef struct {
	Name   string // the imported repo's name
	Path   string // absolute path of the snapshot directory
	Commit string // snapshotted commit (12-char short hash)
}

// Opts parametrizes SeedWorkspace — the growth point for later per-spawn
// seeding knobs (mirrors provider.SeedOpts).
type Opts struct {
	// Secrets is the repo's secret INVENTORY — metadata only (name +
	// description; see store.RepoSecret). The seeder never sees a value: it
	// never imports internal/vault, and this field never carries a decrypted
	// blob. When non-empty, renderContextFile appends a Secrets section
	// (issue #104) documenting the usage norm and listing these names/
	// descriptions; nil or empty yields no section, so a secret-less repo's
	// context file is byte-identical to before this field existed.
	Secrets []store.RepoSecret

	// Imports is the repo's read-only-import INVENTORY (issue #261): one
	// ImportRef per snapshot the launch path has already materialized
	// outside this worktree. The seeder never creates or refreshes a
	// snapshot — it only renders the inventory it is handed. The caller
	// passes them ordered by name, and renderContextFile appends a
	// Read-only imports section in that given order; nil or empty yields no
	// section, so an import-less repo's context file is byte-identical to
	// before this field existed.
	Imports []ImportRef
}

// SeedWorkspace seeds worktree for a run on repo, driven by the provider's
// meta (issue #51 decision 8): the exclude entries first (fails loud before
// anything is written when the dir is not a git worktree — EnsureExcludes
// resolves the git dir up front regardless of how many entries there are, so
// the fail-loud guard holds even for a provider that declares none — and the
// seeded files are invisible to git status from the moment they exist), then
// the skills bundle into meta.SkillsDir, then the context file named
// meta.ContextFileName (which, for a non-native-discovery provider seeding
// skills, gains a generated index of those skills, issue #79). Empty-field
// semantics are explicit: an empty SkillsDir skips the skills copy, an empty
// ContextFileName skips the context file — a provider that seeds neither still
// gets its exclude entries applied. Idempotent: a re-seed overwrites lab's
// files, dedups exclude lines, and never deletes files a user added.
func (s *Seeder) SeedWorkspace(worktree string, repo store.Repo, meta provider.SeedMeta, opts Opts) error {
	if err := EnsureExcludes(worktree, meta.ExcludeEntries); err != nil {
		return err
	}
	if err := seedSkills(worktree, meta.SkillsDir); err != nil {
		return err
	}
	return seedContextFile(worktree, repo, meta, opts.Secrets, opts.Imports)
}
