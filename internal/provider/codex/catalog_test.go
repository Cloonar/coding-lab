package codex

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// fixturePath resolves the committed trimmed `codex debug models` capture
// from codex-cli 0.144.1 (the probe-schema source of truth).
func fixturePath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "models-0.144.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// newWithBin builds a hermetic Provider like testProvider but with an
// explicit codex binary, so catalog tests can point New's boot probe at a
// fakeCodex script (or a deliberately missing binary).
func newWithBin(t *testing.T, bin string) *Provider {
	t.Helper()
	p, err := New(Options{
		CodexBin:   bin,
		ConfigPath: filepath.Join(t.TempDir(), "config.toml"),
		LoginDir:   t.TempDir(),
		Runner:     newFakeRunner(),
		Bus:        events.NewBus(),
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// assertFallbackServed asserts the accessors serve exactly the compiled-in
// pinned fallback catalog — the contract for every probe-failure mode.
func assertFallbackServed(t *testing.T, p *Provider) {
	t.Helper()
	if got := p.Models(); !reflect.DeepEqual(got, provider.CloneModelOptions(fallbackModels)) {
		t.Errorf("Models() = %+v; want the compiled-in fallback %+v", got, fallbackModels)
	}
	if got := p.Efforts(); !slices.Equal(got, fallbackEfforts) {
		t.Errorf("Efforts() = %+v; want the compiled-in fallback %+v", got, fallbackEfforts)
	}
}

// The probe's parse/mapping contract on the committed 0.144.1 fixture:
// visibility filter, priority order, display-name labels, per-model efforts
// through the effort-label rule, defaults, and the first-seen union.
func TestProbeModels_fixtureMapping(t *testing.T) {
	script := fakeCodex(t, "cat '"+fixturePath(t)+"'")
	models, efforts, err := ProbeModels(context.Background(), script)
	if err != nil {
		t.Fatalf("ProbeModels: %v", err)
	}

	// Exactly the visibility=="list" entries, priority ascending — the
	// "hide" codex-auto-review is filtered (the probe's replacement for the
	// old hardcoded slug filter), labels come from display_name.
	wantOrder := []provider.Option{
		{Value: "gpt-5.6-terra", Label: "GPT-5.6-Terra"},
		{Value: "gpt-5.6-luna", Label: "GPT-5.6-Luna"},
		{Value: "gpt-5.5", Label: "GPT-5.5"},
		{Value: "gpt-5.4-mini", Label: "GPT-5.4-Mini"},
	}
	if len(models) != len(wantOrder) {
		t.Fatalf("ProbeModels returned %d models (%+v); want %d", len(models), models, len(wantOrder))
	}
	for i, want := range wantOrder {
		if models[i].Option != want {
			t.Errorf("models[%d] = %+v; want %+v (priority-ascending order)", i, models[i].Option, want)
		}
	}
	if provider.HasModelOption(models, "codex-auto-review") {
		t.Error("codex-auto-review (visibility \"hide\") survived the list filter")
	}

	// terra's six efforts in binary order, mapped through effortLabel — Max
	// and Ultra pin the title-case fallback for post-0.133 values.
	wantTerra := []provider.Option{
		{Value: "low", Label: "Low"},
		{Value: "medium", Label: "Medium"},
		{Value: "high", Label: "High"},
		{Value: "xhigh", Label: "Extra high"},
		{Value: "max", Label: "Max"},
		{Value: "ultra", Label: "Ultra"},
	}
	if !slices.Equal(models[0].Efforts, wantTerra) {
		t.Errorf("terra Efforts = %+v; want %+v", models[0].Efforts, wantTerra)
	}
	for _, m := range models {
		if m.DefaultEffort != "medium" {
			t.Errorf("model %q DefaultEffort = %q; want %q (fixture default_reasoning_level)", m.Value, m.DefaultEffort, "medium")
		}
	}

	// The union is first-seen order across the sorted models — terra (first)
	// already carries all six.
	if !slices.Equal(efforts, wantTerra) {
		t.Errorf("union efforts = %+v; want terra's six %+v", efforts, wantTerra)
	}
}

// Every probe-failure mode leaves New error-free and the accessors on the
// compiled-in fallback catalog.
func TestNew_probeFailureServesFallback(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		// The conformance suite constructs with a missing binary — the probe
		// must fail on the immediate exec lookup, never ride out the 10s
		// timeout budget.
		start := time.Now()
		p := newWithBin(t, filepath.Join(t.TempDir(), "codex-missing"))
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("New with a missing binary took %v; want an immediate exec lookup failure", elapsed)
		}
		assertFallbackServed(t, p)
	})
	for name, script := range map[string]string{
		"nonzero exit":    "exit 1",
		"empty model set": `echo '{"models": []}'`,
		"not json":        "echo 'not json'",
	} {
		t.Run(name, func(t *testing.T) {
			assertFallbackServed(t, newWithBin(t, fakeCodex(t, script)))
		})
	}
}

// A hung binary is cut off at probeTimeout and the fallback stays in place.
// The probe runs during New, so the shrunk-timeout re-run goes through
// probeCatalog directly (the method New calls).
func TestProbeCatalog_timeoutServesFallback(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner()) // missing binary → probe already failed fast
	assertFallbackServed(t, p)

	// exec so the kill at deadline reaps the sleeper itself — nothing keeps
	// the stdout pipe open past the timeout.
	p.codexBin = fakeCodex(t, "exec sleep 5")
	p.probeTimeout = 100 * time.Millisecond
	start := time.Now()
	p.probeCatalog()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("probeCatalog with a hung binary took %v; want ~probeTimeout (100ms)", elapsed)
	}
	assertFallbackServed(t, p)
}

// The success path through New: the probe result replaces the fallback in
// both accessors for the process lifetime.
func TestNew_probeSuccessServesProbedCatalog(t *testing.T) {
	script := fakeCodex(t, `case "$1" in
--version) echo "codex-cli 0.144.1";;
debug) cat '`+fixturePath(t)+`';;
esac`)
	p := newWithBin(t, script)

	models := p.Models()
	if len(models) != 4 || models[0].Value != "gpt-5.6-terra" {
		t.Fatalf("Models() = %+v; want the 4 probed models led by gpt-5.6-terra", models)
	}
	if !provider.HasOption(models[0].Efforts, "ultra") {
		t.Errorf("probed terra Efforts = %+v; want ultra present", models[0].Efforts)
	}
	efforts := p.Efforts()
	if len(efforts) != 6 || !provider.HasOption(efforts, "ultra") {
		t.Errorf("Efforts() = %+v; want the probed six-entry union including ultra", efforts)
	}
}

// A reported default_reasoning_level outside the model's own effort list is
// SANITIZED to "" (conformance pins membership; "" falls back to the
// first-entry rule downstream) — the model is served, not the fallback.
func TestParseDebugModels_sanitizesForeignDefault(t *testing.T) {
	models, _, err := parseDebugModels([]byte(`{"models":[{
		"slug":"gpt-x","display_name":"GPT-X","visibility":"list","priority":1,
		"default_reasoning_level":"turbo",
		"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`))
	if err != nil {
		t.Fatalf("parseDebugModels: %v", err)
	}
	if len(models) != 1 || models[0].Value != "gpt-x" {
		t.Fatalf("models = %+v; want the one listed model", models)
	}
	if models[0].DefaultEffort != "" {
		t.Errorf("DefaultEffort = %q; want \"\" (turbo is not in the model's own list)", models[0].DefaultEffort)
	}
}

// An empty display_name falls back to the slug — conformance requires
// non-empty labels.
func TestParseDebugModels_emptyDisplayNameFallsBackToSlug(t *testing.T) {
	models, _, err := parseDebugModels([]byte(`{"models":[{
		"slug":"gpt-x","visibility":"list","priority":1,
		"default_reasoning_level":"low",
		"supported_reasoning_levels":[{"effort":"low"}]}]}`))
	if err != nil {
		t.Fatalf("parseDebugModels: %v", err)
	}
	if models[0].Label != "gpt-x" {
		t.Errorf("Label = %q; want the slug fallback", models[0].Label)
	}
}

// Malformed catalogs are errors so the caller falls back — never a partial
// or silently-corrected catalog.
func TestParseDebugModels_malformedIsError(t *testing.T) {
	for name, doc := range map[string]string{
		"duplicate listed slugs": `{"models":[
			{"slug":"a","visibility":"list","priority":1,"supported_reasoning_levels":[{"effort":"low"}]},
			{"slug":"a","visibility":"list","priority":2,"supported_reasoning_levels":[{"effort":"low"}]}]}`,
		"empty slug": `{"models":[
			{"slug":"","visibility":"list","priority":1,"supported_reasoning_levels":[{"effort":"low"}]}]}`,
		"no reasoning levels": `{"models":[
			{"slug":"a","visibility":"list","priority":1,"supported_reasoning_levels":[]}]}`,
		"no listed models": `{"models":[
			{"slug":"a","visibility":"hide","priority":1,"supported_reasoning_levels":[{"effort":"low"}]}]}`,
		"empty reasoning level": `{"models":[
			{"slug":"a","visibility":"list","priority":1,"supported_reasoning_levels":[{"effort":""}]}]}`,
		"duplicate reasoning level": `{"models":[
			{"slug":"a","visibility":"list","priority":1,"supported_reasoning_levels":[{"effort":"low"},{"effort":"low"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseDebugModels([]byte(doc)); err == nil {
				t.Error("parseDebugModels succeeded; want a malformed-catalog error")
			}
		})
	}
}

// effortLabel is the one adapter-owned label rule: the four pinned 0.133.0
// values, then a first-rune title-case of anything newer.
func TestEffortLabel(t *testing.T) {
	for value, want := range map[string]string{
		"low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra high",
		"max": "Max", "ultra": "Ultra", "": "",
	} {
		if got := effortLabel(value); got != want {
			t.Errorf("effortLabel(%q) = %q; want %q", value, got, want)
		}
	}
}
