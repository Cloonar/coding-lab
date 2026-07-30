package instance

// Provider layering tests (issue #66): the three-level, skip-layer agent-CLI
// selection — per-spawn pick → repo override → global default (with the
// symmetric AFK overrides) — and the model/effort skip-layer against a second
// provider whose catalogs differ from the claude-shaped seeded defaults.

import (
	"context"
	"errors"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// newSecondProvider builds a registrable provider B: a different id, models
// the claude-shaped defaults do not contain, and an EMPTY efforts catalog (a
// provider without that knob — the effort flag must be omitted, never 400).
func newSecondProvider() *providertest.Fake {
	b := providertest.New()
	b.SetID("fake-b")
	b.SetCatalogs(
		[]provider.Option{{Value: "b-fast", Label: "B Fast"}, {Value: "b-deep", Label: "B Deep"}},
		[]provider.Option{},
	)
	return b
}

// newTwoProviderFixture is the standard multi-provider rig: the claude-code
// fake registered FIRST (the first-registered fallback), provider B second.
func newTwoProviderFixture(t *testing.T) (*fixture, *providertest.Fake) {
	t.Helper()
	b := newSecondProvider()
	f := newFixtureWith(t, fixtureOpts{extraProvs: []provider.AgentProvider{b}})
	return f, b
}

// ResolveProvider layers the effective provider per run kind (issue #66).
// Manual: request (STRICT) → repo.provider → global provider_default → first
// registered. AFK: repo.afk_provider_default → global spawn_provider_default_afk
// → repo.provider → global provider_default → first registered. Every default
// layer skips an id the registry does not carry. The fixture seeds
// provider_default="claude-code".
func TestResolveProvider_chains(t *testing.T) {
	f, _ := newTwoProviderFixture(t)
	ctx := context.Background()

	cases := []struct {
		name          string
		kind          string
		repo          store.Repo
		globalDefault string // "" keeps the seeded claude-code
		globalAFK     string
		req           string
		want          string
		wantErr       bool
	}{
		{
			name: "manual inherits the global default", kind: store.RunKindManual,
			want: "claude-code",
		},
		{
			name: "manual repo override wins over the global default", kind: store.RunKindManual,
			repo: store.Repo{Provider: strptr("fake-b")}, want: "fake-b",
		},
		{
			name: "manual per-spawn request wins over the repo override", kind: store.RunKindManual,
			repo: store.Repo{Provider: strptr("claude-code")}, req: "fake-b", want: "fake-b",
		},
		{
			name: "manual unknown request is strict", kind: store.RunKindManual,
			req: "ghost", wantErr: true,
		},
		{
			name: "manual unregistered repo override is skipped", kind: store.RunKindManual,
			repo: store.Repo{Provider: strptr("ghost")}, want: "claude-code",
		},
		{
			name: "unregistered global default falls back to the first registered", kind: store.RunKindManual,
			globalDefault: "ghost", want: "claude-code",
		},
		{
			name: "global default set to the second provider", kind: store.RunKindManual,
			globalDefault: "fake-b", want: "fake-b",
		},
		{
			name: "manual ignores the AFK overrides", kind: store.RunKindManual,
			repo: store.Repo{AFKProviderDefault: strptr("fake-b")}, globalAFK: "fake-b",
			want: "claude-code",
		},
		{
			name: "afk repo override wins first", kind: store.RunKindAFKManual,
			repo: store.Repo{Provider: strptr("claude-code"), AFKProviderDefault: strptr("fake-b")},
			want: "fake-b",
		},
		{
			name: "afk global override wins over the base chain", kind: store.RunKindAFKAuto,
			repo: store.Repo{Provider: strptr("claude-code")}, globalAFK: "fake-b",
			want: "fake-b",
		},
		{
			name: "afk unregistered override skips to the base chain", kind: store.RunKindAFKAuto,
			repo: store.Repo{Provider: strptr("fake-b"), AFKProviderDefault: strptr("ghost")},
			want: "fake-b",
		},
		{
			name: "afk with nothing set inherits the global default", kind: store.RunKindAFKManual,
			want: "claude-code",
		},
		{
			// A fix run is AFK-class (issue #182): unattended work continuing
			// an AFK claim, so the AFK-override layer applies to its fallback
			// resolution exactly as it does to the two AFK kinds.
			name: "fix consults the AFK overrides", kind: store.RunKindFix,
			repo: store.Repo{Provider: strptr("claude-code"), AFKProviderDefault: strptr("fake-b")},
			want: "fake-b",
		},
		{
			// Lander and escalate are validation-class (issues #181/#182):
			// never AFK, so the AFK overrides are invisible to them.
			name: "lander ignores the AFK overrides", kind: store.RunKindLander,
			repo: store.Repo{AFKProviderDefault: strptr("fake-b")}, globalAFK: "fake-b",
			want: "claude-code",
		},
		{
			name: "escalate ignores the AFK overrides", kind: store.RunKindEscalate,
			repo: store.Repo{AFKProviderDefault: strptr("fake-b")}, globalAFK: "fake-b",
			want: "claude-code",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			globalDefault := tc.globalDefault
			if globalDefault == "" {
				globalDefault = "claude-code"
			}
			setGlobal(t, f, store.SettingProviderDefault, globalDefault)
			setGlobal(t, f, store.SettingSpawnProviderDefaultAFK, tc.globalAFK)

			p, err := f.svc.ResolveProvider(ctx, tc.repo, tc.kind, tc.req)
			if tc.wantErr {
				var bad *BadRequestError
				if !errors.As(err, &bad) {
					t.Fatalf("err = %v, want *BadRequestError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveProvider: %v", err)
			}
			if p.ID() != tc.want {
				t.Errorf("resolved %q, want %q", p.ID(), tc.want)
			}
		})
	}
}

// THE issue-#66 scenario: two providers registered — claude-like A plus B
// with different models and an EMPTY efforts catalog — and the seeded global
// spawn_model_default is the claude-shaped opus[1m]. An AFK run on a repo
// whose provider is B must resolve B, skip the foreign global defaults, land
// on B's first model, resolve effort "" (the flag is omitted), and NEVER 400.
func TestResolveModelEffort_afkOnSecondProviderSkipsClaudeDefaults(t *testing.T) {
	f, b := newTwoProviderFixture(t)
	ctx := context.Background()

	repo := store.Repo{Provider: strptr("fake-b")}
	prov, err := f.svc.ResolveProvider(ctx, repo, store.RunKindAFKAuto, "")
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if prov.ID() != b.ID() {
		t.Fatalf("resolved %q, want %q", prov.ID(), b.ID())
	}

	model, effort, err := f.svc.ResolveModelEffort(ctx, prov, repo, store.RunKindAFKAuto, "", "")
	if err != nil {
		t.Fatalf("ResolveModelEffort on provider B: %v (the claude-shaped global default must fall through, not 400)", err)
	}
	if model != "b-fast" {
		t.Errorf("model = %q, want B's first catalog entry b-fast", model)
	}
	if effort != "" {
		t.Errorf("effort = %q, want \"\" for an empty efforts catalog (flag omitted)", effort)
	}
}

// An explicit request against an EMPTY catalog stays strict: any non-empty
// requested effort is a 400 on a provider without that knob.
func TestResolveModelEffort_explicitRequestAgainstEmptyCatalog(t *testing.T) {
	f, b := newTwoProviderFixture(t)

	_, _, err := f.svc.ResolveModelEffort(context.Background(), b, store.Repo{}, store.RunKindManual, "b-fast", "high")
	var bad *BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("err = %v, want *BadRequestError for an effort request against an empty catalog", err)
	}
}

// newPerModelProvider builds provider C (issue #156): PER-MODEL effort lists
// — "m-a" supports only low; "m-b" supports low+high and reports high as its
// default. The union Efforts() carries low+high, so a union-valid effort can
// still be unsupported on the resolved model.
func newPerModelProvider() *providertest.Fake {
	c := providertest.New()
	c.SetID("fake-c")
	low := provider.Option{Value: "low", Label: "low"}
	high := provider.Option{Value: "high", Label: "high"}
	c.SetCatalogs(nil, []provider.Option{low, high}) // the union; models come from SetModels
	c.SetModels([]provider.ModelOption{
		{Option: provider.Option{Value: "m-a", Label: "M A"}, Efforts: []provider.Option{low}},
		{Option: provider.Option{Value: "m-b", Label: "M B"}, Efforts: []provider.Option{low, high}, DefaultEffort: "high"},
	})
	return c
}

// The issue-#156 per-model effort resolution: the explicit effort validates
// against the RESOLVED MODEL's list (the unsupported-combo 400), stored
// defaults skip-layer against that same list, and the all-unset fallback is
// the model's reported default when it declares one, else the FIRST entry of
// the model's own list. The fixture's seeded global base (opus[1m]/max) is
// foreign to provider C and always falls through.
func TestResolveModelEffort_perModelEfforts(t *testing.T) {
	c := newPerModelProvider()
	f := newFixtureWith(t, fixtureOpts{extraProvs: []provider.AgentProvider{c}})
	ctx := context.Background()

	cases := []struct {
		name                  string
		kind                  string
		repo                  store.Repo
		globalBaseEffort      string // "" keeps the seeded foreign "max"
		reqModel, reqEffort   string
		wantModel, wantEffort string
		wantErr               bool
	}{
		{
			// THE new rejection: "high" is in the union (and in m-b's list)
			// but NOT in the resolved model m-a's list → 400, where pre-#156
			// union validation would have accepted the combo.
			name: "explicit effort in the union but not the resolved model's list is a bad request",
			kind: store.RunKindManual, reqModel: "m-a", reqEffort: "high", wantErr: true,
		},
		{
			// A stored effort default outside the resolved model's list is
			// SKIPPED (never a 400) — resolution falls to the model's own
			// fallback: m-a reports no default, so its first entry low.
			name: "stored effort default unsupported by the model skips to the model fallback",
			kind: store.RunKindManual, repo: store.Repo{ModelDefault: strptr("m-a")},
			globalBaseEffort: "high", wantModel: "m-a", wantEffort: "low",
		},
		{
			// The same stored default IS supported by m-b — it wins there,
			// proving the skip above is per-model, not per-provider.
			name: "stored effort default supported by the model wins",
			kind: store.RunKindManual, repo: store.Repo{ModelDefault: strptr("m-b")},
			globalBaseEffort: "high", wantModel: "m-b", wantEffort: "high",
		},
		{
			// All layers unset/foreign + a model with a reported default →
			// the reported default, NOT the first entry (m-b's list starts
			// with low).
			name: "all unset resolves the model's reported default effort",
			kind: store.RunKindManual, repo: store.Repo{ModelDefault: strptr("m-b")},
			wantModel: "m-b", wantEffort: "high",
		},
		{
			// All layers unset/foreign + a model WITHOUT a reported default →
			// the first entry of the MODEL's list (the pre-#156 rule).
			name: "all unset without a reported default falls to the model's first entry",
			kind: store.RunKindManual, repo: store.Repo{ModelDefault: strptr("m-a")},
			wantModel: "m-a", wantEffort: "low",
		},
		{
			// An explicit effort in the resolved model's list is accepted even
			// when the model itself came from a default layer.
			name: "explicit effort in the defaulted model's list is accepted",
			kind: store.RunKindManual, repo: store.Repo{ModelDefault: strptr("m-b")},
			reqEffort: "high", wantModel: "m-b", wantEffort: "high",
		},
		{
			// Model catalog first-entry fallback + per-model effort list: with
			// nothing stored at all, the model resolves to m-a and the effort
			// to m-a's only entry.
			name:      "everything unset resolves first model and its first effort",
			kind:      store.RunKindAFKAuto,
			wantModel: "m-a", wantEffort: "low",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The AFK overrides stay unset; the base globals stay the seeded
			// claude-shaped values unless the case pins an effort (both are
			// foreign to provider C and skip-layer).
			setGlobal(t, f, store.SettingSpawnModelDefaultAFK, "")
			setGlobal(t, f, store.SettingSpawnEffortDefaultAFK, "")
			baseEffort := tc.globalBaseEffort
			if baseEffort == "" {
				baseEffort = "max"
			}
			setGlobal(t, f, store.SettingSpawnEffortDefault, baseEffort)

			model, effort, err := f.svc.ResolveModelEffort(ctx, c, tc.repo, tc.kind, tc.reqModel, tc.reqEffort)
			if tc.wantErr {
				var bad *BadRequestError
				if !errors.As(err, &bad) {
					t.Fatalf("err = %v, want *BadRequestError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveModelEffort: %v", err)
			}
			if model != tc.wantModel || effort != tc.wantEffort {
				t.Errorf("= %q/%q, want %q/%q", model, effort, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// The scheduled kind joins the AFK side of the resolver (issue #247 /
// ADR-0062): the AFK override layers apply to RunKindScheduled exactly as
// they do to the two AFK kinds and fix.
func TestResolveProvider_scheduledConsultsAFKOverrides(t *testing.T) {
	f, _ := newTwoProviderFixture(t)
	setGlobal(t, f, store.SettingProviderDefault, "claude-code")
	setGlobal(t, f, store.SettingSpawnProviderDefaultAFK, "")

	repo := store.Repo{Provider: strptr("claude-code"), AFKProviderDefault: strptr("fake-b")}
	p, err := f.svc.ResolveProvider(context.Background(), repo, store.RunKindScheduled, "")
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if p.ID() != "fake-b" {
		t.Errorf("resolved %q, want fake-b (the AFK override layer must apply to the scheduled kind)", p.ID())
	}
}

// ResolveScheduleProvider layers the per-Schedule provider override as the
// TOPMOST skip-layer default rung (issue #247 / ADR-0062): a registered
// override wins over every AFK layer; a nil, empty, or stale one falls
// through to the plain AFK-kind chain — never an error, because a stored
// override a provider switch orphaned must degrade the firing, not break
// every firing forever (ADR-0030's rationale; the API's write-time check is
// where strictness lives).
func TestResolveScheduleProvider_overrideIsTopSkipLayerRung(t *testing.T) {
	f, _ := newTwoProviderFixture(t)
	ctx := context.Background()
	setGlobal(t, f, store.SettingProviderDefault, "claude-code")
	setGlobal(t, f, store.SettingSpawnProviderDefaultAFK, "")

	cases := []struct {
		name     string
		repo     store.Repo
		override *string
		want     string
	}{
		{name: "registered override wins over the AFK override layer",
			repo: store.Repo{AFKProviderDefault: strptr("claude-code")}, override: strptr("fake-b"), want: "fake-b"},
		{name: "nil override inherits the AFK chain",
			repo: store.Repo{AFKProviderDefault: strptr("fake-b")}, want: "fake-b"},
		{name: "empty override inherits the AFK chain",
			repo: store.Repo{AFKProviderDefault: strptr("fake-b")}, override: strptr(""), want: "fake-b"},
		{name: "stale override skip-layers to the AFK chain",
			repo: store.Repo{AFKProviderDefault: strptr("fake-b")}, override: strptr("ghost"), want: "fake-b"},
		{name: "stale override with nothing under it lands on the global default",
			override: strptr("ghost"), want: "claude-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := f.svc.ResolveScheduleProvider(ctx, tc.repo, tc.override)
			if err != nil {
				t.Fatalf("ResolveScheduleProvider: %v", err)
			}
			if p.ID() != tc.want {
				t.Errorf("resolved %q, want %q", p.ID(), tc.want)
			}
		})
	}
}

// ResolveScheduleModelEffort layers the per-Schedule model/effort overrides
// as the topmost skip-layer rung over the AFK chain (issue #247 / ADR-0062),
// under the issue-#156 per-model effort discipline: the effort override
// validates against the RESOLVED model's own list — including a model the
// override itself picked, and including the chain-resolved model when only
// the effort is overridden — and a stale value on either knob falls through
// to the inherited chain, never errors.
func TestResolveScheduleModelEffort_skipLayerOverrides(t *testing.T) {
	c := newPerModelProvider() // m-a: low only; m-b: low+high, default high
	f := newFixtureWith(t, fixtureOpts{extraProvs: []provider.AgentProvider{c}})
	ctx := context.Background()

	cases := []struct {
		name                  string
		repo                  store.Repo
		modelOv, effortOv     *string
		wantModel, wantEffort string
	}{
		{name: "both overrides win when present in the catalogs",
			modelOv: strptr("m-b"), effortOv: strptr("high"), wantModel: "m-b", wantEffort: "high"},
		{name: "stale model override skip-layers to the AFK chain",
			repo:    store.Repo{AFKModelDefault: strptr("m-b")},
			modelOv: strptr("ghost"), wantModel: "m-b", wantEffort: "high"},
		{name: "effort override without a model override layers over the resolved model",
			repo:     store.Repo{AFKModelDefault: strptr("m-b")},
			effortOv: strptr("low"), wantModel: "m-b", wantEffort: "low"},
		{name: "effort override foreign to the resolved model's list skip-layers",
			// m-a supports only low: the "high" override is in the provider
			// union but NOT in m-a's list, so it falls through to m-a's own
			// fallback (first entry, low) — never a 400.
			modelOv: strptr("m-a"), effortOv: strptr("high"), wantModel: "m-a", wantEffort: "low"},
		{name: "no overrides inherit the plain AFK-kind resolution",
			wantModel: "m-a", wantEffort: "low"},
		{name: "empty-string overrides inherit like nil",
			modelOv: strptr(""), effortOv: strptr(""), wantModel: "m-a", wantEffort: "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The seeded claude-shaped base globals are foreign to provider C
			// and skip-layer; the AFK globals stay unset.
			setGlobal(t, f, store.SettingSpawnModelDefaultAFK, "")
			setGlobal(t, f, store.SettingSpawnEffortDefaultAFK, "")

			model, effort, err := f.svc.ResolveScheduleModelEffort(ctx, c, tc.repo, tc.modelOv, tc.effortOv)
			if err != nil {
				t.Fatalf("ResolveScheduleModelEffort: %v (schedule overrides are skip-layer — never an error)", err)
			}
			if model != tc.wantModel || effort != tc.wantEffort {
				t.Errorf("= %q/%q, want %q/%q", model, effort, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// The manual Start path threads the per-spawn provider pick end to end
// (issue #66): the run row records the RESOLVED provider and the skip-layer
// model/effort, and the spawn argv is provider B's.
func TestStart_perSpawnProviderPick(t *testing.T) {
	f, b := newTwoProviderFixture(t)

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Provider: "fake-b"})
	if err != nil {
		t.Fatalf("Start with a provider pick: %v", err)
	}
	if run.Provider != "fake-b" {
		t.Errorf("run.Provider = %q, want fake-b (the resolved provider is stamped)", run.Provider)
	}
	if run.Model != "b-fast" || run.Effort != "" {
		t.Errorf("run model/effort = %q/%q, want b-fast/\"\" (skip-layer against B's catalogs)", run.Model, run.Effort)
	}
	specs := b.SpawnSpecs()
	if len(specs) != 1 {
		t.Fatalf("provider B SpawnArgv called %d times, want 1", len(specs))
	}
	if specs[0].Model != "b-fast" || specs[0].Effort != "" {
		t.Errorf("B's SpawnSpec = %+v, want Model=b-fast Effort=\"\"", specs[0])
	}

	// An unknown per-spawn pick is strict — 400, nothing spawned.
	_, err = f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Provider: "ghost"})
	var bad *BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("unknown provider pick err = %v, want *BadRequestError", err)
	}
}
