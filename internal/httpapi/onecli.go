package httpapi

// OneCLI credential gateway health (issue #23 / ADR-0067): GET
// /api/v1/onecli/health, the one thing this epic does with the sidecar's
// reachability. It REPORTS, it never enforces — nothing here refuses, retries
// or pauses on a bad answer; the refusal is #24's, at the spawn, where it can
// name the run it is refusing and the repo whose secrets are missing.
//
// Two properties are pinned by the ADR and must not drift:
//
//   - The status is the payload, never the HTTP code. This endpoint always
//     answers 200, because it reports a DEPENDENCY's health, not lab's. A 503
//     for "the sidecar is down" would make lab's own API look broken to a
//     monitoring probe, and a 404/409 for "not configured" would force the SPA
//     to read two different response shapes for one question.
//   - "Not configured" is not "unhealthy". An unconfigured lab reports "off"
//     and that is a healthy, complete answer — a health surface that cries
//     about a feature nobody turned on is one operators learn to ignore.
//
// Probes are the two weakest checks that still prove what an operator needs:
// the REST API through the client's own Health call, and the gateway through a
// bare TCP dial (onecli.ProbeGateway — no CONNECT, no access token, because
// an agent's token is a per-repo credential, and spending a credential on a
// health check is how health checks start causing outages).

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
)

// oneCLIProbeTimeout bounds the WHOLE handler — both probes together, since
// they run concurrently. Short on purpose: this endpoint backs an operator
// screen and a monitoring probe, and the interesting failure (a sidecar that
// is configured but not listening) is exactly the one that would otherwise
// hang on a connect timeout. A dead host that black-holes SYNs answers in 3s
// with "unreachable", which is the true answer, arrived at early.
const oneCLIProbeTimeout = 3 * time.Second

// The four states of the OneCLI integration, as pinned by ADR-0067. They are
// the only values the state field ever takes.
const (
	oneCLIStateOff         = "off"
	oneCLIStateOK          = "ok"
	oneCLIStateDegraded    = "degraded"
	oneCLIStateUnreachable = "unreachable"
)

// oneCLIComponentHealth is one half of the answer: is this component
// configured at all, did it answer, at which URL, and if it did not, why.
//
// Error is OPERATOR-FACING TEXT and is therefore held to the same hygiene rule
// as the rest of the integration: it must be safe to render in a browser and
// safe to paste into a bug report. The onecli package guarantees the input
// side — an *APIError carries method, path and status, and the API key travels
// only in a header, so it structurally cannot be in one — and this file
// guarantees the rest by never composing a message out of configuration. The
// only config that reaches the payload is the two URLs below, deliberately, so
// an operator can see WHICH address answered or did not.
type oneCLIComponentHealth struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	URL        string `json:"url,omitempty"`
	// Status is the component's own self-reported health word, carried
	// through VERBATIM and only for the REST API (the gateway probe is a TCP
	// dial, which has no word to report). It exists because reachable and
	// healthy are not the same claim: a sidecar can answer 200 on /v1/health
	// while calling itself degraded, and flattening that into reachable=true
	// would have lab tell an operator "ok" over the sidecar's own objection.
	//
	// Lab deliberately does NOT interpret it. OneCLI publishes no vocabulary
	// for this field, so any allow-list of "healthy" words lab invented would
	// be a guess that silently mis-reports the day upstream adds a word — the
	// same unverified-wire-shape trap internal/onecli/wire.go exists to keep
	// in one place. State stays derived from reachability alone; the word is
	// shown to the human, who can read it.
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// oneCLIHealthResponse is the endpoint's one and only body shape, at every
// state and in every failure. State is derived from the two components by
// oneCLIState.
type oneCLIHealthResponse struct {
	State   string                `json:"state"`
	API     oneCLIComponentHealth `json:"api"`
	Gateway oneCLIComponentHealth `json:"gateway"`
}

// oneCLIState derives the overall state from the components, and is total over
// every combination of them — including the ones the handler cannot produce.
// The rule, straight out of ADR-0067:
//
//   - nothing configured        → off          (not an error; the default lab)
//   - every configured one up   → ok
//   - no configured one up      → unreachable
//   - some up, some not         → degraded
//
// Unconfigured components are skipped rather than counted as failures: a lab
// that set --onecli-gateway-url alone and got an answer is "ok", not
// "degraded", because there is no second thing it asked for. That is also why
// the count is taken over Configured only and Reachable is read strictly
// inside it — a Reachable=true on an unconfigured component (impossible here,
// but this function is also the unit under test) can never manufacture health.
func oneCLIState(components ...oneCLIComponentHealth) string {
	configured, reachable := 0, 0
	for _, c := range components {
		if !c.Configured {
			continue
		}
		configured++
		if c.Reachable {
			reachable++
		}
	}
	switch {
	case configured == 0:
		return oneCLIStateOff
	case reachable == configured:
		return oneCLIStateOK
	case reachable == 0:
		return oneCLIStateUnreachable
	default:
		return oneCLIStateDegraded
	}
}

// handleOneCLIHealth is GET /api/v1/onecli/health. Always 200 (see the file
// comment); the state is in the body.
func (s *Server) handleOneCLIHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), oneCLIProbeTimeout)
	defer cancel()

	// Configured-ness is read off the wiring, not off the config strings: the
	// API component is configured exactly when cmd/lab built a client for it
	// (a nil client with a stray URL is not a half-live integration, it is an
	// unconfigured one), and the gateway exactly when a URL was given, since
	// nothing else is needed to dial it.
	api := oneCLIComponentHealth{Configured: s.onecli != nil}
	if api.Configured {
		// Redacted like the gateway URL below, for the same reason: config
		// validates both as absolute http(s) URLs but neither forbids
		// userinfo, and an operator who put credentials in one would not
		// expect the other to be the leak.
		api.URL = redactedURL(s.oneCLIAPIURL)
	}
	gateway := oneCLIComponentHealth{Configured: s.oneCLIGatewayURL != ""}
	if gateway.Configured {
		gateway.URL = redactedURL(s.oneCLIGatewayURL)
	}

	// The two probes are independent network waits on two different ports, so
	// running them serially would double the worst case an operator sits
	// through — and the worst case is the common one when something is wrong.
	// Each goroutine writes only its own component, and both are read after
	// Wait, so there is nothing to synchronize beyond the barrier itself.
	var wg sync.WaitGroup
	if api.Configured {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := s.onecli.Health(ctx)
			if err != nil {
				api.Error = err.Error()
				return
			}
			api.Reachable = true
			api.Status = h.Status
		}()
	}
	if gateway.Configured {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := onecli.ProbeGateway(ctx, s.oneCLIGatewayURL); err != nil {
				gateway.Error = err.Error()
				return
			}
			gateway.Reachable = true
		}()
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, oneCLIHealthResponse{
		State:   oneCLIState(api, gateway),
		API:     api,
		Gateway: gateway,
	})
}

// redactedURL renders a configured URL safe to echo back. The URLs are echoed
// DELIBERATELY — "which address is lab dialing" is half of what makes this
// endpoint useful, and a loopback REST base is not a secret — but a proxy URL
// legitimately may carry userinfo (http://user:pw@host:10255), and userinfo is
// a credential like any other. url.URL.Redacted replaces the password with a
// placeholder and leaves everything an operator needs to recognize the address.
//
// A URL net/url refuses to parse yields "" rather than the raw string: it is
// unreachable in production (config.Parse already validated both URLs as
// absolute http(s) URLs before they ever reached this package), and it is
// precisely the input this function cannot prove is free of userinfo, so the
// only safe answer is to echo nothing. The state and error fields still carry
// the operator's signal in that case.
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Redacted()
}
