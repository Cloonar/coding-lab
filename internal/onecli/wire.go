package onecli

// wire.go is the ONE file that carries OneCLI's URL paths and JSON shapes
// (issue #23): every path and every request/response shape the package uses
// lives here, and the operation files (agents.go, grants.go, pool.go) contain
// only lab-side semantics — never a literal path or a struct tag. The split
// exists because upstream documents its REST SURFACE (https://onecli.sh/docs)
// but not its JSON, so a mismatch against a real build must be a one-file fix.
//
// It earned its keep: the original file was a good-faith reading written
// without a live instance, and the first live spawn falsified two of its
// guesses (the create body lacked the required "identifier"; the token
// endpoint was guessed as POST /agents/{id}/token, which does not exist).
// What follows is no longer a guess — every shape below was read off the
// OneCLI 1.45.0 SOURCE (packages/api/src/routes/*.ts and the services behind
// them), the release the sidecar deployment pins (≥ 1.44 per ADR-0067's
// gateway epics). A future OneCLI bump that changes a shape is corrected
// HERE, and the operation files follow.
//
// A consequence worth stating, because a linter argues with it: several wire
// structs are field-identical to the exported type they map to, so
// staticcheck's S1016 proposes replacing the field-by-field copies in
// agents.go / pool.go with struct conversions. Those copies carry a
// //nolint:staticcheck for this reason. A conversion compiles only while the
// two shapes stay identical, and the day a OneCLI bump changes the wire shape
// — the event this whole split exists to absorb — it would break with a
// compile error whose most obvious repair is to change the EXPORTED type to
// match upstream's new wire shape. That is exactly the coupling the split
// prevents: lab's public type is lab's contract, and upstream's JSON is
// upstream's. The explicit copy is the seam; keep it.
//
// # The verified surface (OneCLI 1.45.0)
//
//  1. The API root is <base>/v1 (Hono basePath) and paths hang off it:
//     /health, /agents, /agents/{id}/grants,
//     /agents/{id}/grants/{secrets|connections}/{id}, /secrets, /connections.
//  2. GET /health answers {"status":"ok","version":…,"timestamp":…}.
//  3. List endpoints answer a BARE JSON ARRAY. decodeList additionally
//     tolerates a single-key envelope ({"agents":[…]} or {"data":[…]})
//     because that is the likeliest shape for a future build to drift to, and
//     guessing wrong there would turn every list into a silent empty slice.
//  4. GET /agents rows carry {"id","name","identifier","accessToken",
//     "isDefault","secretMode","createdAt","lastSeenAt"}. The accessToken is
//     the agent's GATEWAY CREDENTIAL, stable until explicitly regenerated —
//     which is why lab reads it from the listing and never calls the
//     regenerate endpoint (see agents.go on why regenerating at spawn would
//     invalidate concurrent runs).
//  5. POST /agents takes {"name": 1–255 chars, "identifier": a slug matching
//     ^[a-z0-9][a-z0-9-]{0,49}$} — both REQUIRED (the identifier's absence is
//     the 400 the original guess shipped) — and answers 201 with
//     {"id","name","identifier","createdAt"}: NO accessToken, so a creator
//     re-lists to obtain it. A duplicate identifier answers 409.
//  6. GET /agents/{id}/grants answers {"agentId","mode","connections":[…],
//     "secrets":[…]} where a secret row is {"secretId","name","type","scope"}
//     and a connection row is {"connectionId","provider","label","scope",…}.
//     Note: rows name their id by KIND (secretId/connectionId), never "id",
//     and a connection's display name is its "label".
//  7. PUT /agents/{id}/grants/secrets/{id} takes NO body. PUT …/connections/
//     {id} REQUIRES a body — {"access":"full"} is the whole-app attach; the
//     "custom" per-tool arm is dashboard territory lab does not drive. Both
//     answer 200 with the agent's grants; DELETE takes no body and answers
//     204. All are idempotent.
//  8. GET /secrets rows carry {"id","name","type",…} where "type" is the
//     provider enum ("anthropic"|"openai"|"generic") — upstream has no
//     "provider" field on secrets. GET /connections rows carry
//     {"id","provider","label",…} — no "name"; "label" is the display name
//     and may be null.
//  9. Errors are {"error":{"message":…,"type":…}} from the global handler,
//     or {"error":"…"} from route-level validation; onecli.go's errorMessage
//     accepts both. A CONFLICT is HTTP 409.
//
// POST /agents/{id}/regenerate-token exists upstream ({"accessToken":…}) but
// is deliberately NOT bound here: lab reads the stable token from the listing
// (point 4), and a regenerate call is a footgun that would invalidate the
// token every already-running run of that repo holds.

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
// identifier slug OneCLI's validation requires (point 5 above). Nothing else
// is sent; any field OneCLI defaults is left to OneCLI.
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

// wireConnectionGrantAttach is the PUT …/grants/connections/{id} body (point
// 7): "full" is the whole-app attach. The "custom" arm — per-tool allow/ask
// lists — is OneCLI-dashboard territory; lab's grant model is binary
// (attached or not), so this is the only access value it ever sends.
type wireConnectionGrantAttach struct {
	Access string `json:"access"`
}

// grantAccessFull is the one access value lab sends (see above).
const grantAccessFull = "full"

// --- response shapes -------------------------------------------------------

type wireHealth struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// wireAgent is a GET /agents row (point 4). AccessToken is the agent's
// gateway credential — a SECRET the moment it is decoded; agents.go carries
// it into Agent.Token and the hygiene rules there apply. The create answer
// (point 5) is a subset of this shape without the token, which is why a
// creator resolves the full row by re-listing.
type wireAgent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	AccessToken string `json:"accessToken"`
}

// wireGrantSecret is a secret row of the grants answer (point 6): the id is
// named "secretId", never "id".
type wireGrantSecret struct {
	SecretID string `json:"secretId"`
	Name     string `json:"name"`
}

// wireGrantConnection is a connection row of the grants answer (point 6): the
// id is named "connectionId", and the display name is "label" (nullable —
// pool.go's display fallback to the provider slug applies here too).
type wireGrantConnection struct {
	ConnectionID string `json:"connectionId"`
	Provider     string `json:"provider"`
	Label        string `json:"label"`
}

// wireSecret is a GET /secrets row (point 8): "type" is the provider enum;
// there is no "provider" field on secrets.
type wireSecret struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// wireConnection is a GET /connections row (point 8): no "name" — "label" is
// the display name and may be null.
type wireConnection struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
}

// --- decoding --------------------------------------------------------------

// decodeList decodes a list endpoint's body into rows. The verified shape is
// a bare JSON array (point 3); the single-key envelope is tolerated as the
// likeliest drift for a future build, because guessing wrong there would turn
// every list into an empty slice — a silent wrong answer rather than a loud
// one. An empty body (a 204 where a list was expected) is an empty list.
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
	return nil, fmt.Errorf("onecli: the %s list is neither a JSON array nor an object carrying one under %q, \"data\", \"items\" or \"results\"; this OneCLI build's wire shape differs from the one verified in internal/onecli/wire.go", resource, resource)
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

// decodeGrants turns a grants answer (point 6) into lab's flat Grant list,
// secrets first, then connections. An agent with no grants is an empty slice
// and no error — the normal state of a freshly created per-repo agent.
//
// The two array keys are decoded as POINTERS so that "both absent" is
// distinguishable from "both empty": an object carrying neither is a shape
// this package does not know, and it must be a loud error rather than an
// empty grants list an operator would read as "this repo has no secrets".
func decodeGrants(body []byte) ([]Grant, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return []Grant{}, nil
	}
	var wire struct {
		Secrets     *[]wireGrantSecret     `json:"secrets"`
		Connections *[]wireGrantConnection `json:"connections"`
	}
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return nil, fmt.Errorf("onecli: decoding the grants answer: %w (see internal/onecli/wire.go)", err)
	}
	if wire.Secrets == nil && wire.Connections == nil {
		return nil, errors.New(`onecli: the grants answer carries neither "secrets" nor "connections"; this OneCLI build's wire shape differs from the one verified in internal/onecli/wire.go`)
	}
	var secrets []wireGrantSecret
	if wire.Secrets != nil {
		secrets = *wire.Secrets
	}
	var connections []wireGrantConnection
	if wire.Connections != nil {
		connections = *wire.Connections
	}
	out := make([]Grant, 0, len(secrets)+len(connections))
	for _, s := range secrets {
		out = append(out, Grant{Kind: GrantSecret, ID: s.SecretID, Name: s.Name})
	}
	for _, c := range connections {
		out = append(out, Grant{Kind: GrantConnection, ID: c.ConnectionID, Name: displayName(c.Label, c.Provider)})
	}
	return out, nil
}

// displayName picks a connection's human name: the label when upstream has
// one (it is nullable), else the provider slug — a grant or pool row must
// never render nameless in an inventory an operator reads.
func displayName(label, provider string) string {
	if label != "" {
		return label
	}
	return provider
}
