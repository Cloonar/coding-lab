package afk

// The autoland gather (issue #181 / ADR-0048): the state-derived walk that
// produces lander candidates for the fleet spawn pass (SpawnOnce, issue
// #185 — the walk itself launches nothing). Nothing is message-passed
// (ADR-0015 poll-only stands): every candidate is a pure function of polled
// forge state — the pull listing, its native reviews, its verdict-marker
// comments — plus the runs store, so a restart loses nothing and a re-pass
// re-derives the same decision. Candidates are only ever emitted for a
// VIRGIN claim PR: any verdict marker, any word, means verdict state exists
// and belongs to the fix-forward loop (#182); any live native review means a
// human is already in the loop.

import (
	"context"
	"errors"
	"fmt"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// landerCandidates is the lander producer (issue #185: the old autoland
// sweep, now gathering only — it never launches and never decides the cap):
// for every autoland-enabled, forge-bound, ready, un-paused repo with a
// logged-in lander provider, walk the open pulls and emit one StageLander
// candidate per virgin claim PR (open, head matching the claim-branch
// pattern, no live review, no verdict marker, no active run on the branch).
// Per-repo and per-pull errors log and skip — mirroring the new-work
// producer, one bad repo never aborts the gather. repos, liveCount, and
// loggedIn are the pass's shared listing, liveness snapshot, and forced-auth
// memo.
func (s *Service) landerCandidates(ctx context.Context, repos []store.Repo, liveCount int, loggedIn map[string]bool) []spawnCandidate {
	// Cheap pre-filter, mirroring the new-work producer: only autoland-on,
	// forge-bound, ready repos can spawn, so an autoland-less fleet costs
	// nothing beyond the pass's one repo listing.
	var enabled []store.Repo
	for _, r := range repos {
		if r.AutolandEnabled && r.TrackerBinding == store.TrackerBindingForge && r.CloneStatus == store.CloneStatusReady {
			enabled = append(enabled, r)
		}
	}
	if len(enabled) == 0 {
		return nil
	}

	var out []spawnCandidate
	for _, candidate := range enabled {
		// Re-read rather than trust the pre-filter snapshot: honours an
		// autoland toggle-off (or settings change) landing mid-pass.
		repo, err := s.store.RepoByID(ctx, candidate.ID)
		if err != nil {
			s.log.Warn("spawn: repo", "component", "afk", "repo", candidate.Name, "err", err)
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
			PullOpen:        true,
			ClaimBranch:     true,
		}
		if !ShouldSpawnLander(base) {
			continue
		}
		// At-cap production veto against the pass snapshot, BEFORE the forge
		// reads — a load-shedding hint, never enforcement (the pass owns the
		// cap, #185). It sits exactly where the old sweep's UnderCap veto
		// sat, so an at-cap repo still costs zero forge reads.
		if liveCount >= s.instances.EffectiveCap(ctx, repo) {
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
			s.log.Warn("spawn: resolving lander provider", "component", "afk", "repo", repo.Name, "err", err)
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
			s.log.Warn("spawn: tracker", "component", "afk", "repo", repo.Name, "err", err)
			continue
		}
		pulls, err := trk.Pulls(ctx)
		if err != nil {
			s.log.Warn("spawn: list pulls", "component", "afk", "repo", repo.Name, "err", err)
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
			// failed read skips THIS pull this pass (retried next pass):
			// fail closed, never spawn on unknown verdict state.
			reviews, err := trk.Reviews(ctx, pull.Number)
			if err != nil {
				s.log.Warn("spawn: reviews", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
				continue
			}
			d.ReviewPresent = LiveReview(reviews)
			comments, err := trk.PullComments(ctx, pull.Number)
			if err != nil {
				s.log.Warn("spawn: pull comments", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
				continue
			}
			d.VerdictPresent = len(VerdictWords(comments)) > 0
			// The runs-store gate: ANY active run on the branch — the
			// authoring AFK run still idling at its composer, or a lander
			// already working it — suppresses the candidate.
			onBranch, err := s.store.ActiveRunOnBranch(ctx, repo.ID, pull.HeadBranch)
			if err != nil {
				s.log.Warn("spawn: active run on branch", "component", "afk", "repo", repo.Name, "branch", pull.HeadBranch, "err", err)
				continue
			}
			d.RunOnBranch = onBranch
			if !ShouldSpawnLander(d) {
				continue
			}
			out = append(out, spawnCandidate{
				stage: StageLander,
				repo:  repo,
				label: fmt.Sprintf("lander %s#%d", repo.Name, pull.Number),
				launch: func(ctx context.Context) spawnOutcome {
					err := s.LaunchLander(ctx, repo.ID, pull.Number, pull.HeadBranch, issueN)
					switch {
					case err == nil:
						return spawnSpawned
					case errors.Is(err, instance.ErrOverCap):
						// LaunchLander proved fresh liveness reached the cap;
						// a lander launch claims nothing, so this was a pure
						// no-op the pass turns into its per-repo floor-raise
						// (spawnPass owns that reasoning).
						return spawnAtCap
					default:
						s.log.Warn("spawn: launch lander", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
						return spawnSkipped
					}
				},
			})
		}
	}
	return out
}
