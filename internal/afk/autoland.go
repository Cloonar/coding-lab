package afk

// The autoland gather (issues #181/#182 / ADR-0048): the state-derived walk
// that produces EVERY autoland candidate — lander, fix, escalate — for the
// fleet spawn pass (SpawnOnce, issue #185 — the walk itself launches nothing).
// Nothing is message-passed (ADR-0015 poll-only stands): every candidate is a
// pure function of polled forge state — the pull listing, its native reviews,
// its verdict-marker comments — plus the runs store, so a restart loses
// nothing and a re-pass re-derives the same decision. The per-PR spawn rule
// is DecideAutoland (decide.go): a virgin claim PR gets its round-1 lander
// (#181, unchanged), a fix-done PR its re-validation lander, a rejected PR a
// fix run under the attempt bound and the terminal escalate run at it, and an
// escalated PR nothing, forever.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// autolandCandidates is the fix-forward producer (issue #185 made the old
// sweep gather-only; issue #182 generalized it from landers-only to the whole
// autoland decision — it still never launches and never decides the cap): for
// every autoland-enabled, forge-bound, ready, un-paused repo, walk the open
// claim PRs, fold each one's verdict state, and emit the ONE candidate
// DecideAutoland yields — StageLander for validation rounds, StageFix for fix
// runs and the escalate runs that conclude the fix pipeline. Per-repo and
// per-pull errors log and skip — mirroring the new-work producer, one bad
// repo never aborts the gather. repos, liveCount, and loggedIn are the pass's
// shared listing, liveness snapshot, and forced-auth memo.
func (s *Service) autolandCandidates(ctx context.Context, repos []store.Repo, liveCount int, loggedIn map[string]bool) []spawnCandidate {
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
		// The repo half of the spawn decision, inline (DecideAutoland owns the
		// pull half): opted in (default off — never by upgrade), forge-bound
		// (builtin has no PR comments to poll, ADR-0048), clone ready, and not
		// three-strikes paused (the pause stops every autoland spawn too).
		// Judged before any forge read — the pull facts cannot rescue a vetoed
		// repo.
		if !repo.AutolandEnabled || repo.TrackerBinding != store.TrackerBindingForge ||
			repo.CloneStatus != store.CloneStatusReady || repo.ConsecutiveFailures >= PauseThreshold {
			continue
		}
		// At-cap production veto against the pass snapshot, BEFORE the forge
		// reads — a load-shedding hint, never enforcement (the pass owns the
		// cap, #185). It sits exactly where the old sweep's UnderCap veto
		// sat, so an at-cap repo still costs zero forge reads.
		if liveCount >= s.instances.EffectiveCap(ctx, repo) {
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
			// Only an OPEN claim PR is autoland's: a merged or closed PR needs
			// nothing, and human-branch PRs are never touched. Skipped before
			// the per-pull forge reads.
			if pull.State != tracker.PullOpen {
				continue
			}
			issueN, matched := gitx.MatchBranch(repo.AFKBranchPattern, pull.HeadBranch)
			if !matched {
				continue
			}
			// The two per-pull forge reads, bounded to open claim PRs. A
			// failed read skips THIS pull this pass (retried next pass):
			// fail closed, never spawn on unknown verdict state.
			reviews, err := trk.Reviews(ctx, pull.Number)
			if err != nil {
				s.log.Warn("spawn: reviews", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
				continue
			}
			comments, err := trk.PullComments(ctx, pull.Number)
			if err != nil {
				s.log.Warn("spawn: pull comments", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
				continue
			}
			words := VerdictWords(comments)
			// The runs-store facts. ANY active run on the branch — the
			// authoring AFK run still idling at its composer, or an autoland
			// run already working it — suppresses every candidate (the gate
			// that serializes marker writers per branch).
			onBranch, err := s.store.ActiveRunOnBranch(ctx, repo.ID, pull.HeadBranch)
			if err != nil {
				s.log.Warn("spawn: active run on branch", "component", "afk", "repo", repo.Name, "branch", pull.HeadBranch, "err", err)
				continue
			}
			// Escalated is the OR of both sources (#182): the escalate word at
			// ANY comment position, or an escalated-outcome run row — a human
			// can delete the marker comment but never the row, so the row is
			// the durable half; the marker short-circuits the store read.
			escalated := slices.Contains(words, verdictWordEscalate)
			if !escalated {
				escalated, err = s.store.EscalatedRunOnBranch(ctx, repo.ID, pull.HeadBranch)
				if err != nil {
					s.log.Warn("spawn: escalated run on branch", "component", "afk", "repo", repo.Name, "branch", pull.HeadBranch, "err", err)
					continue
				}
			}
			st := PullVerdictState{
				Words:          words,
				LiveReview:     LiveReview(reviews),
				HumanRejected:  HumanRejected(reviews),
				Escalated:      escalated,
				RunOnBranch:    onBranch,
				MaxFixAttempts: repo.MaxFixAttempts,
			}
			action, approveOnly := DecideAutoland(st)
			// FixSpawns is fetched lazily: it matters only inside
			// rejected-state (the fix↔escalate split), and zero is its most
			// permissive value — the ONLY action a real count can change is
			// ActionFix (to escalate at the bound), so the count query is
			// spent exactly when the zero-count decision says fix, never on
			// the pass's virgin/validated/escalated majority.
			if action == ActionFix {
				n, err := s.store.FixRunCountForBranch(ctx, repo.ID, pull.HeadBranch)
				if err != nil {
					s.log.Warn("spawn: fix run count", "component", "afk", "repo", repo.Name, "branch", pull.HeadBranch, "err", err)
					continue
				}
				st.FixSpawns = n
				action, approveOnly = DecideAutoland(st)
			}
			if action == ActionNone {
				continue
			}
			// The action's effective provider gates the forced-auth pre-check
			// (memoized per provider per pass): a logged-out launch strands a
			// session that dies at the login wall and reaps as a death. The
			// locked launch path re-makes the same resolution authoritatively.
			prov, err := s.autolandProvider(ctx, repo, pull.HeadBranch, action)
			if err != nil {
				s.log.Warn("spawn: resolving autoland provider", "component", "afk", "repo", repo.Name, "pull", pull.Number, "err", err)
				continue
			}
			if _, checked := loggedIn[prov.ID()]; !checked {
				st, _ := prov.AuthStatus(ctx, true)
				loggedIn[prov.ID()] = st.LoggedIn
			}
			if !loggedIn[prov.ID()] {
				continue
			}
			switch action {
			case ActionLander:
				out = append(out, spawnCandidate{
					stage: StageLander,
					repo:  repo,
					label: fmt.Sprintf("lander %s#%d", repo.Name, pull.Number),
					launch: s.autolandLaunch(repo, pull.Number, "lander", func(ctx context.Context) error {
						return s.LaunchLander(ctx, repo.ID, pull.Number, pull.HeadBranch, issueN, approveOnly)
					}),
				})
			case ActionFix:
				// The work order is captured at gather time, from the same
				// comment/review reads the decision folded — the launch never
				// re-reads the forge.
				rejection := RejectionContext(comments, reviews)
				out = append(out, spawnCandidate{
					stage: StageFix,
					repo:  repo,
					label: fmt.Sprintf("fix %s#%d", repo.Name, pull.Number),
					launch: s.autolandLaunch(repo, pull.Number, "fix", func(ctx context.Context) error {
						return s.LaunchFix(ctx, repo.ID, pull.Number, pull.HeadBranch, issueN, rejection)
					}),
				})
			case ActionEscalate:
				// StageFix rank, deliberately: escalation concludes the fix
				// pipeline and must not queue behind new work.
				history := EscalationHistory(comments, reviews)
				out = append(out, spawnCandidate{
					stage: StageFix,
					repo:  repo,
					label: fmt.Sprintf("escalate %s#%d", repo.Name, pull.Number),
					launch: s.autolandLaunch(repo, pull.Number, "escalate", func(ctx context.Context) error {
						return s.LaunchEscalate(ctx, repo.ID, pull.Number, pull.HeadBranch, issueN, history)
					}),
				})
			}
		}
	}
	return out
}

// autolandProvider resolves the provider gating an autoland action's
// candidate: lander and escalate runs on the lander chain (lander_provider
// else base — validation-class, non-AFK), fix runs on the authoring chain
// (the persisted authoring run's provider, else the repo chain — fixProvider).
// Each arm is the SAME resolution its launch path re-makes under the engine
// lock, so the gate and the launch can never disagree on which login matters.
func (s *Service) autolandProvider(ctx context.Context, repo store.Repo, branch string, action AutolandAction) (provider.AgentProvider, error) {
	switch action {
	case ActionFix:
		prov, _, _, err := s.fixProvider(ctx, repo, branch)
		return prov, err
	case ActionEscalate:
		return s.landerChainProvider(ctx, repo, store.RunKindEscalate)
	default: // ActionLander
		return s.landerChainProvider(ctx, repo, store.RunKindLander)
	}
}

// autolandLaunch wraps a locked autoland launch path into the pass's
// normalized outcome. ErrOverCap is the pure at-cap no-op: no autoland launch
// claims anything, so the pass turns it into its per-repo floor-raise
// (spawnPass owns that reasoning). Any other error is logged here and the
// candidate skipped — no slot spent.
func (s *Service) autolandLaunch(repo store.Repo, pullNumber int, verb string, launch func(context.Context) error) func(context.Context) spawnOutcome {
	return func(ctx context.Context) spawnOutcome {
		err := launch(ctx)
		switch {
		case err == nil:
			return spawnSpawned
		case errors.Is(err, instance.ErrOverCap):
			return spawnAtCap
		default:
			s.log.Warn("spawn: launch "+verb, "component", "afk", "repo", repo.Name, "pull", pullNumber, "err", err)
			return spawnSkipped
		}
	}
}
