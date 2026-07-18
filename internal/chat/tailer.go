package chat

// The tailer keeps one goroutine per active run, polling its transcript file
// (and live-signal spool) for changes and publishing a debounced
// run.messages.changed while tracking the run's adapter-composed
// conversational state for the instance list. The set of tailers is
// kept in sync with store.ActiveRuns: the service subscribes to the event bus
// and re-syncs on run.changed, so a launch arms a tailer and a terminal
// outcome disarms it — no wiring into instance/afk/reconcile is needed.

import (
	"context"
	"os"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// eventRunChanged is the run.changed wire name (defined per-package by design,
// brief §8.1) — the trigger the tailer re-syncs its set on.
const eventRunChanged = "run.changed"

// Run drives the tailer set until ctx (or the service ctx) is done. It syncs
// once immediately, then on every run.changed — the same envelope a launch or
// stop already publishes — and on a periodic resync tick: the bus drops events
// for a slow subscriber, and a dropped run.changed must cost one resync
// interval, never a permanently desynced tailer set. Blocking; cmd/lab runs it
// in a goroutine.
func (s *Service) Run(ctx context.Context) {
	sub, cancel := s.bus.Subscribe(ctx)
	defer cancel()
	resync := time.NewTicker(resyncFactor * s.poll)
	defer resync.Stop()
	s.sync(ctx)
	for {
		select {
		case <-ctx.Done():
			s.tailers.stopAll()
			return
		case <-s.ctx.Done():
			s.tailers.stopAll()
			return
		case <-resync.C:
			s.sync(ctx)
		case e, ok := <-sub:
			if !ok {
				s.tailers.stopAll()
				return
			}
			if e.Type == eventRunChanged {
				s.sync(ctx)
			}
		}
	}
}

// resyncFactor × poll is the periodic-resync cadence (30 s at the default
// 1 s poll) — the bound on how long a dropped run.changed can leave the
// tailer set stale.
const resyncFactor = 30

// sync arms a tailer for every active run not yet tailed and disarms tailers
// whose run is no longer active, then GCs the dialog spools of runs that are no
// longer active. Only ever called from Run's select — never on shutdown — so a
// clean shutdown leaves an active run's spool intact for restart survival
// (ADR-0020 decision 2).
func (s *Service) sync(ctx context.Context) {
	runs, err := s.store.ActiveRuns(ctx)
	if err != nil {
		s.log.Warn("chat tailer sync: listing active runs", "component", "chat", "err", err)
		return
	}
	active := make(map[string]store.Run, len(runs))
	activeIDs := make(map[string]bool, len(runs))
	for _, r := range runs {
		active[r.SessionName] = r
		activeIDs[r.ID] = true
	}
	s.tailers.retain(active, func(run store.Run) { s.arm(run) })
	s.sweepSpools(activeIDs)
}

// sweepSpools removes the dialog spool, blocked marker, and per-run settings
// file of every run no longer active (ADR-0020: a spool for an ended run is
// garbage; one for an active run survives a lab restart, so the active set is
// the keep-set — the same principle as the credential runtime GC). Runs against
// every provider advertising the LiveSignals capability.
func (s *Service) sweepSpools(activeIDs map[string]bool) {
	if s.runtimeDir == "" {
		return
	}
	keep := func(runID string) bool { return activeIDs[runID] }
	for _, prov := range s.providers.List() {
		h, ok := prov.(provider.LiveSignals)
		if !ok {
			continue
		}
		if err := h.SweepSpools(s.runtimeDir, keep); err != nil {
			s.log.Warn("chat tailer sync: sweeping dialog spools", "component", "chat", "provider", prov.ID(), "err", err)
		}
	}
}

// arm starts a tailer goroutine for run under a child of the service ctx.
func (s *Service) arm(run store.Run) {
	ctx, cancel := context.WithCancel(s.ctx)
	h := &tailerHandle{cancel: cancel}
	s.tailers.add(run.SessionName, h)
	go s.tail(ctx, run, h)
}

// tail is one run's poll loop. Each tick it re-resolves the live transcript path
// (following a /clear or /rewind rotation — locateActive, issue #34), then
// re-stats the transcript AND the dialog spool (a pending dialog appears while
// the transcript is byte-frozen — compat §5 — so watching only the file would
// never notice it). On any change it re-reads through ReadChat — the adapter
// composes transcript + live signals itself (issue #92) — and publishes
// run.messages.changed when the transcript changed OR the composed state
// changed; the chat view refetches on both. On a rotation it resets the
// file-change bookkeeping and forces a re-read + republish so the fresh
// (cleared) transcript is served and the view resets its stream.
func (s *Service) tail(ctx context.Context, run store.Run, h *tailerHandle) {
	// Remove by handle identity, not bare session name: a disarmed goroutine
	// can outlive its cancel by up to one tick, and session names are reused
	// (stop→start in the same minute) — an unconditional delete here could
	// tear down the successor tailer's registration and leave it unmanaged.
	defer s.tailers.remove(run.SessionName, h)
	prov, ok := s.providers.Get(run.Provider)
	if !ok {
		return
	}
	signals, _ := prov.(provider.LiveSignals) // nil for a transcript-only provider
	// The notify gate exists only when cmd/lab injected the push seam (issue
	// #99). It is owned by this one goroutine (not concurrency-safe by design)
	// and dies with it: a run ending or the dead-session sweep cancels ctx, so an
	// armed-but-not-due gate simply never fires — that IS the desired "run
	// ending fires nothing" semantics, no explicit teardown needed.
	var gate *notifyGate
	if s.notify != nil {
		gate = newNotifyGate(s.now, s.notifyDebounce, run)
	}
	t := time.NewTicker(s.poll)
	defer t.Stop()

	var (
		path      string
		lastMod   time.Time
		lastSz    int64
		lastSig   string
		lastState string
		first     = true
		// baseline maps seq → content hash of the previous SUCCESSFUL read —
		// the backpatch detector (issue #175). Owned by this one goroutine and
		// dying with it, like the notify gate: a re-armed tailer starts with no
		// baseline and its first tick simply reports no backpatch. Replaced
		// after every successful read — even when the publish gate doesn't fire
		// (a read without a publish only happens on a sig-only flip, whose
		// content is unchanged, so nothing is lost) — and reset to nil on a
		// rotation BEFORE comparing, so a fresh transcript's restarted seqs
		// never read as back-patches of the old file's.
		baseline map[int64]string
	)
	// Seed from the last-known persisted path (a re-adopted run already has one)
	// so the first tick's locateActive is a no-op when nothing rotated, rather
	// than a redundant re-persist of the same path.
	if run.TranscriptPath != nil {
		path = *run.TranscriptPath
	}
	for {
		// Re-resolve every tick against the tailer's OWN cached path (not the
		// stale in-memory run): a different non-empty result is a rotation to
		// follow (compat §5 / issue #34).
		prev := path
		path = s.locateActive(ctx, prov, run, path)
		rotated := prev != "" && path != prev
		if rotated {
			// A fresh file: drop the old file's change bookkeeping so this tick
			// re-reads the new (cleared) transcript from scratch and republishes,
			// even if the two files' stat coincides.
			lastMod, lastSz = time.Time{}, 0
		}
		// Stat into staged values; they are committed only after a successful
		// read, so a failed read tick re-detects the same change and retries.
		transcriptChanged := rotated
		mod, sz := lastMod, lastSz
		if path != "" {
			if fi, err := os.Stat(path); err == nil && (fi.ModTime() != lastMod || fi.Size() != lastSz) {
				transcriptChanged = true
				mod, sz = fi.ModTime(), fi.Size()
			}
		}
		var sig string
		if signals != nil && s.runtimeDir != "" {
			sig = signals.SpoolSig(run.ID, s.runtimeDir)
		}
		if first || transcriptChanged || sig != lastSig {
			// The tailer only ever tails ACTIVE runs, so it always passes the
			// runtime dir — composition (spool dialog / blocked-state precedence)
			// lives inside the adapter since issue #92. That move retired the
			// core-side parse cache, so a spool-only flip re-reads the transcript
			// too — a deliberate trade: spool flips are rare (a dialog opening or
			// resolving), and the common no-change tick still costs only stats.
			// A failed read updates NO bookkeeping (first/lastSig/lastState and
			// the staged stat all stand), so the next tick re-detects the same
			// change and retries the read instead of freezing on a half-observed
			// change (e.g. the file vanishing mid-rotation).
			chat, err := prov.ReadChat(run.ID, s.runtimeDir, path)
			if err == nil {
				// Scan + mask FIRST — before setState and, critically, before
				// gate.observe below: the needs-input push body is built from the
				// dialog prompt or the newest assistant text (notifyBody), so a
				// plaintext secret value in either must be masked before the gate
				// snapshots its payload — issue #108's "no event payload carries
				// the value" criterion. Scanning rides the read-happened branch
				// only, so quiet no-change ticks stay stat-only (no per-tick cost).
				s.scanAndRedact(ctx, run, &chat)
				// Hash AFTER redaction (issue #175) — the same order Service.Read
				// applies, so the two paths agree on every hash — then diff this
				// read's hashes against the previous read's to spot a back-patch:
				// an earlier message re-rendering under growth (a tool_result
				// completing a prior tool_use). A rotation drops the baseline
				// first — the fresh file restarts seq at 1, so cross-file
				// comparison is meaningless and the client resets its stream via
				// transcript_id anyway.
				provider.HashMessages(chat.Messages)
				if rotated {
					baseline = nil
				}
				backpatch := lowestBackpatchSeq(baseline, chat.Messages)
				baseline = hashesBySeq(chat.Messages)
				s.tailers.setState(run.SessionName, chat.State)
				// Feed the state edge to the notify gate with the PREVIOUS state
				// (lastState, before the reassignment below) so it edge-triggers on
				// the transition INTO needs_input/question (issue #99).
				if gate != nil {
					gate.observe(lastState, chat)
				}
				if first || transcriptChanged || chat.State != lastState {
					s.publishMessagesChanged(run, chat.State, backpatch)
				}
				first, lastSig, lastState = false, sig, chat.State
				lastMod, lastSz = mod, sz
			}
		}
		// The due-check ends EVERY tick — quiet no-change ticks and failed-read
		// ticks alike (the debounce deadline elapses while nothing else about the
		// run changes), and always AFTER this tick's observe: a revert read on
		// the deadline tick disarms the gate before it can fire, and a fired
		// payload reflects the freshest read (issue #99). A revert the transcript
		// records inside the window but the poll only reads after the deadline
		// still fires — the debounce is best-effort at poll granularity, and a
		// rare marginal send is harmless under tag coalescing.
		if gate != nil {
			if n, ok := gate.due(); ok {
				s.notify(n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// publishMessagesChanged emits the run.messages.changed envelope with the
// tick's composed state and backpatch seq (issue #175 — see
// messagesChangedPayload for the field semantics; backpatch 0 = append-only).
func (s *Service) publishMessagesChanged(run store.Run, state string, backpatch int64) {
	s.bus.Publish(events.Event{
		Type: EventMessagesChanged,
		Payload: messagesChangedPayload{Type: EventMessagesChanged, RepoID: run.RepoID, RunID: run.ID,
			State: state, BackpatchSeq: backpatch},
	})
}

// hashesBySeq snapshots one successful read's per-seq content hashes — the
// tailer's next backpatch baseline (issue #175).
func hashesBySeq(msgs []provider.Message) map[int64]string {
	m := make(map[int64]string, len(msgs))
	for _, msg := range msgs {
		m[msg.Seq] = msg.ContentHash
	}
	return m
}

// lowestBackpatchSeq returns the lowest seq present in BOTH the baseline and
// msgs whose content hash changed — 0 when nothing already-seen re-rendered.
// A seq absent from the baseline is an append, never a backpatch (issue #175).
func lowestBackpatchSeq(baseline map[int64]string, msgs []provider.Message) int64 {
	var lowest int64
	for _, msg := range msgs {
		prev, ok := baseline[msg.Seq]
		if !ok || prev == msg.ContentHash {
			continue
		}
		if lowest == 0 || msg.Seq < lowest {
			lowest = msg.Seq
		}
	}
	return lowest
}

// tailerHandle identifies one tailer goroutine's registration. remove
// compares handle identity so a late-exiting predecessor can never delete a
// successor that reused its session name.
type tailerHandle struct {
	cancel context.CancelFunc
}

// tailerSet is the concurrency-safe registry of live tailers and their last
// derived states.
type tailerSet struct {
	mu      sync.Mutex
	handles map[string]*tailerHandle
	states  map[string]string
}

func newTailerSet() *tailerSet {
	return &tailerSet{handles: map[string]*tailerHandle{}, states: map[string]string{}}
}

// retain disarms tailers whose session is not in active, and calls arm for
// every active session not already tailed. arm must call add before starting.
func (ts *tailerSet) retain(active map[string]store.Run, arm func(store.Run)) {
	ts.mu.Lock()
	var toStop []context.CancelFunc
	for session, h := range ts.handles {
		if _, ok := active[session]; !ok {
			toStop = append(toStop, h.cancel)
			delete(ts.handles, session)
			delete(ts.states, session)
		}
	}
	var toArm []store.Run
	for session, run := range active {
		if _, ok := ts.handles[session]; !ok {
			toArm = append(toArm, run)
		}
	}
	ts.mu.Unlock()

	for _, cancel := range toStop {
		cancel()
	}
	for _, run := range toArm {
		arm(run)
	}
}

func (ts *tailerSet) add(session string, h *tailerHandle) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.handles[session] = h
}

// remove clears a tailer's cancel/state on goroutine exit — but only when the
// session still maps to this goroutine's own handle. It does not call cancel
// (the goroutine is already returning).
func (ts *tailerSet) remove(session string, h *tailerHandle) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.handles[session] != h {
		return
	}
	delete(ts.handles, session)
	delete(ts.states, session)
}

func (ts *tailerSet) setState(session, state string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.states[session] = state
}

func (ts *tailerSet) state(session string) (string, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	st, ok := ts.states[session]
	return st, ok
}

func (ts *tailerSet) stopAll() {
	ts.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(ts.handles))
	for session, h := range ts.handles {
		cancels = append(cancels, h.cancel)
		delete(ts.handles, session)
		delete(ts.states, session)
	}
	ts.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
