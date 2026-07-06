package afk

import (
	"context"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// SchedulerLoop is the engine's second long-lived goroutine (v0
// runAFKScheduler): every afk_schedule_seconds (re-read per tick — D12c) it
// runs one ScheduleOnce sweep. Deliberately a separate cadence from the
// reaper (v0 parity: 45s vs 30s). Blocks until ctx is cancelled. Toggle-on
// and reset kicks may additionally invoke ScheduleOnce directly — every
// claim is single-flighted under the engine lock, so kicks are race-safe
// against the ticker.
func (s *Service) SchedulerLoop(ctx context.Context) {
	for {
		tick := s.settingSeconds(ctx, store.SettingAFKScheduleSeconds, defaultScheduleSeconds)
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		}
		s.ScheduleOnce(ctx)
	}
}

// ScheduleOnce is one scheduler sweep (v0 scheduleAFKRuns): for every
// auto-enabled ready repo with a claimable issue, no auto run in flight,
// headroom under the cap, and a live provider login, launch the next run
// through the shared claim path. Serial per repo (launch re-checks
// one-auto-per-repo under the lock), it drains the queue one issue per
// launch, idles when nothing is claimable, and launches nothing — without
// erroring — at the cap. A per-repo tracker error skips just that repo
// (retried next tick), never the whole sweep.
func (s *Service) ScheduleOnce(ctx context.Context) {
	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("afk scheduler: list repos", "component", "afk", "err", err)
		return
	}
	// Cheap pre-filter: only auto-on repos with a usable clone can launch,
	// so an all-manual fleet costs one repo listing per tick and nothing
	// else.
	var candidates []store.Repo
	for _, r := range repos {
		if r.AFKAutoEnabled && r.CloneStatus == store.CloneStatusReady {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return
	}

	// Auth is gated per tick with a FORCED refresh, once per distinct
	// provider (v0 gated the whole tick): a logged-out auto-launch would
	// claim an issue into a session that dies at the login wall — reaped as
	// a death, parking the issue for nothing.
	loggedIn := map[string]bool{}

	live, err := s.runner.List(ctx)
	if err != nil {
		s.log.Warn("afk scheduler: list sessions", "component", "afk", "err", err)
		return
	}
	liveCount := instance.LiveInstanceCount(live)

	for _, candidate := range candidates {
		// Re-read rather than trust the pre-filter snapshot: honours a
		// toggle-off (or settings change) landing mid-tick.
		repo, err := s.store.RepoByID(ctx, candidate.ID)
		if err != nil {
			s.log.Warn("afk scheduler: repo", "component", "afk", "repo", candidate.Name, "err", err)
			continue
		}
		prov, ok := s.providers.Get(repo.Provider)
		if !ok {
			s.log.Warn("afk scheduler: provider not registered", "component", "afk", "repo", repo.Name, "provider", repo.Provider)
			continue
		}
		if _, checked := loggedIn[repo.Provider]; !checked {
			st, _ := prov.AuthStatus(ctx, true)
			loggedIn[repo.Provider] = st.LoggedIn
		}
		if !loggedIn[repo.Provider] {
			continue
		}
		// A repo whose tracker binding can't be driven (unsupported forge,
		// missing forge credential) simply cannot launch AFK runs — the M5
		// generalisation of v0's "not a git.cloonar.com repo" gate.
		trk, err := s.trackers.TrackerFor(ctx, repo)
		if err != nil {
			s.log.Warn("afk scheduler: tracker", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		issues, err := trk.ReadyIssues(ctx)
		if err != nil {
			s.log.Warn("afk scheduler: ready issues", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		claimed, err := s.claimedIssueSet(ctx, repo)
		if err != nil {
			s.log.Warn("afk scheduler: claim branches", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		// The CLAIMABLE queue — ready issues without an existing claim
		// branch — not the raw ready count, so parked/in-flight issues read
		// as zero and can never loop.
		claimable := ClaimableIssues(issues, claimed)

		_, autoInFlight, err := s.store.ActiveAutoRunForRepo(ctx, repo.ID)
		if err != nil {
			s.log.Warn("afk scheduler: auto in flight", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		d := AutoDecision{
			AutoEnabled:  repo.AFKAutoEnabled,
			UnderCap:     liveCount < s.instances.EffectiveCap(ctx, repo),
			AutoInFlight: autoInFlight,
			ReadyExists:  len(claimable) > 0,
			Paused:       repo.ConsecutiveFailures >= PauseThreshold,
		}
		if !ShouldLaunchAuto(d) {
			continue
		}
		_, outcome, err := s.launch(ctx, repo.ID, true)
		if err != nil {
			s.log.Warn("afk scheduler: launch", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		switch outcome {
		case launchSpawned:
			liveCount++ // this run consumed a slot; the next candidate sees it
		case launchAtCap:
			// launch() proved the fresh GLOBAL live count reached THIS repo's
			// effective cap. Under per-repo cap overrides (D12c) that no longer
			// implies every repo is at cap, so skip only this repo — a later
			// candidate whose cap still has headroom must still launch. Raise
			// the stale snapshot to this repo's cap first: a sound lower bound
			// (launch proved the fresh count is at least that) that lets later
			// pre-checks veto without another launch() call, and reproduces v0's
			// whole-tick stop whenever every repo shares the one global cap.
			if repoCap := s.instances.EffectiveCap(ctx, repo); repoCap > liveCount {
				liveCount = repoCap
			}
			continue
		}
		// launchNoReady / launchAutoInFlight: a manual start or another
		// sweep raced us between the predicate and the locked claim; move on.
	}
}
