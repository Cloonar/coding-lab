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

	// defaultForensicCaptureTimeout bounds the mismatch backstop's diagnostic
	// pane snapshot (issue #165 item 1). ReadChat has no caller ctx to thread
	// through the capture closure, so it owns a fixed background timeout
	// instead: long enough for a tmux capture-pane round trip, short enough
	// that a wedged pane never hangs a chat read waiting on forensics nobody
	// will ever look at synchronously.
	defaultForensicCaptureTimeout = 5 * time.Second
)

// models is the provider-owned model catalog (pinned: v0 spawn.go). Family
// aliases, not pinned ids, so the list tracks the latest of each family;
// [1m] only where the 1M-context variant is included on the plan. Enriched
// per issue #156: every model carries the FULL shared efforts list — Claude
// Code clamps unsupported model+effort combos itself, so every level is
// offered on every model — and no DefaultEffort (claude reports no per-model
// default; the first-entry rule keeps "low" as the all-unset fallback).
var models = []provider.ModelOption{
	{Option: provider.Option{Value: "opus[1m]", Label: "Opus (1M)"}, Efforts: efforts},
	{Option: provider.Option{Value: "sonnet", Label: "Sonnet"}, Efforts: efforts},
	{Option: provider.Option{Value: "fable", Label: "Fable"}, Efforts: efforts},
	{Option: provider.Option{Value: "haiku", Label: "Haiku"}, Efforts: efforts},
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
	// ConfigPath is claude's MASTER global config file (~/.claude.json). It is
	// the folder-trust reference only for master-store ops — the
	// InjectCredentials source for the oauthAccount metadata (issue #202); the
	// per-run trust seed writes to <SeedOpts.Home>/.claude.json instead. Empty
	// defaults to LoginDir/.claude.json (LoginDir is the service user's HOME) —
	// the same adapter-owned-default decision as ClaudeBin (issue #78). The old
	// `-claude-config` flag / LAB_CLAUDE_CONFIG env is becoming a deprecated
	// alias for the generic `-provider-config claude-code=path`; either way the
	// resolved value lands here unchanged.
	ConfigPath string
	// LoginDir is the working directory of the login session ($HOME —
	// login is global, one machine-level credential). All INSTANCE-facing
	// state (session registry, transcript tree, user commands) is NOT resolved
	// from here: it derives per-call from the run's private instance HOME on the
	// seam (issue #202 / ADR-0038), never from a construction-time master path.
	LoginDir string
	// Runner drives tmux for the login session.
	Runner tmuxx.SessionRunner
	// CLI runs this provider's machine-level CLI invocations — the auth
	// status check, `claude auth logout`, and the credential-refresh poke
	// (issue #206). nil defaults to provider.HostCLI (direct host execution,
	// the pre-seam behavior byte-for-byte); a container-mode server injects a
	// containerized runner so the host needs no claude binary. This package
	// never learns which it got — podman stays entirely outside it.
	CLI provider.CLIRunner
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
	claudeBin  string
	configPath string // MASTER ~/.claude.json — master-store ops only (login flow, InjectCredentials source)
	loginDir   string
	runner     tmuxx.SessionRunner
	cli        provider.CLIRunner // machine-level CLI seam (issue #206) — never exec directly
	bus        *events.Bus
	log        *slog.Logger
	now        func() time.Time

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
var _ provider.RemoteCapable = (*Provider)(nil)

// New validates o and returns a Provider with the pinned production
// timeouts. ClaudeBin/ConfigPath are the two fields issue #78 made
// optional: main hands them through only when a caller has configured an
// entry for this provider (the generic `-provider-bin`/`-provider-config`
// maps, or their deprecated `-claude`/`-claude-config` aliases), and New
// fills the adapter-owned default when the entry is absent.
func New(o Options) (*Provider, error) {
	switch {
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
	// CLI defaults to direct host execution (issue #206) — the same
	// adapter-owned-default rule as ClaudeBin: a caller that configures
	// nothing gets exactly the pre-seam behavior.
	cli := o.CLI
	if cli == nil {
		cli = &provider.HostCLI{}
	}
	p := &Provider{
		claudeBin:      claudeBin,
		configPath:     configPath,
		loginDir:       o.LoginDir,
		runner:         o.Runner,
		cli:            cli,
		bus:            o.Bus,
		log:            logger,
		now:            now,
		captureTimeout: defaultCaptureTimeout,
		bridgeTimeout:  defaultBridgeTimeout,
		loginTimeout:   defaultLoginTimeout,
		loginPoll:      defaultLoginPoll,
		authTTL:        defaultAuthTTL,
		logoutTimeout:  defaultLogoutTimeout,
		keyDelay:       defaultDialogKeyDelay,
		capturing:      map[string]bool{},
	}
	// Wire the mismatch backstop's forensic instrumentation (issue #165 item
	// 1) onto the intent registry: p.intents is a plain zero-value struct
	// field, so it starts life with both log and capture nil (instrumentation
	// off) until set here. The capture closure owns its own bounded
	// background context — see defaultForensicCaptureTimeout — since ReadChat
	// has no per-call ctx to thread through the registry.
	p.intents.log = logger
	p.intents.capture = func(sessionName string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), defaultForensicCaptureTimeout)
		defer cancel()
		return p.runner.CapturePane(ctx, sessionName)
	}
	return p, nil
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
	// The one host a claude-code run must reach DIRECTLY (issue #24): the
	// Messages API endpoint the CLI streams every turn to. It rides in a
	// gateway-wired run's NO_PROXY so a credential-gateway outage cannot take
	// the model connection down with it (ADR-0067 rules LLM traffic out of the
	// gateway's scope). The literal belongs HERE and nowhere else — core naming
	// it is the ADR-0033 neutrality violation this declaration exists to undo.
	// Bare hostname: no scheme, no port.
	DirectAPIHosts: []string{"api.anthropic.com"},
}

// SeedMeta implements provider.AgentProvider: claude-code's seeding shapes,
// returned as a copy with cloned slices so a caller can never mutate the
// declaration.
func (p *Provider) SeedMeta() provider.SeedMeta {
	m := seedMeta
	m.ExcludeEntries = slices.Clone(m.ExcludeEntries)
	m.SeededPathPatterns = slices.Clone(m.SeededPathPatterns)
	m.ScrubPatterns = slices.Clone(m.ScrubPatterns)
	m.DirectAPIHosts = slices.Clone(m.DirectAPIHosts)
	return m
}

// Models returns a deep copy of the enriched model catalog, in dropdown
// order (issue #156: the entries share one efforts backing array, so a
// shallow clone would let a caller poison every model's list at once).
func (p *Provider) Models() []provider.ModelOption { return provider.CloneModelOptions(models) }

// Efforts returns a copy of the effort catalog, in dropdown order.
func (p *Provider) Efforts() []provider.Option { return slices.Clone(efforts) }

// SpawnOptions returns a copy of the spawn-options catalog (issue #19), in
// render order.
func (p *Provider) SpawnOptions() []provider.OptionSpec { return slices.Clone(spawnOptions) }

// MasterStore is the package-level form of the MasterStoreSpec declaration.
// It exists so the wiring layer can hand providercli a Spec closure BEFORE
// the adapter is constructed — the container-decorated login runner is an
// input to New, so the closure cannot come from the not-yet-existing
// Provider. Function and method MUST stay one resolver (the single-source
// rule the method doc pins): a divergence would let the containerized login
// mount a different store than the adapter's own master-store ops target.
func MasterStore() provider.MasterStoreSpec {
	return provider.MasterStoreSpec{
		EnvVar:     "CLAUDE_CONFIG_DIR",
		HostDir:    masterConfigDir(),
		HomeSubdir: ".claude",
	}
}

// MasterStoreSpec implements provider.AgentProvider (issue #206): claude's
// MASTER credential store, resolved freshly per call through masterConfigDir
// (logout.go) — the ONE resolver every master-store operation shares — so
// this declaration can never disagree with the logout rm-escalation, the
// InjectCredentials source, or the refresh poke's env pin. CLAUDE_CONFIG_DIR
// outranks HOME for ALL claude state (compat §3a), which is what makes the
// declared override-awareness real (the issue #211 criterion, pinned by the
// conformance suite's master-store-spec obligation). Delegates to the
// package-level MasterStore so method and wiring closure are one resolver.
func (p *Provider) MasterStoreSpec() provider.MasterStoreSpec {
	return MasterStore()
}

// SupportsRemoteControl implements provider.RemoteCapable (issue #163):
// claude-code honors SpawnSpec.Remote — it is the provider whose sessions the
// knob was named after, and the claude.ai deep link is a byproduct of the flag
// SpawnArgv emits for it. Lab reads only this yes/no; the mechanism stays here.
func (p *Provider) SupportsRemoteControl() bool { return true }

// SpawnArgv builds the pinned instance spawn command:
//
//	{claude} [--remote-control <session>] --permission-mode auto [--model M] [--effort E] [prompt]
//
// The remote-control flag AND its session-name argument ride on spec.Remote
// (issue #163) — both present when it is set, both absent otherwise, never a
// stray flag or an empty positional. A non-remote session registers nothing
// with claude's session registry, so it has no deep link to capture; lab gates
// that on the same knob (it never learns the flag).
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
	argv := []string{claudeBin}
	if spec.Remote {
		argv = append(argv, "--remote-control", spec.SessionName)
	}
	argv = append(argv, "--permission-mode", "auto")
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

// Per-run instance-HOME path derivation (issue #202 / ADR-0038). Every
// INSTANCE-facing claude path lives under the run's private HOME, derived per
// call from the home argument threaded on the seam — never from a
// construction-time master path. An empty home means "no per-run home": the
// caller treats it as a miss / no-op and NEVER falls back to the master store
// or the process HOME (isolation by construction).
//
// The layout is <home>/.claude as an EXPLICIT config dir: the spawn env pins
// CLAUDE_CONFIG_DIR=<home>/.claude (instanceConfigDirUnder — see
// InjectCredentials), so the session registry, the transcript tree, the
// user-command dir, the global config, and the credentials file all sit where
// the CLI's config-dir resolution puts them (compat §3a). This is deliberately
// version-proof: config-dir-set resolution is identical on every probed CLI,
// while the previous empty-pin's "empty behaves as unset" only holds from
// ~2.1.214 — on 2.1.198 an empty value is joined as a real (relative) config
// dir for the CREDENTIALS path, so the instance spawned unauthenticated and
// every chat reply died at "Not logged in" (live-straced 2026-07-23). The only
// path this moved is the global config: .claude.json now lives INSIDE
// <home>/.claude rather than at <home>/.claude.json (HOME-convention).
func instanceConfigDirUnder(home string) string { return filepath.Join(home, ".claude") }
func registryDirUnder(home string) string       { return filepath.Join(home, ".claude", "sessions") }
func projectsDirUnder(home string) string       { return filepath.Join(home, ".claude", "projects") }
func userCommandsDirUnder(home string) string   { return filepath.Join(home, ".claude", "commands") }
func globalConfigUnder(home string) string {
	return filepath.Join(home, ".claude", ".claude.json")
}
func instanceCredsUnder(home string) string {
	return filepath.Join(home, ".claude", ".credentials.json")
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
