package onecli

// The agent operations. A OneCLI "agent" is an identity the gateway proxy
// authenticates a client as; lab's model (issue #23) maps ONE OneCLI agent to
// one lab repo, so the repo's grants are exactly that agent's grants and a run
// spawned for the repo carries that agent's proxy token. Everything here
// exists to make that mapping safe to establish lazily, from many goroutines,
// forever.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Health is GET /v1/health's answer: the sidecar's own liveness word and, when
// it reports one, its version. This is the read behind lab's "is the gateway
// configured and reachable" status signal, and it deliberately returns the
// server's Status verbatim rather than a boolean — "configured but degraded"
// and "configured and healthy" are different things for an operator, and lab
// should not flatten them at the transport layer.
type Health struct {
	Status  string
	Version string
}

// Health checks the OneCLI API. The endpoint needs no auth, but the request
// carries the Bearer header like every other call (see do) — one code path,
// no special case to get wrong. A non-2xx is an *APIError, so a wrong API key
// still shows up as a 401 here rather than as a confusing success.
func (c *Client) Health(ctx context.Context) (Health, error) {
	body, err := c.do(ctx, http.MethodGet, c.healthURL(), nil)
	if err != nil {
		return Health{}, err
	}
	wire, err := decodeOne[wireHealth](body, "health")
	if err != nil {
		return Health{}, err
	}
	// The field-by-field copy is deliberate; see the wire-mapping note in
	// wire.go for why a struct conversion is the wrong tool here.
	return Health{Status: wire.Status, Version: wire.Version}, nil //nolint:staticcheck // S1016: wire→domain mapping stays explicit so the two shapes can diverge
}

// Agent is one OneCLI agent identity: the id every other call addresses it by,
// and the name lab maps to a repo.
type Agent struct {
	ID   string
	Name string
}

// ListAgents lists the agents in the current project (the project the API key
// belongs to, or the one named by Options.ProjectID). Grants are NOT requested
// — the documented ?include=grants-summary is deliberately not used, because
// the only consumer that needs grants asks for them per agent through
// ListGrants, and a summary would be a second undocumented wire shape to keep
// true for no gain.
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	body, err := c.do(ctx, http.MethodGet, c.agentsURL(), nil)
	if err != nil {
		return nil, err
	}
	rows, err := decodeList[wireAgent](body, segAgents)
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(rows))
	for _, r := range rows {
		out = append(out, Agent{ID: r.ID, Name: r.Name}) //nolint:staticcheck // S1016: wire→domain mapping stays explicit (see wire.go)
	}
	return out, nil
}

// EnsureAgent returns the project's agent named name, creating it if it does
// not exist yet. It is idempotent by name and safe to call concurrently: lab
// calls it once per lab repo at spawn, from every spawn, forever, and the
// per-repo agent mapping the whole gateway design rests on (issue #23) is only
// as good as this method's refusal to ever create a duplicate.
//
// The sequence, and why each step is there:
//
//  1. List, and return the agent whose name matches EXACTLY. The match is
//     case-sensitive because a name is an identifier here, not prose — lab
//     repo names differing only in case are different repos, and folding them
//     together would hand one repo another repo's credentials.
//  2. Otherwise POST. Between (1) and (2) another lab goroutine (or another
//     lab process against the same OneCLI project) may create the same name.
//  3. If the POST answers 409 Conflict, that race happened and we lost it:
//     re-list ONCE and return the winner. The re-list is the entire reason
//     this is safe under concurrency — a 409 means the name exists, so a
//     second list must see it.
//  4. If the re-list still does not contain the name, that is an ERROR, never
//     a zero Agent and a nil error. A 409 for a name that then does not exist
//     means the conflict was about something other than this name (or the
//     listing is filtered) and lab's assumption is broken; reporting success
//     would hand the caller an empty agent id and, downstream, a run wired to
//     no identity at all.
//
// The same re-list resolves the other way this can go wrong: a POST that
// succeeds but answers a shape this package cannot read an id out of (see
// wire.go). Rather than return an agent with an empty ID — useless to every
// caller, since grants and tokens are addressed by id — it resolves the name
// through a list and errors if even that does not produce one.
func (c *Client) EnsureAgent(ctx context.Context, name string) (Agent, error) {
	if name == "" {
		return Agent{}, errors.New("onecli: agent name must not be empty")
	}

	agents, err := c.ListAgents(ctx)
	if err != nil {
		return Agent{}, err
	}
	if agent, ok := findAgent(agents, name); ok {
		return agent, nil
	}

	created, err := c.createAgent(ctx, name)
	switch {
	case err == nil && created.ID != "":
		return created, nil
	case err == nil:
		return c.resolveAgent(ctx, name, "the create succeeded but its answer carried no agent id")
	case isConflict(err):
		return c.resolveAgent(ctx, name, "the create answered 409 Conflict (another caller won the race)")
	default:
		return Agent{}, err
	}
}

// createAgent POSTs a new agent and decodes the created object.
func (c *Client) createAgent(ctx context.Context, name string) (Agent, error) {
	req := newWireCreateAgent(name)
	if req.Identifier == "" {
		// A name with no alphanumeric in it derives an empty slug, which OneCLI's
		// validation would 400. Lab's repo names (repo_<32 hex>) cannot get here;
		// failing locally keeps the error attributable if something else does.
		return Agent{}, fmt.Errorf("onecli: agent name %q contains no character usable in an identifier slug", name)
	}
	body, err := c.do(ctx, http.MethodPost, c.agentsURL(), req)
	if err != nil {
		return Agent{}, err
	}
	// A create that answers 204/empty is not a failure of the WRITE — the caller
	// resolves the name by listing instead (see EnsureAgent step 4).
	if len(body) == 0 {
		return Agent{}, nil
	}
	wire, err := decodeOne[wireAgent](body, "agent")
	if err != nil {
		return Agent{}, err
	}
	return Agent{ID: wire.ID, Name: wire.Name}, nil //nolint:staticcheck // S1016: wire→domain mapping stays explicit (see wire.go)
}

// resolveAgent re-lists and returns the agent named name, or an error that
// says why the re-list was needed. Both callers reached here having already
// established that the agent SHOULD exist, so a miss is a broken assumption
// and must be loud (never a zero Agent with a nil error).
func (c *Client) resolveAgent(ctx context.Context, name, because string) (Agent, error) {
	agents, err := c.ListAgents(ctx)
	if err != nil {
		return Agent{}, fmt.Errorf("onecli: resolving agent %q after %s: %w", name, because, err)
	}
	if agent, ok := findAgent(agents, name); ok {
		return agent, nil
	}
	return Agent{}, fmt.Errorf("onecli: agent %q is still absent from the project listing after %s; refusing to report success", name, because)
}

// findAgent returns the agent whose Name equals name exactly. Case-sensitive
// on purpose — see EnsureAgent step 1.
func findAgent(agents []Agent, name string) (Agent, bool) {
	for _, a := range agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// AgentToken is a minted proxy token for an agent: the credential a run
// presents to the gateway on port 10255. ExpiresAt is the zero time when the
// server reported no expiry — which means "no expiry reported", NOT "already
// expired"; a caller scheduling a refresh must check IsZero before comparing.
//
// The Token field is a secret. It exists to be handed to a run's proxy
// configuration and nothing else: never log it, never fold it into an error,
// never put it in a URL.
type AgentToken struct {
	Token     string
	ExpiresAt time.Time
}

// AgentToken mints (or regenerates) the agent's proxy token. This is a WRITE:
// the documented endpoint is a POST that regenerates, so calling it
// invalidates whatever token the agent had. Callers must treat it as such and
// not use it as a read-my-token probe.
func (c *Client) AgentToken(ctx context.Context, agentID string) (AgentToken, error) {
	if agentID == "" {
		return AgentToken{}, errors.New("onecli: agent id must not be empty")
	}
	body, err := c.do(ctx, http.MethodPost, c.agentTokenURL(agentID), nil)
	if err != nil {
		return AgentToken{}, err
	}
	wire, err := decodeOne[wireToken](body, "agent token")
	if err != nil {
		return AgentToken{}, err
	}
	if wire.Token == "" {
		// Never echo the body — this endpoint's body is a credential by design.
		return AgentToken{}, fmt.Errorf("onecli: the token answer for agent %q carried no token; this OneCLI build's wire shape differs from the one assumed in internal/onecli/wire.go", agentID)
	}
	token := AgentToken{Token: wire.Token}
	if raw := wire.expiresAt(); raw != "" {
		expires, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			// The value is not echoed: if the field mapping is wrong, whatever
			// landed in it could be anything, including the token itself.
			return AgentToken{}, fmt.Errorf("onecli: the token answer for agent %q carried an expiry that is not RFC3339; see internal/onecli/wire.go", agentID)
		}
		token.ExpiresAt = expires
	}
	return token, nil
}
