package httpapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
)

// oneCLIAPIStub is a stand-in for the sidecar's REST API: it answers
// GET /v1/health with the given status and body. Tests never touch a live
// OneCLI (ADR-0067: "Tests run against a stubbed HTTP API, never a live
// sidecar").
func oneCLIAPIStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// oneCLIClient builds the client the way cmd/lab does, minus the key file.
func oneCLIClient(t *testing.T, baseURL string) *onecli.Client {
	t.Helper()
	c, err := onecli.New(onecli.Options{BaseURL: baseURL, APIKey: "oc_proj_test"})
	if err != nil {
		t.Fatalf("onecli.New: %v", err)
	}
	return c
}

// liveGateway opens a real listener and returns its http:// URL. ProbeGateway
// dials TCP and nothing more, so an accept-and-close listener is a complete
// stand-in for the gateway proxy.
func liveGateway(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String()
}

// deadAddr returns a host:port that was bound and immediately released, so a
// dial to it is refused rather than left hanging.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// oneCLIHealth fetches the endpoint as an authenticated operator and returns
// the decoded body, asserting the pinned 200 on the way through: the state is
// the payload, never the HTTP code (ADR-0067).
func (x *testServer) oneCLIHealth() map[string]any {
	x.t.Helper()
	resp := x.do("GET", "/api/v1/onecli/health", nil, nil)
	wantStatus(x.t, resp, http.StatusOK)
	return decodeBody(x.t, resp)
}

// component pulls one of the two component objects out of a decoded body.
func component(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	c, ok := body[name].(map[string]any)
	if !ok {
		t.Fatalf("body[%q] = %#v, want an object (full body %#v)", name, body[name], body)
	}
	return c
}

func wantComponent(t *testing.T, body map[string]any, name string, configured, reachable bool) map[string]any {
	t.Helper()
	c := component(t, body, name)
	if c["configured"] != configured || c["reachable"] != reachable {
		t.Fatalf("%s = %#v, want configured=%v reachable=%v", name, c, configured, reachable)
	}
	return c
}

// TestOneCLIHealthUnconfigured pins the default lab: nothing wired, the route
// still mounted (never a 404), state "off", both components unconfigured. An
// unconfigured lab is not an unhealthy lab.
func TestOneCLIHealthUnconfigured(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	body := x.oneCLIHealth()
	if body["state"] != oneCLIStateOff {
		t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateOff, body)
	}
	api := wantComponent(t, body, "api", false, false)
	gw := wantComponent(t, body, "gateway", false, false)
	// Nothing configured means nothing to report: no URLs, and no error —
	// "off" is a complete answer, not a failure with a reason.
	if api["url"] != nil || api["error"] != nil || gw["url"] != nil || gw["error"] != nil {
		t.Fatalf("unconfigured body carries url/error fields: %#v", body)
	}
}

// TestOneCLIHealthStates walks the state machine against real sockets: a stub
// REST API and a real TCP listener for the reachable halves, a
// bound-then-closed port for the dead ones.
func TestOneCLIHealthStates(t *testing.T) {
	const healthBody = `{"status":"ok","version":"1.2.3"}`

	t.Run("both reachable is ok", func(t *testing.T) {
		stub := oneCLIAPIStub(t, http.StatusOK, healthBody)
		gwURL := liveGateway(t)
		x := newTestServer(t, func(o *Options) {
			o.OneCLI = oneCLIClient(t, stub.URL)
			o.OneCLIAPIURL = stub.URL
			o.OneCLIGatewayURL = gwURL
		})
		x.setup("op", "password123")

		body := x.oneCLIHealth()
		if body["state"] != oneCLIStateOK {
			t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateOK, body)
		}
		api := wantComponent(t, body, "api", true, true)
		gw := wantComponent(t, body, "gateway", true, true)
		// Both URLs are echoed back deliberately — "which address is lab
		// dialing" is half of what makes this endpoint useful.
		if api["url"] != stub.URL || gw["url"] != gwURL {
			t.Fatalf("urls = %v / %v, want %q / %q", api["url"], gw["url"], stub.URL, gwURL)
		}
		if api["error"] != nil || gw["error"] != nil {
			t.Fatalf("healthy body carries an error: %#v", body)
		}
	})

	t.Run("dead gateway is degraded", func(t *testing.T) {
		stub := oneCLIAPIStub(t, http.StatusOK, healthBody)
		x := newTestServer(t, func(o *Options) {
			o.OneCLI = oneCLIClient(t, stub.URL)
			o.OneCLIAPIURL = stub.URL
			o.OneCLIGatewayURL = "http://" + deadAddr(t)
		})
		x.setup("op", "password123")

		body := x.oneCLIHealth()
		if body["state"] != oneCLIStateDegraded {
			t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateDegraded, body)
		}
		wantComponent(t, body, "api", true, true)
		gw := wantComponent(t, body, "gateway", true, false)
		msg, _ := gw["error"].(string)
		if msg == "" {
			t.Fatalf("unreachable gateway reported no error: %#v", gw)
		}
	})

	t.Run("api answering 500 is degraded", func(t *testing.T) {
		stub := oneCLIAPIStub(t, http.StatusInternalServerError, `{"error":"boom"}`)
		gwURL := liveGateway(t)
		x := newTestServer(t, func(o *Options) {
			o.OneCLI = oneCLIClient(t, stub.URL)
			o.OneCLIAPIURL = stub.URL
			o.OneCLIGatewayURL = gwURL
		})
		x.setup("op", "password123")

		body := x.oneCLIHealth()
		if body["state"] != oneCLIStateDegraded {
			t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateDegraded, body)
		}
		api := wantComponent(t, body, "api", true, false)
		wantComponent(t, body, "gateway", true, true)
		msg, _ := api["error"].(string)
		if msg == "" {
			t.Fatalf("failing API reported no error: %#v", api)
		}
		// The message is the *APIError's: method, path, status, server
		// message — and structurally never the API key, which only ever
		// travels in a header (internal/onecli's secret-hygiene rule).
		if !strings.Contains(msg, "500") {
			t.Errorf("error %q does not name the status", msg)
		}
		if strings.Contains(msg, "oc_proj_test") {
			t.Fatalf("the API key leaked into the payload: %q", msg)
		}
	})

	t.Run("both dead is unreachable", func(t *testing.T) {
		apiURL := "http://" + deadAddr(t)
		x := newTestServer(t, func(o *Options) {
			o.OneCLI = oneCLIClient(t, apiURL)
			o.OneCLIAPIURL = apiURL
			o.OneCLIGatewayURL = "http://" + deadAddr(t)
		})
		x.setup("op", "password123")

		body := x.oneCLIHealth()
		if body["state"] != oneCLIStateUnreachable {
			t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateUnreachable, body)
		}
		api := wantComponent(t, body, "api", true, false)
		gw := wantComponent(t, body, "gateway", true, false)
		if api["error"] == nil || gw["error"] == nil {
			t.Fatalf("unreachable components reported no errors: %#v", body)
		}
	})

	t.Run("gateway only, no api client", func(t *testing.T) {
		// The gateway URL is deliberately outside the REST config's pairing
		// rule (ADR-0067), so this is a supported deployment, not a broken
		// one: one configured component, reachable, therefore "ok".
		gwURL := liveGateway(t)
		x := newTestServer(t, func(o *Options) {
			o.OneCLIGatewayURL = gwURL
		})
		x.setup("op", "password123")

		body := x.oneCLIHealth()
		if body["state"] != oneCLIStateOK {
			t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateOK, body)
		}
		api := wantComponent(t, body, "api", false, false)
		wantComponent(t, body, "gateway", true, true)
		if api["url"] != nil {
			t.Fatalf("unconfigured api echoed a url: %#v", api)
		}
	})

	t.Run("gateway only and dead is unreachable", func(t *testing.T) {
		x := newTestServer(t, func(o *Options) {
			o.OneCLIGatewayURL = "http://" + deadAddr(t)
		})
		x.setup("op", "password123")

		body := x.oneCLIHealth()
		if body["state"] != oneCLIStateUnreachable {
			t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateUnreachable, body)
		}
		wantComponent(t, body, "api", false, false)
		wantComponent(t, body, "gateway", true, false)
	})

	t.Run("api url without a client stays unconfigured", func(t *testing.T) {
		// OneCLIAPIURL is reporting-only: it can never make the component
		// look configured on its own, or a half-wired server would claim an
		// integration it cannot use.
		x := newTestServer(t, func(o *Options) {
			o.OneCLIAPIURL = "http://127.0.0.1:10254"
		})
		x.setup("op", "password123")

		body := x.oneCLIHealth()
		if body["state"] != oneCLIStateOff {
			t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateOff, body)
		}
		api := wantComponent(t, body, "api", false, false)
		if api["url"] != nil {
			t.Fatalf("unconfigured api echoed a url: %#v", api)
		}
	})
}

// TestOneCLIHealthRequiresAuth proves requireAuth is really on the route: a
// user exists (so this is not just "nobody is set up yet"), the client carries
// no session, and the answer is 401 — never the 200 the endpoint gives every
// other caller.
func TestOneCLIHealthRequiresAuth(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.OneCLIGatewayURL = liveGateway(t)
	})
	x.setup("op", "password123") // x.client's jar now holds a session

	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/onecli/health", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	// Same request WITH the session: 200. The 401 above was the auth guard,
	// not a missing route.
	body := x.oneCLIHealth()
	if body["state"] != oneCLIStateOK {
		t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateOK, body)
	}
}

// TestOneCLIHealthRedactsGatewayUserinfo pins the one credential that can
// legitimately live in a URL lab echoes back. A proxy URL may carry userinfo;
// the password must never reach the payload.
func TestOneCLIHealthRedactsGatewayUserinfo(t *testing.T) {
	live := strings.TrimPrefix(liveGateway(t), "http://")
	x := newTestServer(t, func(o *Options) {
		o.OneCLIGatewayURL = "http://user:pw@" + live
	})
	x.setup("op", "password123")

	body := x.oneCLIHealth()
	if body["state"] != oneCLIStateOK {
		t.Fatalf("state = %v, want %q (body %#v)", body["state"], oneCLIStateOK, body)
	}
	gw := wantComponent(t, body, "gateway", true, true)
	got, _ := gw["url"].(string)
	if strings.Contains(got, "pw") {
		t.Fatalf("gateway url %q still carries the userinfo password", got)
	}
	// The rest of the address survives — a redacted URL an operator cannot
	// recognize is no better than no URL at all.
	if !strings.Contains(got, "user") || !strings.Contains(got, live) {
		t.Fatalf("gateway url %q lost the address (want user@%s, password redacted)", got, live)
	}

	// Whole-body check: nothing else (an error string, say) smuggles it out.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	if strings.Contains(string(raw), `pw@`) {
		t.Fatalf("payload carries the userinfo credential: %s", raw)
	}
}

// TestOneCLIState table-tests the derivation directly, across every
// combination of the two components' three reachable states (unconfigured,
// configured-and-down, configured-and-up) — the handler can only reach a
// subset of these, and the rule is supposed to be total.
func TestOneCLIState(t *testing.T) {
	var (
		off  = oneCLIComponentHealth{}
		down = oneCLIComponentHealth{Configured: true}
		up   = oneCLIComponentHealth{Configured: true, Reachable: true}
	)
	tests := []struct {
		name    string
		api     oneCLIComponentHealth
		gateway oneCLIComponentHealth
		want    string
	}{
		{"neither configured", off, off, oneCLIStateOff},
		{"both up", up, up, oneCLIStateOK},
		{"both down", down, down, oneCLIStateUnreachable},
		{"api up, gateway down", up, down, oneCLIStateDegraded},
		{"api down, gateway up", down, up, oneCLIStateDegraded},
		{"api up, gateway unconfigured", up, off, oneCLIStateOK},
		{"api down, gateway unconfigured", down, off, oneCLIStateUnreachable},
		{"gateway up, api unconfigured", off, up, oneCLIStateOK},
		{"gateway down, api unconfigured", off, down, oneCLIStateUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := oneCLIState(tt.api, tt.gateway); got != tt.want {
				t.Errorf("oneCLIState(%+v, %+v) = %q, want %q", tt.api, tt.gateway, got, tt.want)
			}
		})
	}

	// Total on the shapes the handler cannot build, too. An unconfigured
	// component claiming reachability must never manufacture health — it is
	// skipped, not counted.
	ghost := oneCLIComponentHealth{Reachable: true}
	if got := oneCLIState(ghost, ghost); got != oneCLIStateOff {
		t.Errorf("oneCLIState(unconfigured-but-reachable × 2) = %q, want %q", got, oneCLIStateOff)
	}
	if got := oneCLIState(ghost, down); got != oneCLIStateUnreachable {
		t.Errorf("oneCLIState(unconfigured-but-reachable, down) = %q, want %q", got, oneCLIStateUnreachable)
	}
	// And on no components at all.
	if got := oneCLIState(); got != oneCLIStateOff {
		t.Errorf("oneCLIState() = %q, want %q", got, oneCLIStateOff)
	}
}

// TestOneCLIHealthBodyMatchesOpsDoc pins the exact JSON docs/ops.md shows
// operators under "OneCLI credential gateway → Checking it works", with that
// section's own two example URLs. The doc is the spec for this endpoint
// (ADR-0067), so a field rename or reordering here has to update it — and this
// test is what makes that a build failure rather than documentation drift.
func TestOneCLIHealthBodyMatchesOpsDoc(t *testing.T) {
	const want = `{"state":"ok","api":{"configured":true,"reachable":true,"url":"http://127.0.0.1:10254","status":"ok"},` +
		`"gateway":{"configured":true,"reachable":true,"url":"http://10.88.0.1:10255"}}`

	api := oneCLIComponentHealth{Configured: true, Reachable: true, URL: "http://127.0.0.1:10254", Status: "ok"}
	gateway := oneCLIComponentHealth{Configured: true, Reachable: true, URL: "http://10.88.0.1:10255"}
	got, err := json.Marshal(oneCLIHealthResponse{State: oneCLIState(api, gateway), API: api, Gateway: gateway})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("all-healthy body =\n%s\ndocs/ops.md shows\n%s", got, want)
	}
}
