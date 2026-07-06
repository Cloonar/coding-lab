// Package seeder owns lab-side workspace seeding beyond the provider's trust
// step (design §1; brief D13): on EVERY spawn — incogni or not — it copies
// the embedded skills bundle into <worktree>/.claude/skills/, writes a
// generated CLAUDE.local.md at the worktree root (tracker binding, labctl
// vocabulary, triage-label table, the labctl-supersedes-tea/gh note), and
// lists everything lab seeds in .git/info/exclude — never .gitignore (D15 §9
// measure 6) — so a seeded worktree's own git status shows nothing.
//
// instance.Launch calls SeedWorkspace right after the provider's
// SeedWorkspace and before the tmux spawn; any failure aborts the Start
// through the existing rollback (worktree + branch + credential files gone,
// nothing stranded).
package seeder

import (
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// ExcludeEntries are the .git/info/exclude lines lab-side seeding guarantees
// (D15 §9 measure 6: everything lab seeds into a worktree is excluded). The
// provider's trust step already appends ".claude/" since M3; listing it here
// too keeps the guarantee complete for any provider that seeds nothing, and
// the dedup in EnsureExcludes makes the overlap a no-op.
var ExcludeEntries = []string{".claude/", "CLAUDE.local.md"}

// SeededPathPatterns are the grep BREs the pre-push guard (measure 7) matches
// changed paths against — the paths lab ACTUALLY seeds, not the broader
// ExcludeEntries set. ExcludeEntries hides all of `.claude/` from git status
// (harmless: excludes touch only untracked files), but the guard must not
// reject a commit editing a repo's OWN tracked `.claude/` content (commands,
// agents, a committed settings.json) — only lab's own seeded files leak lab.
var SeededPathPatterns = []string{
	`^\.claude/skills/`,
	`^\.claude/settings\.local\.json$`,
	`^CLAUDE\.local\.md$`,
}

// Seeder is the lab-side workspace seeder. Construct with New; it carries no
// state — everything it writes derives from the embedded bundle and the repo
// row passed per call.
type Seeder struct{}

// New returns a Seeder.
func New() *Seeder { return &Seeder{} }

// Opts parametrizes SeedWorkspace — the growth point for later per-spawn
// seeding knobs (mirrors provider.SeedOpts). Empty in M7: the rendered
// content derives entirely from the repo row.
type Opts struct{}

// SeedWorkspace seeds worktree for a run on repo: the exclude entries first
// (fails loud before anything is written when the dir is not a git worktree,
// and the seeded files are invisible to git status from the moment they
// exist), then the skills bundle, then CLAUDE.local.md. Idempotent: a
// re-seed overwrites lab's files, dedups exclude lines, and never deletes
// files a user added.
func (s *Seeder) SeedWorkspace(worktree string, repo store.Repo, _ Opts) error {
	if err := EnsureExcludes(worktree, ExcludeEntries); err != nil {
		return err
	}
	if err := seedSkills(worktree); err != nil {
		return err
	}
	return seedClaudeLocal(worktree, repo)
}
