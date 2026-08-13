// Package onecli is lab's client for the OneCLI credential gateway
// (github.com/onecli/onecli, https://onecli.sh) — the sidecar that will own
// lab's repo secrets once the gateway epics land (issue #23). OneCLI runs two
// listeners: 10254 serves the dashboard and the REST API this package speaks,
// 10255 is the gateway proxy that runs will point HTTPS_PROXY at so an
// instance never holds a real secret value. This package speaks ONLY to the
// first; ProbeGateway is the one thing here that touches the second, and it
// touches it at the TCP level only.
//
// Scope is deliberately the subset issue #23's later epics consume — health,
// the idempotent per-repo agent mapping, the project's secret/connection pool,
// an agent's grants, and an agent's proxy token. It is not a full binding of
// OneCLI's API and should not grow into one: every endpoint added here is one
// more undocumented wire shape lab has to keep true (see below).
//
// # The wire shapes are an assumption, and wire.go is where to fix them
//
// OneCLI community edition documents its REST SURFACE — base URL, bearer auth,
// the X-Project-Id context header, and the paths — at https://onecli.sh/docs
// and https://onecli.sh/docs/llms.txt. It does NOT publish the JSON field
// names of its responses, and this package was written without a live
// instance to read them off. Every request/response struct in this package is
// therefore a good-faith reading of that surface, not a verified contract, and
// every one of them — together with every path — lives in wire.go so that a
// mismatch against a real OneCLI build is a one-file fix. wire.go carries an
// explicit list of the assumptions made. Nothing in this package pretends to
// a "spec version": there is no published schema to pin.
//
// # Secret hygiene
//
// The API key is a credential. It travels ONLY in the Authorization header —
// never in a URL, never in a query parameter — so an *APIError, which carries
// the method, path and status, structurally cannot contain it. Response bodies
// folded into errors are capped at errBodySnippetMax because a body from a
// token endpoint could itself contain a credential. Client deliberately has a
// redacting String method (see below) so that a caller who prints it with %v
// cannot spill the key into a log.
package onecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// defaultTimeout bounds a request when the caller supplies no HTTPClient.
	// OneCLI is a loopback (or same-host) sidecar, so a call that has not
	// answered in this long is wedged, not slow — and lab must not park a
	// spawn on it. Callers that need tighter control pass their own client or
	// a ctx deadline; both win over this.
	defaultTimeout = 30 * time.Second

	// maxResponseBody bounds every body read. The endpoints bound here return
	// small documents (a health object, a project's agent/secret lists), so a
	// body larger than this is a misconfiguration — something else answering on
	// the API port — and must not be read into memory unbounded.
	maxResponseBody = 1 << 20 // 1 MiB

	// errBodySnippetMax caps how much of a non-2xx body is folded into an
	// *APIError. It is a diagnostic aid, not a transcript: POST /agents/{id}/token
	// answers with a credential, and an error path on such an endpoint must not
	// be able to spill an unbounded body into lab's logs.
	errBodySnippetMax = 512
)

// Options configures New. It is a plain value so a caller can build it from
// flags/config, but note that APIKey is a credential: never log an Options,
// and never print it with %v.
type Options struct {
	// BaseURL is the OneCLI API origin — http://127.0.0.1:10254 for the
	// self-hosted sidecar, https://api.onecli.sh for the cloud service. The
	// "/v1" API root is appended by New if the URL does not already end in it,
	// so all four spellings a config file realistically carries
	// (…:10254, …:10254/, …:10254/v1, …:10254/v1/) resolve to the same base.
	BaseURL string

	// APIKey authenticates every request as "Authorization: Bearer <key>".
	// Self-hosted project keys look like oc_proj_*; the client does not check
	// the shape, only that it is a single line (it rides in an HTTP header).
	APIKey string

	// ProjectID, when non-empty, is sent as X-Project-Id on every request —
	// OneCLI's multi-project context header. Lab's model is one OneCLI project
	// for the whole lab (issue #23), so this is normally empty and the key's
	// own project is the context.
	ProjectID string

	// HTTPClient overrides the transport (tests inject httptest's). nil gets a
	// client with defaultTimeout.
	HTTPClient *http.Client
}

// Client is a OneCLI REST client bound to one API base and one API key.
// It is safe for concurrent use: it is immutable after New and holds no state
// beyond its *http.Client, which is itself concurrency-safe. That matters —
// lab calls EnsureAgent once per repo at spawn, concurrently.
type Client struct {
	httpClient *http.Client
	base       *url.URL // normalized API root, path ends in /v1, no trailing slash
	apiKey     string   // Bearer credential; header-only, never in a URL or an error
	projectID  string
}

// String renders the client WITHOUT its API key. It exists precisely so that
// fmt's %v on a *Client — the classic accidental credential leak, since %v on
// a struct pointer prints every field including unexported ones — prints a
// redacted line instead of the bearer token (issue #23's secret-hygiene rule).
// The receiver is a value so that both Client and *Client are covered.
func (c Client) String() string {
	return fmt.Sprintf("onecli.Client{base:%s, project:%q, apiKey:REDACTED}", c.baseString(), c.projectID)
}

// baseString renders the API root for messages, tolerating the zero Client
// (String must never panic — it is reached from logging paths).
func (c Client) baseString() string {
	if c.base == nil {
		return "(unset)"
	}
	return c.base.String()
}

// New validates opts and returns a client whose API root is already resolved.
// An empty BaseURL or APIKey, a BaseURL that is not parseable or not http(s),
// and a key or project id carrying CR/LF/NUL are all refused HERE rather than
// at the first call: lab reads these from flags at startup, and a
// misconfiguration must be a loud startup error, not a confusing 400 during a
// spawn hours later. The CR/LF/NUL rule is the same single-line rule vault
// applies to header/askpass-bound credentials (ADR-0006) — an embedded newline
// in a header value is a request-smuggling shape, and Go's transport would
// reject it far from the cause.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("onecli: BaseURL is required (e.g. http://127.0.0.1:10254)")
	}
	if opts.APIKey == "" {
		return nil, errors.New("onecli: APIKey is required")
	}
	if strings.ContainsAny(opts.APIKey, "\r\n\x00") {
		// Never echo the key, not even a prefix of it.
		return nil, errors.New("onecli: APIKey must be a single line without control characters")
	}
	if strings.ContainsAny(opts.ProjectID, "\r\n\x00") {
		return nil, errors.New("onecli: ProjectID must be a single line without control characters")
	}
	base, err := apiRoot(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		httpClient: httpClient,
		base:       base,
		apiKey:     opts.APIKey,
		projectID:  opts.ProjectID,
	}, nil
}

// apiRoot normalizes a configured base URL to the API root every request path
// hangs off: scheme://host[:port]/…/v1, with no trailing slash. It is the
// single place URL shape is decided, so that path building afterwards is exact
// concatenation of segments and can never double a slash or drop one.
//
// The four spellings an operator plausibly writes — with and without a
// trailing slash, with and without the /v1 the docs show in examples — all
// collapse here. A URL that already ends in /v1 is left alone rather than
// getting a second one (the failure this normalization exists to prevent is a
// silent 404 on /v1/v1/agents).
func apiRoot(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// url.Parse's own error embeds the raw URL, which may carry userinfo
		// credentials; report only the reason. The message names the expected
		// shape because the most common way to land here is a base written
		// without a scheme (127.0.0.1:10254), which url.Parse rejects with a
		// message about path segments that explains nothing to an operator.
		return nil, fmt.Errorf("onecli: BaseURL is not a valid http(s) URL (e.g. http://127.0.0.1:10254): %w", urlErrReason(err))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("onecli: BaseURL must be an http(s) URL, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("onecli: BaseURL must include a host (e.g. http://127.0.0.1:10254)")
	}
	// Query and fragment are meaningless on an API root and would ride along on
	// every built URL; drop them rather than silently smuggling them into calls.
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	// Normalize to an absolute path with no trailing slash. The leading slash is
	// not cosmetic: url.URL.JoinPath returns a RELATIVE path when the receiver's
	// path is empty, which still renders correctly in a request URL but makes
	// EscapedPath() — the string *APIError reports — come out as "v1/agents".
	u.Path = "/" + strings.Trim(u.Path, "/")
	u.RawPath = ""
	if !strings.HasSuffix(u.Path, "/"+apiVersionSegment) {
		u = u.JoinPath(apiVersionSegment)
	}
	return u, nil
}

// urlErrReason unwraps a *url.Error to its underlying reason, discarding the
// URL the error would otherwise quote back. A configured URL can legitimately
// carry userinfo (http://user:pass@host), so the raw string is treated as
// potentially secret-bearing everywhere in this package.
func urlErrReason(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// APIError is a non-2xx answer from the OneCLI API. It carries the machine
// readable status alongside the request that produced it so callers can branch
// (EnsureAgent branches on 409; a caller distinguishing "not configured" from
// "unauthorized" branches on 401 vs 404) via errors.As.
//
// Its Error() names the method, path, status and the server's message and
// NOTHING else — in particular never the API key, which only ever exists in a
// request header, never in the path this type stores.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Message    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "(no message)"
	}
	return fmt.Sprintf("onecli %s %s: unexpected status %d: %s", e.Method, e.Path, e.StatusCode, msg)
}

// newAPIError builds the error for a non-2xx response, reading a bounded
// snippet of the body for the message. It prefers the server's own message
// field when the body is the JSON error envelope, and falls back to a
// whitespace-collapsed snippet of whatever did come back (an HTML page from
// something else listening on the port is the common real-world case).
func newAPIError(method, path string, resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodySnippetMax))
	return &APIError{
		StatusCode: resp.StatusCode,
		Method:     method,
		Path:       path,
		Message:    errorMessage(body),
	}
}

// errorMessage extracts a human message from an error body: the JSON envelope's
// error/message field when present, otherwise a collapsed snippet. The snippet
// is already bounded by the caller's LimitReader — the bound is the point, since
// a body on a token endpoint could contain a credential.
func errorMessage(body []byte) string {
	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.Error != "" {
			return envelope.Error
		}
		if envelope.Message != "" {
			return envelope.Message
		}
	}
	s := strings.Join(strings.Fields(string(body)), " ")
	if s == "" {
		return "(empty body)"
	}
	return s
}

// isConflict reports whether err is the API's 409 — the lost-the-race answer
// EnsureAgent must recognize rather than surface.
func isConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// do performs one REST call and returns the 2xx body, bounded. It is the ONLY
// place a request is built, so every request in this package carries the same
// three headers by construction:
//
//   - Authorization: Bearer <key> — on every call including GET /health, which
//     does not require auth. Sending it anyway keeps one code path; there is no
//     "unauthenticated request" shape in this client to get subtly wrong.
//   - Accept: application/json.
//   - X-Project-Id — only when a project id is configured. Sending an empty
//     one would be a different request than sending none.
//
// A 204 (attach/detach) returns a nil body, which the callers treat as success.
// A non-2xx returns *APIError. ctx governs the whole call; a cancellation
// surfaces wrapped, so errors.Is(err, context.Canceled) holds for callers.
func (c *Client) do(ctx context.Context, method string, u *url.URL, reqBody any) ([]byte, error) {
	path := u.EscapedPath()

	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("onecli %s %s: encode request: %w", method, path, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("onecli %s %s: build request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if c.projectID != "" {
		req.Header.Set("X-Project-Id", c.projectID)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// http.Client wraps transport failures in *url.Error, whose URL field is
		// this request's URL — safe (the key is header-only), but the reason is
		// what the operator needs, and unwrapping keeps errors.Is against
		// context.Canceled/DeadlineExceeded working for the caller.
		return nil, fmt.Errorf("onecli %s %s: %w", method, path, urlErrReason(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(method, path, resp)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("onecli %s %s: read response: %w", method, path, err)
	}
	return data, nil
}
