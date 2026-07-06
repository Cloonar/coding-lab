package httpapi

// PAT CRUD suite (D7, M5): mint-once semantics, list shape, and the
// delete-revokes-immediately property (a deleted PAT 401s on its next use).

import (
	"net/http"
	"strings"
	"testing"
)

func TestAPI_TokensLifecycle(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")
	h := csrfHeaders(x.ts.URL)

	// Name is required.
	resp := x.do("POST", "/api/v1/tokens", map[string]any{"name": "  "}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = resp.Body.Close()

	// Mint: the secret appears exactly once, in the create response.
	resp = x.do("POST", "/api/v1/tokens", map[string]any{"name": "ci"}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	token, _ := created["token"].(string)
	if !strings.HasPrefix(token, "lab_pat_") {
		t.Fatalf("token = %q, want lab_pat_ prefix", token)
	}
	if created["id"] == "" || created["name"] != "ci" || created["created_at"] == "" {
		t.Fatalf("create response = %v", created)
	}
	tokenID := created["id"].(string)

	// List: id/name/timestamps only — never the secret.
	resp = x.do("GET", "/api/v1/tokens", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	items, _ := list["tokens"].([]any)
	if len(items) != 1 {
		t.Fatalf("tokens = %v, want one", list)
	}
	entry := items[0].(map[string]any)
	if entry["id"] != tokenID || entry["name"] != "ci" || entry["last_used_at"] != nil {
		t.Errorf("list entry = %v", entry)
	}
	if _, leaked := entry["token"]; leaked {
		t.Error("list response leaks the token secret")
	}

	// The PAT authenticates (Bearer, fresh client without the cookie jar).
	bearer := map[string]string{"Authorization": "Bearer " + token}
	resp = doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil, bearer)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	// …and its use stamps last_used_at.
	resp = x.do("GET", "/api/v1/tokens", nil, nil)
	list = decodeBody(t, resp)
	entry = list["tokens"].([]any)[0].(map[string]any)
	if entry["last_used_at"] == nil {
		t.Error("last_used_at not stamped after a Bearer use")
	}

	// Delete revokes immediately: the very next Bearer use 401s.
	resp = x.do("DELETE", "/api/v1/tokens/"+tokenID, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	resp = doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/me", nil, bearer)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	resp = x.do("GET", "/api/v1/tokens", nil, nil)
	if list = decodeBody(t, resp); len(list["tokens"].([]any)) != 0 {
		t.Errorf("tokens after delete = %v, want none", list)
	}

	// Deleting the deleted (or an unknown) id is a 404.
	resp = x.do("DELETE", "/api/v1/tokens/"+tokenID, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}
