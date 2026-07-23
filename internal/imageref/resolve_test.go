package imageref

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// testDigest is what a cooperative registry hands back in
// Docker-Content-Digest during tests; the value is arbitrary but
// grammatically valid.
var testDigest = "sha256:" + strings.Repeat("abcd", 16)

// newTestRegistry spins up a TLS registry stub and a Resolver wired to
// trust it, returning the host:port refs should name. Every resolver test
// goes through here — no test in this package ever touches a live network.
func newTestRegistry(t *testing.T, h http.Handler) (host string, rv *Resolver) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parsing test server url %q: %v", ts.URL, err)
	}
	return u.Host, &Resolver{Client: ts.Client()}
}

func TestManifestURL(t *testing.T) {
	cases := []struct {
		host, path, ref, want string
	}{
		// docker.io maps to its real API endpoint; everything else resolves
		// at the host the ref names.
		{"docker.io", "library/debian", "bookworm", "https://registry-1.docker.io/v2/library/debian/manifests/bookworm"},
		{"ghcr.io", "foo/bar", "v1", "https://ghcr.io/v2/foo/bar/manifests/v1"},
		{"localhost:5000", "a/b", "latest", "https://localhost:5000/v2/a/b/manifests/latest"},
		{"example.com:8443", "repo", testDigest, "https://example.com:8443/v2/repo/manifests/" + testDigest},
	}
	for _, tc := range cases {
		if got := manifestURL(tc.host, tc.path, tc.ref); got != tc.want {
			t.Errorf("manifestURL(%q, %q, %q) = %q, want %q", tc.host, tc.path, tc.ref, got, tc.want)
		}
	}
	if got := endpointHost("docker.io"); got != "registry-1.docker.io" {
		t.Errorf("endpointHost(docker.io) = %q, want registry-1.docker.io", got)
	}
	if got := endpointHost("quay.io"); got != "quay.io" {
		t.Errorf("endpointHost(quay.io) = %q, want quay.io", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// A pinned ref must canonicalize with zero network traffic — Pin is called
// on every settings save, including saves that change nothing.
func TestPinAlreadyPinnedNoNetwork(t *testing.T) {
	called := false
	rv := &Resolver{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("no network allowed")
	})}}
	for in, want := range map[string]string{
		"ghcr.io/foo/bar@" + testDigest:          "ghcr.io/foo/bar@" + testDigest,
		"ghcr.io/foo/bar:v1@" + testDigest:       "ghcr.io/foo/bar:v1@" + testDigest,
		"docker.io/debian@" + testDigest:         "docker.io/library/debian@" + testDigest,
		"index.docker.io/debian:b@" + testDigest: "docker.io/library/debian:b@" + testDigest,
	} {
		got, err := rv.Pin(context.Background(), in)
		if err != nil {
			t.Fatalf("Pin(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Errorf("Pin(%q) = %q, want %q", in, got, want)
		}
	}
	if called {
		t.Error("Pin of a pinned ref hit the network")
	}
}

// Parse failures pass through Pin unchanged, so the settings form shows
// the same message whether validation happens client- or server-side.
func TestPinParseErrorPassthrough(t *testing.T) {
	rv := &Resolver{}
	_, err := rv.Pin(context.Background(), "debian")
	if err == nil || !strings.Contains(err.Error(), "fully qualified") {
		t.Fatalf("Pin(debian) error = %v, want fully-qualified parse error", err)
	}
}

func TestPinHeadDigestHeader(t *testing.T) {
	var gotMethod, gotPath, gotAccept string
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotMethod, gotPath, gotAccept = req.Method, req.URL.Path, req.Header.Get("Accept")
		w.Header().Set("Docker-Content-Digest", testDigest)
		w.WriteHeader(http.StatusOK)
	}))

	got, err := rv.Pin(context.Background(), host+"/some/repo:v1")
	if err != nil {
		t.Fatalf("Pin unexpected error: %v", err)
	}
	if want := host + "/some/repo:v1@" + testDigest; got != want {
		t.Errorf("Pin = %q, want %q", got, want)
	}
	if gotMethod != http.MethodHead {
		t.Errorf("registry saw method %q, want HEAD", gotMethod)
	}
	if want := "/v2/some/repo/manifests/v1"; gotPath != want {
		t.Errorf("registry saw path %q, want %q", gotPath, want)
	}
	for _, mt := range []string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
	} {
		if !strings.Contains(gotAccept, mt) {
			t.Errorf("Accept header %q missing %q", gotAccept, mt)
		}
	}
}

// A registry that answers HEAD with a garbage digest must be an error, not
// a stored garbage ref.
func TestPinHeadMalformedDigestHeader(t *testing.T) {
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:nothex")
		w.WriteHeader(http.StatusOK)
	}))
	_, err := rv.Pin(context.Background(), host+"/some/repo:v1")
	if err == nil || !strings.Contains(err.Error(), "malformed digest") {
		t.Fatalf("Pin error = %v, want malformed-digest error", err)
	}
}

func TestPinGetHashFallback(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	var methods []string
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		methods = append(methods, req.Method)
		if req.URL.Path != "/v2/some/repo/manifests/v1" {
			t.Errorf("registry saw path %q", req.URL.Path)
		}
		// No Docker-Content-Digest on either response; the GET serves the
		// manifest bytes the resolver must hash.
		if req.Method == http.MethodGet {
			_, _ = w.Write(manifest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	got, err := rv.Pin(context.Background(), host+"/some/repo:v1")
	if err != nil {
		t.Fatalf("Pin unexpected error: %v", err)
	}
	sum := sha256.Sum256(manifest)
	want := host + "/some/repo:v1@sha256:" + hex.EncodeToString(sum[:])
	if got != want {
		t.Errorf("Pin = %q, want %q (sha256 of the served body)", got, want)
	}
	if len(methods) != 2 || methods[0] != http.MethodHead || methods[1] != http.MethodGet {
		t.Errorf("registry saw methods %v, want [HEAD GET]", methods)
	}
}

func TestPinBearerTokenFlow(t *testing.T) {
	var tokenQuery url.Values
	var retryAuth string
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/token":
			tokenQuery = req.URL.Query()
			_, _ = fmt.Fprint(w, `{"token":"tok123"}`)
		case "/v2/some/repo/manifests/v1":
			if req.Header.Get("Authorization") == "" {
				// Challenge without scope: the resolver must default it.
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer realm="https://%s/token",service="test-registry"`, req.Host))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			retryAuth = req.Header.Get("Authorization")
			w.Header().Set("Docker-Content-Digest", testDigest)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("registry saw unexpected path %q", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	got, err := rv.Pin(context.Background(), host+"/some/repo:v1")
	if err != nil {
		t.Fatalf("Pin unexpected error: %v", err)
	}
	if want := host + "/some/repo:v1@" + testDigest; got != want {
		t.Errorf("Pin = %q, want %q", got, want)
	}
	if got := tokenQuery.Get("service"); got != "test-registry" {
		t.Errorf("token request service = %q, want test-registry", got)
	}
	if got := tokenQuery.Get("scope"); got != "repository:some/repo:pull" {
		t.Errorf("token request scope = %q, want default repository:some/repo:pull", got)
	}
	if retryAuth != "Bearer tok123" {
		t.Errorf("retry Authorization = %q, want Bearer tok123", retryAuth)
	}
}

// A challenge that names its own scope wins over the default, and a token
// endpoint that only speaks OAuth2's access_token still works.
func TestPinBearerExplicitScopeAccessToken(t *testing.T) {
	var tokenQuery url.Values
	var retryAuth string
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/token":
			tokenQuery = req.URL.Query()
			_, _ = fmt.Fprint(w, `{"access_token":"alt456"}`)
		default:
			if req.Header.Get("Authorization") == "" {
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer realm="https://%s/token",service="svc",scope="repository:other/thing:pull"`, req.Host))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			retryAuth = req.Header.Get("Authorization")
			w.Header().Set("Docker-Content-Digest", testDigest)
			w.WriteHeader(http.StatusOK)
		}
	}))

	if _, err := rv.Pin(context.Background(), host+"/some/repo:v1"); err != nil {
		t.Fatalf("Pin unexpected error: %v", err)
	}
	if got := tokenQuery.Get("scope"); got != "repository:other/thing:pull" {
		t.Errorf("token request scope = %q, want the challenge's own scope", got)
	}
	if retryAuth != "Bearer alt456" {
		t.Errorf("retry Authorization = %q, want Bearer alt456", retryAuth)
	}
}

// A realm on plain http would route the token flow over an insecure
// channel; the resolver must refuse before any token request happens.
func TestPinBearerRealmNotHTTPS(t *testing.T) {
	tokenRequested := false
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/token" {
			tokenRequested = true
		}
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="http://%s/token",service="svc"`, req.Host))
		w.WriteHeader(http.StatusUnauthorized)
	}))

	_, err := rv.Pin(context.Background(), host+"/some/repo:v1")
	if err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("Pin error = %v, want not-https realm rejection", err)
	}
	if tokenRequested {
		t.Error("resolver contacted an http token realm")
	}
}

func TestPin404(t *testing.T) {
	host, rv := newTestRegistry(t, http.NotFoundHandler())
	ref := host + "/no/such:v1"
	_, err := rv.Pin(context.Background(), ref)
	if err == nil {
		t.Fatal("Pin succeeded against a 404 registry")
	}
	for _, want := range []string{ref, "404", "check the image path and tag"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Pin error %q missing %q", err, want)
		}
	}
}

// 401 with no Bearer challenge (or after a successful token) means the
// image genuinely is not anonymously pullable — say so.
func TestPin401NoBearerChallenge(t *testing.T) {
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	_, err := rv.Pin(context.Background(), host+"/priv/repo:v1")
	if err == nil || !strings.Contains(err.Error(), "anonymous pull is not allowed") {
		t.Fatalf("Pin error = %v, want anonymous-pull-not-allowed", err)
	}
}

func TestPinTokenEndpointFailure(t *testing.T) {
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm="https://%s/token",service="svc"`, req.Host))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	_, err := rv.Pin(context.Background(), host+"/priv/repo:v1")
	if err == nil || !strings.Contains(err.Error(), "token endpoint returned") {
		t.Fatalf("Pin error = %v, want token-endpoint failure", err)
	}
}

// A "manifest" larger than the cap is not a manifest; hashing a truncated
// body would store a wrong digest, so the resolver must refuse instead.
func TestPinOversizedManifest(t *testing.T) {
	chunk := bytes.Repeat([]byte{'x'}, 1<<20)
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			for range 5 { // 5 MiB > the 4 MiB cap
				_, _ = w.Write(chunk)
			}
			return
		}
		w.WriteHeader(http.StatusOK) // HEAD without a digest header
	}))
	_, err := rv.Pin(context.Background(), host+"/big/repo:v1")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Pin error = %v, want oversized-manifest refusal", err)
	}
}

func TestPinContextCanceled(t *testing.T) {
	host, rv := newTestRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Docker-Content-Digest", testDigest)
		w.WriteHeader(http.StatusOK)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := rv.Pin(ctx, host+"/some/repo:v1")
	if err == nil {
		t.Fatal("Pin succeeded with a canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Pin error = %v, want chain to context.Canceled", err)
	}
}

// The package-default client is what production wiring gets on a nil
// Client: bounded, and hard-refusing redirects off https.
func TestDefaultClientPolicy(t *testing.T) {
	if defaultClient.Timeout <= 0 {
		t.Error("default client has no timeout")
	}
	httpsReq, _ := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	httpReq, _ := http.NewRequest(http.MethodGet, "http://example.com/x", nil)
	if err := refuseInsecureRedirect(httpsReq, nil); err != nil {
		t.Errorf("https redirect refused: %v", err)
	}
	if err := refuseInsecureRedirect(httpReq, nil); err == nil {
		t.Error("redirect to http was allowed")
	}
	via := make([]*http.Request, 10)
	if err := refuseInsecureRedirect(httpsReq, via); err == nil {
		t.Error("11th redirect was allowed")
	}
}
