package tmuxx

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// FakeSession is what the Fake recorded about one Start call.
type FakeSession struct {
	Dir      string
	Argv     []string
	ExtraEnv []string
}

// SentKey is one recorded SendKeys call.
type SentKey struct {
	Text  string
	Enter bool
}

// Fake is the behavioral SessionRunner test double (design §4d: fakes
// live next to the interface). It mirrors the real semantics the service
// builders lean on: idempotent Start/Stop, exact-name liveness, "absent
// session" errors from SendKeys/CapturePane, and a missing server being
// an empty List. Test-only controls: pane buffers are scriptable
// (SetPane), SendKeys calls are recorded (Sent), sessions can be
// pre-seeded (AddLive — restart re-adoption tests) or killed out from
// under lab (Kill — reaper tests), and per-call errors can be injected.
// Safe for concurrent use.
type Fake struct {
	mu           sync.Mutex
	sessions     map[string]FakeSession
	panes        map[string]string
	sent         map[string][]SentKey
	startErr     map[string]error
	startErrLive map[string]error
	stopErr      map[string]error
	sendErr      map[string]error
	listErr      error
}

var _ SessionRunner = (*Fake)(nil)

// NewFake returns an empty Fake: no live sessions, no server error.
func NewFake() *Fake {
	return &Fake{
		sessions:     map[string]FakeSession{},
		panes:        map[string]string{},
		sent:         map[string][]SentKey{},
		startErr:     map[string]error{},
		startErrLive: map[string]error{},
		stopErr:      map[string]error{},
		sendErr:      map[string]error{},
	}
}

// Start records the session as live with copies of argv and extraEnv
// (fresh-slice discipline: a caller's later append must not alias into
// the fake). Idempotent like the real one: an already-live name returns
// nil and keeps the original record.
func (f *Fake) Start(_ context.Context, name, dir string, argv []string, extraEnv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.startErr[name]; err != nil {
		return err
	}
	if _, ok := f.sessions[name]; ok {
		return nil
	}
	f.sessions[name] = FakeSession{
		Dir:      dir,
		Argv:     append([]string(nil), argv...),
		ExtraEnv: append([]string(nil), extraEnv...),
	}
	if err := f.startErrLive[name]; err != nil {
		// Session recorded live, but Start reports failure — models tmux's
		// daemonized new-session succeeding while Start still errors (ctx
		// cancelled during the 500ms post-spawn recheck). Rollback must Stop it.
		return err
	}
	return nil
}

// Stop removes the session. Idempotent: an absent name returns nil.
func (f *Fake) Stop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.stopErr[name]; err != nil {
		return err
	}
	delete(f.sessions, name)
	return nil
}

// IsRunning reports whether the exact name is live.
func (f *Fake) IsRunning(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.sessions[name]
	return ok, nil
}

// List returns the live names, sorted for determinism. No sessions →
// nil, nil (the real runner's missing-server shape).
func (f *Fake) List(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.sessions) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(f.sessions))
	for n := range f.sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// SendKeys records the call. An absent session is an error, like the
// real send-keys.
func (f *Fake) SendKeys(_ context.Context, name, text string, enter bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.sendErr[name]; err != nil {
		return err
	}
	if _, ok := f.sessions[name]; !ok {
		return fmt.Errorf("tmux send-keys (literal): no such session %q", name)
	}
	f.sent[name] = append(f.sent[name], SentKey{Text: text, Enter: enter})
	return nil
}

// CapturePane returns the scripted pane buffer. An absent session is an
// error, like the real capture-pane.
func (f *Fake) CapturePane(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[name]; !ok {
		return "", fmt.Errorf("tmux capture-pane: no such session %q", name)
	}
	return f.panes[name], nil
}

// --- test controls -------------------------------------------------------

// AddLive pre-seeds a live session without a Start call — the "sessions
// survived a lab restart" fixture for re-adoption tests.
func (f *Fake) AddLive(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[name]; !ok {
		f.sessions[name] = FakeSession{}
	}
}

// Kill removes a session as if its process exited outside lab's control
// (agent finished, crashed, was killed) — no Stop bookkeeping runs.
func (f *Fake) Kill(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, name)
}

// SetPane scripts the pane buffer CapturePane returns for name.
func (f *Fake) SetPane(name, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes[name] = content
}

// Session returns the recorded Start of name and whether it is live.
func (f *Fake) Session(name string) (FakeSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[name]
	return s, ok
}

// Sent returns a copy of the SendKeys calls recorded for name, in order.
// The record survives Stop/Kill so tests can assert after teardown.
func (f *Fake) Sent(name string) []SentKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SentKey(nil), f.sent[name]...)
}

// FailStart makes Start(name) return err (nil clears the injection). The
// session is NOT recorded live — models new-session failing outright.
func (f *Fake) FailStart(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErr[name] = err
}

// FailStartLive makes Start(name) record the session as live AND return err —
// models tmux's daemonized new-session succeeding while Start still errors
// (request ctx cancelled during the post-spawn recheck). Used to prove
// rollback reaps the orphan session.
func (f *Fake) FailStartLive(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErrLive[name] = err
}

// FailStop makes Stop(name) return err (nil clears the injection).
func (f *Fake) FailStop(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopErr[name] = err
}

// FailSendKeys makes SendKeys(name, …) return err (nil clears the
// injection).
func (f *Fake) FailSendKeys(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr[name] = err
}

// FailList makes List return err (nil clears the injection).
func (f *Fake) FailList(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
}
