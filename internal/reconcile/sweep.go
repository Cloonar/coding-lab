package reconcile

import (
	"context"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
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

// deathReasonSessionGone is the failure_reason a runtime dead-session sweep
// (issue #93) writes for an active MANUAL run whose tmux session is gone.
// Deliberately distinct from deathReasonAtStartup (startup.go): the two
// sweeps run at different times, over different windows, and readers (the
// chat view, run history) benefit from telling a startup-restart death apart
// from a runtime one.
const deathReasonSessionGone = "session gone"

// sweepDeadSessions is the runtime sibling of readopt (issue #93): every tick,
// end any active store.RunKindManual run whose tmux session is gone. AFK kinds
// (afk_manual, afk_auto) are NEVER touched here — done-vs-death classification
// for AFK runs is exclusively the AFK reaper's, which has tracker context (a
// session gone + a PR present reads as done, not death) that this sweep does
// not have.
//
// Read order is load-bearing, exactly as readopt/gatherRefs pin it:
//
//  1. s.store.ActiveRuns — the store read FIRST. A run inserted after this
//     read is invisible to this tick and simply waits for the next one.
//  2. s.guard.Snapshot() — the starting set SECOND.
//  3. s.runner.List — tmux STRICTLY LAST.
//
// instance.Launch marks the startguard BEFORE it writes the runs row and
// clears it only once the session is live. So a run this pass sees was Marked
// before our Snapshot; if its session is not yet up, its name is still in the
// starting set and the union protects it. If it was Cleared before our
// Snapshot, its session was live before our later List and shows up there
// too. Reading Snapshot() strictly before List is the same discipline
// gatherRefs documents (gather.go:18-26) and the startguard package doc pins
// (internal/startguard/startguard.go:10-16) — a run mid-Start must never be
// reaped.
//
// A List error logs and returns without touching anything: tmuxx already
// treats a missing tmux server as zero sessions (not an error), so that case
// IS hard evidence every session is gone; an actual List error must never be
// misread the same way.
func (s *Service) sweepDeadSessions(ctx context.Context) {
	active, err := s.store.ActiveRuns(ctx)
	if err != nil {
		s.log.Warn("dead-session sweep: active runs", "component", "reconcile", "err", err)
		return
	}
	starting := s.guard.Snapshot()
	live, err := s.runner.List(ctx)
	if err != nil {
		s.log.Warn("dead-session sweep: list sessions", "component", "reconcile", "err", err)
		return
	}
	set := liveSet(append(starting, live...))

	for _, run := range active {
		if run.Kind != store.RunKindManual {
			continue
		}
		if set[run.SessionName] {
			continue
		}
		if err := s.store.EndRun(ctx, run.ID, store.RunOutcomeDeath, s.now(), deathReasonSessionGone); err != nil {
			s.log.Warn("dead-session sweep: end run", "component", "reconcile", "run", run.ID, "err", err)
			continue
		}
		s.publishRunChanged(run.RepoID, run.ID)
	}
}

// DeadSessionLoop drives sweepDeadSessions on a plain deadSessionTick ticker
// (issue #93) — it does NOT ride the reaper's throttled RuntimeSweep hook
// (sweep_interval_minutes, default 10m): a dead manual session should flip the
// chat view to ended within one short tick, not wait out a GC-cadence
// throttle. Issue #93 chose the "~30-45s ticker in the same family as
// SweepLoop" option over a new settings key; deadSessionTick is a fixed
// constant, matching the existing sweepTick idiom. Blocks until ctx is
// cancelled. Mirrors SweepLoop's shape minus the throttle.
func (s *Service) DeadSessionLoop(ctx context.Context) {
	ticker := time.NewTicker(deadSessionTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepDeadSessions(ctx)
		}
	}
}
