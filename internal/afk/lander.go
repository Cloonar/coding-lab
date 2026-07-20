package afk

// The lander run (issue #181 / ADR-0048): Autoland's validation run. It
// adopts a claim's EXISTING PR head branch into its own worktree
// (instance.LaunchSpec.AdoptBranch — never a fresh fork, so the launch is
// not a claim and its rollback never deletes the branch), gets fresh run
// credentials, and is seeded with a prompt wrapping the validation core
// (assets/skills/land-pr/validation-core.md — the ONE definition of
// landable, shared with the interactive land-pr skill). The state-derived
// poller that decides WHEN to spawn one ships behind this entry point.

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/assets"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// landerLabelPrefix is the lander's instance-label namespace, the sibling of
// the AFK labelPrefix — [a-z0-9-] only, never "~", and never parsed by
// ParseLabel (no "afk-" prefix), so a lander label can't register as an AFK
// run.
const landerLabelPrefix = "lander-"

// LanderLabel renders a lander run's instance label from the issue number
// whose claim PR it validates: lander-<N>.
func LanderLabel(n int) string {
	return landerLabelPrefix + strconv.Itoa(n)
}

// PRToken is the pull-request-number placeholder in the lander seed-prompt
// template — the sibling of BranchToken, interpolated at EVERY occurrence
// (strings.ReplaceAll), never text/template.
const PRToken = "<PR>"

// validationCore is the embedded validation-core doc the lander prompt wraps
// (ADR-0048: one definition of landable, shared with the land-pr skill), read
// once from the embedded skills bundle. A missing file is a build defect —
// the bundle is compiled in — so the panic can only fire on a bad rename.
var validationCore = func() string {
	b, err := assets.Skills.ReadFile("skills/land-pr/validation-core.md")
	if err != nil {
		panic("afk: embedded validation core: " + err.Error())
	}
	return string(b)
}()

// LanderSeedPromptTemplate is the built-in lander seed-prompt template with
// LITERAL PRToken (<PR>) and BranchToken (<BRANCH>) placeholders — the
// instruction set delivered to a just-spawned lander run, wrapping the full
// embedded validation core below a separator (instructions above, doc below).
// The verdict step is the ADR-0048 action table: autoMerge decides whether a
// clean PASS merges or stops at the approve; PASS with CONCERNS never merges;
// FAIL or blocking CONCERNS rejects with the findings. The incogni variant
// appends the same attribution sentence the AFK template appends to its
// commit step (the inline-fix step is the lander's only committing step).
func LanderSeedPromptTemplate(autoMerge, incogni bool) string {
	fix := "3. Fix trivial findings only — formatting, lint, merge conflicts — inline: commit, then" +
		" `git push origin HEAD:refs/heads/" + BranchToken + "` (your worktree is DETACHED at the PR head, so" +
		" push the refspec explicitly — a bare `git push` has no upstream to resolve)." +
		" Substantive problems are never yours to fix — they belong to the verdict."
	if incogni {
		fix += " No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links."
	}
	pass := "clean PASS → `labctl pr approve " + PRToken + "`, then `labctl pr merge " + PRToken + "`."
	if !autoMerge {
		pass = "clean PASS → `labctl pr approve " + PRToken + "`, then stop — a human merges."
	}
	return strings.Join([]string{
		"You are an autonomous lander run. Validate pull request #" + PRToken + " (head branch `" + BranchToken + "`) against the validation core below, act on the verdict, and stop.",
		"",
		"1. Run `labctl pr view " + PRToken + "` and `labctl pr checks " + PRToken + " --wait`.",
		"2. Validate the pull request against the validation core below.",
		fix,
		"4. Act on the verdict: " + pass +
			" PASS with CONCERNS → `labctl pr approve " + PRToken + " <concerns>`, then stop — never merge." +
			" FAIL or blocking CONCERNS → `labctl pr reject " + PRToken + " <findings>`.",
		"5. Then stop working. Do not start unrelated work.",
		"",
		"---",
		"",
		validationCore,
	}, "\n")
}

// LanderSeedPrompt renders the seed prompt delivered to a just-spawned lander
// run: the built-in template for the repo's autoMerge/incogni settings with
// <PR> then <BRANCH> interpolated at ALL occurrences (the SeedPrompt
// mechanism — literal token replacement, unknown tokens like the verdict
// step's <concerns>/<findings> pass through untouched). There is no override
// slot: the lander's contract IS the validation core, never operator prose.
func LanderSeedPrompt(pr int, branch string, autoMerge, incogni bool) string {
	tmpl := LanderSeedPromptTemplate(autoMerge, incogni)
	tmpl = strings.ReplaceAll(tmpl, PRToken, strconv.Itoa(pr))
	tmpl = strings.ReplaceAll(tmpl, BranchToken, branch)
	return tmpl
}

// landerWorktreePath is the on-disk worktree of a lander run for issue n:
// <worktrees>/<repoName>-lander-<N> — the label keeps its PATH disjoint from
// the AFK run's <repoName>-<N> for the same issue. Disjoint paths are only
// half of what lets a lander validate a claim whose (parked) AFK worktree
// still exists: both worktrees would want the same afk/<N> ref, and git lets
// only one hold it. The adopt is DETACHED (gitx.AddWorktreeExisting) so the
// lander never asks for the ref at all.
func (s *Service) landerWorktreePath(repoName string, n int) string {
	return filepath.Join(s.worktreeRoot, gitx.WorktreeDir(repoName, LanderLabel(n)))
}

// LaunchLander claims nothing and selects nothing: it spawns the lander run
// for an ALREADY-DECIDED (PR, head branch, issue) triple — the poller owns
// the decision. Single-flighted under the engine lock like every launch, with
// the repo re-read and the cap re-checked against fresh liveness there. The
// provider resolves through the lander_provider setting as a STRICT per-spawn
// request when set (an unknown id fails the launch — the operator asked for
// it by name), else the repo's base chain; model/effort/options/remote
// resolve with the lander kind, which is NOT an AFK kind (isAFKKind), so the
// AFK override layers never apply. Budget clock and token expiry follow the
// AFK rule exactly (effectiveBudget; token dies runTokenSlack past the
// deadline) — the reaper owns lander classification on the same contract.
func (s *Service) LaunchLander(ctx context.Context, repoID string, prNumber int, headBranch string, issueN int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read the repo under the lock: autoland settings, budget, caps, and
	// incogni must be current, not a caller snapshot.
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.CloneStatus != store.CloneStatusReady {
		return instance.ErrRepoNotReady
	}
	landerProv := ""
	if repo.LanderProvider != nil {
		landerProv = *repo.LanderProvider
	}
	prov, err := s.instances.ResolveProvider(ctx, repo, store.RunKindLander, landerProv)
	if err != nil {
		return err
	}

	// Cap guard on fresh liveness. Nothing is claimed by a lander launch, so
	// at-cap is a pure no-op the poller retries next tick.
	live, err := s.runner.List(ctx)
	if err != nil {
		return err
	}
	if instance.LiveInstanceCount(live) >= s.instances.EffectiveCap(ctx, repo) {
		return instance.ErrOverCap
	}

	model, effort, err := s.instances.ResolveModelEffort(ctx, prov, repo, store.RunKindLander, "", "")
	if err != nil {
		return err
	}
	options, err := s.instances.ResolveSpawnOptions(ctx, prov, repo, store.RunKindLander)
	if err != nil {
		return err
	}
	remote, err := s.instances.ResolveRemote(ctx, prov, repo, store.RunKindLander, nil)
	if err != nil {
		return err
	}

	deadline := s.now().Add(s.effectiveBudget(ctx, repo))
	tokenExpiry := deadline.Add(runTokenSlack)

	_, err = s.instances.Launch(ctx, instance.LaunchSpec{
		Repo:           repo,
		Provider:       prov,
		Kind:           store.RunKindLander,
		AdoptBranch:    true,
		IssueNumber:    &issueN,
		SessionName:    gitx.ComposeSessionName(repo.Name, LanderLabel(issueN)),
		Branch:         headBranch,
		WorktreePath:   s.landerWorktreePath(repo.Name, issueN),
		Model:          model,
		Effort:         effort,
		Remote:         remote,
		Options:        options,
		BudgetDeadline: &deadline,
		TokenExpiry:    &tokenExpiry,
		SeedPrompt:     LanderSeedPrompt(prNumber, headBranch, repo.AutoMerge, repo.Incogni),
	})
	return err
}
