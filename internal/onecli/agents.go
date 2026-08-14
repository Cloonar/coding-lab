package onecli

// The agent operations. A OneCLI "agent" is an identity the gateway proxy
// authenticates a client as; lab's model (issue #23) maps ONE OneCLI agent to
// one lab repo, so the repo's grants are exactly that agent's grants and a run
// spawned for the repo carries that agent's access token. Everything here
// exists to make that mapping safe to establish lazily, from many goroutines,
// forever.
//
// The addressing convention that mapping implies, and the one thing a caller
// outside this package has to hold in its head: an AGENT is addressed by its
// IDENTIFIER (EnsureAgent, DeleteAgent) — the slug AgentIdentifier derives
// from a repo's store ID, which is the only handle a caller naturally has for
// an identity it thinks of as "this repo's". A GRANT is addressed by the
// agent's upstream ID, which the caller is already holding, because it got an
// Agent back from EnsureAgent or ListAgents before it had anything to attach.
// So no consumer ever has to learn upstream's internal id for an agent it only
// knows as a repo, and none has to re-resolve one it already has.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
// identifier lab maps to a repo, the name a human reads, and the agent's
// gateway access token.
//
// Identifier is the MATCH KEY, and the only field lab may resolve an agent by.
// Upstream constrains it unique per project and immutable (wire.go point 4),
// and lab derives it from the repo's STORE ID (AgentIdentifier), so it holds
// still across every rename on either side.
//
// Name is the opposite of that: a DISPLAY string, owned by lab and overwritten
// by it. EnsureAgent stomps whatever it finds with the repo's current name, so
// a rename typed into the OneCLI dashboard survives exactly until the next
// ensure — that is the deal issue #35 struck to make the dashboard readable
// without lab storing an id anywhere. Never match on it: upstream constrains a
// name neither unique nor immutable, and the project is SHARED surface — it
// holds agents lab never created, which may legitimately carry a name equal to
// a lab repo's — so matching on a name is how a repo ends up holding an
// identity whose grants were never meant for it.
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
	ID         string
	Identifier string
	Name       string
	Token      string
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
		out = append(out, Agent{ID: r.ID, Identifier: r.Identifier, Name: r.Name, Token: r.AccessToken}) //nolint:staticcheck // S1016: wire→domain mapping stays explicit (see wire.go)
	}
	return out, nil
}

// EnsureAgent returns the project's agent carrying identifier, creating it if
// it does not exist yet, and keeps its display name equal to displayName. It
// is idempotent by identifier and safe to call concurrently: lab calls it once
// per lab repo at repo creation, at startup, at every spawn and at every
// grant-attach, forever, and the per-repo agent mapping the whole gateway
// design rests on (issue #23) is only as good as this method's refusal to ever
// create a duplicate.
//
// identifier is the slug AgentIdentifier derives from a repo's store ID, and
// this method insists on being handed one that already IS a well-formed slug
// rather than sluggifying whatever arrives. A caller that forgot to derive
// then fails locally and attributably, instead of as an opaque upstream 400 —
// or, worse, as an agent quietly created under an identifier no other call
// site will ever derive again, invisible to every later ensure.
//
// The sequence, and why each step is there:
//
//  1. List, and return the agent whose IDENTIFIER matches EXACTLY. The match
//     is exact and case-sensitive because the identifier IS the unique key
//     upstream (@@unique([projectId, identifier]), wire.go point 4) — there is
//     no looser match that could be correct, and folding two identifiers
//     together would hand one repo another repo's credentials. The listing row
//     carries the agent's access token, so the steady state — every touchpoint
//     after the first — is one GET and done.
//  2. On that hit, if the agent's name is not displayName, PATCH it (wire.go
//     point 10) and report the agent under its new name. This is the whole
//     rename story: it ADOPTS an agent created before issue #35, whose name is
//     still the repo's store ID and whose identifier is byte-identical to the
//     one derived today, and it heals a repo rename at whichever touchpoint
//     comes first — with no stored state and no call site that exists only to
//     rename.
//     A rename that FAILS is deliberately not an error: the agent found in (1)
//     is returned unchanged, carrying its stale name. Issue #35 decision 7
//     pins that degradation, and the asymmetry is the reason — the name is
//     cosmetic, while the launch path is fail-closed on EnsureAgent, so
//     surfacing this error would take a spawn down over a display string. The
//     failure is silent rather than warned because this package has no logger
//     and will not grow one for this; the next touchpoint tries again.
//  3. Otherwise POST {name: displayName, identifier: identifier}. Between (1)
//     and (3) another lab goroutine (or another lab process against the same
//     OneCLI project) may create the same identifier.
//  4. A successful create is ALWAYS followed by a re-list: the create answer
//     does not carry the access token (wire.go point 5), and an Agent without
//     its token is useless to the one caller this exists for. The re-list
//     reads the same stable token every other caller reads — nothing is
//     regenerated, so the creator cannot invalidate anyone (see Agent).
//  5. If the POST answers 409 Conflict, that race happened and we lost it:
//     re-list ONCE and return the winner. The re-list is the entire reason
//     this is safe under concurrency — a 409 means the identifier exists, so a
//     second list must see it.
//  6. If the re-list still does not carry the identifier, that is an ERROR,
//     never a zero Agent and a nil error. A 409 for an identifier that then
//     does not exist means the conflict was about something else (or the
//     listing is filtered) and lab's assumption is broken; reporting success
//     would hand the caller an empty agent and, downstream, a run wired to no
//     identity at all.
func (c *Client) EnsureAgent(ctx context.Context, identifier, displayName string) (Agent, error) {
	// The empty identifier is checked separately because it would survive the
	// slug comparison below (deriving "" yields ""), and an empty match key
	// matches whatever degenerate row upstream might answer with.
	if identifier == "" {
		return Agent{}, errors.New("onecli: agent identifier must not be empty")
	}
	if AgentIdentifier(identifier) != identifier {
		return Agent{}, fmt.Errorf("onecli: %q is not a well-formed OneCLI identifier slug; derive it from the repo's store ID with onecli.AgentIdentifier", identifier)
	}
	// Upstream requires 1–255 characters after trimming, so a name that is only
	// whitespace is a 400 waiting to happen — and would be an unreadable row in
	// the dashboard even if it were accepted.
	if strings.TrimSpace(displayName) == "" {
		return Agent{}, fmt.Errorf("onecli: agent %q must have a non-empty display name", identifier)
	}

	agents, err := c.ListAgents(ctx)
	if err != nil {
		return Agent{}, err
	}
	if agent, ok := findAgent(agents, identifier); ok {
		return c.healDisplayName(ctx, agent, displayName), nil
	}

	switch err := c.createAgent(ctx, identifier, displayName); {
	case err == nil:
		return c.resolveAgent(ctx, identifier, "the create succeeded (the create answer carries no access token)")
	case isConflict(err):
		return c.resolveAgent(ctx, identifier, "the create answered 409 Conflict (another caller won the race)")
	default:
		return Agent{}, err
	}
}

// healDisplayName brings a found agent's name back to the one lab says it
// should have, and returns the agent the caller reports. Nothing happens when
// the name is already current — the steady state must stay one GET (EnsureAgent
// step 1), and a PATCH per spawn would be a write on every launch of every repo
// forever.
//
// A failed PATCH is swallowed and the agent returned with the stale name it
// still has upstream; see EnsureAgent step 2 for why a cosmetic write must not
// fail a fail-closed caller. A successful one updates the name locally rather
// than re-reading it: the PATCH is the authority on what upstream now carries
// (wire.go point 10), and a confirming re-list would put a second GET behind
// every rename to learn what lab just wrote.
func (c *Client) healDisplayName(ctx context.Context, agent Agent, displayName string) Agent {
	if agent.Name == displayName {
		return agent
	}
	if _, err := c.do(ctx, http.MethodPatch, c.agentURL(agent.ID), wireRenameAgent{Name: displayName}); err != nil {
		return agent
	}
	agent.Name = displayName
	return agent
}

// createAgent POSTs a new agent. The answer body is deliberately not decoded:
// it never carries the access token, so every successful create is resolved
// by re-listing anyway (EnsureAgent step 4), and a second decode path would
// be one more wire shape to keep true for nothing. Both arguments arrive
// already validated by EnsureAgent, which is the only caller.
func (c *Client) createAgent(ctx context.Context, identifier, displayName string) error {
	_, err := c.do(ctx, http.MethodPost, c.agentsURL(), newWireCreateAgent(identifier, displayName))
	return err
}

// resolveAgent re-lists and returns the agent carrying identifier, or an error
// that says why the re-list was needed. Both callers reached here having
// already established that the agent SHOULD exist, so a miss is a broken
// assumption and must be loud (never a zero Agent with a nil error).
//
// It deliberately does not heal the name: whoever just created the agent —
// this call or the caller that won the race — created it with a display name,
// and a rename on top of a create would be a write chasing a write. A name
// that is somehow stale here is healed at the next touchpoint like any other.
func (c *Client) resolveAgent(ctx context.Context, identifier, because string) (Agent, error) {
	agents, err := c.ListAgents(ctx)
	if err != nil {
		return Agent{}, fmt.Errorf("onecli: resolving agent %q after %s: %w", identifier, because, err)
	}
	if agent, ok := findAgent(agents, identifier); ok {
		return agent, nil
	}
	return Agent{}, fmt.Errorf("onecli: agent %q is still absent from the project listing after %s; refusing to report success", identifier, because)
}

// DeleteAgent removes the agent carrying identifier, reporting whether there
// was one to remove. Lab calls it when a repo is deleted, so a repo's identity
// does not outlive the repo with its grants still attached.
//
// (false, nil) — nothing in the project carries that identifier — is an
// ORDINARY answer, never an error: a repo created before OneCLI was configured
// never got an agent, and a delete with nothing to delete has done its job.
//
// What upstream does (wire.go point 11): the agent's grants cascade away with
// it, while the project's secret/connection POOL is untouched. Deleting a
// repo's agent revokes that repo's access to shared credentials; it does not
// destroy the credentials, which other repos and other tools are still using.
// Every other failure surfaces as the *APIError it is — in particular the 400
// "Cannot delete the default agent", which lab-created agents can never
// trigger, so that answer means this identifier resolved to an agent lab did
// not create: something for the caller to REPORT, never to work around.
//
// And the rule that shapes this whole file: NEVER delete-and-recreate an agent
// in order to rename it. That would destroy the grant set an operator attached
// (the cascade above) and mint a new access token, 401ing every in-flight run
// of that repo (see Agent). A rename is the PATCH in healDisplayName or it
// does not happen — issue #35 decision 7.
func (c *Client) DeleteAgent(ctx context.Context, identifier string) (bool, error) {
	// An empty identifier is a caller bug rather than a miss: it would match a
	// row whose identifier upstream left empty, and deleting an agent nobody
	// asked about is not a failure mode worth having.
	if identifier == "" {
		return false, errors.New("onecli: agent identifier must not be empty")
	}
	agents, err := c.ListAgents(ctx)
	if err != nil {
		return false, err
	}
	agent, ok := findAgent(agents, identifier)
	if !ok {
		return false, nil
	}
	if _, err := c.do(ctx, http.MethodDelete, c.agentURL(agent.ID), nil); err != nil {
		return false, err
	}
	return true, nil
}

// findAgent returns the agent whose Identifier equals identifier exactly.
// Exact and case-sensitive on purpose: the identifier is upstream's unique key
// and lab derives it deterministically (see AgentIdentifier), so anything
// looser could only ever match an agent that is not this repo's. Ensure and
// delete both resolve through here, which is what keeps the match rule in one
// place — a delete matching differently than an ensure would delete the wrong
// identity.
func findAgent(agents []Agent, identifier string) (Agent, bool) {
	for _, a := range agents {
		if a.Identifier == identifier {
			return a, true
		}
	}
	return Agent{}, false
}
