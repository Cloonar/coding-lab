//go:build !ui

package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Untagged builds serve the "UI not embedded" hint page at / (design §8).
// The `ui` counterpart lives in root_ui_test.go — what the root serves is a
// build-tag property, so the assertion has to follow the tag.
func TestRootServesUIPlaceholder(t *testing.T) {
	x := newTestServer(t, nil)
	resp := x.do("GET", "/", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "UI not embedded") {
		t.Fatal("root did not serve the placeholder page")
	}
}
