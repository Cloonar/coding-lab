// Package codex is the Codex CLI AgentProvider: spawn argv, the model/effort
// catalogs, the machine-level auth status + device-code login flow, workspace
// trust seeding, and the rollout-transcript chat surface. It is lab's entire
// coupling surface to the Codex CLI's machine-local state — every exact
// string here is a fragile Codex coupling, live-verified against the
// installed codex-cli 0.133.0 (issue #87 spikes, 2026-07-10). The model
// catalog is probed from `codex debug models` at construction, with the
// 0.133.0-pinned list as the fallback (probe schema live-verified on
// 0.144.1, issue #156).
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
	defaultProbeTimeout   = 10 * time.Second // boot-time `codex debug models` catalog probe (issue #156)

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

// fallbackModels is the compiled-in pinned FALLBACK model catalog (issue
// #156): served when the boot probe fails, and the zero-value Provider's
// catalog; pinned from `codex debug models` on 0.133.0. The internal
// `codex-auto-review` model is deliberately filtered out — it is not an
// operator-selectable spawn model (the probe expresses the same rule as the
// visibility=="list" filter). First entry is the spawn default. Each model
// carries its own supported_reasoning_levels (the full four-entry list on
// both) and its reported default_reasoning_level (medium on both, per the
// compat fixture) — codex does NOT clamp unsupported combos, so the
// enrichment is what lets the spawn path 400 them.
var fallbackModels = []provider.ModelOption{
	{Option: provider.Option{Value: "gpt-5.5", Label: "GPT-5.5"}, Efforts: fallbackEfforts, DefaultEffort: defaultEffort},
	{Option: provider.Option{Value: "gpt-5.4-mini", Label: "GPT-5.4-Mini"}, Efforts: fallbackEfforts, DefaultEffort: defaultEffort},
}

// fallbackEfforts is the compiled-in pinned FALLBACK reasoning-effort
// catalog (issue #156): served when the boot probe fails, and the zero-value
// Provider's catalog; pinned from `codex debug models` on 0.133.0. The
// labels follow effortLabel — the one adapter-owned label rule shared with
// the probe path.
var fallbackEfforts = []provider.Option{
	{Value: "low", Label: "Low"},
	{Value: "medium", Label: "Medium"},
	{Value: "high", Label: "High"},
	{Value: "xhigh", Label: "Extra high"},
}

// defaultEffort is injected when a spawn spec carries no effort: `codex exec`
// defaults the reasoning effort to NONE, not medium, so spawns must always
// pass it explicitly (pinned spike finding, 0.133.0). A last-resort argv
// default for direct SpawnArgv callers — lab's spawn resolution always
// passes an effort resolved against the (possibly probed) catalog.
const defaultEffort = "medium"

// Options configures a Provider. LoginDir, Runner, and Bus are required;
// the path fields default from the adapter-owned codex home resolution
// (issue #78: per-provider defaults live in the owning adapter).
type Options struct {
	// CodexBin is the codex binary (path or PATH-resolved name). Empty
	// defaults to the adapter-owned "codex" (PATH lookup).
	CodexBin string
	// ConfigPath is codex's MASTER global config file
	// (<codexHome>/config.toml). Master-store only: the per-run directory-trust
	// seed writes to <SeedOpts.Home>/.codex/config.toml instead (issue #202).
	// Empty defaults from codexHome.
	ConfigPath string
	// LoginDir is the working directory of the login session ($HOME —
	// login is global, one machine-level credential). Also the base of the
	// default codex home (<LoginDir>/.codex) when $CODEX_HOME is unset — the
	// MASTER store, used for logout and as the InjectCredentials source. All
	// INSTANCE-facing state (rollout tree, trust config, AGENTS.md bridge)
	// derives per-call from the run's private instance HOME on the seam instead
	// (issue #202 / ADR-0038).
	LoginDir string
	// Runner drives tmux for the login session and the reply/interrupt
	// recipes.
	Runner tmuxx.SessionRunner
	// CLI runs this provider's machine-level CLI invocations — the auth
	// status check, `codex logout`, the credential-refresh probe, and the
	// boot-time catalog/version probes (issue #206). nil defaults to
	// provider.HostCLI (direct host execution, the pre-seam behavior
	// byte-for-byte); a container-mode server injects a containerized runner
	// so the host needs no codex binary. This package never learns which it
	// got — podman stays entirely outside it.
	CLI provider.CLIRunner
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
	codexBin   string
	configPath string // MASTER <codexHome>/config.toml — master-store reference only
	loginDir   string
	runner     tmuxx.SessionRunner
	cli        provider.CLIRunner // machine-level CLI seam (issue #206) — never exec directly
	bus        *events.Bus
	log        *slog.Logger
	now        func() time.Time

	captureTimeout time.Duration
	authTTL        time.Duration
	loginPoll      time.Duration // completion poller's forced-refresh gap
	loginWindow    time.Duration // device-code validity window (poller lifetime)
	logoutTimeout  time.Duration // defensive cap on the non-interactive `codex logout`
	keyDelay       time.Duration // paste→Enter settling gap in the reply recipe
	probeTimeout   time.Duration // boot-time `codex debug models` catalog probe budget

	// catModels/catEfforts hold the catalog probed from `codex debug models`
	// at construction (issue #156). nil = the probe never succeeded — the
	// accessors serve the compiled-in fallback then, which is also why the
	// zero-value Provider (compat fixture test) keeps serving the pinned
	// catalog. Probed ONCE, cached for the process lifetime — no TTL, no
	// refresh: the binary is nix-pinned, a new binary implies a restart.
	catModels  []provider.ModelOption
	catEfforts []provider.Option

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
// timeouts. The MASTER path fields (CodexBin, ConfigPath) are optional: an
// explicit value always wins, an empty one falls back to the adapter-owned
// default under codex's own home resolution ($CODEX_HOME, else
// <LoginDir>/.codex). Instance-facing paths are NOT construction-time inputs —
// they derive per-call from the run's private instance HOME (issue #202).
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
	// CLI defaults to direct host execution (issue #206) — the same
	// adapter-owned-default rule as CodexBin: a caller that configures
	// nothing gets exactly the pre-seam behavior. Set before probeCatalog
	// below, which already runs through it.
	cli := o.CLI
	if cli == nil {
		cli = &provider.HostCLI{}
	}
	p := &Provider{
		codexBin:       codexBin,
		configPath:     configPath,
		loginDir:       o.LoginDir,
		runner:         o.Runner,
		cli:            cli,
		bus:            o.Bus,
		log:            logger,
		now:            now,
		captureTimeout: defaultCaptureTimeout,
		authTTL:        defaultAuthTTL,
		loginPoll:      defaultLoginPoll,
		loginWindow:    defaultLoginWindow,
		logoutTimeout:  defaultLogoutTimeout,
		keyDelay:       defaultKeyDelay,
		probeTimeout:   defaultProbeTimeout,
	}
	// Boot probe (issue #156): discover the live model catalog ONCE,
	// synchronously. A probe failure NEVER errors New — the accessors fall
	// back to the compiled-in pinned catalog (loudly logged in probeCatalog).
	p.probeCatalog()
	return p, nil
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

// instanceCodexHome is the codex state home under a run's private instance HOME
// (issue #202 / ADR-0038): <home>/.codex, mirroring codex's own HOME-relative
// default (the $CODEX_HOME default is ~/.codex). Every INSTANCE-facing codex
// path hangs off it — the rollout tree, the trust config.toml, the global
// AGENTS.md bridge — and it is also the CODEX_HOME env pin InjectCredentials
// returns so a session inheriting a stale $CODEX_HOME cannot reach the master
// store.
func instanceCodexHome(home string) string { return filepath.Join(home, ".codex") }

// MasterStore is the package-level form of the MasterStoreSpec declaration,
// parameterized on the loginDir the adapter itself is handed. It exists so
// the wiring layer can hand providercli a Spec closure BEFORE the adapter is
// constructed — the container-decorated login runner is an input to New, so
// the closure cannot come from the not-yet-existing Provider. Function and
// method MUST stay one resolver (the single-source rule the method doc
// pins): a divergence would let the containerized login mount a different
// store than the adapter's own master-store ops target.
func MasterStore(loginDir string) provider.MasterStoreSpec {
	return provider.MasterStoreSpec{
		EnvVar:     "CODEX_HOME",
		HostDir:    codexHomeDir(loginDir),
		HomeSubdir: ".codex",
	}
}

// MasterStoreSpec implements provider.AgentProvider (issue #206): codex's
// MASTER credential store, resolved freshly per call through codexHomeDir
// ($CODEX_HOME when set, else <loginDir>/.codex — the resolver
// credentialsPath itself wraps), the same chain logout's rm-escalation
// target and the refresh probe's CODEX_HOME pin derive from, so this
// declaration can never disagree with them. The env override outranking the
// default is what makes the declared override-awareness real (the issue #211
// criterion, pinned by the conformance suite's master-store-spec obligation).
// Delegates to the package-level MasterStore so method and wiring closure
// are one resolver.
func (p *Provider) MasterStoreSpec() provider.MasterStoreSpec {
	return MasterStore(p.loginDir)
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

// Models returns a deep copy of the enriched model catalog, in dropdown
// order (issue #156: entries can share one efforts backing array, so a
// shallow clone would let a caller poison every model's list at once). The
// probed catalog when the boot probe succeeded, else the compiled-in
// fallback — a zero-value Provider therefore serves the pinned fallback.
func (p *Provider) Models() []provider.ModelOption {
	if p.catModels != nil {
		return provider.CloneModelOptions(p.catModels)
	}
	return provider.CloneModelOptions(fallbackModels)
}

// Efforts returns a copy of the effort catalog, in dropdown order: the
// probed union when the boot probe succeeded, else the compiled-in fallback.
func (p *Provider) Efforts() []provider.Option {
	if p.catEfforts != nil {
		return slices.Clone(p.catEfforts)
	}
	return slices.Clone(fallbackEfforts)
}

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
//   - spec.Remote is unused too, and codex deliberately does NOT implement
//     provider.RemoteCapable (issue #163): it receives lab's remote knob and
//     ignores it, so this argv is byte-identical for both values. (0.133's
//     `codex remote-control start|stop|pair` is a HOST-level app-server daemon
//     with a pairing code, not a per-session spawn flag — nothing to honor
//     here. Wiring it later would mean implementing the capability, with zero
//     change to core.)
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
