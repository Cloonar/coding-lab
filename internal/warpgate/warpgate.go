// Package warpgate is lab's client for the Warpgate SSH bastion
// (github.com/warp-tech/warpgate) — the sidecar that lets a run reach an
// operator's SSH targets without ever holding a target credential (issue #39,
// ADR-0068). Warpgate serves its admin REST API and a small gateway API on
// one HTTPS listener (8888 by default) and the SSH bastion on another (2222).
// This package speaks to the first over HTTPS and touches the second only to
// read its host keys (hostkey.go); it never authenticates to the bastion.
//
// Scope is deliberately the subset lab consumes — health, the idempotent
// per-repo identity (one Warpgate user and one role per lab repo, both named
// RepoSlug(repoID)), SSH target listing and target↔role assignment, a user's
// public-key credentials (one ephemeral key per run), and the SSH host-key
// pin. It is not a full binding of Warpgate's admin API and should not grow
// into one: every endpoint added here is one more wire shape lab has to keep
// true (see below).
//
// # The wire shapes live in wire.go, and wire.go is where to fix them
//
// Every URL path and every JSON field name this package sends or reads lives
// in wire.go, verified against the Warpgate 0.29.1 source (file references in
// its header), so that a mismatch against a future Warpgate build is a
// one-file fix. The operation files (identity.go, targets.go, keys.go,
// health.go) carry only lab-side semantics — never a literal path or a struct
// tag.
//
// # Secret hygiene
//
// The admin token is a credential with authority over every Warpgate user and
// target. It travels ONLY in the X-Warpgate-Token header — never in a URL,
// never in a query parameter — so an *APIError, which carries the method,
// path and status, structurally cannot contain it. Client has a redacting
// String method so a caller that prints it with %v cannot spill the token.
//
// Warpgate's target endpoints answer with each target's stored credentials
// (SSH passwords among them). This package decodes ONLY a target's id, name,
// description and options.kind — the decoder never materializes anything
// else — and never folds a targets response body into an error, not even a
// capped snippet. Every other error body is capped at errBodySnippetMax.
package warpgate

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// defaultTimeout bounds a request when the caller supplies no HTTPClient.
	// Warpgate is a same-host (usually loopback) sidecar, so a call that has
	// not answered in this long is wedged, not slow — and lab must not park a
	// spawn on it. A caller's ctx deadline wins over this.
	defaultTimeout = 30 * time.Second

	// maxResponseBody bounds every body read. It is larger than OneCLI's
	// because GET /targets answers every target in full (options included),
	// and an operator with a few thousand targets is plausible; a body beyond
	// this is a misconfiguration — something else answering on the port — and
	// is refused loudly rather than read into memory or silently truncated.
	maxResponseBody = 4 << 20 // 4 MiB

	// errBodySnippetMax caps how much of a non-2xx body is folded into an
	// *APIError. It is a diagnostic aid, not a transcript: an error path must
	// not be able to spill an unbounded body into lab's logs.
	errBodySnippetMax = 512
)

// Options configures New. Token is a credential: never log an Options, and
// never print it with %v.
type Options struct {
	// BaseURL is Warpgate's HTTPS origin, e.g. https://localhost:8888. A path
	// prefix is kept (Warpgate mounted behind a reverse proxy at /warpgate/
	// stays /warpgate/), while a trailing slash and a pasted Warpgate API path
	// (/@warpgate, /@warpgate/api, /@warpgate/admin/api and their /_warpgate
	// aliases) are normalized away, so every realistic spelling resolves to
	// the same admin API root.
	BaseURL string

	// Token authenticates every request as "X-Warpgate-Token: <token>" —
	// Warpgate's static admin token (warpgate run --enable-admin-token with
	// WARPGATE_ADMIN_TOKEN) or a Warpgate admin's per-user API token. It must
	// be a single line (it rides in an HTTP header) and is never logged.
	Token string

	// CAFile, when set, is a PEM file whose CERTIFICATE blocks are the ONLY
	// roots the client trusts — Warpgate's generated self-signed certificate
	// is the expected content, and Go's verifier accepts a self-signed leaf
	// placed in the pool. It is read and validated in New (unreadable, no
	// CERTIFICATE block, or a block that does not parse is a startup error).
	// Empty means the system roots.
	CAFile string

	// HTTPClient overrides the transport (tests inject httptest's). nil gets
	// a client with defaultTimeout whose TLS trust is built from CAFile. When
	// both are set, HTTPClient wins for the transport; CAFile is still loaded
	// and validated so a broken file is loud either way.
	HTTPClient *http.Client
}

// Client is a Warpgate admin API client bound to one base URL and one token.
// It is safe for concurrent use: everything but slugLocks is immutable after
// New, and slugLocks is itself concurrency-safe. That matters — lab calls
// EnsureRepoIdentity at spawn, concurrently, and slugLocks is what keeps two
// concurrent ensures of one repo from creating two roles (Warpgate has no
// unique constraint on role names; see identity.go).
type Client struct {
	httpClient *http.Client
	base       *url.URL // normalized origin (+ operator path prefix), path "/" or "/prefix"
	adminRoot  *url.URL // base + /@warpgate/admin/api
	infoURL    *url.URL // base + /@warpgate/api/info
	token      string   // header-only, never in a URL or an error
	slugLocks  *keyedMutex
}

// String renders the client WITHOUT its token (and with any URL userinfo
// password masked). It exists so that fmt's %v on a *Client — the classic
// accidental credential leak, since %v on a struct pointer prints every field
// including unexported ones — prints a redacted line instead. The receiver is
// a value so that both Client and *Client are covered.
func (c Client) String() string {
	return fmt.Sprintf("warpgate.Client{base:%s, token:REDACTED}", c.baseString())
}

// baseString renders the base URL for messages, tolerating the zero Client
// (String must never panic — it is reached from logging paths).
func (c Client) baseString() string {
	if c.base == nil {
		return "(unset)"
	}
	return c.base.Redacted()
}

// New validates opts and returns a client whose URLs and TLS trust are
// already resolved. Every misconfiguration — an empty or non-http(s) BaseURL,
// an empty token or one carrying CR/LF/NUL, an unreadable or certificate-free
// CAFile — is refused HERE rather than at the first call: lab reads these from
// flags at startup, and a misconfiguration must be a loud startup error, not a
// confusing failure during a spawn hours later. The CR/LF/NUL rule is the
// single-line rule vault applies to header-bound credentials (ADR-0006).
// No error echoes the token or the URL's userinfo.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("warpgate: BaseURL is required (e.g. https://localhost:8888)")
	}
	if opts.Token == "" {
		return nil, errors.New("warpgate: Token is required")
	}
	if strings.ContainsAny(opts.Token, "\r\n\x00") {
		// Never echo the token, not even a prefix of it.
		return nil, errors.New("warpgate: Token must be a single line without control characters")
	}
	base, err := normalizeBase(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	var roots *x509.CertPool
	if opts.CAFile != "" {
		if roots, err = loadCAPool(opts.CAFile); err != nil {
			return nil, err
		}
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = newHTTPClient(roots)
	}
	return &Client{
		httpClient: httpClient,
		base:       base,
		adminRoot:  adminRootURL(base),
		infoURL:    infoURL(base),
		token:      opts.Token,
		slugLocks:  newKeyedMutex(),
	}, nil
}

// newHTTPClient builds the default client: http.DefaultTransport's settings
// (proxy-from-environment — which never proxies loopback — dial and
// handshake timeouts, connection reuse) with a TLS config pinned to roots when
// a CAFile was given (nil roots means the system pool) and TLS 1.2 as the
// floor, stated explicitly rather than inherited.
func newHTTPClient(roots *x509.CertPool) *http.Client {
	var transport *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = dt.Clone()
	} else {
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	transport.TLSClientConfig = &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}
	return &http.Client{Timeout: defaultTimeout, Transport: transport}
}

// loadCAPool reads a PEM file into a pool holding ONLY its certificates. Non-
// CERTIFICATE blocks (a private key someone concatenated in) are skipped, but
// a CERTIFICATE block that does not parse is an error rather than a skip — a
// half-read trust file is exactly the misconfiguration that should be loud —
// and a file with no certificate at all is refused, because an empty pool
// would fail every call with an x509 error far from its cause.
func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("warpgate: CA file: %w", err)
	}
	pool := x509.NewCertPool()
	n := 0
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("warpgate: CA file %s: CERTIFICATE block %d does not parse: %w", path, n+1, err)
		}
		pool.AddCert(cert)
		n++
	}
	if n == 0 {
		return nil, fmt.Errorf("warpgate: CA file %s contains no PEM CERTIFICATE block (expected Warpgate's TLS certificate, e.g. the http.certificate file from warpgate.yaml)", path)
	}
	return pool, nil
}

// normalizeBase reduces a configured base URL to the origin plus any
// reverse-proxy path prefix, with an absolute path and no trailing slash, and
// with a pasted Warpgate API path stripped (see Options.BaseURL). It is the
// single place URL shape is decided, so building a request URL afterwards is
// exact concatenation of segments.
func normalizeBase(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// url.Parse's own error embeds the raw URL, which may carry userinfo
		// credentials; report only the reason, and name the expected shape
		// because the common way to land here is a base without a scheme.
		return nil, fmt.Errorf("warpgate: BaseURL is not a valid http(s) URL (e.g. https://localhost:8888): %w", urlErrReason(err))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("warpgate: BaseURL must be an http(s) URL (e.g. https://localhost:8888), got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("warpgate: BaseURL must include a host (e.g. https://localhost:8888)")
	}
	// Query and fragment are meaningless on an API root and would ride along
	// on every built URL; drop them rather than smuggle them into calls.
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	// The leading slash is not cosmetic: url.URL.JoinPath returns a RELATIVE
	// path when the receiver's path is empty, which makes EscapedPath() — the
	// string *APIError reports — come out without its leading slash.
	u.Path = "/" + strings.Trim(u.Path, "/")
	u.RawPath = ""
	u.Path = stripAPIPath(u.Path)
	return u, nil
}

// urlErrReason unwraps a *url.Error to its underlying reason, discarding the
// URL the error would otherwise quote back. A configured URL can carry
// userinfo, so the raw string is treated as potentially secret-bearing
// everywhere in this package.
func urlErrReason(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// APIError is a non-2xx answer from Warpgate. It carries the machine-readable
// status alongside the request that produced it, so callers branch via
// errors.As (the identity code branches on 400/404/409; a caller telling
// "wrong token" from "Warpgate down" branches on 401/403).
//
// Its Error() names the method, path, status and the server's message and
// NOTHING else — never the token, which only ever exists in a request header.
// For the targets endpoints Message is always empty (their bodies may carry
// target credentials; see the package doc).
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
	switch e.StatusCode {
	case http.StatusUnauthorized:
		// Warpgate answers 401 both for an unknown token and for a valid
		// per-user token whose user holds no admin role (admin_scheme.rs), so
		// the message names both fixes.
		return fmt.Sprintf("warpgate %s %s: admin token rejected (401): check --warpgate-admin-token-file — it must hold Warpgate's static admin token (warpgate run --enable-admin-token) or an API token of a Warpgate admin", e.Method, e.Path)
	case http.StatusForbidden:
		return fmt.Sprintf("warpgate %s %s: admin token lacks a permission lab needs (403): %s — use the static admin token, or grant the token's user the missing admin permission", e.Method, e.Path, msg)
	}
	return fmt.Sprintf("warpgate %s %s: unexpected status %d: %s", e.Method, e.Path, e.StatusCode, msg)
}

// isStatus reports whether err is an *APIError with the given status — the
// idempotency answers (409 already assigned, 404 already gone) callers must
// recognize rather than surface.
func isStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

// deleteIdempotent issues a bodyless DELETE and reports whether THIS call
// removed the resource: 204 → true; 404 → false with no error, because every
// DELETE lab issues targets something whose absence is the goal (a user, a
// role, a target assignment, a run key — wire.go points 4, 6, 8, 9), and a
// repeat or a concurrent removal must not fail the caller.
func (c *Client) deleteIdempotent(ctx context.Context, u *url.URL) (bool, error) {
	if _, err := c.do(ctx, http.MethodDelete, u, nil, plainBody); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// bodySecrecy says whether a request's response body may carry credentials.
// It is a named type rather than a bare bool so every call site reads as a
// decision about secrecy, not an unexplained true/false.
type bodySecrecy bool

const (
	// plainBody: the body is ordinary admin data; a non-2xx body is folded
	// into *APIError as a capped snippet.
	plainBody bodySecrecy = false
	// secretBody: the body may carry target credentials (GET /targets, GET
	// /role/{id}/targets). A non-2xx body is DISCARDED unread — not even a
	// snippet — and a decode failure is reported without the decoder's text.
	secretBody bodySecrecy = true
)

// do performs one REST call and returns the 2xx body, bounded. It is the ONLY
// place a request is built, so every request carries the same headers by
// construction:
//
//   - X-Warpgate-Token: <token> — on every call, including the info probe,
//     where the token is what turns the anonymous answer into an
//     authenticated one (health.go).
//   - Accept: application/json — Warpgate answers text/html with an HTML page.
//   - Content-Type: application/json — exactly when there is a body. Warpgate
//     415s a JSON-bodied endpoint without it, including the user-role grant
//     whose body is optional (wire.go).
//
// A 204 returns a nil body. A non-2xx returns *APIError. ctx governs the whole
// call; a cancellation surfaces wrapped, so errors.Is(err, context.Canceled)
// holds for callers.
func (c *Client) do(ctx context.Context, method string, u *url.URL, reqBody any, secrecy bodySecrecy) ([]byte, error) {
	path := u.EscapedPath()

	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("warpgate %s %s: encode request: %w", method, path, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("warpgate %s %s: build request: %w", method, path, urlErrReason(err))
	}
	req.Header.Set(tokenHeader, c.token)
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// http.Client wraps transport failures in *url.Error, whose URL field
		// is this request's URL — token-free, but possibly carrying userinfo;
		// the reason is what the operator needs, and unwrapping keeps errors.Is
		// against context.Canceled/DeadlineExceeded working for the caller.
		return nil, fmt.Errorf("warpgate %s %s: %w", method, path, urlErrReason(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{StatusCode: resp.StatusCode, Method: method, Path: path}
		if secrecy == plainBody {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodySnippetMax))
			apiErr.Message = errorMessage(snippet)
		}
		return nil, apiErr
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	// Read one byte past the bound so an oversized body is a loud error rather
	// than a silent truncation that would surface as a confusing decode error.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("warpgate %s %s: read response: %w", method, path, err)
	}
	if len(data) > maxResponseBody {
		return nil, fmt.Errorf("warpgate %s %s: response exceeds %d bytes; is something other than Warpgate answering on this port?", method, path, maxResponseBody)
	}
	return data, nil
}

// errorMessage extracts a human message from an error body. Warpgate has no
// JSON error envelope (wire.go): a declared 4xx answers a bare JSON STRING
// (often just the offending field name, e.g. "username") or nothing; an
// internal error answers text/plain "<reason> (reference: <uuid>)"; a request
// that fails to parse answers poem's plain-text reason. So: a JSON string is
// unquoted, anything else is whitespace-collapsed, and the caller's
// LimitReader has already bounded it — the bound is the point, since an
// unexpected body could contain anything.
func errorMessage(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	var s string
	if len(trimmed) > 0 && trimmed[0] == '"' && json.Unmarshal(trimmed, &s) == nil {
		trimmed = []byte(s)
	}
	msg := strings.Join(strings.Fields(string(trimmed)), " ")
	if msg == "" {
		return "(empty body)"
	}
	return msg
}
