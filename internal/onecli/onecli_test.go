package onecli

// The client's spec, as a set of decision tables.
//
// Every test here runs against an httptest stub, never a live OneCLI sidecar
// and never the network — the package is a transport, and what it must get
// right (the method, the path, the headers, the error mapping, the
// no-duplicate-agent rule) is fully observable from the server side.
//
// One honest caveat, worth stating where the tests live: the stubs answer with
// the wire shapes declared in wire.go, so these tests prove the client is
// SELF-CONSISTENT with its assumptions about OneCLI's JSON — not that those
// assumptions match a real OneCLI build. wire.go says the same thing in more
// detail and is the single place to correct if they turn out to differ.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// testAPIKey is deliberately distinctive so a leak assertion cannot pass by
// accident: any error string containing this substring has spilled the
// credential.
const testAPIKey = "oc_proj_TEST_SECRET_KEY_do_not_leak"

// recordedRequest is what the stub saw, reduced to the parts that are contract.
type recordedRequest struct {
	Method string
	Path   string // escaped — a path-escaped id must stay escaped
	Query  string
	Header http.Header
	Body   string
}

// stub is an httptest server that records every request before delegating to
// the case's handler. The handler gets an intact r.Body (it is restored after
// recording), so a handler may decode the request it is answering.
type stub struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

func newStub(t *testing.T, h http.HandlerFunc) *stub {
	t.Helper()
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, recordedRequest{
			Method: r.Method,
			Path:   r.URL.EscapedPath(),
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   string(body),
		})
		s.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
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

// only returns the single request the stub saw, failing the test otherwise.
func (s *stub) only(t *testing.T) recordedRequest {
	t.Helper()
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("stub saw %d requests, want exactly 1: %+v", len(reqs), reqs)
	}
	return reqs[0]
}

// jsonHandler answers every request with one status and body.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func newTestClient(t *testing.T, baseURL string, tweak ...func(*Options)) *Client {
	t.Helper()
	opts := Options{BaseURL: baseURL, APIKey: testAPIKey}
	for _, f := range tweak {
		f(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New(%+v): %v", opts.BaseURL, err)
	}
	return c
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return parsed
}

// --- New -------------------------------------------------------------------

func TestNewRejectsBadOptions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     Options
		wantWord string // a phrase the error must carry, so the diagnosis is the right one
	}{
		{"empty base url", Options{APIKey: testAPIKey}, "BaseURL is required"},
		{"blank base url", Options{BaseURL: "   ", APIKey: testAPIKey}, "BaseURL is required"},
		{"empty api key", Options{BaseURL: "http://127.0.0.1:10254"}, "APIKey is required"},
		{"non-http scheme", Options{BaseURL: "ftp://127.0.0.1:10254", APIKey: testAPIKey}, "http(s)"},
		{"unix scheme", Options{BaseURL: "unix:///run/onecli.sock", APIKey: testAPIKey}, "http(s)"},
		{"no scheme", Options{BaseURL: "127.0.0.1:10254", APIKey: testAPIKey}, "http(s)"},
		{"unparseable url", Options{BaseURL: "http://[::1", APIKey: testAPIKey}, "not a valid"},
		{"no host", Options{BaseURL: "http:///v1", APIKey: testAPIKey}, "must include a host"},
		{"api key with newline", Options{BaseURL: "http://127.0.0.1:10254", APIKey: "abc\ndef"}, "single line"},
		{"api key with NUL", Options{BaseURL: "http://127.0.0.1:10254", APIKey: "abc\x00def"}, "single line"},
		{"project id with CR", Options{BaseURL: "http://127.0.0.1:10254", APIKey: testAPIKey, ProjectID: "p\r1"}, "single line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.opts)
			if err == nil {
				t.Fatalf("New succeeded, want refusal (client %v)", c)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not mention %q", err, tc.wantWord)
			}
			if strings.Contains(err.Error(), tc.opts.APIKey) && tc.opts.APIKey != "" {
				t.Errorf("error %q echoes the API key", err)
			}
		})
	}
}

func TestNewAcceptsValidOptions(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:10254",
		"https://api.onecli.sh",
		"https://api.onecli.sh/v1",
		"  http://127.0.0.1:10254/v1/  ",
	} {
		if _, err := New(Options{BaseURL: base, APIKey: testAPIKey}); err != nil {
			t.Errorf("New(%q): %v", base, err)
		}
	}
}

// TestBaseURLSpellingsResolveToOneAPIRoot pins the normalization contract: the
// spellings an operator plausibly writes in a config file all address the same
// endpoint, and none of them produces //agents or /v1/v1/agents.
func TestBaseURLSpellingsResolveToOneAPIRoot(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusOK, `{"agents":[]}`))

	for _, suffix := range []string{"", "/", "/v1", "/v1/", "//", "/v1//"} {
		t.Run("base"+suffix, func(t *testing.T) {
			c := newTestClient(t, s.URL+suffix)
			if _, err := c.ListAgents(context.Background()); err != nil {
				t.Fatalf("ListAgents: %v", err)
			}
			reqs := s.requests()
			got := reqs[len(reqs)-1].Path
			if got != "/v1/agents" {
				t.Errorf("base %q hit path %q, want /v1/agents", s.URL+suffix, got)
			}
		})
	}
}

// TestBaseURLKeepsReverseProxySubpath: a base behind a reverse proxy mount
// point keeps its prefix, and still gets exactly one /v1.
func TestBaseURLKeepsReverseProxySubpath(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusOK, `{"agents":[]}`))
	c := newTestClient(t, s.URL+"/onecli/")
	if _, err := c.ListAgents(context.Background()); err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if got := s.only(t).Path; got != "/onecli/v1/agents" {
		t.Errorf("path = %q, want /onecli/v1/agents", got)
	}
}

// --- headers ---------------------------------------------------------------

// TestEveryRequestCarriesAuthAndAccept walks the whole operation surface and
// asserts the three-header contract on each one — including Health, which the
// API does not require auth for and which is therefore the easiest one to
// accidentally special-case.
func TestEveryRequestCarriesAuthAndAccept(t *testing.T) {
	for _, tc := range operationCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(tc.status, tc.respBody))
			c := newTestClient(t, s.URL)
			if _, err := tc.call(context.Background(), c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			req := s.only(t)
			if got, want := req.Header.Get("Authorization"), "Bearer "+testAPIKey; got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
			if got := req.Header.Get("Accept"); got != "application/json" {
				t.Errorf("Accept = %q, want application/json", got)
			}
			if got := req.Header.Get("X-Project-Id"); got != "" {
				t.Errorf("X-Project-Id = %q, want it absent when ProjectID is unset", got)
			}
		})
	}
}

func TestProjectIDHeaderPresentOnlyWhenConfigured(t *testing.T) {
	for _, tc := range []struct {
		name      string
		projectID string
		want      string
	}{
		{"configured", "proj_42", "proj_42"},
		{"unset", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(http.StatusOK, `{"agents":[]}`))
			c := newTestClient(t, s.URL, func(o *Options) { o.ProjectID = tc.projectID })
			if _, err := c.ListAgents(context.Background()); err != nil {
				t.Fatalf("ListAgents: %v", err)
			}
			if got := s.only(t).Header.Get("X-Project-Id"); got != tc.want {
				t.Errorf("X-Project-Id = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- operations ------------------------------------------------------------

// operationCase is one operation's happy path: the request the stub must see,
// the answer it gives, and the value the client must produce from it.
type operationCase struct {
	name       string
	wantMethod string
	wantPath   string
	status     int
	respBody   string
	call       func(context.Context, *Client) (any, error)
	want       any // nil ⇒ the operation returns only an error; nothing to compare
}

func operationCases(t *testing.T) []operationCase {
	t.Helper()
	return []operationCase{
		{
			name: "Health", wantMethod: http.MethodGet, wantPath: "/v1/health",
			status: http.StatusOK, respBody: `{"status":"ok","version":"1.4.2"}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.Health(ctx) },
			want: Health{Status: "ok", Version: "1.4.2"},
		},
		{
			name: "ListAgents enveloped", wantMethod: http.MethodGet, wantPath: "/v1/agents",
			status: http.StatusOK, respBody: `{"agents":[{"id":"ag_1","name":"coding-lab"},{"id":"ag_2","name":"web"}]}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.ListAgents(ctx) },
			want: []Agent{{ID: "ag_1", Name: "coding-lab"}, {ID: "ag_2", Name: "web"}},
		},
		{
			name: "ListAgents bare array", wantMethod: http.MethodGet, wantPath: "/v1/agents",
			status: http.StatusOK, respBody: `[{"id":"ag_1","name":"coding-lab"}]`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.ListAgents(ctx) },
			want: []Agent{{ID: "ag_1", Name: "coding-lab"}},
		},
		{
			name: "ListAgents generic data envelope", wantMethod: http.MethodGet, wantPath: "/v1/agents",
			status: http.StatusOK, respBody: `{"data":[{"id":"ag_1","name":"coding-lab"}]}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.ListAgents(ctx) },
			want: []Agent{{ID: "ag_1", Name: "coding-lab"}},
		},
		{
			name: "ListSecrets", wantMethod: http.MethodGet, wantPath: "/v1/secrets",
			status: http.StatusOK, respBody: `{"secrets":[{"id":"sec_1","name":"DEPLOY_TOKEN","provider":"generic"}]}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.ListSecrets(ctx) },
			want: []Secret{{ID: "sec_1", Name: "DEPLOY_TOKEN", Provider: "generic"}},
		},
		{
			name: "ListConnections", wantMethod: http.MethodGet, wantPath: "/v1/connections",
			status: http.StatusOK, respBody: `{"connections":[{"id":"con_1","name":"GitHub","provider":"github"}]}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.ListConnections(ctx) },
			want: []Connection{{ID: "con_1", Name: "GitHub", Provider: "github"}},
		},
		{
			name: "ListGrants split shape", wantMethod: http.MethodGet, wantPath: "/v1/agents/ag_1/grants",
			status: http.StatusOK,
			respBody: `{"secrets":[{"id":"sec_1","name":"DEPLOY_TOKEN"}],` +
				`"connections":[{"id":"con_1","name":"GitHub"}]}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.ListGrants(ctx, "ag_1") },
			want: []Grant{
				{Kind: GrantSecret, ID: "sec_1", Name: "DEPLOY_TOKEN"},
				{Kind: GrantConnection, ID: "con_1", Name: "GitHub"},
			},
		},
		{
			name: "ListGrants flat shape with singular kinds", wantMethod: http.MethodGet, wantPath: "/v1/agents/ag_1/grants",
			status:   http.StatusOK,
			respBody: `[{"kind":"secret","id":"sec_1","name":"DEPLOY_TOKEN"},{"type":"connection","id":"con_1","name":"GitHub"}]`,
			call:     func(ctx context.Context, c *Client) (any, error) { return c.ListGrants(ctx, "ag_1") },
			want: []Grant{
				{Kind: GrantSecret, ID: "sec_1", Name: "DEPLOY_TOKEN"},
				{Kind: GrantConnection, ID: "con_1", Name: "GitHub"},
			},
		},
		{
			name: "ListGrants empty", wantMethod: http.MethodGet, wantPath: "/v1/agents/ag_1/grants",
			status: http.StatusOK, respBody: `{"secrets":[],"connections":[]}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.ListGrants(ctx, "ag_1") },
			want: []Grant{},
		},
		{
			name: "AttachGrant secret", wantMethod: http.MethodPut, wantPath: "/v1/agents/ag_1/grants/secrets/sec_1",
			status: http.StatusNoContent,
			call: func(ctx context.Context, c *Client) (any, error) {
				return nil, c.AttachGrant(ctx, "ag_1", GrantSecret, "sec_1")
			},
		},
		{
			name: "AttachGrant connection with 200", wantMethod: http.MethodPut, wantPath: "/v1/agents/ag_1/grants/connections/con_1",
			status: http.StatusOK, respBody: `{"ok":true}`,
			call: func(ctx context.Context, c *Client) (any, error) {
				return nil, c.AttachGrant(ctx, "ag_1", GrantConnection, "con_1")
			},
		},
		{
			name: "DetachGrant secret", wantMethod: http.MethodDelete, wantPath: "/v1/agents/ag_1/grants/secrets/sec_1",
			status: http.StatusNoContent,
			call: func(ctx context.Context, c *Client) (any, error) {
				return nil, c.DetachGrant(ctx, "ag_1", GrantSecret, "sec_1")
			},
		},
		{
			name: "DetachGrant connection", wantMethod: http.MethodDelete, wantPath: "/v1/agents/ag_1/grants/connections/con_1",
			status: http.StatusNoContent,
			call: func(ctx context.Context, c *Client) (any, error) {
				return nil, c.DetachGrant(ctx, "ag_1", GrantConnection, "con_1")
			},
		},
		{
			name: "AgentToken with snake_case expiry", wantMethod: http.MethodPost, wantPath: "/v1/agents/ag_1/token",
			status: http.StatusOK, respBody: `{"token":"oc_agent_abc","expires_at":"2026-09-01T10:30:00Z"}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.AgentToken(ctx, "ag_1") },
			want: AgentToken{Token: "oc_agent_abc", ExpiresAt: mustTime(t, "2026-09-01T10:30:00Z")},
		},
		{
			name: "AgentToken with camelCase expiry", wantMethod: http.MethodPost, wantPath: "/v1/agents/ag_1/token",
			status: http.StatusOK, respBody: `{"token":"oc_agent_abc","expiresAt":"2026-09-01T10:30:00Z"}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.AgentToken(ctx, "ag_1") },
			want: AgentToken{Token: "oc_agent_abc", ExpiresAt: mustTime(t, "2026-09-01T10:30:00Z")},
		},
		{
			name: "AgentToken without expiry", wantMethod: http.MethodPost, wantPath: "/v1/agents/ag_1/token",
			status: http.StatusOK, respBody: `{"token":"oc_agent_abc"}`,
			call: func(ctx context.Context, c *Client) (any, error) { return c.AgentToken(ctx, "ag_1") },
			want: AgentToken{Token: "oc_agent_abc"},
		},
	}
}

func TestOperationsHappyPath(t *testing.T) {
	for _, tc := range operationCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(tc.status, tc.respBody))
			c := newTestClient(t, s.URL)

			got, err := tc.call(context.Background(), c)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			req := s.only(t)
			if req.Method != tc.wantMethod {
				t.Errorf("method = %s, want %s", req.Method, tc.wantMethod)
			}
			if req.Path != tc.wantPath {
				t.Errorf("path = %s, want %s", req.Path, tc.wantPath)
			}
			if tc.want != nil && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("result = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestAttachGrantSendsNoBody pins wire.go assumption 7: the grant is addressed
// entirely by the URL, so no request body is sent (and therefore no
// Content-Type either).
func TestAttachGrantSendsNoBody(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusNoContent, ""))
	c := newTestClient(t, s.URL)
	if err := c.AttachGrant(context.Background(), "ag_1", GrantSecret, "sec_1"); err != nil {
		t.Fatalf("AttachGrant: %v", err)
	}
	req := s.only(t)
	if req.Body != "" {
		t.Errorf("request body = %q, want empty", req.Body)
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type = %q, want it absent on a bodyless request", ct)
	}
}

// TestIdentifiersArePathEscaped: an id carrying a slash must address one
// escaped path element, never traverse into a different endpoint. Without the
// escape, agent id "ag/../.." would rewrite the request's target.
func TestIdentifiersArePathEscaped(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusNoContent, ""))
	c := newTestClient(t, s.URL)
	if err := c.AttachGrant(context.Background(), "ag/1", GrantSecret, "sec/1"); err != nil {
		t.Fatalf("AttachGrant: %v", err)
	}
	if got, want := s.only(t).Path, "/v1/agents/ag%2F1/grants/secrets/sec%2F1"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestGrantWritesRejectUnknownKind: GrantKind doubles as a path segment, so an
// unvalidated kind would be caller-controlled routing. It must fail before any
// request leaves.
func TestGrantWritesRejectUnknownKind(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusNoContent, ""))
	c := newTestClient(t, s.URL)
	for _, kind := range []GrantKind{"", "secret", "../admin", "connection"} {
		if err := c.AttachGrant(context.Background(), "ag_1", kind, "sec_1"); err == nil {
			t.Errorf("AttachGrant with kind %q succeeded, want refusal", kind)
		}
		if err := c.DetachGrant(context.Background(), "ag_1", kind, "sec_1"); err == nil {
			t.Errorf("DetachGrant with kind %q succeeded, want refusal", kind)
		}
	}
	if reqs := s.requests(); len(reqs) != 0 {
		t.Errorf("stub saw %d requests, want none: %+v", len(reqs), reqs)
	}
}

func TestEmptyIdentifiersRejectedWithoutRequest(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusOK, `{}`))
	c := newTestClient(t, s.URL)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"ListGrants", func() error { _, err := c.ListGrants(ctx, ""); return err }},
		{"AgentToken", func() error { _, err := c.AgentToken(ctx, ""); return err }},
		{"AttachGrant no agent", func() error { return c.AttachGrant(ctx, "", GrantSecret, "sec_1") }},
		{"AttachGrant no resource", func() error { return c.AttachGrant(ctx, "ag_1", GrantSecret, "") }},
		{"DetachGrant no agent", func() error { return c.DetachGrant(ctx, "", GrantSecret, "sec_1") }},
		{"DetachGrant no resource", func() error { return c.DetachGrant(ctx, "ag_1", GrantSecret, "") }},
		{"EnsureAgent no name", func() error { _, err := c.EnsureAgent(ctx, ""); return err }},
	} {
		if err := tc.call(); err == nil {
			t.Errorf("%s with an empty identifier succeeded, want refusal", tc.name)
		}
	}
	if reqs := s.requests(); len(reqs) != 0 {
		t.Errorf("stub saw %d requests, want none: %+v", len(reqs), reqs)
	}
}

// --- error mapping ---------------------------------------------------------

func TestNon2xxBecomesAPIError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantMessage string
	}{
		{"401 error envelope", http.StatusUnauthorized, `{"error":"invalid api key"}`, "invalid api key"},
		{"404 message envelope", http.StatusNotFound, `{"message":"agent not found"}`, "agent not found"},
		{"500 plain text", http.StatusInternalServerError, "upstream exploded", "upstream exploded"},
		{"502 html from a stray proxy", http.StatusBadGateway, "<html><body>nginx</body></html>", "<html><body>nginx</body></html>"},
		{"409 empty body", http.StatusConflict, "", "(empty body)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(tc.status, tc.body))
			c := newTestClient(t, s.URL)

			_, err := c.ListAgents(context.Background())
			if err == nil {
				t.Fatal("ListAgents succeeded, want an error")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %v (%T) is not an *APIError", err, err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			if apiErr.Method != http.MethodGet {
				t.Errorf("Method = %q, want GET", apiErr.Method)
			}
			if apiErr.Path != "/v1/agents" {
				t.Errorf("Path = %q, want /v1/agents", apiErr.Path)
			}
			if apiErr.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", apiErr.Message, tc.wantMessage)
			}
			msg := apiErr.Error()
			for _, want := range []string{"GET", "/v1/agents", fmt.Sprint(tc.status), tc.wantMessage} {
				if !strings.Contains(msg, want) {
					t.Errorf("Error() = %q, missing %q", msg, want)
				}
			}
		})
	}
}

// TestAPIErrorNeverCarriesTheAPIKey is the secret-hygiene assertion: the key
// lives only in a request header, so no error the package builds can contain
// it — including when the server helpfully echoes the Authorization header
// back in its error body (the bounded snippet is the mitigation for the body
// itself carrying a credential; here the key must simply not be there).
func TestAPIErrorNeverCarriesTheAPIKey(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		s := newStub(t, jsonHandler(status, `{"error":"denied"}`))
		c := newTestClient(t, s.URL)

		if _, err := c.ListAgents(context.Background()); err == nil {
			t.Fatalf("status %d: ListAgents succeeded, want an error", status)
		} else if strings.Contains(err.Error(), testAPIKey) {
			t.Errorf("status %d: error %q contains the API key", status, err)
		}
	}
}

// TestClientPrintsRedacted: %v on a *Client is the classic accidental leak
// (it would otherwise print every field, unexported ones included). Client's
// String method makes that print redacted instead. %#v is NOT covered — no
// Go type can defend against it — which is why the package doc says not to
// log an Options either.
func TestClientPrintsRedacted(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:10254", func(o *Options) { o.ProjectID = "proj_42" })
	for _, rendered := range []string{
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%+v", c),
		c.String(),
		fmt.Sprint(*c),
	} {
		if strings.Contains(rendered, testAPIKey) {
			t.Errorf("rendered client %q contains the API key", rendered)
		}
		if !strings.Contains(rendered, "REDACTED") {
			t.Errorf("rendered client %q does not say REDACTED", rendered)
		}
	}
}

// TestTokenErrorsNeverEchoTheBody: the token endpoint's body IS a credential,
// so the two failure paths that inspect it must describe the shape mismatch
// without quoting what came back.
func TestTokenErrorsNeverEchoTheBody(t *testing.T) {
	for _, tc := range []struct {
		name, body, secret string
	}{
		{"no token field", `{"credential":"oc_agent_LEAKED"}`, "oc_agent_LEAKED"},
		{"unparseable expiry", `{"token":"oc_agent_ok","expires_at":"oc_agent_LEAKED"}`, "oc_agent_LEAKED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, jsonHandler(http.StatusOK, tc.body))
			c := newTestClient(t, s.URL)

			_, err := c.AgentToken(context.Background(), "ag_1")
			if err == nil {
				t.Fatal("AgentToken succeeded, want an error")
			}
			if strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error %q echoes the response body", err)
			}
			if !strings.Contains(err.Error(), "internal/onecli/wire.go") {
				t.Errorf("error %q does not point at wire.go, the one place to fix a wire mismatch", err)
			}
		})
	}
}

// TestUnknownListShapeIsLoud: a shape this package cannot read must never look
// like an empty pool — the grant picker built on these lists would show
// "no secrets" with no explanation.
func TestUnknownListShapeIsLoud(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusOK, `{"unexpected":{"nested":"shape"}}`))
	c := newTestClient(t, s.URL)

	got, err := c.ListSecrets(context.Background())
	if err == nil {
		t.Fatalf("ListSecrets succeeded with %#v, want a loud error", got)
	}
	if !strings.Contains(err.Error(), "internal/onecli/wire.go") {
		t.Errorf("error %q does not point at wire.go", err)
	}
}

// TestUnknownGrantKindIsLoud: a grant row lab cannot classify must not be
// silently dropped from a list an operator makes access decisions from.
func TestUnknownGrantKindIsLoud(t *testing.T) {
	for _, body := range []string{
		`[{"kind":"ssh-key","id":"x","name":"n"}]`,
		`[{"id":"x","name":"n"}]`,
	} {
		s := newStub(t, jsonHandler(http.StatusOK, body))
		c := newTestClient(t, s.URL)
		if got, err := c.ListGrants(context.Background(), "ag_1"); err == nil {
			t.Errorf("ListGrants(%s) succeeded with %#v, want a loud error", body, got)
		}
	}
}

// --- context ---------------------------------------------------------------

// TestContextCancellationPropagates: lab cancels spawn work routinely, and a
// wedged sidecar must not outlive the caller's context. The stub blocks until
// the request context is done, so the only way this returns is cancellation.
func TestContextCancellationPropagates(t *testing.T) {
	started := make(chan struct{})
	s := newStub(t, func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	c := newTestClient(t, s.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	_, err := c.ListAgents(ctx)
	if err == nil {
		t.Fatal("ListAgents succeeded, want a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not unwrap to context.Canceled", err)
	}
}

func TestContextDeadlinePropagates(t *testing.T) {
	s := newStub(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	c := newTestClient(t, s.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.Health(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not unwrap to context.DeadlineExceeded", err)
	}
}

// jsonEncode is a test helper for stub responses built from Go values.
func jsonEncode(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
