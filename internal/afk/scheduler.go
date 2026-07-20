package afk

import (
	"context"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// SchedulerLoop is the engine's second long-lived goroutine (v0
// runAFKScheduler): every afk_schedule_seconds (re-read per tick — D12c) it
// runs one SpawnOnce pass. Since issue #185 the loop makes no launch decision
// of its own — the ONE priority-ordered spawn pass owns every spawn — so this
// cadence is an additional fill heartbeat: new work gets considered at least
// this often even when the reaper tick is quiet. Deliberately a separate
// cadence from the reaper (v0 parity: 45s vs 30s). Blocks until ctx is
// cancelled. Toggle-on and reset kicks may additionally invoke SpawnOnce
// directly — the pass is serialized under its own lock and every claim is
// single-flighted under the engine lock, so kicks are race-safe against both
// tickers.
func (s *Service) SchedulerLoop(ctx context.Context) {
	for {
		tick := s.settingSeconds(ctx, store.SettingAFKScheduleSeconds, defaultScheduleSeconds)
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		}
		s.SpawnOnce(ctx)
	}
}

// newWorkCandidates is the new-work producer (issue #185: the old scheduler
// sweep, v0 scheduleAFKRuns, now gathering only — it never launches and never
// decides the cap): for every auto-enabled ready repo with a claimable issue,
// no auto run in flight, and a live provider login, emit one StageNewWork
// candidate whose closure drives the shared claim path. Serial per repo
// (launch re-checks one-auto-per-repo under the lock), the loop drains the
// queue one issue per pass and idles when nothing is claimable. A per-repo
// tracker error skips just that repo (retried next pass), never the whole
// gather. repos, liveCount, and loggedIn are the pass's shared listing,
// liveness snapshot, and forced-auth memo.
func (s *Service) newWorkCandidates(ctx context.Context, repos []store.Repo, liveCount int, loggedIn map[string]bool) []spawnCandidate {
	// Cheap pre-filter: only auto-on repos with a usable clone can launch,
	// so an all-manual fleet costs nothing beyond the pass's one repo
	// listing.
	var enabled []store.Repo
	for _, r := range repos {
		if r.AFKAutoEnabled && r.CloneStatus == store.CloneStatusReady {
			enabled = append(enabled, r)
		}
	}
	if len(enabled) == 0 {
		return nil
	}

	var out []spawnCandidate
	for _, candidate := range enabled {
		// Re-read rather than trust the pre-filter snapshot: honours a
		// toggle-off (or settings change) landing mid-pass.
		repo, err := s.store.RepoByID(ctx, candidate.ID)
		if err != nil {
			s.log.Warn("spawn: repo", "component", "afk", "repo", candidate.Name, "err", err)
			continue
		}
		// At-cap production veto against the pass snapshot, BEFORE the
		// tracker reads — a load-shedding hint, never enforcement (the pass
		// owns the cap, #185): an at-cap repo costs zero forge reads.
		// Strictly better than the pre-#185 scheduler, which spent
		// ReadyIssues + FilterClaimable on a repo its own predicate was about
		// to veto on the cap term.
		if liveCount >= s.instances.EffectiveCap(ctx, repo) {
			continue
		}
		// The AFK-effective provider (issue #66) gates the auth pre-check; the
		// locked claim path re-resolves it authoritatively at launch.
		prov, err := s.instances.ResolveProvider(ctx, repo, store.RunKindAFKAuto, "")
		if err != nil {
			s.log.Warn("spawn: resolving provider", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		if _, checked := loggedIn[prov.ID()]; !checked {
			st, _ := prov.AuthStatus(ctx, true)
			loggedIn[prov.ID()] = st.LoggedIn
		}
		if !loggedIn[prov.ID()] {
			continue
		}
		// A repo whose tracker binding can't be driven (unsupported forge,
		// missing forge credential) simply cannot launch AFK runs — the M5
		// generalisation of v0's "not a git.cloonar.com repo" gate.
		trk, err := s.trackers.TrackerFor(ctx, repo)
		if err != nil {
			s.log.Warn("spawn: tracker", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		issues, err := trk.ReadyIssues(ctx)
		if err != nil {
			s.log.Warn("spawn: ready issues", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		// The CLAIMABLE queue — ready issues minus existing claim branches
		// minus issues held back by an open `## Blocked by` ref (ADR-0042) —
		// not the raw ready count, so parked/in-flight/blocked issues read as
		// zero and can never loop. A per-repo open-set fetch failure skips only
		// THIS repo's pass (retried next pass), the same stance as every other
		// per-repo error in the gather.
		claimable, err := s.FilterClaimable(ctx, repo, trk, issues)
		if err != nil {
			s.log.Warn("spawn: filter claimable", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}

		_, autoInFlight, err := s.store.ActiveAutoRunForRepo(ctx, repo.ID)
		if err != nil {
			s.log.Warn("spawn: auto in flight", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		d := AutoDecision{
			AutoEnabled:  repo.AFKAutoEnabled,
			AutoInFlight: autoInFlight,
			ReadyExists:  len(claimable) > 0,
			Paused:       repo.ConsecutiveFailures >= PauseThreshold,
		}
		if !ShouldLaunchAuto(d) {
			continue
		}
		out = append(out, spawnCandidate{
			stage: StageNewWork,
			repo:  repo,
			label: "afk-auto " + repo.Name,
			launch: func(ctx context.Context) spawnOutcome {
				_, outcome, err := s.launch(ctx, repo.ID, true)
				if err != nil {
					s.log.Warn("spawn: launch", "component", "afk", "repo", repo.Name, "err", err)
					return spawnSkipped
				}
				switch outcome {
				case launchSpawned:
					return spawnSpawned
				case launchAtCap:
					return spawnAtCap
				default:
					// launchNoReady / launchAutoInFlight: a manual start or
					// another launch raced us between gather and spend; the
					// locked claim path is authoritative, so just move on.
					return spawnSkipped
				}
			},
		})
	}
	return out
}
