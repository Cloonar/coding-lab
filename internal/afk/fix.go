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
// ORDER; on red checks, `labctl pr logs` comes FIRST — before any local
// repro or re-push, so the run reads the actual failure instead of guessing
// (issue #241); the worktree is DETACHED at the PR head, so the push names
// its refspec explicitly (a bare `git push` has no upstream to resolve);
// `labctl pr rerequest` is the done-signal (FixDone reads the fix-done
// marker it posts); and `labctl pr create` is explicitly not the fix run's
// to run — the PR exists, and a second one would plant a competing
// done-signal on the same branch. The incogni variant appends the same
// attribution sentence the AFK template appends to its commit step. No
// override slot (the lander rule: the contract is pinned, never operator
// prose).
func FixSeedPromptTemplate(incogni bool) string {
	commit := "5. Commit in Conventional Commits style."
	if incogni {
		commit += " No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links."
	}
	return strings.Join([]string{
		"You are an autonomous fix run. Pull request #" + PRToken + " (head branch `" + BranchToken + "`) was rejected in review; resolve every finding, push, re-request review, stop.",
		"",
		"1. `labctl issue view " + gitx.NToken + "` and `labctl pr view " + PRToken + "` — read fully.",
		"2. Run `labctl pr checks " + PRToken + "`. If checks are red, run `labctl pr logs " + PRToken + "` first and read the failing jobs' logs before any local repro or re-push.",
		"3. The rejection review below is your work order — address every finding. Work only in this worktree; it is DETACHED at the PR head.",
		"4. Run the project's tests, build, and linters; fix what you break.",
		commit,
		"6. `git push origin HEAD:refs/heads/" + BranchToken + "` (detached worktree — push the refspec explicitly).",
		"7. `labctl pr rerequest " + PRToken + "` — never open a new PR; `labctl pr create` is not yours to run.",
		"8. Then stop working. Do not start unrelated work.",
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

// LaunchFix claims nothing and selects nothing: it spawns the fix run for an
// ALREADY-DECIDED (PR, head branch, issue) triple plus the rejection work
// order the producer captured at gather time — the poller owns the decision.
// Single-flighted under the engine lock like every launch, with the repo
// re-read and the cap re-checked against fresh liveness there. Provider,
// model, and effort resolve through the NORMAL AFK chain (repo AFK override →
// global AFK override → base) like any AFK-kind run — issue #189 reverses the
// ADR-0048 inheritance rule (issue #182) that read them from the persisted
// authoring run row. Nothing provider-specific carries over from the run that
// authored the claim: the adopt is DETACHED (AdoptBranch: the PR head branch
// pre-exists the run and IS the claim; the rollback never deletes it), so a
// fix may legitimately run under a different provider than the authoring run —
// the operator wants one AFK settings surface, not a second one shadowing it.
// RunKindFix IS an AFK kind (instance-side isAFKKind — unattended AFK-class
// work continuing an AFK claim), so the empty-request resolutions here pick up
// the AFK override layers automatically, and the same layers govern the
// options bag and the remote knob. A resolution miss is fatal, no fallback
// ladder: with no per-spawn request there is nothing to fall back FROM.
// Budget clock and token expiry follow the AFK rule exactly (effectiveBudget;
// token dies runTokenSlack past the deadline) — the reaper owns fix
// classification on the same contract, with FixDone as the done-signal.
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
	// Provider resolves through the normal AFK chain (RunKindFix is an AFK
	// kind): empty request → repo AFK override → global AFK override → base.
	// The authoring run's persisted provider has no say (issue #189).
	prov, err := s.instances.ResolveProvider(ctx, repo, store.RunKindFix, "")
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

	// Model/effort resolve through the same AFK chain — one empty-request
	// call, error fatal (issue #189: no authoring row to inherit from, so no
	// fallback ladder). The AFK override layers already govern the result.
	model, effort, err := s.instances.ResolveModelEffort(ctx, prov, repo, store.RunKindFix, "", "")
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
