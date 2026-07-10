package httpapi

// httptest coverage for the run-title PATCH (issue #111): the title is a pure
// display overlay — set/clear round-trips, the validation 400s, the 404
// mapping, the GET reflection, and the run.changed event other tabs' rails
// refetch on.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAPI_RunTitlePatch(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	runID, _ := startRun(t, x)

	// Set: the response echoes the trimmed value.
	resp := x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"title": "  Fix login flow  "}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp)["title"]; got != "Fix login flow" {
		t.Errorf("title = %v, want the trimmed value", got)
	}

	// GET /runs/{id} reflects the stored overlay.
	resp = x.do("GET", "/api/v1/runs/"+runID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp)["title"]; got != "Fix login flow" {
		t.Errorf("GET title = %v, want Fix login flow", got)
	}

	// JSON null clears a previously-set title.
	resp = x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"title": nil}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp)["title"]; got != nil {
		t.Errorf("cleared title = %v, want null", got)
	}
}

func TestAPI_RunTitlePatchValidation(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	runID, _ := startRun(t, x)

	// Whitespace-only is a clear (NULL), not an error.
	resp := x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"title": "   "}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp)["title"]; got != nil {
		t.Errorf("whitespace title = %v, want null", got)
	}

	// The cap counts RUNES: 120 multi-byte runes pass, 121 are a 400.
	resp = x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"title": strings.Repeat("ä", 120)}, h)
	wantStatus(t, resp, http.StatusOK)
	_ = decodeBody(t, resp)
	resp = x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"title": strings.Repeat("ä", 121)}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := decodeBody(t, resp); got["error"] == "" {
		t.Fatal("400 without error message")
	}

	// Unknown field and wrong type → 400.
	resp = x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"nonsense": 1}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = decodeBody(t, resp)
	resp = x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"title": 7}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = decodeBody(t, resp)

	// Unknown run → 404.
	resp = x.do("PATCH", "/api/v1/runs/run_missing", map[string]any{"title": "x"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)
}

// A successful title PATCH publishes run.changed with the pinned SSE payload
// keys (type/repoID) so other tabs' rails refetch.
func TestAPI_RunTitlePatchPublishesRunChanged(t *testing.T) {
	x := newInstanceServer(t)
	h := csrfHeaders(x.ts.URL)
	runID, _ := startRun(t, x)

	// Subscribe AFTER the spawn so its own run.changed is not in the log.
	log := recordBus(t, x.bus)
	resp := x.do("PATCH", "/api/v1/runs/"+runID, map[string]any{"title": "Renamed"}, h)
	wantStatus(t, resp, http.StatusOK)
	_ = decodeBody(t, resp)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range log.snapshot() {
			if p, ok := ev.Payload.(runChangedPayload); ok &&
				p.Type == "run.changed" && p.RepoID == x.repo.ID {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no run.changed with repoID %s after the title PATCH; events = %v", x.repo.ID, log.snapshot())
}
