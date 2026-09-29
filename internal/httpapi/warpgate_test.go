package httpapi

// Warpgate SSH bastion health (issue #39 / ADR-0068).
// Most of this runs against in-memory fakes of the two seams (WarpgateAPI,
// WarpgateHostKeyPin), because what these handlers own is the fold and the
// status mapping, not the wire; the warpgate package's own tests pin the wire
// against a stubbed admin API. One test drives the REAL *warpgate.HostKeyPin
// (with an injected scan), so the fold is proven
// against the statuses the pin actually returns rather than
// against this file's idea of them.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

const (
	testWarpgateURL     = "https://localhost:8888"
	testWarpgateSSHAddr = "10.88.0.1:2222"
	testFingerprintA    = "SHA256:AAAAtestfingerprintAAAA"
	testFingerprintB    = "SHA256:BBBBtestfingerprintBBBB"
)

// --- fakes ------------------------------------------------------------------

// fakeHostKeyPin is an in-memory WarpgateHostKeyPin: Check answers a canned
// status and records its calls.
type fakeHostKeyPin struct {
	mu     sync.Mutex
	check  warpgate.HostKeyStatus
	checks int
}

func (p *fakeHostKeyPin) Check(context.Context) warpgate.HostKeyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks++
	return p.check
}

func (p *fakeHostKeyPin) checkCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checks
}

func pinnedStatus(fp string) warpgate.HostKeyStatus {
	return warpgate.HostKeyStatus{State: warpgate.HostKeyPinned, Pinned: []string{fp}, Observed: []string{fp}}
}

// --- harness ----------------------------------------------------------------

// warpgateWiring is what a health test configures; the zero value is the
// default, unconfigured lab. Fakes are set only when non-nil, so a test can
// never smuggle a typed-nil into an interface field by accident.
type warpgateWiring struct {
	api     *fakeWarpgateAPI
	apiURL  string
	sshAddr string
	pin     *fakeHostKeyPin
}

func newWarpgateHealthServer(t *testing.T, wiring warpgateWiring) *testServer {
	t.Helper()
	x := newTestServer(t, func(o *Options) {
		if wiring.api != nil {
			o.Warpgate = wiring.api
		}
		if wiring.pin != nil {
			o.WarpgateHostKeys = wiring.pin
		}
		o.WarpgateAPIURL = wiring.apiURL
		o.WarpgateSSHAddr = wiring.sshAddr
	})
	x.setup("op", "password123")
	return x
}

// warpgateHealth fetches the endpoint as an authenticated operator, asserting
// the pinned 200 on the way through (the state is the payload), and returns
// both the decoded and the raw body.
func (x *testServer) warpgateHealth() (map[string]any, string) {
	x.t.Helper()
	resp := x.do("GET", "/api/v1/warpgate/health", nil, nil)
	wantStatus(x.t, resp, http.StatusOK)
	raw := rawBody(x.t, resp)
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		x.t.Fatalf("decode health body %q: %v", raw, err)
	}
	return body, raw
}

func wantWarpgateState(t *testing.T, body map[string]any, want string) {
	t.Helper()
	if body["state"] != want {
		t.Fatalf("state = %v, want %q (body %#v)", body["state"], want, body)
	}
}

// fullyWired is the healthy, fully configured lab every degraded case starts
// from and breaks exactly one thing of.
func fullyWired() warpgateWiring {
	return warpgateWiring{
		api:     &fakeWarpgateAPI{info: warpgate.Info{Version: "0.29.1", Authenticated: true}},
		apiURL:  testWarpgateURL,
		sshAddr: testWarpgateSSHAddr,
		pin:     &fakeHostKeyPin{check: pinnedStatus(testFingerprintA)},
	}
}

// --- health -----------------------------------------------------------------

// TestWarpgateHealthOff pins the default lab: route mounted (never a 404),
// state "off", both components unconfigured with nothing to report, and no
// hostKey object at all.
func TestWarpgateHealthOff(t *testing.T) {
	x := newWarpgateHealthServer(t, warpgateWiring{})

	body, raw := x.warpgateHealth()
	wantWarpgateState(t, body, warpgateStateOff)
	api := wantComponent(t, body, "api", false, false)
	ssh := wantComponent(t, body, "ssh", false, false)
	for _, field := range []string{"url", "version", "authenticated", "error"} {
		if _, ok := api[field]; ok {
			t.Errorf("unconfigured api carries %q: %#v", field, api)
		}
	}
	for _, field := range []string{"addr", "error"} {
		if _, ok := ssh[field]; ok {
			t.Errorf("unconfigured ssh carries %q: %#v", field, ssh)
		}
	}
	if strings.Contains(raw, "hostKey") {
		t.Fatalf("unconfigured body carries a hostKey: %s", raw)
	}
}

// TestWarpgateHealthOK is the fully wired, healthy lab, and pins the exact
// fields the SPA reads.
func TestWarpgateHealthOK(t *testing.T) {
	wiring := fullyWired()
	x := newWarpgateHealthServer(t, wiring)

	body, raw := x.warpgateHealth()
	wantWarpgateState(t, body, warpgateStateOK)
	api := wantComponent(t, body, "api", true, true)
	if api["url"] != testWarpgateURL || api["version"] != "0.29.1" || api["authenticated"] != true {
		t.Fatalf("api = %#v, want url/version/authenticated reported", api)
	}
	ssh := wantComponent(t, body, "ssh", true, true)
	if ssh["addr"] != testWarpgateSSHAddr {
		t.Fatalf("ssh.addr = %v, want %q", ssh["addr"], testWarpgateSSHAddr)
	}
	if api["error"] != nil || ssh["error"] != nil {
		t.Fatalf("healthy body carries an error: %s", raw)
	}
	hostKey := component(t, body, "hostKey")
	if hostKey["state"] != "pinned" {
		t.Fatalf("hostKey = %#v, want pinned", hostKey)
	}
	if !strings.Contains(raw, `"pinned":["`+testFingerprintA+`"]`) || !strings.Contains(raw, `"observed":["`+testFingerprintA+`"]`) {
		t.Fatalf("hostKey fingerprints missing: %s", raw)
	}
	if wiring.pin.checkCount() != 1 {
		t.Errorf("the pin was checked %d times, want once per health read", wiring.pin.checkCount())
	}
}

// TestWarpgateHealthDegradedAndUnreachable walks every way a configured lab
// stops being "ok". The HTTP status is 200 in every one of them
// (warpgateHealth asserts it).
func TestWarpgateHealthDegradedAndUnreachable(t *testing.T) {
	scanFailed := warpgate.HostKeyStatus{State: warpgate.HostKeyUnreachable, Pinned: []string{testFingerprintA}, Observed: []string{}, Error: "dial tcp 10.88.0.1:2222: connect: connection refused"}

	t.Run("api unreachable only", func(t *testing.T) {
		wiring := fullyWired()
		wiring.api.healthErr = errors.New("warpgate GET /@warpgate/api/info: dial tcp: connection refused")
		x := newWarpgateHealthServer(t, wiring)

		body, _ := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateDegraded)
		api := wantComponent(t, body, "api", true, false)
		if msg, _ := api["error"].(string); !strings.Contains(msg, "connection refused") {
			t.Fatalf("api.error = %v, want the probe's error", api["error"])
		}
		// No answer means no version and no authenticated verdict: the SPA
		// must not read "token rejected" off an API it never reached.
		if _, ok := api["authenticated"]; ok {
			t.Fatalf("unreachable api carries authenticated: %#v", api)
		}
		if _, ok := api["version"]; ok {
			t.Fatalf("unreachable api carries a version: %#v", api)
		}
		if api["url"] != testWarpgateURL {
			t.Fatalf("api.url = %v, want the configured address even when it is down", api["url"])
		}
		wantComponent(t, body, "ssh", true, true)
	})

	t.Run("ssh unreachable only", func(t *testing.T) {
		wiring := fullyWired()
		wiring.pin.check = scanFailed
		x := newWarpgateHealthServer(t, wiring)

		body, _ := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateDegraded)
		wantComponent(t, body, "api", true, true)
		ssh := wantComponent(t, body, "ssh", true, false)
		if ssh["error"] != scanFailed.Error {
			t.Fatalf("ssh.error = %v, want the scan's error %q", ssh["error"], scanFailed.Error)
		}
		hostKey := component(t, body, "hostKey")
		if hostKey["state"] != "unreachable" || hostKey["error"] != scanFailed.Error {
			t.Fatalf("hostKey = %#v, want unreachable with the scan error", hostKey)
		}
	})

	t.Run("both unreachable", func(t *testing.T) {
		wiring := fullyWired()
		wiring.api.healthErr = errors.New("warpgate GET /@warpgate/api/info: connection refused")
		wiring.pin.check = scanFailed
		x := newWarpgateHealthServer(t, wiring)

		body, _ := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateUnreachable)
		wantComponent(t, body, "api", true, false)
		wantComponent(t, body, "ssh", true, false)
	})

	t.Run("token not accepted as admin", func(t *testing.T) {
		wiring := fullyWired()
		// Upstream reveals the version only to an authenticated caller.
		wiring.api.info = warpgate.Info{}
		x := newWarpgateHealthServer(t, wiring)

		body, raw := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateDegraded)
		wantComponent(t, body, "api", true, true)
		// Pinned literally: false must be PRESENT, not omitted — the SPA reads
		// `authenticated === false` as "token rejected".
		if !strings.Contains(raw, `"authenticated":false`) {
			t.Fatalf("body = %s, want an explicit authenticated:false", raw)
		}
	})

	t.Run("host key mismatch", func(t *testing.T) {
		wiring := fullyWired()
		wiring.pin.check = warpgate.HostKeyStatus{State: warpgate.HostKeyMismatch, Pinned: []string{testFingerprintA}, Observed: []string{testFingerprintB}}
		x := newWarpgateHealthServer(t, wiring)

		body, _ := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateDegraded)
		// The listener answered — a mismatch is a reachable listener with the
		// wrong key, not an outage.
		wantComponent(t, body, "ssh", true, true)
		hostKey := component(t, body, "hostKey")
		if hostKey["state"] != "mismatch" {
			t.Fatalf("hostKey = %#v, want mismatch", hostKey)
		}
		pinned, _ := hostKey["pinned"].([]any)
		observed, _ := hostKey["observed"].([]any)
		if len(pinned) != 1 || pinned[0] != testFingerprintA || len(observed) != 1 || observed[0] != testFingerprintB {
			t.Fatalf("hostKey = %#v, want pinned A and observed B", hostKey)
		}
	})

	t.Run("api only and down is unreachable", func(t *testing.T) {
		x := newWarpgateHealthServer(t, warpgateWiring{
			api:    &fakeWarpgateAPI{healthErr: errors.New("connection refused")},
			apiURL: testWarpgateURL,
		})
		body, _ := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateUnreachable)
	})
}

// TestWarpgateHealthPartialConfigurations covers the two supported halves:
// the REST pair alone (health and the identity lifecycle, no run wiring), and
// an SSH address alone. Each is "ok" when its one component answers.
func TestWarpgateHealthPartialConfigurations(t *testing.T) {
	t.Run("rest pair only: no hostKey, pin never probed", func(t *testing.T) {
		pin := &fakeHostKeyPin{check: pinnedStatus(testFingerprintA)}
		x := newWarpgateHealthServer(t, warpgateWiring{
			api:    &fakeWarpgateAPI{info: warpgate.Info{Version: "0.29.1", Authenticated: true}},
			apiURL: testWarpgateURL,
			// A pin wired without an address is not an SSH configuration:
			// the component follows the address, and the pin stays unprobed.
			pin: pin,
		})
		body, raw := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateOK)
		wantComponent(t, body, "api", true, true)
		wantComponent(t, body, "ssh", false, false)
		if strings.Contains(raw, "hostKey") {
			t.Fatalf("hostKey reported with no SSH listener configured: %s", raw)
		}
		if pin.checkCount() != 0 {
			t.Fatalf("the pin was scanned %d times with no SSH address configured", pin.checkCount())
		}
	})

	t.Run("ssh only", func(t *testing.T) {
		x := newWarpgateHealthServer(t, warpgateWiring{
			sshAddr: testWarpgateSSHAddr,
			pin:     &fakeHostKeyPin{check: pinnedStatus(testFingerprintA)},
		})
		body, _ := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateOK)
		wantComponent(t, body, "api", false, false)
		wantComponent(t, body, "ssh", true, true)
		component(t, body, "hostKey")
	})

	t.Run("address without a pin falls back to a dial and omits hostKey", func(t *testing.T) {
		live := strings.TrimPrefix(liveGateway(t), "http://")
		x := newWarpgateHealthServer(t, warpgateWiring{sshAddr: live})
		body, raw := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateOK)
		wantComponent(t, body, "ssh", true, true)
		if strings.Contains(raw, "hostKey") {
			t.Fatalf("hostKey invented without a pin: %s", raw)
		}

		dead := newWarpgateHealthServer(t, warpgateWiring{sshAddr: deadAddr(t)})
		body, _ = dead.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateUnreachable)
		ssh := wantComponent(t, body, "ssh", true, false)
		if msg, _ := ssh["error"].(string); msg == "" {
			t.Fatalf("dead listener reported no error: %#v", ssh)
		}
	})

	t.Run("api url without a client stays unconfigured", func(t *testing.T) {
		x := newWarpgateHealthServer(t, warpgateWiring{apiURL: testWarpgateURL})
		body, _ := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateOff)
		api := wantComponent(t, body, "api", false, false)
		if api["url"] != nil {
			t.Fatalf("unconfigured api echoed a url: %#v", api)
		}
	})
}

// TestWarpgateHealthTypedNilIsUnconfigured pins the nil-interface trap's
// guard: a nil *warpgate.Client and a nil *warpgate.HostKeyPin stored in the
// interface-typed Options fields — the exact mistake cmd/lab is told not to
// make — read as unconfigured, never as a configured client that panics on
// its first call.
func TestWarpgateHealthTypedNilIsUnconfigured(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		var client *warpgate.Client
		var pin *warpgate.HostKeyPin
		o.Warpgate = client
		o.WarpgateHostKeys = pin
		o.WarpgateAPIURL = testWarpgateURL
	})
	x.setup("op", "password123")

	body, _ := x.warpgateHealth()
	wantWarpgateState(t, body, warpgateStateOff)
	wantComponent(t, body, "api", false, false)

	repo := seedTrackerRepo(t, x, "typed-nil", nil)
	resp := x.do("GET", "/api/v1/repos/"+repo.ID+"/warpgate/targets", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if raw := rawBody(t, resp); !strings.Contains(raw, `"configured":false`) {
		t.Fatalf("targets body = %s, want configured:false", raw)
	}
}

// TestWarpgateHealthRedactsURLUserinfo pins that a credential in the
// configured admin URL never reaches the payload.
func TestWarpgateHealthRedactsURLUserinfo(t *testing.T) {
	wiring := fullyWired()
	wiring.apiURL = "https://admin:s3cretpw@localhost:8888"
	x := newWarpgateHealthServer(t, wiring)

	body, raw := x.warpgateHealth()
	api := wantComponent(t, body, "api", true, true)
	got, _ := api["url"].(string)
	if strings.Contains(raw, "s3cretpw") {
		t.Fatalf("payload carries the userinfo password: %s", raw)
	}
	if !strings.Contains(got, "admin") || !strings.Contains(got, "localhost:8888") {
		t.Fatalf("api.url = %q, want the address kept with the password redacted", got)
	}
}

// TestWarpgateHealthNeverNullFingerprints pins the never-null arrays even for
// a pin implementation that hands back nil slices.
func TestWarpgateHealthNeverNullFingerprints(t *testing.T) {
	wiring := fullyWired()
	wiring.pin.check = warpgate.HostKeyStatus{State: warpgate.HostKeyUnpinned}
	x := newWarpgateHealthServer(t, wiring)

	body, raw := x.warpgateHealth()
	if !strings.Contains(raw, `"pinned":[]`) || !strings.Contains(raw, `"observed":[]`) {
		t.Fatalf("body = %s, want empty arrays, never null", raw)
	}
	// No trusted key configured is the operator's choice, not a fault: ok.
	wantWarpgateState(t, body, warpgateStateOK)
	if hk := component(t, body, "hostKey"); hk["state"] != "unpinned" {
		t.Fatalf("hostKey = %#v, want unpinned", hk)
	}
}

// TestWarpgateHealthRequiresAuth proves requireAuth is on the route.
func TestWarpgateHealthRequiresAuth(t *testing.T) {
	x := newWarpgateHealthServer(t, fullyWired())

	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/warpgate/health", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()

	body, _ := x.warpgateHealth()
	wantWarpgateState(t, body, warpgateStateOK)
}

// TestWarpgateState table-tests the fold directly, including shapes the
// handler cannot build.
func TestWarpgateState(t *testing.T) {
	yes, no := true, false
	var (
		apiOff    = warpgateAPIHealth{}
		apiDown   = warpgateAPIHealth{Configured: true}
		apiUp     = warpgateAPIHealth{Configured: true, Reachable: true, Authenticated: &yes}
		apiNoAuth = warpgateAPIHealth{Configured: true, Reachable: true, Authenticated: &no}
		sshOff    = warpgateSSHHealth{}
		sshDown   = warpgateSSHHealth{Configured: true}
		sshUp     = warpgateSSHHealth{Configured: true, Reachable: true}
		pinned    = &warpgateHostKeyBody{State: "pinned"}
		mismatch  = &warpgateHostKeyBody{State: "mismatch"}
		unpinned  = &warpgateHostKeyBody{State: "unpinned"}
	)
	tests := []struct {
		name    string
		api     warpgateAPIHealth
		ssh     warpgateSSHHealth
		hostKey *warpgateHostKeyBody
		want    string
	}{
		{"nothing configured", apiOff, sshOff, nil, warpgateStateOff},
		{"all up", apiUp, sshUp, pinned, warpgateStateOK},
		{"all down", apiDown, sshDown, nil, warpgateStateUnreachable},
		{"api down, ssh up", apiDown, sshUp, pinned, warpgateStateDegraded},
		{"api up, ssh down", apiUp, sshDown, nil, warpgateStateDegraded},
		{"api only, up", apiUp, sshOff, nil, warpgateStateOK},
		{"api only, down", apiDown, sshOff, nil, warpgateStateUnreachable},
		{"api only, token rejected", apiNoAuth, sshOff, nil, warpgateStateDegraded},
		{"token rejected, ssh up", apiNoAuth, sshUp, pinned, warpgateStateDegraded},
		{"ssh only, up", apiOff, sshUp, pinned, warpgateStateOK},
		{"ssh only, down", apiOff, sshDown, nil, warpgateStateUnreachable},
		{"ssh only, mismatch", apiOff, sshUp, mismatch, warpgateStateDegraded},
		{"all up but mismatch", apiUp, sshUp, mismatch, warpgateStateDegraded},
		{"unpinned (no trusted key configured) is ok", apiUp, sshUp, unpinned, warpgateStateOK},
		{"ssh up, no pin wired", apiUp, sshUp, nil, warpgateStateOK},
		// A reachable API with no verdict at all counts as not authenticated.
		{"reachable api, no verdict", warpgateAPIHealth{Configured: true, Reachable: true}, sshOff, nil, warpgateStateDegraded},
		// An unconfigured component claiming reachability is skipped, never
		// counted.
		{"ghost components", warpgateAPIHealth{Reachable: true}, warpgateSSHHealth{Reachable: true}, nil, warpgateStateOff},
		// A mismatch reported for an unconfigured listener cannot degrade.
		{"ghost mismatch", apiUp, sshOff, mismatch, warpgateStateOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := warpgateState(tt.api, tt.ssh, tt.hostKey); got != tt.want {
				t.Errorf("warpgateState = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWarpgateHealthBodyShape pins the exact JSON of a healthy, fully wired
// lab against the HTTP contract the SPA is built on (web/src/api/warpgate.ts):
// field names, nesting, and that empty strings are omitted.
func TestWarpgateHealthBodyShape(t *testing.T) {
	const want = `{"state":"ok",` +
		`"api":{"configured":true,"reachable":true,"url":"https://localhost:8888","version":"0.29.1","authenticated":true},` +
		`"ssh":{"configured":true,"reachable":true,"addr":"10.88.0.1:2222"},` +
		`"hostKey":{"state":"pinned","pinned":["SHA256:abc"],"observed":["SHA256:abc"]}}`

	yes := true
	api := warpgateAPIHealth{Configured: true, Reachable: true, URL: testWarpgateURL, Version: "0.29.1", Authenticated: &yes}
	ssh := warpgateSSHHealth{Configured: true, Reachable: true, Addr: testWarpgateSSHAddr}
	hostKey := hostKeyBody(pinnedStatus("SHA256:abc"))
	got, err := json.Marshal(warpgateHealthResponse{State: warpgateState(api, ssh, &hostKey), API: api, SSH: ssh, HostKey: &hostKey})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != want {
		t.Errorf("healthy body =\n%s\nwant\n%s", got, want)
	}
}

// --- the real pin -----------------------------------------------------------

func newHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh public key: %v", err)
	}
	return key
}

// TestWarpgateHostKeyThroughHealth drives the REAL *warpgate.HostKeyPin
// through the health endpoint, so the fold is proven against the statuses the
// pin actually produces: with --warpgate-ssh-host-key set, a listener
// presenting the trusted key is ok, a different one degraded (mismatch), and
// a failed scan unreachable; with nothing configured, whatever the listener
// presents is unpinned — and ok, with the observed fingerprints listed for
// the operator to copy into the setting.
func TestWarpgateHostKeyThroughHealth(t *testing.T) {
	keyA, keyB := newHostKey(t), newHostKey(t)
	fpA, fpB := ssh.FingerprintSHA256(keyA), ssh.FingerprintSHA256(keyB)

	var (
		mu      sync.Mutex
		current []ssh.PublicKey
		scanErr error
	)
	scan := func(context.Context, string) ([]ssh.PublicKey, error) {
		mu.Lock()
		defer mu.Unlock()
		if scanErr != nil {
			return nil, scanErr
		}
		return current, nil
	}
	serve := func(keys []ssh.PublicKey, err error) {
		mu.Lock()
		defer mu.Unlock()
		current, scanErr = keys, err
	}

	t.Run("trusted key configured", func(t *testing.T) {
		x := newTestServer(t, func(o *Options) {
			o.WarpgateSSHAddr = testWarpgateSSHAddr
			o.WarpgateHostKeys = warpgate.NewHostKeyPin(testWarpgateSSHAddr, []ssh.PublicKey{keyA}, scan)
		})
		x.setup("op", "password123")

		serve([]ssh.PublicKey{keyA, keyB}, nil)
		body, raw := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateOK)
		if hk := component(t, body, "hostKey"); hk["state"] != "pinned" {
			t.Fatalf("hostKey = %#v, want pinned", hk)
		}
		if !strings.Contains(raw, `"pinned":["`+fpA+`"]`) || !strings.Contains(raw, `"observed":[`) || !strings.Contains(raw, fpB) {
			t.Fatalf("body = %s, want the trusted key pinned and both keys observed", raw)
		}

		// The listener presents a key that is not the trusted one: degraded,
		// and the spawn path refuses on the same comparison.
		serve([]ssh.PublicKey{keyB}, nil)
		body, raw = x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateDegraded)
		if hk := component(t, body, "hostKey"); hk["state"] != "mismatch" {
			t.Fatalf("hostKey = %#v, want mismatch", hk)
		}
		if !strings.Contains(raw, `"pinned":["`+fpA+`"]`) || !strings.Contains(raw, `"observed":["`+fpB+`"]`) {
			t.Fatalf("mismatch body = %s, want pinned A and observed B", raw)
		}

		// The listener goes away: unreachable (it is the only configured
		// component), the scan error on the ssh component.
		serve(nil, errors.New("dial tcp 10.88.0.1:2222: connect: connection refused"))
		body, _ = x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateUnreachable)
		sshComp := wantComponent(t, body, "ssh", true, false)
		if msg, _ := sshComp["error"].(string); !strings.Contains(msg, "connection refused") {
			t.Fatalf("ssh.error = %v, want the scan failure", sshComp["error"])
		}
		if hk := component(t, body, "hostKey"); hk["state"] != "unreachable" {
			t.Fatalf("hostKey = %#v, want unreachable", hk)
		}
	})

	t.Run("no trusted key configured", func(t *testing.T) {
		x := newTestServer(t, func(o *Options) {
			o.WarpgateSSHAddr = testWarpgateSSHAddr
			o.WarpgateHostKeys = warpgate.NewHostKeyPin(testWarpgateSSHAddr, nil, scan)
		})
		x.setup("op", "password123")

		serve([]ssh.PublicKey{keyB}, nil)
		body, raw := x.warpgateHealth()
		wantWarpgateState(t, body, warpgateStateOK)
		if hk := component(t, body, "hostKey"); hk["state"] != "unpinned" {
			t.Fatalf("hostKey = %#v, want unpinned", hk)
		}
		if !strings.Contains(raw, `"pinned":[]`) || !strings.Contains(raw, `"observed":["`+fpB+`"]`) {
			t.Fatalf("unpinned body = %s, want no pinned key and the observed one listed", raw)
		}
	})
}
