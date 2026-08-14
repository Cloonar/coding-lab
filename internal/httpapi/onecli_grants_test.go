package httpapi

// The per-repo grant picker's server side (issue #25 / ADR-0067). Everything
// here runs against a STUBBED OneCLI REST API — the ADR's rule is "tests run
// against a stubbed HTTP API, never a live sidecar" — and the stub records
// every request it receives, so an attach assertion checks the real wire call
// (method, path, body) rather than the handler's intent.
//
// The stub's agent-identity rows carry testProxyToken, a live gateway
// credential in the real system. That is deliberate: it turns "no response may
// carry the token" from a code-reading claim into an assertion any of these
// tests can make.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// testProxyToken stands in for an agent identity's gateway access token — the
// credential a run authenticates to the proxy with. Distinctive on purpose:
// every test here can grep a whole response body for it.
const testProxyToken = "oc_agent_live_proxy_token_5f1c"

// --- the OneCLI REST stub ---------------------------------------------------

// stubCall is one request the stub received, in the only three dimensions the
// assertions care about.
type stubCall struct {
	Method string
	Path   string
	Body   string
}

// stubPoolRow is one pool resource the stub serves. Provider doubles as a
// secret's wire "type" and a connection's "provider"; Name doubles as a
// connection's "label" and is deliberately allowed to be empty, so the
// client's fallback-to-provider-slug rule is exercised end to end.
type stubPoolRow struct {
	ID       string
	Name     string
	Provider string
}

// stubAgent is one agent identity the stub's project holds. The project is
// keyed by IDENTIFIER, never by Name, because that is upstream's own uniqueness
// rule and lab's match key since issue #35 — a stub keyed by name would let a
// name-matching regression pass.
type stubAgent struct {
	ID   string
	Name string
}

// oneCLIGrantStub answers the six OneCLI REST paths this slice drives, keeps
// the project state the handlers mutate (agent identities and their grants),
// and records every request. Safe for concurrent use; the handlers under test
// issue their calls serially, but httptest serves each on its own goroutine.
type oneCLIGrantStub struct {
	URL string

	mu          sync.Mutex
	calls       []stubCall
	secrets     []stubPoolRow
	connections []stubPoolRow
	agents      map[string]*stubAgent      // agent IDENTIFIER → the identity
	grants      map[string]map[string]bool // agent identity id → "kind/resourceID"
	nextAgent   int

	// failStatus, when non-zero, makes every route answer that status with
	// OneCLI's error envelope instead of doing its job — the "configured but
	// the call fails" half of the contract.
	failStatus int
}

func newOneCLIGrantStub(t *testing.T) *oneCLIGrantStub {
	t.Helper()
	stub := &oneCLIGrantStub{
		agents: map[string]*stubAgent{},
		grants: map[string]map[string]bool{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/secrets", stub.handleSecrets)
	mux.HandleFunc("GET /v1/connections", stub.handleConnections)
	mux.HandleFunc("GET /v1/agents", stub.handleListAgents)
	mux.HandleFunc("POST /v1/agents", stub.handleCreateAgent)
	mux.HandleFunc("PATCH /v1/agents/{id}", stub.handleRenameAgent)
	mux.HandleFunc("GET /v1/agents/{id}/grants", stub.handleListGrants)
	mux.HandleFunc("PUT /v1/agents/{id}/grants/{kind}/{rid}", stub.handleAttach)
	mux.HandleFunc("DELETE /v1/agents/{id}/grants/{kind}/{rid}", stub.handleDetach)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.record(r)
		if status := stub.failure(); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"the stub is refusing on purpose","type":"stub"}}`))
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	stub.URL = ts.URL
	return stub
}

// record captures the request and restores its body for the real handler.
func (s *oneCLIGrantStub) record(r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{Method: r.Method, Path: r.URL.Path, Body: string(body)})
}

func (s *oneCLIGrantStub) failure() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failStatus
}

func (s *oneCLIGrantStub) fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failStatus = status
}

// seedPool sets the project's secrets and connections.
func (s *oneCLIGrantStub) seedPool(secrets, connections []stubPoolRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets, s.connections = secrets, connections
}

// seedAgent creates the agent identity carrying identifier, under the display
// name name, and returns its OneCLI id — without going through the API, the
// "this repo already has an identity" starting state. The two are separate
// arguments so a test can seed the pre-#35 shape: the derived identifier with
// the repo's store id still sitting in the name.
func (s *oneCLIGrantStub) seedAgent(identifier, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createAgentLocked(identifier, name).ID
}

func (s *oneCLIGrantStub) createAgentLocked(identifier, name string) *stubAgent {
	if agent, ok := s.agents[identifier]; ok {
		return agent
	}
	s.nextAgent++
	agent := &stubAgent{ID: fmt.Sprintf("agt_%d", s.nextAgent), Name: name}
	s.agents[identifier] = agent
	s.grants[agent.ID] = map[string]bool{}
	return agent
}

// seedGrant attaches a resource to an agent identity directly, creating the
// identity when the case did not seed one. Its display name is then the
// identifier itself — a placeholder no assertion reads, since a test that cares
// about the name seeds the identity with seedAgent.
func (s *oneCLIGrantStub) seedGrant(identifier, kind, resourceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := s.createAgentLocked(identifier, identifier)
	s.grants[agent.ID][kind+"/"+resourceID] = true
}

func (s *oneCLIGrantStub) hasGrant(identifier, kind, resourceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	agent, ok := s.agents[identifier]
	if !ok {
		return false
	}
	return s.grants[agent.ID][kind+"/"+resourceID]
}

// agentName reports the display name the identity carrying identifier now
// holds upstream — the assertion behind "an ensure heals a stale name".
func (s *oneCLIGrantStub) agentName(identifier string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	agent, ok := s.agents[identifier]
	if !ok {
		return ""
	}
	return agent.Name
}

// calledCount counts recorded requests with exactly this method and path.
func (s *oneCLIGrantStub) calledCount(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.Method == method && c.Path == path {
			n++
		}
	}
	return n
}

// call returns the single recorded request with this method and path, failing
// the test when there is not exactly one.
func (s *oneCLIGrantStub) call(t *testing.T, method, path string) stubCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []stubCall
	for _, c := range s.calls {
		if c.Method == method && c.Path == path {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s %s was recorded %d times, want exactly 1 (all calls: %+v)", method, path, len(found), s.calls)
	}
	return found[0]
}

func (s *oneCLIGrantStub) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *oneCLIGrantStub) allCalls() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

func (s *oneCLIGrantStub) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleSecrets serves GET /v1/secrets: a bare array whose provider field is
// spelled "type" (wire.go point 8).
func (s *oneCLIGrantStub) handleSecrets(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	rows := make([]map[string]any, 0, len(s.secrets))
	for _, r := range s.secrets {
		rows = append(rows, map[string]any{"id": r.ID, "name": r.Name, "type": r.Provider})
	}
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, rows)
}

// handleConnections serves GET /v1/connections: no "name" — the display name
// is "label", and it may be empty.
func (s *oneCLIGrantStub) handleConnections(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	rows := make([]map[string]any, 0, len(s.connections))
	for _, r := range s.connections {
		rows = append(rows, map[string]any{"id": r.ID, "provider": r.Provider, "label": r.Name})
	}
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, rows)
}

// handleListAgents serves GET /v1/agents — rows carrying both human-facing
// fields and the access token, exactly as the real listing does (wire.go point
// 4).
func (s *oneCLIGrantStub) handleListAgents(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	rows := make([]map[string]any, 0, len(s.agents))
	for identifier, agent := range s.agents {
		rows = append(rows, map[string]any{"id": agent.ID, "name": agent.Name, "identifier": identifier, "accessToken": testProxyToken})
	}
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, rows)
}

// handleCreateAgent serves POST /v1/agents: 409 on a duplicate IDENTIFIER —
// the only field upstream constrains unique — else 201 with a body that
// deliberately carries NO access token (wire.go points 4 and 5).
func (s *oneCLIGrantStub) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Identifier string `json:"identifier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Identifier == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and identifier are required"})
		return
	}
	s.mu.Lock()
	_, exists := s.agents[req.Identifier]
	if exists {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "identifier already taken"})
		return
	}
	agent := s.createAgentLocked(req.Identifier, req.Name)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, map[string]any{"id": agent.ID, "name": req.Name, "identifier": req.Identifier})
}

// handleRenameAgent serves PATCH /v1/agents/{id} (wire.go point 10): the
// display name and nothing else, answered with {"success":true}. There is
// deliberately no way to move an identifier through it — upstream's is
// immutable, so a rename structurally cannot restate what a repo's grants hang
// off.
func (s *oneCLIGrantStub) handleRenameAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, agent := range s.agents {
		if agent.ID == id {
			agent.Name = req.Name
			s.writeJSON(w, http.StatusOK, map[string]any{"success": true})
			return
		}
	}
	s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown agent"})
}

// grantsDocLocked renders GET /v1/agents/{id}/grants's answer (wire.go point
// 6): ids named by kind, connection display names in "label".
func (s *oneCLIGrantStub) grantsDocLocked(agentID string) map[string]any {
	secrets := []map[string]any{}
	connections := []map[string]any{}
	for key := range s.grants[agentID] {
		kind, id, _ := strings.Cut(key, "/")
		switch kind {
		case "secrets":
			secrets = append(secrets, map[string]any{"secretId": id, "name": s.poolNameLocked(s.secrets, id), "type": "generic"})
		case "connections":
			connections = append(connections, map[string]any{"connectionId": id, "provider": "github", "label": s.poolNameLocked(s.connections, id)})
		}
	}
	return map[string]any{"agentId": agentID, "mode": "strict", "secrets": secrets, "connections": connections}
}

func (s *oneCLIGrantStub) poolNameLocked(rows []stubPoolRow, id string) string {
	for _, r := range rows {
		if r.ID == id {
			return r.Name
		}
	}
	return id
}

func (s *oneCLIGrantStub) handleListGrants(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	doc := s.grantsDocLocked(r.PathValue("id"))
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, doc)
}

// handleAttach serves PUT …/grants/{kind}/{rid}: 200 with the agent
// identity's grants, idempotent (wire.go point 7).
func (s *oneCLIGrantStub) handleAttach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	if s.grants[id] == nil {
		s.mu.Unlock()
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown agent identity"})
		return
	}
	s.grants[id][r.PathValue("kind")+"/"+r.PathValue("rid")] = true
	doc := s.grantsDocLocked(id)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, doc)
}

// handleDetach serves DELETE …/grants/{kind}/{rid}: 204, idempotent.
func (s *oneCLIGrantStub) handleDetach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	if s.grants[id] != nil {
		delete(s.grants[id], r.PathValue("kind")+"/"+r.PathValue("rid"))
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// --- test harness -----------------------------------------------------------

// newOneCLIGrantServer builds a logged-in server wired to stub (nil = the
// integration is unconfigured) plus one repo to hang the per-repo routes off.
func newOneCLIGrantServer(t *testing.T, stub *oneCLIGrantStub) (*testServer, store.Repo) {
	t.Helper()
	x := newTestServer(t, func(o *Options) {
		if stub != nil {
			o.OneCLI = oneCLIClient(t, stub.URL)
		}
	})
	x.setup("op", "password123")
	return x, seedTrackerRepo(t, x, "grant-picker", nil)
}

// agentKey is the identifier a repo's agent identity is matched by. Derived
// rather than spelled out: the rule lives in exactly one place
// (onecli.AgentIdentifier), and a second copy of it here is the drift that
// would let a repo end up with two identities, only one carrying its grants.
func agentKey(repo store.Repo) string { return onecli.AgentIdentifier(repo.ID) }

// grantPath builds the mutation URL the SPA sends.
func grantPath(repo store.Repo, kind, resourceID string) string {
	return "/api/v1/repos/" + repo.ID + "/onecli/grants/" + kind + "/" + resourceID
}

func grantsPath(repo store.Repo) string {
	return "/api/v1/repos/" + repo.ID + "/onecli/grants"
}

// rawBody reads and closes a response body.
func rawBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// entriesOf pulls an array of objects out of a decoded body.
func entriesOf(t *testing.T, body map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("body[%q] = %#v, want an array (full body %#v)", key, body[key], body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		obj, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("body[%q] carries a non-object entry %#v", key, e)
		}
		out = append(out, obj)
	}
	return out
}

// wantErrorBody asserts the canonical {"error":"…"} shape with a non-empty
// message — an error an operator cannot read is not an answer.
func wantErrorBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body := decodeBody(t, resp)
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatalf("response carries no error message: %#v", body)
	}
	return msg
}

// --- the pool ---------------------------------------------------------------

// TestOneCLIPool covers the picker's left-hand side: the whole lab-wide pool,
// both kinds, with the metadata the picker renders — and the empty pool, which
// must stay distinguishable from an unconfigured lab.
func TestOneCLIPool(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		stub := newOneCLIGrantStub(t)
		stub.seedPool(
			[]stubPoolRow{{ID: "sec_1", Name: "DEPLOY_TOKEN", Provider: "generic"}, {ID: "sec_2", Name: "ANTHROPIC_API_KEY", Provider: "anthropic"}},
			// The second connection has no label: its display name falls back
			// to the provider slug, so no row ever renders nameless.
			[]stubPoolRow{{ID: "con_1", Name: "Acme GitHub", Provider: "github"}, {ID: "con_2", Provider: "slack"}},
		)
		x, _ := newOneCLIGrantServer(t, stub)

		resp := x.do("GET", "/api/v1/onecli/pool", nil, nil)
		wantStatus(t, resp, http.StatusOK)
		body := decodeBody(t, resp)
		if body["configured"] != true {
			t.Fatalf("configured = %v, want true (body %#v)", body["configured"], body)
		}
		secrets := entriesOf(t, body, "secrets")
		if len(secrets) != 2 || secrets[0]["id"] != "sec_1" || secrets[0]["name"] != "DEPLOY_TOKEN" || secrets[0]["provider"] != "generic" {
			t.Fatalf("secrets = %#v", secrets)
		}
		if secrets[1]["provider"] != "anthropic" {
			t.Fatalf("second secret lost its provider: %#v", secrets[1])
		}
		connections := entriesOf(t, body, "connections")
		if len(connections) != 2 || connections[0]["id"] != "con_1" || connections[0]["name"] != "Acme GitHub" || connections[0]["provider"] != "github" {
			t.Fatalf("connections = %#v", connections)
		}
		if connections[1]["name"] != "slack" {
			t.Fatalf("label-less connection = %#v, want name to fall back to the provider slug", connections[1])
		}
	})

	t.Run("empty pool is configured with two empty arrays", func(t *testing.T) {
		stub := newOneCLIGrantStub(t)
		x, _ := newOneCLIGrantServer(t, stub)

		resp := x.do("GET", "/api/v1/onecli/pool", nil, nil)
		wantStatus(t, resp, http.StatusOK)
		raw := rawBody(t, resp)
		// Pinned literally: the arrays are always present and never null, so
		// the SPA reads one shape, and configured:true is what distinguishes an
		// empty pool from an unconfigured lab.
		if !strings.Contains(raw, `"configured":true`) || !strings.Contains(raw, `"secrets":[]`) || !strings.Contains(raw, `"connections":[]`) {
			t.Fatalf("empty-pool body = %s, want configured:true with two empty arrays", raw)
		}
	})
}

// --- the unconfigured lab ---------------------------------------------------

// TestOneCLIGrantsUnconfigured pins the default lab across all four routes:
// the reads answer a well-formed body saying the integration is off, the
// mutations refuse with a 409 that says why. Never a 404 (indistinguishable to
// the SPA from a lab too old to have these endpoints), never a panic on the
// nil client.
func TestOneCLIGrantsUnconfigured(t *testing.T) {
	x, repo := newOneCLIGrantServer(t, nil)

	resp := x.do("GET", "/api/v1/onecli/pool", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	raw := rawBody(t, resp)
	if !strings.Contains(raw, `"configured":false`) || !strings.Contains(raw, `"secrets":[]`) || !strings.Contains(raw, `"connections":[]`) {
		t.Fatalf("unconfigured pool body = %s, want configured:false with two empty arrays", raw)
	}

	resp = x.do("GET", grantsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	raw = rawBody(t, resp)
	if !strings.Contains(raw, `"configured":false`) || !strings.Contains(raw, `"grants":[]`) {
		t.Fatalf("unconfigured grants body = %s, want configured:false with an empty array", raw)
	}

	// A mutation against an integration that is off is a real error: a click
	// that silently succeeded would be a lie about the repo's access.
	for _, method := range []string{"PUT", "DELETE"} {
		resp = x.do(method, grantPath(repo, "secrets", "sec_1"), nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusConflict)
		if msg := wantErrorBody(t, resp); !strings.Contains(msg, "not configured") {
			t.Errorf("%s error = %q, want it to say the integration is not configured", method, msg)
		}
	}
}

// --- a configured but failing sidecar ---------------------------------------

// TestOneCLIGrantsUnreachableSidecar covers the third state a consumer must
// render distinctly: configured, but the call failed. Every endpoint answers
// 502 — what broke is the upstream lab proxies, not lab — with a message an
// operator can act on and never the API key.
func TestOneCLIGrantsUnreachableSidecar(t *testing.T) {
	t.Run("nothing listening", func(t *testing.T) {
		dead := "http://" + deadAddr(t)
		x := newTestServer(t, func(o *Options) { o.OneCLI = oneCLIClient(t, dead) })
		x.setup("op", "password123")
		repo := seedTrackerRepo(t, x, "grant-picker", nil)

		for _, path := range []string{"/api/v1/onecli/pool", grantsPath(repo)} {
			resp := x.do("GET", path, nil, nil)
			wantStatus(t, resp, http.StatusBadGateway)
			if msg := wantErrorBody(t, resp); !strings.Contains(msg, "OneCLI") {
				t.Errorf("GET %s error = %q, want it to name OneCLI", path, msg)
			}
		}
	})

	t.Run("the api key was revoked", func(t *testing.T) {
		stub := newOneCLIGrantStub(t)
		stub.fail(http.StatusUnauthorized)
		x, repo := newOneCLIGrantServer(t, stub)

		cases := []struct {
			method, path string
			headers      map[string]string
		}{
			{"GET", "/api/v1/onecli/pool", nil},
			{"GET", grantsPath(repo), nil},
			{"PUT", grantPath(repo, "secrets", "sec_1"), csrfHeaders(x.ts.URL)},
			{"DELETE", grantPath(repo, "secrets", "sec_1"), csrfHeaders(x.ts.URL)},
		}
		for _, tc := range cases {
			resp := x.do(tc.method, tc.path, nil, tc.headers)
			wantStatus(t, resp, http.StatusBadGateway)
			msg := wantErrorBody(t, resp)
			// The *APIError's own text: the status is what tells an operator
			// "revoked key", not "sidecar down".
			if !strings.Contains(msg, "401") {
				t.Errorf("%s %s error = %q, does not name the status", tc.method, tc.path, msg)
			}
			// The API key travels in a header and structurally cannot be in an
			// *APIError — this is the assertion that keeps it that way.
			if strings.Contains(msg, "oc_proj_test") {
				t.Fatalf("%s %s leaked the API key: %q", tc.method, tc.path, msg)
			}
		}
	})
}

// --- reads never create an agent identity -----------------------------------

// TestOneCLIGrantListWithoutAgentIdentity pins the read's no-side-effects
// rule: a repo nobody has granted anything to yet has no OneCLI agent
// identity, and merely opening its picker must not create one.
func TestOneCLIGrantListWithoutAgentIdentity(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	x, repo := newOneCLIGrantServer(t, stub)

	resp := x.do("GET", grantsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	raw := rawBody(t, resp)
	if !strings.Contains(raw, `"configured":true`) || !strings.Contains(raw, `"grants":[]`) {
		t.Fatalf("body = %s, want configured:true with an empty grants array", raw)
	}
	if n := stub.calledCount("POST", "/v1/agents"); n != 0 {
		t.Fatalf("the read created %d agent identities (calls %+v)", n, stub.allCalls())
	}
	if n := stub.calledCount("GET", "/v1/agents"); n != 1 {
		t.Fatalf("GET /v1/agents recorded %d times, want 1 (calls %+v)", n, stub.allCalls())
	}
}

// TestOneCLIGrantListReportsBothKinds pins the list body for a repo that has
// grants: kind, id and display name for each, both kinds flattened into one
// array in the shape the picker reads.
func TestOneCLIGrantListReportsBothKinds(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	stub.seedPool(
		[]stubPoolRow{{ID: "sec_1", Name: "DEPLOY_TOKEN", Provider: "generic"}},
		[]stubPoolRow{{ID: "con_1", Name: "Acme GitHub", Provider: "github"}},
	)
	x, repo := newOneCLIGrantServer(t, stub)
	stub.seedGrant(agentKey(repo), "secrets", "sec_1")
	stub.seedGrant(agentKey(repo), "connections", "con_1")

	resp := x.do("GET", grantsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	if body["configured"] != true {
		t.Fatalf("configured = %v, want true", body["configured"])
	}
	grants := entriesOf(t, body, "grants")
	if len(grants) != 2 {
		t.Fatalf("grants = %#v, want two", grants)
	}
	// Secrets first, then connections (decodeGrants' order).
	if grants[0]["kind"] != "secrets" || grants[0]["id"] != "sec_1" || grants[0]["name"] != "DEPLOY_TOKEN" {
		t.Errorf("secret grant = %#v", grants[0])
	}
	if grants[1]["kind"] != "connections" || grants[1]["id"] != "con_1" || grants[1]["name"] != "Acme GitHub" {
		t.Errorf("connection grant = %#v", grants[1])
	}
}

// TestOneCLIGrantListAdoptsALegacyAgentIdentity is issue #35's zero-migration
// promise at the HTTP layer. An identity created before the inversion carries
// the repo's STORE ID in its NAME, and nothing anywhere rewrites it; what makes
// it findable today is its IDENTIFIER, which lab has always derived from that
// same store id and so is byte-identical to the one derived now. A read that
// missed it would report "no grants yet" for a repo that has them — inviting
// the operator to attach a second copy of everything, and leaving the first set
// attached to an identity nothing points at any more.
func TestOneCLIGrantListAdoptsALegacyAgentIdentity(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	stub.seedPool([]stubPoolRow{{ID: "sec_1", Name: "DEPLOY_TOKEN", Provider: "generic"}}, nil)
	x, repo := newOneCLIGrantServer(t, stub)
	// The pre-#35 row: today's derived identifier, the store id still sitting
	// in the display name.
	stub.seedAgent(agentKey(repo), repo.ID)
	stub.seedGrant(agentKey(repo), "secrets", "sec_1")

	resp := x.do("GET", grantsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	grants := entriesOf(t, decodeBody(t, resp), "grants")
	if len(grants) != 1 || grants[0]["id"] != "sec_1" || grants[0]["name"] != "DEPLOY_TOKEN" {
		t.Fatalf("grants = %#v, want the legacy identity's one secret", grants)
	}
	// Adoption is a MATCH, never a write: the read created nothing and renamed
	// nothing, which is what keeps opening a picker free of side effects.
	if n := stub.calledCount("POST", "/v1/agents"); n != 0 {
		t.Errorf("the read created %d agent identities (calls %+v)", n, stub.allCalls())
	}
	if got := stub.agentName(agentKey(repo)); got != repo.ID {
		t.Errorf("the read renamed the identity to %q; only a mutation may write", got)
	}
}

// --- attach -----------------------------------------------------------------

// TestOneCLIGrantAttach walks the picker's write path: the first attach
// creates the repo's agent identity — identified by the slug derived from the
// repo's STORE ID, the one key internal/instance's launch path also uses, and
// named by the repo so the OneCLI dashboard reads as a list of repositories —
// and issues the kind-qualified PUT; a repeat is a no-op that neither creates a
// second identity nor fails.
func TestOneCLIGrantAttach(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	stub.seedPool(
		[]stubPoolRow{{ID: "sec_1", Name: "DEPLOY_TOKEN", Provider: "generic"}},
		[]stubPoolRow{{ID: "con_1", Name: "Acme GitHub", Provider: "github"}},
	)
	x, repo := newOneCLIGrantServer(t, stub)

	resp := x.do("PUT", grantPath(repo, "secrets", "sec_1"), nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	if body := rawBody(t, resp); body != "" {
		t.Errorf("204 carried a body: %q", body)
	}

	// The identity was created under the derived identifier — the key the
	// grants hang off, which a rename cannot move — and carries the repo's
	// display name, which is the half a rename is free to change.
	create := stub.call(t, "POST", "/v1/agents")
	var created struct {
		Name       string `json:"name"`
		Identifier string `json:"identifier"`
	}
	if err := json.Unmarshal([]byte(create.Body), &created); err != nil {
		t.Fatalf("decode create body %q: %v", create.Body, err)
	}
	if created.Identifier != agentKey(repo) {
		t.Fatalf("created agent identity carries identifier %q, want the one derived from the repo's store id %q", created.Identifier, agentKey(repo))
	}
	if created.Name != repo.Name {
		t.Fatalf("created agent identity named %q, want the repo's display name %q", created.Name, repo.Name)
	}
	if !stub.hasGrant(agentKey(repo), "secrets", "sec_1") {
		t.Fatalf("the secret was not attached (calls %+v)", stub.allCalls())
	}
	stub.call(t, "PUT", "/v1/agents/agt_1/grants/secrets/sec_1")

	// A connection attach is the other wire shape: same URL grammar, but the
	// client must send the whole-app {"access":"full"} body OneCLI requires.
	resp = x.do("PUT", grantPath(repo, "connections", "con_1"), nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	attach := stub.call(t, "PUT", "/v1/agents/agt_1/grants/connections/con_1")
	if !strings.Contains(attach.Body, `"access":"full"`) {
		t.Errorf("connection attach body = %q, want the whole-app access", attach.Body)
	}

	// Idempotent: replaying the operator's whole selection is safe, and the
	// second attach must not create a second identity.
	resp = x.do("PUT", grantPath(repo, "secrets", "sec_1"), nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if n := stub.calledCount("POST", "/v1/agents"); n != 1 {
		t.Fatalf("POST /v1/agents recorded %d times, want exactly 1 (calls %+v)", n, stub.allCalls())
	}
	if n := stub.calledCount("PUT", "/v1/agents/agt_1/grants/secrets/sec_1"); n != 2 {
		t.Fatalf("the repeat attach issued %d PUTs, want 2", n)
	}
	// Nor did any of the three attaches rename: the identity was created with
	// the repo's current name, so every later ensure finds it already right.
	// The rename is a heal for drift, never a write per click.
	if n := stub.calledCount("PATCH", "/v1/agents/agt_1"); n != 0 {
		t.Errorf("the attaches issued %d renames of an already-current name, want none", n)
	}

	// And the read now reports both.
	resp = x.do("GET", grantsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if grants := entriesOf(t, decodeBody(t, resp), "grants"); len(grants) != 2 {
		t.Fatalf("grants after two attaches = %#v, want two", grants)
	}
}

// TestOneCLIGrantAttachHealsALegacyAgentName pins the other half of the attach
// path's #35 contract: the click hands EnsureAgent the repo's DISPLAY name, so
// it stomps whatever the identity was carrying — here the store id a pre-#35
// lab wrote — while the identifier the grants hang off does not move. Two
// things are bought at once and neither costs a call: the operator's existing
// grants keep the identity they are attached to (no second, grant-less copy),
// and the OneCLI dashboard row stops reading as a hex blob.
func TestOneCLIGrantAttachHealsALegacyAgentName(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	stub.seedPool([]stubPoolRow{{ID: "sec_1", Name: "DEPLOY_TOKEN", Provider: "generic"}}, nil)
	x, repo := newOneCLIGrantServer(t, stub)
	agentID := stub.seedAgent(agentKey(repo), repo.ID)

	resp := x.do("PUT", grantPath(repo, "secrets", "sec_1"), nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	if n := stub.calledCount("POST", "/v1/agents"); n != 0 {
		t.Fatalf("the attach created %d identities beside the legacy one (calls %+v)", n, stub.allCalls())
	}
	stub.call(t, "PUT", "/v1/agents/"+agentID+"/grants/secrets/sec_1")
	if got := stub.agentName(agentKey(repo)); got != repo.Name {
		t.Errorf("agent display name = %q, want the attach to have healed it to the repo's name %q", got, repo.Name)
	}
	// The rename carries the name and NOTHING else — upstream's identifier is
	// immutable, so the one write lab aims at an existing identity is
	// structurally incapable of moving that identity's match key.
	rename := stub.call(t, "PATCH", "/v1/agents/"+agentID)
	if !strings.Contains(rename.Body, `"name":"`+repo.Name+`"`) {
		t.Errorf("rename body = %q, want it to carry the repo's display name", rename.Body)
	}
	if strings.Contains(rename.Body, "identifier") {
		t.Errorf("rename body = %q, want it to say nothing about the identifier", rename.Body)
	}
}

// --- detach -----------------------------------------------------------------

// TestOneCLIGrantDetach pins the revoke path, including the case that must
// never create anything: detaching from a repo that has no agent identity at
// all is a 204, because the state the caller asked for already holds.
func TestOneCLIGrantDetach(t *testing.T) {
	t.Run("existing identity", func(t *testing.T) {
		stub := newOneCLIGrantStub(t)
		stub.seedPool([]stubPoolRow{{ID: "sec_1", Name: "DEPLOY_TOKEN", Provider: "generic"}}, nil)
		x, repo := newOneCLIGrantServer(t, stub)
		agentID := stub.seedAgent(agentKey(repo), repo.Name)
		stub.seedGrant(agentKey(repo), "secrets", "sec_1")

		resp := x.do("DELETE", grantPath(repo, "secrets", "sec_1"), nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusNoContent)
		if body := rawBody(t, resp); body != "" {
			t.Errorf("204 carried a body: %q", body)
		}
		stub.call(t, "DELETE", "/v1/agents/"+agentID+"/grants/secrets/sec_1")
		if stub.hasGrant(agentKey(repo), "secrets", "sec_1") {
			t.Fatal("the grant survived the detach")
		}
		if n := stub.calledCount("POST", "/v1/agents"); n != 0 {
			t.Fatalf("the detach created %d agent identities", n)
		}
	})

	t.Run("no identity yet", func(t *testing.T) {
		stub := newOneCLIGrantStub(t)
		x, repo := newOneCLIGrantServer(t, stub)

		resp := x.do("DELETE", grantPath(repo, "connections", "con_1"), nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusNoContent)
		_ = resp.Body.Close()
		if n := stub.calledCount("POST", "/v1/agents"); n != 0 {
			t.Fatalf("a detach created %d agent identities (calls %+v)", n, stub.allCalls())
		}
		// Nothing was addressed either: there was no identity to address.
		for _, c := range stub.allCalls() {
			if c.Method == "DELETE" {
				t.Fatalf("a detach against a repo with no identity still issued %+v", c)
			}
		}
	})
}

// --- validation -------------------------------------------------------------

// TestOneCLIGrantInvalidKind pins the edge check: a kind is a path segment in
// OneCLI's own URL, so anything but the two known words is refused at lab's
// edge, before a caller's string has steered any request at all.
func TestOneCLIGrantInvalidKind(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	x, repo := newOneCLIGrantServer(t, stub)

	for _, kind := range []string{"bogus", "secret", "Secrets", "..%2f..%2fagents"} {
		for _, method := range []string{"PUT", "DELETE"} {
			resp := x.do(method, grantPath(repo, kind, "sec_1"), nil, csrfHeaders(x.ts.URL))
			wantStatus(t, resp, http.StatusBadRequest)
			if msg := wantErrorBody(t, resp); !strings.Contains(msg, "grant kind") {
				t.Errorf("%s kind=%q error = %q, want it to name the offending kind", method, kind, msg)
			}
		}
	}
	if n := stub.totalCalls(); n != 0 {
		t.Fatalf("a rejected kind still reached OneCLI %d times: %+v", n, stub.allCalls())
	}

	// A URL truncated to the kind addresses an agent identity's whole grants
	// collection in OneCLI's grammar, so it must never reach OneCLI. It does
	// not match the route's resource wildcard at all and lands on the API
	// tree's JSON 404 — pinned here because the interesting property is "no
	// request was issued", not which layer refused.
	for _, method := range []string{"PUT", "DELETE"} {
		resp := x.do(method, grantsPath(repo)+"/secrets/", nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusNotFound)
		_ = resp.Body.Close()
	}
	if n := stub.totalCalls(); n != 0 {
		t.Fatalf("a truncated grant URL reached OneCLI %d times: %+v", n, stub.allCalls())
	}
}

// TestOneCLIGrantUnknownRepo pins the per-repo routes' 404: the path names a
// repo, and answering "here are no grants" for one that does not exist would
// hide a stale link behind a healthy-looking screen.
func TestOneCLIGrantUnknownRepo(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	x, _ := newOneCLIGrantServer(t, stub)

	resp := x.do("GET", "/api/v1/repos/repo_missing/onecli/grants", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	resp = x.do("PUT", "/api/v1/repos/repo_missing/onecli/grants/secrets/sec_1", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	if n := stub.totalCalls(); n != 0 {
		t.Fatalf("an unknown repo still reached OneCLI %d times: %+v", n, stub.allCalls())
	}
}

// --- auth -------------------------------------------------------------------

// TestOneCLIGrantRoutesRequireAuth proves requireAuth is really on all four
// routes: an operator exists (so this is not "nobody is set up yet"), the
// second client carries no session, and every route answers 401 — never the
// answer the same request gets with a session.
func TestOneCLIGrantRoutesRequireAuth(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	x, repo := newOneCLIGrantServer(t, stub) // x.client's jar now holds a session

	routes := []struct {
		method, path string
		authed       int
	}{
		{"GET", "/api/v1/onecli/pool", http.StatusOK},
		{"GET", grantsPath(repo), http.StatusOK},
		{"PUT", grantPath(repo, "secrets", "sec_1"), http.StatusNoContent},
		{"DELETE", grantPath(repo, "secrets", "sec_1"), http.StatusNoContent},
	}
	for _, rt := range routes {
		resp := doWith(t, http.DefaultClient, x.ts.URL, rt.method, rt.path, nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusUnauthorized)
		_ = resp.Body.Close()

		// The same request WITH the session works: the 401 above was the auth
		// guard, not a missing route.
		resp = x.do(rt.method, rt.path, nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, rt.authed)
		_ = resp.Body.Close()
	}
}

// --- the token that must never be served ------------------------------------

// TestOneCLIGrantResponsesNeverCarryTheProxyToken is the hygiene assertion the
// whole file's stub exists for. Every agent identity the stub lists carries a
// live gateway credential; no response body of any endpoint, in any state, may
// contain it. The handlers hold an identity's ID and nothing else — this test
// is what keeps that true when someone later "just adds the agent identity to
// the payload for debugging".
func TestOneCLIGrantResponsesNeverCarryTheProxyToken(t *testing.T) {
	stub := newOneCLIGrantStub(t)
	stub.seedPool(
		[]stubPoolRow{{ID: "sec_1", Name: "DEPLOY_TOKEN", Provider: "generic"}},
		[]stubPoolRow{{ID: "con_1", Name: "Acme GitHub", Provider: "github"}},
	)
	x, repo := newOneCLIGrantServer(t, stub)
	stub.seedAgent(agentKey(repo), repo.Name)
	stub.seedGrant(agentKey(repo), "secrets", "sec_1")

	requests := []struct {
		method, path string
		headers      map[string]string
	}{
		{"GET", "/api/v1/onecli/pool", nil},
		{"GET", grantsPath(repo), nil},
		{"PUT", grantPath(repo, "connections", "con_1"), csrfHeaders(x.ts.URL)},
		{"DELETE", grantPath(repo, "connections", "con_1"), csrfHeaders(x.ts.URL)},
		// The failure paths too: an error body is the classic place a
		// credential-bearing upstream answer gets echoed back by accident.
		{"PUT", grantPath(repo, "bogus", "con_1"), csrfHeaders(x.ts.URL)},
		{"GET", "/api/v1/repos/repo_missing/onecli/grants", nil},
	}
	for _, rq := range requests {
		body := rawBody(t, x.do(rq.method, rq.path, nil, rq.headers))
		if strings.Contains(body, testProxyToken) {
			t.Fatalf("%s %s served the agent identity's proxy token: %s", rq.method, rq.path, body)
		}
	}

	// Same again with a sidecar that fails every call, so the 502 bodies are
	// covered as well.
	stub.fail(http.StatusInternalServerError)
	for _, rq := range requests {
		body := rawBody(t, x.do(rq.method, rq.path, nil, rq.headers))
		if strings.Contains(body, testProxyToken) {
			t.Fatalf("%s %s served the proxy token in a failure body: %s", rq.method, rq.path, body)
		}
	}
}
