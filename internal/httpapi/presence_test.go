package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// fakeDeviceHash is a well-formed 64-char lowercase hex SHA-256 stand-in.
var fakeDeviceHash = strings.Repeat("ab", 32)

func TestPresenceRoundTrip(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	// Open the stream that owns this conn's presence and keep it live —
	// Update no-ops without it (the no-resurrection rule).
	stream := x.do("GET", "/api/v1/events?conn=roundtrip", nil, nil)
	wantStatus(t, stream, http.StatusOK)
	defer func() { _ = stream.Body.Close() }()

	// Report visible: the device becomes present.
	resp := x.do("POST", "/api/v1/presence",
		map[string]any{"conn": "roundtrip", "device": fakeDeviceHash, "visible": true},
		csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if !x.presence.Visible(fakeDeviceHash) {
		t.Fatal("device not visible after a visible:true beacon")
	}

	// Report hidden: the same conn flips the device back to not-present.
	resp = x.do("POST", "/api/v1/presence",
		map[string]any{"conn": "roundtrip", "device": fakeDeviceHash, "visible": false},
		csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if x.presence.Visible(fakeDeviceHash) {
		t.Fatal("device still visible after a visible:false beacon")
	}
}

func TestPresenceUnknownConnDoesNotResurrect(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	// No stream ever registered "ghost": Update must no-op, so the beacon is
	// accepted (204) but changes nothing — a stray beacon can never fabricate
	// presence for a dead or never-seen connection.
	resp := x.do("POST", "/api/v1/presence",
		map[string]any{"conn": "ghost", "device": fakeDeviceHash, "visible": true},
		csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if x.presence.Visible(fakeDeviceHash) {
		t.Fatal("unknown conn resurrected presence")
	}
}

func TestPresenceValidation(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing conn", map[string]any{"device": fakeDeviceHash, "visible": true}, http.StatusBadRequest},
		{"conn too long", map[string]any{"conn": strings.Repeat("c", maxPresenceConn+1), "visible": true}, http.StatusBadRequest},
		{"uppercase device", map[string]any{"conn": "c1", "device": strings.ToUpper(fakeDeviceHash), "visible": true}, http.StatusBadRequest},
		{"short device", map[string]any{"conn": "c1", "device": "abcd", "visible": true}, http.StatusBadRequest},
		{"empty device ok", map[string]any{"conn": "c1", "device": "", "visible": true}, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := x.do("POST", "/api/v1/presence", tt.body, csrfHeaders(x.ts.URL))
			wantStatus(t, resp, tt.want)
			_ = resp.Body.Close()
		})
	}
}

func TestPresenceRequiresAuth(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	// No cookie client: requireAuth rejects before the handler runs.
	resp := doWith(t, http.DefaultClient, x.ts.URL, "POST", "/api/v1/presence",
		map[string]any{"conn": "c1", "device": fakeDeviceHash, "visible": true}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestPresenceDisabledLeavesRouteUnmounted(t *testing.T) {
	x := newTestServer(t, func(o *Options) { o.Presence = nil })
	x.setup("op", "password123")

	// Nil registry: the beacon route is not mounted, so it falls through to
	// the JSON 404 catch-all (Origin is still supplied so CSRF passes cleanly
	// to that 404, not a 403).
	resp := x.do("POST", "/api/v1/presence",
		map[string]any{"conn": "c1", "device": fakeDeviceHash, "visible": true},
		csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()

	// The SSE stream still serves — the conn param is simply ignored, no panic.
	stream := x.do("GET", "/api/v1/events?conn=c1", nil, nil)
	wantStatus(t, stream, http.StatusOK)
	if ct := stream.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	_ = stream.Body.Close()
}
