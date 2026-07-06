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

	deepLink  string // returned by CaptureDeepLink (a real hit); "" → GenericDeepLink miss
	seedErr   error
	seeded    []string // worktrees passed to SeedWorkspace, in order
	connect   map[string]bool
	oauthURL  string
	loginErr  error
	codeErr   error
	codes     []string // codes submitted via LoginSubmitCode
	captureCt int      // CaptureDeepLink call count
}

var (
	_ provider.AgentProvider      = (*Fake)(nil)
	_ provider.ConnectingReporter = (*Fake)(nil)
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
		loggedIn: true,
		email:    "op@example.invalid",
		method:   "claude.ai",
		now:      time.Now,
		deepLink: "https://claude.ai/code/session_fake",
		connect:  map[string]bool{},
		oauthURL: "https://claude.com/cai/oauth/authorize?code=true",
	}
}

func (f *Fake) ID() string { return f.id }

func (f *Fake) Models() []provider.Option  { return f.models }
func (f *Fake) Efforts() []provider.Option { return f.efforts }

// SpawnArgv mirrors the pinned claude argv shape so tests can assert the
// recorded tmux argv.
func (f *Fake) SpawnArgv(session, model, effort string) []string {
	argv := []string{"claude", "--remote-control", session, "--permission-mode", "auto"}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "--effort", effort)
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

// CaptureDeepLink returns the scripted real link, or GenericDeepLink when the
// deep link was cleared (a miss). It marks the session connecting for the
// duration of one call.
func (f *Fake) CaptureDeepLink(_ context.Context, session, _ string) (string, error) {
	f.mu.Lock()
	f.captureCt++
	link := f.deepLink
	f.mu.Unlock()
	if link == "" {
		return "https://claude.ai/code", nil // GenericDeepLink
	}
	return link, nil
}

func (f *Fake) SeedWorkspace(worktree string, _ provider.SeedOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seedErr != nil {
		return f.seedErr
	}
	f.seeded = append(f.seeded, worktree)
	return nil
}

func (f *Fake) Connecting(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connect[session]
}

// --- test controls -------------------------------------------------------

// SetLoggedIn scripts the auth state the forced pre-spawn refresh reads.
func (f *Fake) SetLoggedIn(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedIn = v
}

// SetDeepLink scripts CaptureDeepLink's real hit ("" → generic miss).
func (f *Fake) SetDeepLink(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deepLink = url
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

// SetLoginError / SetCodeError script the login flow.
func (f *Fake) SetLoginError(err error) { f.mu.Lock(); f.loginErr = err; f.mu.Unlock() }
func (f *Fake) SetCodeError(err error)  { f.mu.Lock(); f.codeErr = err; f.mu.Unlock() }

// Seeded returns the worktrees SeedWorkspace was called with.
func (f *Fake) Seeded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seeded...)
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
