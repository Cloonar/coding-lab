package httpapi

// Repo secret surface (issue #104): write-only per-repo secrets — mirrors
// credentials_test.go's no-readback discipline scoped to a repo, plus
// labels_test.go's cross-repo guard.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

const testSecretValue = "the-value-sekrit-9921"

// newRepoSecretsTestServer builds a logged-in test server with a vault
// (mounting the secrets routes) and a controllable clock: each call to
// s.now() advances by a minute, so create/rotate timestamps are always
// distinguishable — a real wall clock can tick twice inside one test with an
// unchanged millisecond, which would falsely pass an "updated_at bumped"
// assertion.
func newRepoSecretsTestServer(t *testing.T) (*testServer, *vault.Vault) {
	t.Helper()
	vlt, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	clock := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	x := newTestServer(t, func(o *Options) {
		o.Vault = vlt
		o.Now = func() time.Time {
			now := clock
			clock = clock.Add(time.Minute)
			return now
		}
	})
	x.setup("op", "password123")
	return x, vlt
}

// assertNoRepoSecretValue fails when a known plaintext secret value appears
// in body — the response leak assertion every handler test in this file runs.
func assertNoRepoSecretValue(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{testSecretValue, "rotated-" + testSecretValue, "other-val"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks a secret value %q: %s", secret, body)
		}
	}
}

func mustUnmarshalMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal response %q: %v", body, err)
	}
	return m
}

// secretsOf pulls the secrets array out of a decoded list response.
func secretsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["secrets"].([]any)
	if !ok {
		t.Fatalf("no secrets array in %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestRepoSecretCreateListAndNoReadback(t *testing.T) {
	x, _ := newRepoSecretsTestServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/secrets"

	// Create: 201 metadata only, no value field anywhere in the body.
	resp := x.do("POST", base, map[string]any{
		"name": "Z_KEY", "description": "last alphabetically", "value": testSecretValue,
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	body := readBody(t, resp)
	assertNoRepoSecretValue(t, body)
	zKey := mustUnmarshalMap(t, body)
	for _, k := range []string{"id", "name", "description", "created_at", "updated_at"} {
		if zKey[k] == "" || zKey[k] == nil {
			t.Errorf("create response missing %q: %v", k, zKey)
		}
	}
	// A freshly created secret is never exposed: both keys present, both null.
	for _, k := range []string{"exposed_run_id", "exposed_at"} {
		if v, ok := zKey[k]; !ok || v != nil {
			t.Errorf("create response %q = %v (ok=%v), want present and null", k, v, ok)
		}
	}
	if len(zKey) != 7 {
		t.Errorf("create response has extra fields: %v", zKey)
	}
	if zKey["name"] != "Z_KEY" || zKey["description"] != "last alphabetically" {
		t.Fatalf("created secret = %v", zKey)
	}

	resp = x.do("POST", base, map[string]any{"name": "A_KEY", "value": "other-val"}, h)
	wantStatus(t, resp, http.StatusCreated)
	body = readBody(t, resp)
	assertNoRepoSecretValue(t, body)
	aKey := mustUnmarshalMap(t, body)

	// List: metadata only, ordered by name, never a value.
	resp = x.do("GET", base, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	assertNoRepoSecretValue(t, body)
	list := secretsOf(t, mustUnmarshalMap(t, body))
	if len(list) != 2 {
		t.Fatalf("listed %d secrets, want 2", len(list))
	}
	if list[0]["name"] != "A_KEY" || list[1]["name"] != "Z_KEY" {
		t.Fatalf("list order = [%v, %v], want [A_KEY, Z_KEY]", list[0]["name"], list[1]["name"])
	}
	if list[0]["id"] != aKey["id"] {
		t.Fatalf("A_KEY id mismatch: list %v, create %v", list[0]["id"], aKey["id"])
	}

	// Duplicate name within the repo -> 409, no leak.
	resp = x.do("POST", base, map[string]any{"name": "A_KEY", "value": "irrelevant"}, h)
	wantStatus(t, resp, http.StatusConflict)
	body = readBody(t, resp)
	if got := mustUnmarshalMap(t, body)["error"]; got != store.ErrNameTaken.Error() {
		t.Fatalf("duplicate error = %v, want %q", got, store.ErrNameTaken.Error())
	}

	// Same name in a different repo is fine (scoped uniqueness).
	other := seedTrackerRepo(t, x, "other", nil)
	resp = x.do("POST", "/api/v1/repos/"+other.ID+"/secrets",
		map[string]any{"name": "A_KEY", "value": "yet-another"}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = readBody(t, resp)

	// Unauthenticated list -> 401.
	resp = doWith(t, http.DefaultClient, x.ts.URL, "GET", base, nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestRepoSecretCreateValidation(t *testing.T) {
	x, _ := newRepoSecretsTestServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/secrets"

	// Invalid name -> 400 with the pinned grammar message; value never echoed.
	resp := x.do("POST", base, map[string]any{"name": "not valid", "value": testSecretValue}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	body := readBody(t, resp)
	assertNoRepoSecretValue(t, body)
	if got := mustUnmarshalMap(t, body)["error"]; got != repoSecretGrammarMessage {
		t.Fatalf("error = %v, want %q", got, repoSecretGrammarMessage)
	}

	// Empty value -> 400.
	resp = x.do("POST", base, map[string]any{"name": "EMPTY_VALUE", "value": ""}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = readBody(t, resp)

	// Unknown repo -> 404.
	resp = x.do("POST", "/api/v1/repos/repo_missing/secrets",
		map[string]any{"name": "X", "value": "v"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = readBody(t, resp)

	// Nothing was actually created by the rejected requests.
	resp = x.do("GET", base, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if list := secretsOf(t, decodeBody(t, resp)); len(list) != 0 {
		t.Fatalf("secrets after only-rejected creates = %v, want none", list)
	}
}

func TestRepoSecretRotateAndDelete(t *testing.T) {
	x, vlt := newRepoSecretsTestServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	other := seedTrackerRepo(t, x, "other", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/secrets"

	resp := x.do("POST", base, map[string]any{"name": "ROTATE_ME", "value": testSecretValue}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	id := created["id"].(string)
	createdUpdatedAt := created["updated_at"].(string)

	// Rotate: 200 fresh metadata, no leak, updated_at bumped, stored blob
	// changed (verified via the store handle + vault decrypt — the API itself
	// never returns the value).
	resp = x.do("PATCH", base+"/"+id, map[string]any{"value": "rotated-" + testSecretValue}, h)
	wantStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	assertNoRepoSecretValue(t, body)
	rotated := mustUnmarshalMap(t, body)
	if rotated["updated_at"] == createdUpdatedAt {
		t.Fatalf("rotate did not bump updated_at: still %v", rotated["updated_at"])
	}
	if rotated["created_at"] != created["created_at"] {
		t.Fatalf("rotate changed created_at: %v -> %v", created["created_at"], rotated["created_at"])
	}

	values, err := x.st.RepoSecretValues(context.Background(), repo.ID, []string{"ROTATE_ME"})
	if err != nil {
		t.Fatalf("RepoSecretValues: %v", err)
	}
	plaintext, err := vlt.Decrypt(values["ROTATE_ME"])
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plaintext) != "rotated-"+testSecretValue {
		t.Fatalf("stored value after rotate = %q, want %q", plaintext, "rotated-"+testSecretValue)
	}

	// Empty value on rotate -> 400.
	resp = x.do("PATCH", base+"/"+id, map[string]any{"value": ""}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = readBody(t, resp)

	// Rotate through ANOTHER repo's path -> 404, never cross-repo access.
	resp = x.do("PATCH", "/api/v1/repos/"+other.ID+"/secrets/"+id,
		map[string]any{"value": "stolen"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = readBody(t, resp)
	// The value under the legitimate repo is untouched by the cross-repo attempt.
	values, err = x.st.RepoSecretValues(context.Background(), repo.ID, []string{"ROTATE_ME"})
	if err != nil {
		t.Fatalf("RepoSecretValues after cross-repo rotate attempt: %v", err)
	}
	plaintext, err = vlt.Decrypt(values["ROTATE_ME"])
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(plaintext) != "rotated-"+testSecretValue {
		t.Fatalf("cross-repo rotate attempt mutated the value: %q", plaintext)
	}

	// Delete through ANOTHER repo's path -> 404; the secret survives.
	resp = x.do("DELETE", "/api/v1/repos/"+other.ID+"/secrets/"+id, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = readBody(t, resp)
	if _, err := x.st.RepoSecretByID(context.Background(), id); err != nil {
		t.Fatalf("secret gone after cross-repo delete attempt: %v", err)
	}

	// Delete through the OWNING repo -> 204, then the list is empty.
	resp = x.do("DELETE", base+"/"+id, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	resp = x.do("GET", base, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if list := secretsOf(t, decodeBody(t, resp)); len(list) != 0 {
		t.Fatalf("secrets after delete = %v, want none", list)
	}

	// Unknown id -> 404 for both rotate and delete.
	resp = x.do("PATCH", base+"/sec_missing", map[string]any{"value": "x"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = readBody(t, resp)
	resp = x.do("DELETE", base+"/sec_missing", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = readBody(t, resp)

	// Unauthenticated rotate -> 401.
	resp = doWith(t, http.DefaultClient, x.ts.URL, "PATCH", base+"/"+id,
		map[string]any{"value": "x"}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

// TestRepoSecretExposureSurfacesAndClears exercises the read-side of issue
// #108's exposure flag through the LIST endpoint: store.MarkRepoSecretExposed
// is the tailer's write path (out of scope here — arranged directly against
// the store), but the settings page only ever learns about it by listing.
func TestRepoSecretExposureSurfacesAndClears(t *testing.T) {
	x, _ := newRepoSecretsTestServer(t)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID + "/secrets"

	resp := x.do("POST", base, map[string]any{"name": "LEAKY", "value": testSecretValue}, h)
	wantStatus(t, resp, http.StatusCreated)
	leaky := decodeBody(t, resp)
	resp = x.do("POST", base, map[string]any{"name": "SAFE", "value": "other-val"}, h)
	wantStatus(t, resp, http.StatusCreated)

	// Mark LEAKY exposed by a run's transcript (the tailer's write path —
	// exposed_run_id carries no FK, so an arbitrary run id is fine here).
	exposedAt := time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)
	flipped, err := x.st.MarkRepoSecretExposed(context.Background(), repo.ID, "LEAKY", "run_leaker", exposedAt)
	if err != nil {
		t.Fatalf("MarkRepoSecretExposed: %v", err)
	}
	if !flipped {
		t.Fatal("MarkRepoSecretExposed did not flip unset -> exposed")
	}

	// List: LEAKY carries exposed_run_id + exposed_at; SAFE carries null for both.
	resp = x.do("GET", base, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list := secretsOf(t, decodeBody(t, resp))
	byName := make(map[string]map[string]any, len(list))
	for _, item := range list {
		byName[item["name"].(string)] = item
	}
	if got := byName["LEAKY"]["exposed_run_id"]; got != "run_leaker" {
		t.Errorf("LEAKY exposed_run_id = %v, want run_leaker", got)
	}
	if got := byName["LEAKY"]["exposed_at"]; got != store.FormatTime(exposedAt) {
		t.Errorf("LEAKY exposed_at = %v, want %v", got, store.FormatTime(exposedAt))
	}
	for _, k := range []string{"exposed_run_id", "exposed_at"} {
		if v, ok := byName["SAFE"][k]; !ok || v != nil {
			t.Errorf("SAFE %q = %v (ok=%v), want present and null", k, v, ok)
		}
	}

	// Rotate LEAKY -> the exposure flag clears (RotateRepoSecret's job), and
	// the NEXT list reflects it: both fields back to null.
	resp = x.do("PATCH", base+"/"+leaky["id"].(string), map[string]any{"value": "rotated-" + testSecretValue}, h)
	wantStatus(t, resp, http.StatusOK)
	rotated := decodeBody(t, resp)
	for _, k := range []string{"exposed_run_id", "exposed_at"} {
		if v, ok := rotated[k]; !ok || v != nil {
			t.Errorf("rotate response %q = %v (ok=%v), want present and null", k, v, ok)
		}
	}

	resp = x.do("GET", base, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list = secretsOf(t, decodeBody(t, resp))
	for _, item := range list {
		if item["name"] != "LEAKY" {
			continue
		}
		for _, k := range []string{"exposed_run_id", "exposed_at"} {
			if v, ok := item[k]; !ok || v != nil {
				t.Errorf("post-rotate list LEAKY %q = %v (ok=%v), want present and null", k, v, ok)
			}
		}
	}
}
