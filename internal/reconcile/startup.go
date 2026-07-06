package reconcile

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// deathReasonAtStartup is the failure_reason written to an active run whose
// session did not survive the restart (§3b). consecutive_failures is left
// UNTOUCHED — downtime deaths are invisible to the three-strikes counter (v0
// parity).
const deathReasonAtStartup = "session gone at startup"

// StartupReconcile runs once, synchronously, before any scheduler goroutine
// (design §3b). Order is load-bearing:
//
//  1. Re-adoption FIRST: match runs.outcome='active' against live tmux — adopt
//     survivors (re-arm deep-link capture if the link is still NULL), mark the
//     rest dead. Finishing before the schedulers means no Start can race it.
//  2. Pass A/B per ready repo (guarded orphan teardown + bare-merged GC).
//  3. runtime/ credential keep-set cleanup: keep exactly the files of re-adopted
//     LIVE runs, unlink the rest (design §6 restart rule).
func (s *Service) StartupReconcile(ctx context.Context) error {
	liveRunIDs, err := s.readopt(ctx)
	if err != nil {
		return err
	}

	repos, err := s.readyRepos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		s.reconcileProject(ctx, repo)
	}

	if err := s.mat.CleanupAll(credentialKeep(liveRunIDs)); err != nil {
		s.log.Warn("sweeping runtime credential dir", "component", "reconcile", "err", err)
	}
	return nil
}

// readopt reconciles active runs against live tmux and returns the set of run
// ids whose session survived (the credential keep-set). A survivor keeps its
// budget clock untouched (D12b) and re-arms deep-link capture when its link is
// still NULL; a dead run gets a terminal 'death' outcome with the pinned reason,
// counters untouched, and worktree disposal left to Pass A/B.
func (s *Service) readopt(ctx context.Context) (map[string]bool, error) {
	active, err := s.store.ActiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	live, err := s.runner.List(ctx)
	if err != nil {
		return nil, err
	}
	set := liveSet(live)

	keep := make(map[string]bool)
	for _, run := range active {
		if set[run.SessionName] {
			keep[run.ID] = true
			if run.DeepLinkURL == nil || *run.DeepLinkURL == "" {
				if s.armCapture != nil {
					s.armCapture(run)
				}
			}
			continue
		}
		if err := s.store.EndRun(ctx, run.ID, store.RunOutcomeDeath, s.now(), deathReasonAtStartup); err != nil {
			s.log.Warn("marking dead run at startup", "component", "reconcile", "run", run.ID, "err", err)
			continue
		}
		s.publishRunChanged(run.RepoID)
	}
	return keep, nil
}

// keptRunIDs returns the run ids whose active run is live in tmux right now —
// the runtime credential keep-set (a lighter readopt used by the throttled
// sweep, which never marks runs dead: startup owns that).
func (s *Service) keptRunIDs(ctx context.Context) (map[string]bool, error) {
	active, err := s.store.ActiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	live, err := s.runner.List(ctx)
	if err != nil {
		return nil, err
	}
	set := liveSet(live)
	keep := make(map[string]bool)
	for _, run := range active {
		if set[run.SessionName] {
			keep[run.ID] = true
		}
	}
	return keep, nil
}

// credentialKeep builds the CleanupAll keep predicate from a set of run ids: a
// materialized file is kept iff its opID (the run id, per the credID.opID
// filename grammar) belongs to a live run. Everything else — orphaned by a
// finished/dead run — is unlinked.
func credentialKeep(liveRunIDs map[string]bool) func(filename string) bool {
	return func(filename string) bool {
		opID, ok := vault.OpIDFromFile(filename)
		return ok && liveRunIDs[opID]
	}
}
