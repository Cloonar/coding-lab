package tracker

// The Registry resolves the right Tracker for a repo from its tracker binding
// (§3a: forge|builtin) and, for a forge binding, the flavor of the repo's
// decrypted forge credential — forgejo or github (ADR-0015). It is the single
// seam the operator API, the agent API, and the AFK engine go through to
// obtain a repo-scoped Tracker.
//
// The credential's flavor — not repos.forge_kind — is the routing authority:
// forge_kind stays the auto-binding hint and the mismatch tripwire, but the
// operator's explicit credential decides which REST client is built, so
// arbitrary Forgejo instances and GitHub Enterprise hosts resolve from the
// credential alone.
//
// Import-cycle note: the forge/github/built-in backends import this package
// for the Tracker interface and the shared types, so this package must NOT
// import them. The backend constructors are therefore injected as function
// values (BuiltinFactory / ForgejoFactory / GitHubFactory) by the wiring
// layer, which is the one place that may import all of them. See the reported
// constructor signatures.

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
	// ErrForgeUnsupported: a forge credential whose flavor lab has no REST
	// client for. With the flavor validated at credential-create time
	// (forgejo|github), this is now defensive — a corrupted or pre-validation
	// payload with an unknown flavor.
	ErrForgeUnsupported = errors.New("tracker: forge flavor not supported")
	// ErrForgeCredentialMissing: a forge-bound repo with no forge credential
	// attached (the DB requires one at Add/PATCH; this guards the seam).
	ErrForgeCredentialMissing = errors.New("tracker: forge repo has no forge credential")
	// ErrForgeCredentialKind: the attached credential is not a forge_token.
	ErrForgeCredentialKind = errors.New("tracker: credential is not a forge token")
	// ErrForgeFlavorMismatch: the repo's detected forge host disagrees with
	// the credential's flavor (e.g. a github.com remote with a forgejo-flavored
	// credential) — a loud configuration error rather than a silent 404 on
	// first use. A forge_kind of 'none' (an unrecognized host: codeberg, a GHE
	// instance, a second private Forgejo) is exempt: the operator picked the
	// forge binding and its flavor deliberately, so the credential is trusted.
	ErrForgeFlavorMismatch = errors.New("tracker: forge credential flavor does not match the repo's detected host")
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

// CRMerger is the built-in change-request merge orchestration, injected into
// the built-in tracker so its MergePull reuses the ADR-0011 merge service the
// operator route uses — instead of reimplementing the per-CR serialization,
// cancellation-immunity, Closes-#N closure, and events. The concrete
// implementation (internal/crmerge.Service) is wired by the same layer that
// injects the backend factories (cmd/lab), so this package never imports it
// and no cycle forms. Merge lands the repo's open CR `number` and returns the
// merged row; a non-open CR yields the current row plus store.ErrCRNotOpen so
// the caller can decide whether an already-merged CR is a convergent success
// (the agent seam) or a conflict (the operator route).
type CRMerger interface {
	Merge(ctx context.Context, repoID string, number int) (store.CR, error)
}

// BuiltinConfig is everything the store-backed tracker needs for one repo.
// tracker/builtin.New consumes it.
type BuiltinConfig struct {
	Store  *store.Store
	RepoID string
	// Merger is the shared CR-merge service the built-in tracker's MergePull
	// routes into. Nil when the wiring layer did not provide one (degraded
	// wiring / a test that never merges): MergePull then fails loudly rather
	// than half-merging.
	Merger CRMerger
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

// GitHubConfig is everything the GitHub REST client needs for one repo.
// tracker/github.New consumes it. Token is the decrypted forge_token and must
// never be logged. BaseURL is the API origin verbatim (https://api.github.com,
// or a GHE root like https://ghe.example.com/api/v3), built from the
// forge_token payload's host with no derivation heuristics; Owner/Repo come
// from the remote URL.
type GitHubConfig struct {
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

// GitHubFactory constructs a GitHub REST-backed Tracker. tracker/github.New
// has this exact signature; injected for the same no-cycle reason.
type GitHubFactory func(GitHubConfig) Tracker

// Registry builds a repo-scoped Tracker on demand. It holds the store and
// vault (for forge-credential decryption and the built-in tracker), the shared
// HTTP client every Forgejo client reuses, and the injected backend factories.
type Registry struct {
	store      *store.Store
	vault      *vault.Vault
	httpClient *http.Client
	newBuiltin BuiltinFactory
	newForgejo ForgejoFactory
	newGitHub  GitHubFactory
	observe    Observer // optional metrics seam (instrument.go); nil → unwrapped
	merger     CRMerger // optional built-in CR-merge service; nil → MergePull fails loud
}

// NewRegistry builds a Registry. st and v back forge-credential decryption and
// the built-in tracker. httpClient is handed to every forge REST client the
// registry builds; nil yields a client with the pinned forge HTTP timeout.
// builtin, forgejo and github are the backend constructors, injected to avoid
// an import cycle — all are required (a nil factory is a wiring bug).
func NewRegistry(st *store.Store, v *vault.Vault, httpClient *http.Client, builtin BuiltinFactory, forgejo ForgejoFactory, github GitHubFactory) *Registry {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: forgeHTTPTimeout}
	}
	return &Registry{
		store:      st,
		vault:      v,
		httpClient: httpClient,
		newBuiltin: builtin,
		newForgejo: forgejo,
		newGitHub:  github,
	}
}

// SetCRMerger wires the built-in CR-merge service the built-in tracker's
// MergePull routes into. Call once during startup wiring, before any
// TrackerFor — the field is read without a lock, exactly like SetObserver.
// Left unset (tests, degraded wiring), a built-in MergePull fails loud.
func (r *Registry) SetCRMerger(m CRMerger) { r.merger = m }

// TrackerFor returns the Tracker that answers for repo. A builtin binding
// yields the store-backed tracker; a forge binding decrypts the repo's forge
// credential and builds the REST client its flavor names (forgejo or github).
// An unknown binding, an unsupported flavor, a flavor/host mismatch, a missing
// or wrong-kind forge credential, a credential that fails to decrypt, an
// invalid credential host, or a remote with no owner/repo pair are all errors
// — the caller never gets a half-built tracker. With an observer set (SetObserver), the returned tracker is
// wrapped so every call reports (binding, op, ok) — the metrics seam.
func (r *Registry) TrackerFor(ctx context.Context, repo store.Repo) (Tracker, error) {
	switch repo.TrackerBinding {
	case store.TrackerBindingBuiltin:
		return r.instrument(r.newBuiltin(BuiltinConfig{Store: r.store, RepoID: repo.ID, Merger: r.merger}), store.TrackerBindingBuiltin), nil
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

// forgeTracker resolves the forge-bound branch of TrackerFor: decrypt the
// forge credential, route on its flavor (the authority — not repos.forge_kind),
// and build the matching REST client scoped to the repo's owner/repo path. The
// credential load precedes the flavor decision because the flavor LIVES in the
// decrypted payload.
func (r *Registry) forgeTracker(ctx context.Context, repo store.Repo) (Tracker, error) {
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
	flavor := payload.ForgeFlavor()
	if flavor != vault.ForgeForgejo && flavor != vault.ForgeGitHub {
		return nil, fmt.Errorf("tracker for repo %q: %w (%q)", repo.ID, ErrForgeUnsupported, flavor)
	}

	// Mismatch tripwire: the credential's flavor routes, but a RECOGNIZED
	// remote host that disagrees with it is almost certainly a misconfiguration
	// (a github.com remote pointed at a forgejo credential, or vice versa) —
	// fail loud instead of surfacing a confusing 404 on first use. forge_kind
	// 'none' (an unrecognized host: codeberg, a GHE instance, a second private
	// Forgejo) is exempt: there is no detected truth to contradict, so the
	// operator's explicit flavor wins.
	if repo.ForgeKind != string(ForgeKindNone) && repo.ForgeKind != flavor {
		return nil, fmt.Errorf("tracker for repo %q: %w (host detected as %q, credential is %q)",
			repo.ID, ErrForgeFlavorMismatch, repo.ForgeKind, flavor)
	}

	host, err := NormalizeForgeHost(flavor, payload.Host)
	if err != nil {
		return nil, fmt.Errorf("tracker for repo %q: forge credential %q: %w", repo.ID, cred.Name, err)
	}

	path, ok := RepoPath(repo.RemoteURL)
	if !ok {
		return nil, fmt.Errorf("tracker for repo %q: %w", repo.ID, ErrRemotePath)
	}
	owner, name, _ := strings.Cut(path, "/") // RepoPath guarantees exactly two segments

	if flavor == vault.ForgeGitHub {
		// github's host IS the API origin (api.github.com, or a GHE root) —
		// used verbatim, no /api/v1 derivation (GHE URL layouts make any
		// heuristic silently wrong).
		return r.newGitHub(GitHubConfig{
			HTTPClient: r.httpClient,
			BaseURL:    "https://" + host,
			Token:      payload.Token,
			Owner:      owner,
			Repo:       name,
		}), nil
	}
	return r.newForgejo(ForgejoConfig{
		HTTPClient: r.httpClient,
		BaseURL:    "https://" + host + "/api/v1",
		Token:      payload.Token,
		Owner:      owner,
		Repo:       name,
	}), nil
}

// forgeHostScheme is the one scheme prefix NormalizeForgeHost forgives in a
// credential's host field — lab talks to forges over https only.
const forgeHostScheme = "https://"

// NormalizeForgeHost canonicalizes the host stored in a forge_token credential
// into the string the BaseURL is built from, flavor-aware. It is the single
// source of truth used at BOTH credential-create time (the API 400s) and
// tracker-resolve time (ErrForgeHost → 409), so a host that passes create can
// never be rejected at resolve. Operators naturally paste a URL into a host
// field, so a leading "https://" and trailing slashes are forgiven; another
// scheme (plain http included — lab does not send forge tokens over
// cleartext), userinfo, a query or a fragment are rejected for both flavors.
//
// The flavors differ on paths:
//
//   - forgejo: a bare host[:port]; the registry appends /api/v1, so any path
//     component would make the composed origin wrong.
//   - github: the API origin verbatim — a bare host (api.github.com) OR a
//     host+path (a GHE root like ghe.example.com/api/v3), used as
//     "https://"+host with no derivation. A path is therefore allowed.
func NormalizeForgeHost(flavor, raw string) (string, error) {
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

	if flavor == vault.ForgeGitHub {
		// A path is allowed (the GHE API root); userinfo/query/fragment/opaque
		// are not, and the reparse must round-trip so a space or stray
		// character cannot slip through.
		u, err := url.Parse(forgeHostScheme + host)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Host+u.EscapedPath() != host {
			return "", fmt.Errorf("%w: %q is not a valid https API origin", ErrForgeHost, raw)
		}
		return host, nil
	}

	// forgejo (and the empty→forgejo default): a bare host[:port].
	if strings.Contains(host, "/") {
		return "", fmt.Errorf("%w: host %q carries a path", ErrForgeHost, host)
	}
	u, err := url.Parse(forgeHostScheme + host)
	if err != nil || u.Host != host || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: %q is not a bare host[:port]", ErrForgeHost, raw)
	}
	return host, nil
}
