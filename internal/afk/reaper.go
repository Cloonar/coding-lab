package afk

import (
	"context"
	"errors"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// ReaperLoop is the engine's watcher goroutine (v0 watchAFKRuns): every
// afk_tick_seconds (re-read per tick — D12c) it reaps the live AFK runs, and
// on the sweep_interval_minutes throttle it additionally runs the runtime
// worktree/branch sweep (piggybacked here so lab keeps one janitorial
// goroutine, exactly as v0's watcher did). Blocks until ctx is cancelled.
// lastSweep starts at loop start: startup reconcile just swept, so the first
// runtime sweep fires ~one interval later, not on the first tick.
func (s *Service) ReaperLoop(ctx context.Context) {
	lastSweep := s.now()
	for {
		tick := s.settingSeconds(ctx, store.SettingAFKTickSeconds, defaultTickSeconds)
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		}
		now := s.now()
		s.ReapOnce(ctx, now)
		// The autoland poller rides the same tick, AFTER the reap — ordering is
		// load-bearing: the reap frees the authoring AFK run's row first, so a
		// fresh PR gets its lander within one tick (issue #181 AC #2).
		s.AutolandOnce(ctx)
		if s.sweep == nil {
			continue
		}
		if now.Sub(lastSweep) >= s.settingMinutes(ctx, store.SettingSweepIntervalMinutes, defaultSweepMinutes) {
			lastSweep = now
			s.sweep(ctx)
		}
	}
}

// ReapOnce performs one reaper sweep at time now (a parameter, never
// time.Now inside — deterministic under test; v0 reapAFKRuns). It classifies
// the active AFK runs, then drains any leftover zombie session — a kill that
// failed after a terminal outcome was written would otherwise leak a
// permanent, cap-consuming, un-stoppable session, so the drain runs every
// tick regardless of whether there were active runs.
func (s *Service) ReapOnce(ctx context.Context, now time.Time) {
	s.reapActiveRuns(ctx, now)
	s.drainZombies(ctx)
}

// reapActiveRuns classifies the persisted active AFK runs (D12a/b: the runs
// table IS the budget clock — no in-memory stamps, so a restart never resets
// a budget). Each repo's pulls are fetched once per tick, UNLOCKED (the slow
// part); the per-run decision is remade under runsMu from a fresh row read
// and fresh liveness — never from this loop's snapshot. A repo whose tracker
// can't be read this tick is skipped (its runs are reclassified next tick;
// NEVER classified on missing data) rather than failing the whole sweep.
func (s *Service) reapActiveRuns(ctx context.Context, now time.Time) {
	runs, err := s.store.ActiveAFKRuns(ctx)
	if err != nil {
		s.log.Warn("afk watcher: list active runs", "component", "afk", "err", err)
		return
	}
	if len(runs) == 0 {
		return
	}

	// Group by repo, preserving first-seen order (runs come back sorted by
	// session name, so a tick is deterministic).
	byRepo := map[string][]store.Run{}
	var order []string
	for _, run := range runs {
		if _, seen := byRepo[run.RepoID]; !seen {
			order = append(order, run.RepoID)
		}
		byRepo[run.RepoID] = append(byRepo[run.RepoID], run)
	}

	for _, repoID := range order {
		repo, err := s.store.RepoByID(ctx, repoID)
		if err != nil {
			s.log.Warn("afk watcher: repo", "component", "afk", "repo", repoID, "err", err)
			continue
		}
		trk, err := s.trackers.TrackerFor(ctx, repo)
		if err != nil {
			s.log.Warn("afk watcher: tracker", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		pulls, err := trk.Pulls(ctx)
		if err != nil {
			s.log.Warn("afk watcher: list pulls", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		for _, run := range byRepo[repoID] {
			// The done-signal, derived per kind and fed into the ONE Classify
			// table. AFK kinds: an open or merged PR/CR whose head is the
			// run's branch (closed-unmerged deliberately does NOT count —
			// tracker.DonePull). Lander: its PR pre-exists the run, so
			// presence alone would success-reap it on its first tick — its
			// done-signal is the state the run PRODUCED (merged, or a
			// pass/reject verdict marker; landerDoneSignal). Matched
			// client-side against the one pull listing; donePull is the
			// winning pull, meaningful only when the outcome is success (it
			// feeds the done-signal notification).
			donePull, prPresent := tracker.DonePull(pulls, run.Branch)
			done := prPresent
			if run.Kind == store.RunKindLander {
				var readable bool
				done, readable = s.landerDoneSignal(ctx, trk, repo, run, donePull, prPresent)
				if !readable {
					continue // comments unreadable this tick — NEVER classify on missing data
				}
			}
			outcome, alive, claimed := s.classifyAndClaim(ctx, run, done, now)
			if claimed {
				s.reapRun(ctx, trk, repo, run, outcome, alive, donePull, now)
			}
		}
	}
}

// drainZombies retries the tmux kill for any live session that belongs to a
// known repo but has no active run row and is not mid-Launch — v0's
// self-healing (its watcher lazily re-tracked any live afk session and
// re-reaped it). A tmux kill can fail after the terminal-outcome write in
// StopAFK (§4c) or reapRun; both are best-effort and give up after one
// attempt. The leftover is a LIVE session whose runs row is terminal, so it
// is never a reap candidate again (ActiveAFKRuns selects outcome='active'),
// absent from GET /instances, un-stoppable via DELETE /instances/{session}
// (RunBySession → 404), survives restart (readopt only marks unmatched ACTIVE
// rows dead, never kills a live session), and counts against the live-instance
// cap forever while it keeps burning tokens. Retrying the kill drains it and
// frees the cap within one tick.
//
// A mid-Launch session (startguard-marked: worktree/branch exist but the
// tmux session is not yet live, or the Start is rolling back) is deliberately
// left alone — killing it would race the launcher-owned rollback. Sessions
// with an active run row (a live manual instance or a run the reaper still
// owns) are left to their normal lifecycle. On an ambiguous DB error the
// session is skipped rather than killed.
func (s *Service) drainZombies(ctx context.Context) {
	live, err := s.runner.List(ctx)
	if err != nil {
		s.log.Warn("afk watcher: list sessions for zombie drain", "component", "afk", "err", err)
		return
	}
	if len(live) == 0 {
		return
	}
	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("afk watcher: list repos for zombie drain", "component", "afk", "err", err)
		return
	}
	known := make(map[string]bool, len(repos))
	for _, r := range repos {
		known[r.Name] = true
	}
	for _, name := range live {
		if tmuxx.IsLoginSession(name) {
			continue
		}
		repoName, _ := gitx.ParseSessionName(name)
		if !known[repoName] {
			continue // not a lab-managed session — never our concern
		}
		// Mid-Launch (row not yet written, or being rolled back): the
		// startguard owns it, so it is never a zombie.
		if s.guard.Has(name) {
			continue
		}
		if _, err := s.store.RunBySession(ctx, name); err == nil {
			continue // has an active run row — its normal lifecycle owns it
		} else if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("afk watcher: zombie run lookup", "component", "afk", "session", name, "err", err)
			continue // ambiguous DB error — do not kill on incomplete data
		}
		// Terminal row (or none) but the session is still live: a kill failed
		// after the outcome write. Retry it to free the cap.
		if err := s.runner.Stop(ctx, name); err != nil {
			s.log.Warn("afk watcher: drain zombie session", "component", "afk", "session", name, "err", err)
			continue
		}
		s.log.Info("afk watcher: drained zombie session", "component", "afk", "session", name)
	}
}

// landerDoneSignal derives a lander run's done-signal (issue #181 /
// ADR-0048) — the pure LanderDone reading over polled state: the PR merged,
// or a pass/reject verdict-marker comment on it (the spawn rule only launches
// onto a virgin PR, so any such marker is this run's; fix-done is a fix run's
// signal and never a lander's). PullComments is ONE bounded call per active
// lander per tick, made only when the PR exists and is not merged; a read
// failure returns readable=false — the run is skipped and re-derived next
// tick, never classified on missing data (the same stance as a failed pull
// listing).
func (s *Service) landerDoneSignal(ctx context.Context, trk tracker.Tracker, repo store.Repo, run store.Run, pull tracker.PullRef, prPresent bool) (done, readable bool) {
	if !prPresent || pull.State == tracker.PullMerged {
		return LanderDone(pull.State, prPresent, nil), true
	}
	comments, err := trk.PullComments(ctx, pull.Number)
	if err != nil {
		s.log.Warn("afk watcher: lander pull comments", "component", "afk",
			"repo", repo.Name, "session", run.SessionName, "pull", pull.Number, "err", err)
		return false, false
	}
	return LanderDone(pull.State, prPresent, VerdictWords(comments)), true
}

// classifyAndClaim makes the reap decision for one run atomically against a
// concurrent neutral Stop (v0's atomicity core, §4c). Under runsMu — the
// same lock StopAFK holds to write outcome 'stopped' and kill the session —
// it re-reads the run row and FRESH liveness, classifies, and, when the
// outcome is terminal, claims the run by writing it (EndRun touches only
// rows still outcome='active', so the DB double-guards the claim). claimed
// is false (leave the run alone) when the run is no longer active — a Stop
// neutralised it or a prior tick reaped it — when liveness can't be read
// this tick, or when it is simply still in progress.
//
// Because StopAFK writes the terminal outcome AND kills the session under
// this one lock, this never observes a dead-but-active run from a half-done
// Stop: it sees active-and-alive (a genuine fresh decision) or
// stopped-and-gone (claimed=false, neutral). A genuine crash leaves the row
// active, so an active run that is not alive is a real death.
// done is the run's kind-derived done-signal (AFK: PR presence; lander:
// landerDoneSignal) — the classification table itself is kind-blind.
func (s *Service) classifyAndClaim(ctx context.Context, run store.Run, done bool, now time.Time) (outcome Outcome, alive, claimed bool) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()

	fresh, err := s.store.RunBySession(ctx, run.SessionName)
	if err != nil || fresh.ID != run.ID {
		// No active row for this session (or a different, newer run owns the
		// name): a Stop already neutralised it or a prior tick reaped it.
		return OutcomeRunning, false, false
	}
	// Mid-Launch guard (§4b): the run's row is active but its tmux session
	// may not be live yet — instance.Launch marks the startguard BEFORE it
	// writes the runs row and clears only once the session is live OR its
	// rollback is complete. Reaping now would read the not-yet-spawned
	// session as dead (IsRunning=false) and tear the fresh claim's worktree
	// and branch down (a zero-commit fork reads clean+merged). Leave it: a
	// live run becomes a genuine reap candidate next tick, and a rolled-back
	// run's row is deleted so it is never a candidate again. Checked under
	// runsMu, so it is race-free against a concurrent Clear.
	if s.guard.Has(run.SessionName) {
		return OutcomeRunning, false, false
	}
	alive, err = s.runner.IsRunning(ctx, run.SessionName)
	if err != nil {
		// Can't read liveness (a transient tmux failure): don't reap this
		// tick. The run stays active and is reclassified next tick.
		s.log.Warn("afk watcher: liveness", "component", "afk", "session", run.SessionName, "err", err)
		return OutcomeRunning, false, false
	}
	deadline := now.Add(24 * time.Hour) // defensive: an AFK row always carries one
	if fresh.BudgetDeadline != nil {
		deadline = *fresh.BudgetDeadline
	}
	outcome = Classify(done, alive, now, deadline)
	if outcome == OutcomeRunning {
		return outcome, alive, false
	}
	if err := s.store.EndRun(ctx, fresh.ID, outcome.RunOutcome(), now, outcome.failureReason()); err != nil {
		s.log.Warn("afk watcher: end run", "component", "afk", "run", fresh.ID, "err", err)
		return OutcomeRunning, alive, false
	}
	return outcome, alive, true
}

// reapRun is the single chokepoint every terminal reaper outcome routes
// through for its side effects (v0 reapAFKRun; the outcome row is already
// written by the claim): stop a still-live session, apply the ONE guarded
// teardown to worktree + branch (identical for success and failure —
// dirty keeps everything, clean removes the worktree, the branch dies only
// when merged; so a clean failure leaves the unmerged claim branch and the
// issue stays parked), delete the run's tokens and credential files, update
// the repo's consecutive-failure counter, and publish the events.
//
// This is the ONLY place the run lifecycle writes the failure counter: a
// success reap resets it (re-arming a three-strikes pause), a death or
// timeout increments it; at PauseThreshold the auto scheduler stops
// launching and manual starts 409 until reset. The counter is kind-agnostic
// — manual and auto AFK runs both feed it. A neutral Stop never reaches
// here (classifyAndClaim refuses a stopped run under runsMu). now is the
// tick time the claim stamped as ended_at, so the metrics duration below is
// exactly started_at→ended_at.
//
// This chokepoint is also where the done-signal push fires — beside the
// metrics report, once per success reap. trk and donePull are meaningful only
// when outcome is success (donePull is the winning pull the notification names);
// death, timeout, and the three-strikes pause never send.
func (s *Service) reapRun(ctx context.Context, trk tracker.Tracker, repo store.Repo, run store.Run, outcome Outcome, alive bool, donePull tracker.PullRef, now time.Time) {
	// The reaper half of the M8 terminal-outcome metrics (the neutral Stop
	// is the other half — StopAFK). The outcome row is already written, so
	// this reports exactly once per terminal reap.
	s.metrics.AFKRunEnded(run.Kind, outcome.RunOutcome(), now.Sub(run.StartedAt))

	// The done-signal push (issue #100): only a success reap of the two AFK
	// kinds notifies, riding this once-per-run chokepoint so the injected
	// sender fires exactly once. A lander is excluded by constraint: the copy
	// is "<run> opened PR #n" and a lander opened nothing — its forge-
	// observable outcome (the merge or verdict comment) IS its notification
	// surface for now.
	if outcome == OutcomeSuccess && s.notify != nil &&
		(run.Kind == store.RunKindAFKManual || run.Kind == store.RunKindAFKAuto) {
		s.notify(s.doneNotification(ctx, trk, repo, run, donePull))
	}

	if alive {
		if err := s.runner.Stop(ctx, run.SessionName); err != nil {
			s.log.Warn("afk reap: stop session", "component", "afk", "session", run.SessionName, "err", err)
		}
	}
	s.git.TeardownGuarded(ctx, s.log, s.bareDir(repo.ID), run.WorktreePath, run.Branch, repo.DefaultBranch, s.gitEnv)

	// Terminal-outcome cleanup (§3a): the run's tokens must 401 immediately,
	// and its materialized credential files go with it.
	if err := s.store.DeleteRunTokens(ctx, run.ID); err != nil {
		s.log.Warn("afk reap: delete run tokens", "component", "afk", "run", run.ID, "err", err)
	}
	s.cleanupCredential(repo, run.ID)

	switch outcome {
	case OutcomeSuccess:
		changed, err := s.store.ResetRepoFailures(ctx, repo.ID)
		if err != nil {
			s.log.Warn("afk reap: reset failures", "component", "afk", "repo", repo.Name, "err", err)
		} else if changed {
			s.publish(EventRepoChanged, repo.ID)
		}
	case OutcomeDeath, OutcomeTimeout:
		n, err := s.store.IncrementRepoFailures(ctx, repo.ID)
		if err != nil {
			s.log.Warn("afk reap: increment failures", "component", "afk", "repo", repo.Name, "err", err)
		} else {
			if n >= PauseThreshold {
				s.log.Warn("afk auto loop paused after consecutive failures — reset from the UI to re-arm",
					"component", "afk", "repo", repo.Name, "failures", n)
			}
			s.publish(EventRepoChanged, repo.ID)
		}
	}

	s.publish(EventRunChanged, repo.ID)
	s.publish(EventParkedChanged, repo.ID)
	issue := 0
	if run.IssueNumber != nil {
		issue = *run.IssueNumber
	}
	s.log.Info("afk run reaped", "component", "afk",
		"repo", repo.Name, "issue", issue, "session", run.SessionName, "outcome", outcome.String())
}
