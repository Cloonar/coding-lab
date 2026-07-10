// Package codex is the Codex CLI AgentProvider: spawn argv, the model/effort
// catalogs, the machine-level auth status + device-code login flow, workspace
// trust seeding, and the rollout-transcript chat surface. It is lab's entire
// coupling surface to the Codex CLI's machine-local state — every exact
// string here is a fragile Codex coupling, live-verified against the
// installed codex-cli 0.133.0 (issue #87 spikes, 2026-07-10).
//
// Structure mirrors internal/provider/claudecode file for file; deltas are
// codex-shaped: device-code auth (no pasted code), a global config.toml trust
// append instead of a JSON grant, rollout JSONL under a date tree instead of
// a cwd-slug projects dir, and no web surface (no DeepLinker, no LiveSignals
// — honest degradation, instance rows fall back to tmux attach).
package codex

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// ID is the provider identifier stored in repos.provider / runs.provider.
const ID = "codex"

// loginSession is this provider's login tmux session for the global
// `codex login --device-auth` flow, derived from its provider id
// (lab-login-codex). It never counts against the instance cap and is
// excluded from ownership/stop-all — all exclusions recognize it via
// tmuxx.IsLoginSession, no equality coupling to this name (issue #77).
var loginSession = tmuxx.LoginSessionName(ID)

// Pinned durations. Constructor-seeded struct fields so tests can shrink
// them without touching production values.
const (
	defaultCaptureTimeout = 3 * time.Second  // synchronous device-code pane scrape
	defaultAuthTTL        = 30 * time.Second // login-status cache freshness
	defaultLoginPoll      = 3 * time.Second  // completion poller's forced-refresh gap
	defaultLoginWindow    = 15 * time.Minute // device codes expire in 15 minutes (live 0.133.0)
	defaultLogoutTimeout  = 20 * time.Second // defensive cap on `codex logout` (non-interactive)

	// defaultKeyDelay paces the reply recipe: the settling gap between the
	// bracketed paste and its committing Enter (mirrors claudecode's
	// live-verified pacing). LIVE HAZARD on 0.133.0: slash-command text and
	// its Enter MUST remain separate paced bursts — delivered in one burst
	// the command merges with subsequent input.
	defaultKeyDelay = 250 * time.Millisecond

	// pollInterval is the pane-scrape cadence (v0-pinned 200ms, shared with
	// claudecode).
	pollInterval = 200 * time.Millisecond
)

// models is the provider-owned model catalog, pinned from `codex debug
// models` on 0.133.0. The internal `codex-auto-review` model is deliberately
// filtered out — it is not an operator-selectable spawn model. First entry
// is the spawn default.
var models = []provider.Option{
	{Value: "gpt-5.5", Label: "GPT-5.5"},
	{Value: "gpt-5.4-mini", Label: "GPT-5.4-Mini"},
}

// efforts is the provider-owned reasoning-effort catalog (pinned 0.133.0).
var efforts = []provider.Option{
	{Value: "low", Label: "Low"},
	{Value: "medium", Label: "Medium"},
	{Value: "high", Label: "High"},
	{Value: "xhigh", Label: "Extra high"},
}

// defaultEffort is injected when a spawn spec carries no effort: `codex exec`
// defaults the reasoning effort to NONE, not medium, so spawns must always
// pass it explicitly (pinned spike finding, 0.133.0).
const defaultEffort = "medium"

// Options configures a Provider. LoginDir, Runner, and Bus are required;
// the path fields default from the adapter-owned codex home resolution
// (issue #78: per-provider defaults live in the owning adapter).
type Options struct {
	// CodexBin is the codex binary (path or PATH-resolved name). Empty
	// defaults to the adapter-owned "codex" (PATH lookup).
	CodexBin string
	// ConfigPath is codex's global config file (<codexHome>/config.toml),
	// the directory-trust seeding target. Empty defaults from codexHome.
	ConfigPath string
	// SessionsDir is codex's rollout-transcript tree
	// (<codexHome>/sessions/YYYY/MM/DD/rollout-*.jsonl), the chat surface's
	// read root. Empty defaults from codexHome.
	SessionsDir string
	// AgentsFile is codex's GLOBAL AGENTS.md (<codexHome>/AGENTS.md) — the
	// one instructions file codex always concatenates, where SeedWorkspace
	// maintains the marker-guarded AGENTS.local.md bridge. Empty defaults
	// from codexHome.
	AgentsFile string
	// LoginDir is the working directory of the login session ($HOME —
	// login is global, one machine-level credential). Also the base of the
	// default codex home (<LoginDir>/.codex) when $CODEX_HOME is unset.
	LoginDir string
	// Runner drives tmux for the login session and the reply/interrupt
	// recipes.
	Runner tmuxx.SessionRunner
	// Bus receives provider.auth.changed on login/logout transitions.
	Bus *events.Bus
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now overrides the clock stamped into AuthStatus.CheckedAt (tests);
	// nil → time.Now. Poll deadlines always use the real clock.
	Now func() time.Time
}

// Provider is the codex AgentProvider. Construct with New.
type Provider struct {
	codexBin    string
	configPath  string
	sessionsDir string
	agentsFile  string
	loginDir    string
	runner      tmuxx.SessionRunner
	bus         *events.Bus
	log         *slog.Logger
	now         func() time.Time

	captureTimeout time.Duration
	authTTL        time.Duration
	loginPoll      time.Duration // completion poller's forced-refresh gap
	loginWindow    time.Duration // device-code validity window (poller lifetime)
	logoutTimeout  time.Duration // defensive cap on the non-interactive `codex logout`
	keyDelay       time.Duration // paste→Enter settling gap in the reply recipe

	// authMu guards the lazy login-status cache (v0/claudecode discipline:
	// the status command runs while holding it; accepted for a single-user
	// tool).
	authMu      sync.Mutex
	authCache   provider.AuthStatus
	authChecked time.Time

	// loginMu guards the pending device-code login attempt: the scraped
	// verification URL + one-time user code (in-memory only, re-scraped
	// from the live pane after a restart) and the single-completion-poller
	// flag.
	loginMu    sync.Mutex
	loginURL   string
	loginCode  string
	pollerLive bool
}

var _ provider.AgentProvider = (*Provider)(nil)
var _ provider.LoginCodeReporter = (*Provider)(nil)

// New validates o and returns a Provider with the pinned production
// timeouts. The path fields (CodexBin, ConfigPath, SessionsDir, AgentsFile)
// are optional: an explicit value always wins, an empty one falls back to
// the adapter-owned default under codex's own home resolution ($CODEX_HOME,
// else <LoginDir>/.codex). The environment is consulted ONLY when a default
// is actually needed — hermetic tests and the conformance suite pass every
// path explicitly and never touch env or the real HOME.
func New(o Options) (*Provider, error) {
	switch {
	case o.LoginDir == "":
		return nil, errors.New("codex: Options.LoginDir is required")
	case o.Runner == nil:
		return nil, errors.New("codex: Options.Runner is required")
	case o.Bus == nil:
		return nil, errors.New("codex: Options.Bus is required")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	codexBin := o.CodexBin
	if codexBin == "" {
		codexBin = "codex"
	}
	configPath := o.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(codexHomeDir(o.LoginDir), "config.toml")
	}
	sessionsDir := o.SessionsDir
	if sessionsDir == "" {
		sessionsDir = filepath.Join(codexHomeDir(o.LoginDir), "sessions")
	}
	agentsFile := o.AgentsFile
	if agentsFile == "" {
		agentsFile = filepath.Join(codexHomeDir(o.LoginDir), "AGENTS.md")
	}
	return &Provider{
		codexBin:       codexBin,
		configPath:     configPath,
		sessionsDir:    sessionsDir,
		agentsFile:     agentsFile,
		loginDir:       o.LoginDir,
		runner:         o.Runner,
		bus:            o.Bus,
		log:            logger,
		now:            now,
		captureTimeout: defaultCaptureTimeout,
		authTTL:        defaultAuthTTL,
		loginPoll:      defaultLoginPoll,
		loginWindow:    defaultLoginWindow,
		logoutTimeout:  defaultLogoutTimeout,
		keyDelay:       defaultKeyDelay,
	}, nil
}

// codexHomeDir resolves codex's state home the way the codex binary does:
// $CODEX_HOME when set, else <loginDir>/.codex (~/.codex). Used for the
// path-field defaults in New and for the logout escalation's auth.json —
// both must target the exact files the codex binary itself reads/writes.
func codexHomeDir(loginDir string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(loginDir, ".codex")
}

// ID implements provider.AgentProvider.
func (p *Provider) ID() string { return ID }

// DisplayName implements provider.AgentProvider (issue #51 decision 9): the
// human name driving all UI copy for this provider.
func (p *Provider) DisplayName() string { return "Codex" }

// AuthFlow implements provider.AgentProvider (issue #51 decision 7): codex
// logs in via device-code authorization — LoginStart launches the flow and
// returns the verification URL, the one-time user code is surfaced through
// PendingLoginCode (provider.LoginCodeReporter), and the code is entered
// browser-side, never pasted back into lab. Completion is provider-driven:
// the adapter polls its own status and publishes provider.auth.changed.
func (p *Provider) AuthFlow() provider.AuthFlow {
	return provider.AuthFlow{
		Kind: provider.AuthFlowDeviceCode,
		Instructions: "Start the login here, then open the verification link in any browser, " +
			"sign in, and enter the one-time code shown. The login completes on the lab host " +
			"automatically — the status flips once the Codex CLI records it.",
	}
}

// seedMeta is codex's workspace-seeding declaration (issue #51 decision 8),
// consumed by the ONE generic seeder. AGENTS.local.md is the lab-owned
// context file (issue #79 / ADR-0035 hard rule — codex's native AGENTS.md is
// repo-trackable and must never be written by lab); SeedWorkspace's global
// AGENTS.md bridge is what makes codex actually read it. Skills are seeded
// into .codex/skills, but 0.133 discovers skills only from $CODEX_HOME/skills
// — no per-workspace path — so NativeSkillDiscovery=false and the seeder
// appends a skills index to the context file (ADR-0035). ScrubPatterns are
// DEFENSIVE: codex 0.133 writes no commit/PR attribution at the source (the
// codex_git_commit feature is off), so these are the adapter's conformance
// marker samples per ADR-0033 (BRE∩RE2 dialect), a backstop against a future
// version turning attribution on.
var seedMeta = provider.SeedMeta{
	ContextFileName:      "AGENTS.local.md",
	SkillsDir:            ".codex/skills",
	NativeSkillDiscovery: false, // 0.133 discovers skills only from $CODEX_HOME/skills — no per-workspace path
	ExcludeEntries:       []string{".codex/", "AGENTS.local.md"},
	SeededPathPatterns:   []string{`^\.codex/skills/`, `^AGENTS\.local\.md$`},
	ScrubPatterns: []string{
		`co-authored-by:[[:space:]]*codex`,
		`co-authored-by:.*<[^>]*@openai\.com>`,
		`generated with.*codex`,
	},
}

// SeedMeta implements provider.AgentProvider: codex's seeding shapes,
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

// SpawnOptions implements provider.AgentProvider: codex declares no generic
// spawn options (issue #19 / ADR-0021).
func (p *Provider) SpawnOptions() []provider.OptionSpec { return nil }

// SpawnArgv builds the pinned instance spawn command:
//
//	{codex} --ask-for-approval never --sandbox danger-full-access
//	        -c project_doc_fallback_filenames=["AGENTS.local.md"]
//	        [--model M] -c model_reasoning_effort=E [prompt]
//
// See the exported SpawnArgv for the per-flag pins.
func (p *Provider) SpawnArgv(spec provider.SpawnSpec) []string {
	return SpawnArgv(p.codexBin, spec)
}

// SpawnArgv is the pure spawn-argv builder behind Provider.SpawnArgv,
// exported for the compat snapshot test. Pins (all live 0.133.0):
//
//   - --ask-for-approval never + --sandbox danger-full-access is the
//     unattended posture: no approval prompt can ever block an AFK run.
//   - project_doc_fallback_filenames makes codex read the lab-owned
//     AGENTS.local.md; each -c value is a SINGLE argv element, no shell
//     quoting (tmux receives argv verbatim).
//   - Effort is ALWAYS emitted: `codex exec` defaults effort to none, so an
//     empty spec.Effort injects defaultEffort instead of omitting the flag.
//   - NO `-c commit_attribution=""`: unknown config field on 0.133 (hard
//     error under --strict-config); attribution is absent at source anyway
//     (the codex_git_commit feature is off/removed in 0.133).
//   - spec.SessionName is unused — codex has no --remote-control equivalent.
//   - A non-empty InitialPrompt rides as the single trailing positional (the
//     AFK seed prompt, present before the process so it never races the
//     cold-start TUI); an empty prompt appends nothing.
func SpawnArgv(codexBin string, spec provider.SpawnSpec) []string {
	argv := []string{
		codexBin,
		"--ask-for-approval", "never",
		"--sandbox", "danger-full-access",
		"-c", `project_doc_fallback_filenames=["AGENTS.local.md"]`,
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	effort := spec.Effort
	if effort == "" {
		effort = defaultEffort
	}
	argv = append(argv, "-c", "model_reasoning_effort="+effort)
	if spec.InitialPrompt != "" {
		argv = append(argv, spec.InitialPrompt)
	}
	return argv
}

func (p *Provider) publishAuthChanged() {
	p.bus.Publish(events.Event{
		Type:    provider.EventAuthChanged,
		Payload: provider.AuthChangedPayload{Type: provider.EventAuthChanged, Provider: ID},
	})
}

// sleepOrDone waits d or until ctx is cancelled; reports false on
// cancellation. The poll loops check work BEFORE the deadline so at least
// one pass always happens, even with timeout 0 (claudecode's pinned loop
// shape).
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
