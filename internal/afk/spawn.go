package afk

// The fleet's ONE spawn pass (issue #185). Before it, two independent loops
// raced for the same bounded resource: the scheduler tick launched new AFK
// work and the reaper tick's autoland sweep launched landers, each judging
// `liveCount < EffectiveCap` against its own snapshot — two consumers of one
// cap is a race by construction, not an edge case, and whether a claim PR got
// validated before fresh work started was down to tick alignment. Now both
// loops (and the HTTP toggle/reset kicks) invoke SpawnOnce: producers GATHER
// candidates and never launch, one pass sorts them by pipeline stage and
// spends the cap down the list. The rule: drain the pipeline before filling
// it — lander > fix (#182) > new AFK work.

import (
	"context"
	"sort"

	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// spawnOutcome is a candidate launch's normalized result — the only three
// facts the pass acts on, whichever locked launch path produced them.
type spawnOutcome int

const (
	spawnSpawned spawnOutcome = iota // a run spawned — the pass counts the consumed slot
	spawnAtCap                       // the locked launch proved fresh liveness reached this repo's cap
	spawnSkipped                     // nothing spawned, no slot spent — raced, queue emptied, or an already-logged error
)

// spawnCandidate is one launchable unit of work a producer gathered. Stage is
// the ONLY priority — data, not control flow (issue #185) — so a new producer
// (#182's fix runs) joins the order by emitting its rank, never by editing
// the pass. The repo row snapshot serves the pass's cheap cap veto and its
// logging; the launch closure owns the actual spawn through the engine's
// locked paths (launch / LaunchLander), whose authoritative per-launch cap
// guards on fresh liveness remain the backstop on this same path.
type spawnCandidate struct {
	stage  SpawnStage
	repo   store.Repo
	label  string // short log tag, e.g. "lander proj#3", "afk-auto proj"
	launch func(ctx context.Context) spawnOutcome
}

// SpawnOnce is the fleet's single spawn decision point (issue #185): one
// repo listing, one liveness snapshot, and one shared forced-auth memo feed
// every producer; the gathered candidates then go through spawnPass, which
// is the sole consumer of the live-instance cap. Invoked from both long-
// lived loops — the reaper tick right after the reap (drain-then-fill made
// literal) and the scheduler tick as an additional fill heartbeat — plus the
// HTTP toggle-on/reset kicks.
func (s *Service) SpawnOnce(ctx context.Context) {
	// Serialized under its own lock so there is ONE cap consumer by
	// construction: issue #185 — two loops racing one bounded resource was a
	// race by construction, and merging the decision only helps if concurrent
	// scheduler/reaper ticks (and kicks) queue here instead of interleaving
	// their snapshots.
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()

	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("spawn: list repos", "component", "afk", "err", err)
		return
	}
	live, err := s.runner.List(ctx)
	if err != nil {
		s.log.Warn("spawn: list sessions", "component", "afk", "err", err)
		return
	}
	liveCount := instance.LiveInstanceCount(live)

	// The forced-auth gate memo, once per distinct provider per PASS, shared
	// across producers (each loop used to refresh separately — the pass
	// dedupes that): a logged-out launch strands a session that dies at the
	// login wall and reaps as a death. Producers fill it lazily through their
	// own kind-specific provider resolution.
	loggedIn := map[string]bool{}

	candidates := s.landerCandidates(ctx, repos, liveCount, loggedIn)
	// StageFix (issue #182) plugs in HERE: the fix-forward producer appends
	// its candidates alongside the two gathers. Its rank is already reserved
	// (StageFix, between lander and new work) and the sort below owns the
	// order, so the insertion point is cosmetic — the producer is the whole
	// addition.
	candidates = append(candidates, s.newWorkCandidates(ctx, repos, liveCount, loggedIn)...)

	s.spawnPass(ctx, liveCount, candidates)
}

// spawnPass is the pass core, deliberately separated from the gathering so
// tests drive it over synthetic candidates: sort by stage, then launch down
// the list while the snapshot says there is headroom. liveCount is the
// caller's liveness snapshot — a hint the pass keeps current as it spends
// slots; every launch closure re-proves the cap under the engine lock.
func (s *Service) spawnPass(ctx context.Context, liveCount int, candidates []spawnCandidate) {
	// Stable, and by stage ONLY: within a stage the production order is
	// preserved — repos in listing order, pulls in listing order — so the
	// sort adds priority without reshuffling anything the producers decided.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].stage < candidates[j].stage
	})
	for _, c := range candidates {
		// Cheap snapshot veto: at/over this repo's effective cap, skip
		// without spending a locked launch call. Best-effort by design — the
		// closure's locked path re-checks fresh liveness authoritatively.
		if liveCount >= s.instances.EffectiveCap(ctx, c.repo) {
			continue
		}
		switch c.launch(ctx) {
		case spawnSpawned:
			liveCount++ // this run consumed a slot; the next candidate sees it
		case spawnAtCap:
			// The locked launch proved the fresh GLOBAL live count reached
			// THIS repo's effective cap. Under per-repo cap overrides (D12c)
			// that no longer implies every repo is at cap, so skip only this
			// candidate — a later one whose repo cap still has headroom must
			// still launch. Raise the stale snapshot to this repo's cap
			// first: a sound lower bound (the launch proved the fresh count
			// is at least that) that lets later vetoes fire without another
			// locked launch call, and reproduces a whole-pass stop whenever
			// every repo shares the one global cap. This deliberately
			// replaces the old autoland sweep's stop-everything on
			// ErrOverCap, which was correct only under a single global cap
			// (issue #185).
			s.log.Warn("spawn: at the live-instance cap", "component", "afk", "candidate", c.label)
			if repoCap := s.instances.EffectiveCap(ctx, c.repo); repoCap > liveCount {
				liveCount = repoCap
			}
		case spawnSkipped:
			// The queue emptied or another launch raced us between gather and
			// launch, or the closure already logged its error; no slot spent.
		}
	}
}
