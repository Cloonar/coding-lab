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

// AFKStopper is the AFK engine's neutral-Stop seam (design §4c): the afk
// service owns Stop for AFK runs — outcome 'stopped' under the engine's
// stop-vs-reap lock, session killed after the outcome write, worktree and
// claim branch KEPT (parked), no failure-counter write, no teardown. The
// instance service only delegates; it is wired once at startup via
// SetAFKStopper (cmd/lab), before any request is served.
type AFKStopper interface {
	StopAFK(ctx context.Context, session string) error
}

// SetAFKStopper wires the AFK engine's neutral Stop into this service. Call
// once during startup wiring, before serving — the field is read without a
// lock.
func (s *Service) SetAFKStopper(stopper AFKStopper) { s.afkStop = stopper }

// Stop tears down a manual instance: kill the session, apply the guarded
// teardown rule to its worktree+branch, move the run to outcome 'stopped',
// delete its run tokens, and remove its materialized credential files. It
// returns OutcomeRemoved when the worktree went away, OutcomeParked when a
// dirty tree was kept. store.ErrNotFound → 404. A run of an AFK kind is
// delegated to the AFK engine's neutral Stop (§4c) and always reports
// OutcomeParked — worktree and claim branch stay; without an engine wired
// (a build without the instance stack) it is refused with
// ErrAFKStopUnsupported → 501.
func (s *Service) Stop(ctx context.Context, session string) (string, error) {
	run, err := s.store.RunBySession(ctx, session)
	if err != nil {
		return "", err // ErrNotFound → 404
	}
	if run.Kind != store.RunKindManual {
		if s.afkStop == nil {
			return "", ErrAFKStopUnsupported
		}
		if err := s.afkStop.StopAFK(ctx, session); err != nil {
			return "", err
		}
		return OutcomeParked, nil
	}
	repo, err := s.store.RepoByID(ctx, run.RepoID)
	if err != nil {
		return "", err
	}
	removed := s.teardownManualRun(ctx, run, repo)
	s.publishRunChanged(run.RepoID, run.ID)
	s.publishParkedChanged(run.RepoID)
	if removed {
		return OutcomeRemoved, nil
	}
	return OutcomeParked, nil
}

// StopAll tears down every instance of a repo (pinned POST
// /api/v1/repos/{id}/stop-all → {"stopped":n}), so no session outlives its
// repo — a forced repo delete drives this before dropping the bare clone. The
// provider login session is excluded (it belongs to no repo). Manual runs get
// the guarded teardown; AFK runs are delegated to the engine's neutral Stop
// (§4c — outcome 'stopped' under the stop-vs-reap lock, session killed, claim
// parked), never stranded. Best-effort per session — one teardown hiccup, a
// delegation error, or a missing AFK stopper never aborts the rest.
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
		if tmuxx.IsLoginSession(name) || !gitx.BelongsTo(name, repo.Name) {
			continue
		}
		run, err := s.store.RunBySession(ctx, name)
		if err != nil {
			continue // no active run for this session (history-only)
		}
		if run.Kind != store.RunKindManual {
			// AFK run: hand off to the engine's neutral Stop so a forced repo
			// delete cannot strand a live agent working against a deleted repo.
			// Best-effort — a nil stopper (build without the AFK stack) or a
			// delegation error is logged and skipped, never aborting the rest.
			if s.afkStop == nil {
				s.log.Warn("stop-all: no AFK stopper wired, AFK session left live", "component", "instance", "session", name)
				continue
			}
			if err := s.afkStop.StopAFK(ctx, name); err != nil {
				s.log.Warn("stop-all: stop AFK session", "component", "instance", "session", name, "err", err)
				continue
			}
			stopped++
			continue
		}
		s.teardownManualRun(ctx, run, repo)
		stopped++
	}
	if stopped > 0 {
		// Genuinely repo-scoped: many runs ended at once, so no runID (issue
		// #175) — the SPA refetches the repo's whole run list, as before.
		s.publishRunChanged(repoID, "")
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
