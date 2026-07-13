package httpapi

import (
	"net/http"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
)

// rcFlusher adapts http.ResponseController to http.Flusher so
// events.WriteSSE can flush through the middleware wrappers.
type rcFlusher struct{ rc *http.ResponseController }

func (f rcFlusher) Flush() { _ = f.rc.Flush() }

// handleEvents is GET /api/v1/events: an authenticated SSE stream of bus
// events plus a heartbeat every s.heartbeat. Clients refetch on event — no
// state diffing over SSE (design §5).
//
// When the request carries ?conn=<uuid> and presence tracking is enabled, the
// stream doubles as the liveness owner for that connection in the presence
// registry (issue #160): it Connects on open and, via defer, Disconnects the
// moment the handler returns — client disconnect, a logout-invalidated
// credential, a failed heartbeat write, or server shutdown all delete the
// presence entry instantly. That makes "the tab is gone" cheap and reliable,
// and errs toward notifying: a lost stream can only make a device look less
// present than it is (see internal/presence). An absent or oversized conn
// param simply skips registration — the stream itself still serves normally.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	// Subscribe BEFORE the headers go out: once the client sees 200 it may
	// trigger actions whose events must not fall into a gap.
	ctx := r.Context()
	sub, cancel := s.bus.Subscribe(ctx)
	defer cancel()

	// Register presence BEFORE the headers/200 go out (ordering is
	// load-bearing: the client POSTs its presence beacon the instant onopen
	// fires, so Connect must happen-before the 200 or that beacon races an
	// unregistered conn and no-ops). The deferred Disconnect is the whole
	// liveness design — see the doc comment above.
	if conn := r.URL.Query().Get("conn"); s.presence != nil && conn != "" && len(conn) <= maxPresenceConn {
		s.presence.Connect(conn)
		defer s.presence.Disconnect(conn)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		// The connection cannot stream; there is nothing useful to answer.
		return
	}

	// SSE streams outlive any server write timeout; disable it for this
	// response (best effort — not every listener supports it).
	_ = rc.SetWriteDeadline(time.Time{})

	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()

	flusher := rcFlusher{rc}
	heartbeat := events.Event{Type: "heartbeat", Payload: map[string]string{"type": "heartbeat"}}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdownCtx.Done():
			// Server shutdown: end the stream so http.Server.Shutdown can
			// drain the connection instead of timing out on it.
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if err := events.WriteSSE(w, flusher, ev); err != nil {
				return
			}
		case <-ticker.C:
			// Re-validate the credential each beat: logout, session expiry,
			// or PAT deletion must kill live streams, not just future
			// requests.
			if !s.identityStillValid(ctx) {
				return
			}
			if err := events.WriteSSE(w, flusher, heartbeat); err != nil {
				return
			}
		}
	}
}
