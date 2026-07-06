//go:build !ui

package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlaceholderServesHintPage(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	for _, path := range []string{"/", "/repos/abc", "/settings"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s content-type = %q, want text/html", path, ct)
		}
		if !strings.Contains(string(body), "UI not embedded") {
			t.Fatalf("%s body missing the not-embedded hint", path)
		}
	}
}

func TestPlaceholderRejectsMutatingMethods(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}
}
