package warpgate

// New, the transport, and the error mapping. Operation semantics live in the
// per-file suites (identity_test.go, targets_test.go, keys_test.go,
// health_test.go); this one pins what every request shares.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- New -------------------------------------------------------------------

func TestNewRejectsBadOptions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     Options
		wantWord string
	}{
		{"empty base url", Options{Token: testToken}, "BaseURL is required"},
		{"blank base url", Options{BaseURL: "  ", Token: testToken}, "BaseURL is required"},
		{"empty token", Options{BaseURL: "https://localhost:8888"}, "Token is required"},
		{"non-http scheme", Options{BaseURL: "ssh://localhost:2222", Token: testToken}, "http(s)"},
		{"no scheme", Options{BaseURL: "localhost:8888", Token: testToken}, "http(s)"},
		{"unparseable", Options{BaseURL: "https://[::1", Token: testToken}, "not a valid"},
		{"no host", Options{BaseURL: "https:///@warpgate", Token: testToken}, "must include a host"},
		{"token with newline", Options{BaseURL: "https://localhost:8888", Token: "abc\ndef"}, "single line"},
		{"token with CR", Options{BaseURL: "https://localhost:8888", Token: "abc\rdef"}, "single line"},
		{"token with NUL", Options{BaseURL: "https://localhost:8888", Token: "abc\x00def"}, "single line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.opts)
			if err == nil {
				t.Fatalf("New succeeded, want refusal (client %v)", c)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not mention %q", err, tc.wantWord)
			}
			if tc.opts.Token != "" && strings.Contains(err.Error(), tc.opts.Token) {
				t.Errorf("error %q echoes the token", err)
			}
		})
	}
}

// TestNewNeverEchoesURLUserinfo: a configured URL may carry basic-auth
// credentials for a reverse proxy; no error may quote them.
func TestNewNeverEchoesURLUserinfo(t *testing.T) {
	for _, raw := range []string{
		"https://admin:SUPERSECRETPW@[::1",
		"ftp://admin:SUPERSECRETPW@localhost:8888",
	} {
		_, err := New(Options{BaseURL: raw, Token: testToken})
		if err == nil {
			t.Fatalf("New(%q) succeeded, want refusal", raw)
		}
		if strings.Contains(err.Error(), "SUPERSECRETPW") {
			t.Errorf("error %q echoes URL userinfo", err)
		}
	}
}

// TestBaseURLSpellingsResolveToOneAdminRoot pins the normalization contract:
// every spelling an operator plausibly pastes addresses the same endpoints,
// and none of them doubles a Warpgate path.
func TestBaseURLSpellingsResolveToOneAdminRoot(t *testing.T) {
	s := newStub(t, answer(http.StatusOK, `[]`))
	for _, suffix := range []string{
		"", "/", "//", "/@warpgate", "/@warpgate/", "/@warpgate/admin/api", "/@warpgate/admin/api/",
		"/@warpgate/api", "/_warpgate", "/_warpgate/admin/api", "?x=1", "#frag",
	} {
		t.Run("base"+suffix, func(t *testing.T) {
			c := newTestClient(t, s.URL+suffix)
			if _, err := c.ListSSHTargets(context.Background()); err != nil {
				t.Fatalf("ListSSHTargets: %v", err)
			}
			reqs := s.requests()
			last := reqs[len(reqs)-1]
			if last.Path != "/@warpgate/admin/api/targets" {
				t.Errorf("base %q hit path %q, want /@warpgate/admin/api/targets", s.URL+suffix, last.Path)
			}
			if len(last.Query) != 0 {
				t.Errorf("base %q leaked query %v into the request", s.URL+suffix, last.Query)
			}
			if _, err := c.Health(context.Background()); err == nil {
				t.Fatal("Health against a list answer succeeded; want a decode error")
			}
			reqs = s.requests()
			if got := reqs[len(reqs)-1].Path; got != "/@warpgate/api/info" {
				t.Errorf("base %q probed %q, want /@warpgate/api/info", s.URL+suffix, got)
			}
		})
	}
}

// TestBaseURLKeepsReverseProxyPrefix: Warpgate mounted under a proxy path
// keeps its prefix, with a pasted API path still stripped behind it.
func TestBaseURLKeepsReverseProxyPrefix(t *testing.T) {
	s := newStub(t, answer(http.StatusOK, `[]`))
	for _, base := range []string{s.URL + "/wg", s.URL + "/wg/", s.URL + "/wg/@warpgate/admin/api"} {
		c := newTestClient(t, base)
		if _, err := c.ListSSHTargets(context.Background()); err != nil {
			t.Fatalf("ListSSHTargets: %v", err)
		}
		reqs := s.requests()
		if got := reqs[len(reqs)-1].Path; got != "/wg/@warpgate/admin/api/targets" {
			t.Errorf("base %q hit %q, want /wg/@warpgate/admin/api/targets", base, got)
		}
	}
}

// TestIdentifiersArePathEscaped: an id carrying a slash must address one
// escaped path element, never traverse into another endpoint.
func TestIdentifiersArePathEscaped(t *testing.T) {
	s := newStub(t, answer(http.StatusNoContent, ""))
	c := newTestClient(t, s.URL)
	if err := c.RemovePublicKey(context.Background(), "u/../..", "k/1"); err != nil {
		t.Fatalf("RemovePublicKey: %v", err)
	}
	if got, want := s.only(t).Path, "/@warpgate/admin/api/users/u%2F..%2F../credentials/public-keys/k%2F1"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestEveryRequestCarriesTokenAndAccept drives the whole operation surface
// through the fake — a repo's full lifecycle — and asserts the header
// contract on EVERY request: the token in X-Warpgate-Token (never anywhere
// else), Accept: application/json, and Content-Type: application/json
// exactly when a body is sent.
func TestEveryRequestCarriesTokenAndAccept(t *testing.T) {
	f := newFake(t)
	f.targets = []string{sshTarget("t-1", "staging", "")}
	c := f.client(t)
	ctx := context.Background()

	id, err := c.EnsureRepoIdentity(ctx, testRepoID, "Coding Lab")
	if err != nil {
		t.Fatalf("EnsureRepoIdentity: %v", err)
	}
	if _, _, err := c.FindRepoIdentity(ctx, testRepoID); err != nil {
		t.Fatalf("FindRepoIdentity: %v", err)
	}
	if _, err := c.ListSSHTargets(ctx); err != nil {
		t.Fatalf("ListSSHTargets: %v", err)
	}
	if err := c.AssignTargetRole(ctx, "t-1", id.Role.ID); err != nil {
		t.Fatalf("AssignTargetRole: %v", err)
	}
	if _, err := c.RoleSSHTargets(ctx, id.Role.ID); err != nil {
		t.Fatalf("RoleSSHTargets: %v", err)
	}
	if err := c.UnassignTargetRole(ctx, "t-1", id.Role.ID); err != nil {
		t.Fatalf("UnassignTargetRole: %v", err)
	}
	key, err := c.AddPublicKey(ctx, id.User.ID, RunKeyLabel(testRunID), testAuthorizedKey(t))
	if err != nil {
		t.Fatalf("AddPublicKey: %v", err)
	}
	if _, err := c.ListPublicKeys(ctx, id.User.ID); err != nil {
		t.Fatalf("ListPublicKeys: %v", err)
	}
	if err := c.RemovePublicKey(ctx, id.User.ID, key.ID); err != nil {
		t.Fatalf("RemovePublicKey: %v", err)
	}
	if _, err := c.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if _, err := c.DeleteRepoIdentity(ctx, testRepoID); err != nil {
		t.Fatalf("DeleteRepoIdentity: %v", err)
	}

	reqs := f.requests()
	if len(reqs) < 15 {
		t.Fatalf("lifecycle issued only %d requests: %v", len(reqs), reqs)
	}
	for _, r := range reqs {
		if got := r.Header.Get(tokenHeader); got != testToken {
			t.Errorf("%v: %s = %q, want the token", r, tokenHeader, got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("%v: Authorization = %q; the token rides only in %s", r, got, tokenHeader)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("%v: Accept = %q, want application/json", r, got)
		}
		wantCT := ""
		if r.Body != "" {
			wantCT = "application/json"
		}
		if got := r.Header.Get("Content-Type"); got != wantCT {
			t.Errorf("%v: Content-Type = %q with body %q, want %q", r, got, r.Body, wantCT)
		}
		if strings.Contains(r.Path, testToken) || strings.Contains(r.Query.Encode(), testToken) || strings.Contains(r.Body, testToken) {
			t.Errorf("%v carries the token outside its header", r)
		}
	}
}

// --- errors ----------------------------------------------------------------

func TestNon2xxBecomesAPIError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantMsg   string   // APIError.Message
		wantInErr []string // substrings of Error()
	}{
		{
			// No/unknown token, or a user token without any admin role
			// (admin_scheme.rs) — the message must send the operator to the
			// token file, not leave them with a bare 401.
			name: "401 empty body", status: http.StatusUnauthorized, body: "",
			wantMsg:   "(empty body)",
			wantInErr: []string{"admin token rejected (401)", "--warpgate-admin-token-file", "--enable-admin-token"},
		},
		{
			name: "401 poem authorization error", status: http.StatusUnauthorized, body: "authorization error",
			wantMsg:   "authorization error",
			wantInErr: []string{"admin token rejected (401)"},
		},
		{
			name: "403 missing permission", status: http.StatusForbidden, body: "admin permission required: UsersCreate",
			wantMsg:   "admin permission required: UsersCreate",
			wantInErr: []string{"lacks a permission", "(403)", "UsersCreate"},
		},
		{
			// Declared 4xx bodies are bare JSON strings (wire.go point 10).
			name: "400 json string", status: http.StatusBadRequest, body: `"username"`,
			wantMsg:   "username",
			wantInErr: []string{"unexpected status 400", "username"},
		},
		{
			name: "500 plain text with reference", status: http.StatusInternalServerError,
			body:      "Internal Server Error (reference: 3f1c0e2a-0000-4000-8000-000000000000)",
			wantMsg:   "Internal Server Error (reference: 3f1c0e2a-0000-4000-8000-000000000000)",
			wantInErr: []string{"unexpected status 500", "reference:"},
		},
		{
			name: "502 html from a stray proxy", status: http.StatusBadGateway, body: "<html>\n <body>nginx</body>\n</html>",
			wantMsg:   "<html> <body>nginx</body> </html>",
			wantInErr: []string{"unexpected status 502"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, answer(tc.status, tc.body))
			c := newTestClient(t, s.URL)
			_, err := c.ListPublicKeys(context.Background(), "u1")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error %v (%T) is not an *APIError", err, err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Method != http.MethodGet ||
				apiErr.Path != "/@warpgate/admin/api/users/u1/credentials/public-keys" {
				t.Errorf("APIError = %+v", apiErr)
			}
			if apiErr.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", apiErr.Message, tc.wantMsg)
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Error() = %q, missing %q", err, want)
				}
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("Error() = %q contains the token", err)
			}
		})
	}
}

// TestErrorSnippetIsCapped: an error body is a diagnostic aid, not a
// transcript.
func TestErrorSnippetIsCapped(t *testing.T) {
	s := newStub(t, answer(http.StatusInternalServerError, strings.Repeat("x", 10*errBodySnippetMax)))
	c := newTestClient(t, s.URL)
	_, err := c.ListPublicKeys(context.Background(), "u1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if len(apiErr.Message) > errBodySnippetMax {
		t.Errorf("Message is %d bytes, want at most %d", len(apiErr.Message), errBodySnippetMax)
	}
}

// TestOversizedBodyIsLoud: a body past maxResponseBody is refused, never
// silently truncated into a confusing decode error.
func TestOversizedBodyIsLoud(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("["))
		_, _ = w.Write([]byte(strings.Repeat(" ", maxResponseBody)))
		_, _ = w.Write([]byte("]"))
	})
	c := newTestClient(t, s.URL)
	_, err := c.ListPublicKeys(context.Background(), "u1")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want the oversized-body refusal", err)
	}
}

// TestClientPrintsRedacted: %v on a *Client is the classic accidental leak.
func TestClientPrintsRedacted(t *testing.T) {
	c, err := New(Options{BaseURL: "https://proxyuser:PROXYPASSWORD@localhost:8888", Token: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, rendered := range []string{
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprint(*c), c.String(), Client{}.String(),
	} {
		if strings.Contains(rendered, testToken) {
			t.Errorf("rendered client %q contains the token", rendered)
		}
		if strings.Contains(rendered, "PROXYPASSWORD") {
			t.Errorf("rendered client %q contains the URL password", rendered)
		}
		if !strings.Contains(rendered, "REDACTED") {
			t.Errorf("rendered client %q does not say REDACTED", rendered)
		}
	}
}

// --- context ---------------------------------------------------------------

func TestContextCancellationPropagates(t *testing.T) {
	started := make(chan struct{})
	s := newStub(t, func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	c := newTestClient(t, s.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := c.ListSSHTargets(ctx); !errors.Is(err, context.Canceled) {
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

// --- TLS / CAFile ----------------------------------------------------------

// writePEM writes certificates as a PEM file and returns its path.
func writePEM(t *testing.T, blocks ...*pem.Block) string {
	t.Helper()
	var b strings.Builder
	for _, blk := range blocks {
		if err := pem.Encode(&b, blk); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "warpgate-ca.pem")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTLSInfoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(answer(http.StatusOK, infoBody(allPerms, "0.29.1")))
	t.Cleanup(srv.Close)
	return srv
}

// TestCAFileTrustsTheGivenCertificate is the TLS round trip: the httptest
// server's self-signed certificate, written to a PEM file, is the ONLY trust
// anchor, and a call succeeds through it.
func TestCAFileTrustsTheGivenCertificate(t *testing.T) {
	srv := newTLSInfoServer(t)
	caFile := writePEM(t, &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c, err := New(Options{BaseURL: srv.URL, Token: testToken, CAFile: caFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health over TLS with the CA file: %v", err)
	}
	if !info.Authenticated || info.Version != "0.29.1" {
		t.Errorf("info = %+v", info)
	}

	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatalf("transport = %T, want an *http.Transport with a TLS config", c.httpClient.Transport)
	}
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", transport.TLSClientConfig.MinVersion)
	}
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("Timeout = %v, want %v", c.httpClient.Timeout, defaultTimeout)
	}
}

// TestCAFileTrustsOnlyItsCertificates: a CA file holding a DIFFERENT
// certificate must not fall back to anything — the server is untrusted.
func TestCAFileTrustsOnlyItsCertificates(t *testing.T) {
	srv := newTLSInfoServer(t)
	caFile := writePEM(t, &pem.Block{Type: "CERTIFICATE", Bytes: selfSignedCert(t)})
	c, err := New(Options{BaseURL: srv.URL, Token: testToken, CAFile: caFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Health(context.Background())
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Errorf("Health with a foreign CA: error %v, want an unknown-authority failure", err)
	}
}

// TestNoCAFileUsesSystemRoots: without a CA file the self-signed httptest
// certificate is (correctly) untrusted.
func TestNoCAFileUsesSystemRoots(t *testing.T) {
	srv := newTLSInfoServer(t)
	c := newTestClient(t, srv.URL)
	if _, err := c.Health(context.Background()); err == nil {
		t.Error("Health against a self-signed server succeeded without a CA file")
	}
}

func TestCAFileRefusals(t *testing.T) {
	keyPEM := &pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a real key")}
	for _, tc := range []struct {
		name     string
		path     func(t *testing.T) string
		wantWord string
	}{
		{"missing file", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.pem") }, "no such file"},
		{"garbage", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "garbage.pem")
			_ = os.WriteFile(p, []byte("this is not PEM at all\n"), 0o644)
			return p
		}, "no PEM CERTIFICATE block"},
		{"only a key block", func(t *testing.T) string { return writePEM(t, keyPEM) }, "no PEM CERTIFICATE block"},
		{"certificate block that does not parse", func(t *testing.T) string {
			return writePEM(t, &pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage DER")})
		}, "does not parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path(t)
			_, err := New(Options{BaseURL: "https://localhost:8888", Token: testToken, CAFile: path})
			if err == nil {
				t.Fatal("New succeeded, want a CA file refusal")
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not mention %q", err, tc.wantWord)
			}
		})
	}
}

// TestCAFileSkipsNonCertificateBlocks: a key concatenated next to the
// certificate is ignored, not fatal.
func TestCAFileSkipsNonCertificateBlocks(t *testing.T) {
	srv := newTLSInfoServer(t)
	caFile := writePEM(t,
		&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("ignored")},
		&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw},
	)
	c, err := New(Options{BaseURL: srv.URL, Token: testToken, CAFile: caFile})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Errorf("Health: %v", err)
	}
}

// selfSignedCert returns the DER of a fresh self-signed certificate that has
// nothing to do with httptest's.
func selfSignedCert(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "not-warpgate"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
