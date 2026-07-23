package reconcile

import (
	"context"
	"fmt"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// BadRequestError marks a discard of a non-managed branch — a crafted request
// can never force-delete main or a human's branch. The API answers 400.
type BadRequestError struct{ msg string }

func (e *BadRequestError) Error() string { return e.msg }

// ParkedEntry is one row of the parked view (pinned API: GET
// /api/v1/repos/{id}/parked → {parked:[{branch, worktree_path, dirty,
// commits_ahead, unpushed}]}). worktree_path is "" for a bare claim branch. The
// per-entry stats are best-effort: a stat error leaves that field at its zero
// value (enumeration already failed loud if it could not list). Age and PR
// badges are M5/M6.
type ParkedEntry struct {
	Branch       string
	WorktreePath string
	Dirty        bool
	CommitsAhead int
	Unpushed     int
}

// Parked enumerates a repo's parked work: every managed branch NOT owned by a
// live or mid-Start session, in git's branch order, each paired with its
// worktree (if any) and its dirty/ahead/unpushed stats. Enumeration is fail
// loud (a Branches/Worktrees/List error propagates — never a misleading empty
// list); the per-entry stats degrade to zero on error. Reuses the exact
// reconciliation derivation (managed + owned) so the parked set can never drift
// from what reconciliation considers managed.
func (s *Service) Parked(ctx context.Context, repoID string) ([]ParkedEntry, error) {
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return nil, err
	}
	r, ok, err := s.parkedRefs(ctx, repo)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	bareDir := s.bareDir(repo.ID)

	entries := make([]ParkedEntry, 0, len(r.branches))
	for _, branch := range r.branches {
		if r.owned[branch] {
			continue
		}
		e := ParkedEntry{Branch: branch, WorktreePath: r.wtByBranch[branch]}
		if e.WorktreePath != "" {
			if dirty, err := s.git.WorktreeDirty(ctx, e.WorktreePath, s.gitEnv); err != nil {
				s.log.Warn("parked: worktree status", "component", "reconcile", "repo", repo.Name, "branch", branch, "err", err)
			} else {
				e.Dirty = dirty
			}
		}
		if ahead, err := s.git.CommitsAhead(ctx, bareDir, branch, repo.DefaultBranch, s.gitEnv); err != nil {
			s.log.Warn("parked: commits ahead", "component", "reconcile", "repo", repo.Name, "branch", branch, "err", err)
		} else {
			e.CommitsAhead = ahead
		}
		if unpushed, err := s.git.UnpushedCount(ctx, bareDir, branch, repo.DefaultBranch, s.gitEnv); err != nil {
			s.log.Warn("parked: unpushed count", "component", "reconcile", "repo", repo.Name, "branch", branch, "err", err)
		} else {
			e.Unpushed = unpushed
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// parkedRefs is gatherRefs' fail-loud sibling: it PROPAGATES enumeration errors
// (the parked strip must show the error, never a misleading empty set) rather
// than logging and aborting the project. ok=false only when the branch set is
// empty (no error). Same read order (branches → worktrees → starting → live) and
// same managed/owned derivation.
func (s *Service) parkedRefs(ctx context.Context, repo store.Repo) (refs, bool, error) {
	bareDir := s.bareDir(repo.ID)
	branches, err := s.git.ManagedBranches(ctx, bareDir, repo.AFKBranchPattern, repo.ManualBranchPrefix, s.gitEnv)
	if err != nil {
		return refs{}, false, err
	}
	wts, err := s.git.Worktrees(ctx, bareDir, s.gitEnv)
	if err != nil {
		return refs{}, false, err
	}
	starting := s.guard.Snapshot()
	live, err := s.runner.List(ctx)
	if err != nil {
		return refs{}, false, err
	}
	r := refs{
		branches:   branches,
		owned:      ownedBranches(repo.AFKBranchPattern, repo.ManualBranchPrefix, append(starting, live...), repo.Name),
		wtByBranch: map[string]string{},
	}
	for _, wt := range wts {
		if wt.Branch != "" && gitx.ManagedBranch(repo.AFKBranchPattern, repo.ManualBranchPrefix, wt.Branch) {
			r.wtByBranch[wt.Branch] = wt.Path
		}
	}
	return r, len(branches) > 0, nil
}

// Discard is the ONE unguarded destroyer (pinned API: POST
// /api/v1/repos/{id}/parked/discard {branch} → 204). It force-removes a
// managed branch's worktree and force-deletes the branch, bypassing the guarded
// teardown on purpose — but only after a managed-branch check (never main or a
// human's branch). Any live session on the branch is killed and its run
// terminated first. Discarding a claim branch returns its issue to the
// claimable set (M5).
func (s *Service) Discard(ctx context.Context, repoID, branch string) error {
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return err
	}
	if !gitx.ManagedBranch(repo.AFKBranchPattern, repo.ManualBranchPrefix, branch) {
		return &BadRequestError{msg: "not a parked branch: " + branch}
	}
	bareDir := s.bareDir(repo.ID)

	s.killSessionOnBranch(ctx, repo, branch)

	// Worktree first — git refuses to delete a branch still checked out
	// somewhere. Both underlying ops are already --force / -D.
	wts, err := s.git.Worktrees(ctx, bareDir, s.gitEnv)
	if err != nil {
		return err
	}
	for _, wt := range wts {
		if wt.Branch == branch {
			if err := s.git.RemoveWorktree(ctx, bareDir, wt.Path, s.gitEnv); err != nil {
				return fmt.Errorf("remove worktree %s: %w", wt.Path, err)
			}
			break
		}
	}
	if err := s.git.DeleteBranch(ctx, bareDir, branch, s.gitEnv); err != nil {
		return fmt.Errorf("delete branch %s: %w", branch, err)
	}

	s.publishParkedChanged(repoID)
	// Repo-scoped, no runID (issue #175): a discard is keyed by branch — any
	// run it incidentally terminated is not what the event is about.
	s.publishRunChanged(repoID, "")
	return nil
}

// killSessionOnBranch stops any live session of the repo whose derived branch
// is branch, and terminates its active run (outcome 'stopped', tokens deleted,
// credential files removed) — the "kill session if any" half of the unguarded
// discard. Best-effort throughout.
func (s *Service) killSessionOnBranch(ctx context.Context, repo store.Repo, branch string) {
	live, err := s.runner.List(ctx)
	if err != nil {
		return
	}
	for _, name := range live {
		if tmuxx.IsLoginSession(name) || !gitx.BelongsTo(name, repo.Name) {
			continue
		}
		_, label := gitx.ParseSessionName(name)
		if instanceBranch(repo.AFKBranchPattern, repo.ManualBranchPrefix, label) != branch {
			continue
		}
		if err := s.runner.Stop(ctx, name); err != nil {
			s.log.Warn("discard: stop session", "component", "reconcile", "session", name, "err", err)
		}
		// Container backstop (issue #205): the discard kill is a session kill
		// like Stop's, so it gets the same forced rm behind it — a container
		// pane's CLI ignoring SIGHUP must not leave the container running
		// against a force-deleted worktree.
		s.removeRunContainer(ctx, name)
		run, err := s.store.RunBySession(ctx, name)
		if err != nil {
			continue // no active run for this session (already reaped)
		}
		now := s.now()
		if err := s.store.EndRun(ctx, run.ID, store.RunOutcomeStopped, now, "discarded"); err != nil {
			s.log.Warn("discard: end run", "component", "reconcile", "run", run.ID, "err", err)
		} else if s.afkRunEnded != nil && run.Kind != store.RunKindManual {
			// This kill is a terminal-outcome writer like the reaper/Stop
			// chokepoints, so it reports to lab_afk_runs_total the same way for
			// every reaper-owned kind (afk_manual, afk_auto, lander). Manual-kind
			// runs stay outside the metric's label vocabulary.
			s.afkRunEnded(run.Kind, store.RunOutcomeStopped, now.Sub(run.StartedAt))
		}
		if err := s.store.DeleteRunTokens(ctx, run.ID); err != nil {
			s.log.Warn("discard: delete run tokens", "component", "reconcile", "run", run.ID, "err", err)
		}
		if repo.CredentialID != nil {
			if err := s.mat.Cleanup(*repo.CredentialID, run.ID); err != nil {
				s.log.Warn("discard: cleanup credential", "component", "reconcile", "run", run.ID, "err", err)
			}
		}
		// Wipe the discarded run's private per-run tree NOW (issues #202/#205):
		// its credential files and provider state must not wait out the
		// age-based sweep a prior refactor left them to — a discard is a
		// terminal outcome like Stop/reap, and those wipe immediately.
		if err := s.homes.Wipe(run.ID); err != nil {
			s.log.Warn("discard: wipe instance home", "component", "reconcile", "run", run.ID, "err", err)
		}
	}
}
