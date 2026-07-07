// Package chat is the embedded-chat brain (issue #7 / ADR-0016): it reads an
// instance's provider-native transcript through the AgentProvider seam into
// lab's universal message schema, drives replies / dialog answers / interrupts
// back into the live session, and runs a per-live-instance tailer that derives
// conversational state and publishes a debounced run.messages.changed so the
// chat view refetches. Read-through only — no message table; the sole
// persisted state is runs.transcript_path, captured by cwd-match exactly like
// the deep link.
//
// Intervention neutrality (issue #7 decision 12): a reply, dialog answer, or
// interrupt goes through provider send-keys and never touches a run's budget
// clock, claim, or three-strikes counter — none of this code writes a run
// outcome or ends a session.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// EventMessagesChanged is the SSE event published when a live instance's
// transcript changes: the chat view refetches GET /runs/{id}/messages. Payload
// is the small run-scoped envelope (brief §8.1 — envelopes, not state).
const EventMessagesChanged = "run.messages.changed"

// ErrRunEnded is returned by Reply/AnswerDialog/Interrupt for a run past its
// active outcome — the composer is closed for ended instances. The API maps it
// to 409.
var ErrRunEnded = errors.New("run has ended; the chat is read-only")

// ErrDialogPending is returned by Reply when an interactive dialog is pending —
// free text is locked so it cannot hit a focused picker (issue #7 decision 5).
// The API maps it to 409.
var ErrDialogPending = errors.New("a dialog is pending; answer it or interrupt first")

// ErrNoDialog is returned by AnswerDialog when no dialog is pending. The API
// maps it to 409.
var ErrNoDialog = errors.New("no dialog is pending")

// ErrDialogChanged is returned by AnswerDialog when the client's tool_id does
// not match the live pending dialog — a stale answer must never drive
// keystrokes into a picker it wasn't built for. The API maps it to 409.
var ErrDialogChanged = errors.New("the pending dialog has changed; reload the chat")

type messagesChangedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
	RunID  string `json:"runID"`
}

// Options configures a Service. Store, Providers, and Bus are required.
type Options struct {
	Store     *store.Store
	Providers *provider.Registry
	Bus       *events.Bus
	Logger    *slog.Logger

	// RuntimeDir is lab's runtime dir (<state>/runtime — the materializer dir),
	// where a DialogHooker provider spools a run's pending dialog + blocked
	// marker (ADR-0020). Empty disables the spool read/GC (the chat keeps
	// transcript-only behavior); cmd/lab passes the materializer's Dir().
	RuntimeDir string

	// Poll is the transcript poll/debounce cadence; nil → defaultPoll.
	Poll time.Duration
	// Ctx bounds the tailer goroutines; nil → context.Background(). cmd/lab
	// passes a shutdown-linked context.
	Ctx context.Context
	// Now overrides the clock (tests); nil → time.Now.
	Now func() time.Time
}

// defaultPoll is the tailer's file-change poll cadence. It doubles as the
// run.messages.changed debounce window: at most one event per instance per
// tick, no matter how fast claude appends.
const defaultPoll = 1 * time.Second

// Service is the chat brain. Construct with New; start the tailer with Run.
type Service struct {
	store      *store.Store
	providers  *provider.Registry
	bus        *events.Bus
	log        *slog.Logger
	poll       time.Duration
	ctx        context.Context
	now        func() time.Time
	runtimeDir string

	tailers *tailerSet

	// sessions serializes interventions per session name: each reply, dialog
	// answer, or interrupt is 1..N tmux calls, and two interleaving at the
	// tmux level would merge pastes or land keystrokes on the wrong picker
	// row. The guard read (pending dialog / tool_id match) runs under the
	// same lock, closing the check-then-send race.
	sessions keyedMutex
}

// keyedMutex is a per-key lock (precedent: httpapi's per-repo crMu). Entries
// are never removed — the key space is session names, bounded by the instance
// cap.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (k *keyedMutex) get(key string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &sync.Mutex{}
		k.locks[key] = l
	}
	return l
}

// New validates o and returns a Service.
func New(o Options) (*Service, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("chat: Options.Store is required")
	case o.Providers == nil:
		return nil, errors.New("chat: Options.Providers is required")
	case o.Bus == nil:
		return nil, errors.New("chat: Options.Bus is required")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	poll := o.Poll
	if poll <= 0 {
		poll = defaultPoll
	}
	ctx := o.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:      o.Store,
		providers:  o.Providers,
		bus:        o.Bus,
		log:        log,
		poll:       poll,
		ctx:        ctx,
		now:        now,
		runtimeDir: o.RuntimeDir,
		tailers:    newTailerSet(),
	}, nil
}

// View is the chat service's combined read: the transcript-derived chat plus
// the live spool signals a DialogHooker provider surfaces (ADR-0020). The
// message stream (embedded provider.Chat) stays purely transcript-derived —
// PendingDialog is a side-channel field, never a synthetic message, so seq
// numbers remain reparse-stable when the real tool_use retro-flushes.
type View struct {
	provider.Chat
	// PendingDialog is the run's live interactive dialog read from the
	// PreToolUse spool (nil when none is pending). When set, State is
	// StateQuestion.
	PendingDialog *provider.Dialog
}

// Read returns the run's chat: it resolves the transcript path (using the
// persisted one, else — for an active run — a best-effort locate that is
// persisted on a hit), reads it through the provider, and stamps the
// conversational state — StateEnded overrides the transcript-derived state for
// a terminated run. An active run with no transcript yet returns an empty chat
// in StateIdle (the chat view shows a "waiting for the transcript"
// placeholder); an ended run that never captured one, like a retired
// transcript file, returns provider.ErrTranscriptGone.
func (s *Service) Read(ctx context.Context, run store.Run) (View, error) {
	prov, ok := s.providers.Get(run.Provider)
	if !ok {
		return View{}, fmt.Errorf("chat: unknown provider %q", run.Provider)
	}
	path := s.resolvePath(ctx, prov, run)
	if path == "" {
		if run.Outcome != store.RunOutcomeActive {
			return View{}, provider.ErrTranscriptGone
		}
		// Active but no transcript located yet — still consult the spool: a
		// pending dialog can exist before LocateTranscript first hits.
		view := View{Chat: provider.Chat{State: provider.StateIdle}}
		s.applyLiveSignals(&view, run, "")
		return view, nil
	}
	chat, err := prov.ReadTranscript(path)
	if err != nil {
		return View{}, err
	}
	view := View{Chat: chat}
	if run.Outcome != store.RunOutcomeActive {
		view.State = provider.StateEnded // a terminal run's state never comes from the spool
		return view, nil
	}
	s.applyLiveSignals(&view, run, path)
	return view, nil
}

// applyLiveSignals overlays a DialogHooker provider's spool signals onto an
// ACTIVE run's transcript-derived view (ADR-0020). Precedence: a live pending
// dialog forces StateQuestion and is surfaced as View.PendingDialog (kept out
// of the message stream — decision 5); else, when the transcript itself does
// not already show a pending dialog (the dormant flushed-tool_use fallback), a
// live blocked marker forces StateNeedsInput (decision 7). A provider without
// the capability, or an empty RuntimeDir, is a no-op — transcript-only.
func (s *Service) applyLiveSignals(view *View, run store.Run, path string) {
	hooker, ok := s.hooker(run.Provider)
	if !ok {
		return
	}
	if d, ok := hooker.PendingDialog(run.ID, s.runtimeDir, path); ok {
		view.State = provider.StateQuestion
		dc := d
		view.PendingDialog = &dc
		return
	}
	if view.State == provider.StateQuestion {
		return // a transcript-flushed dialog stands on its own (dormant fallback)
	}
	if st, ok := hooker.BlockedState(run.ID, s.runtimeDir, path); ok {
		view.State = st
	}
}

// hooker resolves a provider id to its optional DialogHooker capability
// (ADR-0020). (nil, false) when the provider is unregistered, lacks the
// capability, or the runtime dir is unset.
func (s *Service) hooker(providerID string) (provider.DialogHooker, bool) {
	if s.runtimeDir == "" {
		return nil, false
	}
	prov, ok := s.providers.Get(providerID)
	if !ok {
		return nil, false
	}
	h, ok := prov.(provider.DialogHooker)
	return h, ok
}

// resolvePath returns the transcript path for run: the persisted one if set,
// otherwise a best-effort locate that is persisted (and announced) on a hit so
// the first chat open works before the tailer has run. "" means not found yet.
// Locating only ever runs for an active run: the locate is a cwd-match against
// the newest LIVE claude in the worktree, so for an ended run any hit would by
// definition belong to a newer run reusing the worktree — persisting it would
// permanently point this run at someone else's transcript.
func (s *Service) resolvePath(ctx context.Context, prov provider.AgentProvider, run store.Run) string {
	if run.TranscriptPath != nil && *run.TranscriptPath != "" {
		return *run.TranscriptPath
	}
	if run.Outcome != store.RunOutcomeActive {
		return ""
	}
	path, err := prov.LocateTranscript(ctx, run.SessionName, run.WorktreePath)
	if err != nil || path == "" {
		return ""
	}
	if err := s.store.UpdateRunTranscriptPath(ctx, run.ID, path); err != nil {
		s.log.Warn("persisting located transcript", "component", "chat",
			"run", run.ID, "session", run.SessionName, "err", err)
	}
	return path
}

// Reply delivers a free-text reply to a live instance. It refuses ended runs
// and, so stray text can't hit a focused picker, a run with a pending dialog.
func (s *Service) Reply(ctx context.Context, run store.Run, text string) error {
	prov, err := s.liveProvider(run)
	if err != nil {
		return err
	}
	mu := s.sessions.get(run.SessionName)
	mu.Lock()
	defer mu.Unlock()
	if s.dialogPending(ctx, run) {
		return ErrDialogPending
	}
	return prov.Reply(ctx, run.SessionName, text)
}

// AnswerDialog answers the pending dialog on a live instance. It re-reads the
// live dialog under the session lock and requires toolID to match it, so a
// stale client never drives keystrokes into a picker that already moved on —
// the keystroke recipe is always built from the live dialog, not the client's
// copy.
func (s *Service) AnswerDialog(ctx context.Context, run store.Run, toolID string, answer provider.DialogAnswer) error {
	prov, err := s.liveProvider(run)
	if err != nil {
		return err
	}
	mu := s.sessions.get(run.SessionName)
	mu.Lock()
	defer mu.Unlock()
	dialog, ok := s.PendingDialog(ctx, run)
	if !ok {
		return ErrNoDialog
	}
	if toolID != dialog.ToolID {
		return ErrDialogChanged
	}
	return prov.AnswerDialog(ctx, run.SessionName, dialog, answer)
}

// Interrupt sends the interrupt keystroke to a live instance.
func (s *Service) Interrupt(ctx context.Context, run store.Run) error {
	prov, err := s.liveProvider(run)
	if err != nil {
		return err
	}
	mu := s.sessions.get(run.SessionName)
	mu.Lock()
	defer mu.Unlock()
	return prov.Interrupt(ctx, run.SessionName)
}

// PendingDialog returns the run's currently pending dialog, if any. The live
// PreToolUse spool is authoritative (ADR-0020) — Claude Code never flushes a
// pending tool_use to the transcript, so the spool is the only live source;
// the transcript-tail scan stays as the dormant fallback for a future provider
// that does flush pending tool_use.
func (s *Service) PendingDialog(ctx context.Context, run store.Run) (provider.Dialog, bool) {
	view, err := s.Read(ctx, run)
	if err != nil {
		return provider.Dialog{}, false
	}
	if view.PendingDialog != nil {
		return *view.PendingDialog, true
	}
	if view.State != provider.StateQuestion {
		return provider.Dialog{}, false
	}
	return lastDialog(view.Messages)
}

// State reports the tailer's latest derived conversational state for a live
// session, for the instance list. Absent (ended or never-tailed) → "", false.
func (s *Service) State(session string) (string, bool) {
	return s.tailers.state(session)
}

func (s *Service) liveProvider(run store.Run) (provider.AgentProvider, error) {
	if run.Outcome != store.RunOutcomeActive {
		return nil, ErrRunEnded
	}
	prov, ok := s.providers.Get(run.Provider)
	if !ok {
		return nil, fmt.Errorf("chat: unknown provider %q", run.Provider)
	}
	return prov, nil
}

// dialogPending reports whether a dialog is pending for the run — the composer
// lock (a free-text reply must not hit a focused picker). Keys off the same
// server-side StateQuestion the spool drives (ADR-0020 decision 5).
func (s *Service) dialogPending(ctx context.Context, run store.Run) bool {
	view, err := s.Read(ctx, run)
	if err != nil {
		return false
	}
	return view.State == provider.StateQuestion
}

// lastDialog returns the last dialog message's Dialog, if the tail is one.
func lastDialog(msgs []provider.Message) (provider.Dialog, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind == provider.MessageDialog && msgs[i].Dialog != nil {
			return *msgs[i].Dialog, true
		}
	}
	return provider.Dialog{}, false
}
