package reconcile

import (
	"context"
	"time"
)

// RuntimeSweep is the throttled runtime pass (merged-only). Per ready repo, in
// scan order: a best-effort `git fetch origin` first (a failure logs and the
// sweep still runs on the last-known origin refs — never abort the GC), then
// sweepProject. It closes with the credential keep-set cleanup (design §6: the
// throttled sweep repeats the restart rule). Owned/starting sessions are never
// touched (gatherRefs unions them).
func (s *Service) RuntimeSweep(ctx context.Context) {
	repos, err := s.readyRepos(ctx)
	if err != nil {
		s.log.Warn("sweep: list repos", "component", "reconcile", "err", err)
		return
	}
	for _, repo := range repos {
		if err := s.git.Fetch(ctx, s.bareDir(repo.ID), s.gitEnv); err != nil {
			s.log.Warn("sweep: fetch, using last-known origin", "component", "reconcile", "repo", repo.Name, "err", err)
		}
		s.sweepProject(ctx, repo)
	}

	keep, err := s.keptRunIDs(ctx)
	if err != nil {
		s.log.Warn("sweep: keep-set", "component", "reconcile", "err", err)
		return
	}
	if err := s.mat.CleanupAll(credentialKeep(keep)); err != nil {
		s.log.Warn("sweep: runtime credential dir", "component", "reconcile", "err", err)
	}
}

// SweepLoop drives RuntimeSweep on a sweepTick ticker throttled to sweepThrottle
// (design §3a; port-spec §3.4). lastSweep is initialized at loop start so the
// first runtime sweep fires ~sweepThrottle after startup — NOT on the first
// tick — since startup reconcile just ran. Blocks until ctx is cancelled. This
// is a plain ticker the M5 reaper loop can absorb.
func (s *Service) SweepLoop(ctx context.Context) {
	ticker := time.NewTicker(sweepTick)
	defer ticker.Stop()
	lastSweep := s.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.now().Sub(lastSweep) < sweepThrottle {
				continue
			}
			lastSweep = s.now()
			s.RuntimeSweep(ctx)
		}
	}
}
