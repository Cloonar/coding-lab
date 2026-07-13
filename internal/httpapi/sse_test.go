package httpapi

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
)

func TestSSEHeartbeatAndBusEvents(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.HeartbeatInterval = 50 * time.Millisecond
	})
	x.setup("op", "password123")

	resp := x.do("GET", "/api/v1/events", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	// Publish an application event while the stream is live.
	x.bus.Publish(events.Event{Type: "repo.changed", Payload: map[string]string{"type": "repo.changed", "repoID": "repo_1"}})

	// Read until two heartbeats and the repo.changed frame arrived.
	type result struct {
		heartbeats int
		repoEvents int
		err        error
	}
	done := make(chan result, 1)
	go func() {
		var res result
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			switch scanner.Text() {
			case "event: heartbeat":
				res.heartbeats++
			case "event: repo.changed":
				res.repoEvents++
			}
			if res.heartbeats >= 2 && res.repoEvents >= 1 {
				done <- res
				return
			}
		}
		res.err = scanner.Err()
		done <- res
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("reading stream: %v", res.err)
		}
		if res.heartbeats < 2 || res.repoEvents < 1 {
			t.Fatalf("saw %d heartbeats, %d repo events", res.heartbeats, res.repoEvents)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for SSE frames")
	}

	// Client disconnect cleans the subscription up.
	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for x.bus.SubscriberCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("subscriber not cleaned up after disconnect (count %d)", x.bus.SubscriberCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSSEStreamClosesAfterLogout(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		o.HeartbeatInterval = 50 * time.Millisecond
	})
	x.setup("op", "password123")

	resp := x.do("GET", "/api/v1/events", nil, nil)
	wantStatus(t, resp, http.StatusOK)

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		done <- err
	}()

	// Logout deletes the web session; the next heartbeat tick re-validates
	// the stream's credential and must close it.
	lo := x.do("POST", "/api/v1/auth/logout", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, lo, http.StatusNoContent)
	_ = lo.Body.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stream ended with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream still open 5s (100 heartbeats) after logout")
	}
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for x.bus.SubscriberCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("subscriber not cleaned up after logout (count %d)", x.bus.SubscriberCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSSEStreamClosesOnShutdown(t *testing.T) {
	// Heartbeat is the 1h test default: only the shutdown context can end
	// the stream.
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	resp := x.do("GET", "/api/v1/events", nil, nil)
	wantStatus(t, resp, http.StatusOK)

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()

	start := time.Now()
	x.srv.CloseStreams() // what http.Server.Shutdown triggers via RegisterOnShutdown
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("stream took %v to close after CloseStreams, want < 1s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("SSE stream still open 1s after CloseStreams")
	}
	_ = resp.Body.Close()
}

func TestSSERegistersPresenceForLifetimeOfStream(t *testing.T) {
	x := newTestServer(t, nil)
	x.setup("op", "password123")

	// Registration happens-before the 200: the moment x.do returns with the
	// stream's headers, Connect has already run for this conn.
	resp := x.do("GET", "/api/v1/events?conn=conn-lifecycle", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if !x.presence.Connected("conn-lifecycle") {
		t.Fatal("conn not registered by the time the stream's 200 arrived")
	}

	// Closing the stream ends the handler, whose deferred Disconnect deletes
	// the presence entry — the whole liveness design in one poll.
	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for x.presence.Connected("conn-lifecycle") {
		if time.Now().After(deadline) {
			t.Fatal("presence entry not cleaned up after stream close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSSERequiresAuth(t *testing.T) {
	x := newTestServer(t, nil)
	resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", "/api/v1/events", nil, nil)
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
	if got := x.bus.SubscriberCount(); got != 0 {
		t.Fatalf("unauthenticated request subscribed to the bus (count %d)", got)
	}
}
