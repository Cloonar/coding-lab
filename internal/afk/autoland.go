package afk

// The autoland poller (issue #181 / ADR-0048): the state-derived sweep that
// spawns lander runs. Nothing is message-passed (ADR-0015 poll-only stands):
// every spawn is a pure function of polled forge state — the pull listing,
// its native reviews, its verdict-marker comments — plus the runs store, so a
// restart loses nothing and a re-sweep re-derives the same decision. The
// sweep rides the reaper tick (ReaperLoop, right after ReapOnce) and only
// ever spawns onto a VIRGIN claim PR: any verdict marker, any word, means
// verdict state exists and belongs to the fix-forward loop (#182); any live
// native review means a human is already in the loop.

import (
	"context"
	"errors"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// AutolandOnce is one autoland poller sweep: for every autoland-enabled,
// forge-bound, ready, un-paused repo with a logged-in lander provider, walk
// the open pulls and spawn a lander for each virgin claim PR (open, head
// matching the claim-branch pattern, no live review, no verdict marker, no
// active run on the branch). Per-repo and per-pull errors log and skip —
// mirroring ScheduleOnce, one bad repo never aborts the sweep — except
// instance.ErrOverCap, which LaunchLander proved against fresh liveness and
// which stops the whole sweep this tick (retried next tick).
func (s *Service) AutolandOnce(ctx context.Context) {
	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("autoland: list repos", "component", "afk", "err", err)
		return
	}
	// Cheap pre-filter, mirroring ScheduleOnce: only autoland-on, forge-bound,
	// ready repos can spawn, so an autoland-less fleet costs one repo listing
	// per tick and nothing else.
	var candidates []store.Repo
	for _, r := range repos {
		if r.AutolandEnabled && r.TrackerBinding == store.TrackerBindingForge && r.CloneStatus == store.CloneStatusReady {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return
	}

	// Forced-auth gate, once per distinct provider per tick (the ScheduleOnce
	// rule): a logged-out lander would adopt the claim into a session that
	// dies at the login wall — reaped as a death, a three-strikes strike for
	// nothing.
	loggedIn := map[string]bool{}

	live, err := s.runner.List(ctx)
	if err != nil {
		s.log.Warn("autoland: list sessions", "component", "afk", "err", err)
		return
	}
	liveCount := instance.LiveInstanceCount(live)

	for _, candidate := range candidates {
		// Re-read rather than trust the pre-filter snapshot: honours an
		// autoland toggle-off (or settings change) landing mid-tick.
		repo, err := s.store.RepoByID(ctx, candidate.ID)
		if err != nil {
			s.log.Warn("autoland: repo", "component", "afk", "repo", candidate.Name, "err", err)
			continue
		}
		// The repo half of the spawn decision, judged with the pull half
		// all-clear: when the repo alone already vetoes, no forge read is
		// spent on it — the pull facts cannot rescue a vetoed repo.
		base := LanderSpawnDecision{
			AutolandEnabled: repo.AutolandEnabled,
			ForgeBound:      repo.TrackerBinding == store.TrackerBindingForge,
			RepoReady:       repo.CloneStatus == store.CloneStatusReady,
			Paused:          repo.ConsecutiveFailures >= PauseThreshold,
			UnderCap:        liveCount < s.instances.EffectiveCap(ctx, repo),
			PullOpen:        true,
			ClaimBranch:     true,
		}
		if !ShouldSpawnLander(base) {
			continue
		}
		// The lander-effective provider gates the auth pre-check: the
		// lander_provider override when set, else the repo's base chain — the
		// same resolution LaunchLander re-makes authoritatively (and strictly)
		// under the lock.
		landerProv := ""
		if repo.LanderProvider != nil {
			landerProv = *repo.LanderProvider
		}
		prov, err := s.instances.ResolveProvider(ctx, repo, store.RunKindLander, landerProv)
		if err != nil {
			s.log.Warn("autoland: resolving provider", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		if _, checked := loggedIn[prov.ID()]; !checked {
			st, _ := prov.AuthStatus(ctx, true)
			loggedIn[prov.ID()] = st.LoggedIn
		}
		if !loggedIn[prov.ID()] {
			continue
		}
		trk, err := s.trackers.TrackerFor(ctx, repo)
		if err != nil {
			s.log.Warn("autoland: tracker", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		pulls, err := trk.Pulls(ctx)
		if err != nil {
			s.log.Warn("autoland: list pulls", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		for _, pull := range pulls {
			d := base
			d.PullOpen = pull.State == tracker.PullOpen
			issueN, matched := gitx.MatchBranch(repo.AFKBranchPattern, pull.HeadBranch)
			d.ClaimBranch = matched
			if !d.PullOpen || !d.ClaimBranch {
				continue // never a candidate — skip before the per-pull forge reads
			}
			// The two per-pull forge reads, bounded to open claim PRs. A
			// failed read skips THIS pull this tick (retried next tick):
			// fail closed, never spawn on unknown verdict state.
			reviews, err := trk.Reviews(ctx, pull.Number)
			if err != nil {
				s.log.Warn("autoland: reviews", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
				continue
			}
			d.ReviewPresent = LiveReview(reviews)
			comments, err := trk.PullComments(ctx, pull.Number)
			if err != nil {
				s.log.Warn("autoland: pull comments", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
				continue
			}
			d.VerdictPresent = len(VerdictWords(comments)) > 0
			// The runs-store gate: ANY active run on the branch — the
			// authoring AFK run still idling at its composer, or a lander
			// already working it — suppresses the spawn.
			onBranch, err := s.store.ActiveRunOnBranch(ctx, repo.ID, pull.HeadBranch)
			if err != nil {
				s.log.Warn("autoland: active run on branch", "component", "afk", "repo", repo.Name, "branch", pull.HeadBranch, "err", err)
				continue
			}
			d.RunOnBranch = onBranch
			d.UnderCap = liveCount < s.instances.EffectiveCap(ctx, repo)
			if !ShouldSpawnLander(d) {
				continue
			}
			if err := s.LaunchLander(ctx, repo.ID, pull.Number, pull.HeadBranch, issueN); err != nil {
				if errors.Is(err, instance.ErrOverCap) {
					// LaunchLander proved fresh liveness reached the cap: a
					// lander launch claims nothing, so stopping the whole
					// sweep here costs nothing — next tick retries.
					s.log.Warn("autoland: at the live-instance cap; sweep stops this tick",
						"component", "afk", "repo", repo.Name)
					return
				}
				s.log.Warn("autoland: launch lander", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
				continue
			}
			liveCount++ // the lander consumed a slot; later candidates see it
		}
	}
}
