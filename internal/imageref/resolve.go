// Tag → digest resolution against the registry v2 API (issue #207): one
// HEAD to /v2/<path>/manifests/<tag>, reading the digest the registry
// already computed from the Docker-Content-Digest header, with a GET+hash
// fallback for registries that omit it (the manifest bytes ARE the digest
// preimage, so hashing the raw body is the same answer). Anonymous only —
// the #207 contract is public registries — but "anonymous" still means the
// docker-style token dance: registries answer 401 with a Bearer challenge
// whose realm hands out unauthenticated pull tokens, so one token fetch and
// one retry is part of the anonymous path, not an auth feature.
//
// Everything is https by construction: request URLs are built with an
// https:// prefix and no fallback, a token realm on any other scheme is
// rejected, and the package-default client refuses redirects that leave
// https. The only endpoint indirection is docker.io → registry-1.docker.io
// (the API host behind the docker.io name); every other ref is resolved at
// the host it names.
//
// Resolution errors surface verbatim in the repo-settings form banner
// alongside Parse errors, so they name the ref and what to check rather
// than leaking bare status codes.

package imageref

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// acceptManifests advertises the four manifest media types (docker v2 +
// list, OCI manifest + index) so multi-arch images answer with the index
// digest — the same digest `podman pull name@sha256:…` expects.
const acceptManifests = "application/vnd.docker.distribution.manifest.v2+json" +
	", application/vnd.docker.distribution.manifest.list.v2+json" +
	", application/vnd.oci.image.manifest.v1+json" +
	", application/vnd.oci.image.index.v1+json"

// maxManifestBytes caps the GET+hash fallback read. Real manifests are a
// few KB; a body that reaches 4 MiB is not a manifest, and hashing an
// unbounded stream on the settings-save path would be a self-DoS.
const maxManifestBytes = 4 << 20

// maxTokenBytes caps the token-endpoint JSON read for the same reason.
const maxTokenBytes = 1 << 20

// Resolver resolves tags to digests. The zero value is ready to use: a nil
// Client falls back to the package default (15s timeout, redirects refused
// off https). Tests inject an httptest TLS server's client; production
// wiring can inject nothing.
type Resolver struct {
	Client *http.Client
}

// defaultClient bounds the settings-save round-trip (the form submit blocks
// on Pin) and keeps the https-only guarantee across redirects, which the
// URL construction alone cannot see.
var defaultClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: refuseInsecureRedirect,
}

// refuseInsecureRedirect is the default client's redirect policy: any hop
// off https would silently break the package's transport guarantee, so it
// is an error, as is a redirect chain long enough to be a loop.
func refuseInsecureRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-https url %s", req.URL)
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

func (rv *Resolver) httpClient() *http.Client {
	if rv.Client != nil {
		return rv.Client
	}
	return defaultClient
}

// Pin parses s and returns its canonical digest-pinned form. An
// already-pinned ref canonicalizes with no network at all; otherwise the
// tag is resolved against the registry and the digest appended, keeping
// the tag for readability (see Ref.String).
func (rv *Resolver) Pin(ctx context.Context, s string) (string, error) {
	r, err := Parse(s)
	if err != nil {
		return "", err
	}
	if r.Pinned() {
		return r.String(), nil
	}
	digest, err := rv.resolveTag(ctx, r)
	if err != nil {
		return "", err
	}
	r.Digest = digest
	return r.String(), nil
}

// endpointHost maps a ref host to the API endpoint host: docker.io's v2
// API lives at registry-1.docker.io (docker.io itself serves a website);
// every other registry serves its API at the host the ref names.
func endpointHost(host string) string {
	if host == "docker.io" {
		return "registry-1.docker.io"
	}
	return host
}

// manifestURL builds the v2 manifest URL, https by construction. Path and
// tag/digest grammar admit only URL-safe characters, so no escaping is
// needed.
func manifestURL(host, path, reference string) string {
	return "https://" + endpointHost(host) + "/v2/" + path + "/manifests/" + reference
}

// resolveTag is the network path of Pin: HEAD the manifest, run the
// anonymous token dance on a Bearer 401 (retry exactly once), take the
// digest from the response header or fall back to hashing the manifest
// body. r.Tag is always set here — Pin short-circuits pinned refs.
func (rv *Resolver) resolveTag(ctx context.Context, r Ref) (string, error) {
	client := rv.httpClient()
	u := manifestURL(r.Host, r.Path, r.Tag)

	auth := ""
	resp, err := doManifest(ctx, client, http.MethodHead, u, auth)
	if err != nil {
		return "", fmt.Errorf("registry %s not reachable: %w", endpointHost(r.Host), err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		params, ok := parseBearerChallenge(resp.Header.Get("Www-Authenticate"))
		drainClose(resp.Body)
		if !ok {
			return "", fmt.Errorf("resolving %s: registry returned 401 without a Bearer challenge (anonymous pull is not allowed for this image)", r)
		}
		token, err := fetchToken(ctx, client, params, r)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", r, err)
		}
		auth = "Bearer " + token
		resp, err = doManifest(ctx, client, http.MethodHead, u, auth)
		if err != nil {
			return "", fmt.Errorf("registry %s not reachable: %w", endpointHost(r.Host), err)
		}
	}
	drainClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("resolving %s: registry returned 404 (check the image path and tag)", r)
	case http.StatusUnauthorized:
		return "", fmt.Errorf("resolving %s: registry returned 401 (anonymous pull is not allowed for this image)", r)
	default:
		return "", fmt.Errorf("resolving %s: registry returned %s", r, resp.Status)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		if !digestRE.MatchString(d) {
			return "", fmt.Errorf("resolving %s: registry returned malformed digest %q", r, d)
		}
		return d, nil
	}
	// HEAD succeeded but carried no digest header (some registries only
	// compute it on GET): fetch the manifest and hash it ourselves — the
	// digest is by definition the sha256 of the raw manifest bytes.
	return hashManifest(ctx, client, u, auth, r)
}

// hashManifest GETs the manifest (with whatever Authorization the HEAD
// earned) and computes its sha256. The read is capped: a body that hits
// maxManifestBytes is refused rather than truncated-and-hashed, because a
// truncated hash would be a wrong digest, which is worse than no digest.
func hashManifest(ctx context.Context, client *http.Client, u, auth string, r Ref) (string, error) {
	resp, err := doManifest(ctx, client, http.MethodGet, u, auth)
	if err != nil {
		return "", fmt.Errorf("registry %s not reachable: %w", endpointHost(r.Host), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving %s: registry returned %s on manifest fetch", r, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return "", fmt.Errorf("resolving %s: reading manifest: %w", r, err)
	}
	if n > maxManifestBytes {
		return "", fmt.Errorf("resolving %s: manifest exceeds %d bytes, refusing to hash it", r, maxManifestBytes)
	}
	d := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if !digestRE.MatchString(d) {
		return "", fmt.Errorf("resolving %s: computed malformed digest %q", r, d)
	}
	return d, nil
}

// doManifest issues one manifest request; auth is a ready-made
// Authorization header value or "".
func doManifest(ctx context.Context, client *http.Client, method, u, auth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptManifests)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return client.Do(req)
}

// challengeParamRE picks the k="v" pairs out of a Bearer challenge. The
// docker token spec quotes every value, so this deliberately ignores
// unquoted forms.
var challengeParamRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_]*)="([^"]*)"`)

// parseBearerChallenge splits a WWW-Authenticate Bearer challenge into its
// lowercased params (realm, service, scope). ok is false when the header
// is absent or names another scheme — the caller then reports plain 401.
func parseBearerChallenge(header string) (params map[string]string, ok bool) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return nil, false
	}
	params = make(map[string]string)
	for _, m := range challengeParamRE.FindAllStringSubmatch(header[len(prefix):], -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	return params, true
}

// fetchToken performs the anonymous half of the docker token flow: GET the
// challenge's realm with the service and scope as query params (default
// scope repository:<path>:pull when the challenge names none) and take the
// token from the JSON response — `token` per the spec, `access_token` as
// the OAuth2-shaped fallback some registries send instead. The realm must
// be https: a registry could otherwise redirect the "anonymous" flow
// through a channel we promised never to use.
func fetchToken(ctx context.Context, client *http.Client, params map[string]string, r Ref) (string, error) {
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("registry auth challenge has no realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("registry auth realm %q is not a valid url: %v", realm, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("registry auth realm %q is not https, refusing to fetch a token over an insecure channel", realm)
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + r.Path + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint %s not reachable: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %s (anonymous pull may not be allowed for this image)", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenBytes)).Decode(&body); err != nil {
		return "", fmt.Errorf("token endpoint sent malformed JSON: %v", err)
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return "", errors.New("token endpoint sent no token")
	}
	return token, nil
}

// drainClose reads at most a few KB of a response body and closes it, so
// the keep-alive connection is reusable across the 401 → token → retry hop.
func drainClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	_ = body.Close()
}
