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
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// eventRunChanged is the run.changed wire name (defined per-package by design,
// brief §8.1) — the trigger the tailer re-syncs its set on.
const eventRunChanged = "run.changed"

// Run drives the tailer set until ctx (or the service ctx) is done. It syncs
// once immediately, then on every run.changed — the same envelope a launch or
// stop already publishes. Blocking; cmd/lab runs it in a goroutine.
func (s *Service) Run(ctx context.Context) {
	sub, cancel := s.bus.Subscribe(ctx)
	defer cancel()
	s.sync(ctx)
	for {
		select {
		case <-ctx.Done():
			s.tailers.stopAll()
			return
		case <-s.ctx.Done():
			s.tailers.stopAll()
			return
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

// sync arms a tailer for every active run not yet tailed and disarms tailers
// whose run is no longer active.
func (s *Service) sync(ctx context.Context) {
	runs, err := s.store.ActiveRuns(ctx)
	if err != nil {
		s.log.Warn("chat tailer sync: listing active runs", "component", "chat", "err", err)
		return
	}
	active := make(map[string]store.Run, len(runs))
	for _, r := range runs {
		active[r.SessionName] = r
	}
	s.tailers.retain(active, func(run store.Run) { s.arm(run) })
}

// arm starts a tailer goroutine for run under a child of the service ctx.
func (s *Service) arm(run store.Run) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.tailers.add(run.SessionName, cancel)
	go s.tail(ctx, run)
}

// tail is one run's poll loop: resolve the transcript path, then on each tick
// re-stat it and, when it changed, re-read → derive state → publish. The state
// is cached for the instance list even between changes.
func (s *Service) tail(ctx context.Context, run store.Run) {
	defer s.tailers.remove(run.SessionName)
	prov, ok := s.providers.Get(run.Provider)
	if !ok {
		return
	}
	t := time.NewTicker(s.poll)
	defer t.Stop()

	var (
		path    string
		lastMod time.Time
		lastSz  int64
		first   = true
	)
	for {
		if path == "" {
			path = s.resolvePath(ctx, prov, run)
		}
		if path != "" {
			if fi, err := os.Stat(path); err == nil {
				if first || fi.ModTime() != lastMod || fi.Size() != lastSz {
					first, lastMod, lastSz = false, fi.ModTime(), fi.Size()
					if chat, err := prov.ReadTranscript(path); err == nil {
						s.tailers.setState(run.SessionName, chat.State)
						s.publishMessagesChanged(run)
					}
				}
			}
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

// tailerSet is the concurrency-safe registry of live tailers and their last
// derived states.
type tailerSet struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	states  map[string]string
}

func newTailerSet() *tailerSet {
	return &tailerSet{cancels: map[string]context.CancelFunc{}, states: map[string]string{}}
}

// retain disarms tailers whose session is not in active, and calls arm for
// every active session not already tailed. arm must call add before starting.
func (ts *tailerSet) retain(active map[string]store.Run, arm func(store.Run)) {
	ts.mu.Lock()
	var toStop []context.CancelFunc
	for session, cancel := range ts.cancels {
		if _, ok := active[session]; !ok {
			toStop = append(toStop, cancel)
			delete(ts.cancels, session)
			delete(ts.states, session)
		}
	}
	var toArm []store.Run
	for session, run := range active {
		if _, ok := ts.cancels[session]; !ok {
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

func (ts *tailerSet) add(session string, cancel context.CancelFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.cancels[session] = cancel
}

// remove clears a tailer's cancel/state on goroutine exit. It does not call
// cancel (the goroutine is already returning).
func (ts *tailerSet) remove(session string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.cancels, session)
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
	cancels := make([]context.CancelFunc, 0, len(ts.cancels))
	for session, cancel := range ts.cancels {
		cancels = append(cancels, cancel)
		delete(ts.cancels, session)
		delete(ts.states, session)
	}
	ts.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
