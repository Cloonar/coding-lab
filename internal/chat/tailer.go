package chat

// The tailer keeps one goroutine per active run, polling its transcript file
// for changes and publishing a debounced run.messages.changed while deriving
// the run's conversational state for the instance list. The set of tailers is
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
// every provider advertising the DialogHooker capability.
func (s *Service) sweepSpools(activeIDs map[string]bool) {
	if s.runtimeDir == "" {
		return
	}
	keep := func(runID string) bool { return activeIDs[runID] }
	for _, prov := range s.providers.List() {
		h, ok := prov.(provider.DialogHooker)
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

// tail is one run's poll loop. Each tick it re-stats the transcript AND the
// dialog spool (a pending dialog appears while the transcript is byte-frozen —
// compat §5 — so watching only the file would never notice it). On any change
// it recomputes the combined state (transcript + spool + marker) and publishes
// run.messages.changed when the transcript changed OR the derived state changed
// — the chat view refetches on both. The transcript is re-parsed only when its
// file changed (cached across ticks); a bare spool change reuses the last parse.
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
	hooker, _ := prov.(provider.DialogHooker) // nil for a transcript-only provider
	t := time.NewTicker(s.poll)
	defer t.Stop()

	var (
		path      string
		lastMod   time.Time
		lastSz    int64
		lastSig   string
		lastState string
		lastChat  = provider.Chat{State: provider.StateIdle}
		first     = true
	)
	for {
		if path == "" {
			path = s.resolvePath(ctx, prov, run)
		}
		transcriptChanged := false
		if path != "" {
			if fi, err := os.Stat(path); err == nil && (fi.ModTime() != lastMod || fi.Size() != lastSz) {
				transcriptChanged = true
				lastMod, lastSz = fi.ModTime(), fi.Size()
			}
		}
		var sig string
		if hooker != nil && s.runtimeDir != "" {
			sig = hooker.SpoolSig(run.ID, s.runtimeDir)
		}
		if first || transcriptChanged || sig != lastSig {
			if transcriptChanged {
				if chat, err := prov.ReadTranscript(path); err == nil {
					lastChat = chat
				}
			}
			// The tailer only ever tails ACTIVE runs, so overlay the spool
			// signals directly (no ended-state override to apply).
			view := View{Chat: lastChat}
			s.applyLiveSignals(&view, run, path)
			s.tailers.setState(run.SessionName, view.State)
			if first || transcriptChanged || view.State != lastState {
				s.publishMessagesChanged(run)
			}
			first, lastSig, lastState = false, sig, view.State
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) publishMessagesChanged(run store.Run) {
	s.bus.Publish(events.Event{
		Type:    EventMessagesChanged,
		Payload: messagesChangedPayload{Type: EventMessagesChanged, RepoID: run.RepoID, RunID: run.ID},
	})
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
