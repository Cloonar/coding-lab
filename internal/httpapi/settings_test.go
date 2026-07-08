package httpapi

// Settings surface suite (M5): typed GET, PATCH roundtrip, and the
// validation 400s (unknown keys, non-integers, floors, catalog-checked spawn
// defaults) — with the all-or-nothing write property.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// newSettingsServer: seeded settings + the fake provider registry (its
// catalogs back the spawn-default validation).
func newSettingsServer(t *testing.T) *testServer {
	t.Helper()
	x := newTestServer(t, func(o *Options) {
		if err := o.Store.SeedDefaultSettings(context.Background(), 6); err != nil {
			t.Fatal(err)
		}
		reg, err := provider.NewRegistry(providertest.New())
		if err != nil {
			t.Fatal(err)
		}
		o.Providers = reg
	})
	x.setup("op", "password123")
	return x
}

func settingsOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	m, ok := body["settings"].(map[string]any)
	if !ok {
		t.Fatalf("no settings object in %v", body)
	}
	return m
}

func TestAPI_SettingsGetTyped(t *testing.T) {
	x := newSettingsServer(t)

	resp := x.do("GET", "/api/v1/settings", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))

	// Integer knobs arrive as JSON numbers, strings as strings.
	if got[store.SettingMaxInstances] != float64(6) {
		t.Errorf("max_instances = %v (%T), want 6", got[store.SettingMaxInstances], got[store.SettingMaxInstances])
	}
	if got[store.SettingAFKBudgetMinutes] != float64(120) {
		t.Errorf("afk_budget_minutes = %v, want 120", got[store.SettingAFKBudgetMinutes])
	}
	if got[store.SettingSpawnModelDefault] != "opus[1m]" {
		t.Errorf("spawn_model_default = %v", got[store.SettingSpawnModelDefault])
	}
	if got[store.SettingGitAuthorName] != "" {
		t.Errorf("git_author_name = %v, want empty", got[store.SettingGitAuthorName])
	}
	// The read-only afk_prompt_default (issue #52 / ADR-0027) is injected into
	// every settings response: the built-in template with its <N> token literal.
	def, ok := got["afk_prompt_default"].(string)
	if !ok || def == "" {
		t.Errorf("afk_prompt_default = %v (%T), want the non-empty built-in template", got["afk_prompt_default"], got["afk_prompt_default"])
	}
	if !strings.Contains(def, "<N>") {
		t.Errorf("afk_prompt_default missing literal <N>:\n%s", def)
	}
}

// The AFK seed-prompt override (issue #52 / ADR-0027): a real value stores and
// echoes, whitespace-only normalizes to "" (inherit), an over-cap value is a 400,
// and the read-only afk_prompt_default is rejected on PATCH as an unknown key.
func TestAPI_SettingsAFKPromptOverride(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	// A real override stores and echoes verbatim.
	resp := x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingAFKPrompt: "Custom playbook: resolve <N> on <BRANCH>.",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if got[store.SettingAFKPrompt] != "Custom playbook: resolve <N> on <BRANCH>." {
		t.Errorf("afk_prompt = %v, want the stored override verbatim", got[store.SettingAFKPrompt])
	}
	if v, err := x.st.GetString(context.Background(), store.SettingAFKPrompt, ""); err != nil || v != "Custom playbook: resolve <N> on <BRANCH>." {
		t.Errorf("stored afk_prompt = %q (%v)", v, err)
	}

	// Whitespace-only normalizes to "" (inherit the built-in).
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingAFKPrompt: "   \n\t "}, h)
	wantStatus(t, resp, http.StatusOK)
	if got = settingsOf(t, decodeBody(t, resp)); got[store.SettingAFKPrompt] != "" {
		t.Errorf("whitespace-only afk_prompt = %q, want \"\" (inherit)", got[store.SettingAFKPrompt])
	}

	// Over the 16 KiB cap → 400.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingAFKPrompt: strings.Repeat("a", (16<<10)+1),
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if body := decodeBody(t, resp); body["error"] == "" {
		t.Error("over-cap afk_prompt 400 without error message")
	}

	// PATCHing the read-only default is an unknown-key 400.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{"afk_prompt_default": "x"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if body := decodeBody(t, resp); body["error"] == "" {
		t.Error("afk_prompt_default 400 without error message")
	}
}

func TestAPI_SettingsPatchRoundtrip(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	// Numbers, numeric strings, catalog values, and free strings all land.
	resp := x.do("PATCH", "/api/v1/settings", map[string]any{
		"afk_budget_minutes":  90,
		"afk_tick_seconds":    "15",
		"spawn_model_default": "sonnet",
		"git_author_name":     "Lab Bot",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if got["afk_budget_minutes"] != float64(90) || got["afk_tick_seconds"] != float64(15) {
		t.Errorf("patched intervals = %v / %v", got["afk_budget_minutes"], got["afk_tick_seconds"])
	}
	if got["spawn_model_default"] != "sonnet" || got["git_author_name"] != "Lab Bot" {
		t.Errorf("patched strings = %v / %v", got["spawn_model_default"], got["git_author_name"])
	}

	// Persisted where the runtime loops read them (typed accessor).
	if n, err := x.st.GetInt(context.Background(), store.SettingAFKBudgetMinutes, 0); err != nil || n != 90 {
		t.Errorf("stored afk_budget_minutes = %d (%v), want 90", n, err)
	}
	if n, err := x.st.GetInt(context.Background(), store.SettingAFKTickSeconds, 0); err != nil || n != 15 {
		t.Errorf("stored afk_tick_seconds = %d (%v), want 15", n, err)
	}

	// GET agrees.
	resp = x.do("GET", "/api/v1/settings", nil, nil)
	if got = settingsOf(t, decodeBody(t, resp)); got["spawn_model_default"] != "sonnet" {
		t.Errorf("GET after PATCH = %v", got["spawn_model_default"])
	}
}

func TestAPI_SettingsPatchValidation(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	bad := []struct {
		name string
		body map[string]any
	}{
		{"tick below 5s", map[string]any{"afk_tick_seconds": 3}},
		{"schedule below 5s", map[string]any{"afk_schedule_seconds": 4}},
		{"zero budget", map[string]any{"afk_budget_minutes": 0}},
		{"zero cap", map[string]any{"max_instances": 0}},
		{"non-integer", map[string]any{"max_instances": "abc"}},
		{"fractional", map[string]any{"afk_budget_minutes": 1.5}},
		{"unknown model", map[string]any{"spawn_model_default": "gpt-9"}},
		{"blank model", map[string]any{"spawn_model_default": ""}},
		{"unknown effort", map[string]any{"spawn_effort_default": "ultra"}},
		{"unknown key", map[string]any{"warp_factor": 9}},
		// AFK-override defaults (issue #19): a NON-empty value still validates
		// against the provider catalogs; the options bag validates keys + values.
		{"afk unknown model", map[string]any{"spawn_model_default_afk": "gpt-9"}},
		{"afk unknown effort", map[string]any{"spawn_effort_default_afk": "ultra"}},
		{"unknown spawn option key", map[string]any{"spawn_options_afk": map[string]any{"warp_drive": "true"}}},
		{"bad spawn option value", map[string]any{"spawn_options_afk": map[string]any{"ultracode": "maybe"}}},
		{"spawn options not an object", map[string]any{"spawn_options_afk": "nope"}},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			resp := x.do("PATCH", "/api/v1/settings", tt.body, h)
			wantStatus(t, resp, http.StatusBadRequest)
			if got := decodeBody(t, resp); got["error"] == "" {
				t.Error("400 without error message")
			}
		})
	}

	// All-or-nothing: one invalid entry rejects the whole PATCH — the valid
	// sibling must NOT have been written.
	resp := x.do("PATCH", "/api/v1/settings", map[string]any{
		"git_author_name":  "Half Applied",
		"afk_tick_seconds": 1,
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()
	if v, err := x.st.GetString(context.Background(), store.SettingGitAuthorName, ""); err != nil || v != "" {
		t.Errorf("git_author_name = %q (%v) after rejected PATCH, want empty", v, err)
	}

	// Nothing above changed the tick either.
	if n, err := x.st.GetInt(context.Background(), store.SettingAFKTickSeconds, 0); err != nil || n != 30 {
		t.Errorf("afk_tick_seconds = %d (%v), want the seeded 30", n, err)
	}
}

// The AFK-override defaults (issue #19 / ADR-0021): a catalog value lands, an
// EMPTY AFK model/effort is explicitly allowed (means inherit — unlike the base
// key), and the options bag validates + round-trips as canonical JSON.
func TestAPI_SettingsAFKDefaultsRoundtrip(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingSpawnModelDefaultAFK:  "sonnet",
		store.SettingSpawnEffortDefaultAFK: "", // inherit — allowed for the AFK override
		store.SettingSpawnOptionsAFK:       map[string]any{"ultracode": "true"},
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if got[store.SettingSpawnModelDefaultAFK] != "sonnet" {
		t.Errorf("spawn_model_default_afk = %v, want sonnet", got[store.SettingSpawnModelDefaultAFK])
	}
	if got[store.SettingSpawnEffortDefaultAFK] != "" {
		t.Errorf("spawn_effort_default_afk = %v, want empty (inherit)", got[store.SettingSpawnEffortDefaultAFK])
	}
	// The bag comes back as canonical JSON text.
	if got[store.SettingSpawnOptionsAFK] != `{"ultracode":"true"}` {
		t.Errorf("spawn_options_afk = %v, want the canonical bag", got[store.SettingSpawnOptionsAFK])
	}
	// Persisted where ResolveSpawnOptions reads it.
	if v, err := x.st.GetString(context.Background(), store.SettingSpawnOptionsAFK, ""); err != nil || v != `{"ultracode":"true"}` {
		t.Errorf("stored spawn_options_afk = %q (%v)", v, err)
	}

	// An empty options object is valid and round-trips.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingSpawnOptionsAFK: map[string]any{},
	}, h)
	wantStatus(t, resp, http.StatusOK)
	if got = settingsOf(t, decodeBody(t, resp)); got[store.SettingSpawnOptionsAFK] != `{}` {
		t.Errorf("empty spawn_options_afk = %v, want {}", got[store.SettingSpawnOptionsAFK])
	}
}
