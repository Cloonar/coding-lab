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

// loginSession is this provider's login tmux session for the global `claude
// auth login` flow, derived from its provider id (lab-login-claude-code). It
// never counts against the instance cap and is excluded from
// ownership/stop-all — all exclusions recognize it via tmuxx.IsLoginSession,
// no equality coupling to this name (design §4d, issue #77).
var loginSession = tmuxx.LoginSessionName(ID)

// EventAuthChanged aliases the provider-generic SSE auth event (issue #51
// decision 7): the wire name is provider.auth.changed and the payload
// carries this provider's id — the old claude.auth.changed name is gone,
// SPA listener renamed in lockstep. Kept as an alias so package-internal
// references and tests read naturally.
const EventAuthChanged = provider.EventAuthChanged

// Pinned durations (port-spec claude-integration §5). Constructor-seeded
// struct fields so tests can shrink them without touching production
// values.
const (
	defaultCaptureTimeout = 3 * time.Second  // synchronous OAuth pane scrape
	defaultBridgeTimeout  = 30 * time.Second // background registry poll
	defaultLoginTimeout   = 20 * time.Second // post-code auth-status poll
	defaultLoginPoll      = 1 * time.Second  // gap between forced refreshes
	defaultAuthTTL        = 30 * time.Second // login-status cache freshness
	defaultLogoutTimeout  = 20 * time.Second // defensive cap on `claude auth logout` (non-interactive)

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

// spawnOptions is claude-code's provider-owned spawn-options catalog (issue #19
// / ADR-0021): the declared schema lab renders and validates the options bag
// against. ultracode is the first (and currently only) entry — the AFK-only
// keyword that steers a run into multi-agent workflows. It is a *prompt trigger
// keyword* (renamed from "workflow" in Claude Code v2.1.186), NOT a CLI flag,
// env var, or settings.json field — which is why it is applied by prompt
// injection (ultracodeDirective) rather than argv.
var spawnOptions = []provider.OptionSpec{
	{Key: "ultracode", Label: "Ultracode (multi-agent workflows)", Type: provider.OptionTypeBool, Default: "false"},
}

// ultracodeDirective is the provider-owned line prepended to a non-empty
// InitialPrompt when the ultracode option is on. It is OUR wording — freely
// tunable, NOT a pinned Claude coupling (compat §1) — so it is not on the
// fragile list and needs no version pin; the argv builder is merely re-snapshot
// tested. A manual spawn passes an empty prompt, so this never fires for manual
// runs (decision: ultracode is AFK-only; manual users type the keyword).
const ultracodeDirective = "Operate in ultracode mode: decompose substantial work into a multi-agent workflow."

// Options configures a Provider. Everything except Logger, Now, ClaudeBin,
// and ConfigPath is required.
type Options struct {
	// ClaudeBin is the claude binary (path or PATH-resolved name). Empty
	// defaults to the adapter-owned "claude" (PATH lookup): issue #78 moved
	// per-provider bin/config-path defaults out of internal/config and into
	// the owning adapter, so a caller with no configured entry for this
	// provider still gets a working Provider.
	ClaudeBin string
	// ConfigPath is claude's global config file (~/.claude.json), the
	// folder-trust seeding target. Empty defaults to LoginDir/.claude.json
	// (LoginDir is the service user's HOME) — the same adapter-owned-default
	// decision as ClaudeBin (issue #78). The old `-claude-config` flag /
	// LAB_CLAUDE_CONFIG env is becoming a deprecated alias for the generic
	// `-provider-config claude-code=path`; either way the resolved value
	// lands here unchanged.
	ConfigPath string
	// RegistryDir is claude's per-process session registry
	// ($HOME/.claude/sessions); injectable for tests.
	RegistryDir string
	// ProjectsDir is claude's transcript tree ($HOME/.claude/projects); the
	// chat surface reads <ProjectsDir>/<cwd-slug>/<sessionId>.jsonl.
	// Injectable for tests.
	ProjectsDir string
	// UserCommandsDir is claude's user-level custom slash-command directory
	// ($HOME/.claude/commands), scanned by Commands (issue #51 decision 5).
	// Defaults to the "commands" sibling of RegistryDir; injectable for tests.
	UserCommandsDir string
	// LoginDir is the working directory of the login session ($HOME —
	// login is global, one machine-level credential).
	LoginDir string
	// Runner drives tmux for the login session.
	Runner tmuxx.SessionRunner
	// Bus receives provider.auth.changed on login success.
	Bus *events.Bus
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now overrides the clock stamped into AuthStatus.CheckedAt (tests);
	// nil → time.Now. Poll deadlines always use the real clock.
	Now func() time.Time
}

// Provider is the claude-code AgentProvider. Construct with New.
type Provider struct {
	claudeBin       string
	configPath      string
	registryDir     string
	projectsDir     string
	userCommandsDir string
	loginDir        string
	runner          tmuxx.SessionRunner
	bus             *events.Bus
	log             *slog.Logger
	now             func() time.Time

	captureTimeout time.Duration
	bridgeTimeout  time.Duration
	loginTimeout   time.Duration
	loginPoll      time.Duration
	authTTL        time.Duration
	logoutTimeout  time.Duration // defensive cap on the non-interactive `claude auth logout`
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

	// intents is the post-resolve verification backstop's in-memory intent
	// registry (issue #51 decision 3, dialogintent.go): AnswerDialog records
	// what an answer was meant to be, ReadChat verifies the recorded
	// resolution against it. Zero value ready; bounded internally.
	intents intentRegistry
}

var _ provider.AgentProvider = (*Provider)(nil)
var _ provider.ConnectingReporter = (*Provider)(nil)
var _ provider.DeepLinker = (*Provider)(nil)

// New validates o and returns a Provider with the pinned production
// timeouts. ClaudeBin/ConfigPath are the two fields issue #78 made
// optional: main hands them through only when a caller has configured an
// entry for this provider (the generic `-provider-bin`/`-provider-config`
// maps, or their deprecated `-claude`/`-claude-config` aliases), and New
// fills the adapter-owned default when the entry is absent.
func New(o Options) (*Provider, error) {
	switch {
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
	// ClaudeBin defaults to a bare PATH-resolved name; ConfigPath defaults
	// to LoginDir/.claude.json, mirroring claude's own ~/.claude.json
	// convention. LoginDir is validated above, so it is always set here
	// (issue #78: adapters keep their own defaults when no config entry
	// exists).
	claudeBin := o.ClaudeBin
	if claudeBin == "" {
		claudeBin = "claude"
	}
	configPath := o.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(o.LoginDir, ".claude.json")
	}
	// ProjectsDir/UserCommandsDir default to siblings of RegistryDir
	// (~/.claude/sessions → ~/.claude/projects, ~/.claude/commands) so
	// existing callers keep working; main.go sets ProjectsDir explicitly.
	projectsDir := o.ProjectsDir
	if projectsDir == "" {
		projectsDir = filepath.Join(filepath.Dir(o.RegistryDir), "projects")
	}
	userCommandsDir := o.UserCommandsDir
	if userCommandsDir == "" {
		userCommandsDir = filepath.Join(filepath.Dir(o.RegistryDir), "commands")
	}
	return &Provider{
		claudeBin:       claudeBin,
		configPath:      configPath,
		registryDir:     o.RegistryDir,
		projectsDir:     projectsDir,
		userCommandsDir: userCommandsDir,
		loginDir:        o.LoginDir,
		runner:          o.Runner,
		bus:             o.Bus,
		log:             logger,
		now:             now,
		captureTimeout:  defaultCaptureTimeout,
		bridgeTimeout:   defaultBridgeTimeout,
		loginTimeout:    defaultLoginTimeout,
		loginPoll:       defaultLoginPoll,
		authTTL:         defaultAuthTTL,
		logoutTimeout:   defaultLogoutTimeout,
		keyDelay:        defaultDialogKeyDelay,
		capturing:       map[string]bool{},
	}, nil
}

// ID implements provider.AgentProvider.
func (p *Provider) ID() string { return ID }

// DisplayName implements provider.AgentProvider (issue #51 decision 9): the
// human name driving all UI copy for this provider.
func (p *Provider) DisplayName() string { return "Claude Code" }

// AuthFlow implements provider.AgentProvider (issue #51 decision 7): claude
// logs in via the paste-back OAuth code flow — LoginStart returns the
// authorize URL, the operator pastes the resulting code into LoginSubmitCode.
// No extra operator instructions are needed.
func (p *Provider) AuthFlow() provider.AuthFlow {
	return provider.AuthFlow{Kind: provider.AuthFlowOAuthCode}
}

// seedMeta is claude-code's workspace-seeding declaration (issue #51 decision
// 8), consumed by the ONE generic seeder (issue #51 Phase 2c:
// internal/seeder.SeedWorkspace takes it per call; the seeder's own claude
// vars are gone). Byte-identity of the seeded worktree is pinned from both
// sides: the seeder goldens assert "generic seeder + these values == the
// pre-#51 bytes" against their OWN inlined literals (no claudecode import),
// and claudecode/seedmeta_test.go pins THIS declaration to the same literals
// — a drift in either place fails a test. Changing any value here changes
// what lab seeds into every claude-code worktree.
var seedMeta = provider.SeedMeta{
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
		`co-authored-by:.*<[^>]*@anthropic\.com>`, // the stable discriminator is the email, not a "Claude" display name
		`generated with.*claude`,
		`claude-session:`,
	},
}

// SeedMeta implements provider.AgentProvider: claude-code's seeding shapes,
// returned as a copy with cloned slices so a caller can never mutate the
// declaration.
func (p *Provider) SeedMeta() provider.SeedMeta {
	m := seedMeta
	m.ExcludeEntries = slices.Clone(m.ExcludeEntries)
	m.SeededPathPatterns = slices.Clone(m.SeededPathPatterns)
	m.ScrubPatterns = slices.Clone(m.ScrubPatterns)
	return m
}

// Models returns a copy of the model catalog, in dropdown order.
func (p *Provider) Models() []provider.Option { return slices.Clone(models) }

// Efforts returns a copy of the effort catalog, in dropdown order.
func (p *Provider) Efforts() []provider.Option { return slices.Clone(efforts) }

// SpawnOptions returns a copy of the spawn-options catalog (issue #19), in
// render order.
func (p *Provider) SpawnOptions() []provider.OptionSpec { return slices.Clone(spawnOptions) }

// SpawnArgv builds the pinned instance spawn command:
//
//	{claude} --remote-control <session> --permission-mode auto [--model M] [--effort E] [prompt]
//
// Empty model/effort omit the flag (defaults resolve from settings before
// the call, so production always passes both). A non-empty InitialPrompt is
// appended as claude's trailing positional argument — the AFK seed prompt,
// pinned to the v0 mechanism (v0 afk.go: append(baseStartArgv(),
// afkSeedPrompt(n))) so it is present before the process starts and cannot be
// lost to the cold-start TUI race; manual spawns pass "" and get no trailing
// argument. spec.Options is applied here, provider-owned (ultracode prepends a
// directive to a non-empty prompt).
func (p *Provider) SpawnArgv(spec provider.SpawnSpec) []string {
	return SpawnArgv(p.claudeBin, spec)
}

// SpawnArgv is the pure spawn-argv builder behind Provider.SpawnArgv,
// exported for the compat snapshot test.
func SpawnArgv(claudeBin string, spec provider.SpawnSpec) []string {
	argv := []string{claudeBin, "--remote-control", spec.SessionName, "--permission-mode", "auto"}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "--effort", spec.Effort)
	}
	// Trailing positional AFTER the flags (claude CLI: `claude [options]
	// [prompt]`). The provider applies its own spawn options to the prompt here
	// — ultracode prepends a directive — so a bad option can never reach argv
	// as a flag. Omitted when empty so a manual spawn carries no stray arg (and
	// a prompt-scoped option is a natural no-op for manual runs).
	if prompt := applySpawnOptions(spec.InitialPrompt, spec.Options); prompt != "" {
		argv = append(argv, prompt)
	}
	return argv
}

// applySpawnOptions applies claude-code's spawn options to the seed prompt. It
// is a no-op on an empty prompt (manual spawns), so ultracode is AFK-only by
// construction. ultracode prepends a directive line ahead of the seed prompt.
func applySpawnOptions(prompt string, options map[string]string) string {
	if prompt == "" {
		return ""
	}
	if options["ultracode"] == "true" {
		return ultracodeDirective + "\n\n" + prompt
	}
	return prompt
}

func (p *Provider) publishAuthChanged() {
	p.bus.Publish(events.Event{
		Type:    provider.EventAuthChanged,
		Payload: provider.AuthChangedPayload{Type: provider.EventAuthChanged, Provider: ID},
	})
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
