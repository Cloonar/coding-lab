package httpapi

// Web Push operator surface (issue #98): the VAPID public key browsers need
// to subscribe, and CRUD over the device-level subscription rows the sender
// (internal/push) delivers to. Subscriptions are deliberately NOT scoped to
// the calling user — see internal/store/pushsubscriptions.go's package
// comment — so, unlike tokens.go, these handlers never filter by identity.

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/push"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// maxEndpointLen and maxKeyLen bound the subscribe payload. Deeper
// validation (that p256dh/auth are well-formed Web Push keys) is deliberately
// NOT done here: the sender surfaces bad keys at send time, where the error
// belongs to a concrete delivery attempt rather than a guess at register time.
const (
	maxEndpointLen = 2048
	maxKeyLen      = 512
)

// pushSubscriptionResponse is one GET/POST /api/v1/push/subscriptions row.
// The keys (p256dh/auth) never appear here — the client that just registered
// them already knows them, and no other reader needs them.
type pushSubscriptionResponse struct {
	ID        string `json:"id"`
	Endpoint  string `json:"endpoint"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at"`
}

func pushSubscriptionJSON(sub store.PushSubscription) pushSubscriptionResponse {
	return pushSubscriptionResponse{
		ID:        sub.ID,
		Endpoint:  sub.Endpoint,
		Label:     sub.Label,
		CreatedAt: store.FormatTime(sub.CreatedAt),
	}
}

// handlePushKey is GET /api/v1/push/key: the VAPID application-server public
// key a browser's PushManager.subscribe needs as applicationServerKey. Not
// secret — see push.Key.PublicKeyB64.
func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"public_key": s.push.PublicKeyB64()})
}

// handlePushSubscriptionList is GET /api/v1/push/subscriptions: every
// registered device, newest first (store order) — the settings page's device
// list.
func (s *Server) handlePushSubscriptionList(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.PushSubscriptions(r.Context())
	if err != nil {
		s.internalError(w, "listing push subscriptions", err)
		return
	}
	items := make([]pushSubscriptionResponse, 0, len(subs))
	for _, sub := range subs {
		items = append(items, pushSubscriptionJSON(sub))
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": items})
}

type pushSubscriptionCreateRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// handlePushSubscriptionCreate is POST /api/v1/push/subscriptions: register
// (or re-register) a browser's PushSubscription. Upsert on endpoint means
// re-subscribing is idempotent — same endpoint updates keys/label in place —
// but the response is always 201: the client cannot tell the difference and
// doesn't need to.
func (s *Server) handlePushSubscriptionCreate(w http.ResponseWriter, r *http.Request) {
	var req pushSubscriptionCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}

	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	if len(endpoint) > maxEndpointLen {
		writeError(w, http.StatusBadRequest, "endpoint is too long")
		return
	}
	u, err := url.Parse(endpoint)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "endpoint must be an absolute http(s) URL")
		return
	}

	p256dh := strings.TrimSpace(req.Keys.P256dh)
	auth := strings.TrimSpace(req.Keys.Auth)
	if p256dh == "" || auth == "" {
		writeError(w, http.StatusBadRequest, "keys.p256dh and keys.auth are required")
		return
	}
	if len(p256dh) > maxKeyLen || len(auth) > maxKeyLen {
		writeError(w, http.StatusBadRequest, "keys.p256dh and keys.auth must be at most 512 characters")
		return
	}

	// The device label is derived server-side from User-Agent rather than
	// trusted from the client: it is display-only (an operator's "which
	// phone is this" hint), not a security boundary, but letting the browser
	// name itself invites confusing or spoofed device lists for no benefit.
	label := deviceLabel(r.Header.Get("User-Agent"))

	sub, err := s.store.UpsertPushSubscription(r.Context(), endpoint, p256dh, auth, label)
	if err != nil {
		s.internalError(w, "creating push subscription", err)
		return
	}
	resp := pushSubscriptionJSON(sub)
	writeJSON(w, http.StatusCreated, resp)
}

// handlePushSubscriptionDelete is DELETE /api/v1/push/subscriptions/{id}:
// unregister a device. 404 when the id names no subscription (mirrors
// handleTokenDelete).
func (s *Server) handlePushSubscriptionDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeletePushSubscription(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "deleting push subscription", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePushSubscriptionTest is POST /api/v1/push/subscriptions/{id}/test:
// the settings page's "send test" button. It answers 202 once the send has
// been HANDED TO the sender, not once it has been delivered — Sender.Send is
// fire-and-forget by design (see internal/push/sender.go), so a delivery
// failure (dead endpoint, unreachable gateway, bad keys) surfaces only in
// server logs, never in this response. That asymmetry is the point: the test
// button proves the plumbing accepted the request, and an operator checking
// "did my phone actually buzz" is the real end-to-end test.
func (s *Server) handlePushSubscriptionTest(w http.ResponseWriter, r *http.Request) {
	sub, err := s.store.PushSubscriptionByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "loading push subscription", err)
		return
	}
	s.push.Send(sub, push.Payload{
		Title: "lab",
		Body:  "Test notification — push works on this device",
		Tag:   "test",
		Route: "/settings",
	})
	w.WriteHeader(http.StatusAccepted)
}

// deviceLabel derives a short human display label from a User-Agent string,
// e.g. "Safari on iPhone". It is a tiny, deterministic best-effort parse over
// substrings — not a full UA parser — good enough for an operator's device
// list, never used for feature detection or security. Order matters: Edge
// UAs contain "Chrome", and Chrome UAs contain "Safari", so the more specific
// browser must be checked first. An empty or unrecognized UA yields "unknown
// device" rather than a misleading guess.
func deviceLabel(userAgent string) string {
	if userAgent == "" {
		return "unknown device"
	}

	var browser string
	switch {
	case strings.Contains(userAgent, "Edg/"):
		browser = "Edge"
	case strings.Contains(userAgent, "Chrome"):
		browser = "Chrome"
	case strings.Contains(userAgent, "Firefox"):
		browser = "Firefox"
	case strings.Contains(userAgent, "Safari"):
		browser = "Safari"
	}

	var platform string
	switch {
	case strings.Contains(userAgent, "iPhone"):
		platform = "iPhone"
	case strings.Contains(userAgent, "iPad"):
		platform = "iPad"
	case strings.Contains(userAgent, "Android"):
		platform = "Android"
	case strings.Contains(userAgent, "Mac"):
		platform = "Mac"
	case strings.Contains(userAgent, "Windows"):
		platform = "Windows"
	case strings.Contains(userAgent, "Linux"):
		platform = "Linux"
	case strings.Contains(userAgent, "X11"):
		platform = "Linux"
	}

	switch {
	case browser != "" && platform != "":
		return browser + " on " + platform
	case browser != "":
		return browser
	case platform != "":
		return platform
	default:
		return "unknown device"
	}
}
