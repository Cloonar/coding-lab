// Package preview owns lab's previews (issue #105): an agent in a run calls
// `labctl expose <name> <port>`, labctl opens a reverse tunnel to lab over the
// agent API, and lab serves that port to the operator's browser at
// http://<name>.<preview-domain> on the opt-in preview listener.
//
// Both ends of the tunnel protocol live here so they are tested together:
// the lab side (Registry.Attach, the stream dialer behind Registry.Handler)
// and the labctl side (ServeTunnel). The package deliberately imports nothing
// of lab's own — no store, no httpapi, no agentapi — because labctl links it
// too and must stay a small binary; lab's event bus is reached through a
// plain callback instead.
//
// The Registry is in memory only. A tunnel cannot survive a lab restart (the
// hijacked connection dies with the process) and labctl re-registers on
// reconnect, so persisting names would only ever create stale reservations.
package preview

import (
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// MaxPerRun caps how many exposures one run may hold at once. Each live one
// pins a long-lived connection and a goroutine set on lab, and an agent that
// needs more than a handful of previews is far more likely looping than
// serving eight apps — the cap turns that into a specific error instead of
// unbounded growth.
const MaxPerRun = 8

// The Registry's error vocabulary. The agent API maps each one to a status
// and a one-line message, and labctl prints that message, so the texts here
// are internal; what matters is that callers can tell them apart with
// errors.Is.
var (
	// ErrInvalidName: the name is not a single DNS label (see ValidName). A
	// preview's name becomes the leftmost label of its hostname, so anything
	// else could not be served.
	ErrInvalidName = errors.New("preview: name is not a single DNS label")
	// ErrInvalidPort: the port is outside 1–65535.
	ErrInvalidPort = errors.New("preview: port outside 1-65535")
	// ErrConflict: another run holds the name. The error says nothing about
	// which run — one run learns nothing about another's work from it.
	ErrConflict = errors.New("preview: name is held by another run")
	// ErrLimit: the run already holds MaxPerRun exposures.
	ErrLimit = errors.New("preview: run already holds the maximum number of exposures")
	// ErrUnknown: the run does not hold the name (Stop, or Attach without a
	// successful Reserve first).
	ErrUnknown = errors.New("preview: no such exposure in this run")
)

// ValidName reports whether s is a single DNS label: 1–63 characters of
// [a-z0-9-], not starting or ending with '-'. Lowercase only — labctl
// lowercases nothing on the agent's behalf, so "Web" is rejected rather than
// silently becoming a different name from the one the agent announced. Dots
// are rejected too: a dotted name would be a multi-label hostname, and the
// preview listener routes on exactly one label in front of the domain.
func ValidName(s string) bool {
	if len(s) < 1 || len(s) > 63 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// validPort reports whether port is a usable TCP port.
func validPort(port int) bool { return port >= 1 && port <= 65535 }

// Exposure is one exposure as the operator UI sees it, carried on the run's
// API representation. Live is false while the name is reserved but no tunnel
// is attached — labctl reconnecting after a dropped connection or a lab
// restart, during which the preview answers 503.
type Exposure struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Port int    `json:"port"`
	Live bool   `json:"live"`
}

// exposure is the Registry's record of one reserved name. tun is the live
// tunnel, nil while reserved-not-live. All fields are guarded by Registry.mu.
type exposure struct {
	name   string
	repoID string
	runID  string
	port   int
	tun    *tunnel
}

// Registry maps preview names to the runs that hold them, and each name to
// its live tunnel when one is attached. A name belongs to the run that first
// reserved it until the run ends (CloseRun) or the agent stops it (Stop) —
// NOT until its connection drops: a lab restart drops every hijacked
// connection while the runs survive, and releasing on disconnect would let a
// sibling run take the name during the reconnect window and serve a
// different app to a stale browser tab. Safe for concurrent use.
type Registry struct {
	domain     string
	listenPort int
	onChange   func(repoID, runID string)
	proxy      *httputil.ReverseProxy

	mu     sync.Mutex
	byName map[string]*exposure
}

// NewRegistry builds an empty Registry. domain is the preview domain (e.g.
// "lab.internal", already validated and lowercase); listenPort is the
// preview listener's port, used only to build URLs (80 — or 0, unknown — gives
// a URL without a port suffix). onChange(repoID, runID) is called on every
// reserve, attach, detach, stop and CloseRun so lab can publish run.changed;
// it is always called outside the Registry's lock, so it may call back into
// the Registry, and it may be nil.
func NewRegistry(domain string, listenPort int, onChange func(repoID, runID string)) *Registry {
	r := &Registry{
		domain:     domain,
		listenPort: listenPort,
		onChange:   onChange,
		byName:     make(map[string]*exposure),
	}
	r.proxy = r.newProxy()
	return r
}

// URL returns the browser-facing URL of the preview called name:
// "http://<name>.<domain>", with ":<port>" appended when the listener is not
// on 80. It does not check that name is reserved — the agent API uses it to
// answer a registration, before and independent of any lookup.
func (r *Registry) URL(name string) string {
	host := name + "." + r.domain
	if r.listenPort != 0 && r.listenPort != 80 {
		host = net.JoinHostPort(host, strconv.Itoa(r.listenPort))
	}
	return "http://" + host
}

// Reserve claims name for runID, or re-claims it for the same run — a
// reconnecting labctl calls it again before every attach — updating the
// recorded port. It validates name and port first. It never attaches a
// session; Attach does that. Errors: ErrInvalidName, ErrInvalidPort,
// ErrConflict (another run holds name), ErrLimit (runID already holds
// MaxPerRun other names).
func (r *Registry) Reserve(repoID, runID, name string, port int) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	if !validPort(port) {
		return ErrInvalidPort
	}

	r.mu.Lock()
	if e, ok := r.byName[name]; ok {
		if e.runID != runID {
			r.mu.Unlock()
			return ErrConflict
		}
		e.repoID = repoID
		e.port = port
	} else {
		held := 0
		for _, e := range r.byName {
			if e.runID == runID {
				held++
			}
		}
		if held >= MaxPerRun {
			r.mu.Unlock()
			return ErrLimit
		}
		r.byName[name] = &exposure{name: name, repoID: repoID, runID: runID, port: port}
	}
	r.mu.Unlock()

	r.changed(repoID, runID)
	return nil
}

// Stop releases runID's name: the live session, if any, is told "stopped"
// and closed, and the reservation is deleted, so another run may take the
// name at once and the preview host answers 404. ErrUnknown when runID does
// not hold name — including when another run does, which Stop must not
// disclose.
func (r *Registry) Stop(runID, name string) error {
	r.mu.Lock()
	e, ok := r.byName[name]
	if !ok || e.runID != runID {
		r.mu.Unlock()
		return ErrUnknown
	}
	delete(r.byName, name)
	tun := e.tun
	e.tun = nil
	r.mu.Unlock()

	if tun != nil {
		tun.end(ControlStopped)
	}
	r.changed(e.repoID, runID)
	return nil
}

// CloseRun releases every exposure runID holds, telling each live session
// "run-ended" before closing it. It is the run-end chokepoint's half of the
// feature: every site that deletes a run's tokens calls it, so an ended run
// never keeps a name or a tunnel. Idempotent, and a no-op (no onChange) for a
// run holding nothing.
func (r *Registry) CloseRun(runID string) {
	var (
		repoID string
		tuns   []*tunnel
		found  bool
	)
	r.mu.Lock()
	for name, e := range r.byName {
		if e.runID != runID {
			continue
		}
		found = true
		repoID = e.repoID
		if e.tun != nil {
			tuns = append(tuns, e.tun)
			e.tun = nil
		}
		delete(r.byName, name)
	}
	r.mu.Unlock()

	if !found {
		return
	}
	// The control messages go out concurrently: each end can wait out a
	// write deadline on a dead peer, and the run-end callers should not pay
	// that once per exposure.
	var wg sync.WaitGroup
	for _, t := range tuns {
		wg.Go(func() { t.end(ControlRunEnded) })
	}
	wg.Wait()
	r.changed(repoID, runID)
}

// Exposures lists runID's exposures sorted by name. Never nil: a run with
// none gets an empty slice, so the run API always renders "exposures": [].
func (r *Registry) Exposures(runID string) []Exposure {
	out := []Exposure{}
	r.mu.Lock()
	for _, e := range r.byName {
		if e.runID == runID {
			out = append(out, Exposure{Name: e.name, Port: e.port, Live: e.tun != nil})
		}
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Exposure) int { return strings.Compare(a.Name, b.Name) })
	for i := range out {
		out[i].URL = r.URL(out[i].Name)
	}
	return out
}

// changed fires onChange, if set. Callers must not hold r.mu.
func (r *Registry) changed(repoID, runID string) {
	if r.onChange != nil {
		r.onChange(repoID, runID)
	}
}

// lookup returns the record for name and a snapshot of its live tunnel and
// port, taken under the lock so the handler never reads a record mid-update.
func (r *Registry) lookup(name string) (tun *tunnel, port int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byName[name]
	if !ok {
		return nil, 0, false
	}
	return e.tun, e.port, true
}

// Handler is the preview listener's http.Handler: it routes every request by
// its Host to the tunnel of the run holding that name. See handler.go.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(r.serveHTTP)
}
