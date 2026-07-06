package tracker

// The Registry resolves the right Tracker for a repo from its tracker binding
// (§3a: forge|builtin) and, for a forge binding, its forge kind — decrypting
// the repo's forge credential to build the REST client. It is the single seam
// the operator API (now) and the agent API (M5) go through to obtain a
// repo-scoped Tracker.
//
// Import-cycle note: the forge and built-in backends import this package for
// the Tracker interface and the shared types, so this package must NOT import
// them. The two backend constructors are therefore injected as function values
// (BuiltinFactory / ForgejoFactory) by the wiring layer, which is the one
// place that may import all three. See the reported constructor signatures.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// forgeHTTPTimeout is the per-request ceiling for the Forgejo REST client (the
// port-spec leaves the value to M4: tea ran with no deadline, a REST client
// must not). Applied to the default client NewRegistry synthesizes when the
// caller passes none.
const forgeHTTPTimeout = 30 * time.Second

// Registry-resolution errors. All are wrapped with the repo id and the
// offending value for diagnosis; none ever carry credential bytes.
var (
	// ErrForgeUnsupported: a forge-bound repo whose forge_kind lab has no REST
	// client for (github is fast-follow #1; none/unknown cannot be driven).
	ErrForgeUnsupported = errors.New("tracker: forge kind not supported")
	// ErrForgeCredentialMissing: a forge-bound repo with no forge credential
	// attached (the DB requires one at Add/PATCH; this guards the seam).
	ErrForgeCredentialMissing = errors.New("tracker: forge repo has no forge credential")
	// ErrForgeCredentialKind: the attached credential is not a forge_token.
	ErrForgeCredentialKind = errors.New("tracker: credential is not a forge token")
	// ErrUnknownBinding: a tracker_binding value that is neither forge nor
	// builtin.
	ErrUnknownBinding = errors.New("tracker: unknown tracker binding")
	// ErrRemotePath: a forge repo whose remote URL has no owner/repo pair.
	ErrRemotePath = errors.New("tracker: remote url has no owner/repo path")
	// ErrForgeHost: the forge credential's host field could not be normalized
	// into a bare host[:port] (wrong scheme, embedded path, or empty) — the
	// credential needs fixing, not the repo.
	ErrForgeHost = errors.New("tracker: invalid forge host in credential")
)

// BuiltinConfig is everything the store-backed tracker needs for one repo.
// tracker/builtin.New consumes it.
type BuiltinConfig struct {
	Store  *store.Store
	RepoID string
}

// ForgejoConfig is everything the Forgejo REST client needs for one repo.
// tracker/forgejo.New consumes it. Token is the decrypted forge_token and must
// never be logged. BaseURL is https://<host>/api/v1, host taken from the
// forge_token payload; Owner/Repo come from the remote URL.
type ForgejoConfig struct {
	HTTPClient *http.Client
	BaseURL    string
	Token      string
	Owner      string
	Repo       string
}

// BuiltinFactory constructs a store-backed Tracker. tracker/builtin.New has
// this exact signature; the wiring layer passes it to NewRegistry.
type BuiltinFactory func(BuiltinConfig) Tracker

// ForgejoFactory constructs a Forgejo REST-backed Tracker. tracker/forgejo.New
// has this exact signature; injected for the same no-cycle reason.
type ForgejoFactory func(ForgejoConfig) Tracker

// Registry builds a repo-scoped Tracker on demand. It holds the store and
// vault (for forge-credential decryption and the built-in tracker), the shared
// HTTP client every Forgejo client reuses, and the injected backend factories.
type Registry struct {
	store      *store.Store
	vault      *vault.Vault
	httpClient *http.Client
	newBuiltin BuiltinFactory
	newForgejo ForgejoFactory
	observe    Observer // optional metrics seam (instrument.go); nil → unwrapped
}

// NewRegistry builds a Registry. st and v back forge-credential decryption and
// the built-in tracker. httpClient is handed to every Forgejo client the
// registry builds; nil yields a client with the pinned forge HTTP timeout.
// builtin and forgejo are the backend constructors, injected to avoid an
// import cycle — both are required (a nil factory is a wiring bug).
func NewRegistry(st *store.Store, v *vault.Vault, httpClient *http.Client, builtin BuiltinFactory, forgejo ForgejoFactory) *Registry {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: forgeHTTPTimeout}
	}
	return &Registry{
		store:      st,
		vault:      v,
		httpClient: httpClient,
		newBuiltin: builtin,
		newForgejo: forgejo,
	}
}

// TrackerFor returns the Tracker that answers for repo. A builtin binding
// yields the store-backed tracker; a forge binding decrypts the repo's forge
// credential and builds the matching REST client (forgejo today; github is
// #1). An unknown binding, an unsupported or absent forge kind, a missing or
// wrong-kind forge credential, a credential that fails to decrypt, or a remote
// with no owner/repo pair are all errors — the caller never gets a half-built
// tracker. With an observer set (SetObserver), the returned tracker is
// wrapped so every call reports (binding, op, ok) — the metrics seam.
func (r *Registry) TrackerFor(ctx context.Context, repo store.Repo) (Tracker, error) {
	switch repo.TrackerBinding {
	case store.TrackerBindingBuiltin:
		return r.instrument(r.newBuiltin(BuiltinConfig{Store: r.store, RepoID: repo.ID}), store.TrackerBindingBuiltin), nil
	case store.TrackerBindingForge:
		trk, err := r.forgeTracker(ctx, repo)
		if err != nil {
			return nil, err
		}
		return r.instrument(trk, store.TrackerBindingForge), nil
	default:
		return nil, fmt.Errorf("tracker for repo %q: %w (%q)", repo.ID, ErrUnknownBinding, repo.TrackerBinding)
	}
}

// forgeTracker resolves the forge-bound branch of TrackerFor: validate the
// forge kind, decrypt the forge credential, and build the REST client scoped
// to the repo's owner/repo path.
func (r *Registry) forgeTracker(ctx context.Context, repo store.Repo) (Tracker, error) {
	switch ForgeKind(repo.ForgeKind) {
	case ForgeKindForgejo:
		// The one forge with a REST client today; fall through to build it.
	case ForgeKindGitHub:
		return nil, fmt.Errorf("tracker for repo %q: %w (github; issue #1)", repo.ID, ErrForgeUnsupported)
	default:
		return nil, fmt.Errorf("tracker for repo %q: %w (%q)", repo.ID, ErrForgeUnsupported, repo.ForgeKind)
	}

	if repo.ForgeCredentialID == nil {
		return nil, fmt.Errorf("tracker for repo %q: %w", repo.ID, ErrForgeCredentialMissing)
	}
	cred, err := r.store.CredentialByID(ctx, *repo.ForgeCredentialID)
	if err != nil {
		return nil, fmt.Errorf("tracker for repo %q: load forge credential: %w", repo.ID, err)
	}
	if cred.Kind != store.CredentialKindForgeToken {
		return nil, fmt.Errorf("tracker for repo %q: %w (%q)", repo.ID, ErrForgeCredentialKind, cred.Kind)
	}
	var payload vault.ForgeTokenPayload
	if err := r.vault.DecryptPayload(cred.EncryptedPayload, &payload); err != nil {
		return nil, fmt.Errorf("tracker for repo %q: decrypt forge credential: %w", repo.ID, err)
	}
	host, err := normalizeForgeHost(payload.Host)
	if err != nil {
		return nil, fmt.Errorf("tracker for repo %q: forge credential %q: %w", repo.ID, cred.Name, err)
	}

	path, ok := RepoPath(repo.RemoteURL)
	if !ok {
		return nil, fmt.Errorf("tracker for repo %q: %w", repo.ID, ErrRemotePath)
	}
	owner, name, _ := strings.Cut(path, "/") // RepoPath guarantees exactly two segments

	return r.newForgejo(ForgejoConfig{
		HTTPClient: r.httpClient,
		BaseURL:    "https://" + host + "/api/v1",
		Token:      payload.Token,
		Owner:      owner,
		Repo:       name,
	}), nil
}

// forgeHostScheme is the one scheme prefix normalizeForgeHost forgives in a
// credential's host field — lab talks to forges over https only.
const forgeHostScheme = "https://"

// normalizeForgeHost canonicalizes the host stored in a forge_token credential
// into the bare host[:port] the BaseURL is built from. Operators naturally
// paste a URL into a host field, so a leading "https://" and trailing slashes
// are forgiven; anything else that keeps "https://"+host from being exactly
// the API origin — another scheme (plain http included: lab does not send
// forge tokens over cleartext), an embedded path, userinfo, or an
// empty/garbled value — is rejected with ErrForgeHost so the failure points
// at the credential instead of surfacing as an opaque dial error on the first
// tracker call.
func normalizeForgeHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if len(host) >= len(forgeHostScheme) && strings.EqualFold(host[:len(forgeHostScheme)], forgeHostScheme) {
		host = host[len(forgeHostScheme):]
	} else if i := strings.Index(host, "://"); i >= 0 {
		return "", fmt.Errorf("%w: scheme %q not supported (use https:// or a bare host)", ErrForgeHost, host[:i])
	}
	host = strings.TrimRight(host, "/")
	if host == "" {
		return "", fmt.Errorf("%w: host is empty", ErrForgeHost)
	}
	if strings.Contains(host, "/") {
		return "", fmt.Errorf("%w: host %q carries a path", ErrForgeHost, host)
	}
	u, err := url.Parse(forgeHostScheme + host)
	if err != nil || u.Host != host || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: %q is not a bare host[:port]", ErrForgeHost, raw)
	}
	return host, nil
}
