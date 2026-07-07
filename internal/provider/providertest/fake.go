// Package providertest provides a controllable fake AgentProvider for the
// instance/reconcile/httpapi test suites (design §4d: fakes live next to the
// interface). It records SpawnArgv/SeedWorkspace/CaptureDeepLink calls and lets
// tests script auth state, the captured deep link, seeding failures, and the
// login flow.
package providertest

import (
	"context"
	"errors"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Fake is a scriptable AgentProvider. Its ID defaults to "claude-code" so it
// resolves for a repo's default provider. Safe for concurrent use.
type Fake struct {
	mu sync.Mutex

	id      string
	models  []provider.Option
	efforts []provider.Option

	loggedIn   bool
	email      string
	method     string
	now        func() time.Time
	authForced int // count of forced AuthStatus calls

	deepLink     string                  // returned by CaptureDeepLink; "" → a miss (ADR-0017)
	fallbackOpen provider.OpenAffordance // returned by FallbackOpen (the generic web link + title)
	seedErr      error
	seeded       []string            // worktrees passed to SeedWorkspace, in order
	seedOpts     []provider.SeedOpts // the SeedOpts of each SeedWorkspace call, in order
	connect      map[string]bool
	oauthURL     string
	loginErr     error
	codeErr      error
	codes        []string // codes submitted via LoginSubmitCode
	captureCt    int      // CaptureDeepLink call count

	// Chat surface (issue #7). transcriptPath is what LocateTranscript
	// returns (""→miss); chat is what ReadTranscript returns; readErr forces
	// a ReadTranscript error (e.g. provider.ErrTranscriptGone). Replies,
	// answers, and interrupts are recorded for assertions.
	transcriptPath string
	chat           provider.Chat
	readErr        error
	locateCt       int
	replies        []string
	answers        []provider.DialogAnswer
	interrupts     int
	replyErr       error
	interruptErr   error
}

var (
	_ provider.AgentProvider      = (*Fake)(nil)
	_ provider.ConnectingReporter = (*Fake)(nil)
	_ provider.DeepLinker         = (*Fake)(nil)
)

// New returns a logged-in fake with the claude-code id and catalogs.
func New() *Fake {
	return &Fake{
		id: "claude-code",
		models: []provider.Option{
			{Value: "opus[1m]", Label: "Opus (1M)"},
			{Value: "sonnet", Label: "Sonnet"},
			{Value: "fable", Label: "Fable"},
			{Value: "haiku", Label: "Haiku"},
		},
		efforts: []provider.Option{
			{Value: "low", Label: "low"}, {Value: "medium", Label: "medium"},
			{Value: "high", Label: "high"}, {Value: "xhigh", Label: "xhigh"}, {Value: "max", Label: "max"},
		},
		loggedIn:     true,
		email:        "op@example.invalid",
		method:       "claude.ai",
		now:          time.Now,
		deepLink:     "https://claude.ai/code/session_fake",
		fallbackOpen: provider.OpenAffordance{URL: "https://claude.ai/code", Title: "Opens the claude.ai session picker — the exact deep link wasn't captured"},
		connect:      map[string]bool{},
		oauthURL:     "https://claude.com/cai/oauth/authorize?code=true",
	}
}

func (f *Fake) ID() string { return f.id }

func (f *Fake) Models() []provider.Option  { return f.models }
func (f *Fake) Efforts() []provider.Option { return f.efforts }

// SpawnArgv mirrors the pinned claude argv shape so tests can assert the
// recorded tmux argv, including the seed prompt carried as the trailing
// positional (non-empty initialPrompt) — manual spawns pass "" and get none.
func (f *Fake) SpawnArgv(session, model, effort, initialPrompt string) []string {
	argv := []string{"claude", "--remote-control", session, "--permission-mode", "auto"}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "--effort", effort)
	}
	if initialPrompt != "" {
		argv = append(argv, initialPrompt)
	}
	return argv
}

func (f *Fake) AuthStatus(_ context.Context, force bool) (provider.AuthStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if force {
		f.authForced++
	}
	st := provider.AuthStatus{LoggedIn: f.loggedIn, Email: f.email, Method: f.method, CheckedAt: f.now()}
	if !f.loggedIn {
		return st, nil
	}
	return st, nil
}

func (f *Fake) LoginStart(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.oauthURL, f.loginErr
}

func (f *Fake) LoginSubmitCode(_ context.Context, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes = append(f.codes, code)
	return f.codeErr
}

// CaptureDeepLink implements provider.DeepLinker: it returns the scripted real
// link, or "" when the deep link was cleared (a miss — ADR-0017: the generic
// fallback is FallbackOpen's job, never returned through capture).
func (f *Fake) CaptureDeepLink(_ context.Context, session, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captureCt++
	return f.deepLink, nil
}

// FallbackOpen implements provider.DeepLinker: the scripted generic web open
// affordance (URL + title) the SPA renders on a capture miss.
func (f *Fake) FallbackOpen() provider.OpenAffordance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fallbackOpen
}

func (f *Fake) SeedWorkspace(worktree string, opts provider.SeedOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seedErr != nil {
		return f.seedErr
	}
	f.seeded = append(f.seeded, worktree)
	f.seedOpts = append(f.seedOpts, opts)
	return nil
}

func (f *Fake) Connecting(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connect[session]
}

// --- chat surface ---------------------------------------------------------

// LocateTranscript returns the scripted path ("" → not found yet) and counts
// the call, mirroring CaptureDeepLink's miss-is-not-an-error contract.
func (f *Fake) LocateTranscript(_ context.Context, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locateCt++
	return f.transcriptPath, nil
}

// ReadTranscript returns the scripted chat, or the scripted read error
// (e.g. provider.ErrTranscriptGone).
func (f *Fake) ReadTranscript(string) (provider.Chat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return provider.Chat{}, f.readErr
	}
	return f.chat, nil
}

// Reply records the reply text (or returns the scripted error).
func (f *Fake) Reply(_ context.Context, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replyErr != nil {
		return f.replyErr
	}
	f.replies = append(f.replies, text)
	return nil
}

// AnswerDialog records the answer.
func (f *Fake) AnswerDialog(_ context.Context, _ string, _ provider.Dialog, answer provider.DialogAnswer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, answer)
	return nil
}

// Interrupt records the call (or returns the scripted error).
func (f *Fake) Interrupt(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.interruptErr != nil {
		return f.interruptErr
	}
	f.interrupts++
	return nil
}

// --- test controls -------------------------------------------------------

// SetLoggedIn scripts the auth state the forced pre-spawn refresh reads.
func (f *Fake) SetLoggedIn(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedIn = v
}

// SetDeepLink scripts CaptureDeepLink's real hit ("" → a miss).
func (f *Fake) SetDeepLink(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deepLink = url
}

// SetFallbackOpen scripts FallbackOpen's generic web open affordance.
func (f *Fake) SetFallbackOpen(fo provider.OpenAffordance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallbackOpen = fo
}

// SetSeedError makes SeedWorkspace fail (the Start-rollback trigger).
func (f *Fake) SetSeedError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedErr = err
}

// SetConnecting scripts the Connecting flag for a session.
func (f *Fake) SetConnecting(session string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connect[session] = v
}

// SetTranscriptPath scripts LocateTranscript's return ("" → miss).
func (f *Fake) SetTranscriptPath(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transcriptPath = path
}

// SetChat scripts what ReadTranscript returns.
func (f *Fake) SetChat(c provider.Chat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chat = c
}

// SetReadError scripts a ReadTranscript failure (e.g. provider.ErrTranscriptGone).
func (f *Fake) SetReadError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readErr = err
}

// SetReplyError / SetInterruptError script the send-keys failures.
func (f *Fake) SetReplyError(err error)     { f.mu.Lock(); f.replyErr = err; f.mu.Unlock() }
func (f *Fake) SetInterruptError(err error) { f.mu.Lock(); f.interruptErr = err; f.mu.Unlock() }

// Replies returns the reply texts delivered, in order.
func (f *Fake) Replies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.replies...)
}

// Answers returns the dialog answers delivered, in order.
func (f *Fake) Answers() []provider.DialogAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.DialogAnswer(nil), f.answers...)
}

// Interrupts reports how many times Interrupt ran.
func (f *Fake) Interrupts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interrupts
}

// LocateCount reports how many times LocateTranscript ran.
func (f *Fake) LocateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locateCt
}

// SetLoginError / SetCodeError script the login flow.
func (f *Fake) SetLoginError(err error) { f.mu.Lock(); f.loginErr = err; f.mu.Unlock() }
func (f *Fake) SetCodeError(err error)  { f.mu.Lock(); f.codeErr = err; f.mu.Unlock() }

// Seeded returns the worktrees SeedWorkspace was called with.
func (f *Fake) Seeded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seeded...)
}

// SeededOpts returns the SeedOpts of each SeedWorkspace call, in order
// (the incogni-flag wiring assertion, D15 §9 measure 1).
func (f *Fake) SeededOpts() []provider.SeedOpts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.SeedOpts(nil), f.seedOpts...)
}

// CaptureCount reports how many times CaptureDeepLink ran.
func (f *Fake) CaptureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captureCt
}

// ForcedAuthChecks reports how many forced AuthStatus refreshes happened.
func (f *Fake) ForcedAuthChecks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authForced
}

// SubmittedCodes returns the login codes submitted, in order.
func (f *Fake) SubmittedCodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.codes...)
}

// ErrSeed is a convenience seeding error for rollback tests.
var ErrSeed = errors.New("seed failed")
