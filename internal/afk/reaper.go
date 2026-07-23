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
		// The spawn pass rides the same tick, AFTER the reap — ordering is
		// load-bearing, and since issue #185 drain-then-fill is literal: the
		// reap frees run rows and cap slots first, then the ONE pass spends
		// them highest-stage-first, so a fresh PR gets its lander within one
		// tick (issue #181 AC #2) and never loses the slot to new work.
		s.SpawnOnce(ctx)
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
// a budget). Each run's done-signal is one bounded PullsForHead read (the
// verdict kinds add one bounded PullComments read — verdictDoneSignal),
// UNLOCKED (the slow part) — O(active runs) tracker requests per tick,
// independent of repo history (issue #176: the old once-per-repo Pulls call
// walked the ENTIRE PR history, growing a page per 50 merged PRs); the
// per-run decision is remade under runsMu from a fresh row read and fresh
// liveness — never from this loop's snapshot. A run whose tracker can't be
// read this tick is skipped (it is reclassified next tick; NEVER classified
// on missing data) rather than failing its repo's other runs or the whole
// sweep.
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
		for _, run := range byRepo[repoID] {
			// The done-signal, derived per kind and fed into the ONE Classify
			// table. AFK kinds: an open or merged PR/CR whose head is the
			// run's branch (closed-unmerged deliberately does NOT count —
			// tracker.DonePull) — plain presence, never a comment read. The
			// verdict kinds (lander/fix/escalate, issue #182): their PR
			// pre-exists the run, so presence alone would success-reap on the
			// first tick — the done-signal is the marker state the run
			// PRODUCED (verdictDoneSignal, one reading per kind). Candidates
			// come from one bounded per-branch lookup, never a history walk
			// (issue #176) — sound for the verdict kinds too: their
			// pre-existing PR was created by the same agentapi handlePRCreate
			// (head = the run's branch, base = the repo default branch), and
			// its head branch outlives the run (MergePull never deletes the
			// head branch, and reap-time teardown runs only after this read),
			// so PullsForHead's LIVE-branch contract holds for every kind.
			// Base is repo.DefaultBranch because that is the base the agent
			// API pins for EVERY lab-created PR (the caller names neither).
			pulls, err := trk.PullsForHead(ctx, run.Branch, repo.DefaultBranch)
			if err != nil {
				// Per-run isolation (tighter than the old per-repo skip): a
				// run whose tracker can't be read this tick is never
				// classified on missing data — it is reclassified next tick.
				// Only this run waits; its repo's other runs still get their
				// own reads this tick.
				s.log.Warn("afk watcher: pulls for head", "component", "afk", "repo", repo.Name, "session", run.SessionName, "err", err)
				continue
			}
			// donePull is the winning pull, meaningful only at a success/
			// escalated outcome (it feeds the done-signal and escalation
			// notifications).
			donePull, prPresent := tracker.DonePull(pulls, run.Branch)
			done, escalated := prPresent, false
			switch run.Kind {
			case store.RunKindLander, store.RunKindFix, store.RunKindEscalate:
				var readable bool
				done, escalated, readable = s.verdictDoneSignal(ctx, trk, repo, run, donePull, prPresent)
				if !readable {
					continue // comments unreadable this tick — NEVER classify on missing data
				}
			}
			outcome, alive, claimed := s.classifyAndClaim(ctx, run, done, escalated, now)
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
		// Container backstop (issue #205): a drained zombie may be a container
		// pane whose podman client also ignored the earlier kill.
		s.removeRunContainer(ctx, name)
		s.log.Info("afk watcher: drained zombie session", "component", "afk", "session", name)
	}
}

// verdictDoneSignal derives a verdict-kind run's done-signal (issues #181/
// #182 / ADR-0048) — the per-kind pure reading over polled state, needed
// because these runs' PRs pre-exist them: presence alone would success-reap
// each on its first tick. Lander: the PR merged, or the LAST verdict word
// pass/reject (LanderDone). Fix: merged — a human merged mid-run, the repair
// is moot and moot is done — or the last word fix-done, its `labctl pr
// rerequest` landed (FixDone). Escalate: merged is plain done; the escalate
// marker as the last word is done-VIA-MARKER (EscalateDelivered), which
// classifyAndClaim promotes to outcome 'escalated'. Every last-word reading
// is safe because the active-run gate (DecideAutoland case 2) admits one
// marker writer per branch at a time. PullComments is ONE bounded call per
// active run per tick, made only when the PR exists and is not merged; a
// read failure returns readable=false — the run is skipped and re-derived
// next tick, never classified on missing data (the same stance as a failed
// PullsForHead read).
func (s *Service) verdictDoneSignal(ctx context.Context, trk tracker.Tracker, repo store.Repo, run store.Run, pull tracker.PullRef, prPresent bool) (done, escalated, readable bool) {
	var verdicts []string
	if prPresent && pull.State != tracker.PullMerged {
		comments, err := trk.PullComments(ctx, pull.Number)
		if err != nil {
			s.log.Warn("afk watcher: pull comments", "component", "afk",
				"repo", repo.Name, "session", run.SessionName, "pull", pull.Number, "err", err)
			return false, false, false
		}
		verdicts = VerdictWords(comments)
	}
	switch run.Kind {
	case store.RunKindFix:
		return FixDone(pull.State, prPresent, verdicts), false, true
	case store.RunKindEscalate:
		done, viaMarker := EscalateDelivered(pull.State, prPresent, verdicts)
		return done, viaMarker, true
	default: // lander
		return LanderDone(pull.State, prPresent, verdicts), false, true
	}
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
//
// done is the run's kind-derived done-signal (AFK: PR presence; the verdict
// kinds: verdictDoneSignal) — the classification table itself is kind-blind.
// escalated is EscalateDelivered's viaMarker (only an escalate run's
// done-signal ever sets it): a success whose signal was the escalate marker
// is promoted to OutcomeEscalated here, still under runsMu and with the same
// claim semantics, so the terminal row IS the poller's permanent-terminality
// gate (issue #182). A merge-first escalate success stays plain success.
func (s *Service) classifyAndClaim(ctx context.Context, run store.Run, done, escalated bool, now time.Time) (outcome Outcome, alive, claimed bool) {
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
	// The escalated promotion (issue #182): Classify stays kind-blind and
	// never returns OutcomeEscalated; a marker-delivered escalate success is
	// promoted before the claim so the row written below is 'escalated'.
	if outcome == OutcomeSuccess && escalated {
		outcome = OutcomeEscalated
	}
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
// This is the ONLY place the run lifecycle writes the failure counter, and
// only the UNATTENDED-WORK kinds feed it — the two AFK kinds (manual, auto;
// #185) plus fix (#182): a success reap resets it (re-arming a three-strikes
// pause), a death or timeout increments it; at PauseThreshold the auto
// scheduler stops launching and manual starts 409 until reset. A fix run
// counts because it IS unattended AFK-class work continuing an AFK claim
// (the issue pins "three-strikes accounting apply"): its death is the same
// evidence of broken unattended runs an AFK death is, and its success — the
// repair pushed and re-requested — is the same evidence of health. The two
// VALIDATION-class kinds, lander and escalate, move the counter in NEITHER
// direction — the switch below used to be kind-agnostic and it ran backwards
// both ways. A flaky lander's death/timeout would increment the counter and
// pause the repo's UNRELATED AFK loop (and 409 its manual starts); and a
// lander correctly posting `reject` reaps as OutcomeSuccess, which would
// RESET the counter — clearing the strikes broken AFK runs earned and
// re-arming the very brake that evidence should trip (a reject is evidence
// FOR the pause, not against it). The same both-ways argument covers the
// escalate run: its 'escalated' outcome is it DOING ITS JOB, never health
// evidence either way. The counter is unattended-run health alone; only the
// failure counter separates — the budget clock stays deliberately shared. A
// neutral Stop never reaches here (classifyAndClaim refuses a stopped run
// under runsMu). now is the tick time the claim stamped as ended_at, so the
// metrics duration below is exactly started_at→ended_at.
//
// This chokepoint is also where the pushes fire — beside the metrics report,
// once per claim: the done-signal push on an AFK success, the escalation push
// on an escalated outcome (#182). trk and donePull are meaningful only at
// those outcomes (donePull is the winning pull the notification names);
// death, timeout, and the three-strikes pause never send.
func (s *Service) reapRun(ctx context.Context, trk tracker.Tracker, repo store.Repo, run store.Run, outcome Outcome, alive bool, donePull tracker.PullRef, now time.Time) {
	// The reaper half of the M8 terminal-outcome metrics (the neutral Stop
	// is the other half — StopAFK). The outcome row is already written, so
	// this reports exactly once per terminal reap.
	s.metrics.AFKRunEnded(run.Kind, outcome.RunOutcome(), now.Sub(run.StartedAt))

	// The done-signal push (issue #100): only a success reap of the two AFK
	// kinds notifies, riding this once-per-run chokepoint so the injected
	// sender fires exactly once. The verdict kinds are excluded by
	// constraint: the copy is "<run> opened PR #n" and a lander opened
	// nothing — its forge-observable outcome (the merge or verdict comment)
	// IS its notification surface for now — and a fix run's success surface
	// is its fix-done marker plus the native reviewer ping `labctl pr
	// rerequest` already sent (#182): no push.
	if outcome == OutcomeSuccess && s.notify != nil &&
		(run.Kind == store.RunKindAFKManual || run.Kind == store.RunKindAFKAuto) {
		s.notify(s.doneNotification(ctx, trk, repo, run, donePull))
	}

	// The escalation push (issue #182): fired once per escalated reap off the
	// same idempotent claim — the operator's signal that the fix-forward loop
	// handed the PR to a human (the agent-side ready-for-human flip already
	// executed by the time its terminal marker landed). donePull is the open
	// PR the marker landed on: EscalateDelivered promotes only via marker,
	// which requires an open, present PR.
	if outcome == OutcomeEscalated && s.notify != nil {
		s.notify(s.escalationNotification(ctx, trk, repo, run, donePull))
	}

	if alive {
		if err := s.runner.Stop(ctx, run.SessionName); err != nil {
			s.log.Warn("afk reap: stop session", "component", "afk", "session", run.SessionName, "err", err)
		}
	}
	// Container backstop (issue #205), run even for a dead session: a
	// container pane's CLI can exit while the podman client (or the container
	// under a SIGHUP-deaf CLI) lingers holding the deterministic name.
	// --ignore keeps the common nothing-there case a no-op.
	s.removeRunContainer(ctx, run.SessionName)
	s.git.TeardownGuarded(ctx, s.log, s.bareDir(repo.ID), run.WorktreePath, run.Branch, repo.DefaultBranch, s.gitEnv)

	// Terminal-outcome cleanup (§3a): the run's tokens must 401 immediately.
	if err := s.store.DeleteRunTokens(ctx, run.ID); err != nil {
		s.log.Warn("afk reap: delete run tokens", "component", "afk", "run", run.ID, "err", err)
	}
	// Wipe the run's private per-run tree (issues #202/#205): the credential
	// copy, config, AND the runtime dir's materialized git credential files go
	// with the reaped run (the wipe subsumes the old per-file credential
	// cleanup). The guarded teardown above owns the worktree/branch; this
	// removes only the per-run tree. A failure logs and continues (the
	// boot/runtime sweep is the backstop).
	if err := s.homes.Wipe(run.ID); err != nil {
		s.log.Warn("afk reap: wipe instance home", "component", "afk", "run", run.ID, "err", err)
	}

	// Only the unattended-work kinds move the shared failure counter — the
	// two AFK kinds (#185) and fix (#182); lander and escalate feed it in
	// NEITHER direction. See the doc comment for why both directions are
	// wrong for the validation-class kinds. An escalated outcome never
	// reaches an arm below even for its own kind — it is neither success nor
	// death/timeout — so the switch stays three-armed.
	if run.Kind == store.RunKindAFKManual || run.Kind == store.RunKindAFKAuto || run.Kind == store.RunKindFix {
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
