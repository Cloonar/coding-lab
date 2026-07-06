package gitx

import (
	"context"
	"log/slog"
)

// TeardownAction is what the guarded teardown does with an instance's
// worktree + branch: whether to remove the worktree and whether to delete
// the branch. TeardownGuarded returns the actions that actually SUCCEEDED,
// so a caller can report removed-vs-parked and forget a deep link only
// when the worktree really went away.
type TeardownAction struct {
	RemoveWorktree bool
	DeleteBranch   bool
}

// decideTeardown is THE guarded-teardown rule as a pure function of two
// facts — is the worktree dirty, is the branch merged into
// origin/<default> — ported verbatim from v0 (git-worktrees port-spec
// §2.1; the four-row table test is the contract). A dirty worktree keeps
// everything (unsaved work survives); a clean one has its worktree removed
// always and its branch deleted only when merged. merged is meaningful
// only when !dirty — the caller skips the merged check on a dirty tree,
// where the result is keep-both regardless.
func decideTeardown(dirty, merged bool) TeardownAction {
	if dirty {
		return TeardownAction{}
	}
	return TeardownAction{RemoveWorktree: true, DeleteBranch: merged}
}

// TeardownGuarded applies lab's single guarded-teardown rule to a worktree
// + branch — the ONLY wiring of decideTeardown to git, shared by manual
// Stop, the AFK reaper, startup reconciliation, and the runtime sweep.
// Conservative on uncertainty (a clean worktree is reproducible from its
// branch; destroyed work is not):
//
//   - WorktreeDirty error → keep worktree AND branch, return.
//   - Merged check runs only on a clean tree; its error → keep both, return.
//   - RemoveWorktree failure → the branch delete is SKIPPED (git would
//     refuse to delete a branch still checked out, and the worktree is the
//     recoverable witness).
//   - DeleteBranch failure → logged only.
//
// Best-effort and never fails its caller: every git hiccup is logged
// (design D17 — the same decision points as v0's log lines, keep-vs-delete
// outcomes observable) and leaves a reclaimable state for the sweeps. The
// returned TeardownAction reflects what actually happened, not what was
// decided.
func (e *Engine) TeardownGuarded(ctx context.Context, log *slog.Logger, bareDir, worktreePath, branch, defaultBranch string, extraEnv []string) TeardownAction {
	if log == nil {
		log = slog.Default()
	}
	var done TeardownAction
	dirty, err := e.WorktreeDirty(ctx, worktreePath, extraEnv)
	if err != nil {
		log.Warn("teardown: worktree status unreadable, keeping worktree and branch",
			"component", "gitx", "worktree", worktreePath, "branch", branch, "err", err)
		return done
	}
	merged := false
	if !dirty {
		if merged, err = e.BranchMerged(ctx, bareDir, branch, defaultBranch, extraEnv); err != nil {
			log.Warn("teardown: merged check failed, keeping worktree and branch",
				"component", "gitx", "worktree", worktreePath, "branch", branch, "err", err)
			return done
		}
	}
	act := decideTeardown(dirty, merged)
	if act.RemoveWorktree {
		if err := e.RemoveWorktree(ctx, bareDir, worktreePath, extraEnv); err != nil {
			log.Warn("teardown: remove worktree failed, keeping branch",
				"component", "gitx", "worktree", worktreePath, "branch", branch, "err", err)
			return done
		}
		done.RemoveWorktree = true
	}
	if act.DeleteBranch {
		if err := e.DeleteBranch(ctx, bareDir, branch, extraEnv); err != nil {
			log.Warn("teardown: delete branch failed",
				"component", "gitx", "worktree", worktreePath, "branch", branch, "err", err)
		} else {
			done.DeleteBranch = true
		}
	}
	return done
}
