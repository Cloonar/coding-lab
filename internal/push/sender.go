package push

// Sender is lab's fire-and-forget Web Push delivery seam (issue #98): the
// trigger slices (#99/#100) hand it a Payload and never see a gateway, a key,
// or an error. Every method that delivers — Broadcast, Send — returns to the
// caller immediately; ALL work (the store read included) runs on background
// goroutines tracked by one WaitGroup, so a delivery outlives the request that
// scheduled it and can never block, delay, or crash the caller. Flush drains
// that WaitGroup and exists only for tests and graceful shutdown.
//
// A dead endpoint (gateway 404/410) is reaped from the store on the sender's
// own background context; every other outcome — non-2xx, transport error,
// timeout, an airgapped unreachable gateway — is logged loudly and dropped.
// Endpoint URLs are never logged: an endpoint path is a bearer capability for
// that push channel, so errors name only the gateway host (url.Host).
//
// Broadcast additionally consults a PresenceView (issue #160): a subscription
// whose device currently has the app visible is dropped rather than sent —
// the live SSE-driven UI already is the notification, so a push would be a
// redundant (and sometimes startling) duplicate. Send, the Settings page's
// "send test" path, deliberately BYPASSES presence and always delivers, so
// an operator testing their setup gets a result even while looking at the
// app.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

const (
	// pushTTL is the gateway TTL in seconds (24h): how long the push service
	// holds an undelivered message for an offline device before dropping it.
	pushTTL = 86400

	// sendTimeout bounds a single delivery — DNS, connect, TLS, and the POST
	// to the gateway. It caps how long a background send goroutine (and thus a
	// Flush) can linger on an unresponsive or airgapped gateway.
	sendTimeout = 30 * time.Second

	// vapidSubscriber is the RFC 8292 `sub` contact claim in the VAPID JWT: a
	// URI a push-service operator can use to reach lab's operator about traffic
	// from this application server. The repo URL is a stable, real contact.
	vapidSubscriber = "https://git.cloonar.com/Cloonar/coding-lab"
)

// Payload is the notification content contract shared with the service
// worker: web/public/sw.js renders title/body/tag and routes a
// notificationclick to route (a PWA-internal path like /runs/abc). It is
// marshaled to JSON as the encrypted push message body.
type Payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag"`
	Route string `json:"route"`
}

// PresenceView is the sender's read-only window onto live device presence
// (issue #160): Visible reports whether the app is currently visible in some
// browser whose push subscription endpoint hashes (SHA-256, lowercase hex)
// to deviceHash. Implemented by internal/presence.Registry.
type PresenceView interface {
	Visible(deviceHash string) bool
}

// Sender delivers Web Push notifications asynchronously. Construct it with
// NewSender; it is safe for concurrent use.
type Sender struct {
	st       *store.Store
	key      Key
	presence PresenceView
	logger   *slog.Logger
	client   *http.Client // shared; webpush-go POSTs through it
	wg       sync.WaitGroup
}

// NewSender wires a Sender to the subscription store, the VAPID key it signs
// with, a PresenceView for Broadcast's send-time suppression (issue #160),
// and a logger (tagged component=push). presence may be nil — meaning "no
// presence knowledge" — in which case Broadcast never suppresses; tests and
// any caller that opts out pass nil. The one shared http.Client carries a
// hard timeout as a backstop to the per-send context deadline.
func NewSender(st *store.Store, key Key, presence PresenceView, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sender{
		st:       st,
		key:      key,
		presence: presence,
		logger:   logger.With("component", "push"),
		client:   &http.Client{Timeout: sendTimeout},
	}
}

// PublicKeyB64 returns the VAPID application-server public key (base64url) the
// subscribe API handler serves to browsers. Pass-through to the key.
func (s *Sender) PublicKeyB64() string {
	return s.key.PublicKeyB64()
}

// Broadcast delivers p to every stored subscription EXCEPT one whose device
// currently has the app visible (issue #160, PresenceView — nil presence
// means never suppress). A suppressed subscription is simply dropped: no
// queue, no retry, no replacement, and its row is untouched — the live
// SSE-driven UI already told that device, so the push would be redundant.
// Broadcast returns immediately; the store read, the presence check, and all
// sends run on background goroutines. A read failure is logged and the
// broadcast is abandoned — a missed notification is never worth blocking or
// failing a caller over.
func (s *Sender) Broadcast(p Payload) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		subs, err := s.st.PushSubscriptions(context.Background())
		if err != nil {
			s.logger.Error("web push broadcast: listing subscriptions failed", "err", err)
			return
		}
		body, err := json.Marshal(p)
		if err != nil {
			s.logger.Error("web push broadcast: marshaling payload failed", "err", err)
			return
		}
		// Each send is its own goroutine so one slow gateway cannot delay
		// delivery to the others; dispatch adds to the WaitGroup before this
		// coordinator goroutine returns, so Flush never sees a premature zero.
		// Each goroutine gets its OWN copy of body: webpush-go wraps the slice
		// it is handed in a bytes.Buffer and writes padding into it in place,
		// so two sends sharing one backing array race on that write.
		for _, sub := range subs {
			if s.presence != nil && s.presence.Visible(endpointDeviceHash(sub.Endpoint)) {
				s.logger.Debug("web push suppressed: app visible on device", "subscription", sub.ID)
				continue
			}
			s.dispatch(sub, append([]byte(nil), body...))
		}
	}()
}

// Send delivers p to exactly one subscription — the settings page's
// "send test" path. It returns immediately; marshaling and the send run on a
// background goroutine.
func (s *Sender) Send(sub store.PushSubscription, p Payload) {
	body, err := json.Marshal(p)
	if err != nil {
		s.logger.Error("web push send: marshaling payload failed", "subscription", sub.ID, "err", err)
		return
	}
	s.dispatch(sub, body)
}

// Flush blocks until every in-flight send (and any in-flight Broadcast store
// read) has finished. For tests and graceful shutdown only — the normal path
// never waits on a send.
func (s *Sender) Flush() {
	s.wg.Wait()
}

// dispatch runs one delivery on a tracked background goroutine. The recover is
// a hard guarantee, not a hope: fire-and-forget means a nil-response
// dereference or any other bug inside a send must not take down the process
// that scheduled it.
func (s *Sender) dispatch(sub store.PushSubscription, body []byte) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("web push send panicked",
					"subscription", sub.ID, "host", endpointHost(sub.Endpoint), "panic", fmt.Sprint(r))
			}
		}()
		s.send(sub, body)
	}()
}

// send performs one encrypted Web Push POST and reacts to the gateway's
// answer. It uses a fresh background context (never a caller's) so the send
// survives the request that scheduled it, bounded by sendTimeout.
func (s *Sender) send(sub store.PushSubscription, body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	resp, err := webpush.SendNotificationWithContext(ctx, body,
		&webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
		},
		&webpush.Options{
			HTTPClient:      s.client,
			Subscriber:      vapidSubscriber,
			TTL:             pushTTL,
			Urgency:         webpush.UrgencyHigh,
			VAPIDPublicKey:  s.key.PublicKeyB64(),
			VAPIDPrivateKey: s.key.PrivateKeyB64(),
		})
	// A transport failure — DNS, refused connection, timeout, airgapped host —
	// or, defensively, any nil response: loud, then move on.
	if err != nil || resp == nil {
		s.logger.Error("web push send failed",
			"subscription", sub.ID, "host", endpointHost(sub.Endpoint), "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// The gateway declares this endpoint dead (unsubscribed / expired).
		// Reap it on the sender's background context; the delete is idempotent,
		// so a racing send reaping the same endpoint is fine.
		if derr := s.st.DeletePushSubscriptionByEndpoint(context.Background(), sub.Endpoint); derr != nil {
			s.logger.Error("web push: removing expired subscription failed",
				"subscription", sub.ID, "err", derr)
		}
		s.logger.Info("push subscription expired; removed",
			"subscription", sub.ID, "status", resp.StatusCode)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		s.logger.Debug("web push delivered", "subscription", sub.ID, "status", resp.StatusCode)
	default:
		s.logger.Error("web push send failed",
			"subscription", sub.ID, "host", endpointHost(sub.Endpoint), "status", resp.StatusCode)
	}
}

// endpointHost returns just the host of a push endpoint for logging — never
// the full URL, whose path is a per-subscription capability token. Unparseable
// or host-less endpoints degrade to "unknown" rather than leaking the raw
// string.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return u.Host
}

// endpointDeviceHash is the device key shared with the browser (issue #160):
// SHA-256 hex of the push subscription endpoint. The client computes the same
// hash from its live subscription via crypto.subtle, so the capability URL
// itself never travels in presence traffic or logs; hashing here (rather than
// storing a hash column) self-heals when the browser rotates the endpoint.
func endpointDeviceHash(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}
