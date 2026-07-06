//go:build ui

package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Runs only under -tags ui, which requires internal/webui/dist to exist
// (make build-ui / nix preBuild copy it there).
func TestEmbeddedSPAFallback(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return resp.StatusCode, string(b)
	}

	code, index := get("/")
	if code != http.StatusOK || index == "" {
		t.Fatalf("/ = %d, body %q", code, index)
	}

	// Unknown non-API path falls back to the SPA shell.
	code, body := get("/repos/repo_123/issues")
	if code != http.StatusOK || body != index {
		t.Fatalf("SPA fallback: status %d, body == index: %v", code, body == index)
	}
}
