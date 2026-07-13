package httpapi

// httptest suite for the Web Push operator surface (issue #98): key
// exposure, subscription CRUD (auth + CSRF + validation + upsert
// idempotency), and the test-send path driven through the REAL sender so the
// handler→sender→store cleanup loop (a gateway 410 reaping the row) is
// exercised end to end, not mocked.

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/push"
)

// newPushTestServer builds a test Server whose Push sender runs over the
// SAME store the server uses, so a subscribe through the API is visible to
// the sender's PushSubscriptions read and vice versa. The sender reference
// is returned so tests can Flush() after triggering a send.
func newPushTestServer(t *testing.T) (*testServer, *push.Sender) {
	t.Helper()
	var sender *push.Sender
	x := newTestServer(t, func(o *Options) {
		key, err := push.GenerateKey(filepath.Join(t.TempDir(), "vapid.key"))
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		sender = push.NewSender(o.Store, key, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		o.Push = sender
	})
	x.setup("op", "password123")
	return x, sender
}

// browserKeys mints a cryptographically valid Web Push subscription keypair
// the way a real browser's PushManager would: a 65-byte uncompressed P-256
// public point (p256dh) and a 16-byte auth secret, both base64url. The
// sender encrypts to these before any HTTP happens, so a fake gateway only
// ever sees ciphertext — bogus keys would fail encryption, not the request.
func browserKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("browser keypair: %v", err)
	}
	var authSecret [16]byte
	if _, err := rand.Read(authSecret[:]); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(authSecret[:])
}

const iPhoneSafariUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 " +
	"(KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"

// --- auth + CSRF -------------------------------------------------------

func TestAPI_PushRoutesRequireAuth(t *testing.T) {
	x, _ := newPushTestServer(t)

	sub := x.do("POST", "/api/v1/push/subscriptions",
		map[string]any{"endpoint": "https://push.example.com/ep/1", "keys": map[string]any{"p256dh": "k", "auth": "a"}},
		csrfHeaders(x.ts.URL))
	wantStatus(t, sub, http.StatusCreated)
	subID := decodeBody(t, sub)["id"].(string)

	cases := []struct {
		method, path string
	}{
		{"GET", "/api/v1/push/key"},
		{"GET", "/api/v1/push/subscriptions"},
		{"POST", "/api/v1/push/subscriptions"},
		{"DELETE", "/api/v1/push/subscriptions/" + subID},
		{"POST", "/api/v1/push/subscriptions/" + subID + "/test"},
	}
	for _, c := range cases {
		resp := doWith(t, http.DefaultClient, x.ts.URL, c.method, c.path, nil, nil)
		wantStatus(t, resp, http.StatusUnauthorized)
		_ = resp.Body.Close()
	}
}

func TestAPI_PushSubscribeRequiresCSRF(t *testing.T) {
	x, _ := newPushTestServer(t)

	// Cookie auth (ambient) without the CSRF header on a mutation → 403, the
	// same rejection every other mutating route in this tree gets.
	resp := x.do("POST", "/api/v1/push/subscriptions",
		map[string]any{"endpoint": "https://push.example.com/ep/1", "keys": map[string]any{"p256dh": "k", "auth": "a"}},
		nil)
	wantStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()

	// List remains empty — the rejected request never reached the handler.
	resp = x.do("GET", "/api/v1/push/subscriptions", nil, nil)
	list := decodeBody(t, resp)
	if items := list["subscriptions"].([]any); len(items) != 0 {
		t.Errorf("subscriptions after a CSRF-rejected create = %v, want none", items)
	}
}

// --- key -----------------------------------------------------------------

func TestAPI_PushKey(t *testing.T) {
	x, sender := newPushTestServer(t)

	resp := x.do("GET", "/api/v1/push/key", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	if body["public_key"] != sender.PublicKeyB64() {
		t.Errorf("public_key = %v, want %v", body["public_key"], sender.PublicKeyB64())
	}
}

// --- subscribe / list / upsert -------------------------------------------

func TestAPI_PushSubscriptionLifecycle(t *testing.T) {
	x, _ := newPushTestServer(t)
	h := csrfHeaders(x.ts.URL)

	// Empty list to start — a slice, not null.
	resp := x.do("GET", "/api/v1/push/subscriptions", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	items, ok := list["subscriptions"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("initial subscriptions = %v, want an empty list", list["subscriptions"])
	}

	p256dh, auth := browserKeys(t)
	req := map[string]any{
		"endpoint": "https://push.example.com/ep/device-1",
		"keys":     map[string]any{"p256dh": p256dh, "auth": auth},
	}
	headers := map[string]string{"X-Lab-Csrf": h["X-Lab-Csrf"], "Origin": h["Origin"], "User-Agent": iPhoneSafariUA}
	resp = x.do("POST", "/api/v1/push/subscriptions", req, headers)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)

	id, _ := created["id"].(string)
	if !strings.HasPrefix(id, "psub_") {
		t.Fatalf("id = %q, want psub_ prefix", id)
	}
	if created["endpoint"] != req["endpoint"] {
		t.Errorf("endpoint = %v, want %v", created["endpoint"], req["endpoint"])
	}
	if created["label"] != "Safari on iPhone" {
		t.Errorf("label = %v, want %q (derived from the iPhone Safari User-Agent)", created["label"], "Safari on iPhone")
	}
	if created["created_at"] == "" || created["created_at"] == nil {
		t.Errorf("created_at = %v, want non-empty", created["created_at"])
	}
	// The keys never round-trip in the response.
	if _, leaked := created["p256dh"]; leaked {
		t.Error("create response leaks p256dh")
	}

	// List shows the one subscription.
	resp = x.do("GET", "/api/v1/push/subscriptions", nil, nil)
	list = decodeBody(t, resp)
	items = list["subscriptions"].([]any)
	if len(items) != 1 {
		t.Fatalf("subscriptions = %v, want 1", items)
	}
	entry := items[0].(map[string]any)
	if entry["id"] != id || entry["label"] != "Safari on iPhone" {
		t.Errorf("list entry = %v", entry)
	}

	// Re-subscribing the SAME endpoint (different UA this time) upserts: the
	// list stays length 1, id/created_at are preserved, and the label updates.
	req2 := map[string]any{
		"endpoint": "https://push.example.com/ep/device-1",
		"keys":     map[string]any{"p256dh": p256dh, "auth": auth},
	}
	androidChromeUA := "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Mobile Safari/537.36"
	headers2 := map[string]string{"X-Lab-Csrf": h["X-Lab-Csrf"], "Origin": h["Origin"], "User-Agent": androidChromeUA}
	resp = x.do("POST", "/api/v1/push/subscriptions", req2, headers2)
	wantStatus(t, resp, http.StatusCreated) // still 201: the client can't tell it was an update
	updated := decodeBody(t, resp)
	if updated["id"] != id {
		t.Errorf("re-subscribe id = %v, want the original %v (upsert preserves identity)", updated["id"], id)
	}
	if updated["label"] != "Chrome on Android" {
		t.Errorf("re-subscribe label = %v, want %q", updated["label"], "Chrome on Android")
	}

	resp = x.do("GET", "/api/v1/push/subscriptions", nil, nil)
	list = decodeBody(t, resp)
	items = list["subscriptions"].([]any)
	if len(items) != 1 {
		t.Fatalf("subscriptions after re-subscribe = %v, want still 1 (upsert, not a new row)", items)
	}
	if items[0].(map[string]any)["label"] != "Chrome on Android" {
		t.Errorf("list label after re-subscribe = %v, want updated", items[0].(map[string]any)["label"])
	}
}

func TestAPI_PushSubscriptionValidation(t *testing.T) {
	x, _ := newPushTestServer(t)
	h := csrfHeaders(x.ts.URL)
	p256dh, auth := browserKeys(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing endpoint", map[string]any{"keys": map[string]any{"p256dh": p256dh, "auth": auth}}},
		{"empty endpoint", map[string]any{"endpoint": "  ", "keys": map[string]any{"p256dh": p256dh, "auth": auth}}},
		{"non-url endpoint", map[string]any{"endpoint": "not-a-url", "keys": map[string]any{"p256dh": p256dh, "auth": auth}}},
		{"relative endpoint", map[string]any{"endpoint": "/ep/1", "keys": map[string]any{"p256dh": p256dh, "auth": auth}}},
		{"missing keys", map[string]any{"endpoint": "https://push.example.com/ep/1"}},
		{"missing p256dh", map[string]any{"endpoint": "https://push.example.com/ep/1", "keys": map[string]any{"auth": auth}}},
		{"missing auth", map[string]any{"endpoint": "https://push.example.com/ep/1", "keys": map[string]any{"p256dh": p256dh}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := x.do("POST", "/api/v1/push/subscriptions", c.body, h)
			wantStatus(t, resp, http.StatusBadRequest)
			_ = resp.Body.Close()
		})
	}
}

// --- delete ----------------------------------------------------------------

func TestAPI_PushSubscriptionDelete(t *testing.T) {
	x, _ := newPushTestServer(t)
	h := csrfHeaders(x.ts.URL)
	p256dh, auth := browserKeys(t)

	resp := x.do("POST", "/api/v1/push/subscriptions", map[string]any{
		"endpoint": "https://push.example.com/ep/del",
		"keys":     map[string]any{"p256dh": p256dh, "auth": auth},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)

	resp = x.do("DELETE", "/api/v1/push/subscriptions/"+id, nil, h)
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	// Deleting again → 404.
	resp = x.do("DELETE", "/api/v1/push/subscriptions/"+id, nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()

	// Unknown id → 404.
	resp = x.do("DELETE", "/api/v1/push/subscriptions/psub_doesnotexist", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// --- test-send: exercises the REAL sender end to end ----------------------

// captureGateway is a fake push service recording every request it gets and
// answering with a fixed status.
type captureGateway struct {
	mu       sync.Mutex
	requests []capturedRequest
}

type capturedRequest struct {
	ContentEncoding string
}

func (g *captureGateway) handler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests = append(g.requests, capturedRequest{ContentEncoding: r.Header.Get("Content-Encoding")})
		g.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (g *captureGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

func TestAPI_PushSubscriptionTestSend(t *testing.T) {
	x, sender := newPushTestServer(t)
	h := csrfHeaders(x.ts.URL)

	gw := &captureGateway{}
	srv := httptest.NewServer(gw.handler(http.StatusCreated))
	defer srv.Close()

	p256dh, auth := browserKeys(t)
	resp := x.do("POST", "/api/v1/push/subscriptions", map[string]any{
		"endpoint": srv.URL + "/ep/live",
		"keys":     map[string]any{"p256dh": p256dh, "auth": auth},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)

	resp = x.do("POST", "/api/v1/push/subscriptions/"+id+"/test", nil, h)
	wantStatus(t, resp, http.StatusAccepted)
	_ = resp.Body.Close()

	sender.Flush()

	if got := gw.count(); got != 1 {
		t.Fatalf("gateway received %d requests, want exactly 1", got)
	}
	if got := gw.requests[0].ContentEncoding; got != "aes128gcm" {
		t.Errorf("Content-Encoding = %q, want aes128gcm", got)
	}

	// The row survives a 2xx.
	resp = x.do("GET", "/api/v1/push/subscriptions", nil, nil)
	list := decodeBody(t, resp)
	if items := list["subscriptions"].([]any); len(items) != 1 {
		t.Errorf("subscriptions after a successful test send = %v, want 1", items)
	}
}

func TestAPI_PushSubscriptionTestSendReapsExpired(t *testing.T) {
	x, sender := newPushTestServer(t)
	h := csrfHeaders(x.ts.URL)

	gw := &captureGateway{}
	srv := httptest.NewServer(gw.handler(http.StatusGone))
	defer srv.Close()

	p256dh, auth := browserKeys(t)
	resp := x.do("POST", "/api/v1/push/subscriptions", map[string]any{
		"endpoint": srv.URL + "/ep/dead",
		"keys":     map[string]any{"p256dh": p256dh, "auth": auth},
	}, h)
	wantStatus(t, resp, http.StatusCreated)
	id := decodeBody(t, resp)["id"].(string)

	resp = x.do("POST", "/api/v1/push/subscriptions/"+id+"/test", nil, h)
	wantStatus(t, resp, http.StatusAccepted)
	_ = resp.Body.Close()

	// The 202 only means "handed to the sender" — the delivery (and the
	// gateway's 410) happens asynchronously; Flush drains it before we assert
	// the full handler→sender→store cleanup loop ran through the REAL sender.
	sender.Flush()

	if got := gw.count(); got != 1 {
		t.Fatalf("gateway received %d requests, want exactly 1", got)
	}

	resp = x.do("GET", "/api/v1/push/subscriptions", nil, nil)
	list := decodeBody(t, resp)
	items := list["subscriptions"].([]any)
	if len(items) != 0 {
		t.Errorf("subscriptions after a 410 test send = %v, want the dead subscription reaped", items)
	}
}

func TestAPI_PushSubscriptionTestSendUnknownID(t *testing.T) {
	x, _ := newPushTestServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/push/subscriptions/psub_doesnotexist/test", nil, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// --- unmounted when Options.Push is nil ------------------------------------

func TestAPI_PushRoutesAbsentWithoutSender(t *testing.T) {
	x := newTestServer(t, nil) // default Options: Push is nil
	x.setup("op", "password123")

	resp := x.do("GET", "/api/v1/push/key", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()

	resp = x.do("GET", "/api/v1/push/subscriptions", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// --- deviceLabel -------------------------------------------------------

func TestPushDeviceLabel(t *testing.T) {
	cases := []struct {
		name, ua, want string
	}{
		{"iPhone Safari", iPhoneSafariUA, "Safari on iPhone"},
		{"Android Chrome", "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Mobile Safari/537.36", "Chrome on Android"},
		{"Mac Firefox", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:128.0) Gecko/20100101 Firefox/128.0", "Firefox on Mac"},
		{"Windows Edge", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0", "Edge on Windows"},
		{"Linux Chrome", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36", "Chrome on Linux"},
		{"empty", "", "unknown device"},
		{"unrecognized", "SomeWeirdBot/1.0", "unknown device"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deviceLabel(c.ua); got != c.want {
				t.Errorf("deviceLabel(%q) = %q, want %q", c.ua, got, c.want)
			}
		})
	}
}
