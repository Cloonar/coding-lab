package httpapi

// Warpgate SSH bastion health (issue #39 / ADR-0068): GET
// /api/v1/warpgate/health. The bastion is the non-HTTP sibling of the OneCLI
// credential gateway, and its health is built on onecli.go's contract, which
// is pinned the same way here:
//
//   - The status is the payload, never the HTTP code. Health always answers
//     200, because it reports a DEPENDENCY's health, not lab's.
//   - "Not configured" is not "unhealthy". Nothing configured reports "off",
//     a complete answer, never an error.
//
// Health folds three things where OneCLI's folds two: the admin API (reached
// through the client's own Health, which also says whether the token was
// accepted with every admin permission lab uses), the SSH listener, and its
// host key against what lab trusts (--warpgate-ssh-host-key). The last two
// are ONE probe: the host-key check scans the listener — one SSH handshake
// per host-key algorithm, aborted before any authentication — so a
// successful scan is the listener's reachability, and the same scan is what
// the trusted keys are compared against. A separate TCP dial next to it
// would be a second opinion on a question the scan already answered with
// more proof (a TCP accept proves a port, a host key proves an SSH server).
//
// What the operator does with a host-key finding is configuration, not an
// API call: a mismatch is fixed by putting the listener's current key in
// --warpgate-ssh-host-key, and an unpinned listener is pinned the same way,
// from the observed fingerprints health lists.

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// warpgateProbeTimeout bounds the WHOLE health handler — both probes run
// concurrently under it. It is longer than oneCLIProbeTimeout because the SSH
// probe is a scan, not a dial: up to five handshakes back to back (warpgate's
// hostKeyAlgorithms), three of which a Warpgate without ECDSA keys refuses in
// key exchange. Against a same-host sidecar that is well under a second; a
// listener that black-holes the dial still answers "unreachable" inside the
// bound, since the scan fails on the first handshake that is not a plain
// "algorithm not offered".
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

// WarpgateHostKeyPin is the seam over the pin of Warpgate's SSH host key(s)
// this package drives: Check, for health. *warpgate.HostKeyPin satisfies it;
// the assertion below is the compile-time proof. It is deliberately narrower
// than the pin — KnownHosts is the spawn path's (internal/instance), and
// nothing reachable over HTTP renders a run's known_hosts.
type WarpgateHostKeyPin interface {
	Check(ctx context.Context) warpgate.HostKeyStatus
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

// warpgateHostKeyBody is the pin's state as health reports it. Pinned (the
// trusted keys of --warpgate-ssh-host-key) and Observed (the listener's
// keys, from the scan) are ALWAYS arrays, never null, so the SPA iterates
// them without a guard. Error is passed through whenever the check gave one.
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
//     as admin, or the host key not among the
//     trusted keys (mismatch)                         → degraded
//   - anything else                                   → ok
//
// As in oneCLIState, unconfigured components are skipped rather than counted
// as failures (a REST-only lab whose API answers is "ok"), and Reachable is
// read only inside Configured, so an impossible reachable-but-unconfigured
// component can never manufacture health. The host key is not a component of
// its own for the count: its reachability IS the SSH listener's.
//
// An "unpinned" host key — no --warpgate-ssh-host-key configured — is ok,
// not degraded: the operator chose to accept whatever the listener presents,
// and every target-bearing spawn proceeds on that. Health lists the observed
// fingerprints so the setting can be filled in.
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
	case ssh.Configured && hostKey != nil && hostKey.State == string(warpgate.HostKeyMismatch):
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
// The SSH probe is the pin's Check. If no pin was wired although an address
// was (a wiring gap: cmd/lab builds the pin whenever the address is set), the
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
			// "unreachable" is the one state in which the scan failed; the
			// status's Error says why.
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
