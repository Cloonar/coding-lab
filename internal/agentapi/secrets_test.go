package agentapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// seedSecret encrypts value with the fixture's vault and stores it as a repo
// secret — the exact path the operator's `secret set` will take: the store
// only ever sees ciphertext.
func (f *testFixture) seedSecret(t *testing.T, repoID, name, description, value string) {
	t.Helper()
	blob, err := f.vlt.Encrypt([]byte(value))
	if err != nil {
		t.Fatalf("encrypt %s: %v", name, err)
	}
	if _, err := f.st.CreateRepoSecret(context.Background(), ids.NewID("sec"), repoID, name, description, blob, f.now); err != nil {
		t.Fatalf("CreateRepoSecret %s: %v", name, err)
	}
}

// TestSecretListMetadataOnly pins that GET /secrets returns name + description
// only — never an id, a timestamp, or (above all) the value.
func TestSecretListMetadataOnly(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "prod api key", "sup3r-s3cr3t-value")
	f.seedSecret(t, "repo_a", "DB_URL", "", "postgres://who:cares@host/db")

	rr := doJSON(t, f.server().Handler(), "GET", "/agent/v1/secrets", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Names + descriptions present, ordered by name (store order).
	var resp struct {
		Secrets []secretMeta `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(resp.Secrets) != 2 {
		t.Fatalf("secrets = %+v, want 2", resp.Secrets)
	}
	if resp.Secrets[0].Name != "API_KEY" || resp.Secrets[0].Description != "prod api key" {
		t.Errorf("first = %+v, want API_KEY/prod api key", resp.Secrets[0])
	}
	if resp.Secrets[1].Name != "DB_URL" {
		t.Errorf("second = %+v, want DB_URL", resp.Secrets[1])
	}

	// The raw body must carry no value and no leaked metadata field.
	for _, forbidden := range []string{"sup3r-s3cr3t-value", "postgres://", "\"id\"", "created_at", "updated_at", "encrypted"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("list body leaked %q: %s", forbidden, body)
		}
	}
}

// TestSecretValuesRoundTrip pins the decrypt path: an encrypted secret comes
// back as its plaintext, keyed by name.
func TestSecretValuesRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "plaintext-A")
	f.seedSecret(t, "repo_a", "B_KEY", "", "plaintext-B")

	rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/values", token, `{"names":["API_KEY","B_KEY"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Values["API_KEY"] != "plaintext-A" || resp.Values["B_KEY"] != "plaintext-B" {
		t.Fatalf("values = %v, want the two plaintexts", resp.Values)
	}
}

// TestSecretValuesPartialMiss pins the all-or-nothing rule: one unknown name
// among knowns is a 404 naming ONLY the missing (sorted), with NO values.
func TestSecretValuesPartialMiss(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "plaintext-A")

	rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/values", token,
		`{"names":["API_KEY","ZEBRA","ALPHA"]}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want 404", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Missing named, sorted; the known name is not the subject of the error and
	// no value (or values key) is present.
	if !strings.Contains(body, "unknown secret(s): ALPHA, ZEBRA") {
		t.Errorf("body = %q, want sorted missing names", body)
	}
	for _, forbidden := range []string{"plaintext-A", "\"values\""} {
		if strings.Contains(body, forbidden) {
			t.Errorf("partial-miss body leaked %q: %s", forbidden, body)
		}
	}
}

// TestSecretValuesEmptyNames pins that an empty (or missing) names list is a
// 400 — a values fetch must name at least one secret.
func TestSecretValuesEmptyNames(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)

	for _, body := range []string{`{"names":[]}`, `{}`} {
		rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/values", token, body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rr.Code)
		}
	}
}

// TestSecretScopingCrossRepo pins the run-token scope: a run in repo A cannot
// see or fetch repo B's secret — B's name simply does not exist in A's scope,
// so a list is empty and a values fetch is the same 404 as any unknown name.
func TestSecretScopingCrossRepo(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRepo(t, "repo_b")
	f.seedRun(t, "run_a", "repo_a", "active")
	f.seedRun(t, "run_b", "repo_b", "active")
	tokenA := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_b", "SHARED", "b's secret", "b-plaintext")

	handler := f.server().Handler()

	// A's list does not contain B's secret.
	rr := doJSON(t, handler, "GET", "/agent/v1/secrets", tokenA, "")
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "SHARED") {
		t.Fatalf("A list = %d %s, want 200 without SHARED", rr.Code, rr.Body.String())
	}

	// A fetching B's name by value: 404, no plaintext.
	rr = doJSON(t, handler, "POST", "/agent/v1/secrets/values", tokenA, `{"names":["SHARED"]}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("A fetch B: status = %d, want 404", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "b-plaintext") {
		t.Errorf("cross-repo fetch leaked B's plaintext: %s", rr.Body.String())
	}
}

// TestSecretRoutesRequireAuth pins that both new routes sit behind the run
// token: no header and a garbage token are the same opaque 401.
func TestSecretRoutesRequireAuth(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	handler := f.server().Handler()

	routes := []struct{ method, path, body string }{
		{"GET", "/agent/v1/secrets", ""},
		{"POST", "/agent/v1/secrets/values", `{"names":["API_KEY"]}`},
	}
	for _, rt := range routes {
		for _, authz := range []string{"", "Bearer lab_run_bogus"} {
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			if authz != "" {
				req.Header.Set("Authorization", authz)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s %s auth=%q: status = %d, want 401", rt.method, rt.path, authz, rr.Code)
			}
		}
	}
}
