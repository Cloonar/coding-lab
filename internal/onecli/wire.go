package onecli

// wire.go is the ONE file to correct when this client meets a real OneCLI
// build and disagrees with it (issue #23). Every URL path and every
// request/response JSON shape the package uses lives here; the operation files
// (agents.go, grants.go, pool.go) contain only lab-side semantics and never a
// literal path or a struct tag. That split is the whole point: the REST
// SURFACE is documented (https://onecli.sh/docs, https://onecli.sh/docs/llms.txt)
// but the FIELD NAMES are not, and this package was written without a live
// instance to read them off. What follows is a good-faith reading, not a
// verified contract.
//
// A consequence worth stating, because a linter argues with it: several wire
// structs are currently field-identical to the exported type they map to, so
// staticcheck's S1016 proposes replacing the field-by-field copies in
// agents.go / pool.go with struct conversions. Those copies carry a
// //nolint:staticcheck for this reason. A conversion compiles only while the
// two shapes stay identical, and the day this file is corrected against a real
// OneCLI build — the event this whole split exists to absorb — it would break
// with a compile error whose most obvious repair is to change the EXPORTED
// type to match upstream's new wire shape. That is exactly the coupling the
// split prevents: lab's public type is lab's contract, and upstream's JSON is
// upstream's. The explicit copy is the seam; keep it.
//
// # The assumptions, stated plainly
//
//  1. The API root is <base>/v1 and paths hang off it as documented:
//     /health, /agents, /agents/{id}, /agents/{id}/token,
//     /agents/{id}/grants, /agents/{id}/grants/{secrets|connections}/{id},
//     /secrets, /connections. This is the documented part; the rest is not.
//  2. Objects identify themselves with "id" and "name"; secrets and
//     connections also carry "provider". Go's JSON decoder matches field names
//     case-insensitively, so "ID"/"Id"/"id" all land — but underscores are
//     significant, which is why the one multi-word field below is declared
//     twice, in both snake_case and camelCase. That is the rule this file
//     follows: single-word fields need no alias, multi-word fields get both
//     spellings.
//  3. List endpoints answer either with a bare JSON array or with a
//     single-key envelope ({"agents": […]}, or a generic {"data": […]}).
//     decodeList accepts both because this is the likeliest place a real build
//     differs, and guessing wrong here would turn every list into an empty
//     slice — a silent wrong answer rather than a loud one.
//  4. POST /agents takes {"name": "<name>", "identifier": "<slug>"} and
//     answers with the created agent object. This one is VERIFIED against a
//     real build, the hard way: the original reading ({"name"} alone) was
//     rejected by OneCLI's request validation, which requires a second field,
//     "identifier" — a slug matching ^[a-z0-9][a-z0-9-]{0,49}$. Lab still
//     treats the NAME as the agent's identity (listing answers carry both, and
//     every match in agents.go is by exact name); the identifier is derived
//     from the name by agentIdentifier below, deterministically, so the same
//     repo always sends the same slug and a duplicate create still 409s.
//  5. GET /agents/{id}/grants answers either with {"secrets": […],
//     "connections": […]} or with a flat list whose rows name their own kind
//     under "kind" or "type"; decodeGrants accepts both.
//  6. POST /agents/{id}/token answers {"token": "…"} with an optional RFC3339
//     "expires_at"/"expiresAt". An absent or empty expiry means "no expiry
//     reported", not "expired".
//  7. PUT/DELETE on a grant path take NO request body — the grant is fully
//     addressed by the URL — and answer 200 or 204.
//
// If a real build differs, correct it here and the rest of the package
// follows. Do not spread wire knowledge back out into the operation files.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// apiVersionSegment is the API root every path hangs off; apiRoot appends it
// to a configured base URL that does not already end in it.
const apiVersionSegment = "v1"

// The path segments. They are constants rather than inline literals so that a
// rename upstream is a single edit here and so that GrantKind's values can be
// checked against the two segment names they double as.
const (
	segHealth      = "health"
	segAgents      = "agents"
	segSecrets     = "secrets"
	segConnections = "connections"
	segGrants      = "grants"
	segToken       = "token"
)

// --- path builders ---------------------------------------------------------
//
// All of them build on c.base, which apiRoot already normalized to a clean
// …/v1 with no trailing slash, so joining can neither double nor drop a
// separator. Caller-supplied identifiers are PathEscape'd: an id containing a
// slash must address one (escaped) path element, never traverse into a
// different endpoint.

func (c *Client) healthURL() *url.URL { return c.base.JoinPath(segHealth) }

func (c *Client) agentsURL() *url.URL { return c.base.JoinPath(segAgents) }

func (c *Client) agentTokenURL(agentID string) *url.URL {
	return c.base.JoinPath(segAgents, url.PathEscape(agentID), segToken)
}

func (c *Client) agentGrantsURL(agentID string) *url.URL {
	return c.base.JoinPath(segAgents, url.PathEscape(agentID), segGrants)
}

// agentGrantURL addresses ONE grant: the resource of the given kind attached
// to the given agent. kind is validated by the caller (validGrantKind) before
// it reaches here — GrantKind's values double as the path segment, so an
// unvalidated kind would be a caller-controlled path element.
func (c *Client) agentGrantURL(agentID string, kind GrantKind, resourceID string) *url.URL {
	return c.base.JoinPath(segAgents, url.PathEscape(agentID), segGrants, string(kind), url.PathEscape(resourceID))
}

func (c *Client) secretsURL() *url.URL { return c.base.JoinPath(segSecrets) }

func (c *Client) connectionsURL() *url.URL { return c.base.JoinPath(segConnections) }

// --- request shapes --------------------------------------------------------

// wireCreateAgent is the POST /agents body: the name lab identifies the agent
// by (the per-repo agent identity of issue #23 is exactly a name) plus the
// identifier slug OneCLI's validation requires (assumption 4 above). Nothing
// else is sent; any field OneCLI defaults is left to OneCLI.
type wireCreateAgent struct {
	Name       string `json:"name"`
	Identifier string `json:"identifier"`
}

// newWireCreateAgent builds the create body for a name, deriving the
// identifier so no caller outside this file has to know the slug rule exists.
func newWireCreateAgent(name string) wireCreateAgent {
	return wireCreateAgent{Name: name, Identifier: agentIdentifier(name)}
}

// agentIdentifier derives OneCLI's required identifier slug
// (^[a-z0-9][a-z0-9-]{0,49}$) from an agent name, deterministically: lowercase
// the name, map every byte outside [a-z0-9-] to "-", strip the hyphens that
// mapping may have put in front (a slug must start alphanumeric), and cap the
// result at 50 characters. Determinism is load-bearing — EnsureAgent's
// 409-race resolution assumes the same name always produces the same create
// body, so "identifier already taken" can only ever mean "this agent already
// exists", never a collision between two different repos: lab's names are
// repo_<32 hex>, whose derivations (repo-<32 hex>, 37 chars) differ wherever
// the names do.
func agentIdentifier(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	slug := strings.TrimLeft(b.String(), "-")
	if len(slug) > 50 {
		slug = slug[:50]
	}
	return slug
}

// --- response shapes -------------------------------------------------------

type wireHealth struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

type wireAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// wireResource is a secret or a connection in the project pool, and also a row
// of the split-shape grants answer. The two resource kinds are structurally
// identical on the wire as far as lab reads them.
type wireResource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// wireGrantRow is a row of the FLAT grants shape, where each row names its own
// kind. Kind and Type are two spellings of one concept; grantKindOf prefers
// Kind. Both singular and plural values are accepted (see grantKindOf).
type wireGrantRow struct {
	Kind string `json:"kind"`
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// wireGrantList is the SPLIT grants shape — the two pools listed separately —
// with the flat shape's envelope keys folded in so one decode covers both.
type wireGrantList struct {
	Secrets     []wireResource `json:"secrets"`
	Connections []wireResource `json:"connections"`
	Grants      []wireGrantRow `json:"grants"`
	Data        []wireGrantRow `json:"data"`
}

// wireToken is the POST /agents/{id}/token answer. ExpiresAt is declared twice
// because it is the package's only multi-word field and Go's decoder does not
// bridge snake_case and camelCase; exactly one of the two is populated by any
// given build, and expiresAt() picks whichever came back.
type wireToken struct {
	Token          string `json:"token"`
	ExpiresAtSnake string `json:"expires_at"`
	ExpiresAtCamel string `json:"expiresAt"`
}

func (t wireToken) expiresAt() string {
	if t.ExpiresAtSnake != "" {
		return t.ExpiresAtSnake
	}
	return t.ExpiresAtCamel
}

// --- decoding --------------------------------------------------------------

// decodeList decodes a list endpoint's body into rows, accepting both shapes
// assumption 3 above allows: a bare JSON array, or an object carrying the
// array under the resource's own key or a generic data/items/results key. An
// empty body (a 204 where a list was expected) is an empty list, not an error.
//
// A body that is neither is a LOUD error naming this file: silently returning
// an empty slice would make "OneCLI answers a shape we did not expect" look
// exactly like "the project has no secrets", and the grant picker built on top
// of this (#25) would show an empty pool with no explanation.
func decodeList[T any](body []byte, resource string) ([]T, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return []T{}, nil
	}
	if trimmed[0] == '[' {
		var rows []T
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("onecli: decoding the %s list: %w", resource, err)
		}
		return rows, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, fmt.Errorf("onecli: decoding the %s list: %w", resource, err)
	}
	for _, key := range []string{resource, "data", "items", "results"} {
		raw, ok := envelope[key]
		if !ok {
			continue
		}
		var rows []T
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("onecli: decoding the %s list under %q: %w", resource, key, err)
		}
		return rows, nil
	}
	return nil, fmt.Errorf("onecli: the %s list is neither a JSON array nor an object carrying one under %q, \"data\", \"items\" or \"results\"; this OneCLI build's wire shape differs from the one assumed in internal/onecli/wire.go", resource, resource)
}

// decodeOne decodes a single-object body, wrapping the failure with the
// resource name and the pointer to this file that every wire mismatch gets.
func decodeOne[T any](body []byte, resource string) (T, error) {
	var out T
	if len(bytes.TrimSpace(body)) == 0 {
		return out, fmt.Errorf("onecli: the %s answer was empty; expected a JSON object (see internal/onecli/wire.go)", resource)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("onecli: decoding the %s answer: %w (see internal/onecli/wire.go)", resource, err)
	}
	return out, nil
}

// decodeGrants turns a grants body into lab's flat Grant list, accepting both
// shapes assumption 5 allows. The split shape's two arrays are emitted secrets
// first, then connections; a flat shape keeps its own order. An agent with no
// grants is an empty slice and no error — the normal state of a freshly
// created per-repo agent.
func decodeGrants(body []byte) ([]Grant, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return []Grant{}, nil
	}
	if trimmed[0] == '[' {
		var rows []wireGrantRow
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("onecli: decoding the grants list: %w", err)
		}
		return grantsFromRows(rows)
	}
	var list wireGrantList
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return nil, fmt.Errorf("onecli: decoding the grants list: %w", err)
	}
	out := make([]Grant, 0, len(list.Secrets)+len(list.Connections)+len(list.Grants)+len(list.Data))
	for _, s := range list.Secrets {
		out = append(out, Grant{Kind: GrantSecret, ID: s.ID, Name: s.Name})
	}
	for _, c := range list.Connections {
		out = append(out, Grant{Kind: GrantConnection, ID: c.ID, Name: c.Name})
	}
	rows := list.Grants
	if rows == nil {
		rows = list.Data
	}
	flat, err := grantsFromRows(rows)
	if err != nil {
		return nil, err
	}
	return append(out, flat...), nil
}

// grantsFromRows maps flat grant rows onto Grant, resolving each row's kind.
func grantsFromRows(rows []wireGrantRow) ([]Grant, error) {
	out := make([]Grant, 0, len(rows))
	for _, r := range rows {
		kind, err := grantKindOf(r)
		if err != nil {
			return nil, err
		}
		out = append(out, Grant{Kind: kind, ID: r.ID, Name: r.Name})
	}
	return out, nil
}

// grantKindOf resolves a flat grant row's kind, preferring "kind" over "type"
// and accepting the singular as well as the plural (the plural is what the
// path segments use, so it is what GrantKind's values are). An unrecognized or
// absent kind is an error naming the value and this file — a row whose kind
// lab cannot read must not be silently dropped from a grants list an operator
// is about to make decisions from.
func grantKindOf(r wireGrantRow) (GrantKind, error) {
	raw := r.Kind
	if raw == "" {
		raw = r.Type
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "secret", string(GrantSecret):
		return GrantSecret, nil
	case "connection", string(GrantConnection):
		return GrantConnection, nil
	case "":
		return "", errors.New(`onecli: a grant row carries neither "kind" nor "type"; this OneCLI build's grants shape differs from the one assumed in internal/onecli/wire.go`)
	default:
		return "", fmt.Errorf("onecli: unknown grant kind %q; expected %q or %q (see internal/onecli/wire.go)", raw, GrantSecret, GrantConnection)
	}
}
