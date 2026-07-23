// Package chat is the embedded-chat brain (issue #7 / ADR-0016): it reads an
// instance's conversation through the AgentProvider seam (ReadChat) into
// lab's universal message schema, drives replies / dialog answers / interrupts
// back into the live session, and runs a per-live-instance tailer that tracks
// conversational state and publishes a debounced run.messages.changed so the
// chat view refetches. State COMPOSITION is adapter-owned (issue #92): the
// provider folds its transcript and its own live signals (claude-code: the
// hook spool, ADR-0020) inside ReadChat, and core never interprets a spool —
// core owns run lifecycle only (the StateEnded override and the transcript
// identity). Read-through only — no message table; the sole persisted state
// is runs.transcript_path, captured by cwd-match exactly like the deep link —
// and, for an active run, re-pointed when the live session rotates its
// transcript (a /clear or /rewind starts a new sessionId → transcript file,
// compat §5), so the chat follows the fresh session (issue #34).
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
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/secrets"
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
	// State is the run's adapter-composed conversational state as of this tick
	// (issue #175): carrying it in the envelope lets the SPA update its state
	// chips without a refetch when nothing else changed. Still an envelope, not
	// state duplication in the ADR-0005 sense — one enumerated scalar the
	// tailer already holds at the publish site, carved out by the issue #175
	// maintainer decision.
	State string `json:"state"`
	// BackpatchSeq is the LOWEST seq whose rendered content changed relative
	// to the tailer's previous successful read of the same transcript file
	// (issue #175) — a tool status flipping running→ok, output growth, a
	// dialog gaining an outcome. Omitted (0) when the tick was append-only.
	// A seq rather than a bool so the client fetches after=min(cursor,
	// BackpatchSeq-1): ONE tail fetch covers both the appends and the
	// back-patch, replacing the old unconditional latest-window refetch. New
	// seqs are appends by definition and never set this; a rotation tick
	// (fresh transcript, seq restarts at 1) never sets it either — the client
	// resets its stream via transcript_id then.
	BackpatchSeq int64 `json:"backpatchSeq,omitempty"`
}

// Options configures a Service. Store, Providers, and Bus are required.
type Options struct {
	Store     *store.Store
	Providers *provider.Registry
	Bus       *events.Bus
	Logger    *slog.Logger

	// RuntimeDirFor maps a run id to its PRIVATE runtime dir
	// (<state>/instances/<runID>/runtime, issue #205) — where a LiveSignals
	// provider spools that run's live signals (ADR-0020). Injected as a
	// closure over the pure instancehome.Manager.RuntimePath, exactly like
	// HomeFor, so chat never imports the manager. Core never reads the spool
	// itself (issue #92): it passes the resolved dir through ReadChat for
	// ACTIVE runs — the adapter composes its own signals there. Ended runs
	// always read transcript-only ("" down the seam), and spool GC is no
	// longer chat's job: the per-run dir dies with the run's tree
	// (instancehome.Wipe/SweepAll). Nil disables live signals entirely —
	// every read degrades to transcript-only; cmd/lab passes
	// homes.RuntimePath.
	RuntimeDirFor func(runID string) string

	// Poll is the transcript poll/debounce cadence; nil → defaultPoll.
	Poll time.Duration
	// Ctx bounds the tailer goroutines; nil → context.Background(). cmd/lab
	// passes a shutdown-linked context.
	Ctx context.Context
	// Now overrides the clock (tests); nil → time.Now.
	Now func() time.Time

	// Notify, when non-nil, receives one Notification when a run's conversational
	// state settles into needs_input/question (edge-triggered with a flap
	// debounce; ~2s). The tailer invokes it synchronously on its tick loop, so it
	// must never block — push.Sender.Broadcast (async, fire-and-forget) satisfies
	// this. Injected by cmd/lab as a closure over the push sender so chat never
	// imports push. Nil → the trigger is absent (no other behavior change).
	Notify func(Notification)
	// NotifyDebounce overrides the flap-debounce window; 0 → the 2s default.
	// Tests shrink it to drive the loop-level path quickly.
	NotifyDebounce time.Duration

	// Secrets, when non-nil, returns the repo's current secret redactor — nil
	// when the repo has no secrets (secrets.Source's zero-secret fast path, so
	// a repo without secrets pays no per-message cost). Injected by cmd/lab as
	// a closure over the secrets source so chat never touches the vault or an
	// encrypted blob (issue #108). Nil → transcript scanning/masking is off
	// (the feature is absent, like a nil Notify).
	Secrets func(ctx context.Context, repoID string) (*secrets.Redactor, error)

	// HomeFor maps a run id to its private instance HOME (issue #202) — the
	// pure instancehome.Manager.HomePath, injected as a closure so chat need not
	// import the manager (its idiom for cross-package seams, like Notify and
	// Secrets). LocateTranscript resolves the transcript strictly under this
	// home. Nil → "" for every run, which is a locate MISS by the seam contract
	// (the pre-upgrade / no-home degradation, keeping the run's already-stored
	// path) — the same effect HomePath gives for a run whose home never existed.
	HomeFor func(runID string) string
}

// defaultPoll is the tailer's file-change poll cadence. It doubles as the
// run.messages.changed debounce window: at most one event per instance per
// tick, no matter how fast claude appends.
const defaultPoll = 1 * time.Second

// Service is the chat brain. Construct with New; start the tailer with Run.
type Service struct {
	store     *store.Store
	providers *provider.Registry
	bus       *events.Bus
	log       *slog.Logger
	poll      time.Duration
	ctx       context.Context
	now       func() time.Time

	// runtimeDirFor maps a run id to its private runtime dir (issue #205),
	// nil when cmd/lab wired none — see Options.RuntimeDirFor. Read through
	// runtimeDir() so a nil closure degrades to "" (live signals off).
	runtimeDirFor func(runID string) string

	// notify is the injected push seam (issue #99), nil when cmd/lab wired none;
	// notifyDebounce is the gate's flap-debounce window.
	notify         func(Notification)
	notifyDebounce time.Duration

	// secrets is the injected per-repo redactor seam (issue #108), nil when
	// cmd/lab wired none — see Options.Secrets and redact.go.
	secrets func(ctx context.Context, repoID string) (*secrets.Redactor, error)

	// homeFor maps a run id to its private instance HOME (issue #202), nil when
	// cmd/lab wired none — see Options.HomeFor. Read through home() so a nil
	// closure degrades to "" (a locate miss).
	homeFor func(runID string) string

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
	notifyDebounce := o.NotifyDebounce
	if notifyDebounce <= 0 {
		notifyDebounce = defaultNotifyDebounce
	}
	return &Service{
		store:         o.Store,
		providers:     o.Providers,
		bus:           o.Bus,
		log:           log,
		poll:          poll,
		ctx:           ctx,
		now:           now,
		runtimeDirFor: o.RuntimeDirFor,
		// Store Notify verbatim (no nil-default, like reconcile's afkRunEnded): a
		// nil closure stays nil and the tailer allocates no gate. Same for
		// Secrets: nil stays nil and scanAndRedact returns before touching a run.
		notify:         o.Notify,
		notifyDebounce: notifyDebounce,
		secrets:        o.Secrets,
		homeFor:        o.HomeFor,
		tailers:        newTailerSet(),
	}, nil
}

// home resolves a run's private instance HOME (issue #202) through the injected
// homeFor closure, degrading to "" — a locate MISS by the seam contract — when
// none was wired (a test service, or a build without the instance stack).
func (s *Service) home(runID string) string {
	if s.homeFor == nil {
		return ""
	}
	return s.homeFor(runID)
}

// runtimeDir resolves a run's private runtime dir (issue #205) through the
// injected runtimeDirFor closure, degrading to "" — live signals off, the
// transcript-only read — when none was wired (a test service, or a build
// without the instance stack). home's exact sibling.
func (s *Service) runtimeDir(runID string) string {
	if s.runtimeDirFor == nil {
		return ""
	}
	return s.runtimeDirFor(runID)
}

// View is the chat service's read of one run: the adapter-composed
// conversation (embedded provider.Chat — messages, state, cursor, and the
// side-channel PendingDialog, issue #92) plus the core-owned transcript
// identity. Core forwards the adapter's composition verbatim — including the
// live dialog on Chat.PendingDialog, whose doc pins the seq-stability
// invariant (issues #89/#90) — and owns only run lifecycle: a terminated
// run's view is forced to StateEnded with no dialog, regardless of what the
// adapter returned.
type View struct {
	provider.Chat
	// TranscriptID is the opaque, provider-neutral identity of the transcript
	// this view was read from — a stable hash of the resolved path (transcriptID)
	// that changes exactly when the run's transcript rotates (a /clear or /rewind
	// starts a new sessionId → transcript file, compat §5, and lab re-points the
	// run at it). The chat view watches this token to reset its accumulated
	// stream, since the fresh transcript restarts seq at 1. "" when no transcript
	// is located yet (issue #34).
	TranscriptID string
}

// Read returns the run's chat: it resolves the transcript path (using the
// persisted one, else — for an active run — a best-effort locate that is
// persisted on a hit) and reads through the provider's ReadChat, which
// composes the conversational state from the transcript plus the adapter's
// own live signals (issue #92 — composition is adapter-owned; core never
// interprets a spool). Core contributes only run lifecycle:
//
//   - an ACTIVE run reads with its per-run runtime dir (issue #205), so the
//     adapter's live signals apply — including the no-transcript-yet case
//     (path ""), where the adapter must still consult them (a pending dialog
//     can exist before LocateTranscript first hits) and otherwise yields an
//     idle empty chat;
//   - an ENDED run reads transcript-only (runtimeDir "") BY CONSTRUCTION, so
//     no spool residue can leak into a terminal view, and is then forced to
//     StateEnded with no pending dialog — terminal state is core-owned,
//     whatever the adapter returned;
//   - an ended run with no captured transcript (or a retired transcript file,
//     via the adapter's own ErrTranscriptGone) is the "transcript no longer
//     available" state.
func (s *Service) Read(ctx context.Context, run store.Run) (View, error) {
	prov, ok := s.providers.Get(run.Provider)
	if !ok {
		return View{}, fmt.Errorf("chat: unknown provider %q", run.Provider)
	}
	path := s.resolvePath(ctx, prov, run)
	active := run.Outcome == store.RunOutcomeActive
	if path == "" && !active {
		return View{}, provider.ErrTranscriptGone
	}
	dir := ""
	if active {
		dir = s.runtimeDir(run.ID)
	}
	chat, err := prov.ReadChat(run.ID, dir, path)
	if err != nil {
		return View{}, err
	}
	// Mask secret values before the view is built (issue #108): every HTTP
	// read — GET /runs/{id}/messages — flows through here, including an ended
	// run's history, so a disclosure that predates the exposure feature (or a
	// tick the tailer missed) is still masked on render.
	s.scanAndRedact(ctx, run, &chat)
	// Stamp per-message content hashes AFTER redaction (issue #175), in core
	// rather than the adapters so every provider gets them for free and the
	// hash always covers the SERVED (masked) content — a pre-redaction hash
	// would differ between this path and the tailer's. The tailer read is the
	// only other stamping site.
	provider.HashMessages(chat.Messages)
	// transcriptID("") is "" — the active-no-transcript case needs no branch:
	// the view is exactly what ReadChat("", …) composed, with no identity yet.
	view := View{Chat: chat, TranscriptID: transcriptID(path)}
	if !active {
		view.State = provider.StateEnded
		view.PendingDialog = nil // defensive: a transcript-only read composes none
	}
	return view, nil
}

// resolvePath returns the transcript path for run. An ENDED run returns its
// persisted path verbatim and NEVER re-locates: the locate is a cwd-match
// against the newest LIVE claude in the worktree, so any hit would by definition
// belong to a newer run reusing the worktree — adopting it would permanently
// point this run at someone else's transcript (the successor-safety this guard
// preserves). An ACTIVE run re-asks the provider each read and adopts a rotation
// (locateActive). "" means not found yet.
func (s *Service) resolvePath(ctx context.Context, prov provider.AgentProvider, run store.Run) string {
	known := ""
	if run.TranscriptPath != nil {
		known = *run.TranscriptPath
	}
	if run.Outcome != store.RunOutcomeActive {
		return known
	}
	return s.locateActive(ctx, prov, run, known)
}

// locateActive re-locates the live transcript for an ACTIVE run and follows a
// rotation, persisting a newly adopted path so the first chat open works before
// the tailer has run and so an ended run stays readable. known is the caller's
// current best path — the persisted path for a Read (run comes fresh from the
// store per request), the tailer's own cached path for the poll loop (whose
// in-memory run is captured stale at arm time, so run.TranscriptPath would
// mislead it). Detecting the rotation by its EFFECT — the located transcript
// changing — keeps lab core ignorant of any agent's clear command (the design's
// "detect by effect, not by cause"); it also covers a clear run out-of-band
// (inside claude.ai, or /rewind) for free. Rules:
//   - a locate miss (transient registry gap / between-states) keeps known — an
//     empty result never clobbers a path already in hand;
//   - the same path is a no-op;
//   - a different, non-empty path is a rotation (a /clear or /rewind rotated the
//     sessionId → a fresh transcript file, compat §5): adopt and persist it.
func (s *Service) locateActive(ctx context.Context, prov provider.AgentProvider, run store.Run, known string) string {
	// The transcript is resolved strictly under the run's private instance HOME
	// (issue #202) — the LocateTranscript contract. Only active runs reach here,
	// and a pre-upgrade run's nonexistent home yields a locate miss ("", nil),
	// which the miss branch below turns into "keep known" (the doc comment's
	// promise) — never a fall back to the master store.
	path, err := prov.LocateTranscript(ctx, run.SessionName, run.WorktreePath, s.home(run.ID))
	if err != nil || path == "" {
		return known
	}
	if path == known {
		return known
	}
	// Adopting a DIFFERENT path persists it — re-verify the run is STILL active
	// against the store first. A caller's run.Outcome can be stale: the tailer
	// captures it at arm time and can outlive the run's end by up to one resync
	// interval when a run.changed is dropped (tailer's Run doc). LocateTranscript
	// is a pure cwd-match, so without this a since-ended run would adopt a
	// successor run's transcript in a reused worktree and permanently point its
	// row at someone else's chat — the successor-safety resolvePath's ended-run
	// guard preserves. Costs one store read per rotation (rare), never per tick.
	if cur, err := s.store.RunByID(ctx, run.ID); err != nil || cur.Outcome != store.RunOutcomeActive {
		return known
	}
	if err := s.store.UpdateRunTranscriptPath(ctx, run.ID, path); err != nil {
		s.log.Warn("persisting located transcript", "component", "chat",
			"run", run.ID, "session", run.SessionName, "err", err)
	}
	return path
}

// transcriptID is the opaque identity the chat view watches to notice a
// transcript rotation: a stable hash of the resolved path that changes exactly
// when the path does. Empty in → empty out (nothing located yet). Provider-
// neutral in the envelope (any provider's path hashes the same way) yet
// provider-derived in substance — the path is the provider's, and this is the
// same value the backend compares to detect the rotation (issue #34).
func transcriptID(path string) string {
	if path == "" {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(path))
	return fmt.Sprintf("%016x", h.Sum64())
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

// PendingDialog returns the run's currently pending dialog, if any. The
// adapter's ReadChat composition is authoritative (issue #92): a live dialog
// arrives on Chat.PendingDialog (claude-code reads its PreToolUse spool,
// ADR-0020); the transcript-tail scan below is the fallback for a session that
// DOES flush a pending tool_use (StateQuestion with a nil side-channel dialog).
//
// That fallback is NOT dead code — since issue #163 it is the LIVE path for
// every non-remote run. The flush-on-resolve behavior the spool relies on turns
// out to be a REMOTE-CONTROL behavior: spawned without --remote-control, claude
// flushes a pending tool_use to the transcript immediately (compat §12, live
// A/B on 2.1.206), so the spool overlay self-suppresses (the tool id already
// reads as resolved) and the dialog surfaces here instead. Answering through
// this path is live-verified. Deleting it would break dialogs on every
// remote-off run.
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
// server-side StateQuestion the adapter's ReadChat composition drives
// (ADR-0020 decision 5; adapter-owned since issue #92).
func (s *Service) dialogPending(ctx context.Context, run store.Run) bool {
	view, err := s.Read(ctx, run)
	if err != nil {
		return false
	}
	return view.State == provider.StateQuestion
}

// lastDialog returns the last PENDING dialog message's Dialog, if any.
// Answered dialogs (Outcome set) stay in the stream as history since issue
// #56, so this dormant-fallback answer path must skip them — a resolved
// dialog is never a keystroke target, and returning one would aim AnswerDialog
// at a picker that no longer exists.
func lastDialog(msgs []provider.Message) (provider.Dialog, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind == provider.MessageDialog && msgs[i].Dialog != nil && msgs[i].Dialog.Outcome == nil {
			return *msgs[i].Dialog, true
		}
	}
	return provider.Dialog{}, false
}
