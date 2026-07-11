package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// newCredTestServer builds a logged-in test server with a vault, so the
// credentials routes are mounted.
func newCredTestServer(t *testing.T) *testServer {
	t.Helper()
	v, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	x := newTestServer(t, func(o *Options) { o.Vault = v })
	x.setup("op", "password123")
	return x
}

// readBody drains and closes the response body, returning it as a string —
// the raw bytes the no-secret-leak assertions scan.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

const (
	testPrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nSEKRITKEYMATERIALxyz\n-----END OPENSSH PRIVATE KEY-----"
	testToken      = "sekrit-https-token-8842"
)

// assertNoSecretBytes fails when any known payload secret appears in body.
func assertNoSecretBytes(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{"SEKRITKEYMATERIAL", testToken, "rotated-sekrit"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks payload bytes %q: %s", secret, body)
		}
	}
}

func TestCredentialCreateListAndNoReadback(t *testing.T) {
	x := newCredTestServer(t)
	h := csrfHeaders(x.ts.URL)

	// Create one credential of each kind.
	resp := x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "deploy-key", "kind": "ssh_key",
		"payload": map[string]any{"private_key": testPrivateKey, "passphrase": "pp"},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	body := readBody(t, resp)
	assertNoSecretBytes(t, body)
	var meta map[string]any
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	for _, k := range []string{"id", "name", "kind", "created_at", "updated_at"} {
		if meta[k] == "" || meta[k] == nil {
			t.Errorf("create response missing %q: %v", k, meta)
		}
	}
	if len(meta) != 5 {
		t.Errorf("create response has extra fields: %v", meta)
	}

	resp = x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "https-bot", "kind": "https_token",
		"payload": map[string]any{"username": "bot", "token": testToken},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	assertNoSecretBytes(t, readBody(t, resp))

	resp = x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "forge-api", "kind": "forge_token",
		"payload": map[string]any{"host": "git.cloonar.com", "token": testToken},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = readBody(t, resp)

	// List: metadata + referenced flag, never payloads.
	resp = x.do("GET", "/api/v1/credentials", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body = readBody(t, resp)
	assertNoSecretBytes(t, body)
	var list struct {
		Credentials []map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list.Credentials) != 3 {
		t.Fatalf("listed %d credentials, want 3", len(list.Credentials))
	}
	for _, c := range list.Credentials {
		if c["referenced"] != false {
			t.Errorf("credential %v referenced = %v, want false", c["name"], c["referenced"])
		}
		if _, ok := c["payload"]; ok {
			t.Errorf("list echoes a payload for %v", c["name"])
		}
	}

	// Duplicate name → 409.
	resp = x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "deploy-key", "kind": "ssh_key",
		"payload": map[string]any{"private_key": "other"},
	}, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = readBody(t, resp)

	// Unauthenticated list → 401.
	resp = doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/credentials", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestCredentialPayloadValidation(t *testing.T) {
	x := newCredTestServer(t)
	h := csrfHeaders(x.ts.URL)

	tests := []struct {
		name string
		body map[string]any
	}{
		{"unknown kind", map[string]any{"name": "a", "kind": "gpg", "payload": map[string]any{}}},
		{"missing payload", map[string]any{"name": "a", "kind": "ssh_key"}},
		{"ssh missing private_key", map[string]any{"name": "a", "kind": "ssh_key", "payload": map[string]any{"passphrase": "x"}}},
		{"https missing token", map[string]any{"name": "a", "kind": "https_token", "payload": map[string]any{"username": "u"}}},
		{"forge missing host", map[string]any{"name": "a", "kind": "forge_token", "payload": map[string]any{"token": testToken}}},
		{"forge bad flavor", map[string]any{"name": "a", "kind": "forge_token", "payload": map[string]any{"host": "git.cloonar.com", "token": testToken, "forge": "gitlab"}}},
		{"forge http host", map[string]any{"name": "a", "kind": "forge_token", "payload": map[string]any{"host": "http://git.cloonar.com", "token": testToken}}},
		{"forgejo host with a path", map[string]any{"name": "a", "kind": "forge_token", "payload": map[string]any{"host": "git.cloonar.com/api", "token": testToken, "forge": "forgejo"}}},
		{"forge host with userinfo", map[string]any{"name": "a", "kind": "forge_token", "payload": map[string]any{"host": "user@api.github.com", "token": testToken, "forge": "github"}}},
		{"wrong-shape payload", map[string]any{"name": "a", "kind": "ssh_key", "payload": map[string]any{"private_key": "k", "bogus": testToken}}},
		{"missing name", map[string]any{"kind": "ssh_key", "payload": map[string]any{"private_key": "k"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := x.do("POST", "/api/v1/credentials", tt.body, h)
			wantStatus(t, resp, http.StatusBadRequest)
			body := readBody(t, resp)
			assertNoSecretBytes(t, body)
			var e map[string]any
			if err := json.Unmarshal([]byte(body), &e); err != nil || e["error"] == "" {
				t.Fatalf("400 body not the canonical error shape: %s", body)
			}
		})
	}
}

// TestCredentialForgeFlavorCreate: the flavor-aware host SHAPES accepted at
// create time (ADR-0015) — a github API origin (bare or a GHE path root) and a
// forgejo bare host all 201.
func TestCredentialForgeFlavorCreate(t *testing.T) {
	x := newCredTestServer(t)
	h := csrfHeaders(x.ts.URL)
	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"github-api-origin", map[string]any{"host": "api.github.com", "token": testToken, "forge": "github"}},
		{"github-ghe-path-root", map[string]any{"host": "ghe.example.com/api/v3", "token": testToken, "forge": "github"}},
		{"forgejo-bare-host", map[string]any{"host": "git.cloonar.com", "token": testToken, "forge": "forgejo"}},
		{"absent-flavor-bare-host", map[string]any{"host": "git.cloonar.com", "token": testToken}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := x.do("POST", "/api/v1/credentials", map[string]any{
				"name": "cred-" + tc.name, "kind": "forge_token", "payload": tc.payload,
			}, h)
			wantStatus(t, resp, http.StatusCreated)
			_ = readBody(t, resp)
		})
	}
}

func TestCredentialPatchRenameAndRotate(t *testing.T) {
	x := newCredTestServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "rot", "kind": "https_token",
		"payload": map[string]any{"username": "bot", "token": testToken},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)

	// Rename only.
	resp = x.do("PATCH", "/api/v1/credentials/"+id, map[string]any{"name": "rot2"}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["name"] != "rot2" {
		t.Fatalf("renamed credential = %v", got)
	}

	// Rotate: payload validated against the stored kind, never echoed.
	resp = x.do("PATCH", "/api/v1/credentials/"+id, map[string]any{
		"payload": map[string]any{"username": "bot", "token": "rotated-sekrit"},
	}, h)
	wantStatus(t, resp, http.StatusOK)
	body := readBody(t, resp)
	assertNoSecretBytes(t, body)

	// Rotate with the WRONG kind's shape → 400, no echo.
	resp = x.do("PATCH", "/api/v1/credentials/"+id, map[string]any{
		"payload": map[string]any{"private_key": "rotated-sekrit"},
	}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	assertNoSecretBytes(t, readBody(t, resp))

	// Empty PATCH → 400 (an updated_at stamp would fake a rotation).
	resp = x.do("PATCH", "/api/v1/credentials/"+id, map[string]any{}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	_ = readBody(t, resp)

	// Unknown id → 404.
	resp = x.do("PATCH", "/api/v1/credentials/cred_missing", map[string]any{"name": "x"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = readBody(t, resp)

	// Rename collision → 409.
	resp = x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "other", "kind": "https_token",
		"payload": map[string]any{"username": "u", "token": "t"},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = readBody(t, resp)
	resp = x.do("PATCH", "/api/v1/credentials/"+id, map[string]any{"name": "other"}, h)
	wantStatus(t, resp, http.StatusConflict)
	_ = readBody(t, resp)
}

func TestCredentialDeleteReferenced(t *testing.T) {
	x := newCredTestServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "in-use", "kind": "ssh_key",
		"payload": map[string]any{"private_key": testPrivateKey},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	credID := decodeBody(t, resp)["id"].(string)

	resp = x.do("POST", "/api/v1/credentials", map[string]any{
		"name": "forge-in-use", "kind": "forge_token",
		"payload": map[string]any{"host": "git.cloonar.com", "token": testToken},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	forgeID := decodeBody(t, resp)["id"].(string)

	// A repo referencing both credentials (seeded directly — the clone
	// lifecycle is the repos suite's business).
	repo := store.Repo{
		ID: ids.NewID("repo"), Name: "ref-holder", RemoteURL: "git@git.cloonar.com:me/x.git",
		CredentialID: &credID, ForgeCredentialID: &forgeID,
		TrackerBinding: store.TrackerBindingForge, ForgeKind: "forgejo",
		DefaultBranch:    "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	}
	if _, err := x.st.CreateRepo(context.Background(), repo); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	// Both FK columns block deletion, with the pinned message.
	for _, id := range []string{credID, forgeID} {
		resp = x.do("DELETE", "/api/v1/credentials/"+id, nil, h)
		wantStatus(t, resp, http.StatusConflict)
		if got := decodeBody(t, resp); got["error"] != "credential is referenced by 1 repository" {
			t.Fatalf("409 error = %q, want the pinned referenced message", got["error"])
		}
	}

	// The list reports them as referenced.
	resp = x.do("GET", "/api/v1/credentials", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	var list struct {
		Credentials []map[string]any `json:"credentials"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	_ = resp.Body.Close()
	for _, c := range list.Credentials {
		if c["referenced"] != true {
			t.Errorf("credential %v referenced = %v, want true", c["name"], c["referenced"])
		}
	}

	// Dereference and delete.
	if err := x.st.DeleteRepo(context.Background(), repo.ID); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	for _, id := range []string{credID, forgeID} {
		resp = x.do("DELETE", "/api/v1/credentials/"+id, nil, h)
		wantStatus(t, resp, http.StatusNoContent)
		_ = resp.Body.Close()
	}

	// Gone now.
	resp = x.do("DELETE", "/api/v1/credentials/"+credID, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = readBody(t, resp)
}

// TestValidateNewCredentials pins the username/password rules shared by
// handleSetup and startup seeding (issue #134): drift here is drift there.
func TestValidateNewCredentials(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantErr  string
	}{
		{"empty username", "", "password123", "username must be 1-64 characters"},
		{"64-char username ok", strings.Repeat("u", 64), "password123", ""},
		{"65-char username rejected", strings.Repeat("u", 65), "password123", "username must be 1-64 characters"},
		{"7-char password rejected", "op", "1234567", "password must be at least 8 characters"},
		{"8-char password ok", "op", "12345678", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNewCredentials(tt.username, tt.password)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateNewCredentials(%q, %q) = %v, want nil", tt.username, tt.password, err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("ValidateNewCredentials(%q, %q) = %v, want %q", tt.username, tt.password, err, tt.wantErr)
			}
		})
	}
}

// TestValidateUsername exercises ValidateUsername directly: it is the
// standalone half ValidateNewCredentials composes, and (per issue #137) the
// one startup seeding of a pre-hashed user calls on its own.
func TestValidateUsername(t *testing.T) {
	tests := []struct {
		name     string
		username string
		wantErr  string
	}{
		{"empty rejected", "", "username must be 1-64 characters"},
		{"1-char ok", "u", ""},
		{"64-char ok", strings.Repeat("u", 64), ""},
		{"65-char rejected", strings.Repeat("u", 65), "username must be 1-64 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUsername(tt.username)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateUsername(%q) = %v, want nil", tt.username, err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("ValidateUsername(%q) = %v, want %q", tt.username, err, tt.wantErr)
			}
		})
	}
}

// TestValidateNewPassword exercises ValidateNewPassword directly: it is the
// standalone half ValidateNewCredentials composes, and (per issue #137) the
// rule `lab hash-password` applies to the plaintext it hashes.
func TestValidateNewPassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  string
	}{
		{"7-char rejected", "1234567", "password must be at least 8 characters"},
		{"8-char ok", "12345678", ""},
		{"empty rejected", "", "password must be at least 8 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNewPassword(tt.password)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateNewPassword(%q) = %v, want nil", tt.password, err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("ValidateNewPassword(%q) = %v, want %q", tt.password, err, tt.wantErr)
			}
		})
	}
}
