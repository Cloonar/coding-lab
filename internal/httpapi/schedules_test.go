package httpapi

// Schedule surface suite (issue #247 / ADR-0062): repo-scoped CRUD, the
// re-enable action, the flow catalog, and the cron preview — httptest against
// the real store on a t.TempDir sqlite DB, with the real provider registry
// mounted so the provider override's write-time check is exercised.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// newScheduleServer builds an authenticated test server with the provider
// registry mounted (the fake registers as "claude-code"). The Schedule routes
// are store-backed and always mounted, so nothing else is needed.
func newScheduleServer(t *testing.T) *testServer {
	t.Helper()
	reg, err := provider.NewRegistry(providertest.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	x := newTestServer(t, func(o *Options) { o.Providers = reg })
	x.setup("op", "password123")
	return x
}

// schedulesOf pulls the schedules array out of a decoded list response.
func schedulesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["schedules"].([]any)
	if !ok {
		t.Fatalf("no schedules array in %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// stringsOf reads a JSON array of strings out of a decoded body.
func stringsOf(t *testing.T, body map[string]any, key string) []string {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("%s is not an array in %v", key, body)
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s contains a non-string %v", key, e)
		}
		out = append(out, s)
	}
	return out
}

func TestScheduleCRUD(t *testing.T) {
	x := newScheduleServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/schedules"

	// A repo with no Schedules answers an empty array, never null.
	resp := x.do("GET", base, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := schedulesOf(t, decodeBody(t, resp)); len(got) != 0 {
		t.Fatalf("fresh repo schedules = %v", got)
	}

	// Create with every knob set. The flow selection arrives in the operator's
	// order and must come back in CATALOG order.
	resp = x.do("POST", base, map[string]any{
		"name":           "  weekly update check  ",
		"cadence":        "0 6 * * 1",
		"prompt":         "  investigate dependency updates  ",
		"flows":          []string{"human-triage", "autolander"},
		"budget_minutes": 45,
		"model":          "opus",
		"effort":         "high",
		"provider":       "claude-code",
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)

	id, _ := created["id"].(string)
	if !strings.HasPrefix(id, "sched_") {
		t.Fatalf("created id = %v, want a sched_ prefix", created["id"])
	}
	if created["repo_id"] != repo.ID {
		t.Fatalf("repo_id = %v, want %s", created["repo_id"], repo.ID)
	}
	// Name and prompt are stored trimmed.
	if created["name"] != "weekly update check" {
		t.Fatalf("name = %q, want %q", created["name"], "weekly update check")
	}
	if created["prompt"] != "investigate dependency updates" {
		t.Fatalf("prompt = %q", created["prompt"])
	}
	if got := stringsOf(t, created, "flows"); len(got) != 2 || got[0] != "autolander" || got[1] != "human-triage" {
		t.Fatalf("flows = %v, want catalog order [autolander human-triage]", got)
	}
	if created["enabled"] != true {
		t.Fatalf("enabled = %v, want true (the default)", created["enabled"])
	}
	if created["budget_minutes"] != float64(45) || created["model"] != "opus" ||
		created["effort"] != "high" || created["provider"] != "claude-code" {
		t.Fatalf("overrides = %v", created)
	}
	if created["consecutive_failures"] != float64(0) || created["paused"] != false {
		t.Fatalf("fresh strike state = %v", created)
	}
	// Every key is present; the never-fired stamp is JSON null, not absent.
	for _, key := range []string{"id", "repo_id", "name", "cadence", "prompt", "flows",
		"enabled", "budget_minutes", "model", "effort", "provider",
		"consecutive_failures", "paused", "last_fired_at", "created_at", "updated_at"} {
		if _, ok := created[key]; !ok {
			t.Fatalf("response is missing key %q: %v", key, created)
		}
	}
	if created["last_fired_at"] != nil {
		t.Fatalf("last_fired_at = %v, want null", created["last_fired_at"])
	}

	// A second, minimal Schedule: no prompt but a flow, everything else
	// inherited — every override renders as JSON null.
	resp = x.do("POST", base, map[string]any{
		"name":    "audit",
		"cadence": "*/15 * * * *",
		"flows":   []string{"autolander"},
		"enabled": false,
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	minimal := decodeBody(t, resp)
	if minimal["prompt"] != "" || minimal["enabled"] != false {
		t.Fatalf("minimal schedule = %v", minimal)
	}
	for _, key := range []string{"budget_minutes", "model", "effort", "provider", "last_fired_at"} {
		if minimal[key] != nil {
			t.Fatalf("%s = %v, want null", key, minimal[key])
		}
	}

	// The list is ordered by name.
	resp = x.do("GET", base, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	listed := schedulesOf(t, decodeBody(t, resp))
	if len(listed) != 2 || listed[0]["name"] != "audit" || listed[1]["name"] != "weekly update check" {
		t.Fatalf("listed schedules = %v", listed)
	}

	// Delete: 204, then the id is gone for every verb that names it.
	resp = x.do("DELETE", base+"/"+id, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	resp = x.do("DELETE", base+"/"+id, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	resp = x.do("PATCH", base+"/"+id, map[string]any{"name": "x"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	resp = x.do("GET", base, nil, nil)
	if got := schedulesOf(t, decodeBody(t, resp)); len(got) != 1 {
		t.Fatalf("after delete = %v", got)
	}
}

// TestScheduleCreateValidation pins every write-time refusal and its
// operator-facing message — the form renders these verbatim.
func TestScheduleCreateValidation(t *testing.T) {
	x := newScheduleServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/schedules"

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"missing name", map[string]any{"cadence": "0 6 * * *", "prompt": "p"},
			"name is required"},
		{"blank name", map[string]any{"name": "   ", "cadence": "0 6 * * *", "prompt": "p"},
			"name is required"},
		{"name too long", map[string]any{"name": strings.Repeat("n", 101), "cadence": "0 6 * * *", "prompt": "p"},
			"name must be at most 100 characters"},
		{"missing cadence", map[string]any{"name": "s", "prompt": "p"},
			"cadence is required"},
		{"bad cadence passes the parser message through",
			map[string]any{"name": "s", "cadence": "99 * * * *", "prompt": "p"},
			`cron: minute value 99 out of range 0-59`},
		{"cadence with the wrong field count",
			map[string]any{"name": "s", "cadence": "0 6 * *", "prompt": "p"},
			`cron: expression "0 6 * *" has 4 fields, want exactly 5: minute hour day-of-month month day-of-week`},
		{"cadence that never fires",
			map[string]any{"name": "s", "cadence": "0 0 30 2 *", "prompt": "p"},
			"cadence never matches any upcoming time"},
		{"unknown flow",
			map[string]any{"name": "s", "cadence": "0 6 * * *", "flows": []string{"nope"}},
			`unknown flow "nope"`},
		{"duplicate flow",
			map[string]any{"name": "s", "cadence": "0 6 * * *", "flows": []string{"autolander", "autolander"}},
			`duplicate flow "autolander"`},
		{"neither prompt nor flow",
			map[string]any{"name": "s", "cadence": "0 6 * * *"},
			"a schedule needs a prompt, a flow, or both"},
		{"whitespace prompt with no flow",
			map[string]any{"name": "s", "cadence": "0 6 * * *", "prompt": "   ", "flows": []string{}},
			"a schedule needs a prompt, a flow, or both"},
		{"zero-width prompt with no flow",
			map[string]any{"name": "s", "cadence": "0 6 * * *", "prompt": "\u200B\uFEFF\u2060"},
			"a schedule needs a prompt, a flow, or both"},
		{"budget below the floor",
			map[string]any{"name": "s", "cadence": "0 6 * * *", "prompt": "p", "budget_minutes": 0},
			"budget_minutes must be between 1 and 1440 (null clears the override)"},
		{"budget above the ceiling",
			map[string]any{"name": "s", "cadence": "0 6 * * *", "prompt": "p", "budget_minutes": 1441},
			"budget_minutes must be between 1 and 1440 (null clears the override)"},
		{"unknown provider",
			map[string]any{"name": "s", "cadence": "0 6 * * *", "prompt": "p", "provider": "ghost"},
			`unknown provider "ghost"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := x.do("POST", base, tc.body, h)
			wantStatus(t, resp, http.StatusBadRequest)
			if got := decodeBody(t, resp)["error"]; got != tc.want {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}

	// The boundaries themselves are accepted.
	for i, minutes := range []int{1, 1440} {
		resp := x.do("POST", base, map[string]any{
			"name": "edge" + string(rune('a'+i)), "cadence": "0 6 * * *",
			"prompt": "p", "budget_minutes": minutes,
		}, h)
		wantStatus(t, resp, http.StatusCreated)
		if got := decodeBody(t, resp)["budget_minutes"]; got != float64(minutes) {
			t.Fatalf("budget_minutes = %v, want %d", got, minutes)
		}
	}

	// A name already used in this repo is a 409, the labels precedent.
	resp := x.do("POST", base, map[string]any{"name": "dup", "cadence": "0 6 * * *", "prompt": "p"}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()
	resp = x.do("POST", base, map[string]any{"name": "dup", "cadence": "0 6 * * *", "prompt": "p"}, h)
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp)["error"]; got != store.ErrNameTaken.Error() {
		t.Fatalf("duplicate error = %q, want %q", got, store.ErrNameTaken.Error())
	}
}

func TestSchedulePatch(t *testing.T) {
	x := newScheduleServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/schedules"

	resp := x.do("POST", base, map[string]any{
		"name": "nightly", "cadence": "0 3 * * *", "prompt": "look around",
		"flows": []string{"autolander"}, "budget_minutes": 30, "model": "opus",
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)
	path := base + "/" + id

	// A single-field patch leaves every other column alone.
	resp = x.do("PATCH", path, map[string]any{"cadence": "30 4 * * 2"}, h)
	wantStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	if got["cadence"] != "30 4 * * 2" || got["name"] != "nightly" ||
		got["prompt"] != "look around" || got["budget_minutes"] != float64(30) || got["model"] != "opus" {
		t.Fatalf("after cadence patch = %v", got)
	}
	if flows := stringsOf(t, got, "flows"); len(flows) != 1 || flows[0] != "autolander" {
		t.Fatalf("flows after cadence patch = %v", flows)
	}

	// null clears a nullable override; the enabled flag patches independently.
	resp = x.do("PATCH", path, map[string]any{"budget_minutes": nil, "model": nil, "enabled": false}, h)
	wantStatus(t, resp, http.StatusOK)
	got = decodeBody(t, resp)
	if got["budget_minutes"] != nil || got["model"] != nil || got["enabled"] != false {
		t.Fatalf("after clearing patch = %v", got)
	}

	// A flow selection patches in catalog order.
	resp = x.do("PATCH", path, map[string]any{"flows": []string{"human-triage", "autolander"}}, h)
	wantStatus(t, resp, http.StatusOK)
	if flows := stringsOf(t, decodeBody(t, resp), "flows"); len(flows) != 2 ||
		flows[0] != "autolander" || flows[1] != "human-triage" {
		t.Fatalf("patched flows = %v", flows)
	}

	// Clearing the prompt is legal while flows remain…
	resp = x.do("PATCH", path, map[string]any{"prompt": ""}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["prompt"] != "" {
		t.Fatalf("cleared prompt = %v", got["prompt"])
	}
	// …and dropping the last flow is now a 400, because the MERGED Schedule
	// would have neither.
	resp = x.do("PATCH", path, map[string]any{"flows": nil}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp)["error"]; got != "a schedule needs a prompt, a flow, or both" {
		t.Fatalf("error = %q", got)
	}
	// Supplying both halves in one patch is fine — the rule reads the merge.
	resp = x.do("PATCH", path, map[string]any{"flows": nil, "prompt": "back to prose"}, h)
	wantStatus(t, resp, http.StatusOK)
	got = decodeBody(t, resp)
	if got["prompt"] != "back to prose" || len(stringsOf(t, got, "flows")) != 0 {
		t.Fatalf("after combined patch = %v", got)
	}

	// Field-level refusals.
	patchCases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"blank rename", map[string]any{"name": "  "}, "name is required"},
		{"bad cadence", map[string]any{"cadence": "0 99 * * *"}, "cron: hour value 99 out of range 0-23"},
		{"never-firing cadence", map[string]any{"cadence": "0 0 31 2 *"}, "cadence never matches any upcoming time"},
		{"unknown flow", map[string]any{"flows": []string{"ghost"}}, `unknown flow "ghost"`},
		{"duplicate flow", map[string]any{"flows": []string{"autolander", "autolander"}}, `duplicate flow "autolander"`},
		{"budget out of range", map[string]any{"budget_minutes": 5000},
			"budget_minutes must be between 1 and 1440 (null clears the override)"},
		{"unknown provider", map[string]any{"provider": "ghost"}, `unknown provider "ghost"`},
		{"unknown field", map[string]any{"colour": "red"}, `unknown field "colour"`},
		// The strike state is engine bookkeeping: an edit form must not be
		// able to clear a pause or forge a counter.
		{"paused is not patchable", map[string]any{"paused": false}, `unknown field "paused"`},
		{"counter is not patchable", map[string]any{"consecutive_failures": 0}, `unknown field "consecutive_failures"`},
		{"last_fired_at is not patchable", map[string]any{"last_fired_at": nil}, `unknown field "last_fired_at"`},
		{"flows must be an array", map[string]any{"flows": "autolander"},
			"field flows must be an array of flow keys or null"},
	}
	for _, tc := range patchCases {
		t.Run(tc.name, func(t *testing.T) {
			resp := x.do("PATCH", path, tc.body, h)
			wantStatus(t, resp, http.StatusBadRequest)
			if got := decodeBody(t, resp)["error"]; got != tc.want {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}

	// A rename onto a sibling's name is the 409, not a 400.
	resp = x.do("POST", base, map[string]any{"name": "other", "cadence": "0 6 * * *", "prompt": "p"}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()
	resp = x.do("PATCH", path, map[string]any{"name": "other"}, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()
}

// TestScheduleKnobOverrideTrimParity pins that POST and PATCH store the same
// state for the same input: padded overrides trim, whitespace-only clears to
// NULL, and a padded provider validates the same through both verbs — no
// input may 201 on create and 400 on patch.
func TestScheduleKnobOverrideTrimParity(t *testing.T) {
	x := newScheduleServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/schedules"

	resp := x.do("POST", base, map[string]any{
		"name": "padded-create", "cadence": "0 6 * * *", "prompt": "p",
		"model": "  opus  ", "effort": "  high  ", "provider": "  claude-code  ",
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	if created["model"] != "opus" || created["effort"] != "high" || created["provider"] != "claude-code" {
		t.Fatalf("created overrides = %v, want them stored trimmed", created)
	}

	resp = x.do("POST", base, map[string]any{"name": "padded-patch", "cadence": "0 6 * * *", "prompt": "p"}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)
	resp = x.do("PATCH", base+"/"+id, map[string]any{
		"model": "  opus  ", "effort": "  high  ", "provider": "  claude-code  ",
	}, h)
	wantStatus(t, resp, http.StatusOK)
	patched := decodeBody(t, resp)
	if patched["model"] != "opus" || patched["effort"] != "high" || patched["provider"] != "claude-code" {
		t.Fatalf("patched overrides = %v, want the identical trimmed state create stores", patched)
	}

	// Whitespace-only clears to NULL through PATCH exactly as it would on
	// create.
	resp = x.do("PATCH", base+"/"+id, map[string]any{"model": "   ", "effort": "   ", "provider": "   "}, h)
	wantStatus(t, resp, http.StatusOK)
	cleared := decodeBody(t, resp)
	for _, key := range []string{"model", "effort", "provider"} {
		if cleared[key] != nil {
			t.Fatalf("%s = %v, want null after a whitespace-only patch", key, cleared[key])
		}
	}
}

// TestScheduleInputBounds pins the prompt and cadence size ceilings on both
// write verbs — an unbounded cadence would be re-parsed by every spawn pass
// forever, and the composed prompt rides the spawn argv.
func TestScheduleInputBounds(t *testing.T) {
	x := newScheduleServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/schedules"

	longPrompt := strings.Repeat("p", afkPromptMaxBytes+1)
	longCadence := "0 6 * * " + strings.Repeat("1,", 130) + "1" // parseable shape, over the byte bound
	if len(longCadence) <= scheduleCadenceMaxBytes {
		t.Fatalf("fixture cadence is only %d bytes", len(longCadence))
	}

	createCases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"prompt over the byte bound",
			map[string]any{"name": "s", "cadence": "0 6 * * *", "prompt": longPrompt},
			"prompt must be at most 16384 bytes"},
		{"cadence over the byte bound",
			map[string]any{"name": "s", "cadence": longCadence, "prompt": "p"},
			"cadence must be at most 256 bytes"},
	}
	for _, tc := range createCases {
		t.Run("create "+tc.name, func(t *testing.T) {
			resp := x.do("POST", base, tc.body, h)
			wantStatus(t, resp, http.StatusBadRequest)
			if got := decodeBody(t, resp)["error"]; got != tc.want {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}

	// A prompt at exactly the bound is accepted.
	resp := x.do("POST", base, map[string]any{
		"name": "edge", "cadence": "0 6 * * *", "prompt": strings.Repeat("p", afkPromptMaxBytes),
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)

	patchCases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"prompt over the byte bound", map[string]any{"prompt": longPrompt},
			"prompt must be at most 16384 bytes"},
		{"cadence over the byte bound", map[string]any{"cadence": longCadence},
			"cadence must be at most 256 bytes"},
	}
	for _, tc := range patchCases {
		t.Run("patch "+tc.name, func(t *testing.T) {
			resp := x.do("PATCH", base+"/"+id, tc.body, h)
			wantStatus(t, resp, http.StatusBadRequest)
			if got := decodeBody(t, resp)["error"]; got != tc.want {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScheduleRepoScoping pins the cross-repo guard: a Schedule of repo A is
// invisible through repo B's path, for every verb that names it.
func TestScheduleRepoScoping(t *testing.T) {
	x := newScheduleServer(t)
	repoA := seedTrackerRepo(t, x, "alpha", nil)
	repoB := seedTrackerRepo(t, x, "beta", nil)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/repos/"+repoA.ID+"/schedules",
		map[string]any{"name": "a-only", "cadence": "0 6 * * *", "prompt": "p"}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)

	viaB := "/api/v1/repos/" + repoB.ID + "/schedules/" + id
	resp = x.do("PATCH", viaB, map[string]any{"name": "stolen"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	resp = x.do("POST", viaB+"/reenable", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	resp = x.do("DELETE", viaB, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()

	// Untouched on A, and absent from B's listing.
	resp = x.do("GET", "/api/v1/repos/"+repoA.ID+"/schedules", nil, nil)
	listed := schedulesOf(t, decodeBody(t, resp))
	if len(listed) != 1 || listed[0]["name"] != "a-only" {
		t.Fatalf("repo A schedules = %v", listed)
	}
	resp = x.do("GET", "/api/v1/repos/"+repoB.ID+"/schedules", nil, nil)
	if got := schedulesOf(t, decodeBody(t, resp)); len(got) != 0 {
		t.Fatalf("repo B schedules = %v", got)
	}

	// An unknown repo is a 404 before the Schedule is ever looked up.
	resp = x.do("GET", "/api/v1/repos/repo_missing/schedules", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// TestScheduleReenable covers the human un-pause: the pause clears, the
// counter zeroes, and repo.changed rides only a real transition.
func TestScheduleReenable(t *testing.T) {
	x := newScheduleServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/schedules"
	ctx := context.Background()

	resp := x.do("POST", base, map[string]any{"name": "struck", "cadence": "0 6 * * *", "prompt": "p"}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)

	// Strike it out the way the reap path does.
	for range 3 {
		if _, err := x.st.IncrementScheduleFailures(ctx, id); err != nil {
			t.Fatalf("IncrementScheduleFailures: %v", err)
		}
	}
	if _, err := x.st.SetSchedulePaused(ctx, id, true); err != nil {
		t.Fatalf("SetSchedulePaused: %v", err)
	}
	resp = x.do("GET", base, nil, nil)
	paused := schedulesOf(t, decodeBody(t, resp))[0]
	if paused["paused"] != true || paused["consecutive_failures"] != float64(3) {
		t.Fatalf("paused fixture = %v", paused)
	}

	log := recordBus(t, x.bus)
	resp = x.do("POST", base+"/"+id+"/reenable", nil, h)
	wantStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	if got["paused"] != false || got["consecutive_failures"] != float64(0) {
		t.Fatalf("re-enabled schedule = %v", got)
	}
	ev := waitForBusEvent(t, log, afk.EventRepoChanged)
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	if !strings.Contains(string(payload), repo.ID) {
		t.Fatalf("repo.changed payload = %s, want repo %s", payload, repo.ID)
	}

	// Re-enabling an already-armed Schedule is a no-op: same row, and NO
	// second event. The delete that follows publishes one, and per-subscriber
	// delivery is ordered — so seeing it means nothing was skipped before it.
	resp = x.do("POST", base+"/"+id+"/reenable", nil, h)
	wantStatus(t, resp, http.StatusOK)
	if again := decodeBody(t, resp); again["paused"] != false || again["consecutive_failures"] != float64(0) {
		t.Fatalf("second re-enable = %v", again)
	}
	resp = x.do("DELETE", base+"/"+id, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	waitForBusEventN(t, log, afk.EventRepoChanged, 2)
	if n := len(log.snapshot()); n != 2 {
		t.Fatalf("published %d events, want 2 (the re-enable transition and the delete): %+v", n, log.snapshot())
	}

	// A Schedule that never existed is a 404, not a silent no-op.
	resp = x.do("POST", base+"/sched_missing/reenable", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// TestScheduleFlowCatalogEndpoint pins the catalog listing: exactly the
// built-in keys, in catalog order, with no instruction text.
func TestScheduleFlowCatalogEndpoint(t *testing.T) {
	x := newScheduleServer(t)

	resp := x.do("GET", "/api/v1/schedule-flows", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	raw, ok := body["flows"].([]any)
	if !ok {
		t.Fatalf("no flows array in %v", body)
	}
	catalog := afk.FlowCatalog()
	if len(raw) != len(catalog) {
		t.Fatalf("flows = %v, want %d entries", raw, len(catalog))
	}
	for i, e := range raw {
		entry := e.(map[string]any)
		if entry["key"] != catalog[i].Key || entry["label"] != catalog[i].Label ||
			entry["description"] != catalog[i].Description {
			t.Fatalf("flows[%d] = %v, want %+v", i, entry, catalog[i])
		}
		if len(entry) != 3 {
			t.Fatalf("flows[%d] = %v, want exactly key/label/description", i, entry)
		}
		// The instruction block is lab-owned prose composed at spawn; it must
		// never reach the operator surface under any spelling.
		for _, key := range []string{"instructions", "Instructions"} {
			if _, present := entry[key]; present {
				t.Fatalf("flows[%d] exposes %s: %v", i, key, entry)
			}
		}
	}
}

func TestCronPreview(t *testing.T) {
	x := newScheduleServer(t)
	preview := func(expr string) map[string]any {
		t.Helper()
		resp := x.do("GET", "/api/v1/cron/preview?expr="+url.QueryEscape(expr), nil, nil)
		wantStatus(t, resp, http.StatusOK)
		return decodeBody(t, resp)
	}

	// A valid expression: three strictly increasing firings in server-local
	// time, each also rendered for the form.
	body := preview("0 6 * * 1")
	if body["expr"] != "0 6 * * 1" || body["valid"] != true || body["error"] != nil {
		t.Fatalf("valid preview = %v", body)
	}
	next := stringsOf(t, body, "next")
	display := stringsOf(t, body, "next_display")
	if len(next) != 3 || len(display) != 3 {
		t.Fatalf("next = %v, next_display = %v, want 3 each", next, display)
	}
	var prev time.Time
	for i, s := range next {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("next[%d] = %q: %v", i, s, err)
		}
		if !at.After(time.Now()) {
			t.Fatalf("next[%d] = %s is not in the future", i, s)
		}
		if i > 0 && !at.After(prev) {
			t.Fatalf("next is not strictly increasing: %v", next)
		}
		prev = at
		// Server-local: the display rendering is the same instant in the
		// server's own location, weekday first.
		if want := at.Local().Format("Mon 2006-01-02 15:04"); display[i] != want {
			t.Fatalf("next_display[%d] = %q, want %q", i, display[i], want)
		}
		if at.Weekday() != time.Monday || at.Hour() != 6 || at.Minute() != 0 {
			t.Fatalf("next[%d] = %s, want a Monday at 06:00", i, s)
		}
	}

	// An unparseable expression: 200 with valid:false, the parser's message,
	// and null firings — a preview is not a write.
	body = preview("99 * * * *")
	if body["valid"] != false || body["next"] != nil || body["next_display"] != nil {
		t.Fatalf("invalid preview = %v", body)
	}
	if body["error"] != "cron: minute value 99 out of range 0-59" {
		t.Fatalf("invalid preview error = %v", body["error"])
	}

	// Legal but never matching (February 30th).
	body = preview("0 0 30 2 *")
	if body["valid"] != false || body["error"] != "expression never matches" ||
		body["next"] != nil || body["next_display"] != nil {
		t.Fatalf("never-matching preview = %v", body)
	}

	// Over the write-time byte bound: still 200, valid:false, and the SAME
	// message the write refuses with — never parsed.
	body = preview("0 6 * * " + strings.Repeat("1,", 130) + "1")
	if body["valid"] != false || body["error"] != "cadence must be at most 256 bytes" ||
		body["next"] != nil || body["next_display"] != nil {
		t.Fatalf("over-long preview = %v", body)
	}

	// A missing or empty expr is the same not-valid-yet state a half-typed
	// field is in, never a 400.
	resp := x.do("GET", "/api/v1/cron/preview", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body = decodeBody(t, resp)
	if body["expr"] != "" || body["valid"] != false || body["error"] == nil {
		t.Fatalf("empty preview = %v", body)
	}
}

// TestScheduleRoutesRequireAuth pins that the whole surface — repo-scoped and
// global alike — sits behind operator auth.
func TestScheduleRoutesRequireAuth(t *testing.T) {
	x := newScheduleServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)

	// A fresh client with no session cookie.
	anon := &http.Client{}
	for _, path := range []string{
		"/api/v1/repos/" + repo.ID + "/schedules",
		"/api/v1/schedule-flows",
		"/api/v1/cron/preview?expr=" + url.QueryEscape("0 6 * * *"),
	} {
		resp := doWith(t, anon, x.ts.URL, "GET", path, nil, nil)
		wantStatus(t, resp, http.StatusUnauthorized)
		_ = resp.Body.Close()
	}
}
