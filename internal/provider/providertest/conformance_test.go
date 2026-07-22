package providertest

// Self-tests of the Tier-1 conformance suite (issue #80): deliberately-broken
// minimal adapters must FAIL the specific check with an actionable,
// obligation-naming message, and a fully well-formed adapter must pass every
// check with zero errors (the anti-vacuous half — a suite with no green path
// "catches" broken adapters by failing everything).
//
// The mock here is purpose-built, NOT the package's Fake: Fake mirrors
// claude-code's recording contracts and deliberately does not apply its
// declared ultracode option to argv (its doc: that coupling belongs to the
// compat snapshot), so it cannot clear the spawn-argv options round-trip
// without changing its consumers' contracts.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// mockProvider is a minimal configurable AgentProvider. The zero-config
// newMockProvider() is fully conformant with neutral "mockagent" shapes; each
// broken-adapter test overrides exactly one facet.
type mockProvider struct {
	id       string
	meta     provider.SeedMeta
	models   []provider.ModelOption
	efforts  []provider.Option
	options  []provider.OptionSpec
	authFlow provider.AuthFlow
	// aliasReturns makes SeedMeta/Models/Efforts/SpawnOptions return the
	// internal slices without cloning — the seedmeta-clone breakage.
	aliasReturns bool
	// aliasNestedEfforts makes Models() clone only the OUTER slice, leaving
	// each entry's Efforts aliased — the issue-#156 nested-clone breakage a
	// shallow slices.Clone discipline would ship.
	aliasNestedEfforts bool
	// spawnArgv overrides the default well-formed argv builder.
	spawnArgv func(spec provider.SpawnSpec) []string
	// seed overrides the default no-op SeedWorkspace.
	seed func(worktree string, opts provider.SeedOpts) error
	// readChat overrides the default conforming ReadChat — the read-chat
	// breakage hook (issue #92).
	readChat func(transcriptPath string) (provider.Chat, error)
	// inject overrides the default conforming InjectCredentials — the
	// inject-credentials breakage hook (issue #202).
	inject func(instanceHome string) ([]string, error)
	// locate overrides the default conforming LocateTranscript — the
	// locate-homeless breakage hook (issue #202).
	locate func(home string) (string, error)
}

var _ provider.AgentProvider = (*mockProvider)(nil)

func newMockProvider() *mockProvider {
	return &mockProvider{
		id: "mockagent",
		meta: provider.SeedMeta{
			ContextFileName:      "MOCKAGENT.local.md",
			SkillsDir:            ".mockagent/skills",
			NativeSkillDiscovery: true,
			ExcludeEntries:       []string{".mockagent/", "MOCKAGENT.local.md"},
			SeededPathPatterns: []string{
				`^\.mockagent/skills/`,
				`^MOCKAGENT\.local\.md$`,
			},
			ScrubPatterns: []string{
				`co-authored-by:[[:space:]]*mockagent`,
				`mockagent-session:`,
			},
		},
		// Enriched per issue #156: mock-large carries both efforts and
		// reports high as its default (the codex shape); mock-small supports
		// only low and reports none (the first-entry-fallback shape). The
		// union below covers both lists.
		models: []provider.ModelOption{
			{Option: provider.Option{Value: "mock-large", Label: "Mock Large"}, Efforts: []provider.Option{{Value: "low", Label: "low"}, {Value: "high", Label: "high"}}, DefaultEffort: "high"},
			{Option: provider.Option{Value: "mock-small", Label: "Mock Small"}, Efforts: []provider.Option{{Value: "low", Label: "low"}}},
		},
		efforts:  []provider.Option{{Value: "low", Label: "low"}, {Value: "high", Label: "high"}},
		options:  []provider.OptionSpec{{Key: "turbo", Label: "Turbo", Type: provider.OptionTypeBool, Default: "false"}},
		authFlow: provider.AuthFlow{Kind: provider.AuthFlowExternal},
	}
}

// mockFixture returns the mock's ground-truth marker vectors: lines its
// declared patterns must catch, and near-miss lines they must not.
func mockFixture() Fixture {
	return Fixture{
		AttributionSamples: []string{
			"Co-Authored-By: MockAgent <bot@mockagent.invalid>",
			"MockAgent-Session: https://mockagent.invalid/s/1",
		},
		CleanSamples: []string{
			"Co-Authored-By: Alice <alice@example.com>",
			"Docs generated with pandoc.",
		},
	}
}

// retSlice returns s aliased or cloned, per the mock's breakage flag.
func retSlice[T any](alias bool, s []T) []T {
	if alias {
		return s
	}
	return slices.Clone(s)
}

func (m *mockProvider) ID() string          { return m.id }
func (m *mockProvider) DisplayName() string { return "Mock Agent" }

// Models returns the enriched catalog deep-cloned (the conforming shape), or
// with the scripted aliasing breakages: aliasReturns hands out the internal
// slice itself; aliasNestedEfforts clones only the outer slice (issue #156).
func (m *mockProvider) Models() []provider.ModelOption {
	switch {
	case m.aliasReturns:
		return m.models
	case m.aliasNestedEfforts:
		return slices.Clone(m.models)
	}
	return provider.CloneModelOptions(m.models)
}
func (m *mockProvider) Efforts() []provider.Option { return retSlice(m.aliasReturns, m.efforts) }
func (m *mockProvider) SpawnOptions() []provider.OptionSpec {
	return retSlice(m.aliasReturns, m.options)
}

func (m *mockProvider) SeedMeta() provider.SeedMeta {
	meta := m.meta
	meta.ExcludeEntries = retSlice(m.aliasReturns, meta.ExcludeEntries)
	meta.SeededPathPatterns = retSlice(m.aliasReturns, meta.SeededPathPatterns)
	meta.ScrubPatterns = retSlice(m.aliasReturns, meta.ScrubPatterns)
	return meta
}

// SpawnArgv mirrors the pinned argv contract: flags first, the prompt as the
// trailing positional, and the turbo option applied prompt-scoped (like
// claude-code's ultracode) so the options round-trip has a real knob to see.
// The default builder IGNORES spec.Remote — the plain mock advertises no
// RemoteCapable, so its argv must be identical for both values (issue #163's
// receive-and-ignore half, the codex shape).
func (m *mockProvider) SpawnArgv(spec provider.SpawnSpec) []string {
	if m.spawnArgv != nil {
		return m.spawnArgv(spec)
	}
	return mockArgv(spec, false)
}

// mockArgv is the mock's conforming argv shape, with remote deciding whether
// the session flag pair (the flag AND its argument, claude's shape) is emitted
// at all. Both spawn-remote branches are built from it: remote=false is the
// ignoring provider, remote=spec.Remote the honoring one.
func mockArgv(spec provider.SpawnSpec, remote bool) []string {
	argv := []string{"mockagent"}
	if remote {
		argv = append(argv, "--session", spec.SessionName)
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "--effort", spec.Effort)
	}
	prompt := spec.InitialPrompt
	if prompt != "" && spec.Options["turbo"] == "true" {
		prompt = "turbo mode\n\n" + prompt
	}
	if prompt != "" {
		argv = append(argv, prompt)
	}
	return argv
}

// honorRemote is the conforming remote-honoring argv builder (the spawnArgv
// override a RemoteCapable adapter must ship).
func honorRemote(spec provider.SpawnSpec) []string { return mockArgv(spec, spec.Remote) }

// remoteMock is a mockProvider that ADVERTISES provider.RemoteCapable (issue
// #163) — the plain mock deliberately does not, so the capability half of
// spawn-remote needs this wrapper. Whether it HONORS the knob depends on the
// wrapped mock's argv builder, which is exactly the mismatch spawn-remote
// exists to catch.
type remoteMock struct{ *mockProvider }

var (
	_ provider.AgentProvider = remoteMock{}
	_ provider.RemoteCapable = remoteMock{}
)

func (remoteMock) SupportsRemoteControl() bool { return true }

// newRemoteMock returns a CONFORMING remote-capable mock: it advertises the
// capability and honors spec.Remote in argv.
func newRemoteMock() remoteMock {
	m := newMockProvider()
	m.spawnArgv = honorRemote
	return remoteMock{m}
}

func (m *mockProvider) AuthFlow() provider.AuthFlow { return m.authFlow }

func (m *mockProvider) SeedWorkspace(worktree string, opts provider.SeedOpts) error {
	if m.seed != nil {
		return m.seed(worktree, opts)
	}
	return nil
}

// Live-process seam methods: never called by the conformance suite (they need
// a real CLI); trivial stubs satisfy the interface. ReadChat below is the one
// exception — its ARGUMENT contract is hermetic seam law (issue #92), so the
// mock implements it for real.
func (m *mockProvider) AuthStatus(context.Context, bool) (provider.AuthStatus, error) {
	return provider.AuthStatus{}, nil
}
func (m *mockProvider) LoginStart(context.Context) (string, error)    { return "", nil }
func (m *mockProvider) LoginSubmitCode(context.Context, string) error { return nil }
func (m *mockProvider) Logout(context.Context) error                  { return nil }
func (m *mockProvider) Commands(context.Context, string, string) ([]provider.CommandSpec, error) {
	return nil, nil
}

// LocateTranscript's default is conforming per the locate-homeless obligation
// (issue #202): an empty instance home is a miss ("", nil), never an error and
// never a fall back to a master store the mock does not have.
func (m *mockProvider) LocateTranscript(_ context.Context, _, _, home string) (string, error) {
	if m.locate != nil {
		return m.locate(home)
	}
	return "", nil
}

// InjectCredentials's default is conforming per the inject-credentials
// obligation (issue #202): an empty home is a programmer error; a non-empty
// home writes nothing (the mock has no master store) and returns no env.
func (m *mockProvider) InjectCredentials(instanceHome string) ([]string, error) {
	if m.inject != nil {
		return m.inject(instanceHome)
	}
	if instanceHome == "" {
		return nil, errors.New("mockagent: InjectCredentials requires a non-empty instance HOME")
	}
	return nil, nil
}

// ReadChat's default is conforming per the read-chat obligation (issue #92):
// an empty transcriptPath is the idle pre-transcript read of an active run,
// and a vanished non-empty path surfaces the ErrTranscriptGone sentinel via
// the same os-stat route a real adapter's open takes. runID/runtimeDir are
// ignored — the mock has no live-signal channel, the honest-degradation shape.
func (m *mockProvider) ReadChat(_, _, transcriptPath string) (provider.Chat, error) {
	if m.readChat != nil {
		return m.readChat(transcriptPath)
	}
	if transcriptPath == "" {
		return provider.Chat{State: provider.StateIdle}, nil
	}
	if _, err := os.Stat(transcriptPath); os.IsNotExist(err) {
		return provider.Chat{}, provider.ErrTranscriptGone
	}
	return provider.Chat{State: provider.StateIdle}, nil
}
func (m *mockProvider) Reply(context.Context, string, string) error { return nil }
func (m *mockProvider) AnswerDialog(context.Context, string, provider.Dialog, provider.DialogAnswer) error {
	return nil
}
func (m *mockProvider) Interrupt(context.Context, string) error { return nil }

// wantError asserts errs carries at least one error whose message contains
// EVERY phrase — the "actionable, obligation-naming message" acceptance of
// issue #80.
func wantError(t *testing.T, errs []error, phrases ...string) {
	t.Helper()
	for _, err := range errs {
		msg := err.Error()
		ok := true
		for _, ph := range phrases {
			if !strings.Contains(msg, ph) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Errorf("no error carries all of %q; got %d error(s):\n%s", phrases, len(errs), joinErrs(errs))
}

func joinErrs(errs []error) string {
	var b strings.Builder
	for _, err := range errs {
		b.WriteString("  " + err.Error() + "\n")
	}
	return b.String()
}

// --- green path -------------------------------------------------------------

// Every check function returns zero errors for the fully well-formed mock.
func TestCheckFunctions_wellFormedProviderPasses(t *testing.T) {
	p := newMockProvider()
	checks := []struct {
		name string
		run  func(t *testing.T) []error
	}{
		{"patterns-dialect", func(t *testing.T) []error { return checkPatternsDialect(p.SeedMeta()) }},
		{"context-file-lab-owned", func(t *testing.T) []error { return checkContextFileName(p.SeedMeta()) }},
		{"seedmeta-clone", func(t *testing.T) []error { return checkSeedMetaClone(p) }},
		{"catalogs", func(t *testing.T) []error { return checkCatalogs(p) }},
		{"spawn-argv", func(t *testing.T) []error { return checkSpawnArgv(p) }},
		{"spawn-remote", func(t *testing.T) []error { return checkSpawnRemote(p) }},
		{"auth-flow", func(t *testing.T) []error { return checkAuthFlow(p) }},
		{"login-session", func(t *testing.T) []error { return checkLoginSession(p) }},
		{"read-chat", func(t *testing.T) []error { return checkReadChat(t, p) }},
		{"locate-homeless", func(t *testing.T) []error { return checkLocateHomeless(p) }},
		{"inject-credentials", func(t *testing.T) []error { return checkInjectCredentials(t, p) }},
		{"seeding-exclude-coverage", func(t *testing.T) []error { return checkSeedingExcludeCoverage(t, p) }},
		{"seeding-incogni", func(t *testing.T) []error { return checkSeedingIncogni(t, p) }},
		{"seed-home-containment", func(t *testing.T) []error { return checkSeedHomeContainment(t, p) }},
		{"scrub-markers", func(t *testing.T) []error { return checkScrubMarkers(t, p, mockFixture()) }},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if errs := c.run(t); len(errs) > 0 {
				t.Errorf("well-formed provider failed %s:\n%s", c.name, joinErrs(errs))
			}
		})
	}
}

// The *testing.T front-end passes the well-formed mock end to end.
func TestConformance_wellFormedProvider(t *testing.T) {
	Conformance(t, newMockProvider(), mockFixture())
}

// spawn-remote has TWO green paths (issue #163), and the suite must pass both:
// the plain mock above is the ignoring provider (no RemoteCapable, identical
// argv), this one the honoring provider (advertises the capability, argv
// differs). A suite that only ever saw one of them would pass an adapter that
// picked the wrong half.
func TestConformance_wellFormedRemoteCapableProvider(t *testing.T) {
	Conformance(t, newRemoteMock(), mockFixture())
}

// --- broken adapters ---------------------------------------------------------

func TestCheckPatternsDialect_invalidRE2PatternIsNamed(t *testing.T) {
	p := newMockProvider()
	p.meta.ScrubPatterns = append(p.meta.ScrubPatterns, `co-authored-by:(?`)
	wantError(t, checkPatternsDialect(p.SeedMeta()), "patterns-dialect", `co-authored-by:(?`)
}

func TestCheckPatternsDialect_emptyScrubPatternsFail(t *testing.T) {
	p := newMockProvider()
	p.meta.ScrubPatterns = nil
	wantError(t, checkPatternsDialect(p.SeedMeta()), "patterns-dialect", "empty", "issue #80")
}

func TestCheckContextFileName_trackedNameFails(t *testing.T) {
	p := newMockProvider()
	p.meta.ContextFileName = "AGENTS.md"
	wantError(t, checkContextFileName(p.SeedMeta()), "context-file-lab-owned", `"AGENTS.md"`, "not lab-owned", "ADR-0035")
}

func TestCheckContextFileName_pathFails(t *testing.T) {
	p := newMockProvider()
	p.meta.ContextFileName = "docs/MOCKAGENT.local.md"
	wantError(t, checkContextFileName(p.SeedMeta()), "context-file-lab-owned", "path separator")
}

func TestCheckSeedMetaClone_aliasedSlicesFail(t *testing.T) {
	p := newMockProvider()
	p.aliasReturns = true
	errs := checkSeedMetaClone(p)
	wantError(t, errs, "seedmeta-clone", "SeedMeta().ScrubPatterns")
	wantError(t, errs, "seedmeta-clone", "Models()")
	wantError(t, errs, "seedmeta-clone", "Efforts()")
	wantError(t, errs, "seedmeta-clone", "SpawnOptions()")
}

func TestCheckCatalogs_malformedCatalogsFail(t *testing.T) {
	p := newMockProvider()
	p.models = nil
	p.efforts = []provider.Option{{Value: "high", Label: "high"}, {Value: "high", Label: "again"}}
	p.options = []provider.OptionSpec{{Key: "turbo", Label: "Turbo", Type: provider.OptionTypeBool, Default: "yes"}}
	errs := checkCatalogs(p)
	wantError(t, errs, "catalogs", "Models()", "empty")
	wantError(t, errs, "catalogs", "Efforts()", `"high"`, "duplicates")
	wantError(t, errs, "catalogs", `"turbo"`, `"yes"`)
}

// The issue-#156 per-model obligations: an empty per-model effort list, a
// reported default outside the model's own list, and a per-model effort the
// union Efforts() does not cover each fail with the model named.
func TestCheckCatalogs_perModelEffortViolationsFail(t *testing.T) {
	p := newMockProvider()
	p.models = []provider.ModelOption{
		// Empty per-model efforts.
		{Option: provider.Option{Value: "mock-large", Label: "Mock Large"}},
		// DefaultEffort outside the model's own list (it IS in the union).
		{Option: provider.Option{Value: "mock-small", Label: "Mock Small"}, Efforts: []provider.Option{{Value: "low", Label: "low"}}, DefaultEffort: "high"},
		// A per-model effort value the union does not carry.
		{Option: provider.Option{Value: "mock-mini", Label: "Mock Mini"}, Efforts: []provider.Option{{Value: "turbo", Label: "turbo"}}},
	}
	errs := checkCatalogs(p)
	wantError(t, errs, "catalogs", `"mock-large"`, "Efforts", "empty")
	wantError(t, errs, "catalogs", `"mock-small"`, "DefaultEffort", `"high"`, "issue #156")
	wantError(t, errs, "catalogs", `"mock-mini"`, `"turbo"`, "union Efforts()", "issue #156")
}

// Nested per-model Efforts aliasing (issue #156): an outer-only clone leaves
// the effort lists shared, and the extended seedmeta-clone probe catches it.
func TestCheckSeedMetaClone_nestedEffortsAliasingFails(t *testing.T) {
	p := newMockProvider()
	p.aliasNestedEfforts = true
	wantError(t, checkSeedMetaClone(p), "seedmeta-clone", "Models() per-model Efforts")
}

func TestCheckSpawnArgv_promptNotTrailingFails(t *testing.T) {
	p := newMockProvider()
	p.spawnArgv = func(spec provider.SpawnSpec) []string {
		argv := []string{"mockagent", "--session", spec.SessionName}
		if spec.InitialPrompt != "" {
			// The breakage: a flag AFTER the prompt, so the prompt is no
			// longer the trailing positional.
			argv = append(argv, spec.InitialPrompt, "--prompt-mode=seed")
		}
		return argv
	}
	wantError(t, checkSpawnArgv(p), "spawn-argv", "trailing positional")
}

// The advertise-without-honoring breakage (issue #163): the adapter implements
// RemoteCapable — so lab lights up the toggle and arms deep-link capture for it
// — while its argv drops spec.Remote on the floor. The default mock builder IS
// that breakage; only the capability wrapper is added.
func TestCheckSpawnRemote_advertisedButArgvIdenticalFails(t *testing.T) {
	p := remoteMock{newMockProvider()}
	wantError(t, checkSpawnRemote(p), "spawn-remote", "advertises provider.RemoteCapable", "identical", "issue #163")
}

// The honor-without-advertising breakage (issue #163): the adapter acts on
// spec.Remote but implements no RemoteCapable, so lab would report the knob
// unsupported and disable a toggle its CLI actually obeys.
func TestCheckSpawnRemote_unadvertisedButArgvDiffersFails(t *testing.T) {
	p := newMockProvider()
	p.spawnArgv = honorRemote
	wantError(t, checkSpawnRemote(p), "spawn-remote", "does not implement provider.RemoteCapable", "issue #163")
}

func TestCheckAuthFlow_unknownKindFails(t *testing.T) {
	p := newMockProvider()
	p.authFlow = provider.AuthFlow{Kind: "carrier-pigeon"}
	wantError(t, checkAuthFlow(p), "auth-flow", `"carrier-pigeon"`)
}

// device-code (issue #87) is part of the declared vocabulary: a well-formed
// adapter declaring it clears the auth-flow check.
func TestCheckAuthFlow_deviceCodeKindPasses(t *testing.T) {
	p := newMockProvider()
	p.authFlow = provider.AuthFlow{Kind: provider.AuthFlowDeviceCode}
	if errs := checkAuthFlow(p); len(errs) > 0 {
		t.Errorf("device-code adapter failed auth-flow:\n%s", joinErrs(errs))
	}
}

func TestCheckSeedingExcludeCoverage_uncoveredSeededFileListed(t *testing.T) {
	p := newMockProvider()
	p.seed = func(worktree string, _ provider.SeedOpts) error {
		// The breakage: seed a state file the declared ExcludeEntries do not
		// cover (and no SeededPathPattern describes).
		dir := filepath.Join(worktree, ".myagent")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}\n"), 0o644)
	}
	errs := checkSeedingExcludeCoverage(t, p)
	wantError(t, errs, "seeding-exclude-coverage", ".myagent", "ExcludeEntries")
	wantError(t, errs, "seeding-exclude-coverage", ".myagent/state.json", "SeededPathPattern")
}

func TestCheckScrubMarkers_unmatchedSampleFails(t *testing.T) {
	p := newMockProvider()
	fx := mockFixture()
	fx.AttributionSamples = append(fx.AttributionSamples, "Co-Authored-By: OtherBot <bot@other.invalid>")
	wantError(t, checkScrubMarkers(t, p, fx), "scrub-markers", "OtherBot")
}

func TestCheckScrubMarkers_overBroadPatternFails(t *testing.T) {
	p := newMockProvider()
	// The breakage: a pattern so broad it also catches human co-authors.
	p.meta.ScrubPatterns = append(p.meta.ScrubPatterns, `co-authored-by:`)
	wantError(t, checkScrubMarkers(t, p, mockFixture()), "scrub-markers", "Alice", "over-broad")
}

func TestCheckReadChat_emptyPathErrorFails(t *testing.T) {
	p := newMockProvider()
	p.readChat = func(transcriptPath string) (provider.Chat, error) {
		if transcriptPath == "" {
			// The breakage: treating the pre-transcript read of an active run
			// as a failure instead of the idle empty chat (issue #92).
			return provider.Chat{}, errors.New("no transcript located yet")
		}
		return provider.Chat{}, provider.ErrTranscriptGone
	}
	wantError(t, checkReadChat(t, p), "read-chat", "never an error")
}

func TestCheckReadChat_rawGoneErrorFails(t *testing.T) {
	p := newMockProvider()
	p.readChat = func(transcriptPath string) (provider.Chat, error) {
		if transcriptPath == "" {
			return provider.Chat{State: provider.StateIdle}, nil
		}
		// The breakage: a raw os-flavored error instead of the
		// ErrTranscriptGone sentinel httpapi maps to the graceful state.
		return provider.Chat{}, errors.New("open " + transcriptPath + ": no such file or directory")
	}
	wantError(t, checkReadChat(t, p), "read-chat", "ErrTranscriptGone")
}

func TestCheckReadChat_nilErrorForVanishedPathFails(t *testing.T) {
	p := newMockProvider()
	p.readChat = func(transcriptPath string) (provider.Chat, error) {
		// The breakage: every read "succeeds" — a vanished transcript would
		// render as an eternally empty idle chat instead of the gone state.
		return provider.Chat{State: provider.StateIdle}, nil
	}
	wantError(t, checkReadChat(t, p), "read-chat", "ErrTranscriptGone")
}

// The locate-homeless breakage (issue #202): an adapter that resolves a
// transcript for an empty instance HOME has fallen back to a master store
// instead of treating "no per-run home" as a miss.
func TestCheckLocateHomeless_nonEmptyForHomelessFails(t *testing.T) {
	p := newMockProvider()
	p.locate = func(home string) (string, error) {
		if home == "" {
			return "/master/store/transcript.jsonl", nil // the fallback that must not happen
		}
		return "", nil
	}
	wantError(t, checkLocateHomeless(p), "locate-homeless", "master store")
}

// The inject-credentials empty-HOME breakage (issue #202): treating "" as valid
// instead of a programmer error.
func TestCheckInjectCredentials_emptyHomeNotErroringFails(t *testing.T) {
	p := newMockProvider()
	p.inject = func(string) ([]string, error) { return nil, nil }
	wantError(t, checkInjectCredentials(t, p), "inject-credentials", "empty instance HOME", "programmer error")
}

// The inject-credentials missing-master breakage (issue #202): failing when the
// master credential is absent, instead of proceeding so the CLI shows its own
// login prompt.
func TestCheckInjectCredentials_missingMasterErroringFails(t *testing.T) {
	p := newMockProvider()
	p.inject = func(home string) ([]string, error) {
		if home == "" {
			return nil, errors.New("empty instance HOME")
		}
		return nil, errors.New("no master credential present")
	}
	wantError(t, checkInjectCredentials(t, p), "inject-credentials", "missing master credential is NOT an error")
}

// The seed-home-containment breakage (issue #202): a HOME-global grant that
// writes to the ENV-derived master store instead of under the run's Home.
func TestCheckSeedHomeContainment_masterStoreWriteFails(t *testing.T) {
	p := newMockProvider()
	p.seed = func(worktree string, opts provider.SeedOpts) error {
		// The breakage: seed trust into the master store (CLAUDE_CONFIG_DIR),
		// which the check has pointed at a fresh empty temp dir.
		dir := os.Getenv("CLAUDE_CONFIG_DIR")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("{}\n"), 0o600)
	}
	wantError(t, checkSeedHomeContainment(t, p), "seed-home-containment", "master store")
}
