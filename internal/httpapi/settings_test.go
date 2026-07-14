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
		if err := o.Store.SeedDefaultSettings(context.Background(), 6, "claude-code"); err != nil {
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

// The dialog auto-dismiss window (issue #124) is deliberately NOT seeded, so
// its floor is 0 (not the >0 floors used by the AFK loop intervals): 0 itself
// (never auto-dismiss) and a positive value both PATCH and persist.
func TestAPI_SettingsDialogTimeoutRoundtrip(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingDialogTimeoutMinutes: 0,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if got[store.SettingDialogTimeoutMinutes] != float64(0) {
		t.Errorf("dialog_timeout_minutes = %v, want 0", got[store.SettingDialogTimeoutMinutes])
	}
	if n, err := x.st.GetInt(context.Background(), store.SettingDialogTimeoutMinutes, -1); err != nil || n != 0 {
		t.Errorf("stored dialog_timeout_minutes = %d (%v), want 0", n, err)
	}

	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingDialogTimeoutMinutes: 5,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got = settingsOf(t, decodeBody(t, resp))
	if got[store.SettingDialogTimeoutMinutes] != float64(5) {
		t.Errorf("dialog_timeout_minutes = %v, want 5", got[store.SettingDialogTimeoutMinutes])
	}
	if n, err := x.st.GetInt(context.Background(), store.SettingDialogTimeoutMinutes, -1); err != nil || n != 5 {
		t.Errorf("stored dialog_timeout_minutes = %d (%v), want 5", n, err)
	}

	// Below the floor is a 400 with the exact operator-facing message (floor 0,
	// unlike the AFK loop intervals' >0 floors).
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingDialogTimeoutMinutes: -1,
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if body := decodeBody(t, resp); body["error"] != "dialog_timeout_minutes must be at least 0" {
		t.Errorf("error = %v, want %q", body["error"], "dialog_timeout_minutes must be at least 0")
	}
}

// dialog_timeout_minutes is deliberately NOT seeded (issue #124): absent from
// a fresh GET, but typed as a JSON number once PATCHed.
func TestAPI_SettingsDialogTimeoutNotSeeded(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("GET", "/api/v1/settings", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if _, present := got[store.SettingDialogTimeoutMinutes]; present {
		t.Errorf("dialog_timeout_minutes present on a fresh seed = %v, want absent (not seeded)", got[store.SettingDialogTimeoutMinutes])
	}

	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingDialogTimeoutMinutes: 7,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got = settingsOf(t, decodeBody(t, resp))
	if got[store.SettingDialogTimeoutMinutes] != float64(7) {
		t.Errorf("dialog_timeout_minutes after PATCH = %v (%T), want JSON number 7", got[store.SettingDialogTimeoutMinutes], got[store.SettingDialogTimeoutMinutes])
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
		{"negative dialog timeout", map[string]any{"dialog_timeout_minutes": -1}},
		{"fractional dialog timeout", map[string]any{"dialog_timeout_minutes": 1.5}},
		{"non-integer dialog timeout", map[string]any{"dialog_timeout_minutes": "abc"}},
		{"unknown model", map[string]any{"spawn_model_default": "gpt-9"}},
		{"blank model", map[string]any{"spawn_model_default": ""}},
		{"unknown effort", map[string]any{"spawn_effort_default": "ultra"}},
		{"unknown key", map[string]any{"warp_factor": 9}},
		// AFK-override defaults (issue #19): a NON-empty value still validates
		// against the provider catalogs; the options bag validates keys + values.
		{"afk unknown model", map[string]any{"spawn_model_default_afk": "gpt-9"}},
		{"afk unknown effort", map[string]any{"spawn_effort_default_afk": "ultra"}},
		// The provider defaults (issue #66): the base key must always name a
		// registered provider (no lower operator layer to inherit from); the
		// AFK override allows "" (inherit) but rejects an unknown id.
		{"unknown provider", map[string]any{"provider_default": "ghost"}},
		{"blank provider", map[string]any{"provider_default": ""}},
		{"afk unknown provider", map[string]any{"spawn_provider_default_afk": "ghost"}},
		{"unknown spawn option key", map[string]any{"spawn_options_afk": map[string]any{"warp_drive": "true"}}},
		{"bad spawn option value", map[string]any{"spawn_options_afk": map[string]any{"ultracode": "maybe"}}},
		{"spawn options not an object", map[string]any{"spawn_options_afk": "nope"}},
		// The boolean keys (issue #163) take a JSON bool and NOTHING else — a
		// stringy "yes"/"true" or a 1/0 would leave "is it set?" ambiguous for a
		// knob whose whole design turns on telling false apart from unset.
		{"remote default as string", map[string]any{"spawn_remote_default": "yes"}},
		{"remote default as bool string", map[string]any{"spawn_remote_default": "true"}},
		{"remote default as number", map[string]any{"spawn_remote_default": 1}},
		// null is inherit — legal for the AFK override, but the base key has no
		// lower layer to inherit from, so null is a 400 there.
		{"remote default null", map[string]any{"spawn_remote_default": nil}},
		{"afk remote default as string", map[string]any{"spawn_remote_default_afk": "true"}},
		{"afk remote default as number", map[string]any{"spawn_remote_default_afk": 0}},
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

	// All-or-nothing holds for the boolean keys too (issue #163): a bad bool
	// must 400 BEFORE any SetSetting runs, so its valid sibling — and the seeded
	// remote default itself — are untouched.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		"git_author_email":     "half@applied.test",
		"spawn_remote_default": "yes",
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()
	if v, err := x.st.GetString(context.Background(), store.SettingGitAuthorEmail, ""); err != nil || v != "" {
		t.Errorf("git_author_email = %q (%v) after a PATCH rejected on the bool, want empty", v, err)
	}
	if v, err := x.st.GetBool(context.Background(), store.SettingSpawnRemoteDefault, true); err != nil || v {
		t.Errorf("spawn_remote_default = %v (%v) after the rejected PATCH, want the seeded false", v, err)
	}
}

// The remote-control spawn defaults (issue #163) are the FIRST boolean settings
// keys: the base renders as a real JSON bool (never the string "true"), and the
// AFK override is TRI-STATE on the wire — null (inherit) and false (an explicit
// off that beats a base true) are different answers, which is exactly what a
// boolean knob cannot express with a string sentinel.
func TestAPI_SettingsRemoteDefaultsRoundtrip(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	// The seeded base arrives as JSON false; the AFK override is NOT seeded, so
	// it is absent — absent = inherit.
	resp := x.do("GET", "/api/v1/settings", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if v, ok := got[store.SettingSpawnRemoteDefault].(bool); !ok || v {
		t.Errorf("seeded spawn_remote_default = %v (%T), want the JSON bool false",
			got[store.SettingSpawnRemoteDefault], got[store.SettingSpawnRemoteDefault])
	}
	if _, present := got[store.SettingSpawnRemoteDefaultAFK]; present {
		t.Errorf("spawn_remote_default_afk present on a fresh seed = %v, want absent (not seeded = inherit)",
			got[store.SettingSpawnRemoteDefaultAFK])
	}

	// PATCH true → a real bool comes back (not "true"), and it lands where
	// ResolveRemote reads it.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingSpawnRemoteDefault: true}, h)
	wantStatus(t, resp, http.StatusOK)
	got = settingsOf(t, decodeBody(t, resp))
	if v, ok := got[store.SettingSpawnRemoteDefault].(bool); !ok || !v {
		t.Errorf("patched spawn_remote_default = %v (%T), want the JSON bool true",
			got[store.SettingSpawnRemoteDefault], got[store.SettingSpawnRemoteDefault])
	}
	if v, err := x.st.GetBool(context.Background(), store.SettingSpawnRemoteDefault, false); err != nil || !v {
		t.Errorf("stored spawn_remote_default = %v (%v), want true", v, err)
	}
	resp = x.do("GET", "/api/v1/settings", nil, nil)
	if got = settingsOf(t, decodeBody(t, resp)); got[store.SettingSpawnRemoteDefault] != true {
		t.Errorf("GET after PATCH = %v (%T), want true", got[store.SettingSpawnRemoteDefault], got[store.SettingSpawnRemoteDefault])
	}

	// The AFK override PATCHed to FALSE is a stored value — an explicit "never
	// remote for unattended runs", which must NOT render as null.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingSpawnRemoteDefaultAFK: false}, h)
	wantStatus(t, resp, http.StatusOK)
	got = settingsOf(t, decodeBody(t, resp))
	v, present := got[store.SettingSpawnRemoteDefaultAFK]
	if !present {
		t.Fatal("spawn_remote_default_afk absent after PATCH false")
	}
	if v == nil {
		t.Error("spawn_remote_default_afk = null after PATCH false, want false (a boolean's false is a VALUE, not an absence)")
	}
	if b, ok := v.(bool); !ok || b {
		t.Errorf("spawn_remote_default_afk = %v (%T), want the JSON bool false", v, v)
	}
	if s, err := x.st.GetString(context.Background(), store.SettingSpawnRemoteDefaultAFK, ""); err != nil || s != "false" {
		t.Errorf("stored spawn_remote_default_afk = %q (%v), want %q", s, err, "false")
	}

	// PATCHed to null it renders as null and stores the blank row = inherit —
	// the state the false above is deliberately distinct from.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingSpawnRemoteDefaultAFK: nil}, h)
	wantStatus(t, resp, http.StatusOK)
	got = settingsOf(t, decodeBody(t, resp))
	v, present = got[store.SettingSpawnRemoteDefaultAFK]
	if !present || v != nil {
		t.Errorf("spawn_remote_default_afk = %v (present=%v) after PATCH null, want the key present and null (inherit)", v, present)
	}
	// Read the RAW row (GetString folds a blank value into its default, which is
	// precisely the inherit semantics ResolveRemote's settingBool relies on):
	// the row exists and holds "".
	if s, err := x.st.GetSetting(context.Background(), store.SettingSpawnRemoteDefaultAFK); err != nil || s != "" {
		t.Errorf("stored spawn_remote_default_afk = %q (%v), want the blank inherit row", s, err)
	}

	// And true round-trips on the override too.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingSpawnRemoteDefaultAFK: true}, h)
	wantStatus(t, resp, http.StatusOK)
	if got = settingsOf(t, decodeBody(t, resp)); got[store.SettingSpawnRemoteDefaultAFK] != true {
		t.Errorf("spawn_remote_default_afk = %v, want true", got[store.SettingSpawnRemoteDefaultAFK])
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

// The provider default keys (issue #66): a registered id lands on both keys,
// and the AFK override accepts "" (inherit the base chain).
func TestAPI_SettingsProviderDefaultsRoundtrip(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingProviderDefault:         "claude-code",
		store.SettingSpawnProviderDefaultAFK: "claude-code",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if got[store.SettingProviderDefault] != "claude-code" {
		t.Errorf("provider_default = %v, want claude-code", got[store.SettingProviderDefault])
	}
	if got[store.SettingSpawnProviderDefaultAFK] != "claude-code" {
		t.Errorf("spawn_provider_default_afk = %v, want claude-code", got[store.SettingSpawnProviderDefaultAFK])
	}

	// Clearing the AFK override back to inherit is allowed.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingSpawnProviderDefaultAFK: "",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	if got = settingsOf(t, decodeBody(t, resp)); got[store.SettingSpawnProviderDefaultAFK] != "" {
		t.Errorf("cleared spawn_provider_default_afk = %v, want empty (inherit)", got[store.SettingSpawnProviderDefaultAFK])
	}
}
