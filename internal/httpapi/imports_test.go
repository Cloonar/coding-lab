package httpapi

// Tests for the repo imports API (issue #261 / ADR-0063): list/add/remove
// over HTTP, plus the delete-guard 409 an importED repo's own DELETE hits
// while another repo still imports it. Uses newRepoTestServer (the full M2
// stack) but creates the extra repos it needs directly through the store
// (x.st.CreateRepo, the reposvc imports_test.go readyRepoNamed idiom) rather
// than through POST /api/v1/repos, so these tests never wait on a real git
// clone — RepoByID existence is all AddImport/RemoveImport need.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// readyRepo creates a ready-clone-status repo directly through the store —
// no git remote required — for use as either side of an import declaration.
func (x *repoTestServer) readyRepo(t *testing.T, name string) store.Repo {
	t.Helper()
	r, err := x.st.CreateRepo(context.Background(), store.Repo{
		ID: ids.NewID("repo"), Name: name, RemoteURL: "/tmp/" + name,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (x *repoTestServer) listImports(t *testing.T, repoID string) []map[string]any {
	t.Helper()
	resp := x.do("GET", "/api/v1/repos/"+repoID+"/imports", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	raw, ok := body["imports"].([]any)
	if !ok {
		t.Fatalf("imports field = %#v (%T), want a JSON array", body["imports"], body["imports"])
	}
	items := make([]map[string]any, len(raw))
	for i, v := range raw {
		items[i] = v.(map[string]any)
	}
	return items
}

func TestRepoImportsListEmptyIsEmptyArray(t *testing.T) {
	x := newRepoTestServer(t)
	repo := x.readyRepo(t, "consumer")

	resp := x.do("GET", "/api/v1/repos/"+repo.ID+"/imports", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	// A JSON "null" decodes to a nil interface{}, not a []any — the type
	// assertion below fails on it, distinguishing "[]" (what we want) from
	// "null" (what an unwrapped nil slice would render as, if writeJSON
	// were ever handed one).
	arr, ok := body["imports"].([]any)
	if !ok {
		t.Fatalf("imports = %#v, want a JSON array (never null)", body["imports"])
	}
	if len(arr) != 0 {
		t.Errorf("imports = %v, want empty", arr)
	}
}

func TestRepoImportsAddListDelete(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	consumer := x.readyRepo(t, "consumer")
	target := x.readyRepo(t, "target")

	resp := x.do("POST", "/api/v1/repos/"+consumer.ID+"/imports", map[string]any{"target_repo_id": target.ID}, h)
	wantStatus(t, resp, http.StatusCreated)
	added := decodeBody(t, resp)
	if added["id"] != target.ID || added["name"] != target.Name {
		t.Errorf("POST imports body = %v, want id/name = %s/%s", added, target.ID, target.Name)
	}

	items := x.listImports(t, consumer.ID)
	if len(items) != 1 || items[0]["id"] != target.ID {
		t.Fatalf("imports after add = %v, want [%s]", items, target.ID)
	}

	resp = x.do("DELETE", "/api/v1/repos/"+consumer.ID+"/imports/"+target.ID, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	items = x.listImports(t, consumer.ID)
	if len(items) != 0 {
		t.Errorf("imports after delete = %v, want empty", items)
	}

	// Removing an already-absent import is idempotent (204, not an error).
	resp = x.do("DELETE", "/api/v1/repos/"+consumer.ID+"/imports/"+target.ID, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
}

func TestRepoImportsSelfImportRejected(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	repo := x.readyRepo(t, "solo")

	resp := x.do("POST", "/api/v1/repos/"+repo.ID+"/imports", map[string]any{"target_repo_id": repo.ID}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	body := decodeBody(t, resp)
	if !strings.Contains(fmt.Sprint(body["error"]), "itself") {
		t.Errorf("error = %v, want it to mention 'itself'", body["error"])
	}
}

func TestRepoImportsUnknownTargetRejected(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	repo := x.readyRepo(t, "solo")

	resp := x.do("POST", "/api/v1/repos/"+repo.ID+"/imports", map[string]any{"target_repo_id": "repo_00000000000000000000000000000000"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	body := decodeBody(t, resp)
	if !strings.Contains(fmt.Sprint(body["error"]), "unknown target") {
		t.Errorf("error = %v, want it to mention 'unknown target'", body["error"])
	}
}

func TestRepoImportsAddMissingTargetField(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	repo := x.readyRepo(t, "solo")

	resp := x.do("POST", "/api/v1/repos/"+repo.ID+"/imports", map[string]any{}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	body := decodeBody(t, resp)
	if !strings.Contains(fmt.Sprint(body["error"]), "target_repo_id required") {
		t.Errorf("error = %v, want 'target_repo_id required'", body["error"])
	}
}

func TestRepoImportsDeleteConflictNamesImporter(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	consumer := x.readyRepo(t, "consumer")
	target := x.readyRepo(t, "target")

	resp := x.do("POST", "/api/v1/repos/"+consumer.ID+"/imports", map[string]any{"target_repo_id": target.ID}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()

	// Deleting the imported repo — with or without force — is refused,
	// naming the importing repo (ADR-0063: force never bypasses this guard).
	for _, qs := range []string{"", "?force=true"} {
		resp = x.do("DELETE", "/api/v1/repos/"+target.ID+qs, nil, h)
		wantStatus(t, resp, http.StatusConflict)
		body := decodeBody(t, resp)
		if !strings.Contains(fmt.Sprint(body["error"]), consumer.Name) {
			t.Errorf("409 error = %v, want it to name importer %q", body["error"], consumer.Name)
		}
	}

	// Removing the import declaration clears the guard.
	resp = x.do("DELETE", "/api/v1/repos/"+consumer.ID+"/imports/"+target.ID, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	resp = x.do("DELETE", "/api/v1/repos/"+target.ID, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
}

func TestRepoImportsUnknownRepoNotFound(t *testing.T) {
	x := newRepoTestServer(t)
	h := csrfHeaders(x.ts.URL)
	target := x.readyRepo(t, "target")
	unknown := "repo_00000000000000000000000000000000"

	resp := x.do("GET", "/api/v1/repos/"+unknown+"/imports", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)

	resp = x.do("POST", "/api/v1/repos/"+unknown+"/imports", map[string]any{"target_repo_id": target.ID}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)

	resp = x.do("DELETE", "/api/v1/repos/"+unknown+"/imports/"+target.ID, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)
}
