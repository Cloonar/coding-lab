package httpapi

// The per-repo grant picker's server side (issue #25 / ADR-0067): the lab-wide
// credential pool, a repo's current grants, and the attach/detach pair that
// changes them. Four endpoints, all of them a thin PROXY of OneCLI's REST API
// — lab stores no mirror of the pool or of a grant set, because OneCLI is the
// single source of truth for both and a cached copy would be a second answer
// to the same question, wrong exactly when it matters (an operator who just
// changed something in the OneCLI dashboard).
//
// Why lab proxies at all, rather than letting the SPA call OneCLI directly:
// the sidecar's REST API is authenticated by the PROJECT API key, a credential
// that must never reach a browser, and its address is loopback-or-internal by
// design (ADR-0067's two-port split). Lab already holds the key and already
// authenticates the operator, so the proxy is the only shape where the browser
// needs no credential of its own.
//
// Three properties are pinned here and must not drift:
//
//   - A READ never creates anything. Both GETs resolve the repo's agent
//     identity by listing and matching; only the attach — an explicit operator
//     mutation — may call EnsureAgent. This is what makes "open the picker for
//     every repo" free of side effects, instead of littering the OneCLI project
//     with an identity per repo anyone ever looked at.
//   - "Not configured" is an ANSWER for the two reads (200 with
//     configured:false and empty arrays, matching onecli.go's health rule) and
//     an ERROR for the two mutations (409). A picker rendering "the gateway is
//     off" is a healthy screen; a click that silently succeeds against an
//     integration that is off is a lie.
//   - The agent identity's access token NEVER reaches a response. It is a live
//     gateway credential (onecli.Agent's hygiene rules), and the structural
//     guarantee is below: no handler here holds an onecli.Agent — the two
//     resolvers return the identity's ID as a bare string and nothing else.

import (
	"context"
	"fmt"
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// oneCLINotConfiguredMessage is what a mutation answers when the integration
// is off. It names the two flags that turn it on because the only way to reach
// this from lab's own UI is a deployment that never configured the sidecar,
// and the operator reading the toast is the one who can fix that.
const oneCLINotConfiguredMessage = "the OneCLI credential gateway is not configured on this lab; set --onecli-url and --onecli-api-key-file to attach grants"

// oneCLIGrantAPI is the OneCLI REST seam this file drives. It is deliberately
// a SECOND, wider interface next to instance.GatewayAPI rather than a widening
// of it: that one is ensure+list only so that a spawn structurally cannot
// mutate a repo's grants, and the picker is the one place that may. Keeping
// them separate means neither seam has a method its consumer must be trusted
// not to call.
//
// Satisfied by *onecli.Client; the assertion below is the compile-time proof.
type oneCLIGrantAPI interface {
	ListSecrets(ctx context.Context) ([]onecli.Secret, error)
	ListConnections(ctx context.Context) ([]onecli.Connection, error)
	ListAgents(ctx context.Context) ([]onecli.Agent, error)
	EnsureAgent(ctx context.Context, identifier, displayName string) (onecli.Agent, error)
	ListGrants(ctx context.Context, agentID string) ([]onecli.Grant, error)
	AttachGrant(ctx context.Context, agentID string, kind onecli.GrantKind, resourceID string) error
	DetachGrant(ctx context.Context, agentID string, kind onecli.GrantKind, resourceID string) error
}

var _ oneCLIGrantAPI = (*onecli.Client)(nil)

// grantAPI reports whether the OneCLI integration is configured, and hands out
// the seam when it is. ok=false is the normal state of a lab that never set
// the sidecar up, and each handler decides what that means for it.
//
// The nil check is on the CONCRETE pointer, before it is ever assigned into an
// interface variable, and that ordering is load-bearing: a nil *onecli.Client
// stored in an interface yields a non-nil interface wrapping a nil pointer, so
// a later `api == nil` would read false and the first method call would panic
// inside the client. Same trap cmd/lab/main.go guards against when it wires
// the launch path's GatewayAPI.
func (s *Server) grantAPI() (oneCLIGrantAPI, bool) {
	if s.onecli == nil {
		return nil, false
	}
	return s.onecli, true
}

// oneCLIPoolEntry is one grantable resource — a project secret or a provider
// connection — as the picker renders it. Metadata only: a VALUE never crosses
// this boundary, because lab never reads one (pool.go) and the OneCLI
// dashboard stays the only place a value is created or edited (ADR-0067).
type oneCLIPoolEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// oneCLIPoolResponse is GET /api/v1/onecli/pool's one body shape, at every
// state. Both arrays are ALWAYS present and never null, so the SPA reads one
// shape whether the integration is off, the pool is empty, or it is full — and
// Configured is what distinguishes the first two, which a bare empty pool
// could not.
type oneCLIPoolResponse struct {
	Configured  bool              `json:"configured"`
	Secrets     []oneCLIPoolEntry `json:"secrets"`
	Connections []oneCLIPoolEntry `json:"connections"`
}

// oneCLIGrantEntry is one attached pool resource. Kind is the same plural word
// the URL and onecli.GrantKind use, so nothing in the round trip
// (list → click → PUT/DELETE) needs a mapping table.
type oneCLIGrantEntry struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// oneCLIGrantsResponse is GET /api/v1/repos/{id}/onecli/grants's one body
// shape — same Configured rule as the pool, same never-null array.
type oneCLIGrantsResponse struct {
	Configured bool               `json:"configured"`
	Grants     []oneCLIGrantEntry `json:"grants"`
}

// emptyPool is the body of an unconfigured lab's pool read.
func emptyPool() oneCLIPoolResponse {
	return oneCLIPoolResponse{Secrets: []oneCLIPoolEntry{}, Connections: []oneCLIPoolEntry{}}
}

// noGrants is the body for "no grants to report" — both the unconfigured lab
// (configured=false) and the repo whose agent identity does not exist yet
// (configured=true).
func noGrants(configured bool) oneCLIGrantsResponse {
	return oneCLIGrantsResponse{Configured: configured, Grants: []oneCLIGrantEntry{}}
}

// handleOneCLIPool is GET /api/v1/onecli/pool: everything an operator could
// attach, lab-wide (one OneCLI project for the whole lab, ADR-0067).
func (s *Server) handleOneCLIPool(w http.ResponseWriter, r *http.Request) {
	api, ok := s.grantAPI()
	if !ok {
		writeJSON(w, http.StatusOK, emptyPool())
		return
	}

	// Serial, not concurrent like the health probes: these are two cheap GETs
	// against a sidecar lab reaches on loopback, and short-circuiting on the
	// first failure gives the operator ONE error naming what broke rather than
	// two describing the same outage.
	secrets, err := api.ListSecrets(r.Context())
	if err != nil {
		s.writeOneCLIGatewayError(w, "listing the OneCLI credential pool's secrets", err)
		return
	}
	connections, err := api.ListConnections(r.Context())
	if err != nil {
		s.writeOneCLIGatewayError(w, "listing the OneCLI credential pool's connections", err)
		return
	}

	out := oneCLIPoolResponse{Configured: true, Secrets: make([]oneCLIPoolEntry, 0, len(secrets)), Connections: make([]oneCLIPoolEntry, 0, len(connections))}
	for _, sec := range secrets {
		out.Secrets = append(out.Secrets, oneCLIPoolEntry{ID: sec.ID, Name: sec.Name, Provider: sec.Provider})
	}
	for _, conn := range connections {
		out.Connections = append(out.Connections, oneCLIPoolEntry{ID: conn.ID, Name: conn.Name, Provider: conn.Provider})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleOneCLIGrantList is GET /api/v1/repos/{id}/onecli/grants: what this
// repo's runs may reach. Read-only in the strongest sense — it never creates
// the agent identity (see oneCLIAgentIdentityID).
func (s *Server) handleOneCLIGrantList(w http.ResponseWriter, r *http.Request) {
	// The repo is resolved FIRST, so an unknown id is a 404 whether or not the
	// integration is configured: the path names a repo, and answering "here
	// are no grants" for a repo that does not exist would hide a stale link.
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	api, ok := s.grantAPI()
	if !ok {
		writeJSON(w, http.StatusOK, noGrants(false))
		return
	}

	agentID, found, err := oneCLIAgentIdentityID(r.Context(), api, repo)
	if err != nil {
		s.writeOneCLIGatewayError(w, "resolving the repo's OneCLI agent identity", err)
		return
	}
	if !found {
		// No identity yet means no grants yet — the normal state of every repo
		// until its first attach or first gateway-wired spawn. It is an empty
		// answer, never an error, and emphatically not a reason to create one.
		writeJSON(w, http.StatusOK, noGrants(true))
		return
	}

	grants, err := api.ListGrants(r.Context(), agentID)
	if err != nil {
		s.writeOneCLIGatewayError(w, "listing the repo's OneCLI grants", err)
		return
	}
	out := oneCLIGrantsResponse{Configured: true, Grants: make([]oneCLIGrantEntry, 0, len(grants))}
	for _, g := range grants {
		out.Grants = append(out.Grants, oneCLIGrantEntry{Kind: string(g.Kind), ID: g.ID, Name: g.Name})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleOneCLIGrantAttach is PUT
// /api/v1/repos/{id}/onecli/grants/{kind}/{resourceId}: 204, no body.
//
// This is the ONE path here that may create the repo's agent identity — or
// rename one, since EnsureAgent brings a stale display name back in line — and
// the lazy creation is deliberate: an identity exists once someone decides the
// repo should reach something, which is exactly this click. EnsureAgent is
// idempotent and 409-tolerant, and AttachGrant is a PUT, so replaying a whole
// selection is safe.
func (s *Server) handleOneCLIGrantAttach(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	kind, resourceID, ok := oneCLIGrantTarget(w, r)
	if !ok {
		return
	}
	api, ok := s.grantAPI()
	if !ok {
		writeError(w, http.StatusConflict, oneCLINotConfiguredMessage)
		return
	}

	agentID, err := ensureOneCLIAgentIdentityID(r.Context(), api, repo)
	if err != nil {
		s.writeOneCLIGatewayError(w, "resolving the repo's OneCLI agent identity", err)
		return
	}
	if err := api.AttachGrant(r.Context(), agentID, kind, resourceID); err != nil {
		s.writeOneCLIGatewayError(w, "attaching the OneCLI grant", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleOneCLIGrantDetach is DELETE
// /api/v1/repos/{id}/onecli/grants/{kind}/{resourceId}: 204, no body.
func (s *Server) handleOneCLIGrantDetach(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	kind, resourceID, ok := oneCLIGrantTarget(w, r)
	if !ok {
		return
	}
	api, ok := s.grantAPI()
	if !ok {
		writeError(w, http.StatusConflict, oneCLINotConfiguredMessage)
		return
	}

	// Read-only resolution, never EnsureAgent: creating an identity in order to
	// take something away from it is absurd, and it would turn "revoke
	// everything" — a picker replaying an empty selection — into a machine that
	// manufactures identities.
	agentID, found, err := oneCLIAgentIdentityID(r.Context(), api, repo)
	if err != nil {
		s.writeOneCLIGatewayError(w, "resolving the repo's OneCLI agent identity", err)
		return
	}
	if !found {
		// Nothing to detach from. 204 rather than 404: the caller asked for a
		// state (this resource is not granted) that already holds.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := api.DetachGrant(r.Context(), agentID, kind, resourceID); err != nil {
		s.writeOneCLIGatewayError(w, "detaching the OneCLI grant", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// oneCLIGrantTarget validates the two path segments a mutation addresses its
// resource by, answering 400 itself. ok=false means the response was written.
//
// The kind check runs at the HTTP EDGE, before any OneCLI call, even though
// the client validates it again: GrantKind's value doubles as a path segment
// in OneCLI's URL (wire.go), so an unchecked kind is a caller-controlled path
// element, and the cheapest place to refuse a caller's string is before it has
// steered any request at all. The two checks are not redundant — they guard
// two different boundaries.
func oneCLIGrantTarget(w http.ResponseWriter, r *http.Request) (onecli.GrantKind, string, bool) {
	kind := onecli.GrantKind(r.PathValue("kind"))
	switch kind {
	case onecli.GrantSecret, onecli.GrantConnection:
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown grant kind %q; want %q or %q", kind, onecli.GrantSecret, onecli.GrantConnection))
		return "", "", false
	}
	// The empty resource id is belt and braces: a URL truncated to
	// …/grants/secrets/ does not match the route's second wildcard at all and
	// lands on the API tree's JSON 404 long before this. The check stays
	// because a helper that returns an id its caller then sends to OneCLI must
	// be total on its own input, not on the router's current behaviour — and
	// because the empty id is exactly the shape that would address an agent
	// identity's whole grants collection rather than one grant.
	resourceID := r.PathValue("resourceId")
	if resourceID == "" {
		writeError(w, http.StatusBadRequest, "the grant's resource id must not be empty")
		return "", "", false
	}
	return kind, resourceID, true
}

// oneCLIAgentIdentityID resolves the repo's OneCLI agent identity WITHOUT
// creating it, returning its id and whether it exists at all. found=false is
// an ordinary state, not an error.
//
// The field matched on is the identity's IDENTIFIER, derived from the repo's
// STORE ID (onecli.AgentIdentifier), and the match is exact and
// case-sensitive — the same rule internal/onecli's own findAgent applies, and
// the same one internal/instance's launch path establishes the mapping with.
// The agent's NAME is display text lab overwrites (issue #35) and is matched on
// nowhere. A divergence in either direction is silent and expensive: a looser
// match could hand one repo another repo's credentials, and a different key
// would create a second, grant-less identity whose symptom is "my secrets
// vanished".
//
// It returns the ID as a bare string, never the onecli.Agent it came from.
// That is the structural half of this file's no-token-in-a-response rule: a
// handler that never holds an Agent cannot serialize one by accident.
func oneCLIAgentIdentityID(ctx context.Context, api oneCLIGrantAPI, repo store.Repo) (string, bool, error) {
	agents, err := api.ListAgents(ctx)
	if err != nil {
		return "", false, err
	}
	identifier := onecli.AgentIdentifier(repo.ID)
	for _, a := range agents {
		if a.Identifier == identifier {
			return a.ID, true, nil
		}
	}
	return "", false, nil
}

// ensureOneCLIAgentIdentityID is the attach path's resolver: same match key,
// but creating the identity when it does not exist yet. It hands EnsureAgent
// the repo's NAME as the display name, which makes the attach click one of the
// touchpoints that heals a rename — the identity keeps the identifier its
// grants hang off, and its dashboard row catches up with what the operator
// clicking here is looking at. It drops the Agent for the same reason its
// read-only sibling does: the caller needs an id, and anything more is a
// credential it has no business holding.
func ensureOneCLIAgentIdentityID(ctx context.Context, api oneCLIGrantAPI, repo store.Repo) (string, error) {
	identity, err := api.EnsureAgent(ctx, onecli.AgentIdentifier(repo.ID), repo.Name)
	if err != nil {
		return "", err
	}
	return identity.ID, nil
}

// writeOneCLIGatewayError answers a failed sidecar call: 502, because what
// broke is the upstream lab proxies, not lab itself — a 500 here would send an
// operator reading lab's logs after a sidecar outage.
//
// The underlying error is forwarded VERBATIM to the browser, deliberately. It
// is the only thing that distinguishes "the sidecar is down" from "the API key
// was revoked" from "this OneCLI build's wire shape changed", and every one of
// those is the operator's to fix. It is safe to forward because of what the
// onecli package guarantees about its errors: an *APIError carries method,
// path, status and a bounded server message, and the API key travels only in a
// header, so it structurally cannot be in one. Keep it that way — do not
// compose a message here out of configuration.
func (s *Server) writeOneCLIGatewayError(w http.ResponseWriter, doing string, err error) {
	s.log.Warn(doing, "component", "httpapi", "err", err)
	writeError(w, http.StatusBadGateway, fmt.Sprintf("%s: %s", doing, err))
}
