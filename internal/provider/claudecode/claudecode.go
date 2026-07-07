// Package claudecode is the claude-code AgentProvider: spawn argv, the
// model/effort catalogs, the machine-level auth status + OAuth login flow,
// remote-control deep-link capture from claude's session registry, and
// workspace trust seeding. It is lab's entire coupling surface to the
// Claude Code CLI's machine-local state — every exact string here is on the
// brief's §11 fragile list and is pinned by internal/compat against the
// installed Claude Code version (2.1.198 at port time).
//
// Ported 1:1 from lab-v0 registry.go/auth.go/trust.go/sessions.go and the
// login/auth/capture parts of handlers.go (port-spec claude-integration).
package claudecode

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// ID is the provider identifier stored in repos.provider / runs.provider.
const ID = "claude-code"

// LoginSession is the fixed tmux session name for the global `claude auth
// login` flow. It never counts against the instance cap and is excluded
// from ownership/stop-all — all exclusions key on the one tmuxx symbol
// (design §4d).
const LoginSession = tmuxx.LoginSession

// EventAuthChanged is the SSE event published when the machine-level Claude
// login state changes (pinned SSE contract: `claude.auth.changed {}`).
const EventAuthChanged = "claude.auth.changed"

// Pinned durations (port-spec claude-integration §5). Constructor-seeded
// struct fields so tests can shrink them without touching production
// values.
const (
	defaultCaptureTimeout = 3 * time.Second  // synchronous OAuth pane scrape
	defaultBridgeTimeout  = 30 * time.Second // background registry poll
	defaultLoginTimeout   = 20 * time.Second // post-code auth-status poll
	defaultLoginPoll      = 1 * time.Second  // gap between forced refreshes
	defaultAuthTTL        = 30 * time.Second // login-status cache freshness

	// defaultDialogKeyDelay paces the dialog-answer keystroke recipe: a gap
	// between each send-keys op so the remote-control TUI processes a picker
	// navigation before the next key (compat §7). LIVE-VERIFIED on 2.1.198,
	// 2026-07-07: driving the picker back-to-back with no gap raced the
	// committing Enter ahead of the Down navigation and *intermittently*
	// selected the wrong option (index 0 instead of the intended row); 150ms+
	// was reliable across trials, 0ms was flaky. 250ms carries margin. This is
	// the "robustify the recipe" the embedded-chat brief called for — it never
	// ran against a real picker before this issue.
	defaultDialogKeyDelay = 250 * time.Millisecond

	// pollInterval is the cadence of both the registry poll and the pane
	// scrape (v0-pinned 200ms).
	pollInterval = 200 * time.Millisecond
)

// models is the provider-owned model catalog (pinned: v0 spawn.go). Family
// aliases, not pinned ids, so the list tracks the latest of each family;
// [1m] only where the 1M-context variant is included on the plan.
var models = []provider.Option{
	{Value: "opus[1m]", Label: "Opus (1M)"},
	{Value: "sonnet", Label: "Sonnet"},
	{Value: "fable", Label: "Fable"},
	{Value: "haiku", Label: "Haiku"},
}

// efforts is the provider-owned effort catalog (pinned). Claude Code clamps
// unsupported model+effort combinations itself, so every level is offered
// for every model; labels are the raw values, exactly what v0 rendered.
var efforts = []provider.Option{
	{Value: "low", Label: "low"},
	{Value: "medium", Label: "medium"},
	{Value: "high", Label: "high"},
	{Value: "xhigh", Label: "xhigh"},
	{Value: "max", Label: "max"},
}

// Options configures a Provider. Everything except Logger and Now is
// required.
type Options struct {
	// ClaudeBin is the claude binary (path or PATH-resolved name).
	ClaudeBin string
	// ConfigPath is claude's global config file (~/.claude.json), the
	// folder-trust seeding target (config --claude-config).
	ConfigPath string
	// RegistryDir is claude's per-process session registry
	// ($HOME/.claude/sessions); injectable for tests.
	RegistryDir string
	// ProjectsDir is claude's transcript tree ($HOME/.claude/projects); the
	// chat surface reads <ProjectsDir>/<cwd-slug>/<sessionId>.jsonl.
	// Injectable for tests.
	ProjectsDir string
	// LoginDir is the working directory of the login session ($HOME —
	// login is global, one machine-level credential).
	LoginDir string
	// Runner drives tmux for the login session.
	Runner tmuxx.SessionRunner
	// Bus receives claude.auth.changed on login success.
	Bus *events.Bus
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now overrides the clock stamped into AuthStatus.CheckedAt (tests);
	// nil → time.Now. Poll deadlines always use the real clock.
	Now func() time.Time
}

// Provider is the claude-code AgentProvider. Construct with New.
type Provider struct {
	claudeBin   string
	configPath  string
	registryDir string
	projectsDir string
	loginDir    string
	runner      tmuxx.SessionRunner
	bus         *events.Bus
	log         *slog.Logger
	now         func() time.Time

	captureTimeout time.Duration
	bridgeTimeout  time.Duration
	loginTimeout   time.Duration
	loginPoll      time.Duration
	authTTL        time.Duration
	keyDelay       time.Duration // inter-op gap in the dialog-answer recipe (compat §7)

	// authMu guards the lazy login-status cache. The ~0.75s status command
	// runs while holding it; that brief serialisation is accepted for a
	// single-user tool (v0 note preserved).
	authMu      sync.Mutex
	authCache   provider.AuthStatus
	authChecked time.Time

	// loginMu guards loginURL — the scraped OAuth URL of the current login
	// attempt. In-memory only, never persisted; re-scraped from the live
	// pane on demand after a restart.
	loginMu  sync.Mutex
	loginURL string

	// captureMu guards capturing, the set of session names with an
	// in-flight deep-link capture — the "connecting…" render state.
	captureMu sync.Mutex
	capturing map[string]bool
}

var _ provider.AgentProvider = (*Provider)(nil)
var _ provider.ConnectingReporter = (*Provider)(nil)
var _ provider.DeepLinker = (*Provider)(nil)

// New validates o and returns a Provider with the pinned production
// timeouts.
func New(o Options) (*Provider, error) {
	switch {
	case o.ClaudeBin == "":
		return nil, errors.New("claudecode: Options.ClaudeBin is required")
	case o.ConfigPath == "":
		return nil, errors.New("claudecode: Options.ConfigPath is required")
	case o.RegistryDir == "":
		return nil, errors.New("claudecode: Options.RegistryDir is required")
	case o.LoginDir == "":
		return nil, errors.New("claudecode: Options.LoginDir is required")
	case o.Runner == nil:
		return nil, errors.New("claudecode: Options.Runner is required")
	case o.Bus == nil:
		return nil, errors.New("claudecode: Options.Bus is required")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	// ProjectsDir defaults to the sibling of RegistryDir (~/.claude/sessions →
	// ~/.claude/projects) so existing callers keep working; main.go sets it
	// explicitly.
	projectsDir := o.ProjectsDir
	if projectsDir == "" {
		projectsDir = filepath.Join(filepath.Dir(o.RegistryDir), "projects")
	}
	return &Provider{
		claudeBin:      o.ClaudeBin,
		configPath:     o.ConfigPath,
		registryDir:    o.RegistryDir,
		projectsDir:    projectsDir,
		loginDir:       o.LoginDir,
		runner:         o.Runner,
		bus:            o.Bus,
		log:            logger,
		now:            now,
		captureTimeout: defaultCaptureTimeout,
		bridgeTimeout:  defaultBridgeTimeout,
		loginTimeout:   defaultLoginTimeout,
		loginPoll:      defaultLoginPoll,
		authTTL:        defaultAuthTTL,
		keyDelay:       defaultDialogKeyDelay,
		capturing:      map[string]bool{},
	}, nil
}

// ID implements provider.AgentProvider.
func (p *Provider) ID() string { return ID }

// Models returns a copy of the model catalog, in dropdown order.
func (p *Provider) Models() []provider.Option { return slices.Clone(models) }

// Efforts returns a copy of the effort catalog, in dropdown order.
func (p *Provider) Efforts() []provider.Option { return slices.Clone(efforts) }

// SpawnArgv builds the pinned instance spawn command:
//
//	{claude} --remote-control <session> --permission-mode auto [--model M] [--effort E] [prompt]
//
// Empty model/effort omit the flag (defaults resolve from settings before
// the call, so production always passes both). A non-empty initialPrompt is
// appended as claude's trailing positional argument — the AFK seed prompt,
// pinned to the v0 mechanism (v0 afk.go: append(baseStartArgv(),
// afkSeedPrompt(n))) so it is present before the process starts and cannot be
// lost to the cold-start TUI race; manual spawns pass "" and get no trailing
// argument.
func (p *Provider) SpawnArgv(sessionName, model, effort, initialPrompt string) []string {
	return SpawnArgv(p.claudeBin, sessionName, model, effort, initialPrompt)
}

// SpawnArgv is the pure spawn-argv builder behind Provider.SpawnArgv,
// exported for the compat snapshot test.
func SpawnArgv(claudeBin, sessionName, model, effort, initialPrompt string) []string {
	argv := []string{claudeBin, "--remote-control", sessionName, "--permission-mode", "auto"}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "--effort", effort)
	}
	// Trailing positional AFTER the flags (claude CLI: `claude [options]
	// [prompt]`). Omitted when empty so a manual spawn carries no stray arg.
	if initialPrompt != "" {
		argv = append(argv, initialPrompt)
	}
	return argv
}

type authChangedPayload struct {
	Type string `json:"type"`
}

func (p *Provider) publishAuthChanged() {
	p.bus.Publish(events.Event{Type: EventAuthChanged, Payload: authChangedPayload{Type: EventAuthChanged}})
}

// sleepOrDone waits d or until ctx is cancelled; reports false on
// cancellation. The poll loops check work BEFORE the deadline so at least
// one pass always happens, even with timeout 0 (v0-pinned loop shape).
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
