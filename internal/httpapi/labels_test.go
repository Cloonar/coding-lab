package httpapi

// M4 label-CRUD suite (builtin repos; httptest against the real store on a
// t.TempDir sqlite DB).

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// labelsOf pulls the labels array out of a decoded list response.
func labelsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["labels"].([]any)
	if !ok {
		t.Fatalf("no labels array in %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestLabelCRUD(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	// The five triage labels are seeded at repo creation, sorted by name.
	resp := x.do("GET", base+"/labels", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	seeded := labelsOf(t, decodeBody(t, resp))
	wantNames := []string{"needs-info", "needs-triage", "ready-for-agent", "ready-for-human", "wontfix"}
	if len(seeded) != len(wantNames) {
		t.Fatalf("seeded labels = %v", seeded)
	}
	for i, l := range seeded {
		if l["name"] != wantNames[i] {
			t.Fatalf("seeded[%d] = %v, want %s", i, l["name"], wantNames[i])
		}
		if l["id"] == "" || l["color"] == "" {
			t.Fatalf("seeded label incomplete: %v", l)
		}
	}

	// Create.
	resp = x.do("POST", base+"/labels", map[string]any{"name": "bug", "color": "#ff0000", "description": "broken"}, h)
	wantStatus(t, resp, http.StatusCreated)
	bug := decodeBody(t, resp)
	bugID, _ := bug["id"].(string)
	if !strings.HasPrefix(bugID, "lbl_") || bug["color"] != "#ff0000" || bug["description"] != "broken" {
		t.Fatalf("created label = %v", bug)
	}

	// Omitted color falls back to the store default.
	resp = x.do("POST", base+"/labels", map[string]any{"name": "minor"}, h)
	wantStatus(t, resp, http.StatusCreated)
	if got := decodeBody(t, resp); got["color"] != store.LabelDefaultColor {
		t.Fatalf("default color = %v", got["color"])
	}

	// Duplicate name within the repo → 409.
	resp = x.do("POST", base+"/labels", map[string]any{"name": "bug"}, h)
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp); got["error"] != store.ErrNameTaken.Error() {
		t.Fatalf("duplicate error = %q", got["error"])
	}

	// Empty name → 400.
	resp = x.do("POST", base+"/labels", map[string]any{"name": "  "}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()

	// Update.
	resp = x.do("PATCH", base+"/labels/"+bugID, map[string]any{"name": "defect", "color": "#00ff00"}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["name"] != "defect" || got["color"] != "#00ff00" {
		t.Fatalf("updated label = %v", got)
	}

	// Rename collision → 409; unknown field → 400.
	resp = x.do("PATCH", base+"/labels/"+bugID, map[string]any{"name": "wontfix"}, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()
	resp = x.do("PATCH", base+"/labels/"+bugID, map[string]any{"priority": 1}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()

	// Delete, then the id is gone.
	resp = x.do("DELETE", base+"/labels/"+bugID, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	resp = x.do("GET", base+"/labels", nil, nil)
	for _, l := range labelsOf(t, decodeBody(t, resp)) {
		if l["id"] == bugID {
			t.Fatal("deleted label still listed")
		}
	}
	resp = x.do("DELETE", base+"/labels/"+bugID, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	resp = x.do("PATCH", base+"/labels/lbl_missing", map[string]any{"name": "x"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// TestLabelNamesTrimmed pins that label names are stored trimmed on create
// and rename: label matching is exact everywhere (ready queue, list filters,
// name resolution), so "bug " must collide with "bug" instead of coexisting
// as an invisible twin.
func TestLabelNamesTrimmed(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	// Create with padding stores the trimmed name…
	resp := x.do("POST", base+"/labels", map[string]any{"name": "  bug "}, h)
	wantStatus(t, resp, http.StatusCreated)
	bug := decodeBody(t, resp)
	if bug["name"] != "bug" {
		t.Fatalf("created name = %q, want %q", bug["name"], "bug")
	}
	// …so the exact name now collides (409), as does a padded duplicate of a
	// seeded label.
	resp = x.do("POST", base+"/labels", map[string]any{"name": "bug"}, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()
	resp = x.do("POST", base+"/labels", map[string]any{"name": " ready-for-agent "}, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = resp.Body.Close()

	// Rename trims too.
	resp = x.do("PATCH", base+"/labels/"+bug["id"].(string), map[string]any{"name": " defect  "}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["name"] != "defect" {
		t.Fatalf("renamed label = %q, want %q", got["name"], "defect")
	}

	// The trimmed name resolves in issue-label attachment.
	resp = x.do("POST", base+"/issues", map[string]any{"title": "t", "labels": []string{"defect"}}, h)
	wantStatus(t, resp, http.StatusCreated)
	if got := decodeBody(t, resp); len(got["labels"].([]any)) != 1 || got["labels"].([]any)[0] != "defect" {
		t.Fatalf("issue labels = %v, want [defect]", got["labels"])
	}
}

func TestLabelDeleteCascadesToIssues(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	resp := x.do("POST", base+"/labels", map[string]any{"name": "bug"}, h)
	wantStatus(t, resp, http.StatusCreated)
	bugID := decodeBody(t, resp)["id"].(string)

	resp = x.do("POST", base+"/issues", map[string]any{"title": "labelled", "labels": []string{"bug"}}, h)
	wantStatus(t, resp, http.StatusCreated)
	if got := decodeBody(t, resp); len(got["labels"].([]any)) != 1 {
		t.Fatalf("issue labels = %v", got["labels"])
	}

	// Deleting the label cascades its issue_labels rows (FK ON DELETE
	// CASCADE, live thanks to the pinned sqlite open recipe).
	resp = x.do("DELETE", base+"/labels/"+bugID, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	resp = x.do("GET", base+"/issues/1", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); len(got["labels"].([]any)) != 0 {
		t.Fatalf("labels after cascade = %v", got["labels"])
	}
	stored, err := x.st.IssueByRepoNumber(context.Background(), repo.ID, 1)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if len(stored.Labels) != 0 {
		t.Fatalf("stored labels after cascade = %v", stored.Labels)
	}
}

func TestLabelRepoScoping(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repoA := seedTrackerRepo(t, x, "alpha", nil)
	repoB := seedTrackerRepo(t, x, "beta", nil)
	h := csrfHeaders(x.ts.URL)

	// A label created on B is invisible through A's path — 404, never
	// cross-repo access.
	resp := x.do("POST", "/api/v1/repos/"+repoB.ID+"/labels", map[string]any{"name": "b-only"}, h)
	wantStatus(t, resp, http.StatusCreated)
	bID := decodeBody(t, resp)["id"].(string)

	resp = x.do("PATCH", "/api/v1/repos/"+repoA.ID+"/labels/"+bID, map[string]any{"name": "stolen"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	resp = x.do("DELETE", "/api/v1/repos/"+repoA.ID+"/labels/"+bID, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()

	// Untouched on B.
	resp = x.do("GET", "/api/v1/repos/"+repoB.ID+"/labels", nil, nil)
	found := false
	for _, l := range labelsOf(t, decodeBody(t, resp)) {
		if l["id"] == bID && l["name"] == "b-only" {
			found = true
		}
	}
	if !found {
		t.Fatal("label b-only mutated or lost through the wrong repo path")
	}
}
