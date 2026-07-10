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
	models   []provider.Option
	efforts  []provider.Option
	options  []provider.OptionSpec
	authFlow provider.AuthFlow
	// aliasReturns makes SeedMeta/Models/Efforts/SpawnOptions return the
	// internal slices without cloning — the seedmeta-clone breakage.
	aliasReturns bool
	// spawnArgv overrides the default well-formed argv builder.
	spawnArgv func(spec provider.SpawnSpec) []string
	// seed overrides the default no-op SeedWorkspace.
	seed func(worktree string, opts provider.SeedOpts) error
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
		models:   []provider.Option{{Value: "mock-large", Label: "Mock Large"}, {Value: "mock-small", Label: "Mock Small"}},
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

func (m *mockProvider) Models() []provider.Option  { return retSlice(m.aliasReturns, m.models) }
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
func (m *mockProvider) SpawnArgv(spec provider.SpawnSpec) []string {
	if m.spawnArgv != nil {
		return m.spawnArgv(spec)
	}
	argv := []string{"mockagent", "--session", spec.SessionName}
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

func (m *mockProvider) AuthFlow() provider.AuthFlow { return m.authFlow }

func (m *mockProvider) SeedWorkspace(worktree string, opts provider.SeedOpts) error {
	if m.seed != nil {
		return m.seed(worktree, opts)
	}
	return nil
}

// Live-process seam methods: never called by the conformance suite (they need
// a real CLI); trivial stubs satisfy the interface.
func (m *mockProvider) AuthStatus(context.Context, bool) (provider.AuthStatus, error) {
	return provider.AuthStatus{}, nil
}
func (m *mockProvider) LoginStart(context.Context) (string, error)    { return "", nil }
func (m *mockProvider) LoginSubmitCode(context.Context, string) error { return nil }
func (m *mockProvider) Logout(context.Context) error                  { return nil }
func (m *mockProvider) Commands(context.Context, string) ([]provider.CommandSpec, error) {
	return nil, nil
}
func (m *mockProvider) LocateTranscript(context.Context, string, string) (string, error) {
	return "", nil
}
func (m *mockProvider) ReadTranscript(string) (provider.Chat, error) { return provider.Chat{}, nil }
func (m *mockProvider) Reply(context.Context, string, string) error  { return nil }
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
		{"auth-flow", func(t *testing.T) []error { return checkAuthFlow(p) }},
		{"login-session", func(t *testing.T) []error { return checkLoginSession(p) }},
		{"seeding-exclude-coverage", func(t *testing.T) []error { return checkSeedingExcludeCoverage(t, p) }},
		{"seeding-incogni", func(t *testing.T) []error { return checkSeedingIncogni(t, p) }},
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

func TestCheckAuthFlow_unknownKindFails(t *testing.T) {
	p := newMockProvider()
	p.authFlow = provider.AuthFlow{Kind: "carrier-pigeon"}
	wantError(t, checkAuthFlow(p), "auth-flow", `"carrier-pigeon"`)
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
