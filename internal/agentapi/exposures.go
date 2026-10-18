package agentapi

// Previews (issue #105): `labctl expose <name> <port>` registers an exposure
// and turns its own request into the tunnel that carries the preview's
// traffic. POST /agent/v1/exposures is an HTTP/1.1 Upgrade to the
// "lab-tunnel" protocol: everything that can go wrong is answered with the
// usual JSON envelope BEFORE the switch, and after the 101 the connection
// belongs to the preview Registry (a yamux session, see internal/preview).
// DELETE /agent/v1/exposures/{name} ends one. Both are scoped to the
// authenticated run exactly like every other agent route.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/preview"
)

// Previews is the slice of *preview.Registry the agent API drives. A nil
// Previews (the default) means lab has no preview listener configured, and
// both exposure routes answer the "not enabled" 404.
type Previews interface {
	// Reserve claims name for runID (re-claiming it for the same run) on
	// behalf of the labctl process identified by session.
	Reserve(repoID, runID, name, session string, port int) error
	// Attach serves the tunnel on conn — already past the 101 — and blocks
	// until it ends. It owns conn.
	Attach(runID, name string, conn net.Conn) error
	// Stop releases runID's name and ends its live tunnel.
	Stop(runID, name string) error
	// URL is the browser-facing URL of the preview called name.
	URL(name string) string
}

// SetPreviews enables the exposure routes on p. Call it before the handler
// serves; never calling it (lab runs without a preview listener) leaves
// previews disabled. Callers pass a true nil, never a typed-nil pointer.
func (s *Server) SetPreviews(p Previews) {
	s.previews = p
}

// tunnelProtocol is the Upgrade token of the exposure tunnel.
const tunnelProtocol = "lab-tunnel"

// previewURLHeader carries the preview's URL on the 101; labctl prints it.
const previewURLHeader = "Lab-Preview-URL"

// previewsDisabledMessage is the 404 every exposure route answers when lab
// runs without a preview listener.
const previewsDisabledMessage = "previews are not enabled on this lab (no preview listener configured)"

// maxExposeBody bounds the registration body: {"name": a DNS label, "port",
// "session": 32 hex digits} is well under a hundred bytes.
const maxExposeBody = 4 << 10

// exposeRequest is POST /agent/v1/exposures' body. Session is the random id
// one `labctl expose` process sends on every attempt (issue #105): it is how
// the Registry refuses that process's reconnect after `expose stop` freed
// its name (410 below) while still letting a NEW `labctl expose` of the name
// in. It is opaque to lab; an older labctl omits it and keeps the
// pre-session behavior.
type exposeRequest struct {
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Session string `json:"session"`
}

// exposureRun returns the authenticated run, or writes the response and
// returns ok=false (no run in context → wiring bug; previews off → 404).
func (s *Server) exposureRun(w http.ResponseWriter, r *http.Request) (runID, repoID string, ok bool) {
	info, ok := RunFromContext(r.Context())
	if !ok {
		s.internalError(w, "run missing from context", errors.New("agentapi: handler mounted without auth middleware"))
		return "", "", false
	}
	if s.previews == nil {
		jsonError(w, http.StatusNotFound, previewsDisabledMessage)
		return "", "", false
	}
	return info.RunID, info.RepoID, true
}

// handleExposureCreate registers an exposure and switches the connection to
// the tunnel protocol.
//
// The switch is done by hand: Hijack FIRST, then write the 101 ourselves on
// the raw connection. Letting net/http write a 101 via WriteHeader and
// hijacking afterwards depends on the server's chunk writer flushing a
// bodiless final status on Hijack — it adds its own headers and is easy to
// get subtly wrong — whereas after a Hijack nothing in net/http touches the
// connection again, on the unix-socket server and the main TCP server alike
// (httpapi's response recorder is reached through Unwrap).
//
// The handler then BLOCKS in Attach for the tunnel's whole life. net/http no
// longer tracks a hijacked connection either way (Shutdown neither waits for
// nor closes it), so blocking costs nothing, and it keeps the tunnel inside
// the request's middleware: panic recovery still applies, and the access log
// line, written when the handler returns, spans the tunnel's lifetime.
func (s *Server) handleExposureCreate(w http.ResponseWriter, r *http.Request) {
	runID, repoID, ok := s.exposureRun(w, r)
	if !ok {
		return
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", tunnelProtocol) {
		jsonError(w, http.StatusBadRequest, "expected Upgrade: "+tunnelProtocol)
		return
	}

	// Read the whole (tiny) body before the switch: once hijacked, whatever
	// follows on the connection is tunnel bytes.
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxExposeBody))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var req exposeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if err := s.previews.Reserve(repoID, runID, req.Name, req.Session, req.Port); err != nil {
		switch {
		case errors.Is(err, preview.ErrRunEnded):
			// The run ended after the middleware accepted its token: the same
			// opaque answer the middleware gives an ended run a moment later,
			// so labctl treats it as final either way.
			jsonError(w, http.StatusUnauthorized, "invalid run token")
		case errors.Is(err, preview.ErrInvalidName):
			jsonError(w, http.StatusBadRequest, invalidNameMessage(req.Name))
		case errors.Is(err, preview.ErrInvalidPort):
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("invalid port %d: must be 1–65535", req.Port))
		case errors.Is(err, preview.ErrConflict):
			jsonError(w, http.StatusConflict, fmt.Sprintf("name %q is already exposed by another run", req.Name))
		case errors.Is(err, preview.ErrLimit):
			jsonError(w, http.StatusUnprocessableEntity, fmt.Sprintf("exposure limit reached (%d per run)", preview.MaxPerRun))
		case errors.Is(err, preview.ErrStopped):
			// 410, not 404 or 409: the name is free, but THIS labctl process's
			// exposure is gone for good — `expose stop` ended it while the
			// tunnel was between connections. labctl reads it as "stopped"
			// and exits 0, as it would on the control message it missed.
			jsonError(w, http.StatusGone, fmt.Sprintf("exposure %q was stopped", req.Name))
		default:
			s.internalError(w, "reserving exposure", err)
		}
		return
	}

	// The reservation stands from here on whatever happens to this request:
	// it belongs to the run until `expose stop` or the run's end, and labctl
	// re-registers on its next attempt anyway.
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.internalError(w, "hijacking exposure connection", err)
		return
	}
	// A tunnel lives for hours: no server read/write deadline may survive on
	// it. net/http's Hijack clears them today, but its contract leaves that
	// to the caller, so clear them explicitly.
	_ = conn.SetDeadline(time.Time{})

	status := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: " + tunnelProtocol + "\r\n" +
		previewURLHeader + ": " + s.previews.URL(req.Name) + "\r\n\r\n"
	if _, err := io.WriteString(conn, status); err != nil {
		_ = conn.Close()
		s.log.Debug("writing exposure 101", "component", "agentapi", "run_id", runID, "name", req.Name, "err", err)
		return
	}

	// labctl waits for the 101 before speaking, so the server's read buffer
	// is normally empty — but anything it did read ahead is tunnel data and
	// must reach the session first.
	tun := conn
	if n := brw.Reader.Buffered(); n > 0 {
		buffered, _ := brw.Peek(n)
		tun = &prefixConn{Conn: conn, r: io.MultiReader(bytes.NewReader(bytes.Clone(buffered)), conn)}
	}

	s.log.Info("exposure attached", "component", "agentapi", "run_id", runID, "name", req.Name, "port", req.Port)
	if err := s.previews.Attach(runID, req.Name, tun); err != nil {
		// Stopped or released between Reserve and Attach; Attach has already
		// told labctl on the control stream ("stopped" or "run-ended").
		s.log.Info("exposure attach ended", "component", "agentapi", "run_id", runID, "name", req.Name, "err", err)
		return
	}
	s.log.Info("exposure detached", "component", "agentapi", "run_id", runID, "name", req.Name)
}

// handleExposureDelete stops one of the run's exposures.
func (s *Server) handleExposureDelete(w http.ResponseWriter, r *http.Request) {
	runID, _, ok := s.exposureRun(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := s.previews.Stop(runID, name); err != nil {
		if errors.Is(err, preview.ErrUnknown) {
			jsonError(w, http.StatusNotFound, fmt.Sprintf("no exposure named %q in this run", name))
			return
		}
		s.internalError(w, "stopping exposure", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// invalidNameMessage is the 400 for a name that is not a single DNS label.
// labctl prints the same words when it rejects the name locally.
func invalidNameMessage(name string) string {
	return fmt.Sprintf("invalid name %q: must be a single DNS label (a-z, 0-9, '-', at most 63 characters)", name)
}

// headerHasToken reports whether any comma-separated element of header key
// equals token, case-insensitively ("Connection: keep-alive, Upgrade").
func headerHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, el := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(el), token) {
				return true
			}
		}
	}
	return false
}

// prefixConn is a hijacked connection whose first reads drain the bytes the
// server had already buffered.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }
