package afk

import (
	"context"
	"fmt"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// StopAFK is the neutral Stop for an AFK run (design §4c; v0 stopInstance's
// AFK arm): strictly neither success nor failure. Under runsMu — the same
// lock the reaper's classifyAndClaim holds — it moves the run to outcome
// 'stopped', deletes its run tokens, and only THEN kills the session, as one
// atomic step against a racing reap. The worktree and the claim branch are
// KEPT (the issue stays claimed/parked and inspectable), the failure counter
// is untouched, no teardown runs, and the reaper can never reclassify the
// run afterwards (it re-reads the row under the lock and finds it terminal).
//
// The instance service delegates here for every AFK-kind session
// (instance.AFKStopper). store.ErrNotFound when the session has no active
// run — already stopped or reaped.
func (s *Service) StopAFK(ctx context.Context, session string) error {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()

	run, err := s.store.RunBySession(ctx, session)
	if err != nil {
		return err // ErrNotFound → already terminal
	}
	// Every unattended, reaper-owned kind stops here — the two AFK kinds,
	// lander (issue #181), and fix/escalate (issue #182): the same
	// park-the-claim semantics apply, since an adopted PR head branch must
	// survive a Stop exactly like a claim branch.
	if run.Kind == store.RunKindManual {
		return fmt.Errorf("session %q is not an AFK run", session)
	}

	// Outcome transition FIRST, tmux kill second (§4c): a reaper tick that
	// wins the lock after us sees a terminal row and leaves the run alone. The
	// kill below is best-effort — if it fails (or lab crashes before it), the
	// session is left live with a terminal row, which neither startup
	// re-adoption (it only marks unmatched ACTIVE rows dead, never kills a
	// live session) nor the operator (DELETE /instances/{session} 404s once
	// the row is terminal) recovers. The reaper's zombie drain (drainZombies)
	// does: it retries the kill next tick for any live, known-repo session
	// with no active run row that is not mid-Launch.
	now := s.now()
	if err := s.store.EndRun(ctx, run.ID, store.RunOutcomeStopped, now, ""); err != nil {
		return err
	}
	// The Stop half of the M8 terminal-outcome metrics (reapRun is the
	// reaper half): the outcome write above succeeded, so the run is
	// terminally 'stopped' exactly once.
	s.metrics.AFKRunEnded(run.Kind, store.RunOutcomeStopped, now.Sub(run.StartedAt))
	if err := s.store.DeleteRunTokens(ctx, run.ID); err != nil {
		s.log.Warn("afk stop: delete run tokens", "component", "afk", "run", run.ID, "err", err)
	}
	if err := s.runner.Stop(ctx, session); err != nil {
		s.log.Warn("afk stop: stop session", "component", "afk", "session", session, "err", err)
	}
	// Container backstop (issue #205): forced rm behind the tmux kill, for a
	// provider CLI that ignored the SIGHUP --rm relies on. No-op for host
	// sessions and when container wiring is absent.
	s.removeRunContainer(ctx, session)
	// Wipe the run's private per-run tree (issues #202/#205) — its credential
	// copy, config, transcripts, AND the runtime dir holding the materialized
	// git credential files (the wipe subsumes the old per-file credential
	// cleanup). The park (worktree + claim branch) is untouched; only the
	// per-run tree goes. A failure logs and continues (the boot/runtime
	// sweep is the backstop).
	if err := s.homes.Wipe(run.ID); err != nil {
		s.log.Warn("afk stop: wipe instance home", "component", "afk", "run", run.ID, "err", err)
	}

	s.publishRun(EventRunChanged, run.RepoID, run.ID)
	s.publish(EventParkedChanged, run.RepoID)
	return nil
}
