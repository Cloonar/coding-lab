package onecli

// Grants: which of the project's credentials a given agent may actually use.
// In lab's model (issue #23) an agent is a repo, so "the repo's secrets" is
// literally "the grants on that repo's agent" — this file is the read/write
// surface the per-repo grant picker (#25) is built on.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// GrantKind names the two kinds of credential a grant can point at. The values
// are the PLURAL forms because they double as the path segment in
// /agents/{id}/grants/{kind}/{resourceId} — keeping them identical means there
// is no mapping table between the Go value and the URL to drift.
type GrantKind string

const (
	GrantSecret     GrantKind = "secrets"
	GrantConnection GrantKind = "connections"
)

// Grant is one credential an agent may use, named by kind and id. Name is
// carried for display only; every write addresses the resource by ID.
type Grant struct {
	Kind GrantKind
	ID   string
	Name string
}

// validGrantKind rejects anything but the two known kinds. It runs BEFORE any
// URL is built, because GrantKind's value is a path segment: an unchecked kind
// would let a caller's string steer the request at a different endpoint.
func validGrantKind(kind GrantKind) error {
	switch kind {
	case GrantSecret, GrantConnection:
		return nil
	default:
		return fmt.Errorf("onecli: unknown grant kind %q; want %q or %q", kind, GrantSecret, GrantConnection)
	}
}

// ListGrants returns the credentials the agent may use. An agent with none —
// the normal state of a freshly created per-repo agent — is an empty slice and
// no error.
func (c *Client) ListGrants(ctx context.Context, agentID string) ([]Grant, error) {
	if agentID == "" {
		return nil, errors.New("onecli: agent id must not be empty")
	}
	body, err := c.do(ctx, http.MethodGet, c.agentGrantsURL(agentID), nil)
	if err != nil {
		return nil, err
	}
	return decodeGrants(body)
}

// AttachGrant gives the agent access to the resource. The resource is
// addressed by the URL; a secret attach sends no body, a connection attach
// sends the whole-app {"access":"full"} body OneCLI's validation requires
// (wire.go point 7 — lab's grant model is binary, so the per-tool "custom"
// arm is never sent). A 200 and a 204 are both success (see do). PUT is
// idempotent by construction — re-granting something already granted is a
// no-op, which is what makes this safe to call from a picker that just
// replays an operator's whole selection.
func (c *Client) AttachGrant(ctx context.Context, agentID string, kind GrantKind, resourceID string) error {
	return c.writeGrant(ctx, http.MethodPut, agentID, kind, resourceID)
}

// DetachGrant revokes the agent's access to the resource. Addressed entirely
// by URL for both kinds (no body) and tolerates 200 or 204.
func (c *Client) DetachGrant(ctx context.Context, agentID string, kind GrantKind, resourceID string) error {
	return c.writeGrant(ctx, http.MethodDelete, agentID, kind, resourceID)
}

// writeGrant is the shared attach/detach path: validate, then one request
// whose method — and, for a connection attach, the required access body — is
// the only difference between the two verbs.
func (c *Client) writeGrant(ctx context.Context, method, agentID string, kind GrantKind, resourceID string) error {
	if agentID == "" {
		return errors.New("onecli: agent id must not be empty")
	}
	if resourceID == "" {
		return errors.New("onecli: resource id must not be empty")
	}
	if err := validGrantKind(kind); err != nil {
		return err
	}
	var body any
	if method == http.MethodPut && kind == GrantConnection {
		body = wireConnectionGrantAttach{Access: grantAccessFull}
	}
	_, err := c.do(ctx, method, c.agentGrantURL(agentID, kind, resourceID), body)
	return err
}
