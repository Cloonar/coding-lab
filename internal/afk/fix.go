package afk

// The fix run (issue #182 / ADR-0048): Autoland's bounded re-engagement of a
// rejected claim PR. Like the lander it adopts the claim's EXISTING PR head
// branch DETACHED into its own worktree (never a fresh fork — the launch is
// not a claim, and its rollback never deletes the branch), but where the
// lander judges, the fix run works: the rejection findings ride below its
// seed prompt as the work order — the new information that licenses the
// re-engagement (ADR-0048's fix-forward pin; a re-engagement carrying none
// is the blind form the avoid-list forbids) — and its done-signal is the
// fix-done marker `labctl pr rerequest` posts (FixDone). Attempts are
// bounded per branch and counted in SPAWNS, never rejection verdicts, so a
// fix run that dies on the launch pad still burns an attempt.

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// fixLabelPrefix is the fix run's instance-label namespace, the sibling of
// landerLabelPrefix — [a-z0-9-] only, never "~", and never parsed by
// ParseLabel (no "afk-" prefix), so a fix label can't register as an AFK
// run.
const fixLabelPrefix = "fix-"

// FixLabel renders a fix run's instance label from the issue number whose
// claim PR it repairs: fix-<N>.
func FixLabel(n int) string {
	return fixLabelPrefix + strconv.Itoa(n)
}

// FixSeedPromptTemplate is the built-in fix seed-prompt template with LITERAL
// gitx.NToken (<N>), PRToken (<PR>), and BranchToken (<BRANCH>) placeholders
// — the instruction set delivered to a just-spawned fix run. The contract
// lines are the #182 pins: the rejection below the separator is the WORK
// ORDER; the worktree is DETACHED at the PR head, so the push names its
// refspec explicitly (a bare `git push` has no upstream to resolve);
// `labctl pr rerequest` is the done-signal (FixDone reads the fix-done
// marker it posts); and `labctl pr create` is explicitly not the fix run's
// to run — the PR exists, and a second one would plant a competing
// done-signal on the same branch. The incogni variant appends the same
// attribution sentence the AFK template appends to its commit step. No
// override slot (the lander rule: the contract is pinned, never operator
// prose).
func FixSeedPromptTemplate(incogni bool) string {
	commit := "4. Commit in Conventional Commits style."
	if incogni {
		commit += " No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links."
	}
	return strings.Join([]string{
		"You are an autonomous fix run. Pull request #" + PRToken + " (head branch `" + BranchToken + "`) was rejected in review; resolve every finding, push, re-request review, stop.",
		"",
		"1. `labctl issue view " + gitx.NToken + "` and `labctl pr view " + PRToken + "` — read fully.",
		"2. The rejection review below is your work order — address every finding. Work only in this worktree; it is DETACHED at the PR head.",
		"3. Run the project's tests, build, and linters; fix what you break.",
		commit,
		"5. `git push origin HEAD:refs/heads/" + BranchToken + "` (detached worktree — push the refspec explicitly).",
		"6. `labctl pr rerequest " + PRToken + "` — never open a new PR; `labctl pr create` is not yours to run.",
		"7. Then stop working. Do not start unrelated work.",
	}, "\n")
}

// FixSeedPrompt renders the seed prompt delivered to a just-spawned fix run:
// the built-in template with <N>, then <PR>, then <BRANCH> interpolated at
// ALL occurrences (the SeedPrompt mechanism — literal token replacement,
// unknown tokens pass through), then the rejection content —
// RejectionContext's rendering, verbatim — appended below a `---` separator
// when non-empty. Appended AFTER interpolation, deliberately: the rejection
// is quoted forge prose and must never be token-substituted. An empty
// rejection appends nothing (defensive — the decision table only spawns a
// fix run from rejected-state).
func FixSeedPrompt(n, pr int, branch string, incogni bool, rejection string) string {
	tmpl := FixSeedPromptTemplate(incogni)
	tmpl = strings.ReplaceAll(tmpl, gitx.NToken, strconv.Itoa(n))
	tmpl = strings.ReplaceAll(tmpl, PRToken, strconv.Itoa(pr))
	tmpl = strings.ReplaceAll(tmpl, BranchToken, branch)
	if rejection != "" {
		tmpl += "\n\n---\n\n" + rejection
	}
	return tmpl
}

// fixWorktreePath is the on-disk worktree of a fix run for issue n:
// <worktrees>/<repoName>-fix-<N> — landerWorktreePath's sibling, disjoint
// from both the AFK run's <repoName>-<N> and the lander's
// <repoName>-lander-<N> for the same issue, and detached for the same reason
// the lander's is: a parked authoring worktree may still hold the claim ref,
// and git lets only one worktree hold it.
func (s *Service) fixWorktreePath(repoName string, n int) string {
	return filepath.Join(s.worktreeRoot, gitx.WorktreeDir(repoName, FixLabel(n)))
}

// fixProvider resolves a fix run's effective provider (issue #182 / ADR-0048:
// a fix run inherits its AUTHORING run's provider): the latest afk_manual/
// afk_auto run row for the branch (store.AuthoringRunForBranch) supplies its
// recorded provider as a per-spawn request through the normal resolver, so
// validation applies. When no authoring row exists, or the recorded provider
// no longer resolves (unregistered since the claim was authored), it warns
// and falls back to an empty-request resolution through the repo chain — the
// loop must stay alive; a vanished provider never strands a rejected PR.
// Shared by the producer's auth gate and LaunchFix so the two resolutions can
// never disagree. The authoring row rides back for the model/effort
// inheritance (found=false when none exists); a store read failure is
// returned raw — infrastructure, not a resolution miss.
func (s *Service) fixProvider(ctx context.Context, repo store.Repo, branch string) (provider.AgentProvider, store.Run, bool, error) {
	authoring, found, err := s.store.AuthoringRunForBranch(ctx, repo.ID, branch)
	if err != nil {
		return nil, store.Run{}, false, err
	}
	if found {
		prov, err := s.instances.ResolveProvider(ctx, repo, store.RunKindFix, authoring.Provider)
		if err == nil {
			return prov, authoring, true, nil
		}
		s.log.Warn("fix launch: authoring provider does not resolve — falling back to the repo chain",
			"component", "afk", "repo", repo.Name, "branch", branch, "provider", authoring.Provider, "err", err)
	} else {
		s.log.Warn("fix launch: no authoring run for the branch — falling back to the repo chain",
			"component", "afk", "repo", repo.Name, "branch", branch)
	}
	prov, err := s.instances.ResolveProvider(ctx, repo, store.RunKindFix, "")
	return prov, authoring, found, err
}

// LaunchFix claims nothing and selects nothing: it spawns the fix run for an
// ALREADY-DECIDED (PR, head branch, issue) triple plus the rejection work
// order the producer captured at gather time — the poller owns the decision.
// Single-flighted under the engine lock like every launch, with the repo
// re-read and the cap re-checked against fresh liveness there. Provider,
// model, and effort inherit from the PERSISTED authoring run row
// (fixProvider; runs.provider/model/effort as per-spawn requests through the
// normal resolvers so validation applies) — a missing row or a value that no
// longer resolves warns and falls back through the repo chain, NEVER failing
// the launch: the loop must stay alive. RunKindFix IS an AFK kind
// (instance-side isAFKKind — unattended AFK-class work continuing an AFK
// claim), so the AFK override layers govern the fallbacks, the options bag,
// and the remote knob. The adopt is DETACHED (AdoptBranch: the PR head branch
// pre-exists the run and IS the claim; the rollback never deletes it). Budget
// clock and token expiry follow the AFK rule exactly (effectiveBudget; token
// dies runTokenSlack past the deadline) — the reaper owns fix classification
// on the same contract, with FixDone as the done-signal.
func (s *Service) LaunchFix(ctx context.Context, repoID string, prNumber int, headBranch string, issueN int, rejection string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read the repo under the lock: budget, caps, and incogni must be
	// current, not a caller snapshot.
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.CloneStatus != store.CloneStatusReady {
		return instance.ErrRepoNotReady
	}
	prov, authoring, found, err := s.fixProvider(ctx, repo, headBranch)
	if err != nil {
		return err
	}

	// Cap guard on fresh liveness. Nothing is claimed by a fix launch, so
	// at-cap is a pure no-op the pass retries next tick.
	live, err := s.runner.List(ctx)
	if err != nil {
		return err
	}
	if instance.LiveInstanceCount(live) >= s.instances.EffectiveCap(ctx, repo) {
		return instance.ErrOverCap
	}

	// Model/effort inherit from the authoring row as per-spawn requests. A
	// request the resolved provider's catalog refuses — the provider fell
	// back above, or the model vanished from the catalog — warns and drops
	// BOTH requests for an empty-request resolution through the repo chain:
	// the same stay-alive stance as the provider.
	reqModel, reqEffort := "", ""
	if found {
		reqModel, reqEffort = authoring.Model, authoring.Effort
	}
	model, effort, err := s.instances.ResolveModelEffort(ctx, prov, repo, store.RunKindFix, reqModel, reqEffort)
	if err != nil && (reqModel != "" || reqEffort != "") {
		s.log.Warn("fix launch: authoring model/effort do not resolve — falling back to the repo chain",
			"component", "afk", "repo", repo.Name, "model", reqModel, "effort", reqEffort, "err", err)
		model, effort, err = s.instances.ResolveModelEffort(ctx, prov, repo, store.RunKindFix, "", "")
	}
	if err != nil {
		return err
	}
	options, err := s.instances.ResolveSpawnOptions(ctx, prov, repo, store.RunKindFix)
	if err != nil {
		return err
	}
	remote, err := s.instances.ResolveRemote(ctx, prov, repo, store.RunKindFix, nil)
	if err != nil {
		return err
	}

	deadline := s.now().Add(s.effectiveBudget(ctx, repo))
	tokenExpiry := deadline.Add(runTokenSlack)

	_, err = s.instances.Launch(ctx, instance.LaunchSpec{
		Repo:           repo,
		Provider:       prov,
		Kind:           store.RunKindFix,
		AdoptBranch:    true,
		IssueNumber:    &issueN,
		SessionName:    gitx.ComposeSessionName(repo.Name, FixLabel(issueN)),
		Branch:         headBranch,
		WorktreePath:   s.fixWorktreePath(repo.Name, issueN),
		Model:          model,
		Effort:         effort,
		Remote:         remote,
		Options:        options,
		BudgetDeadline: &deadline,
		TokenExpiry:    &tokenExpiry,
		SeedPrompt:     FixSeedPrompt(issueN, prNumber, headBranch, repo.Incogni, rejection),
	})
	return err
}
