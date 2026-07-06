//go:build ui

package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The PWA surface must get through the embed with usable content types:
// browsers refuse a service worker served as octet-stream, and the manifest
// should carry its registered type (see the init() mime registration).
func TestEmbeddedPWAAssets(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	head := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return resp.StatusCode, resp.Header.Get("Content-Type")
	}

	for _, tc := range []struct {
		path, wantType string
	}{
		{"/sw.js", "text/javascript"},
		{"/manifest.webmanifest", "application/manifest+json"},
		{"/icons/icon-192.png", "image/png"},
	} {
		code, ct := head(tc.path)
		if code != http.StatusOK {
			t.Errorf("%s = %d, want 200", tc.path, code)
		}
		if !strings.HasPrefix(ct, tc.wantType) {
			t.Errorf("%s content-type = %q, want prefix %q", tc.path, ct, tc.wantType)
		}
	}
}

// TestEmbeddedStaleAssetIs404 pins the stale-deploy contract (regression:
// the SPA fallback used to answer 200 index.html for a superseded
// /assets/index-<hash>.js — parsed as a module script it white-screens the
// app, and the service worker caches the poisoned entry): asset-like paths
// miss with a real 404 while extensionless router paths still get the shell.
func TestEmbeddedStaleAssetIs404(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	for _, p := range []string{"/assets/index-stalehash.js", "/assets/index-stalehash.css", "/logo-old.svg"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: code = %d, want 404 (never the SPA shell)", p, resp.StatusCode)
		}
	}

	resp, err := http.Get(srv.URL + "/repos/repo_123/issues")
	if err != nil {
		t.Fatalf("GET router path: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<div id=\"root\">") {
		t.Errorf("router path: code = %d, want 200 with the shell", resp.StatusCode)
	}
}
