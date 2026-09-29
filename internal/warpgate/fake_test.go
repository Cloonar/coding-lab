package warpgate

// The test doubles. Every test in this package runs against an httptest
// server — never a live Warpgate and never the network — because what the
// client must get right (method, path, headers, bodies, the idempotency
// quirks, never-duplicate identity) is fully observable from the server side.
//
// fakeWarpgate is a STATEFUL stand-in for the admin API endpoints lab uses.
// It speaks the JSON upstream speaks (wire.go's header, verified against the
// Warpgate 0.29.1 source) but builds its answers from literal field names
// rather than from wire.go's structs, so a wrong tag in wire.go fails these
// tests instead of being mirrored by them. Upstream's quirks are modelled
// where the client depends on them: the case-insensitive LIKE search,
// duplicate usernames as 400 "username", no uniqueness on role names, the
// user PUT that resets omitted fields, the 415 on a bodyless user-role grant,
// 409 for an unknown role on that grant, 201 on an already-active grant, and
// 409/404 on repeated target-role writes.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// testToken is deliberately distinctive so a leak assertion cannot pass by
// accident: any string containing it has spilled the credential.
const testToken = "wg-TEST-TOKEN-do-not-leak"

// recordedRequest is what a test server saw, reduced to the contract.
type recordedRequest struct {
	Method string
	Path   string // escaped — a path-escaped id must stay escaped
	Query  url.Values
	Header http.Header
	Body   string
}

func (r recordedRequest) String() string { return r.Method + " " + r.Path }

func record(r *http.Request) (recordedRequest, []byte) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	return recordedRequest{
		Method: r.Method,
		Path:   r.URL.EscapedPath(),
		Query:  r.URL.Query(),
		Header: r.Header.Clone(),
		Body:   string(body),
	}, body
}

// stub is a stateless recording server for single-answer cases.
type stub struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

func newStub(t *testing.T, h http.HandlerFunc) *stub {
	t.Helper()
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec, _ := record(r)
		s.mu.Lock()
		s.reqs = append(s.reqs, rec)
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.reqs...)
}

func (s *stub) only(t *testing.T) recordedRequest {
	t.Helper()
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("stub saw %d requests, want exactly 1: %v", len(reqs), reqs)
	}
	return reqs[0]
}

// answer replies with one status and body.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: baseURL, Token: testToken})
	if err != nil {
		t.Fatalf("New(%q): %v", baseURL, err)
	}
	return c
}

// --- the stateful fake -------------------------------------------------------

const adminPrefix = "/@warpgate/admin/api/"

// Route names, used to key fault injection.
const (
	rUsers       = "users"
	rUser        = "user"
	rUserRoles   = "user-roles"
	rUserRole    = "user-role"
	rRoles       = "roles"
	rRole        = "role"
	rRoleTargets = "role-targets"
	rTargets     = "targets"
	rTargetRole  = "target-role"
	rKeys        = "keys"
	rKey         = "key"
	rInfo        = "info"
)

type fakeRole struct {
	ID, Name, Description string
	IsDefault             bool
}

type fakeKey struct{ ID, Label, Key string }

type fakeWarpgate struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	seq         int
	users       map[string]map[string]json.RawMessage // id → the full stored object
	roles       map[string]*fakeRole
	grants      map[string]map[string]bool // user id → role id → active
	targets     []string                   // raw target objects, as upstream answers them
	targetRoles map[string]bool            // targetID + "/" + roleID
	keys        map[string][]fakeKey       // user id → credentials
	info        string                     // GET /@warpgate/api/info body for the right token

	// forced answers a route with a fixed status instead of its logic, keyed
	// "METHOD route" (e.g. "PUT user").
	forced map[string]int
	// beforeCreateUser runs (under mu) when a POST /users arrives, before the
	// duplicate check — where a competing creator would have won the race.
	beforeCreateUser func(f *fakeWarpgate)
	// listDelay stalls every list request before it takes mu, widening the
	// window between an ensure's list and its create so a missing client-side
	// lock would reliably produce duplicates.
	listDelay time.Duration

	reqs []recordedRequest
}

func newFake(t *testing.T) *fakeWarpgate {
	t.Helper()
	f := &fakeWarpgate{
		t:           t,
		users:       map[string]map[string]json.RawMessage{},
		roles:       map[string]*fakeRole{},
		grants:      map[string]map[string]bool{},
		targetRoles: map[string]bool{},
		keys:        map[string][]fakeKey{},
		forced:      map[string]int{},
		info:        infoBody(allPerms, "0.29.1"),
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWarpgate) client(t *testing.T) *Client { return newTestClient(t, f.srv.URL) }

// newID mints upstream-shaped UUIDs in creation order, so "smallest id" is
// "created first" unless a test seeds ids explicitly.
func (f *fakeWarpgate) newID() string {
	f.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq)
}

func (f *fakeWarpgate) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.reqs...)
}

// count reports how many requests matched method and route.
func (f *fakeWarpgate) count(method, route string) int {
	n := 0
	for _, r := range f.requests() {
		if got, _ := matchRoute(r.Path); r.Method == method && got == route {
			n++
		}
	}
	return n
}

// writes returns every non-GET request, in order.
func (f *fakeWarpgate) writes() []recordedRequest {
	var out []recordedRequest
	for _, r := range f.requests() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// seedUser stores a user with upstream's full field set; extra overrides or
// adds raw fields (e.g. a credential_policy the heal must preserve).
func (f *fakeWarpgate) seedUser(id, username, description string, extra map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		id = f.newID()
	}
	obj := map[string]json.RawMessage{
		"id":                          jsonString(id),
		"username":                    jsonString(username),
		"description":                 jsonString(description),
		"credential_policy":           json.RawMessage(`{"http":null,"ssh":null,"mysql":null,"postgres":null,"vnc":null,"rdp":null}`),
		"rate_limit_bytes_per_second": json.RawMessage(`null`),
		"ldap_server_id":              json.RawMessage(`null`),
		"allowed_ip_ranges":           json.RawMessage(`null`),
	}
	for k, v := range extra {
		obj[k] = json.RawMessage(v)
	}
	f.users[id] = obj
	return id
}

func (f *fakeWarpgate) seedRole(id, name, description string, isDefault bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		id = f.newID()
	}
	f.roles[id] = &fakeRole{ID: id, Name: name, Description: description, IsDefault: isDefault}
	return id
}

func (f *fakeWarpgate) seedGrant(userID, roleID string, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grants[userID] == nil {
		f.grants[userID] = map[string]bool{}
	}
	f.grants[userID][roleID] = active
}

func (f *fakeWarpgate) userField(id, field string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.users[id][field])
}

func (f *fakeWarpgate) usersNamed(username string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, u := range f.users {
		var name string
		_ = json.Unmarshal(u["username"], &name)
		if name == username {
			ids = append(ids, id)
		}
	}
	return ids
}

func (f *fakeWarpgate) rolesNamed(name string) []*fakeRole {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeRole
	for _, r := range f.roles {
		if r.Name == name {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *fakeWarpgate) grantActive(userID, roleID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grants[userID][roleID]
}

// matchRoute maps an escaped request path to a route name and its
// (unescaped) id parameters.
func matchRoute(escapedPath string) (string, []string) {
	if escapedPath == "/@warpgate/api/info" {
		return rInfo, nil
	}
	rest, ok := strings.CutPrefix(escapedPath, adminPrefix)
	if !ok {
		return "", nil
	}
	seg := strings.Split(rest, "/")
	for i := range seg {
		if u, err := url.PathUnescape(seg[i]); err == nil {
			seg[i] = u
		}
	}
	switch {
	case len(seg) == 1 && seg[0] == "users":
		return rUsers, nil
	case len(seg) == 2 && seg[0] == "users":
		return rUser, seg[1:2]
	case len(seg) == 3 && seg[0] == "users" && seg[2] == "roles":
		return rUserRoles, seg[1:2]
	case len(seg) == 4 && seg[0] == "users" && seg[2] == "roles":
		return rUserRole, []string{seg[1], seg[3]}
	case len(seg) == 4 && seg[0] == "users" && seg[2] == "credentials" && seg[3] == "public-keys":
		return rKeys, seg[1:2]
	case len(seg) == 5 && seg[0] == "users" && seg[2] == "credentials" && seg[3] == "public-keys":
		return rKey, []string{seg[1], seg[4]}
	case len(seg) == 1 && seg[0] == "roles":
		return rRoles, nil
	case len(seg) == 2 && seg[0] == "role":
		return rRole, seg[1:2]
	case len(seg) == 3 && seg[0] == "role" && seg[2] == "targets":
		return rRoleTargets, seg[1:2]
	case len(seg) == 1 && seg[0] == "targets":
		return rTargets, nil
	case len(seg) == 4 && seg[0] == "targets" && seg[2] == "roles":
		return rTargetRole, []string{seg[1], seg[3]}
	}
	return "", nil
}

func (f *fakeWarpgate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec, body := record(r)
	route, params := matchRoute(rec.Path)
	if r.Method == http.MethodGet && route != rInfo && f.listDelay > 0 {
		time.Sleep(f.listDelay)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, rec)

	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}

	if route == rInfo {
		if r.Header.Get(tokenHeader) != testToken {
			// A rejected token is simply anonymous: 200, nulls (wire.go point 3).
			_, _ = io.WriteString(w, infoBody(nil, ""))
			return
		}
		_, _ = io.WriteString(w, f.info)
		return
	}
	if r.Header.Get(tokenHeader) != testToken {
		w.WriteHeader(http.StatusUnauthorized) // empty body, like upstream
		return
	}
	if route == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if st, ok := f.forced[r.Method+" "+route]; ok {
		reply(st, "forced failure")
		return
	}
	jsonBody := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")

	switch r.Method + " " + route {
	case "GET " + rUsers:
		term := strings.ToLower(rec.Query.Get("search"))
		var rows []map[string]json.RawMessage
		for _, u := range f.users {
			var name string
			_ = json.Unmarshal(u["username"], &name)
			if strings.Contains(strings.ToLower(name), term) {
				rows = append(rows, u)
			}
		}
		sort.Slice(rows, func(i, j int) bool { return string(rows[i]["username"]) < string(rows[j]["username"]) })
		reply(http.StatusOK, nonNil(rows))

	case "POST " + rUsers:
		if f.beforeCreateUser != nil {
			f.beforeCreateUser(f)
		}
		var req struct {
			Username    string  `json:"username"`
			Description *string `json:"description"`
		}
		if !jsonBody || json.Unmarshal(body, &req) != nil {
			reply(http.StatusBadRequest, "parse error")
			return
		}
		if req.Username == "" {
			reply(http.StatusBadRequest, "name")
			return
		}
		for _, u := range f.users {
			var name string
			_ = json.Unmarshal(u["username"], &name)
			if strings.EqualFold(name, req.Username) {
				reply(http.StatusBadRequest, "username")
				return
			}
		}
		id := f.newID()
		desc := ""
		if req.Description != nil {
			desc = *req.Description
		}
		f.users[id] = map[string]json.RawMessage{
			"id": jsonString(id), "username": jsonString(req.Username), "description": jsonString(desc),
			"credential_policy":           json.RawMessage(`{"http":null,"ssh":null,"mysql":null,"postgres":null,"vnc":null,"rdp":null}`),
			"rate_limit_bytes_per_second": json.RawMessage(`null`), "ldap_server_id": json.RawMessage(`null`),
			"allowed_ip_ranges": json.RawMessage(`null`),
		}
		reply(http.StatusCreated, f.users[id])

	case "GET " + rUser:
		u, ok := f.users[params[0]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(http.StatusOK, u)

	case "PUT " + rUser:
		u, ok := f.users[params[0]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req map[string]json.RawMessage
		if !jsonBody || json.Unmarshal(body, &req) != nil {
			reply(http.StatusBadRequest, "parse error")
			return
		}
		// Upstream semantics (users.rs:272-281): the body REPLACES the
		// user-editable fields; an omitted one resets.
		orNull := func(k string) json.RawMessage {
			if v, ok := req[k]; ok {
				return v
			}
			return json.RawMessage(`null`)
		}
		u["username"] = req["username"]
		u["description"] = jsonString("")
		if d, ok := req["description"]; ok && string(d) != "null" {
			u["description"] = d
		}
		u["credential_policy"] = orNull("credential_policy")
		u["rate_limit_bytes_per_second"] = orNull("rate_limit_bytes_per_second")
		u["allowed_ip_ranges"] = orNull("allowed_ip_ranges")
		reply(http.StatusOK, u)

	case "DELETE " + rUser:
		if _, ok := f.users[params[0]]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.users, params[0])
		delete(f.grants, params[0])
		w.WriteHeader(http.StatusNoContent)

	case "GET " + rUserRoles:
		if _, ok := f.users[params[0]]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rows := []map[string]any{}
		for rid, active := range f.grants[params[0]] {
			role := f.roles[rid]
			if role == nil {
				continue
			}
			rows = append(rows, map[string]any{
				"id": rid, "name": role.Name, "description": role.Description,
				"granted_at": "2026-09-29T00:00:00Z", "expires_at": nil, "is_expired": false, "is_active": active,
			})
		}
		reply(http.StatusOK, rows)

	case "POST " + rUserRole:
		if !jsonBody {
			// poem-openapi refuses a JSON-bodied endpoint without the header.
			w.WriteHeader(http.StatusUnsupportedMediaType)
			_, _ = io.WriteString(w, "the client request does not include the `Content-Type` header")
			return
		}
		if _, ok := f.users[params[0]]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		role, ok := f.roles[params[1]]
		if !ok {
			w.WriteHeader(http.StatusConflict) // the trap: unknown ROLE is 409
			return
		}
		if f.grants[params[0]] == nil {
			f.grants[params[0]] = map[string]bool{}
		}
		f.grants[params[0]][params[1]] = true
		reply(http.StatusCreated, map[string]any{"id": role.ID, "name": role.Name, "description": role.Description, "is_active": true, "is_expired": false})

	case "GET " + rRoles:
		term := strings.ToLower(rec.Query.Get("search"))
		rows := []map[string]any{}
		ids := make([]string, 0, len(f.roles))
		for id := range f.roles {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			role := f.roles[id]
			if strings.Contains(strings.ToLower(role.Name), term) {
				rows = append(rows, roleJSON(role))
			}
		}
		reply(http.StatusOK, rows)

	case "POST " + rRoles:
		var req struct {
			Name        string  `json:"name"`
			Description *string `json:"description"`
			IsDefault   *bool   `json:"is_default"`
		}
		if !jsonBody || json.Unmarshal(body, &req) != nil || req.Name == "" {
			reply(http.StatusBadRequest, "name")
			return
		}
		role := &fakeRole{ID: f.newID(), Name: req.Name}
		if req.Description != nil {
			role.Description = *req.Description
		}
		if req.IsDefault != nil {
			role.IsDefault = *req.IsDefault
		}
		f.roles[role.ID] = role // no uniqueness check, like upstream
		reply(http.StatusCreated, roleJSON(role))

	case "PUT " + rRole:
		role, ok := f.roles[params[0]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Name        string  `json:"name"`
			Description *string `json:"description"`
			IsDefault   *bool   `json:"is_default"`
		}
		if !jsonBody || json.Unmarshal(body, &req) != nil {
			reply(http.StatusBadRequest, "parse error")
			return
		}
		role.Name, role.Description = req.Name, ""
		if req.Description != nil {
			role.Description = *req.Description
		}
		if req.IsDefault != nil { // omitted keeps the current flag (roles.rs:175)
			role.IsDefault = *req.IsDefault
		}
		reply(http.StatusOK, roleJSON(role))

	case "DELETE " + rRole:
		if _, ok := f.roles[params[0]]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.roles, params[0])
		w.WriteHeader(http.StatusNoContent)

	case "GET " + rTargets:
		_, _ = io.WriteString(w, "["+strings.Join(f.targets, ",")+"]")

	case "GET " + rRoleTargets:
		if _, ok := f.roles[params[0]]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var picked []string
		for _, raw := range f.targets {
			var t struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal([]byte(raw), &t)
			if f.targetRoles[t.ID+"/"+params[0]] {
				picked = append(picked, raw)
			}
		}
		_, _ = io.WriteString(w, "["+strings.Join(picked, ",")+"]")

	case "POST " + rTargetRole:
		k := params[0] + "/" + params[1]
		if f.targetRoles[k] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.targetRoles[k] = true
		w.WriteHeader(http.StatusCreated)

	case "DELETE " + rTargetRole:
		k := params[0] + "/" + params[1]
		if !f.targetRoles[k] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.targetRoles, k)
		w.WriteHeader(http.StatusNoContent)

	case "GET " + rKeys:
		rows := []map[string]any{}
		for _, k := range f.keys[params[0]] {
			rows = append(rows, keyJSON(k))
		}
		reply(http.StatusOK, rows)

	case "POST " + rKeys:
		if _, ok := f.users[params[0]]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Label string `json:"label"`
			Key   string `json:"openssh_public_key"`
		}
		if !jsonBody || json.Unmarshal(body, &req) != nil {
			reply(http.StatusBadRequest, "parse error")
			return
		}
		// Upstream strips the comment (public_key_credentials.rs:70).
		fields := strings.Fields(req.Key)
		if len(fields) < 2 {
			reply(http.StatusInternalServerError, "Internal Server Error (reference: 00000000-0000-0000-0000-000000000000)")
			return
		}
		k := fakeKey{ID: f.newID(), Label: req.Label, Key: fields[0] + " " + fields[1]}
		f.keys[params[0]] = append(f.keys[params[0]], k)
		reply(http.StatusCreated, keyJSON(k))

	case "DELETE " + rKey:
		keys := f.keys[params[0]]
		for i, k := range keys {
			if k.ID == params[1] {
				f.keys[params[0]] = append(keys[:i:i], keys[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func roleJSON(r *fakeRole) map[string]any {
	return map[string]any{"id": r.ID, "name": r.Name, "description": r.Description, "is_default": r.IsDefault}
}

func keyJSON(k fakeKey) map[string]any {
	return map[string]any{"id": k.ID, "label": k.Label, "date_added": "2026-09-29T00:00:00Z", "last_used": nil, "openssh_public_key": k.Key}
}

// allPerms is the static admin token's permission set (every admin
// permission, warpgate-admin/src/api/common.rs:24-26).
var allPerms = map[string]bool{
	"targets_create": true, "targets_edit": true, "targets_delete": true,
	"users_create": true, "users_edit": true, "users_delete": true,
	"access_roles_create": true, "access_roles_edit": true, "access_roles_delete": true, "access_roles_assign": true,
	"sessions_view": true, "sessions_terminate": true, "approve_sessions": true, "recordings_view": true,
	"tickets_create": true, "tickets_delete": true, "config_edit": true, "admin_roles_manage": true,
	"ticket_requests_manage": true,
}

// infoBody renders a GET /@warpgate/api/info answer (info.rs: struct Info and
// api_get_info).
// perms nil ⇒ the anonymous shape: version, ports and admin_permissions null.
func infoBody(perms map[string]bool, version string) string {
	body := map[string]any{
		"version": nil, "username": nil, "selected_target": nil, "external_host": "localhost",
		"external_hosts":      nil,
		"ports":               map[string]any{"ssh": nil, "http": nil, "mysql": nil, "postgres": nil, "kubernetes": nil, "vnc": nil, "rdp": nil},
		"password_login_mode": "Allowed", "authorized_via_ticket": false,
		"authorized_via_sso_with_single_logout": false, "own_credential_management_allowed": true,
		"web_clients_enabled": true, "has_ldap": false, "needs_mfa_setup": false, "otp_setup_enforced": false,
		"setup_state": nil, "admin_permissions": nil, "running_on_ec2": nil, "should_prompt_analytics": false,
		"banner": "", "show_session_menu": true, "config_warnings": nil,
	}
	if perms != nil {
		body["version"] = version
		body["username"] = nil // the static admin token has no user
		body["ports"] = map[string]any{"ssh": 2222, "http": 8888, "mysql": nil, "postgres": nil, "kubernetes": nil, "vnc": nil, "rdp": nil}
		body["admin_permissions"] = perms
	}
	b, _ := json.Marshal(body)
	return string(b)
}
