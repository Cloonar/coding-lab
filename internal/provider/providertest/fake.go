// Package providertest provides a controllable fake AgentProvider for the
// instance/reconcile/httpapi test suites (design §4d: fakes live next to the
// interface). It records SpawnArgv/SeedWorkspace/CaptureDeepLink calls and lets
// tests script auth state, the captured deep link, seeding failures, and the
// login flow.
package providertest

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Fake is a scriptable AgentProvider. Its ID defaults to "claude-code" so it
// resolves for a repo's default provider. Safe for concurrent use.
type Fake struct {
	mu sync.Mutex

	id          string
	displayName string
	models      []provider.ModelOption
	efforts     []provider.Option
	options     []provider.OptionSpec
	authFlow    provider.AuthFlow
	seedMeta    provider.SeedMeta

	// Slash-command catalog (issue #51 decision 5). commands/commandsErr
	// script Commands; commandsCalls records the worktrees it was asked
	// about, in order.
	commands      []provider.CommandSpec
	commandsErr   error
	commandsCalls []string

	spawnSpecs []provider.SpawnSpec // every SpawnArgv spec, in order (threading assertions)

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
	logoutErr    error    // scripted Logout failure (loggedIn stays true)
	logouts      int      // Logout call count
	captureCt    int      // CaptureDeepLink call count

	// Chat surface (issue #7; adapter-owned composition since issue #92).
	// transcriptPath is what LocateTranscript returns (""→miss); chat is the
	// transcript-derived base ReadChat composes over; readErr forces a
	// ReadChat error (e.g. provider.ErrTranscriptGone) for non-empty paths.
	// readCalls records every ReadChat call's arguments. Replies, answers,
	// and interrupts are recorded for assertions.
	transcriptPath string
	chat           provider.Chat
	readErr        error
	readCalls      []ReadCall
	locateCt       int
	replies        []string
	answers        []provider.DialogAnswer
	interrupts     int
	replyErr       error
	interruptErr   error

	// LiveSignals lifecycle capability (issue #17 / ADR-0020, narrowed in
	// issue #92). hookArgs/hookSettings script Setup; spoolSig scripts the
	// change digest; pendingDialog/blocked script the live signals ReadChat
	// composes (the fake's adapter-private state, like claudecode's spool);
	// hookCalls records Setup runIDs, setupOpts its received opts (issue
	// #124), and sweeps counts SweepSpools.
	hookSettings  []byte
	hookArgs      []string
	pendingDialog *provider.Dialog
	blocked       string
	blockedOK     bool
	spoolSig      string
	hookCalls     []string
	setupOpts     []provider.SetupOpts
	sweeps        int
	sweepErr      error
}

// ReadCall is one recorded ReadChat invocation — the assertion surface for
// what core passes down the seam (issue #92: runtime dir only for active
// runs, the resolved transcript path, the run id keying the spool).
type ReadCall struct {
	RunID          string
	RuntimeDir     string
	TranscriptPath string
}

var (
	_ provider.AgentProvider      = (*Fake)(nil)
	_ provider.ConnectingReporter = (*Fake)(nil)
	_ provider.DeepLinker         = (*Fake)(nil)
	_ provider.LiveSignals        = (*Fake)(nil)
)

// New returns a logged-in fake with the claude-code id, display name,
// catalogs, auth flow descriptor (oauth-code), and seed metadata (claude's
// declared values, issue #51 decision 8).
func New() *Fake {
	return &Fake{
		id:          "claude-code",
		displayName: "Claude Code",
		authFlow:    provider.AuthFlow{Kind: provider.AuthFlowOAuthCode},
		seedMeta: provider.SeedMeta{
			ContextFileName:      "CLAUDE.local.md",
			SkillsDir:            ".claude/skills",
			NativeSkillDiscovery: true,
			ExcludeEntries:       []string{".claude/", "CLAUDE.local.md"},
			SeededPathPatterns: []string{
				`^\.claude/skills/`,
				`^\.claude/settings\.local\.json$`,
				`^CLAUDE\.local\.md$`,
			},
			ScrubPatterns: []string{
				`co-authored-by:[[:space:]]*claude`,
				`co-authored-by:.*<[^>]*@anthropic\.com>`,
				`generated with.*claude`,
				`claude-session:`,
			},
		},
		// Claude-shaped enriched catalog (issue #156): every model carries
		// the full shared efforts list and no reported default, mirroring
		// claudecode's clamp-itself posture.
		models: uniformModelOptions(
			[]provider.Option{
				{Value: "opus[1m]", Label: "Opus (1M)"},
				{Value: "sonnet", Label: "Sonnet"},
				{Value: "fable", Label: "Fable"},
				{Value: "haiku", Label: "Haiku"},
			},
			[]provider.Option{
				{Value: "low", Label: "low"}, {Value: "medium", Label: "medium"},
				{Value: "high", Label: "high"}, {Value: "xhigh", Label: "xhigh"}, {Value: "max", Label: "max"},
			},
		),
		efforts: []provider.Option{
			{Value: "low", Label: "low"}, {Value: "medium", Label: "medium"},
			{Value: "high", Label: "high"}, {Value: "xhigh", Label: "xhigh"}, {Value: "max", Label: "max"},
		},
		options: []provider.OptionSpec{
			{Key: "ultracode", Label: "Ultracode (multi-agent workflows)", Type: provider.OptionTypeBool, Default: "false"},
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

// DisplayName returns the scripted human name (issue #51 decision 9;
// default "Claude Code").
func (f *Fake) DisplayName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.displayName
}

// AuthFlow returns the scripted auth flow descriptor (issue #51 decision 7;
// default oauth-code).
func (f *Fake) AuthFlow() provider.AuthFlow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authFlow
}

// SeedMeta returns the scripted seeding metadata (issue #51 decision 8;
// default = claude-code's declared values).
func (f *Fake) SeedMeta() provider.SeedMeta {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seedMeta
}

// Commands returns the scripted slash-command catalog (or error), recording
// the worktree it was asked about (issue #51 decision 5).
func (f *Fake) Commands(_ context.Context, worktree string) ([]provider.CommandSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commandsCalls = append(f.commandsCalls, worktree)
	if f.commandsErr != nil {
		return nil, f.commandsErr
	}
	return append([]provider.CommandSpec(nil), f.commands...), nil
}

func (f *Fake) Models() []provider.ModelOption { return f.models }
func (f *Fake) Efforts() []provider.Option     { return f.efforts }

// SetID rebrands the fake under another provider id (issue #66: multi-provider
// registries need a second, differently-named fake). Call before the fake is
// registered — a registry key never changes after NewRegistry.
func (f *Fake) SetID(id string) {
	f.mu.Lock()
	f.id = id
	f.mu.Unlock()
}

// SetCatalogs replaces the model/effort catalogs (issue #66: a foreign
// provider whose catalog does not carry the claude-shaped defaults; an empty
// — non-nil or nil — efforts slice models a provider without that knob).
// UNIFORM per issue #156: every given model gets the given efforts list and
// no reported default, so pre-#156 call sites keep their semantics — use
// SetModels for per-model effort lists. Call before the fake is handed to
// concurrent code; Models/Efforts are read without locking, mirroring their
// construction-time contract.
func (f *Fake) SetCatalogs(models, efforts []provider.Option) {
	f.models = uniformModelOptions(models, efforts)
	f.efforts = efforts
}

// SetModels replaces the enriched model catalog verbatim (issue #156: a
// provider whose models carry differing effort lists / reported defaults).
// The union efforts catalog is left alone — pair with SetCatalogs when the
// union must change too.
func (f *Fake) SetModels(models []provider.ModelOption) {
	f.models = models
}

// uniformModelOptions builds the claude-shaped enrichment: each model carries
// the SAME efforts list and no reported default (issue #156). nil models stay
// nil so SetCatalogs(nil, …) still means "no model catalog".
func uniformModelOptions(models, efforts []provider.Option) []provider.ModelOption {
	if models == nil {
		return nil
	}
	out := make([]provider.ModelOption, len(models))
	for i, m := range models {
		out[i] = provider.ModelOption{Option: m, Efforts: efforts}
	}
	return out
}

// SpawnOptions returns the declared spawn-options schema (issue #19); the New
// fake declares claude-code's ultracode bool so resolver/UI tests have a
// schema to validate/render against.
func (f *Fake) SpawnOptions() []provider.OptionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.OptionSpec(nil), f.options...)
}

// SpawnArgv mirrors the pinned claude argv shape so tests can assert the
// recorded tmux argv, including the seed prompt carried as the trailing
// positional (non-empty InitialPrompt) — manual spawns pass "" and get none.
// It records the whole spec so tests can assert the resolved Options bag was
// threaded through (issue #19); it does NOT apply ultracode itself — that is a
// claude-code coupling exercised by the compat snapshot, not the fake.
func (f *Fake) SpawnArgv(spec provider.SpawnSpec) []string {
	f.mu.Lock()
	f.spawnSpecs = append(f.spawnSpecs, spec)
	f.mu.Unlock()
	argv := []string{"claude", "--remote-control", spec.SessionName, "--permission-mode", "auto"}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "--effort", spec.Effort)
	}
	if spec.InitialPrompt != "" {
		argv = append(argv, spec.InitialPrompt)
	}
	return argv
}

// SpawnSpecs returns the SpawnSpec of each SpawnArgv call, in order — the
// options-threading assertion (resolved Options reached the provider).
func (f *Fake) SpawnSpecs() []provider.SpawnSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.SpawnSpec(nil), f.spawnSpecs...)
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

// Logout records the call and, unless a failure is scripted, flips the fake to
// logged-out — the observable transition the HTTP handler reads back (status
// cache now logged-out) and announces via provider.auth.changed.
func (f *Fake) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	if f.logoutErr != nil {
		return f.logoutErr
	}
	f.loggedIn = false
	return nil
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

// ReadChat returns the scripted transcript base composed with the scripted
// live signals, mirroring the adapter-owned precedence a real adapter applies
// (issue #92): a scripted pending dialog → StateQuestion + Chat.PendingDialog;
// else a scripted question state stands on its own (the dormant transcript
// fallback); else a scripted blocked state overrides. transcriptPath "" is
// the pre-transcript read (an idle empty base, never an error, per the seam
// contract); runtimeDir "" turns the signals off (transcript-only). The
// scripted readErr (e.g. provider.ErrTranscriptGone) fires only for a
// non-empty path, like a real adapter's vanished-file open.
func (f *Fake) ReadChat(runID, runtimeDir, transcriptPath string) (provider.Chat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls = append(f.readCalls, ReadCall{RunID: runID, RuntimeDir: runtimeDir, TranscriptPath: transcriptPath})
	var chat provider.Chat
	switch {
	case transcriptPath == "":
		chat = provider.Chat{State: provider.StateIdle}
	case f.readErr != nil:
		return provider.Chat{}, f.readErr
	default:
		chat = f.chat
	}
	if runtimeDir == "" {
		return chat, nil
	}
	if f.pendingDialog != nil {
		d := *f.pendingDialog
		chat.State = provider.StateQuestion
		chat.PendingDialog = &d
		return chat, nil
	}
	if chat.State == provider.StateQuestion {
		return chat, nil
	}
	if f.blockedOK {
		chat.State = f.blocked
	}
	return chat, nil
}

// ReadCalls returns the arguments of every ReadChat call, in order.
func (f *Fake) ReadCalls() []ReadCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ReadCall(nil), f.readCalls...)
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

// --- LiveSignals lifecycle capability (ADR-0020, narrowed in #92) ----------

// Setup records the runID and opts and returns the scripted settings/args (a
// default --settings <dir>/settings.<runID>.json when unset).
func (f *Fake) Setup(runID, dir string, opts provider.SetupOpts) ([]byte, string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hookCalls = append(f.hookCalls, runID)
	f.setupOpts = append(f.setupOpts, opts)
	path := filepath.Join(dir, "settings."+runID+".json")
	settings := f.hookSettings
	if settings == nil {
		settings = []byte(`{"hooks":{}}`)
	}
	args := f.hookArgs
	if args == nil {
		args = []string{"--settings", path}
	}
	return settings, path, args
}

// SpoolSig returns the scripted spool signature.
func (f *Fake) SpoolSig(_, _ string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spoolSig
}

// SweepSpools counts the call (and returns the scripted error).
func (f *Fake) SweepSpools(_ string, _ func(runID string) bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++
	return f.sweepErr
}

// SetPendingDialog scripts the live pending dialog ReadChat composes when a
// runtime dir is passed (nil → none) — the fake's stand-in for claudecode's
// PreToolUse spool.
func (f *Fake) SetPendingDialog(d *provider.Dialog) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingDialog = d
}

// SetBlockedState scripts the blocked state ReadChat composes when a runtime
// dir is passed and no dialog wins — the stand-in for claudecode's
// Notification marker.
func (f *Fake) SetBlockedState(state string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked, f.blockedOK = state, ok
}

// SetSpoolSig scripts the spool change-detector signature.
func (f *Fake) SetSpoolSig(sig string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spoolSig = sig
}

// SetHookResult scripts Setup's returned bytes and args ("" args → the
// default --settings path).
func (f *Fake) SetHookResult(settings []byte, args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hookSettings, f.hookArgs = settings, args
}

// HookCalls returns the runIDs passed to Setup, in order.
func (f *Fake) HookCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hookCalls...)
}

// SetupOpts returns the opts of every Setup call, in order — the
// dialog-timeout threading assertion (issue #124: run-kind policy reached the
// adapter).
func (f *Fake) SetupOpts() []provider.SetupOpts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.SetupOpts(nil), f.setupOpts...)
}

// Sweeps reports how many times SweepSpools ran.
func (f *Fake) Sweeps() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweeps
}

// --- test controls -------------------------------------------------------

// SetLoggedIn scripts the auth state the forced pre-spawn refresh reads.
func (f *Fake) SetLoggedIn(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedIn = v
}

// SetDisplayName scripts the human name DisplayName returns.
func (f *Fake) SetDisplayName(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.displayName = name
}

// SetAuthFlow scripts the auth flow descriptor AuthFlow returns.
func (f *Fake) SetAuthFlow(af provider.AuthFlow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authFlow = af
}

// SetSeedMeta scripts the seeding metadata SeedMeta returns.
func (f *Fake) SetSeedMeta(m provider.SeedMeta) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedMeta = m
}

// SetCommands scripts the slash-command catalog (and error) Commands returns.
func (f *Fake) SetCommands(specs []provider.CommandSpec, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands, f.commandsErr = specs, err
}

// CommandsCalls returns the worktrees Commands was asked about, in order.
func (f *Fake) CommandsCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commandsCalls...)
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

// SetChat scripts the transcript-derived base ReadChat composes over.
func (f *Fake) SetChat(c provider.Chat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chat = c
}

// SetReadError scripts a ReadChat failure for non-empty transcript paths
// (e.g. provider.ErrTranscriptGone).
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

// SetLogoutError scripts a Logout failure (the fake stays logged-in).
func (f *Fake) SetLogoutError(err error) { f.mu.Lock(); f.logoutErr = err; f.mu.Unlock() }

// Logouts reports how many times Logout ran.
func (f *Fake) Logouts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logouts
}

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
