//go:build ui

package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// `-tags ui` builds (make lab, the nix package) serve the embedded SPA
// shell at /. Mirror of root_noui_test.go for the other side of the tag.
func TestRootServesEmbeddedUI(t *testing.T) {
	x := newTestServer(t, nil)
	resp := x.do("GET", "/", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `id="root"`) {
		t.Fatalf("root did not serve the SPA shell: %.200s", body)
	}
}
