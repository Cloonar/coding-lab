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
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
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

// escalateLabelPrefix is the escalate-mode lander's instance-label namespace
// (issue #182), sibling of landerLabelPrefix — [a-z0-9-] only, never "~",
// and never parsed by ParseLabel (no "afk-" prefix), so an escalate label
// can't register as an AFK run.
const escalateLabelPrefix = "escalate-"

// EscalateLabel renders an escalate run's instance label from the issue
// number whose claim PR it hands off: escalate-<N>.
func EscalateLabel(n int) string {
	return escalateLabelPrefix + strconv.Itoa(n)
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

// EscalateSeedPromptTemplate is the built-in escalate seed-prompt template
// with LITERAL gitx.NToken (<N>), PRToken (<PR>), and BranchToken (<BRANCH>)
// placeholders — the instruction set delivered to a just-spawned
// escalate-mode lander (issue #182 / ADR-0048): the terminal hand-off once
// the fix-attempt bound is spent, agent-executed so every forge write stays
// on the run-token boundary (the engine itself never writes to a forge). The
// digest lands on the ISSUE — the human's surface; the label flip re-routes
// the queue, `labctl label create` first because applying an undefined label
// is an error and ready-for-human may not exist yet (create is idempotent,
// safe unconditionally; the remove tolerates failure — the label may already
// be gone); and `labctl pr escalate` posts LAST because it is the terminal
// marker EscalateDelivered reads — posted earlier it could reap the run
// mid-hand-off with the digest and label flip still undone. No commit step,
// so no incogni variant. `<digest>` is an unknown token and passes through
// literally, like the lander template's <concerns>/<findings>.
func EscalateSeedPromptTemplate() string {
	return strings.Join([]string{
		"You are an autonomous escalation run. Pull request #" + PRToken + " (head branch `" + BranchToken + "`, issue #" + gitx.NToken + ") has exhausted its fix budget and is still rejected; hand it to a human and stop.",
		"",
		"1. `labctl issue view " + gitx.NToken + "` and `labctl pr view " + PRToken + "` — read fully; the round history is below. Inspect the branch (git log/diff) as needed.",
		"2. Write a history digest: what was rejected each round, what each fix attempt changed, why it still fails.",
		"3. `labctl issue comment " + gitx.NToken + " <digest>`.",
		"4. `labctl label create --name ready-for-human` (idempotent, safe if it exists), then `labctl issue label remove " + gitx.NToken + " ready-for-agent` and `labctl issue label add " + gitx.NToken + " ready-for-human`. Tolerate a remove error if the label is already gone.",
		"5. `labctl pr escalate " + PRToken + " <digest>` — the terminal marker; post it LAST.",
		"6. Then stop working. Do not start unrelated work.",
	}, "\n")
}

// EscalateSeedPrompt renders the seed prompt delivered to a just-spawned
// escalate run: the template with <N>, then <PR>, then <BRANCH> interpolated
// at ALL occurrences (the SeedPrompt mechanism), then the round history —
// EscalationHistory's rendering, verbatim — appended below a `---` separator
// when non-empty. Appended AFTER interpolation, deliberately: history is
// quoted forge prose and must never be token-substituted.
func EscalateSeedPrompt(n, pr int, branch string, history string) string {
	tmpl := EscalateSeedPromptTemplate()
	tmpl = strings.ReplaceAll(tmpl, gitx.NToken, strconv.Itoa(n))
	tmpl = strings.ReplaceAll(tmpl, PRToken, strconv.Itoa(pr))
	tmpl = strings.ReplaceAll(tmpl, BranchToken, branch)
	if history != "" {
		tmpl += "\n\n---\n\n" + history
	}
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

// landerChainProvider resolves the lander-chain effective provider for a
// validation-class run — the lander, and #182's escalate-mode lander: the
// lander_provider setting as a STRICT per-spawn request when set (an unknown
// id fails the launch — the operator asked for it by name), else the repo's
// base chain. kind rides through so the resolver treats both as NON-AFK
// (isAFKKind): the AFK override layers never apply to a validation-class run.
// Shared by the producer's auth gates and both launch paths so the gate and
// the launch can never disagree.
func (s *Service) landerChainProvider(ctx context.Context, repo store.Repo, kind string) (provider.AgentProvider, error) {
	req := ""
	if repo.LanderProvider != nil {
		req = *repo.LanderProvider
	}
	return s.instances.ResolveProvider(ctx, repo, kind, req)
}

// LaunchLander claims nothing and selects nothing: it spawns the lander run
// for an ALREADY-DECIDED (PR, head branch, issue) triple — the poller owns
// the decision. Single-flighted under the engine lock like every launch, with
// the repo re-read and the cap re-checked against fresh liveness there. The
// provider resolves through the lander chain (landerChainProvider);
// model/effort/options/remote resolve with the lander kind, which is NOT an
// AFK kind (isAFKKind), so the AFK override layers never apply. approveOnly
// forces the approve-and-stop seed variant regardless of repo.AutoMerge — the
// producer sets it for a re-validation round under an outstanding human
// changes-requested (never merge over a live human rejection; ADR-0048's
// ownership rule says only that human's newer review clears it, and
// DecideAutoland decides). Budget clock and token expiry follow the AFK rule
// exactly (effectiveBudget; token dies runTokenSlack past the deadline) — the
// reaper owns lander classification on the same contract.
func (s *Service) LaunchLander(ctx context.Context, repoID string, prNumber int, headBranch string, issueN int, approveOnly bool) error {
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
	prov, err := s.landerChainProvider(ctx, repo, store.RunKindLander)
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
		SeedPrompt:     LanderSeedPrompt(prNumber, headBranch, repo.AutoMerge && !approveOnly, repo.Incogni),
	})
	return err
}

// escalateWorktreePath is the on-disk worktree of an escalate run for issue
// n: <worktrees>/<repoName>-escalate-<N> — landerWorktreePath's sibling,
// disjoint and detached for the same reasons.
func (s *Service) escalateWorktreePath(repoName string, n int) string {
	return filepath.Join(s.worktreeRoot, gitx.WorktreeDir(repoName, EscalateLabel(n)))
}

// LaunchEscalate spawns the escalate-mode lander for an ALREADY-DECIDED (PR,
// head branch, issue) triple plus the round history the producer captured at
// gather time (issue #182 / ADR-0048): the terminal hand-off once the
// fix-attempt bound is spent. Structurally LaunchLander with the escalate
// identity: kind RunKindEscalate — deliberately NON-AFK, a validation-class
// run on the lander chain (landerChainProvider; isAFKKind excludes it, so the
// AFK override layers never apply) — label escalate-<N>, detached adopt of
// the existing PR head, and the AFK budget/token rule. Its own `labctl pr
// escalate` marker is its done-signal (EscalateDelivered), which the reaper
// maps to outcome 'escalated' — the poller's permanent-terminality gate.
func (s *Service) LaunchEscalate(ctx context.Context, repoID string, prNumber int, headBranch string, issueN int, history string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read the repo under the lock: budget, caps, and the lander provider
	// must be current, not a caller snapshot.
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.CloneStatus != store.CloneStatusReady {
		return instance.ErrRepoNotReady
	}
	prov, err := s.landerChainProvider(ctx, repo, store.RunKindEscalate)
	if err != nil {
		return err
	}

	// Cap guard on fresh liveness. Nothing is claimed by an escalate launch,
	// so at-cap is a pure no-op the pass retries next tick.
	live, err := s.runner.List(ctx)
	if err != nil {
		return err
	}
	if instance.LiveInstanceCount(live) >= s.instances.EffectiveCap(ctx, repo) {
		return instance.ErrOverCap
	}

	model, effort, err := s.instances.ResolveModelEffort(ctx, prov, repo, store.RunKindEscalate, "", "")
	if err != nil {
		return err
	}
	options, err := s.instances.ResolveSpawnOptions(ctx, prov, repo, store.RunKindEscalate)
	if err != nil {
		return err
	}
	remote, err := s.instances.ResolveRemote(ctx, prov, repo, store.RunKindEscalate, nil)
	if err != nil {
		return err
	}

	deadline := s.now().Add(s.effectiveBudget(ctx, repo))
	tokenExpiry := deadline.Add(runTokenSlack)

	_, err = s.instances.Launch(ctx, instance.LaunchSpec{
		Repo:           repo,
		Provider:       prov,
		Kind:           store.RunKindEscalate,
		AdoptBranch:    true,
		IssueNumber:    &issueN,
		SessionName:    gitx.ComposeSessionName(repo.Name, EscalateLabel(issueN)),
		Branch:         headBranch,
		WorktreePath:   s.escalateWorktreePath(repo.Name, issueN),
		Model:          model,
		Effort:         effort,
		Remote:         remote,
		Options:        options,
		BudgetDeadline: &deadline,
		TokenExpiry:    &tokenExpiry,
		SeedPrompt:     EscalateSeedPrompt(issueN, prNumber, headBranch, history),
	})
	return err
}
