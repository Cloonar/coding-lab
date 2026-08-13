package onecli

// The project's credential pool: everything that COULD be granted, as opposed
// to grants.go's "what this agent may actually use". Lab reads these to render
// the per-repo grant picker (#25) — the operator picks from the pool, and the
// pick becomes a grant on the repo's agent.
//
// Neither call ever sees a credential VALUE. That is the point of the gateway
// model in issue #23: lab learns that a secret named DEPLOY_TOKEN exists and
// can be granted, and nothing more; the value stays inside OneCLI and is
// injected by the proxy at request time.

import (
	"context"
	"net/http"
)

// Secret is one secret in the project pool — metadata only, never a value.
// Provider is the provider enum the secret is scoped to ("anthropic",
// "openai", "generic"); upstream calls this field "type" on the wire, but
// provider is what it means, and it matches Connection.Provider's role.
type Secret struct {
	ID       string
	Name     string
	Provider string
}

// Connection is one provider connection in the project pool (an OAuth-style
// link to a third party, as opposed to a raw secret) — again metadata only.
// Name is upstream's "label" (the operator-facing display name), falling back
// to the provider slug when the label is unset (wire.go point 8).
type Connection struct {
	ID       string
	Name     string
	Provider string
}

// ListSecrets lists the project's secrets.
func (c *Client) ListSecrets(ctx context.Context) ([]Secret, error) {
	body, err := c.do(ctx, http.MethodGet, c.secretsURL(), nil)
	if err != nil {
		return nil, err
	}
	rows, err := decodeList[wireSecret](body, segSecrets)
	if err != nil {
		return nil, err
	}
	out := make([]Secret, 0, len(rows))
	for _, r := range rows {
		out = append(out, Secret{ID: r.ID, Name: r.Name, Provider: r.Type})
	}
	return out, nil
}

// ListConnections lists the project's connections. The documented ?provider=
// filter is deliberately not exposed: lab's picker shows the whole pool, and
// an unused knob on an unverified wire contract is one more thing to keep
// true (issue #23's "only what the later epics need" scope rule).
func (c *Client) ListConnections(ctx context.Context) ([]Connection, error) {
	body, err := c.do(ctx, http.MethodGet, c.connectionsURL(), nil)
	if err != nil {
		return nil, err
	}
	rows, err := decodeList[wireConnection](body, segConnections)
	if err != nil {
		return nil, err
	}
	out := make([]Connection, 0, len(rows))
	for _, r := range rows {
		out = append(out, Connection{ID: r.ID, Name: displayName(r.Label, r.Provider), Provider: r.Provider})
	}
	return out, nil
}
