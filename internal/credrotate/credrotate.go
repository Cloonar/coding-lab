// Package credrotate is lab core's single credential refresher (issue #222).
//
// Per-run credential snapshots fork a provider's OAuth token family: a
// long-lived instance's CLI refreshes its private copy, rotating the shared
// refresh token and invalidating every OTHER holder — the master store and all
// sibling snapshots. The host then reads logged-out, spawns refuse, and a wipe
// can destroy the only valid family.
//
// The invariant this package enforces is ONE refresher per provider grant: this
// loop, running against the machine's MASTER store, is the only refresher;
// every instance is a CONSUMER that receives the rotated family through
// provider.InjectCredentials. Core owns the schedule, the change comparison,
// the fan-out ordering, and the adopt tie-breaks; each adapter owns its file
// paths and CLI recipes (the credential-authority seam in
// internal/provider/provider.go). Core compares provider credential
// signatures for EQUALITY only — it never parses them.
//
// Two mechanisms keep the fleet coherent, both landing well inside the "minutes"
// window in which a rotation invalidates the old family's access tokens:
//
//   - Blind poke: every pokeEvery the loop drives the provider's own CLI
//     non-interactively against the master store; the CLI itself decides whether
//     the token is near expiry and rotates. The loop detects a rotation purely
//     via the master signature changing, then fans the new family out to every
//     live instance.
//   - Adopt-back: if an instance self-refreshed anyway (its home's signature
//     diverged from the last-injected baseline), the loop adopts that still-valid
//     rotated family into the master store — newest self-refresh wins — and fans
//     it back out, so a later wipe can never destroy the only live copy.
package credrotate

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

const (
	// scanTick is the signature-scan cadence. Each tick only stats credential
	// files (CredentialsSig) and, at most, re-copies them — cheap — so it runs
	// often enough to stay well inside the "minutes" window in which a rotation
	// invalidates the old family's access tokens: a self-refresh or an operator
	// re-login is detected and healed across the fleet within one short tick.
	scanTick = 30 * time.Second

	// pokeEvery bounds the blind CLI poke. Unlike the scan, a poke is a live
	// subprocess making a real provider API call, so it is deliberately sparse:
	// an OAuth access token lives ~8h, so a 15-minute cadence rotates it long
	// before expiry while keeping the call count low. The scan (which detects the
	// resulting rotation and fans it out) stays at the tighter scanTick.
	pokeEvery = 15 * time.Minute

	// refreshTimeout bounds one poke. RefreshCredentials shells out to the
	// provider's CLI, so the call is wrapped in a child context that cannot
	// outlive this bound — a hung CLI must never wedge the loop.
	refreshTimeout = 3 * time.Minute
)

// Options configures a Service. Providers, Store, and Homes are required.
type Options struct {
	// Providers is the provider registry; the loop refreshes every registered
	// provider's master store once per tick.
	Providers *provider.Registry
	// Store is lab's run store — the source of active runs and the persisted
	// per-run last-injected credential signature baseline (Run.CredSig).
	Store *store.Store
	// Homes derives a run's private instance HOME (issue #202) — the credential
	// copy the loop injects into and adopts back from.
	Homes *instancehome.Manager
	// Logger defaults to slog.Default(); every line carries component=credrotate.
	Logger *slog.Logger
	// Now overrides the clock (tests); nil → time.Now.
	Now func() time.Time
}

// Service is the single-refresher rotation loop. Construct with New.
type Service struct {
	providers *provider.Registry
	store     *store.Store
	homes     *instancehome.Manager
	log       *slog.Logger
	now       func() time.Time

	// mu guards the whole of tick and AdoptCheck (AdoptCheck arrives from
	// stop-path goroutines while the loop ticks) and the two per-provider maps.
	mu sync.Mutex
	// lastPoke is the last blind-poke time per provider id — the pokeEvery gate.
	lastPoke map[string]time.Time
	// lastMaster is the last-observed master signature per provider id. A missing
	// entry is the bootstrap (first tick / after restart): core cannot know
	// whether a rotation happened during downtime, so it fans out once — re-inject
	// is idempotent. "" is recorded too, so an operator re-login (a signature
	// appearing) is detected as a change on the next tick.
	lastMaster map[string]string
}

// New validates o and returns a Service.
func New(o Options) (*Service, error) {
	switch {
	case o.Providers == nil:
		return nil, errors.New("credrotate: Options.Providers is required")
	case o.Store == nil:
		return nil, errors.New("credrotate: Options.Store is required")
	case o.Homes == nil:
		return nil, errors.New("credrotate: Options.Homes is required")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		providers:  o.Providers,
		store:      o.Store,
		homes:      o.Homes,
		log:        logger,
		now:        now,
		lastPoke:   map[string]time.Time{},
		lastMaster: map[string]string{},
	}, nil
}

// Loop runs the rotation scan until ctx is cancelled. It runs one tick
// IMMEDIATELY on start — after lab downtime the first tick heals the whole
// fleet (bootstrap fan-out, plus any self-refresh that happened while lab was
// down) — then every scanTick.
func (s *Service) Loop(ctx context.Context) {
	ticker := time.NewTicker(scanTick)
	defer ticker.Stop()
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick runs one scan across every registered provider, under the mutex so a
// concurrent AdoptCheck cannot interleave with the per-provider bookkeeping.
func (s *Service) tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.providers.List() {
		s.tickProvider(ctx, p)
	}
}

// tickProvider runs the load-bearing per-provider sequence:
//
//  1. Adopt-scan — adopt any instance that self-refreshed (newest mtime wins),
//     and fan the adopted family out immediately.
//  2. Poke — the blind, sparse CLI refresh of the master store.
//  3. Master-change detection — did the master signature change (or is this the
//     bootstrap)?
//  4. Fan-out — the shared mechanism steps 1 and 3 trigger.
func (s *Service) tickProvider(ctx context.Context, p provider.AgentProvider) {
	pid := p.ID()
	live := s.liveRuns(ctx, pid)

	// 1. Adopt-scan FIRST: a mid-launch run with a fresh home benefits from
	// fan-out, and a stale-but-surviving home is adopt-checked before any sweep
	// can wipe it. A successful adopt is followed immediately by fan-out —
	// waiting invites a second instance to fork yet another family.
	if s.adoptScan(ctx, p, live) {
		s.fanOut(ctx, p, live)
	}

	// 2. Poke: sparse (pokeEvery) and gated on a non-empty master signature —
	// never poke a logged-out host. lastPoke is stamped BEFORE the call so a
	// failing CLI cannot hot-loop.
	if s.now().Sub(s.lastPoke[pid]) >= pokeEvery {
		s.lastPoke[pid] = s.now()
		if masterSig, _ := p.CredentialsSig(""); masterSig != "" {
			pokeCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
			if err := p.RefreshCredentials(pokeCtx); err != nil {
				s.log.Warn("credrotate: poke failed", "component", "credrotate", "provider", pid, "err", err)
			}
			cancel()
		}
	}

	// 3. Master-change detection → 4. Fan-out. Fan out iff the master signature
	// is non-empty AND either this is the bootstrap (no prior observation) or the
	// signature changed. lastMaster is always recorded (even "") so a later
	// login/logout is seen as a change next tick.
	masterSig, _ := p.CredentialsSig("")
	last, seen := s.lastMaster[pid]
	if masterSig != "" && (!seen || masterSig != last) {
		s.fanOut(ctx, p, live)
	}
	s.lastMaster[pid] = masterSig
}

// adoptScan enumerates live homes for signs of a self-refresh and, if any is
// found, adopts the newest one's family into the master store. It returns true
// iff the master store was mutated by an adopt (so the caller fans out).
//
// Three home states matter (the fourth — no baseline, no creds — is a no-op):
//   - unstamped baseline + present creds: a pre-upgrade row; stamp the baseline
//     (NOT a self-refresh — there is nothing to diverge from).
//   - present baseline + diverged creds: a self-refresh candidate.
//   - present baseline + vanished creds: warn and skip (do not adopt nothing).
func (s *Service) adoptScan(ctx context.Context, p provider.AgentProvider, live []store.Run) bool {
	pid := p.ID()
	type candidate struct {
		run   store.Run
		mtime time.Time
	}
	var cands []candidate
	for _, run := range live {
		home := s.homes.HomePath(run.ID)
		cur, mtime := p.CredentialsSig(home)
		switch {
		case run.CredSig == "" && cur != "":
			// Pre-upgrade / unstamped row: stamp the baseline so a later true
			// self-refresh has something to diverge from.
			if err := s.store.UpdateRunCredSig(ctx, run.ID, cur); err != nil {
				s.logStampErr(err, run.ID)
			}
		case run.CredSig != "" && cur != "" && cur != run.CredSig:
			cands = append(cands, candidate{run: run, mtime: mtime})
		case run.CredSig != "" && cur == "":
			s.log.Warn("credrotate: credentials vanished from live home",
				"component", "credrotate", "provider", pid, "run", run.ID)
		}
	}
	if len(cands) == 0 {
		return false
	}
	// The newest credential-file mtime wins: each rotation invalidates every
	// other family, so the most recent one is the live family. On adopt error,
	// try the next-newest — a vanished file must not strand a still-valid older
	// candidate.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].mtime.After(cands[j].mtime) })
	for _, c := range cands {
		home := s.homes.HomePath(c.run.ID)
		if err := p.AdoptCredentials(home); err != nil {
			s.log.Warn("credrotate: adopt failed, trying next-newest",
				"component", "credrotate", "provider", pid, "run", c.run.ID, "err", err)
			continue
		}
		s.log.Info("credrotate: adopted self-refreshed credentials",
			"component", "credrotate", "provider", pid, "run", c.run.ID)
		return true
	}
	return false
}

// fanOut re-injects the master credentials into each run's home, re-stamps each
// home's POST-inject signature as its new baseline (per-home signatures differ
// by mtime even for identical bytes, so the baseline must be the observed
// value), and records the master signature as lastMaster. The env return of
// InjectCredentials is deliberately discarded: the session already runs with
// its spawn env, so this call is only the file re-copy through the seam's
// designed injection point. One bad home never strands the fleet.
func (s *Service) fanOut(ctx context.Context, p provider.AgentProvider, runs []store.Run) {
	pid := p.ID()
	for _, run := range runs {
		home := s.homes.HomePath(run.ID)
		if _, err := p.InjectCredentials(home); err != nil {
			s.log.Warn("credrotate: fan-out inject failed",
				"component", "credrotate", "provider", pid, "run", run.ID, "err", err)
			continue
		}
		sig, _ := p.CredentialsSig(home)
		if err := s.store.UpdateRunCredSig(ctx, run.ID, sig); err != nil {
			s.logStampErr(err, run.ID)
		}
	}
	masterSig, _ := p.CredentialsSig("")
	s.lastMaster[pid] = masterSig
}

// AdoptCheck is the pre-wipe adopt-check a wipe path runs before destroying a
// run's home (issue #222 decision 4: a wipe must never destroy the only valid
// family). If the run's home holds a self-refreshed family (its current
// signature diverged from the last-injected baseline), it is adopted into the
// master store and fanned out to the provider's OTHER live homes before the
// caller proceeds with the wipe. Best-effort: every failure is logged and
// swallowed. Safe to call concurrently with the loop (shares the mutex).
func (s *Service) AdoptCheck(ctx context.Context, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, err := s.store.RunByID(ctx, runID)
	if err != nil {
		// A missing row is the common race (the run was already reaped) — debug,
		// not warn.
		s.log.Debug("credrotate: adopt-check run lookup", "component", "credrotate", "run", runID, "err", err)
		return
	}
	if run.CredSig == "" {
		return // no baseline: nothing to diverge from
	}
	p, ok := s.providers.Get(run.Provider)
	if !ok {
		return // provider no longer registered
	}
	home := s.homes.HomePath(runID)
	cur, _ := p.CredentialsSig(home)
	if cur == "" || cur == run.CredSig {
		return // creds gone, or unchanged since last injection: nothing to adopt
	}
	if err := p.AdoptCredentials(home); err != nil {
		s.log.Warn("credrotate: adopt-check adopt failed",
			"component", "credrotate", "provider", run.Provider, "run", runID, "err", err)
		return
	}
	s.log.Info("credrotate: adopted self-refreshed credentials before wipe",
		"component", "credrotate", "provider", run.Provider, "run", runID)
	// Fan out to the OTHER live homes (the checked run is about to be wiped) and
	// refresh lastMaster so the loop does not re-detect this as a fresh change.
	others := s.otherLiveRuns(ctx, run.Provider, runID)
	s.fanOut(ctx, p, others)
}

// liveRuns is the provider's fan-out/adopt enumeration: active runs whose home
// directory exists on disk. Liveness is deliberately NOT tmux liveness — a
// mid-launch run with a fresh home still benefits from fan-out, and a stale
// active row with a surviving home must be adopt-checked before a sweep can wipe
// it. A run whose home was never materialized (or is already gone) has nothing
// to inject into and is skipped.
func (s *Service) liveRuns(ctx context.Context, pid string) []store.Run {
	active, err := s.store.ActiveRuns(ctx)
	if err != nil {
		s.log.Warn("credrotate: list active runs", "component", "credrotate", "err", err)
		return nil
	}
	out := active[:0]
	for _, run := range active {
		if run.Provider != pid {
			continue
		}
		if _, err := os.Stat(s.homes.HomePath(run.ID)); err != nil {
			continue
		}
		out = append(out, run)
	}
	return out
}

// otherLiveRuns is liveRuns minus the run identified by exclude — the AdoptCheck
// fan-out target (the checked run is about to be wiped).
func (s *Service) otherLiveRuns(ctx context.Context, pid, exclude string) []store.Run {
	live := s.liveRuns(ctx, pid)
	out := live[:0]
	for _, run := range live {
		if run.ID == exclude {
			continue
		}
		out = append(out, run)
	}
	return out
}

// logStampErr routes an UpdateRunCredSig failure: a run that ended mid-scan
// (ErrNotFound) is an expected race and logged at debug; anything else warns.
func (s *Service) logStampErr(err error, runID string) {
	if errors.Is(err, store.ErrNotFound) {
		s.log.Debug("credrotate: run ended mid-scan", "component", "credrotate", "run", runID)
		return
	}
	s.log.Warn("credrotate: stamp cred sig", "component", "credrotate", "run", runID, "err", err)
}
