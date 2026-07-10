package providertest

// Tier-1 hermetic provider conformance suite (issue #80). Conformance is the
// single entry point: every seam obligation a new AgentProvider must clear
// before registration, executable in ordinary CI with no agent CLI, no tmux,
// and no network. Each obligation is an unexported check function that
// RETURNS its violations as errors; Conformance is only the *testing.T
// front-end (one subtest per obligation, stable kebab-case names). That split
// is what lets this suite's own tests run deliberately-broken adapters
// through the check functions and assert the failure messages directly.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// Fixture is the per-adapter conformance input.
type Fixture struct {
	// AttributionSamples: real single-line marker strings this provider's CLI
	// writes into commits/PRs (its ground-truth test vectors, issue #75/#80).
	// Required, >=1.
	AttributionSamples []string
	// CleanSamples: lines that must NOT match any scrub pattern (over-broad
	// pattern guard). Optional.
	CleanSamples []string
}

// Conformance executes every Tier-1 seam obligation against p, hermetically
// (issue #80): one t.Run subtest per obligation, each t.Error-ing the errors
// its check function returned. The git-backed seeding obligations skip when
// git is off PATH (testutil.RequireTool convention) and scrub-markers skips
// only its BRE sub-check when grep is absent; everything else always runs.
//
// The live-process seam methods — AuthStatus, LoginStart/LoginSubmitCode,
// Logout, Commands, Reply, AnswerDialog, Interrupt, LocateTranscript, and
// ReadChat's transcript GRAMMAR — are deliberately NOT exercised: each is an
// adapter-owned fragile CLI coupling that must be live-verified per adapter
// (the ADR-0008 bar, Tier-2), never hermetically. ReadChat's ARGUMENT
// contract is the exception (issue #92): how an adapter treats an empty
// transcriptPath and a vanished one is seam law core relies on blind, needs
// no CLI, and is pinned here by read-chat. Tier-1 otherwise covers exactly
// the declarations lab's core consumes blind: pattern dialect (issue #75 /
// ADR-0033), the lab-owned context file (issue #79 / ADR-0035), defensive
// catalog/meta clones, spawn argv shape (issue #19 / ADR-0021), the
// auth-flow vocabulary and login-session naming (issue #77 / ADR-0034), and
// executable exclude/scrub coverage.
func Conformance(t *testing.T, p provider.AgentProvider, fx Fixture) {
	t.Helper()
	report := func(t *testing.T, errs []error) {
		t.Helper()
		for _, err := range errs {
			t.Error(err)
		}
	}
	t.Run("patterns-dialect", func(t *testing.T) { report(t, checkPatternsDialect(p.SeedMeta())) })
	t.Run("context-file-lab-owned", func(t *testing.T) { report(t, checkContextFileName(p.SeedMeta())) })
	t.Run("seedmeta-clone", func(t *testing.T) { report(t, checkSeedMetaClone(p)) })
	t.Run("catalogs", func(t *testing.T) { report(t, checkCatalogs(p)) })
	t.Run("spawn-argv", func(t *testing.T) { report(t, checkSpawnArgv(p)) })
	t.Run("auth-flow", func(t *testing.T) { report(t, checkAuthFlow(p)) })
	t.Run("login-session", func(t *testing.T) { report(t, checkLoginSession(p)) })
	t.Run("read-chat", func(t *testing.T) { report(t, checkReadChat(t, p)) })
	t.Run("seeding-exclude-coverage", func(t *testing.T) { report(t, checkSeedingExcludeCoverage(t, p)) })
	t.Run("seeding-incogni", func(t *testing.T) { report(t, checkSeedingIncogni(t, p)) })
	t.Run("scrub-markers", func(t *testing.T) { report(t, checkScrubMarkers(t, p, fx)) })
}

// checkPatternsDialect: every declared ScrubPatterns and SeededPathPatterns
// entry must compile through provider.CompileScrubPatterns — the product
// compile path (never reimplemented here), so what passes is exactly what the
// registry accepts at boot. ScrubPatterns must be non-empty: both incogni
// enforcement points run the cross-provider union, and an adapter declaring
// no markers silently weakens nothing today but cannot satisfy the incogni
// bar for its own output (issue #75 / ADR-0033).
func checkPatternsDialect(meta provider.SeedMeta) []error {
	var errs []error
	if len(meta.ScrubPatterns) == 0 {
		errs = append(errs, fmt.Errorf("patterns-dialect: SeedMeta().ScrubPatterns is empty — an adapter with no declared attribution markers cannot satisfy the incogni bar; if this provider's CLI is genuinely marker-free, flag that on issue #80 instead of declaring nothing (issue #75 / ADR-0033)"))
	}
	for i, pat := range meta.ScrubPatterns {
		if _, err := provider.CompileScrubPatterns([]string{pat}); err != nil {
			errs = append(errs, fmt.Errorf("patterns-dialect: SeedMeta().ScrubPatterns[%d]: %v — every pattern must stay in the BRE∩RE2 common dialect (issue #75 / ADR-0033)", i, err))
		}
	}
	for i, pat := range meta.SeededPathPatterns {
		if _, err := provider.CompileScrubPatterns([]string{pat}); err != nil {
			errs = append(errs, fmt.Errorf("patterns-dialect: SeedMeta().SeededPathPatterns[%d] %q: %v — every pattern must stay in the BRE∩RE2 common dialect (issue #75 / ADR-0033)", i, pat, err))
		}
	}
	return errs
}

// labOwnedContextFile is the .local convention (issue #79 / ADR-0035): the
// base name carries a ".local" suffix before its extension, so lab never
// writes a file repositories legitimately track.
var labOwnedContextFile = regexp.MustCompile(`\.local\.[A-Za-z0-9]+$`)

// checkContextFileName: ContextFileName must be a non-empty bare file name
// (seeded at the worktree root, never a path) and lab-owned per the hard rule
// documented on provider.SeedMeta.ContextFileName (issue #79 / ADR-0035).
func checkContextFileName(meta provider.SeedMeta) []error {
	name := meta.ContextFileName
	if name == "" {
		return []error{fmt.Errorf("context-file-lab-owned: SeedMeta().ContextFileName is empty — the provider must declare the lab-owned context file its CLI reads (issue #79 / ADR-0035)")}
	}
	var errs []error
	if strings.ContainsAny(name, `/\`) {
		errs = append(errs, fmt.Errorf("context-file-lab-owned: SeedMeta().ContextFileName %q: contains a path separator — must be a bare file name, seeded at the worktree root (issue #79 / ADR-0035)", name))
	}
	if !labOwnedContextFile.MatchString(name) {
		errs = append(errs, fmt.Errorf("context-file-lab-owned: SeedMeta().ContextFileName %q: not lab-owned — a repository could legitimately track that name; must use the .local-suffixed convention, e.g. %q or %q (issue #79 / ADR-0035)", name, "CLAUDE.local.md", "AGENTS.local.md"))
	}
	return errs
}

// mutationSentinel is the value the clone check writes into every returned
// slice; distinctive enough that it can never collide with a real catalog or
// pattern entry.
const mutationSentinel = "providertest-conformance-mutation-sentinel"

// corrupt mutates a returned slice the way a careless caller would: overwrite
// element 0 and append. If the provider aliased its internal state, the next
// call observes the overwrite.
func corrupt[T any](s []T, sentinel T) []T {
	if len(s) > 0 {
		s[0] = sentinel
	}
	return append(s, sentinel)
}

// checkSeedMetaClone: SeedMeta(), Models(), Efforts(), and SpawnOptions()
// must return defensive clones — the declarations are shared package state,
// and one caller's mutation corrupting the next caller's view corrupts the
// registry unions and the spawn catalogs identically (issue #80; the pinned
// example is claudecode.SeedMeta's slices.Clone discipline).
func checkSeedMetaClone(p provider.AgentProvider) []error {
	aliased := func(what string) error {
		return fmt.Errorf("seedmeta-clone: %s aliases the provider's internal declaration — a caller's mutation corrupted the next call's view; return defensive clones (issue #80)", what)
	}
	var errs []error

	first := p.SeedMeta()
	origExclude := slices.Clone(first.ExcludeEntries)
	origPaths := slices.Clone(first.SeededPathPatterns)
	origScrub := slices.Clone(first.ScrubPatterns)
	_ = corrupt(first.ExcludeEntries, mutationSentinel)
	_ = corrupt(first.SeededPathPatterns, mutationSentinel)
	_ = corrupt(first.ScrubPatterns, mutationSentinel)
	second := p.SeedMeta()
	if !slices.Equal(second.ExcludeEntries, origExclude) {
		errs = append(errs, aliased("SeedMeta().ExcludeEntries"))
	}
	if !slices.Equal(second.SeededPathPatterns, origPaths) {
		errs = append(errs, aliased("SeedMeta().SeededPathPatterns"))
	}
	if !slices.Equal(second.ScrubPatterns, origScrub) {
		errs = append(errs, aliased("SeedMeta().ScrubPatterns"))
	}

	optionSentinel := provider.Option{Value: mutationSentinel, Label: mutationSentinel}
	models := p.Models()
	origModels := slices.Clone(models)
	_ = corrupt(models, optionSentinel)
	if !slices.Equal(p.Models(), origModels) {
		errs = append(errs, aliased("Models()"))
	}
	efforts := p.Efforts()
	origEfforts := slices.Clone(efforts)
	_ = corrupt(efforts, optionSentinel)
	if !slices.Equal(p.Efforts(), origEfforts) {
		errs = append(errs, aliased("Efforts()"))
	}
	specs := p.SpawnOptions()
	origSpecs := slices.Clone(specs)
	_ = corrupt(specs, provider.OptionSpec{Key: mutationSentinel})
	if !slices.Equal(p.SpawnOptions(), origSpecs) {
		errs = append(errs, aliased("SpawnOptions()"))
	}
	return errs
}

// checkCatalogs: Models()/Efforts() must be non-empty and well-formed — the
// spawn default resolution falls back to catalog[0]
// (internal/instance/credential.go layerSpawnDefault), so a non-empty catalog
// of unique, non-empty values IS the "defaults are members" guarantee.
// SpawnOptions() may be empty, but every declared spec needs a unique
// non-empty Key, a recognized Type, and a Default its own type accepts
// (issue #19 / ADR-0021).
func checkCatalogs(p provider.AgentProvider) []error {
	var errs []error
	errs = append(errs, checkOptionCatalog("Models()", p.Models())...)
	errs = append(errs, checkOptionCatalog("Efforts()", p.Efforts())...)

	seen := map[string]int{}
	for i, spec := range p.SpawnOptions() {
		if spec.Key == "" {
			errs = append(errs, fmt.Errorf("catalogs: SpawnOptions()[%d] declares an empty Key (issue #19 / ADR-0021)", i))
		} else if j, dup := seen[spec.Key]; dup {
			errs = append(errs, fmt.Errorf("catalogs: SpawnOptions()[%d] duplicates Key %q (first at [%d]) — the options bag is keyed by it (issue #19 / ADR-0021)", i, spec.Key, j))
		} else {
			seen[spec.Key] = i
		}
		if spec.Type != provider.OptionTypeBool {
			errs = append(errs, fmt.Errorf("catalogs: SpawnOptions() key %q declares unrecognized Type %q — the vocabulary is exactly %q today (issue #19 / ADR-0021)", spec.Key, spec.Type, provider.OptionTypeBool))
		} else if !provider.ValidOptionValue(spec, spec.Default) {
			errs = append(errs, fmt.Errorf("catalogs: SpawnOptions() key %q declares Default %q, not a valid %q value — the resolution layer applies the declared default verbatim (issue #19 / ADR-0021)", spec.Key, spec.Default, spec.Type))
		}
	}
	return errs
}

func checkOptionCatalog(what string, opts []provider.Option) []error {
	var errs []error
	if len(opts) == 0 {
		errs = append(errs, fmt.Errorf("catalogs: %s is empty — spawn default resolution falls back to the catalog's first entry, so a well-formed catalog is the \"defaults are members\" guarantee (internal/instance/credential.go layerSpawnDefault; issue #80)", what))
	}
	seen := map[string]int{}
	for i, o := range opts {
		if o.Value == "" {
			errs = append(errs, fmt.Errorf("catalogs: %s[%d] has an empty Value (issue #80)", what, i))
			continue
		}
		if o.Label == "" {
			errs = append(errs, fmt.Errorf("catalogs: %s[%d] (Value %q) has an empty Label — the dropdown renders it (issue #80)", what, i, o.Value))
		}
		if j, dup := seen[o.Value]; dup {
			errs = append(errs, fmt.Errorf("catalogs: %s[%d] duplicates Value %q (first at [%d]) — catalog values must be unique (issue #80)", what, i, o.Value, j))
		} else {
			seen[o.Value] = i
		}
	}
	return errs
}

// conformancePrompt is the distinctive marker prompt the spawn-argv check
// threads through SpawnArgv; it must never appear in an empty-prompt argv.
const conformancePrompt = "providertest-conformance-prompt-3f9c1a"

// checkSpawnArgv pins the argv contract (issue #19 / ADR-0021, SpawnSpec
// docs): a single slices.Equal against append(base, P) proves BOTH that a
// non-empty InitialPrompt rides as the trailing positional argument AND that
// an empty prompt appends nothing. The options round-trip then proves every
// declared bool option is a real knob: an explicit default equals unset, and
// the negated default changes the argv — probed with a non-empty prompt,
// because prompt-scoped options (claude-code's ultracode) are legal.
func checkSpawnArgv(p provider.AgentProvider) []error {
	var errs []error
	var model, effort string
	if m := p.Models(); len(m) > 0 {
		model = m[0].Value
	}
	if e := p.Efforts(); len(e) > 0 {
		effort = e[0].Value
	}
	spec := provider.SpawnSpec{SessionName: "lab-conformance-1", Model: model, Effort: effort}

	base := p.SpawnArgv(spec)
	if len(base) == 0 {
		errs = append(errs, fmt.Errorf("spawn-argv: SpawnArgv with empty InitialPrompt returned an empty argv (issue #80)"))
	}
	for i, arg := range base {
		if arg == "" {
			errs = append(errs, fmt.Errorf("spawn-argv: SpawnArgv with empty InitialPrompt: argv[%d] is the empty string — an empty prompt must append nothing, never an empty positional (provider.SpawnSpec.InitialPrompt contract; issue #80)", i))
		}
		if strings.Contains(arg, conformancePrompt) {
			errs = append(errs, fmt.Errorf("spawn-argv: SpawnArgv with empty InitialPrompt: argv[%d] %q carries the probe prompt (issue #80)", i, arg))
		}
	}

	promptSpec := spec
	promptSpec.InitialPrompt = conformancePrompt
	withPrompt := p.SpawnArgv(promptSpec)
	want := append(slices.Clone(base), conformancePrompt)
	if !slices.Equal(withPrompt, want) {
		errs = append(errs, fmt.Errorf("spawn-argv: a non-empty InitialPrompt must ride as the trailing positional argument: SpawnArgv(prompt) = %q, want the empty-prompt argv plus the prompt appended = %q (provider.SpawnSpec.InitialPrompt contract; issue #19 / issue #80)", withPrompt, want))
	}

	for _, optSpec := range p.SpawnOptions() {
		if optSpec.Type != provider.OptionTypeBool || !provider.ValidOptionValue(optSpec, optSpec.Default) {
			continue // catalogs reports the malformed spec; the round-trip needs a valid bool
		}
		defSpec := promptSpec
		defSpec.Options = map[string]string{optSpec.Key: optSpec.Default}
		defArgv := p.SpawnArgv(defSpec)
		if !slices.Equal(defArgv, withPrompt) {
			errs = append(errs, fmt.Errorf("spawn-argv: option %q: an explicit default (%s=%s) yields %q but an unset bag yields %q — explicit default must equal unset (issue #19 / ADR-0021)", optSpec.Key, optSpec.Key, optSpec.Default, defArgv, withPrompt))
		}
		neg := "true"
		if optSpec.Default == "true" {
			neg = "false"
		}
		negSpec := promptSpec
		negSpec.Options = map[string]string{optSpec.Key: neg}
		if negArgv := p.SpawnArgv(negSpec); slices.Equal(negArgv, withPrompt) {
			errs = append(errs, fmt.Errorf("spawn-argv: option %q: %s=%s produced the same argv as an unset bag — a declared spawn option must be observable in the spawn command, prompt-scoped application included (issue #19 / ADR-0021; issue #80)", optSpec.Key, optSpec.Key, neg))
		}
	}
	return errs
}

// checkAuthFlow: the declared Kind must come from the AuthFlow* vocabulary —
// the SPA auth card renders from it, so an unknown kind is an unrenderable
// provider (issue #51 decision 7).
func checkAuthFlow(p provider.AgentProvider) []error {
	kind := p.AuthFlow().Kind
	switch kind {
	case provider.AuthFlowOAuthCode, provider.AuthFlowOAuthRedirect, provider.AuthFlowDeviceCode, provider.AuthFlowAPIKey, provider.AuthFlowExternal:
		return nil
	}
	return []error{fmt.Errorf("auth-flow: AuthFlow().Kind %q is not one of the declared kinds (%q, %q, %q, %q, %q) — the SPA auth card renders from this vocabulary (issue #51 decision 7; issue #80)", kind,
		provider.AuthFlowOAuthCode, provider.AuthFlowOAuthRedirect, provider.AuthFlowDeviceCode, provider.AuthFlowAPIKey, provider.AuthFlowExternal)}
}

// providerIDPattern: the id lands inside tmux session names that are
// targeted BARE (tmuxx doc: send-keys/capture-pane reject the "=" prefix),
// so it must stay in the safe lowercase-kebab alphabet.
var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// checkLoginSession: ID() keys repos.provider/runs.provider and derives the
// provider's login-session name; tmuxx.IsLoginSession must recognize that
// name, or cap counting, branch ownership, and stop-all would treat the login
// session as a normal instance (issue #77 / ADR-0034).
func checkLoginSession(p provider.AgentProvider) []error {
	id := p.ID()
	if id == "" {
		return []error{fmt.Errorf("login-session: ID() is empty — the id keys repos.provider and the login-session name (issue #77 / ADR-0034)")}
	}
	var errs []error
	if !providerIDPattern.MatchString(id) {
		errs = append(errs, fmt.Errorf("login-session: ID() %q must match %s — it is embedded in tmux session names targeted bare (internal/tmuxx docs; issue #77 / ADR-0034)", id, providerIDPattern))
	}
	if name := tmuxx.LoginSessionName(id); !tmuxx.IsLoginSession(name) {
		errs = append(errs, fmt.Errorf("login-session: tmuxx.IsLoginSession(%q) = false for this provider's own login session — the cap/ownership/stop-all exclusions would miss it (issue #77 / ADR-0034)", name))
	}
	return errs
}

// checkReadChat pins ReadChat's ARGUMENT contract hermetically (issue #92) —
// the transcript grammar and live-signal composition stay adapter-owned
// Tier-2 couplings, but how an adapter treats the arguments core passes
// blind is seam law with no CLI in it:
//
//   - transcriptPath "" is the pre-transcript read of an ACTIVE run, the
//     window before LocateTranscript first hits: an idle empty chat, never
//     an error — an adapter that errors here breaks every fresh spawn's
//     chat view.
//   - runtimeDir "" (signals off / transcript-only read) and a fresh empty
//     runtime dir (signals armed, no spool yet — the common state seconds
//     after spawn) must both yield that same idle read.
//   - a non-empty transcriptPath that no longer exists must surface
//     provider.ErrTranscriptGone: httpapi renders the graceful "transcript
//     no longer available" state from that sentinel; a raw error would 500.
func checkReadChat(tb testing.TB, p provider.AgentProvider) []error {
	var errs []error
	requireIdle := func(what string, chat provider.Chat, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("read-chat: %s returned error %v — an unlocated transcript is the normal pre-transcript state of an active run, never an error (provider.AgentProvider.ReadChat contract; issue #92)", what, err))
			return
		}
		if chat.State != provider.StateIdle {
			errs = append(errs, fmt.Errorf("read-chat: %s returned State %q; want %q — before any transcript exists the run has seen no assistant activity (issue #92)", what, chat.State, provider.StateIdle))
		}
		if len(chat.Messages) != 0 {
			errs = append(errs, fmt.Errorf("read-chat: %s returned %d message(s); want none — the message stream is transcript-derived and no transcript exists yet (issue #92)", what, len(chat.Messages)))
		}
		if chat.Cursor != 0 {
			errs = append(errs, fmt.Errorf("read-chat: %s returned Cursor %d; want 0 — a non-zero cursor on an empty chat would corrupt the API's seq windowing (issue #92)", what, chat.Cursor))
		}
		if chat.PendingDialog != nil {
			errs = append(errs, fmt.Errorf("read-chat: %s returned a PendingDialog; want nil — no live signal was armed, so nothing can be pending (issue #92)", what))
		}
	}

	chat, err := p.ReadChat("conformance-run-1", "", "")
	requireIdle(`ReadChat(runID, "", "")`, chat, err)

	chat, err = p.ReadChat("conformance-run-1", tb.TempDir(), "")
	requireIdle(`ReadChat(runID, emptyRuntimeDir, "")`, chat, err)

	gone := filepath.Join(tb.TempDir(), "gone.jsonl")
	if _, err := p.ReadChat("conformance-run-1", "", gone); !errors.Is(err, provider.ErrTranscriptGone) {
		errs = append(errs, fmt.Errorf("read-chat: ReadChat on a vanished transcript path returned %v; want provider.ErrTranscriptGone — httpapi renders the \"transcript no longer available\" state from that sentinel, and a raw error would 500 (issue #92)", err))
	}
	return errs
}

// conformanceRepo is the minimal repo row the lab seeder renders the context
// file from (mirrors internal/seeder's builtin-binding fixture).
var conformanceRepo = store.Repo{Name: "proj", TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none"}

// checkSeedingExcludeCoverage seeds a fresh hermetic linked worktree in
// production order — the provider's own SeedWorkspace first, then the lab
// seeder over its SeedMeta (instance.Launch's order) — and asserts three
// executable facts (issue #80):
//
//   - `git status --porcelain` is empty: ExcludeEntries cover every seeded
//     path AND nothing tracked was modified (D15 §9 measure 6).
//   - Anti-vacuous: the declared context file exists at the root, and a
//     non-empty SkillsDir landed non-empty — an adapter that seeds nothing
//     cannot pass on an empty worktree by accident.
//   - Every file seeding created matches at least one SeededPathPattern,
//     compiled case-SENSITIVELY like the pre-push hook's path grep
//     (internal/seeder/hook.go) — SeededPathPatterns must describe what lab
//     actually seeds (provider.SeedMeta docs).
//
// Setup failures (git missing, worktree creation) skip/fatal on tb;
// obligation violations are returned.
func checkSeedingExcludeCoverage(tb testing.TB, p provider.AgentProvider) []error {
	wt, env := newConformanceWorktree(tb)
	meta := p.SeedMeta()
	pre := walkWorktreeFiles(tb, wt)

	if err := p.SeedWorkspace(wt, provider.SeedOpts{}); err != nil {
		return []error{fmt.Errorf("seeding-exclude-coverage: SeedWorkspace on a fresh linked worktree: %v — seeding must succeed unattended before the session spawns (provider.AgentProvider.SeedWorkspace contract; issue #80)", err)}
	}
	if err := seeder.New().SeedWorkspace(wt, conformanceRepo, meta, seeder.Opts{}); err != nil {
		return []error{fmt.Errorf("seeding-exclude-coverage: lab seeder over this provider's SeedMeta: %v (issue #51 decision 8; issue #80)", err)}
	}

	errs := checkStatusClean("seeding-exclude-coverage", tb, wt, env, meta)
	if meta.ContextFileName != "" && !strings.ContainsAny(meta.ContextFileName, `/\`) {
		if _, err := os.Stat(filepath.Join(wt, meta.ContextFileName)); err != nil {
			errs = append(errs, fmt.Errorf("seeding-exclude-coverage: declared context file %q missing at the worktree root after seeding: %v (issue #80)", meta.ContextFileName, err))
		}
	}
	if meta.SkillsDir != "" {
		entries, err := os.ReadDir(filepath.Join(wt, filepath.FromSlash(meta.SkillsDir)))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("seeding-exclude-coverage: declared SkillsDir %q missing after seeding: %v (issue #80)", meta.SkillsDir, err))
		case len(entries) == 0:
			errs = append(errs, fmt.Errorf("seeding-exclude-coverage: declared SkillsDir %q is empty after seeding — the skills bundle did not land (issue #80)", meta.SkillsDir))
		}
	}

	var pathREs []*regexp.Regexp
	for _, pat := range meta.SeededPathPatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			errs = append(errs, fmt.Errorf("seeding-exclude-coverage: SeededPathPattern %q does not compile case-sensitively: %v (issue #80)", pat, err))
			continue
		}
		pathREs = append(pathREs, re)
	}
	for _, f := range walkWorktreeFiles(tb, wt) {
		if slices.Contains(pre, f) {
			continue // a tracked fixture file, not seeded
		}
		matched := false
		for _, re := range pathREs {
			if re.MatchString(f) {
				matched = true
				break
			}
		}
		if !matched {
			errs = append(errs, fmt.Errorf("seeding-exclude-coverage: seeded file %q matches no SeededPathPattern %q (case-sensitive, like the pre-push hook's path grep) — SeededPathPatterns must describe every path lab actually seeds (provider.SeedMeta docs; internal/seeder/hook.go; issue #80)", f, meta.SeededPathPatterns))
		}
	}
	return errs
}

// checkSeedingIncogni repeats the production seeding order with
// SeedOpts{Incogni: true}: the incogni worktree must carry no tracked-file
// writes either — the provider's attribution-off grants belong in excluded
// local settings, never in files git sees (D15 §9 measure 1; issue #80).
func checkSeedingIncogni(tb testing.TB, p provider.AgentProvider) []error {
	wt, env := newConformanceWorktree(tb)
	meta := p.SeedMeta()
	if err := p.SeedWorkspace(wt, provider.SeedOpts{Incogni: true}); err != nil {
		return []error{fmt.Errorf("seeding-incogni: SeedWorkspace(Incogni) on a fresh linked worktree: %v (D15 §9 measure 1; issue #80)", err)}
	}
	if err := seeder.New().SeedWorkspace(wt, conformanceRepo, meta, seeder.Opts{}); err != nil {
		return []error{fmt.Errorf("seeding-incogni: lab seeder over this provider's SeedMeta: %v (issue #51 decision 8; issue #80)", err)}
	}
	return checkStatusClean("seeding-incogni", tb, wt, env, meta)
}

// checkScrubMarkers proves the fixture's ground-truth marker lines are caught
// by BOTH incogni enforcement engines and that clean lines slip past
// (issue #75 / ADR-0033): the RE2 compile path (provider.CompileScrubPatterns
// — what the agent-API body sanitizer matches), the registry union
// (Registry.ScrubRegexps — the exact objects it runs), and the pre-push
// hook's real BRE engine (`grep -i -q -e …`, internal/seeder/hook.go). The
// dual-engine pass is what makes the BRE∩RE2 dialect rule executable: a
// pattern like `(foo|bar)` compiles as RE2 but means literal parens under
// BRE — only grep catches that. The BRE sub-check is skipped when grep is
// off PATH.
func checkScrubMarkers(tb testing.TB, p provider.AgentProvider, fx Fixture) []error {
	if len(fx.AttributionSamples) == 0 {
		return []error{fmt.Errorf("scrub-markers: Fixture.AttributionSamples is empty — the suite needs at least one real marker line this provider's CLI writes (issue #75; issue #80)")}
	}
	var errs []error
	for i, s := range fx.AttributionSamples {
		if strings.TrimSpace(s) == "" || strings.Contains(s, "\n") {
			errs = append(errs, fmt.Errorf("scrub-markers: Fixture.AttributionSamples[%d] %q must be a single non-empty line — the hook greps commit messages line-wise (issue #80)", i, s))
		}
	}
	for i, s := range fx.CleanSamples {
		if strings.TrimSpace(s) == "" || strings.Contains(s, "\n") {
			errs = append(errs, fmt.Errorf("scrub-markers: Fixture.CleanSamples[%d] %q must be a single non-empty line (issue #80)", i, s))
		}
	}
	if len(errs) > 0 {
		return errs // malformed fixture — engine verdicts would be meaningless
	}

	meta := p.SeedMeta()
	res, err := provider.CompileScrubPatterns(meta.ScrubPatterns)
	if err != nil {
		errs = append(errs, fmt.Errorf("scrub-markers: %v — markers cannot be verified under the RE2 engine (patterns-dialect owns the compile obligation; issue #75 / ADR-0033)", err))
	} else {
		for _, sample := range fx.AttributionSamples {
			if !matchesAny(res, sample) {
				errs = append(errs, fmt.Errorf("scrub-markers: attribution sample %q matches none of ScrubPatterns %q under the RE2 engine — the agent-API body sanitizer would pass this provider's own marker through (issue #75 / ADR-0033)", sample, meta.ScrubPatterns))
			}
		}
		for _, sample := range fx.CleanSamples {
			for i, re := range res {
				if re.MatchString(sample) {
					errs = append(errs, fmt.Errorf("scrub-markers: clean sample %q matched ScrubPatterns[%d] %q under the RE2 engine — an over-broad pattern would scrub legitimate text (issue #75 / ADR-0033)", sample, i, meta.ScrubPatterns[i]))
				}
			}
		}
	}

	reg, err := provider.NewRegistry(p)
	if err != nil {
		errs = append(errs, fmt.Errorf("scrub-markers: provider.NewRegistry(p): %v — the registry precompile is the boot-time gate every adapter must clear (issue #75 / ADR-0033)", err))
	} else {
		for _, sample := range fx.AttributionSamples {
			if !matchesAny(reg.ScrubRegexps(), sample) {
				errs = append(errs, fmt.Errorf("scrub-markers: attribution sample %q matches none of Registry.ScrubRegexps() — the union path the agent-API body sanitizer actually runs (issue #75 / ADR-0033)", sample))
			}
		}
	}

	if _, lookErr := exec.LookPath("grep"); lookErr != nil {
		tb.Logf("scrub-markers: grep not on PATH (%v) — skipping the BRE-engine sub-check", lookErr)
		return errs
	}
	if len(meta.ScrubPatterns) == 0 {
		return errs // patterns-dialect reports the empty declaration
	}
	grepArgs := []string{"-i", "-q"}
	for _, pat := range meta.ScrubPatterns {
		grepArgs = append(grepArgs, "-e", pat)
	}
	for _, sample := range fx.AttributionSamples {
		code, grepErr := grepExit(grepArgs, sample)
		if grepErr != nil {
			errs = append(errs, fmt.Errorf("scrub-markers: grep -i over ScrubPatterns %q failed on sample %q: %v — a pattern is likely invalid under the hook's POSIX BRE engine (issue #75 / ADR-0033)", meta.ScrubPatterns, sample, grepErr))
			continue
		}
		if code != 0 {
			errs = append(errs, fmt.Errorf("scrub-markers: attribution sample %q not matched by ScrubPatterns %q under the pre-push hook's BRE engine (grep -i, internal/seeder/hook.go) — BRE and RE2 must agree on every marker (issue #75 / ADR-0033)", sample, meta.ScrubPatterns))
		}
	}
	for _, sample := range fx.CleanSamples {
		code, grepErr := grepExit(grepArgs, sample)
		if grepErr != nil {
			errs = append(errs, fmt.Errorf("scrub-markers: grep -i over ScrubPatterns %q failed on clean sample %q: %v (issue #75 / ADR-0033)", meta.ScrubPatterns, sample, grepErr))
			continue
		}
		if code == 0 {
			errs = append(errs, fmt.Errorf("scrub-markers: clean sample %q matched ScrubPatterns %q under the pre-push hook's BRE engine — an over-broad pattern would reject legitimate pushes (issue #75 / ADR-0033)", sample, meta.ScrubPatterns))
		}
	}
	return errs
}

// --- hermetic seeding fixtures ---------------------------------------------

// newConformanceWorktree builds a REAL linked worktree (main checkout +
// `git worktree add`, one tracked file committed) with a hermetic git
// environment — the exact shape instance.Launch seeds, mirroring
// internal/seeder's newWorktree fixture (copied, not imported: test helpers
// don't cross packages). Skips the caller when git is off PATH.
func newConformanceWorktree(tb testing.TB) (wt string, env []string) {
	tb.Helper()
	testutil.RequireTool(tb, "git")
	root := tb.TempDir()
	env = append(os.Environ(), testutil.HermeticGitEnv(root)...)
	repo := filepath.Join(root, "repo")
	gitRun(tb, env, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("x\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	gitRun(tb, env, "-C", repo, "add", "tracked.txt")
	gitRun(tb, env, "-C", repo, "commit", "-q", "-m", "init")
	wt = filepath.Join(root, "wt")
	gitRun(tb, env, "-C", repo, "worktree", "add", "-q", "-b", "lab/conformance-1", wt)
	return wt, env
}

func gitRun(tb testing.TB, env []string, args ...string) string {
	tb.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// checkStatusClean returns the executable form of "ExcludeEntries cover every
// seeded path and nothing tracked was modified": a non-empty
// `git status --porcelain` after seeding is the violation, quoted verbatim.
func checkStatusClean(obligation string, tb testing.TB, wt string, env []string, meta provider.SeedMeta) []error {
	out := gitRun(tb, env, "-C", wt, "status", "--porcelain")
	if out == "" {
		return nil
	}
	return []error{fmt.Errorf("%s: git status --porcelain is not empty after seeding:\n%s\nSeedMeta().ExcludeEntries %q must cover every seeded path via .git/info/exclude, and seeding must never modify tracked files (D15 §9 measure 6; issue #51 decision 8; issue #80)", obligation, out, meta.ExcludeEntries)}
}

// walkWorktreeFiles lists wt's files as slash-separated worktree-relative
// paths, excluding the .git gitdir pointer/dir — the before/after probe the
// seeded-path coverage check diffs.
func walkWorktreeFiles(tb testing.TB, wt string) []string {
	tb.Helper()
	var files []string
	err := filepath.WalkDir(wt, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(wt, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("walking worktree %s: %v", wt, err)
	}
	return files
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// grepExit runs grep with args and input (one line) on stdin, returning
// grep's exit code: 0 match, 1 no match. Exit >= 2 — grep's "trouble" status,
// e.g. a pattern its BRE engine rejects — comes back as an error.
func grepExit(args []string, input string) (int, error) {
	cmd := exec.Command("grep", args...)
	cmd.Stdin = strings.NewReader(input + "\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return 1, nil
	}
	return -1, fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
}
