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
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	// Subscribe BEFORE the headers go out: once the client sees 200 it may
	// trigger actions whose events must not fall into a gap.
	ctx := r.Context()
	sub, cancel := s.bus.Subscribe(ctx)
	defer cancel()

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
