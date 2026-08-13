package onecli

// The agent operations. A OneCLI "agent" is an identity the gateway proxy
// authenticates a client as; lab's model (issue #23) maps ONE OneCLI agent to
// one lab repo, so the repo's grants are exactly that agent's grants and a run
// spawned for the repo carries that agent's access token. Everything here
// exists to make that mapping safe to establish lazily, from many goroutines,
// forever.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// Agent is one OneCLI agent identity: the id grants are addressed by, the
// name lab maps to a repo, and the agent's gateway access token.
//
// Token is a SECRET. OneCLI's agent listing carries each agent's stable
// access token (wire.go point 4) — the credential a run presents to the
// gateway proxy — and this type carries it to exactly one consumer, the
// spawn's gateway wiring. Never log an Agent with %v/%+v/%#v, never fold one
// into an error, never put the token in a URL that gets reported anywhere.
//
// Lab deliberately READS the token and never regenerates it. Upstream has a
// regenerate endpoint, but calling it at spawn would invalidate the token
// every already-running run of the same repo still holds — the second
// instance of a repo would silently 401 the first one's gateway calls, and
// the symptom (a run whose credentials stop working the moment a colleague
// starts another run) is about as far from its cause as a symptom gets. A
// stable read has no such failure mode, and it also survives lab restarts
// with no state at all.
type Agent struct {
	ID    string
	Name  string
	Token string
}

// ListAgents lists the agents in the current project (the project the API key
// belongs to, or the one named by Options.ProjectID). Grants are NOT requested
// — the documented ?include=grants-summary is deliberately not used, because
// the only consumer that needs grants asks for them per agent through
// ListGrants, and a summary would be a second wire shape to keep true for no
// gain.
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
		out = append(out, Agent{ID: r.ID, Name: r.Name, Token: r.AccessToken}) //nolint:staticcheck // S1016: wire→domain mapping stays explicit (see wire.go)
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
//     together would hand one repo another repo's credentials. The listing
//     row carries the agent's access token, so the steady state — every spawn
//     after the first — is one GET and done.
//  2. Otherwise POST. Between (1) and (2) another lab goroutine (or another
//     lab process against the same OneCLI project) may create the same name.
//  3. A successful create is ALWAYS followed by a re-list: the create answer
//     does not carry the access token (wire.go point 5), and an Agent without
//     its token is useless to the one caller this exists for. The re-list
//     reads the same stable token every other caller reads — nothing is
//     regenerated, so the creator cannot invalidate anyone (see Agent).
//  4. If the POST answers 409 Conflict, that race happened and we lost it:
//     re-list ONCE and return the winner. The re-list is the entire reason
//     this is safe under concurrency — a 409 means the name exists, so a
//     second list must see it.
//  5. If the re-list still does not contain the name, that is an ERROR, never
//     a zero Agent and a nil error. A 409 for a name that then does not exist
//     means the conflict was about something other than this name (or the
//     listing is filtered) and lab's assumption is broken; reporting success
//     would hand the caller an empty agent and, downstream, a run wired to no
//     identity at all.
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

	switch err := c.createAgent(ctx, name); {
	case err == nil:
		return c.resolveAgent(ctx, name, "the create succeeded (the create answer carries no access token)")
	case isConflict(err):
		return c.resolveAgent(ctx, name, "the create answered 409 Conflict (another caller won the race)")
	default:
		return Agent{}, err
	}
}

// createAgent POSTs a new agent. The answer body is deliberately not decoded:
// it never carries the access token, so every successful create is resolved
// by re-listing anyway (EnsureAgent step 3), and a second decode path would
// be one more wire shape to keep true for nothing.
func (c *Client) createAgent(ctx context.Context, name string) error {
	req := newWireCreateAgent(name)
	if req.Identifier == "" {
		// A name with no alphanumeric in it derives an empty slug, which OneCLI's
		// validation would 400. Lab's repo names (repo_<32 hex>) cannot get here;
		// failing locally keeps the error attributable if something else does.
		return fmt.Errorf("onecli: agent name %q contains no character usable in an identifier slug", name)
	}
	_, err := c.do(ctx, http.MethodPost, c.agentsURL(), req)
	return err
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
