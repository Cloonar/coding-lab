package httpapi

// Settings surface suite (M5): typed GET, PATCH roundtrip, and the
// validation 400s (unknown keys, non-integers, floors, catalog-checked spawn
// defaults) — with the all-or-nothing write property.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/imageref"
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
		{"null dialog timeout", map[string]any{"dialog_timeout_minutes": nil}},
		{"unknown model", map[string]any{"spawn_model_default": "gpt-9"}},
		{"blank model", map[string]any{"spawn_model_default": ""}},
		{"unknown effort", map[string]any{"spawn_effort_default": "ultra"}},
		{"unknown key", map[string]any{"warp_factor": 9}},
		// AFK-override defaults (issue #19): a NON-empty value still validates
		// against the provider catalogs; the options bag validates keys + values.
		{"afk unknown model", map[string]any{"spawn_model_default_afk": "gpt-9"}},
		{"afk unknown effort", map[string]any{"spawn_effort_default_afk": "ultra"}},
		// The lander-override defaults follow the AFK pair's rule exactly.
		{"lander unknown model", map[string]any{"spawn_model_default_lander": "gpt-9"}},
		{"lander unknown effort", map[string]any{"spawn_effort_default_lander": "ultra"}},
		{"lander model not a string", map[string]any{"spawn_model_default_lander": 7}},
		// The provider defaults (issue #66): the base key must always name a
		// registered provider (no lower operator layer to inherit from); the
		// AFK override allows "" (inherit) but rejects an unknown id.
		{"unknown provider", map[string]any{"provider_default": "ghost"}},
		{"blank provider", map[string]any{"provider_default": ""}},
		{"afk unknown provider", map[string]any{"spawn_provider_default_afk": "ghost"}},
		// The container resource-limit defaults (issue #205): the two integer
		// keys floor at 1, and container_memory must match podman's --memory
		// value grammar.
		{"container pids zero", map[string]any{"container_pids": 0}},
		{"container nofile zero", map[string]any{"container_nofile": 0}},
		{"bad container memory", map[string]any{"container_memory": "bogus"}},
		// transcript_retention_days (issue #81): an integer 0..365, nothing else.
		{"negative transcript retention", map[string]any{"transcript_retention_days": -1}},
		{"transcript retention over cap", map[string]any{"transcript_retention_days": 366}},
		{"fractional transcript retention", map[string]any{"transcript_retention_days": 1.5}},
		{"non-integer transcript retention", map[string]any{"transcript_retention_days": "forever"}},
		{"null transcript retention", map[string]any{"transcript_retention_days": nil}},
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

// The lander-override defaults: a catalog value lands, an EMPTY value is
// allowed and means inherit — the AFK pair's rule — and the two layers are
// independent rows, so the lander can hold a different model than AFK.
func TestAPI_SettingsLanderDefaultsRoundtrip(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingSpawnModelDefaultAFK:     "sonnet",
		store.SettingSpawnModelDefaultLander:  "fable",
		store.SettingSpawnEffortDefaultLander: "high",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if got[store.SettingSpawnModelDefaultLander] != "fable" {
		t.Errorf("spawn_model_default_lander = %v, want fable", got[store.SettingSpawnModelDefaultLander])
	}
	if got[store.SettingSpawnEffortDefaultLander] != "high" {
		t.Errorf("spawn_effort_default_lander = %v, want high", got[store.SettingSpawnEffortDefaultLander])
	}
	if got[store.SettingSpawnModelDefaultAFK] != "sonnet" {
		t.Errorf("spawn_model_default_afk = %v, want sonnet (untouched by the lander key)", got[store.SettingSpawnModelDefaultAFK])
	}

	// Clearing back to inherit: "" is a legal value for the override.
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingSpawnModelDefaultLander:  "",
		store.SettingSpawnEffortDefaultLander: "",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got = settingsOf(t, decodeBody(t, resp))
	if got[store.SettingSpawnModelDefaultLander] != "" || got[store.SettingSpawnEffortDefaultLander] != "" {
		t.Errorf("lander overrides = %v/%v, want both empty (inherit)",
			got[store.SettingSpawnModelDefaultLander], got[store.SettingSpawnEffortDefaultLander])
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

// The container resource-limit defaults (issue #205): seeded 8g/4096/16384,
// typed as JSON numbers for the two integer keys, and all three round-trip
// through a PATCH. Floor/grammar violations are covered by
// TestAPI_SettingsPatchValidation.
func TestAPI_SettingsContainerLimitsRoundtrip(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("GET", "/api/v1/settings", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if got[store.SettingContainerMemory] != "8g" {
		t.Errorf("container_memory = %v, want seeded 8g", got[store.SettingContainerMemory])
	}
	if got[store.SettingContainerPids] != float64(4096) {
		t.Errorf("container_pids = %v, want seeded 4096", got[store.SettingContainerPids])
	}
	if got[store.SettingContainerNofile] != float64(16384) {
		t.Errorf("container_nofile = %v, want seeded 16384", got[store.SettingContainerNofile])
	}

	resp = x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingContainerMemory: "16g",
		store.SettingContainerPids:   2048,
		store.SettingContainerNofile: 8192,
	}, h)
	wantStatus(t, resp, http.StatusOK)
	got = settingsOf(t, decodeBody(t, resp))
	if got[store.SettingContainerMemory] != "16g" {
		t.Errorf("container_memory = %v, want 16g", got[store.SettingContainerMemory])
	}
	if got[store.SettingContainerPids] != float64(2048) || got[store.SettingContainerNofile] != float64(8192) {
		t.Errorf("container_pids/nofile = %v/%v, want 2048/8192", got[store.SettingContainerPids], got[store.SettingContainerNofile])
	}

	// Persisted where the container-runner config would read them.
	if v, err := x.st.GetString(context.Background(), store.SettingContainerMemory, ""); err != nil || v != "16g" {
		t.Errorf("stored container_memory = %q (%v), want 16g", v, err)
	}
}

// The global runner default (issue #55 / ADR-0071): seeded host, and exactly
// "host" or "container" round-trip through a PATCH. Anything else — another
// word, blank, a case variant, null, a number — is a 400 naming the key and
// both values, and writes nothing, neither the bad value nor a valid sibling
// in the same body.
func TestAPI_SettingsRunnerDefault(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("GET", "/api/v1/settings", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := settingsOf(t, decodeBody(t, resp)); got[store.SettingRunnerDefault] != store.RunnerHost {
		t.Errorf("runner_default = %v, want the seeded %q", got[store.SettingRunnerDefault], store.RunnerHost)
	}

	for _, v := range []string{store.RunnerContainer, store.RunnerHost, store.RunnerContainer} {
		resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingRunnerDefault: v}, h)
		wantStatus(t, resp, http.StatusOK)
		if got := settingsOf(t, decodeBody(t, resp)); got[store.SettingRunnerDefault] != v {
			t.Errorf("runner_default after PATCH %q = %v", v, got[store.SettingRunnerDefault])
		}
		if stored, err := x.st.GetSetting(context.Background(), store.SettingRunnerDefault); err != nil || stored != v {
			t.Errorf("stored runner_default = %q (%v), want %q", stored, err, v)
		}
	}

	for _, bad := range []any{"podman", "", "HOST", " host", nil, 5} {
		resp = x.do("PATCH", "/api/v1/settings", map[string]any{
			store.SettingRunnerDefault: bad,
			store.SettingGitAuthorName: "Half Applied",
		}, h)
		wantStatus(t, resp, http.StatusBadRequest)
		msg := fmt.Sprint(decodeBody(t, resp)["error"])
		for _, want := range []string{"runner_default", `"host"`, `"container"`} {
			if !strings.Contains(msg, want) {
				t.Errorf("runner_default %#v: 400 error %q does not name %s", bad, msg, want)
			}
		}
	}
	// Every rejected PATCH wrote nothing: the last good value stands, and the
	// valid sibling never landed.
	if stored, err := x.st.GetSetting(context.Background(), store.SettingRunnerDefault); err != nil || stored != store.RunnerContainer {
		t.Errorf("runner_default = %q (%v) after rejected PATCHes, want %q", stored, err, store.RunnerContainer)
	}
	if v, err := x.st.GetString(context.Background(), store.SettingGitAuthorName, ""); err != nil || v != "" {
		t.Errorf("git_author_name = %q (%v) after rejected PATCHes, want empty", v, err)
	}
}

// devImageRegistryDigest is the digest the stub registry hands back for its
// one resolvable tag — grammatically valid, otherwise arbitrary.
const devImageRegistryDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// newDevImageSettingsServer is the dev_image_default suite's server (issue
// #55): the repo test server (so Options.Repos is a real reposvc) with the
// settings seeded and fallback as Options.DevImageFallback, and its injected
// pinner delegating to a REAL imageref.Resolver whose only network is an
// in-process TLS stub registry. The registry resolves exactly one tag,
// <host>/team/dev:v1 → devImageRegistryDigest, and 404s everything else, so
// the save path runs the production pinner end to end with no live registry.
// It returns the server, the registry's host:port, and a count of the
// requests the registry has served.
func newDevImageSettingsServer(t *testing.T, fallback string) (*repoTestServer, string, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method == http.MethodHead && r.URL.Path == "/v2/team/dev/manifests/v1" {
			w.Header().Set("Docker-Content-Digest", devImageRegistryDigest)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(reg.Close)
	u, err := url.Parse(reg.URL)
	if err != nil {
		t.Fatalf("parse registry url: %v", err)
	}
	x := newRepoTestServerWith(t, func(o *Options) {
		if err := o.Store.SeedDefaultSettings(context.Background(), 6, "claude-code"); err != nil {
			t.Fatal(err)
		}
		o.DevImageFallback = fallback
	})
	x.pin.delegate = (&imageref.Resolver{Client: reg.Client()}).Pin
	return x, u.Host, hits
}

// The global default dev image (issue #55 / ADR-0071) saves exactly as a
// repo's Dev image does (ADR-0053), through the same reposvc pinner: unseeded
// at first; a tag ref is stored digest-pinned with the tag kept; an already
// pinned ref is stored as-is without touching the registry; an unqualified or
// unresolvable ref is a 400 carrying the pinner's reason that writes nothing,
// not even a valid sibling key; and blank clears it to "" (fall through to
// the flag) without a pin.
func TestAPI_SettingsDevImageDefault(t *testing.T) {
	x, host, hits := newDevImageSettingsServer(t, "")
	h := csrfHeaders(x.ts.URL)
	stored := func() string {
		t.Helper()
		v, err := x.st.GetSetting(context.Background(), store.SettingDevImageDefault)
		if err != nil {
			t.Fatalf("GetSetting(dev_image_default): %v", err)
		}
		return v
	}

	// Not seeded: absent from GET until first saved.
	resp := x.do("GET", "/api/v1/settings", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if v, ok := settingsOf(t, decodeBody(t, resp))[store.SettingDevImageDefault]; ok {
		t.Errorf("dev_image_default = %v on a fresh install, want absent (unseeded)", v)
	}

	// A tag ref is resolved and stored pinned, tag kept; the pinner saw the
	// trimmed operator input.
	pinned := host + "/team/dev:v1@" + devImageRegistryDigest
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingDevImageDefault: "  " + host + "/team/dev:v1 "}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := settingsOf(t, decodeBody(t, resp)); got[store.SettingDevImageDefault] != pinned {
		t.Errorf("dev_image_default = %v after PATCH, want the pinned %q", got[store.SettingDevImageDefault], pinned)
	}
	if v := stored(); v != pinned {
		t.Errorf("stored dev_image_default = %q, want %q", v, pinned)
	}
	if last := x.pin.lastCall(); last != host+"/team/dev:v1" {
		t.Errorf("pinner called with %q, want the trimmed ref", last)
	}

	// An already pinned ref is stored as-is, with no registry round-trip.
	already := host + "/team/other:v9@" + devImageRegistryDigest
	before := hits.Load()
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingDevImageDefault: already}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := settingsOf(t, decodeBody(t, resp)); got[store.SettingDevImageDefault] != already {
		t.Errorf("dev_image_default = %v, want the already pinned ref as-is", got[store.SettingDevImageDefault])
	}
	if hits.Load() != before {
		t.Errorf("saving an already pinned ref hit the registry %d times, want 0", hits.Load()-before)
	}

	// Unqualified and unresolvable refs: a 400 carrying the pinner's reason,
	// and nothing written — not the ref, not the valid sibling beside it.
	for _, tc := range []struct{ ref, reason string }{
		{"debian", "must be fully qualified"},
		{host + "/team/missing:v1", "registry returned 404 (check the image path and tag)"},
	} {
		resp = x.do("PATCH", "/api/v1/settings", map[string]any{
			store.SettingDevImageDefault: tc.ref,
			store.SettingGitAuthorName:   "Half Applied",
		}, h)
		wantStatus(t, resp, http.StatusBadRequest)
		if msg := fmt.Sprint(decodeBody(t, resp)["error"]); !strings.Contains(msg, tc.reason) {
			t.Errorf("PATCH %q: 400 error %q, want the pinner's reason %q", tc.ref, msg, tc.reason)
		}
		if v := stored(); v != already {
			t.Errorf("dev_image_default = %q after the rejected %q, want the previous %q", v, tc.ref, already)
		}
		if v, err := x.st.GetString(context.Background(), store.SettingGitAuthorName, ""); err != nil || v != "" {
			t.Errorf("git_author_name = %q (%v) after the rejected %q, want empty", v, err, tc.ref)
		}
	}

	// Blank clears to "" — the fall-through to the flag — without a pin.
	calls := x.pin.callCount()
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingDevImageDefault: " \t "}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := settingsOf(t, decodeBody(t, resp)); got[store.SettingDevImageDefault] != "" {
		t.Errorf("dev_image_default = %v after a blank PATCH, want \"\"", got[store.SettingDevImageDefault])
	}
	if v := stored(); v != "" {
		t.Errorf("stored dev_image_default = %q after a blank PATCH, want \"\"", v)
	}
	if n := x.pin.callCount(); n != calls {
		t.Errorf("a blank PATCH called the pinner (%d → %d)", calls, n)
	}
}

// The pin runs only after every cheap check of the same body has passed
// (issue #55, mirroring reposvc's image_ref-validated-last rule): a PATCH
// whose other key is invalid is a 400 for THAT key, and neither the pinner
// nor the registry is ever reached. Repeated, because map iteration order is
// random — a pin inside the validation loop would slip through some runs.
func TestAPI_SettingsDevImageDefaultPinnedLast(t *testing.T) {
	x, host, hits := newDevImageSettingsServer(t, "")
	h := csrfHeaders(x.ts.URL)
	for range 20 {
		resp := x.do("PATCH", "/api/v1/settings", map[string]any{
			store.SettingDevImageDefault: host + "/team/dev:v1",
			store.SettingAFKTickSeconds:  1,
		}, h)
		wantStatus(t, resp, http.StatusBadRequest)
		if msg := fmt.Sprint(decodeBody(t, resp)["error"]); !strings.Contains(msg, "afk_tick_seconds") {
			t.Fatalf("400 error = %q, want the afk_tick_seconds floor", msg)
		}
	}
	if n := x.pin.callCount(); n != 0 {
		t.Errorf("pinner called %d times for PATCHes failing a cheap check, want 0", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("registry hit %d times for PATCHes failing a cheap check, want 0", n)
	}
	if v, err := x.st.GetSetting(context.Background(), store.SettingDevImageDefault); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("dev_image_default = %q (%v), want still unset", v, err)
	}
}

// With no pinner at all — a server built without the repo service — a
// non-blank dev_image_default fails the way the repo field's no-pinner boot
// does (a 500, never an unpinned ref stored as pinned) and writes nothing,
// while a blank value, which needs no pin, still clears.
func TestAPI_SettingsDevImageDefaultPinnerUnavailable(t *testing.T) {
	x := newSettingsServer(t) // Options.Repos is nil
	h := csrfHeaders(x.ts.URL)

	resp := x.do("PATCH", "/api/v1/settings", map[string]any{
		store.SettingDevImageDefault: "docker.io/library/debian:bookworm",
		store.SettingGitAuthorName:   "Half Applied",
	}, h)
	wantStatus(t, resp, http.StatusInternalServerError)
	_ = resp.Body.Close()
	if v, err := x.st.GetSetting(context.Background(), store.SettingDevImageDefault); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("dev_image_default = %q (%v) after the no-pinner failure, want unset", v, err)
	}
	if v, err := x.st.GetString(context.Background(), store.SettingGitAuthorName, ""); err != nil || v != "" {
		t.Errorf("git_author_name = %q (%v) after the no-pinner failure, want empty", v, err)
	}

	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingDevImageDefault: ""}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := settingsOf(t, decodeBody(t, resp)); got[store.SettingDevImageDefault] != "" {
		t.Errorf("dev_image_default = %v after a blank PATCH, want \"\"", got[store.SettingDevImageDefault])
	}
}

// dev_image_fallback (issue #55 / ADR-0071) is the --container-image value,
// injected read-only into both the GET and the PATCH response ("" when the
// flag is unset); a PATCH carrying it is an unknown-setting 400 that stores
// nothing.
func TestAPI_SettingsDevImageFallback(t *testing.T) {
	const flag = "docker.io/library/debian:stable-slim@sha256:" + "abababababababababababababababababababababababababababababababab"
	for _, tc := range []struct {
		name     string
		fallback string
	}{
		{"flag set", flag},
		{"flag unset", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, _, _ := newDevImageSettingsServer(t, tc.fallback)
			h := csrfHeaders(x.ts.URL)

			resp := x.do("GET", "/api/v1/settings", nil, nil)
			wantStatus(t, resp, http.StatusOK)
			if v, ok := settingsOf(t, decodeBody(t, resp))["dev_image_fallback"]; !ok || v != tc.fallback {
				t.Errorf("GET dev_image_fallback = %v (present %v), want %q", v, ok, tc.fallback)
			}
			resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingGitAuthorName: "Lab Bot"}, h)
			wantStatus(t, resp, http.StatusOK)
			if v, ok := settingsOf(t, decodeBody(t, resp))["dev_image_fallback"]; !ok || v != tc.fallback {
				t.Errorf("PATCH dev_image_fallback = %v (present %v), want %q", v, ok, tc.fallback)
			}

			resp = x.do("PATCH", "/api/v1/settings", map[string]any{"dev_image_fallback": "x"}, h)
			wantStatus(t, resp, http.StatusBadRequest)
			if msg := fmt.Sprint(decodeBody(t, resp)["error"]); !strings.Contains(msg, `unknown setting "dev_image_fallback"`) {
				t.Errorf("400 error = %q, want unknown setting", msg)
			}
			if _, err := x.st.GetSetting(context.Background(), "dev_image_fallback"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetSetting(dev_image_fallback) = %v, want ErrNotFound (never a stored row)", err)
			}
		})
	}
}

// transcript_retention_days (issue #81): seeded 30 and typed as a JSON number
// on GET; both bounds (0 = the off switch, 365 = the cap) PATCH and persist
// where TranscriptRetentionDays reads them; one past either bound is a 400
// with the exact operator-facing message that writes nothing — not even a
// valid sibling.
func TestAPI_SettingsTranscriptRetention(t *testing.T) {
	x := newSettingsServer(t)
	h := csrfHeaders(x.ts.URL)
	ctx := context.Background()

	resp := x.do("GET", "/api/v1/settings", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	got := settingsOf(t, decodeBody(t, resp))
	if got[store.SettingTranscriptRetentionDays] != float64(store.DefaultTranscriptRetentionDays) {
		t.Errorf("transcript_retention_days = %v (%T), want seeded JSON number %d",
			got[store.SettingTranscriptRetentionDays], got[store.SettingTranscriptRetentionDays], store.DefaultTranscriptRetentionDays)
	}

	for _, n := range []int{0, store.MaxTranscriptRetentionDays, 7} {
		resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingTranscriptRetentionDays: n}, h)
		wantStatus(t, resp, http.StatusOK)
		got = settingsOf(t, decodeBody(t, resp))
		if got[store.SettingTranscriptRetentionDays] != float64(n) {
			t.Errorf("PATCH %d echoed %v", n, got[store.SettingTranscriptRetentionDays])
		}
		if v, err := x.st.TranscriptRetentionDays(ctx); err != nil || v != n {
			t.Errorf("stored transcript_retention_days = %d (%v), want %d", v, err, n)
		}
	}

	// A numeric string is accepted like every int key (curl users).
	resp = x.do("PATCH", "/api/v1/settings", map[string]any{store.SettingTranscriptRetentionDays: "14"}, h)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	if v, err := x.st.TranscriptRetentionDays(ctx); err != nil || v != 14 {
		t.Errorf("stored after string PATCH = %d (%v), want 14", v, err)
	}

	for _, tc := range []struct {
		value any
		msg   string
	}{
		{-1, "transcript_retention_days must be at least 0"},
		{366, "transcript_retention_days must be at most 365"},
		{"forever", "transcript_retention_days must be an integer"},
	} {
		resp = x.do("PATCH", "/api/v1/settings", map[string]any{
			store.SettingTranscriptRetentionDays: tc.value,
			store.SettingGitAuthorName:           "Half Applied",
		}, h)
		wantStatus(t, resp, http.StatusBadRequest)
		if body := decodeBody(t, resp); body["error"] != tc.msg {
			t.Errorf("PATCH %v error = %v, want %q", tc.value, body["error"], tc.msg)
		}
	}
	if v, err := x.st.TranscriptRetentionDays(ctx); err != nil || v != 14 {
		t.Errorf("transcript_retention_days after rejected PATCHes = %d (%v), want 14", v, err)
	}
	if v, err := x.st.GetString(ctx, store.SettingGitAuthorName, ""); err != nil || v != "" {
		t.Errorf("git_author_name = %q (%v) after rejected PATCHes, want empty", v, err)
	}
}
