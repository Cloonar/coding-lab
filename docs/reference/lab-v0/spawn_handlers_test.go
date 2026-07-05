package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// renderIndex executes the full "index" template against a crafted pageData, so
// the spawn-config control (which lives in the static shell, outside #live, and
// so never appears in the "live" fragment) can be asserted without driving real
// tmux.
func renderIndex(t *testing.T, srv *Server, data pageData) string {
	t.Helper()
	var b strings.Builder
	if err := srv.tmpl.ExecuteTemplate(&b, "index", data); err != nil {
		t.Fatalf("render index: %v", err)
	}
	return b.String()
}

func newSpawnTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
}

// TestIndex_spawnConfigControl pins the model/effort selector's render contract:
// two native <select>s offering exactly the closed allowlists, the persisted
// value pre-selected, the auto-save form wired to /spawn-config, the no-JS Save
// fallback present, and the whole control placed OUTSIDE #live so a background
// poll can't reset a selection. lab has no committed JS harness, so this
// render-level assertion is the server-side guard for the control (#156).
func TestIndex_spawnConfigControl(t *testing.T) {
	srv := newSpawnTestServer(t)
	out := renderIndex(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6,
		SpawnModel: "sonnet", SpawnEffort: "high",
		ModelOptions: spawnModels, EffortOptions: spawnEfforts,
	})

	// The form auto-saves through the intercepted-fetch path (data-action) to the
	// global endpoint, and carries the no-JS Save fallback.
	if !strings.Contains(out, `action="/spawn-config"`) || !strings.Contains(out, "data-action") {
		t.Error("spawn-config form must POST to /spawn-config via the data-action fetch path")
	}
	if !strings.Contains(out, `<select name="model"`) || !strings.Contains(out, `<select name="effort"`) {
		t.Error("expected native model + effort <select>s")
	}
	if !strings.Contains(out, "data-spawn-save") {
		t.Error("expected the no-JS Save fallback button (data-spawn-save)")
	}

	// Every allowlisted option renders with its value (and the model labels).
	for _, m := range spawnModels {
		if !strings.Contains(out, `value="`+m.Value+`"`) {
			t.Errorf("model dropdown missing value %q", m.Value)
		}
		if !strings.Contains(out, ">"+m.Label+"<") {
			t.Errorf("model dropdown missing label %q", m.Label)
		}
	}
	for _, e := range spawnEfforts {
		if !strings.Contains(out, `value="`+e+`"`) {
			t.Errorf("effort dropdown missing value %q", e)
		}
	}

	// The persisted value is pre-selected — and ONLY it (the previous default is
	// not also marked selected).
	if !strings.Contains(out, `value="sonnet" selected`) {
		t.Error("persisted model (sonnet) should be the selected option")
	}
	if strings.Contains(out, `value="opus[1m]" selected`) {
		t.Error("only the persisted model should be selected; opus[1m] must not be")
	}
	if !strings.Contains(out, `value="high" selected`) {
		t.Error("persisted effort (high) should be the selected option")
	}
	if strings.Contains(out, `value="max" selected`) {
		t.Error("only the persisted effort should be selected; max must not be")
	}

	// Placed OUTSIDE #live: the control must precede the live region, so the morph
	// never touches it.
	ci, li := strings.Index(out, "spawn-config"), strings.Index(out, `id="live"`)
	if ci < 0 || li < 0 || ci > li {
		t.Errorf("spawn-config control must render before #live (got positions %d and %d)", ci, li)
	}
	// And it is NOT part of the live fragment, so a poll can't morph it.
	if strings.Contains(renderLive(t, srv, pageData{LoggedIn: true, MaxInstances: 6}), "spawn-config") {
		t.Error("spawn-config must not appear in the #live fragment")
	}
}

// TestIndex_spawnConfigDefaultsPreselected proves a fresh store renders the
// documented opus[1m] + max as the pre-selected options (#156), via the real
// indexData path (the getter's defaults flow into the page model).
func TestIndex_spawnConfigDefaultsPreselected(t *testing.T) {
	requireTmux(t) // indexData → snapshot lists sessions
	srv := newSpawnTestServer(t)
	rec := httptest.NewRecorder()
	srv.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	out := rec.Body.String()
	if !strings.Contains(out, `value="opus[1m]" selected`) {
		t.Error("fresh store: Opus (1M) should be the pre-selected model")
	}
	if !strings.Contains(out, `value="max" selected`) {
		t.Error("fresh store: max should be the pre-selected effort")
	}
}

// TestHandleSpawnConfig_persistsValidAndRejectsInvalid exercises the endpoint's
// validate-then-persist contract for both transports: a valid pair is stored, an
// out-of-allowlist value is rejected with the message surfaced (AJAX) or the
// generic flash (no-JS) and NOTHING is persisted (#156).
func TestHandleSpawnConfig_persistsValidAndRejectsInvalid(t *testing.T) {
	post := func(srv *Server, model, effort string, ajax bool) *httptest.ResponseRecorder {
		form := url.Values{"model": {model}, "effort": {effort}}
		req := httptest.NewRequest(http.MethodPost, "/spawn-config", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if ajax {
			req.Header.Set(fragmentHeader, "1")
		}
		rec := httptest.NewRecorder()
		srv.handleSpawnConfig(rec, req)
		return rec
	}

	// Valid, no-JS: 303 back to the index, pair persisted.
	srv := newSpawnTestServer(t)
	rec := post(srv, "sonnet", "high", false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("valid no-JS: status %d loc %q; want 303 /", rec.Code, rec.Header().Get("Location"))
	}
	if m, e := srv.store.SpawnConfig(); m != "sonnet" || e != "high" {
		t.Errorf("after valid POST, SpawnConfig = (%q,%q); want (sonnet,high)", m, e)
	}

	// The bracketed default id opus[1m] must survive the form url-encode →
	// r.FormValue round-trip (the brackets encode as %5B/%5D); effort=low is
	// non-default, so its persistence proves the write actually landed.
	rec = post(srv, "opus[1m]", "low", false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid opus[1m] no-JS: status %d; want 303", rec.Code)
	}
	if m, e := srv.store.SpawnConfig(); m != "opus[1m]" || e != "low" {
		t.Errorf("after opus[1m] POST, SpawnConfig = (%q,%q); want (opus[1m],low)", m, e)
	}

	// Invalid model, AJAX: 400 with the real message in the body, nothing persisted.
	srv = newSpawnTestServer(t)
	rec = post(srv, "gpt-4", "max", true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid AJAX: status %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown model") {
		t.Errorf("invalid AJAX body = %q; want the real 'unknown model' message", rec.Body.String())
	}
	if m, e := srv.store.SpawnConfig(); m != defaultSpawnModel || e != defaultSpawnEffort {
		t.Errorf("a rejected POST persisted (%q,%q); want the untouched defaults", m, e)
	}

	// Invalid effort, no-JS: bounced to the index with the generic action flash.
	srv = newSpawnTestServer(t)
	rec = post(srv, "opus[1m]", "extreme", false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?error=action" {
		t.Fatalf("invalid no-JS: status %d loc %q; want 303 /?error=action", rec.Code, rec.Header().Get("Location"))
	}

	// Wrong method is refused.
	rec = httptest.NewRecorder()
	newSpawnTestServer(t).handleSpawnConfig(rec, httptest.NewRequest(http.MethodGet, "/spawn-config", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /spawn-config status = %d; want 405", rec.Code)
	}
}

// A valid AJAX save returns the standard #live fragment (not a redirect) so the
// page resyncs in place, per the brief (#156).
func TestHandleSpawnConfig_ajaxReturnsFragment(t *testing.T) {
	requireTmux(t) // ok() renders the fragment, which lists sessions
	srv := newSpawnTestServer(t)
	form := url.Values{"model": {"fable"}, "effort": {"low"}}
	req := httptest.NewRequest(http.MethodPost, "/spawn-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(fragmentHeader, "1")
	rec := httptest.NewRecorder()
	srv.handleSpawnConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("valid AJAX: status %d; want 200 (fragment, not a 303)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Error("valid AJAX should return the #live fragment, not the full page")
	}
	if m, e := srv.store.SpawnConfig(); m != "fable" || e != "low" {
		t.Errorf("after valid AJAX, SpawnConfig = (%q,%q); want (fable,low)", m, e)
	}
}
