package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// --- test fixtures ---------------------------------------------------------

// samplePayload is a representative notification for the tests.
func samplePayload() Payload {
	return Payload{Title: "Run finished", Body: "afk/98 is green", Tag: "run-98", Route: "/runs/98"}
}

// browserKeys mints a cryptographically valid subscription keypair the way a
// real browser's PushManager would: an ECDH P-256 public point (65 raw bytes,
// base64url) as p256dh and 16 random bytes (base64url) as the auth secret.
// webpush-go encrypts the payload to these BEFORE any HTTP happens, so the
// fake gateway only ever sees ciphertext — bogus keys would fail encryption,
// not the request.
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

// testStore opens a migrated sqlite store on a temp file. It mirrors
// testutil.TempStore but is inlined here so package push's internal test does
// not depend on that helper (and to keep this file self-contained per the
// brief's fallback).
func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(),
		"sqlite:"+filepath.Join(t.TempDir(), "lab.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

// seedSub inserts a subscription whose keys are freshly minted browser keys
// and whose endpoint points at the given gateway URL.
func seedSub(t *testing.T, st *store.Store, endpoint, label string) store.PushSubscription {
	t.Helper()
	p256dh, auth := browserKeys(t)
	sub, err := st.UpsertPushSubscription(context.Background(), endpoint, p256dh, auth, label)
	if err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return sub
}

func testKey(t *testing.T) Key {
	t.Helper()
	key, err := GenerateKey(filepath.Join(t.TempDir(), "vapid.key"))
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

// --- collecting slog handler ----------------------------------------------

type logRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]any
}

// collector is a minimal slog.Handler that captures every emitted record so a
// test can assert on level and message. It flattens attrs (including any set
// via WithAttrs, e.g. NewSender's component=push) into a map.
type collector struct {
	mu      *sync.Mutex
	records *[]logRecord
	with    []slog.Attr
}

func newCollector() *collector {
	return &collector{mu: &sync.Mutex{}, records: &[]logRecord{}}
}

func (c *collector) Enabled(context.Context, slog.Level) bool { return true }

func (c *collector) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	for _, a := range c.with {
		attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.records = append(*c.records, logRecord{Level: r.Level, Msg: r.Message, Attrs: attrs})
	return nil
}

func (c *collector) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &collector{mu: c.mu, records: c.records, with: append(append([]slog.Attr{}, c.with...), attrs...)}
}

func (c *collector) WithGroup(string) slog.Handler { return c }

func (c *collector) find(level slog.Level, msg string) *logRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range *c.records {
		if (*c.records)[i].Level == level && (*c.records)[i].Msg == msg {
			return &(*c.records)[i]
		}
	}
	return nil
}

func newTestSender(t *testing.T, st *store.Store) (*Sender, *collector) {
	t.Helper()
	c := newCollector()
	s := NewSender(st, testKey(t), slog.New(c))
	return s, c
}

// --- capturing fake gateway -----------------------------------------------

// captureGateway is a fake push service that records every request it
// receives (headers + body) and answers with a fixed status code.
type captureGateway struct {
	mu       sync.Mutex
	requests []capturedRequest
}

type capturedRequest struct {
	Path            string
	TTL             string
	Urgency         string
	Authorization   string
	ContentEncoding string
	BodyLen         int
}

func (g *captureGateway) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, capturedRequest{
		Path:            r.URL.Path,
		TTL:             r.Header.Get("TTL"),
		Urgency:         r.Header.Get("Urgency"),
		Authorization:   r.Header.Get("Authorization"),
		ContentEncoding: r.Header.Get("Content-Encoding"),
		BodyLen:         len(body),
	})
}

func (g *captureGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

func (g *captureGateway) snapshot() []capturedRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]capturedRequest{}, g.requests...)
}

// --- tests -----------------------------------------------------------------

//  1. Happy path: two subscriptions → two well-formed requests reach the fake
//     gateway; rows survive.
func TestBroadcastHappyPath(t *testing.T) {
	gw := &captureGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gw.record(r)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	st := testStore(t)
	seedSub(t, st, srv.URL+"/ep/a", "device-a")
	seedSub(t, st, srv.URL+"/ep/b", "device-b")

	s, _ := newTestSender(t, st)
	s.Broadcast(samplePayload())
	s.Flush()

	if got := gw.count(); got != 2 {
		t.Fatalf("gateway received %d requests, want 2", got)
	}
	wantKPrefix := "vapid t="
	wantK := "k=" + s.PublicKeyB64()
	for _, req := range gw.snapshot() {
		if req.TTL != "86400" {
			t.Errorf("TTL header = %q, want 86400", req.TTL)
		}
		if req.Urgency != "high" {
			t.Errorf("Urgency header = %q, want high", req.Urgency)
		}
		if req.ContentEncoding != "aes128gcm" {
			t.Errorf("Content-Encoding = %q, want aes128gcm", req.ContentEncoding)
		}
		if req.BodyLen == 0 {
			t.Error("push body is empty, want encrypted ciphertext")
		}
		if !hasPrefix(req.Authorization, wantKPrefix) {
			t.Errorf("Authorization %q lacks vapid scheme prefix %q", req.Authorization, wantKPrefix)
		}
		if !containsSub(req.Authorization, wantK) {
			t.Errorf("Authorization %q does not carry %q (the sender's public key)", req.Authorization, wantK)
		}
	}

	// Rows remain: a 2xx never reaps a subscription.
	subs, err := st.PushSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Errorf("after successful broadcast %d rows remain, want 2", len(subs))
	}
}

// 2. Gateway 410 / 404 → the row is reaped and an Info record is emitted.
func TestExpiredSubscriptionReaped(t *testing.T) {
	for _, status := range []int{http.StatusGone, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			st := testStore(t)
			sub := seedSub(t, st, srv.URL+"/ep/dead", "dead-device")

			s, c := newTestSender(t, st)
			s.Send(sub, samplePayload())
			s.Flush()

			subs, err := st.PushSubscriptions(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(subs) != 0 {
				t.Errorf("subscription survived a %d, want it reaped", status)
			}
			rec := c.find(slog.LevelInfo, "push subscription expired; removed")
			if rec == nil {
				t.Fatalf("no Info 'push subscription expired; removed' record for status %d", status)
			}
			if rec.Attrs["subscription"] != sub.ID {
				t.Errorf("Info record subscription = %v, want %s", rec.Attrs["subscription"], sub.ID)
			}
			if got := asInt(rec.Attrs["status"]); got != status {
				t.Errorf("Info record status = %v, want %d", rec.Attrs["status"], status)
			}
		})
	}
}

// 3. Gateway 500 → the row REMAINS and an Error record is emitted.
func TestServerErrorKeepsRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st := testStore(t)
	sub := seedSub(t, st, srv.URL+"/ep/flaky", "flaky-device")

	s, c := newTestSender(t, st)
	s.Send(sub, samplePayload())
	s.Flush()

	subs, err := st.PushSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Errorf("subscription reaped on a 500, want it kept (%d rows)", len(subs))
	}
	rec := c.find(slog.LevelError, "web push send failed")
	if rec == nil {
		t.Fatal("no Error 'web push send failed' record for a 500")
	}
	if got := asInt(rec.Attrs["status"]); got != http.StatusInternalServerError {
		t.Errorf("Error record status = %v, want 500", rec.Attrs["status"])
	}
	// The endpoint path is a capability token: it must not leak into the log.
	if host, _ := rec.Attrs["host"].(string); host == "" {
		t.Error("Error record has no host attr")
	}
	if _, ok := rec.Attrs["endpoint"]; ok {
		t.Error("Error record leaks the full endpoint URL")
	}
}

//  4. Unreachable gateway (server already closed) → Error record, no panic,
//     row remains.
func TestUnreachableGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // the endpoint now points at a closed port

	st := testStore(t)
	seedSub(t, st, deadURL+"/ep/gone", "gone-device")

	s, c := newTestSender(t, st)
	s.Broadcast(samplePayload())
	s.Flush()

	subs, err := st.PushSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Errorf("subscription reaped on a transport error, want it kept (%d rows)", len(subs))
	}
	if rec := c.find(slog.LevelError, "web push send failed"); rec == nil {
		t.Fatal("no Error 'web push send failed' record for an unreachable gateway")
	}
}

//  5. Never-blocks: even against a gateway that sleeps 2s, Broadcast returns
//     almost immediately; the request still lands after Flush.
func TestBroadcastNeverBlocks(t *testing.T) {
	var landed sync.WaitGroup
	landed.Add(1)
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(landed.Done)
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	st := testStore(t)
	seedSub(t, st, srv.URL+"/ep/slow", "slow-device")

	s, _ := newTestSender(t, st)
	start := time.Now()
	s.Broadcast(samplePayload())
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Broadcast blocked for %v, want well under 500ms", elapsed)
	}

	s.Flush() // now waits out the 2s send
	landed.Wait()
}

// 6. Send targets exactly the one subscription's endpoint.
func TestSendSingleTarget(t *testing.T) {
	gw := &captureGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gw.record(r)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	st := testStore(t)
	// Two rows exist, but Send must touch only the one it is handed.
	target := seedSub(t, st, srv.URL+"/ep/target", "target")
	seedSub(t, st, srv.URL+"/ep/other", "other")

	s, _ := newTestSender(t, st)
	s.Send(target, samplePayload())
	s.Flush()

	reqs := gw.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("gateway received %d requests, want exactly 1", len(reqs))
	}
	if reqs[0].Path != "/ep/target" {
		t.Errorf("Send hit path %q, want /ep/target", reqs[0].Path)
	}
}

// --- tiny string helpers (avoid importing strings just for two calls) ------

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func containsSub(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	default:
		return -1
	}
}
