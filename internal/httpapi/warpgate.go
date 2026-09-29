package httpapi

// Warpgate SSH bastion health and the host-key accept (issue #39 / ADR-0068):
// GET /api/v1/warpgate/health and POST /api/v1/warpgate/host-key/accept. The
// bastion is the non-HTTP sibling of the OneCLI credential gateway, and its
// health is built on onecli.go's contract, which is pinned the same way here:
//
//   - The status is the payload, never the HTTP code. Health always answers
//     200, because it reports a DEPENDENCY's health, not lab's.
//   - "Not configured" is not "unhealthy". Nothing configured reports "off",
//     a complete answer, never an error.
//
// Health folds three things where OneCLI's folds two: the admin API (reached
// through the client's own Health, which also says whether the token was
// accepted with every admin permission lab uses), the SSH listener, and the
// pin of its host key. The last two are ONE probe: the host-key check scans
// the listener — one SSH handshake per host-key algorithm, aborted before any
// authentication — so a successful scan is the listener's reachability, and
// the same scan is what the pin is compared against. A separate TCP dial next
// to it would be a second opinion on a question the scan already answered
// with more proof (a TCP accept proves a port, a host key proves an SSH
// server).
//
// The accept endpoint is the operator's explicit action on a host-key
// mismatch: it takes one of the fingerprints health reported as observed and
// re-pins the listener's current key set, provided that fingerprint is still
// among the keys it presents NOW. It is the only way a changed key is ever
// trusted — never a run, never a spawn, never a settings PATCH (ADR-0068).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// warpgateProbeTimeout bounds the WHOLE health handler — both probes run
// concurrently under it — and the accept's rescan. It is longer than
// oneCLIProbeTimeout because the SSH probe is a scan, not a dial: up to five
// handshakes back to back (warpgate's hostKeyAlgorithms), three of which a
// Warpgate without ECDSA keys refuses in key exchange. Against a same-host
// sidecar that is well under a second; a listener that black-holes the dial
// still answers "unreachable" inside the bound, since the scan fails on the
// first handshake that is not a plain "algorithm not offered". The accept
// shares the bound so that it succeeds exactly when health could have
// observed the fingerprint the operator is accepting.
const warpgateProbeTimeout = 8 * time.Second

// The four states of the Warpgate integration, as pinned by ADR-0068 — the
// same four words as the credential gateway's, so the SPA renders both
// status chips with one vocabulary.
const (
	warpgateStateOff         = "off"
	warpgateStateOK          = "ok"
	warpgateStateDegraded    = "degraded"
	warpgateStateUnreachable = "unreachable"
)

// warpgateSSHNotConfiguredMessage is what the accept answers when there is no
// SSH listener configured: there is no host key to pin, and the flag that
// changes that is named so the operator reading the toast can act on it.
const warpgateSSHNotConfiguredMessage = "Warpgate's SSH listener is not configured on this lab; set --warpgate-ssh-addr so lab can scan and pin its host key"

// WarpgateHostKeyPin is the seam over the pin of Warpgate's SSH host key(s)
// this package drives: Check for health, Accept for the operator's re-pin.
// *warpgate.HostKeyPin satisfies it; the assertion below is the compile-time
// proof. It is deliberately narrower than the pin — KnownHosts is the spawn
// path's (internal/instance), and nothing reachable over HTTP renders a run's
// known_hosts.
type WarpgateHostKeyPin interface {
	Check(ctx context.Context) warpgate.HostKeyStatus
	Accept(ctx context.Context, fingerprint string) (warpgate.HostKeyStatus, error)
}

var _ WarpgateHostKeyPin = (*warpgate.HostKeyPin)(nil)

// normalizeWarpgate turns a typed nil — a nil *warpgate.Client or nil
// *warpgate.HostKeyPin stored in the interface-typed Options field — into the
// nil interface it was meant to be. cmd/lab is required to leave the fields
// nil when unconfigured (see Options), and this is the belt to that braces: a
// non-nil interface wrapping a nil pointer would read as "configured" to every
// `== nil` check here and panic inside the client on the first call. Only the
// two concrete types lab wires are recognized; a test fake is whatever it is.
func normalizeWarpgate(api WarpgateAPI, pin WarpgateHostKeyPin) (WarpgateAPI, WarpgateHostKeyPin) {
	if c, ok := api.(*warpgate.Client); ok && c == nil {
		api = nil
	}
	if p, ok := pin.(*warpgate.HostKeyPin); ok && p == nil {
		pin = nil
	}
	return api, pin
}

// warpgateAPIHealth is the admin API component. Error is OPERATOR-FACING TEXT
// held to the integration's hygiene rule: the warpgate package guarantees an
// error never carries the admin token (it only ever travels in a request
// header) nor a targets body, and this file never composes a message out of
// configuration — the only configuration echoed is the redacted URL and the
// SSH address, deliberately, so an operator can see WHICH address answered.
type warpgateAPIHealth struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	URL        string `json:"url,omitempty"`
	// Version is Warpgate's self-reported version. Upstream reveals it only to
	// an authenticated caller, so a rejected token leaves it empty (omitted).
	Version string `json:"version,omitempty"`
	// Authenticated is present exactly when the API was reached, and then
	// always — false included, because "reachable but the token was not
	// accepted as admin" is the distinct failure the SPA names (it reads
	// `authenticated === false`). A pointer, so omitempty drops it only when
	// there is no answer at all rather than whenever the answer is no.
	Authenticated *bool  `json:"authenticated,omitempty"`
	Error         string `json:"error,omitempty"`
}

// authenticated reports whether the token was accepted; no answer is no.
func (a warpgateAPIHealth) authenticated() bool {
	return a.Authenticated != nil && *a.Authenticated
}

// warpgateSSHHealth is the SSH listener component: configured when
// --warpgate-ssh-addr is set, reachable when the host-key scan succeeded.
type warpgateSSHHealth struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	Addr       string `json:"addr,omitempty"`
	Error      string `json:"error,omitempty"`
}

// warpgateHostKeyBody is the pin's state as health reports it and as the
// accept answers it. Pinned and Observed are ALWAYS arrays, never null, so
// the SPA iterates them without a guard. Error is passed through whenever the
// check gave one — not only on unreachable: a pin that could not be stored
// (unpinned) or could not be parsed (mismatch) carries its reason too.
type warpgateHostKeyBody struct {
	State    string   `json:"state"`
	Pinned   []string `json:"pinned"`
	Observed []string `json:"observed"`
	Error    string   `json:"error,omitempty"`
}

// hostKeyBody renders a check's status. The warpgate package already never
// returns nil fingerprint slices; the nil guard keeps the never-null promise
// independent of that.
func hostKeyBody(st warpgate.HostKeyStatus) warpgateHostKeyBody {
	body := warpgateHostKeyBody{State: string(st.State), Pinned: st.Pinned, Observed: st.Observed, Error: st.Error}
	if body.Pinned == nil {
		body.Pinned = []string{}
	}
	if body.Observed == nil {
		body.Observed = []string{}
	}
	return body
}

// warpgateHealthResponse is the health endpoint's one body shape, at every
// state. HostKey is omitted when the SSH listener is not configured (nothing
// to have scanned) and when no pin was wired (see handleWarpgateHealth).
type warpgateHealthResponse struct {
	State   string               `json:"state"`
	API     warpgateAPIHealth    `json:"api"`
	SSH     warpgateSSHHealth    `json:"ssh"`
	HostKey *warpgateHostKeyBody `json:"hostKey,omitempty"`
}

// warpgateState derives the overall state, total over every combination of
// its inputs. The rule, straight out of ADR-0068:
//
//   - nothing configured                              → off
//   - every configured component unreachable          → unreachable
//   - some configured component unreachable           → degraded
//   - the API reachable but the token not accepted
//     as admin, or the host key mismatching the pin
//     or left unpinned                               → degraded
//   - anything else                                   → ok
//
// As in oneCLIState, unconfigured components are skipped rather than counted
// as failures (a REST-only lab whose API answers is "ok"), and Reachable is
// read only inside Configured, so an impossible reachable-but-unconfigured
// component can never manufacture health. The host key is not a component of
// its own for the count: its reachability IS the SSH listener's.
//
// An "unpinned" host key after a successful scan persists only when storing
// the first pin failed (the host key's error names why), and in that state
// every target-bearing spawn is refused — so it folds to degraded, like a
// mismatch: health must say what the spawn path will do.
func warpgateState(api warpgateAPIHealth, ssh warpgateSSHHealth, hostKey *warpgateHostKeyBody) string {
	configured, reachable := 0, 0
	for _, c := range []struct{ configured, reachable bool }{
		{api.Configured, api.Reachable},
		{ssh.Configured, ssh.Reachable},
	} {
		if !c.configured {
			continue
		}
		configured++
		if c.reachable {
			reachable++
		}
	}
	switch {
	case configured == 0:
		return warpgateStateOff
	case reachable == 0:
		return warpgateStateUnreachable
	case reachable < configured:
		return warpgateStateDegraded
	case api.Configured && api.Reachable && !api.authenticated():
		return warpgateStateDegraded
	case ssh.Configured && hostKey != nil &&
		(hostKey.State == string(warpgate.HostKeyMismatch) || hostKey.State == string(warpgate.HostKeyUnpinned)):
		return warpgateStateDegraded
	default:
		return warpgateStateOK
	}
}

// handleWarpgateHealth is GET /api/v1/warpgate/health. Always 200 (see the
// file comment); the state is in the body.
//
// Configured-ness is read off the wiring, as in onecli.go: the API component
// is configured exactly when cmd/lab built a client (a stray URL with no
// client is an unconfigured integration, not a half-live one), and the SSH
// component exactly when an address was given.
//
// The SSH probe is the pin's Check — which also pins on the first successful
// scan, the one write it ever makes (trust on first use, once, on the
// operator's host; ADR-0068). If no pin was wired although an address was
// (a wiring gap: cmd/lab builds the pin whenever the address is set), the
// listener is still probed, by a bare TCP dial, and hostKey is omitted rather
// than invented.
func (s *Server) handleWarpgateHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), warpgateProbeTimeout)
	defer cancel()

	api := warpgateAPIHealth{Configured: s.warpgate != nil}
	if api.Configured {
		// Redacted exactly like the OneCLI URLs: config validates the URL as
		// absolute http(s) but does not forbid userinfo.
		api.URL = redactedURL(s.warpgateAPIURL)
	}
	ssh := warpgateSSHHealth{Configured: s.warpgateSSHAddr != ""}
	if ssh.Configured {
		// host:port, validated by config (validateSSHAddr) — no userinfo form
		// exists for it, so it is echoed as given.
		ssh.Addr = s.warpgateSSHAddr
	}
	var hostKey *warpgateHostKeyBody

	// Two independent network waits on two different ports, run concurrently
	// so the worst case is the slower probe, not their sum. Each goroutine
	// writes only its own variables, all read after Wait.
	var wg sync.WaitGroup
	if api.Configured {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := s.warpgate.Health(ctx)
			if err != nil {
				api.Error = err.Error()
				return
			}
			api.Reachable = true
			api.Version = info.Version
			authenticated := info.Authenticated
			api.Authenticated = &authenticated
		}()
	}
	if ssh.Configured {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.warpgateHostKeys == nil {
				if err := dialProbe(ctx, s.warpgateSSHAddr); err != nil {
					ssh.Error = err.Error()
					return
				}
				ssh.Reachable = true
				return
			}
			st := s.warpgateHostKeys.Check(ctx)
			body := hostKeyBody(st)
			hostKey = &body
			// "unreachable" is the one state in which the comparison could not
			// be made — the scan failed, or (rarely) lab's own settings read
			// did; either way the status's Error says which.
			if st.State == warpgate.HostKeyUnreachable {
				ssh.Error = st.Error
				return
			}
			ssh.Reachable = true
		}()
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, warpgateHealthResponse{
		State:   warpgateState(api, ssh, hostKey),
		API:     api,
		SSH:     ssh,
		HostKey: hostKey,
	})
}

// dialProbe is the SSH component's fallback probe when no pin is wired: a
// bare TCP connect, closed at once — the weakest check that still proves a
// listener, exactly onecli.ProbeGateway's shape.
func dialProbe(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// warpgateHostKeyAcceptRequest is the accept's body.
type warpgateHostKeyAcceptRequest struct {
	Fingerprint string `json:"fingerprint"`
}

// handleWarpgateHostKeyAccept is POST /api/v1/warpgate/host-key/accept:
// 200 with the resulting host-key object on success.
//
//   - 409 when no SSH listener (or no pin) is configured — there is nothing
//     to accept a key for.
//   - 400 on a malformed body or an empty fingerprint.
//   - 409 when the fingerprint is not among the keys the listener presents
//     right now (warpgate.ErrFingerprintNotObserved): a state conflict, not a
//     malformed request — the operator verified a key that is not the one lab
//     sees, or it changed again since health was read. The message says so.
//   - 502 when the rescan fails: what broke is the upstream listener.
//   - 500 for anything else (storing the new pin failed — lab's own fault).
//
// Nothing is stored on any failure; the warpgate package guarantees that.
func (s *Server) handleWarpgateHostKeyAccept(w http.ResponseWriter, r *http.Request) {
	if s.warpgateSSHAddr == "" || s.warpgateHostKeys == nil {
		writeError(w, http.StatusConflict, warpgateSSHNotConfiguredMessage)
		return
	}
	var req warpgateHostKeyAcceptRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	fingerprint := strings.TrimSpace(req.Fingerprint)
	if fingerprint == "" {
		writeError(w, http.StatusBadRequest, `fingerprint is required: one of the "SHA256:…" fingerprints Warpgate health reports under hostKey.observed`)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), warpgateProbeTimeout)
	defer cancel()
	st, err := s.warpgateHostKeys.Accept(ctx, fingerprint)
	switch {
	case err == nil:
		s.log.Info("warpgate SSH host key accepted", "component", "httpapi", "fingerprint", fingerprint, "pinned", st.Pinned)
		writeJSON(w, http.StatusOK, hostKeyBody(st))
	case errors.Is(err, warpgate.ErrFingerprintNotObserved):
		// The error is the warpgate package's own text — the fingerprint, the
		// observed set and the address, all public — and it already tells the
		// operator to re-check health.
		writeError(w, http.StatusConflict, err.Error())
	case st.State == warpgate.HostKeyUnreachable:
		s.log.Warn("accepting the warpgate SSH host key: scan failed", "component", "httpapi", "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		s.internalError(w, "accepting the warpgate SSH host key", err)
	}
}
