package warpgate

// wire.go is the ONE file that carries Warpgate's URL paths and JSON shapes
// (issue #39): every path and every request/response field name the package
// uses lives here, and the operation files (identity.go, targets.go, keys.go,
// health.go) contain only lab-side semantics — never a literal path or a
// struct tag. It is the single point of correction: a Warpgate release that
// moves a path or renames a field is fixed HERE, and the operation files
// follow. That is the same split internal/onecli/wire.go makes, for the same
// reason, and it carries the same consequence: wire structs are mapped to the
// exported types field by field, never by struct conversion, because lab's
// public types are lab's contract and upstream's JSON is upstream's.
//
// Nothing here was run against a live Warpgate. Every fact below was READ OFF
// THE WARPGATE 0.29.1 SOURCE (tag v0.29.1, commit 54f93c8) — file:line
// references are into that tree — and the stubs in the tests are
// transcriptions of what those handlers answer.
//
// # The verified surface (Warpgate 0.29.1)
//
//  1. Mounts. Both /@warpgate and its alias /_warpgate carry the web app
//     (warpgate-protocol-http/src/lib.rs:297-298); under it, /api is the
//     gateway API (lib.rs:217) and /admin/api the admin API (lib.rs:224,
//     warpgate-admin/src/lib.rs:15). Lab uses <base>/@warpgate/admin/api for
//     everything and <base>/@warpgate/api/info for health. Role detail paths
//     are SINGULAR (/role/{id}); only list and create use /roles.
//  2. Auth. inject_request_authorization wraps the whole app (lib.rs:303) and
//     reads X-Warpgate-Token (warpgate-protocol-http/src/common.rs:470-495):
//     a constant-time match against the static admin token (warpgate run
//     --enable-admin-token + WARPGATE_ADMIN_TOKEN) yields AdminToken, which
//     holds EVERY admin permission (warpgate-admin/src/api/common.rs:24-26);
//     otherwise a per-user API token yields UserToken with its user's admin
//     roles' permissions; otherwise the request is anonymous. The admin API
//     answers 401 (empty body) to an anonymous request (common.rs:155-161)
//     AND to a user token whose user has no admin permission at all
//     (admin_scheme.rs:40-48); a missing specific permission is 403 "admin
//     permission required: UsersCreate" (warpgate-common/src/error.rs:75,198).
//  3. GET /@warpgate/api/info (warpgate-protocol-http/src/api/info.rs:163)
//     answers 200 to EVERYONE; the token only changes the body. "version" is
//     non-null exactly when the request authenticated (info.rs:307), and
//     "admin_permissions" is non-null exactly then too, carrying the resolved
//     permission set as snake_case booleans (info.rs:286-291, 59-85) — all
//     true for the static admin token. A rejected token is simply anonymous:
//     200 with both null. So health must read the BODY, never the status.
//  4. Users (warpgate-admin/src/api/users.rs). GET /users?search= (62) is a
//     case-insensitive substring LIKE (warpgate-admin/src/api/common.rs:
//     80-101) → lab matches the exact username client-side. POST /users {username, description?} (86)
//     answers 201 with the User; a duplicate — compared case-insensitively
//     (173-182) — is 400 with the JSON STRING "username" (106), an empty
//     username 400 "name" (94). GET/PUT/DELETE /users/{id} (225/240/294):
//     200/200/204, 404 when absent. PUT REPLACES THE WHOLE OBJECT: omitted
//     description resets to "", omitted credential_policy /
//     rate_limit_bytes_per_second / allowed_ip_ranges reset to null (272-281)
//     — so the heal is GET-then-full-PUT of the object read (see
//     userUpdateBody). A User is {id, username, description,
//     credential_policy, rate_limit_bytes_per_second, ldap_server_id,
//     allowed_ip_ranges} (warpgate-common/src/config/mod.rs:154-162), every
//     field present (poem serializes None as null). Unknown request fields
//     are ignored (poem-openapi objects do not deny them), which is what
//     makes echoing the read object back safe.
//  5. User roles (users.rs). GET /users/{id}/roles (579) lists EVERY
//     assignment row — revoked and expired included — as {id (the ROLE id),
//     name, description, granted_at, expires_at, is_expired, is_active}, so
//     lab filters on is_active. POST /users/{id}/roles/{rid} (645) takes an
//     OPTIONAL JSON body (650) that still requires Content-Type:
//     application/json (poem-openapi 415s without it), so lab sends {}; it
//     answers 201 whether the grant is new, re-activated, or already active
//     (UserRoleAssignment.rs:69-70 returns the active row unchanged), 404 for
//     an unknown user and — the trap — 409 for an unknown ROLE (662), so a
//     409 here never means "already granted" and is surfaced as an error.
//  6. Roles (warpgate-admin/src/api/roles.rs). GET /roles?search= (43) is the
//     same LIKE; POST /roles {name, description?, is_default?} (65) → 201.
//     There is NO uniqueness check and no unique index on role names:
//     duplicates are possible, and lab picks the lexicographically smallest
//     id deterministically. PUT /role/{id} (157) takes the create body;
//     omitting is_default keeps the current value (175), so lab always sends
//     false explicitly — a default role is auto-granted to every NEW user
//     (users.rs:126), which for a lab repo role would hand that repo's
//     targets to every other repo's user. DELETE /role/{id} (182) → 204, 404
//     when absent, and removes the role's user and target assignments.
//  7. Targets (warpgate-admin/src/api/targets.rs). GET /targets (72) and GET
//     /role/{id}/targets (roles.rs:215) answer full Target objects
//     (config/target.rs:360-376): {id, name, description, allow_roles (always
//     []), options, …}. "options" is a poem-openapi Union discriminated by
//     "kind" whose values are the Rust VARIANT names — "Ssh", "Http",
//     "Kubernetes", "MySql", "Postgres", "Vnc", "Rdp" (target.rs:379-395) —
//     not serde's lowercase config names. options CARRIES CREDENTIALS
//     (SECRET_PATHS, target.rs:430-438: SSH passwords, k8s tokens, private
//     keys), so wireTarget declares ONLY id/name/description/options.kind.
//  8. Target↔role (targets.rs). POST /targets/{id}/roles/{rid} (437) takes no
//     body: 201, or 409 with no body when already assigned (455) — success
//     for lab. DELETE (473) is a hard delete: 204, or 404 when not assigned
//     (490) — also success for lab, which makes unassign idempotent.
//  9. Public keys (warpgate-admin/src/api/public_key_credentials.rs). GET
//     /users/{id}/credentials/public-keys (113) → [{id, label, date_added,
//     last_used, openssh_public_key}] ([] for an unknown user). POST (137)
//     {label, openssh_public_key} → 201 with the row; 404 unknown user; 403
//     for an LDAP-linked user; a key russh cannot parse is a 500 — lab
//     validates locally first. The key COMMENT IS STRIPPED on store (70), so
//     the only lab-owned metadata that survives is "label" (keys.go). DELETE
//     …/{cid} (255) → 204, 404 when absent (idempotent for lab).
// 10. Error bodies. There is no JSON error envelope. A declared 4xx answers a
//     bare JSON string (often a field name: invalid_field,
//     warpgate-common-http/src/errors.rs:21-24) or nothing; an internal error
//     answers text/plain "<reason> (reference: <uuid>)" (errors.rs:43); a
//     body that fails to parse answers poem's plain-text reason. Accept:
//     application/json matters — a text/html Accept gets HTML pages.
// 11. No pagination on any endpoint above; lists are complete.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// tokenHeader is the one auth header Warpgate reads (point 2). There is no
// Bearer scheme.
const tokenHeader = "X-Warpgate-Token"

// Path segments (point 1). Constants rather than inline literals so an
// upstream rename is a single edit here.
const (
	segWarpgate    = "@warpgate"
	segAPI         = "api"
	segAdmin       = "admin"
	segInfo        = "info"
	segUsers       = "users"
	segRoles       = "roles"
	segRole        = "role"
	segTargets     = "targets"
	segCredentials = "credentials"
	segPublicKeys  = "public-keys"
	querySearch    = "search"
)

// pastedAPIPaths are the Warpgate URL suffixes an operator plausibly pastes
// into --warpgate-url (copied from the browser or the API playground), longest
// first so the admin root is stripped whole rather than leaving /admin behind.
// Both mounts are listed because both serve the same app (point 1).
var pastedAPIPaths = []string{
	"/@warpgate/admin/api", "/_warpgate/admin/api",
	"/@warpgate/api", "/_warpgate/api",
	"/@warpgate", "/_warpgate",
}

// stripAPIPath removes one pasted Warpgate API suffix from an already
// slash-normalized path, keeping any reverse-proxy prefix in front of it. The
// result is "/" or "/prefix" — never empty, for the JoinPath reason in
// normalizeBase.
func stripAPIPath(path string) string {
	for _, suffix := range pastedAPIPaths {
		if trimmed, ok := strings.CutSuffix(path, suffix); ok {
			if trimmed == "" {
				return "/"
			}
			return trimmed
		}
	}
	return path
}

// --- URL builders ----------------------------------------------------------
//
// All of them build on roots normalizeBase already cleaned, so joining can
// neither double nor drop a separator. Caller-supplied ids go through escID:
// an id containing a slash must address one (escaped) path element, never
// traverse into a different endpoint.

func adminRootURL(base *url.URL) *url.URL { return base.JoinPath(segWarpgate, segAdmin, segAPI) }

func infoURL(base *url.URL) *url.URL { return base.JoinPath(segWarpgate, segAPI, segInfo) }

// withSearch adds Warpgate's ?search= filter. It is a LIKE (points 4 and 6),
// a narrowing hint only — every caller still matches exactly.
func withSearch(u *url.URL, term string) *url.URL {
	u.RawQuery = url.Values{querySearch: []string{term}}.Encode()
	return u
}

func (c *Client) usersURL(search string) *url.URL {
	return withSearch(c.adminRoot.JoinPath(segUsers), search)
}

func (c *Client) usersCreateURL() *url.URL { return c.adminRoot.JoinPath(segUsers) }

func (c *Client) userURL(userID string) *url.URL {
	return c.adminRoot.JoinPath(segUsers, escID(userID))
}

func (c *Client) userRolesURL(userID string) *url.URL {
	return c.adminRoot.JoinPath(segUsers, escID(userID), segRoles)
}

func (c *Client) userRoleURL(userID, roleID string) *url.URL {
	return c.adminRoot.JoinPath(segUsers, escID(userID), segRoles, escID(roleID))
}

func (c *Client) rolesURL(search string) *url.URL {
	return withSearch(c.adminRoot.JoinPath(segRoles), search)
}

func (c *Client) rolesCreateURL() *url.URL { return c.adminRoot.JoinPath(segRoles) }

// roleURL is SINGULAR /role/{id} (point 1) — the plural spelling is a 404.
func (c *Client) roleURL(roleID string) *url.URL {
	return c.adminRoot.JoinPath(segRole, escID(roleID))
}

func (c *Client) roleTargetsURL(roleID string) *url.URL {
	return c.adminRoot.JoinPath(segRole, escID(roleID), segTargets)
}

func (c *Client) targetsURL() *url.URL { return c.adminRoot.JoinPath(segTargets) }

func (c *Client) targetRoleURL(targetID, roleID string) *url.URL {
	return c.adminRoot.JoinPath(segTargets, escID(targetID), segRoles, escID(roleID))
}

func (c *Client) publicKeysURL(userID string) *url.URL {
	return c.adminRoot.JoinPath(segUsers, escID(userID), segCredentials, segPublicKeys)
}

func (c *Client) publicKeyURL(userID, keyID string) *url.URL {
	return c.adminRoot.JoinPath(segUsers, escID(userID), segCredentials, segPublicKeys, escID(keyID))
}

// --- request shapes --------------------------------------------------------

// wireCreateUser is the POST /users body (point 4). Nothing else is sent:
// credential policy and the rest take Warpgate's defaults, and the default
// policy accepts a single public key for SSH.
type wireCreateUser struct {
	Username    string `json:"username"`
	Description string `json:"description"`
}

// wireRoleWrite is the POST /roles and PUT /role/{id} body (point 6).
// IsDefault is always sent, always false: omitting it on PUT would KEEP a
// default flag someone set, and a default lab role leaks targets across repos.
type wireRoleWrite struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsDefault   bool   `json:"is_default"`
}

// wireGrantRole is the POST /users/{id}/roles/{rid} body: an empty object.
// The body is optional upstream, but its Content-Type is not (point 5) — and
// do() sets Content-Type exactly when there is a body, so the empty object is
// what makes the header go out.
type wireGrantRole struct{}

// wireNewPublicKey is the POST …/credentials/public-keys body (point 9).
type wireNewPublicKey struct {
	Label            string `json:"label"`
	OpenSSHPublicKey string `json:"openssh_public_key"`
}

// Field names of the user object that lab reads or writes in the heal
// (point 4). Everything else in the object is passed through untouched.
const (
	userFieldUsername    = "username"
	userFieldDescription = "description"
)

// userUpdateBody turns a GET /users/{id} answer into the PUT /users/{id} body
// that changes ONLY the description. PUT replaces the whole user (point 4),
// so a body built from lab's own struct would reset every field lab does not
// model — credential_policy, rate_limit_bytes_per_second, allowed_ip_ranges
// today, whatever a later Warpgate adds tomorrow. Instead the object read is
// echoed back as raw JSON, key for key, with only "description" replaced:
// fields lab does not own round-trip byte-for-byte, and read-only fields (id,
// ldap_server_id) are ignored by upstream's parser.
//
// It also returns the username read, which the caller checks against the
// user it meant to heal — a GET that answered some other shape must not turn
// into a PUT that renames a user.
func userUpdateBody(raw []byte, description string) (map[string]json.RawMessage, string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, "", fmt.Errorf("warpgate: decoding the user answer: expected a JSON object (see internal/warpgate/wire.go): %v", err)
	}
	var username string
	if err := json.Unmarshal(obj[userFieldUsername], &username); err != nil || username == "" {
		return nil, "", errors.New(`warpgate: the user answer carries no "username"; this Warpgate build's wire shape differs from the one verified in internal/warpgate/wire.go`)
	}
	desc, err := json.Marshal(description)
	if err != nil {
		return nil, "", fmt.Errorf("warpgate: encoding the user description: %w", err)
	}
	obj[userFieldDescription] = desc
	return obj, username, nil
}

// --- response shapes -------------------------------------------------------

// wireUser is a GET /users row or the POST /users answer (point 4), reduced
// to what lab matches on and displays. The heal does not use it — see
// userUpdateBody.
type wireUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Description string `json:"description"`
}

// wireRole is a GET /roles row or the POST /roles answer (point 6).
type wireRole struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsDefault   bool   `json:"is_default"`
}

// wireUserRole is a GET /users/{id}/roles row (point 5). ID is the ROLE's id;
// IsActive folds revoked and expired together, which is exactly the question
// lab asks ("does this user hold this role right now?").
type wireUserRole struct {
	ID       string `json:"id"`
	IsActive bool   `json:"is_active"`
}

// wireTarget is a target row (point 7) and deliberately NOTHING MORE. The
// answer carries the target's credentials in "options"; because this struct
// has no field for them, encoding/json skips them without ever materializing
// a Go value — there is no string holding a target password anywhere in lab,
// so no log, error or %v can spill one. Do not add fields here without
// checking them against SECRET_PATHS (target.rs:430-438).
type wireTarget struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Options     struct {
		Kind string `json:"kind"`
	} `json:"options"`
}

// targetKindSSH is the discriminator value of an SSH target (point 7): the
// Rust variant name, capitalized as upstream spells it.
const targetKindSSH = "Ssh"

// wirePublicKey is a public-key credential row (point 9). date_added and
// last_used are not decoded: nothing in lab reads them.
type wirePublicKey struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	OpenSSHPublicKey string `json:"openssh_public_key"`
}

// wireInfo is the part of GET /@warpgate/api/info lab reads (point 3). Both
// are pointers because null — not the zero value — is the signal: null means
// the token did not authenticate.
type wireInfo struct {
	Version          *string               `json:"version"`
	AdminPermissions *wireAdminPermissions `json:"admin_permissions"`
}

// wireAdminPermissions is the subset of info's permission set that lab's
// operations require (see each operation's upstream handler): users
// create/edit/delete for the repo identity and its keys (key endpoints gate on
// UsersEdit, even GET), roles create/edit/delete, and access_roles_assign for
// both user↔role and target↔role. Listing users, roles and targets needs
// only "is an admin", which any of these implies.
type wireAdminPermissions struct {
	UsersCreate       bool `json:"users_create"`
	UsersEdit         bool `json:"users_edit"`
	UsersDelete       bool `json:"users_delete"`
	AccessRolesCreate bool `json:"access_roles_create"`
	AccessRolesEdit   bool `json:"access_roles_edit"`
	AccessRolesDelete bool `json:"access_roles_delete"`
	AccessRolesAssign bool `json:"access_roles_assign"`
}

// sufficient reports whether every permission lab needs is held.
func (p wireAdminPermissions) sufficient() bool {
	return p.UsersCreate && p.UsersEdit && p.UsersDelete &&
		p.AccessRolesCreate && p.AccessRolesEdit && p.AccessRolesDelete && p.AccessRolesAssign
}

// --- decoding --------------------------------------------------------------

// decodeList decodes a list endpoint's bare-array body (every list above is
// one; point 11). An empty body is an empty list. Anything else is a LOUD
// error naming this file — an unexpected shape must never read as "Warpgate
// has no users/roles/targets", because the ensure logic would then create
// duplicates and the picker would show an empty inventory with no reason.
//
// secrecy governs the error text: for a secret-bearing list the decoder's own
// message is withheld (sanitizeDecodeErr), because a json error can quote a
// fragment of the input.
func decodeList[T any](body []byte, resource string, secrecy bodySecrecy) ([]T, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return []T{}, nil
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("warpgate: the %s answer is not a JSON array; this Warpgate build's wire shape differs from the one verified in internal/warpgate/wire.go", resource)
	}
	var rows []T
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, fmt.Errorf("warpgate: decoding the %s list: %w (see internal/warpgate/wire.go)", resource, sanitizeDecodeErr(err, secrecy))
	}
	return rows, nil
}

// decodeOne decodes a single-object body.
func decodeOne[T any](body []byte, resource string) (T, error) {
	var out T
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return out, fmt.Errorf("warpgate: the %s answer is not a JSON object; this Warpgate build's wire shape differs from the one verified in internal/warpgate/wire.go", resource)
	}
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return out, fmt.Errorf("warpgate: decoding the %s answer: %w (see internal/warpgate/wire.go)", resource, err)
	}
	return out, nil
}

// sanitizeDecodeErr reduces a json error to its position and type facts for
// a secret-bearing body. json.SyntaxError can quote an input character and
// json.UnmarshalTypeError a numeric literal; neither may leave this package
// for a body that carries target credentials. What survives is enough to
// debug a shape drift (which field, which type, which offset) and nothing of
// the data.
func sanitizeDecodeErr(err error, secrecy bodySecrecy) error {
	if secrecy == plainBody {
		return err
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Errorf("malformed JSON at byte offset %d (details withheld: the body may carry target credentials)", syntaxErr.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Errorf("field %q has an unexpected JSON type, want %s (value withheld: the body may carry target credentials)", typeErr.Field, typeErr.Type)
	}
	return errors.New("undecodable JSON (details withheld: the body may carry target credentials)")
}

// escID renders a caller-supplied id as exactly ONE escaped path element.
// url.PathEscape alone is not enough: it leaves "." and ".." untouched, and
// JoinPath cleans those as dot-segments, so an id of ".." would step out of
// its endpoint (…/targets/../roles/<id> is …/roles/<id>). Percent-encoding the
// dots keeps such an id a literal element, which Warpgate's uuid path
// parameter then rejects.
func escID(id string) string {
	if id == "." || id == ".." {
		return strings.ReplaceAll(id, ".", "%2E")
	}
	return url.PathEscape(id)
}
