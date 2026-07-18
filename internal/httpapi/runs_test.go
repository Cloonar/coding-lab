package httpapi

// httptest coverage for the run-title PATCH (issue #111): the title is a pure
// display overlay — set/clear round-trips, the validation 400s, the 404
// mapping, the GET reflection, and the run.changed event other tabs' rails
// refetch on.
//
// Also covers handleRunGet's exposed_secrets enrichment (issue #108): the
// chat header's exposure warning badge is sourced from GET /api/v1/runs/{id}
// alone — the runs LIST endpoint deliberately never carries it.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
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
// keys (type/repoID/runID — a rename concerns exactly one run, issue #175) so
// other tabs' rails refetch and sibling-run chats can skip it.
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
				p.Type == "run.changed" && p.RepoID == x.repo.ID && p.RunID == runID {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no run.changed with repoID %s after the title PATCH; events = %v", x.repo.ID, log.snapshot())
}

// TestAPI_RunGet_exposedSecrets covers handleRunGet's enrichment (issue
// #108): GET /api/v1/runs/{id} carries the sorted names of secrets this run's
// transcript has exposed, a run with none omits the key entirely (omitempty),
// and the runs LIST endpoint never carries the field at all (N+1 avoidance —
// it is a chat-header-only concern).
func TestAPI_RunGet_exposedSecrets(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	ctx := context.Background()

	// A fresh run has exposed nothing: the key is entirely absent.
	resp := x.do("GET", "/api/v1/runs/"+runID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got, ok := decodeBody(t, resp)["exposed_secrets"]; ok {
		t.Errorf("exposed_secrets on a clean run = %v, want key absent", got)
	}

	// Seed two secrets and mark both exposed by this run (out-of-band, as the
	// tailer would). ExposedSecretNamesForRun sorts by name.
	for _, name := range []string{"ZEBRA_KEY", "ALPHA_KEY"} {
		if _, err := x.st.CreateRepoSecret(ctx, ids.NewID("sec"), x.repo.ID, name, "", []byte("sealed"), instClock); err != nil {
			t.Fatalf("CreateRepoSecret %s: %v", name, err)
		}
		if _, err := x.st.MarkRepoSecretExposed(ctx, x.repo.ID, name, runID, instClock); err != nil {
			t.Fatalf("MarkRepoSecretExposed %s: %v", name, err)
		}
	}

	resp = x.do("GET", "/api/v1/runs/"+runID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	raw, ok := body["exposed_secrets"].([]any)
	if !ok {
		t.Fatalf("exposed_secrets missing or wrong type: %v", body)
	}
	got := make([]string, len(raw))
	for i, v := range raw {
		got[i] = v.(string)
	}
	want := []string{"ALPHA_KEY", "ZEBRA_KEY"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("exposed_secrets = %v, want %v", got, want)
	}

	// The runs LIST endpoint never carries the field, regardless of exposure.
	resp = x.do("GET", "/api/v1/runs?repo="+x.repo.ID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	listBody := decodeBody(t, resp)
	runs, ok := listBody["runs"].([]any)
	if !ok || len(runs) == 0 {
		t.Fatalf("runs list = %v, want at least one run", listBody)
	}
	for _, item := range runs {
		row := item.(map[string]any)
		if row["id"] != runID {
			continue
		}
		if got, ok := row["exposed_secrets"]; ok {
			t.Errorf("runs list row for %s carries exposed_secrets = %v, want key absent", runID, got)
		}
	}
}
