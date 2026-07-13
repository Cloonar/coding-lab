// Package presence is lab's in-memory registry of which browser tabs are
// currently live and visible, feeding the push-suppression design in issue
// #160: a push to a device whose app is already open and on-screen is noise,
// not a notification, so the sender should skip it.
//
// Lifetime is split across two independent inputs on purpose. The SSE stream
// (internal/httpapi/sse.go, a separate task) owns entry existence: Connect
// runs when a stream with ?conn=<uuid> opens, Disconnect when it ends —
// dropping the connection is instant, free, and requires no client
// cooperation, so "the tab is gone" is always cheap and reliable to learn.
// The visibility POST (a separate handler calling Update) is a best-effort
// beacon layered on top of that stream-owned lifetime: it can only refine an
// entry that the SSE stream already created, never resurrect or extend one
// after Disconnect. That split means a slow or dropped beacon can only make
// a device look less present than it is, never more — see Visible.
//
// Everything here lives in a Go map behind a mutex; there is no persistence.
// A server restart (or a lost SSE connection the client hasn't reconnected
// yet) empties the registry, so every device reads as absent until it
// reconnects and reports in. This is the deliberate failure direction for
// the whole design: staleness must err toward over-notifying, never toward
// silently swallowing a push. A registry that is too eager to say "visible"
// risks losing notifications the user needed; a registry that is too eager
// to say "not visible" only costs an extra, harmless notification.
package presence

import "sync"

// entry is one live SSE connection's presence state. The zero value — empty
// deviceHash, visible false — is exactly what a freshly Connect'd connection
// should read as: known to exist, but not yet known to belong to any device
// or be on-screen.
type entry struct {
	deviceHash string
	visible    bool
}

// Registry maps live SSE connection IDs to the device and visibility they
// last reported. The zero value is not usable; construct with NewRegistry.
// Safe for concurrent use — traffic here is one entry per open browser tab
// plus occasional visibility-change beacons, so a single mutex guarding a
// plain map is more than fast enough and keeps the logic easy to audit.
type Registry struct {
	mu   sync.Mutex
	conn map[string]entry
}

// NewRegistry returns an empty Registry ready for use.
func NewRegistry() *Registry {
	return &Registry{conn: make(map[string]entry)}
}

// Connect registers a live SSE connection, called when a stream with
// ?conn=<uuid> opens. A re-used conn id (a client that reconnects with the
// same generated id, or a bug that supplies a stale one) overwrites whatever
// was there with a fresh zero entry rather than preserving the old device
// hash or visibility — a new stream has reported nothing yet, and carrying
// forward a prior tab's state across a reconnect would be a fabrication, not
// a fact.
func (r *Registry) Connect(conn string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conn[conn] = entry{}
}

// Disconnect removes the connection's entry entirely, called when its SSE
// stream ends. This is what makes "not present" instant and free: no
// beacon, no timeout, no heuristic — the moment the stream drops, every
// Visible check for that device stops counting this connection. An unknown
// conn is a no-op; Disconnect can race a duplicate close or arrive for a
// connection this process never saw (e.g. after a restart) and both are
// harmless.
func (r *Registry) Disconnect(conn string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conn, conn)
}

// Update records the device hash and visibility last reported by conn's
// presence beacon (POST /api/v1/presence).
//
// If conn has no entry — never Connected, already Disconnected, or from a
// process restart the client hasn't noticed yet — Update is a no-op. This is
// the load-bearing half of the split-lifetime design: presence liveness is
// owned exclusively by the SSE stream, so a beacon that outlives (or never
// had) a matching stream must not resurrect or fabricate an entry. Without
// this guard a stray or delayed beacon could mark a device "visible"
// forever with nothing left alive to ever clear it.
func (r *Registry) Update(conn, deviceHash string, visible bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.conn[conn]; !ok {
		return
	}
	r.conn[conn] = entry{deviceHash: deviceHash, visible: visible}
}

// Connected reports whether conn currently has a live entry. Exposed for
// tests and for handlers (e.g. the presence POST) that want to distinguish
// "unknown connection, ignored" from "recorded" without duplicating
// Registry's locking.
func (r *Registry) Connected(conn string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.conn[conn]
	return ok
}

// Visible reports whether deviceHash is currently present: whether ANY live
// connection carries this hash AND has reported itself visible. Multi-tab
// devices are handled by that "any" — a device with one hidden tab and one
// foregrounded tab is present, because the user can see it. A device with
// every tab backgrounded, or with no live tab at all, is not.
//
// The empty hash is always false, even if some connection's Update call
// carried an empty deviceHash (e.g. a beacon that fired before the client
// finished computing its hash). A connection that never reported a real
// device must never be able to suppress a broadcast — matching on "" would
// let an under-initialized tab accidentally silence every push aimed at
// devices the registry knows nothing else about.
func (r *Registry) Visible(deviceHash string) bool {
	if deviceHash == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.conn {
		if e.deviceHash == deviceHash && e.visible {
			return true
		}
	}
	return false
}
