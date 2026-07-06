package instance

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// Stop outcomes returned to the API (pinned DELETE /api/v1/instances/{session}
// → {"outcome":"removed"|"parked"}).
const (
	OutcomeRemoved = "removed" // clean worktree torn down (branch kept or deleted)
	OutcomeParked  = "parked"  // dirty worktree kept whole — unsaved work survives
)

// Stop tears down a manual instance: kill the session, apply the guarded
// teardown rule to its worktree+branch, move the run to outcome 'stopped',
// delete its run tokens, and remove its materialized credential files. It
// returns OutcomeRemoved when the worktree went away, OutcomeParked when a
// dirty tree was kept. store.ErrNotFound → 404; a run whose kind is AFK is the
// M5 engine's job and is refused with ErrAFKStopUnsupported → 501.
func (s *Service) Stop(ctx context.Context, session string) (string, error) {
	run, err := s.store.RunBySession(ctx, session)
	if err != nil {
		return "", err // ErrNotFound → 404
	}
	if run.Kind != store.RunKindManual {
		return "", ErrAFKStopUnsupported
	}
	repo, err := s.store.RepoByID(ctx, run.RepoID)
	if err != nil {
		return "", err
	}
	removed := s.teardownManualRun(ctx, run, repo)
	s.publishRunChanged(run.RepoID)
	s.publishParkedChanged(run.RepoID)
	if removed {
		return OutcomeRemoved, nil
	}
	return OutcomeParked, nil
}

// StopAll tears down every manual instance of a repo (pinned POST
// /api/v1/repos/{id}/stop-all → {"stopped":n}). The provider login session is
// excluded (it belongs to no repo); AFK sessions are left for the M5 engine.
// Best-effort per session — one teardown hiccup never aborts the rest.
func (s *Service) StopAll(ctx context.Context, repoID string) (int, error) {
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return 0, err
	}
	live, err := s.runner.List(ctx)
	if err != nil {
		return 0, err
	}
	stopped := 0
	for _, name := range live {
		if name == tmuxx.LoginSession || !gitx.BelongsTo(name, repo.Name) {
			continue
		}
		run, err := s.store.RunBySession(ctx, name)
		if err != nil || run.Kind != store.RunKindManual {
			continue // no active manual run for this session (AFK, or history-only)
		}
		s.teardownManualRun(ctx, run, repo)
		stopped++
	}
	if stopped > 0 {
		s.publishRunChanged(repoID)
		s.publishParkedChanged(repoID)
	}
	return stopped, nil
}

// teardownManualRun is the shared Stop body for a known manual run: kill the
// session (best-effort), run the guarded teardown, write the terminal outcome,
// delete the run's tokens, and clean its credential files. It reports whether
// the worktree was actually removed (drives removed-vs-parked). Never returns
// an error — a Stop must not fail on a teardown hiccup (the session is already
// gone; a leftover worktree is reclaimable by the sweeps).
func (s *Service) teardownManualRun(ctx context.Context, run store.Run, repo store.Repo) (removed bool) {
	if err := s.runner.Stop(ctx, run.SessionName); err != nil {
		s.log.Warn("stopping session", "component", "instance", "session", run.SessionName, "err", err)
	}
	action := s.git.TeardownGuarded(ctx, s.log, s.bareDir(repo.ID), run.WorktreePath, run.Branch, repo.DefaultBranch, s.gitEnv)

	if err := s.store.EndRun(ctx, run.ID, store.RunOutcomeStopped, s.now(), ""); err != nil {
		s.log.Warn("recording run stopped", "component", "instance", "run", run.ID, "err", err)
	}
	if err := s.store.DeleteRunTokens(ctx, run.ID); err != nil {
		s.log.Warn("deleting run tokens", "component", "instance", "run", run.ID, "err", err)
	}
	s.cleanupCredential(repo, run.ID)
	// v0's ForgetURL (drop the deep link when the worktree is gone) is subsumed
	// by the active-runs filter: a stopped run is terminal, so GET /instances
	// never surfaces its stale deep link.
	return action.RemoveWorktree
}
