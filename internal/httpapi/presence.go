package httpapi

// Presence beacon (issue #160): POST /api/v1/presence, the client's report of
// which live SSE connection belongs to which push device and whether that tab
// is currently on-screen. The browser fires it once right after its event
// stream opens, again on every visibilitychange, and a final time via
// navigator.sendBeacon on pagehide — feeding the send-time push suppression in
// internal/push (a broadcast to an already-visible device is noise). Existence
// of the connection is owned by the SSE stream (sse.go Connect/Disconnect);
// this endpoint can only refine an entry that stream already created, never
// resurrect one — see internal/presence and Registry.Update.

import (
	"net/http"
)

// maxPresenceConn bounds the accepted conn id — generous for a
// crypto.randomUUID() value (36 chars) while capping what a malformed or
// hostile client can push through both here and the SSE registration path.
const maxPresenceConn = 128

// presenceRequest is the beacon body. Conn is the ?conn=<uuid> the stream was
// opened with; Device is the SHA-256 hash of the push endpoint this tab is
// subscribed under (empty when the tab has no subscription, so nothing to
// suppress); Visible is the tab's current visibility.
type presenceRequest struct {
	Conn    string `json:"conn"`
	Device  string `json:"device"`
	Visible bool   `json:"visible"`
}

// handlePresence is POST /api/v1/presence: record a live connection's device
// and visibility. It answers 204 in every non-error case — including an
// unknown conn, which Registry.Update no-ops BY DESIGN so a stray or delayed
// beacon can never resurrect a dead stream's presence. The client cannot act
// on the difference anyway (sendBeacon cannot even read the response) and it
// re-reports once its stream reopens.
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	// decodeJSON ignores Content-Type on purpose: a sendBeacon Blob arrives as
	// application/json, but the text/plain fallback some paths use must decode
	// identically — the body shape is all that matters.
	var req presenceRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}

	if req.Conn == "" {
		writeError(w, http.StatusBadRequest, "conn is required")
		return
	}
	if len(req.Conn) > maxPresenceConn {
		writeError(w, http.StatusBadRequest, "conn is too long")
		return
	}
	// Device is either empty (a tab with no push subscription reports nothing
	// to suppress) or exactly the 64-char lowercase hex SHA-256 endpoint hash
	// the sender keys suppression on. Anything else is a client bug we reject
	// rather than store as an un-matchable hash.
	if req.Device != "" && !isHexHash64(req.Device) {
		writeError(w, http.StatusBadRequest, "device must be a 64-char lowercase hex SHA-256 hash")
		return
	}

	s.presence.Update(req.Conn, req.Device, req.Visible)
	w.WriteHeader(http.StatusNoContent)
}

// isHexHash64 reports whether s is exactly 64 lowercase hex characters — the
// canonical form of a SHA-256 endpoint hash. A plain loop (no regexp) keeps
// the check allocation-free and matches the file's style; uppercase is
// deliberately rejected so the stored hash compares byte-for-byte with the
// sender's lowercase-hex hash.
func isHexHash64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
