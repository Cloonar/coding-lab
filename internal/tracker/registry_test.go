package tracker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// stubTracker is a no-op Tracker; the fakes embed it so a factory can return a
// value implementing the interface while capturing the config it was handed.
type stubTracker struct{}

func (stubTracker) ReadyIssues(context.Context) ([]Issue, error)     { return nil, nil }
func (stubTracker) Issues(context.Context, string) ([]Issue, error)  { return nil, nil }
func (stubTracker) Issue(context.Context, int) (Issue, error)        { return Issue{}, nil }
func (stubTracker) CreateComment(context.Context, int, string) error { return nil }
func (stubTracker) Pulls(context.Context) ([]PullRef, error)         { return nil, nil }
func (stubTracker) Pull(context.Context, int) (PullDetail, error)    { return PullDetail{}, nil }
func (stubTracker) Checks(context.Context, int) ([]Check, error)     { return nil, nil }
func (stubTracker) CreatePull(context.Context, string, string, string, string) (PullRef, error) {
	return PullRef{}, nil
}
func (stubTracker) MergePull(context.Context, int) (PullRef, error) { return PullRef{}, nil }
func (stubTracker) Reviews(context.Context, int) ([]Review, error)  { return nil, nil }
func (stubTracker) RejectPull(context.Context, int, string) (Review, error) {
	return Review{}, nil
}
func (stubTracker) ApprovePull(context.Context, int, string) (Review, error) {
	return Review{}, nil
}
func (stubTracker) RerequestReview(context.Context, int) error     { return nil }
func (stubTracker) CommentPull(context.Context, int, string) error { return nil }
func (stubTracker) CloseIssue(context.Context, int) error          { return nil }
func (stubTracker) CreateIssue(context.Context, string, string, []string) (Issue, error) {
	return Issue{}, nil
}
func (stubTracker) EditIssue(context.Context, int, IssueEdit) (Issue, error) {
	return Issue{}, nil
}
func (stubTracker) AddIssueLabels(context.Context, int, []string) error    { return nil }
func (stubTracker) RemoveIssueLabels(context.Context, int, []string) error { return nil }
func (stubTracker) Labels(context.Context) ([]Label, error)                { return nil, nil }
func (stubTracker) EnsureLabel(context.Context, string, string, string) (Label, error) {
	return Label{}, nil
}

type fakeBuiltin struct {
	stubTracker
	cfg BuiltinConfig
}

type fakeForgejo struct {
	stubTracker
	cfg ForgejoConfig
}

type fakeGitHub struct {
	stubTracker
	cfg GitHubConfig
}

// registryFixture wires a Registry over a real store + vault with fake backend
// factories, so a resolved Tracker's captured config can be asserted.
type registryFixture struct {
	reg   *Registry
	store *store.Store
	vault *vault.Vault
}

func newRegistryFixture(t *testing.T) registryFixture {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "lab.db"), logger)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	v, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("new vault: %v", err)
	}

	reg := NewRegistry(st, v, nil,
		func(c BuiltinConfig) Tracker { return fakeBuiltin{cfg: c} },
		func(c ForgejoConfig) Tracker { return fakeForgejo{cfg: c} },
		func(c GitHubConfig) Tracker { return fakeGitHub{cfg: c} },
	)
	return registryFixture{reg: reg, store: st, vault: v}
}

// forgeCred stores a forgejo-flavored forge_token credential (the absent-flavor
// default) encrypted under the fixture's vault and returns its id.
func (f registryFixture) forgeCred(t *testing.T, host, token string) string {
	return f.forgeCredFlavor(t, host, token, "")
}

// forgeCredFlavor stores a forge_token credential with an explicit flavor
// (empty → the forgejo default) and returns its id.
func (f registryFixture) forgeCredFlavor(t *testing.T, host, token, flavor string) string {
	t.Helper()
	id := ids.NewID("cred")
	blob, err := f.vault.EncryptPayload(vault.ForgeTokenPayload{Host: host, Token: token, Forge: flavor})
	if err != nil {
		t.Fatalf("encrypt forge payload: %v", err)
	}
	if _, err := f.store.CreateCredential(context.Background(), id, "cred-"+id,
		store.CredentialKindForgeToken, blob, time.Now()); err != nil {
		t.Fatalf("create forge credential: %v", err)
	}
	return id
}

// cred stores a credential of an arbitrary kind with the given payload bytes
// (used for the wrong-kind and un-decryptable cases) and returns its id.
func (f registryFixture) cred(t *testing.T, kind string, payload []byte) string {
	t.Helper()
	id := ids.NewID("cred")
	if _, err := f.store.CreateCredential(context.Background(), id, "cred-"+id,
		kind, payload, time.Now()); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	return id
}

func TestTrackerForBuiltin(t *testing.T) {
	f := newRegistryFixture(t)
	repo := store.Repo{ID: "repo_builtin", TrackerBinding: store.TrackerBindingBuiltin}

	tr, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor builtin: %v", err)
	}
	fb, ok := tr.(fakeBuiltin)
	if !ok {
		t.Fatalf("TrackerFor builtin returned %T, want the builtin backend", tr)
	}
	if fb.cfg.RepoID != "repo_builtin" {
		t.Errorf("builtin cfg.RepoID = %q, want %q", fb.cfg.RepoID, "repo_builtin")
	}
	if fb.cfg.Store != f.store {
		t.Errorf("builtin cfg.Store = %p, want the registry's store %p", fb.cfg.Store, f.store)
	}
}

func TestTrackerForForgejo(t *testing.T) {
	f := newRegistryFixture(t)
	credID := f.forgeCred(t, "git.cloonar.com", "sekret-token")
	repo := store.Repo{
		ID:                "repo_forge",
		TrackerBinding:    store.TrackerBindingForge,
		ForgeKind:         string(ForgeKindForgejo),
		ForgeCredentialID: &credID,
		RemoteURL:         "forgejo@git.cloonar.com:Cloonar/nixos.git",
	}

	tr, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor forgejo: %v", err)
	}
	ff, ok := tr.(fakeForgejo)
	if !ok {
		t.Fatalf("TrackerFor forgejo returned %T, want the forgejo backend", tr)
	}
	if want := "https://git.cloonar.com/api/v1"; ff.cfg.BaseURL != want {
		t.Errorf("forgejo cfg.BaseURL = %q, want %q", ff.cfg.BaseURL, want)
	}
	if ff.cfg.Token != "sekret-token" {
		t.Errorf("forgejo cfg.Token = %q, want the decrypted forge token", ff.cfg.Token)
	}
	if ff.cfg.Owner != "Cloonar" || ff.cfg.Repo != "nixos" {
		t.Errorf("forgejo cfg owner/repo = %q/%q, want Cloonar/nixos", ff.cfg.Owner, ff.cfg.Repo)
	}
	if ff.cfg.HTTPClient == nil {
		t.Error("forgejo cfg.HTTPClient is nil; registry must supply a client")
	}
}

// TestTrackerForForgejoHostNormalized: operators naturally paste a URL into
// the credential's host field. A https:// prefix and trailing slashes are
// forgiven — the composed BaseURL must still be exactly
// https://<host>/api/v1, never https://https://… (which dials host "https")
// or …//api/v1.
func TestTrackerForForgejoHostNormalized(t *testing.T) {
	f := newRegistryFixture(t)
	for _, tc := range []struct {
		name, host, wantBase string
	}{
		{"bare host unchanged", "git.cloonar.com", "https://git.cloonar.com/api/v1"},
		{"https prefix stripped", "https://git.cloonar.com", "https://git.cloonar.com/api/v1"},
		{"trailing slash trimmed", "git.cloonar.com/", "https://git.cloonar.com/api/v1"},
		{"prefix and slashes", "https://git.cloonar.com//", "https://git.cloonar.com/api/v1"},
		{"surrounding whitespace", "  git.cloonar.com ", "https://git.cloonar.com/api/v1"},
		{"port kept", "git.cloonar.com:3000", "https://git.cloonar.com:3000/api/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credID := f.forgeCred(t, tc.host, "tok")
			repo := store.Repo{
				ID:                "repo_norm",
				TrackerBinding:    store.TrackerBindingForge,
				ForgeKind:         string(ForgeKindForgejo),
				ForgeCredentialID: &credID,
				RemoteURL:         "forgejo@git.cloonar.com:Cloonar/nixos.git",
			}
			tr, err := f.reg.TrackerFor(context.Background(), repo)
			if err != nil {
				t.Fatalf("TrackerFor(host %q): %v", tc.host, err)
			}
			ff := tr.(fakeForgejo)
			if ff.cfg.BaseURL != tc.wantBase {
				t.Errorf("BaseURL = %q; want %q", ff.cfg.BaseURL, tc.wantBase)
			}
		})
	}
}

// TestTrackerForForgejoHostRejected: hosts that cannot be normalized into a
// bare host[:port] fail with ErrForgeHost, and the error names the credential
// so the operator fixes the right thing instead of chasing a dial error.
func TestTrackerForForgejoHostRejected(t *testing.T) {
	f := newRegistryFixture(t)
	for _, tc := range []struct{ name, host string }{
		{"http scheme", "http://git.cloonar.com"},
		{"other scheme", "ssh://git.cloonar.com"},
		{"empty", ""},
		{"whitespace only", "   "},
		{"bare scheme", "https://"},
		{"path-carrying host", "git.cloonar.com/api"},
		{"url with path", "https://git.cloonar.com/Cloonar/nixos"},
		{"userinfo in host", "token@git.cloonar.com"},
		{"space inside host", "git cloonar.com"},
		{"query in host", "git.cloonar.com?x=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credID := f.forgeCred(t, tc.host, "tok")
			repo := store.Repo{
				ID:                "repo_badhost",
				TrackerBinding:    store.TrackerBindingForge,
				ForgeKind:         string(ForgeKindForgejo),
				ForgeCredentialID: &credID,
				RemoteURL:         "forgejo@git.cloonar.com:Cloonar/nixos.git",
			}
			tr, err := f.reg.TrackerFor(context.Background(), repo)
			if !errors.Is(err, ErrForgeHost) {
				t.Fatalf("TrackerFor(host %q) err = %v; want errors.Is ErrForgeHost", tc.host, err)
			}
			if tr != nil {
				t.Errorf("TrackerFor returned a non-nil tracker (%T) alongside the error", tr)
			}
			if !strings.Contains(err.Error(), "cred-"+credID) {
				t.Errorf("error %q does not name the offending credential %q", err, "cred-"+credID)
			}
		})
	}
}

// TestTrackerForGitHub: a github-flavored credential routes to the GitHub
// backend, and the BaseURL is the API origin verbatim (no /api/v1 derivation).
func TestTrackerForGitHub(t *testing.T) {
	f := newRegistryFixture(t)
	credID := f.forgeCredFlavor(t, "api.github.com", "gh-token", vault.ForgeGitHub)
	repo := store.Repo{
		ID:                "repo_gh",
		TrackerBinding:    store.TrackerBindingForge,
		ForgeKind:         string(ForgeKindGitHub),
		ForgeCredentialID: &credID,
		RemoteURL:         "git@github.com:octocat/hello-world.git",
	}
	tr, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor github: %v", err)
	}
	gh, ok := tr.(fakeGitHub)
	if !ok {
		t.Fatalf("TrackerFor github returned %T, want the github backend", tr)
	}
	if want := "https://api.github.com"; gh.cfg.BaseURL != want {
		t.Errorf("github cfg.BaseURL = %q, want %q (no /api/v1 derivation)", gh.cfg.BaseURL, want)
	}
	if gh.cfg.Token != "gh-token" || gh.cfg.Owner != "octocat" || gh.cfg.Repo != "hello-world" {
		t.Errorf("github cfg = %+v", gh.cfg)
	}
	if gh.cfg.HTTPClient == nil {
		t.Error("github cfg.HTTPClient is nil; registry must supply a client")
	}
}

// TestTrackerForGitHubEnterprise: a GHE credential's host is the real API root
// (bare host under subdomain isolation, or host+path); the registry uses it
// verbatim, never guessing a layout. A GHE host is not github.com, so
// forge_kind is 'none' — exempt from the mismatch tripwire.
func TestTrackerForGitHubEnterprise(t *testing.T) {
	f := newRegistryFixture(t)
	for _, tc := range []struct{ name, host, wantBase string }{
		{"subdomain-isolated root", "api.ghe.example.com", "https://api.ghe.example.com"},
		{"path-style API root", "ghe.example.com/api/v3", "https://ghe.example.com/api/v3"},
		{"https prefix stripped", "https://ghe.example.com/api/v3", "https://ghe.example.com/api/v3"},
		{"trailing slash trimmed", "ghe.example.com/api/v3/", "https://ghe.example.com/api/v3"},
		{"port kept", "ghe.example.com:8443/api/v3", "https://ghe.example.com:8443/api/v3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credID := f.forgeCredFlavor(t, tc.host, "tok", vault.ForgeGitHub)
			repo := store.Repo{
				ID:                "repo_ghe",
				TrackerBinding:    store.TrackerBindingForge,
				ForgeKind:         string(ForgeKindNone),
				ForgeCredentialID: &credID,
				RemoteURL:         "git@ghe.example.com:team/proj.git",
			}
			tr, err := f.reg.TrackerFor(context.Background(), repo)
			if err != nil {
				t.Fatalf("TrackerFor(host %q): %v", tc.host, err)
			}
			gh := tr.(fakeGitHub)
			if gh.cfg.BaseURL != tc.wantBase {
				t.Errorf("BaseURL = %q; want %q", gh.cfg.BaseURL, tc.wantBase)
			}
		})
	}
}

// TestTrackerForGitHubHostRejected: github-flavor hosts that are not a valid
// https API origin fail with ErrForgeHost naming the credential.
func TestTrackerForGitHubHostRejected(t *testing.T) {
	f := newRegistryFixture(t)
	for _, tc := range []struct{ name, host string }{
		{"http scheme", "http://api.github.com"},
		{"other scheme", "ssh://api.github.com"},
		{"empty", ""},
		{"whitespace only", "   "},
		{"bare scheme", "https://"},
		{"userinfo", "tok@api.github.com"},
		{"query", "api.github.com?x=1"},
		{"space inside host", "api github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credID := f.forgeCredFlavor(t, tc.host, "tok", vault.ForgeGitHub)
			repo := store.Repo{
				ID:                "repo_ghbad",
				TrackerBinding:    store.TrackerBindingForge,
				ForgeKind:         string(ForgeKindNone),
				ForgeCredentialID: &credID,
				RemoteURL:         "git@ghe.example.com:team/proj.git",
			}
			tr, err := f.reg.TrackerFor(context.Background(), repo)
			if !errors.Is(err, ErrForgeHost) {
				t.Fatalf("TrackerFor(host %q) err = %v; want ErrForgeHost", tc.host, err)
			}
			if tr != nil {
				t.Errorf("TrackerFor returned a non-nil tracker (%T) alongside the error", tr)
			}
			if !strings.Contains(err.Error(), "cred-"+credID) {
				t.Errorf("error %q does not name the offending credential", err)
			}
		})
	}
}

// TestTrackerForAbsentFlavorDefaultsForgejo: a credential written before
// flavors existed (no forge field) decodes as forgejo and routes there —
// correct by construction for every pre-ADR-0015 credential.
func TestTrackerForAbsentFlavorDefaultsForgejo(t *testing.T) {
	f := newRegistryFixture(t)
	credID := f.forgeCred(t, "git.cloonar.com", "tok") // no explicit flavor
	repo := store.Repo{
		ID:                "repo_legacy",
		TrackerBinding:    store.TrackerBindingForge,
		ForgeKind:         string(ForgeKindForgejo),
		ForgeCredentialID: &credID,
		RemoteURL:         "forgejo@git.cloonar.com:Cloonar/nixos.git",
	}
	tr, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	if _, ok := tr.(fakeForgejo); !ok {
		t.Fatalf("absent-flavor credential routed to %T, want the forgejo backend", tr)
	}
}

// TestTrackerForFlavorMismatch: a recognized host that disagrees with the
// credential's flavor is a loud ErrForgeFlavorMismatch, both directions.
func TestTrackerForFlavorMismatch(t *testing.T) {
	f := newRegistryFixture(t)
	for _, tc := range []struct {
		name, forgeKind, flavor, remote string
	}{
		{"github host, forgejo cred", string(ForgeKindGitHub), vault.ForgeForgejo, "git@github.com:o/r.git"},
		{"forgejo host, github cred", string(ForgeKindForgejo), vault.ForgeGitHub, "forgejo@git.cloonar.com:o/r.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credID := f.forgeCredFlavor(t, "api.github.com", "tok", tc.flavor)
			repo := store.Repo{
				ID:                "repo_mm",
				TrackerBinding:    store.TrackerBindingForge,
				ForgeKind:         tc.forgeKind,
				ForgeCredentialID: &credID,
				RemoteURL:         tc.remote,
			}
			_, err := f.reg.TrackerFor(context.Background(), repo)
			if !errors.Is(err, ErrForgeFlavorMismatch) {
				t.Fatalf("err = %v, want ErrForgeFlavorMismatch", err)
			}
		})
	}
}

// TestTrackerForNoneKindResolvesFromCredential: an unrecognized host
// (forge_kind 'none') resolves from the credential's flavor alone — the
// arbitrary-Forgejo-instance and GitHub-Enterprise unlock (ADR-0015).
func TestTrackerForNoneKindResolvesFromCredential(t *testing.T) {
	f := newRegistryFixture(t)

	fjCred := f.forgeCredFlavor(t, "codeberg.org", "tok", vault.ForgeForgejo)
	fjRepo := store.Repo{
		ID:                "repo_codeberg",
		TrackerBinding:    store.TrackerBindingForge,
		ForgeKind:         string(ForgeKindNone),
		ForgeCredentialID: &fjCred,
		RemoteURL:         "git@codeberg.org:team/proj.git",
	}
	tr, err := f.reg.TrackerFor(context.Background(), fjRepo)
	if err != nil {
		t.Fatalf("TrackerFor codeberg: %v", err)
	}
	fj, ok := tr.(fakeForgejo)
	if !ok {
		t.Fatalf("codeberg routed to %T, want forgejo", tr)
	}
	if want := "https://codeberg.org/api/v1"; fj.cfg.BaseURL != want {
		t.Errorf("codeberg BaseURL = %q, want %q", fj.cfg.BaseURL, want)
	}

	ghCred := f.forgeCredFlavor(t, "ghe.example.com/api/v3", "tok", vault.ForgeGitHub)
	ghRepo := store.Repo{
		ID:                "repo_ghe2",
		TrackerBinding:    store.TrackerBindingForge,
		ForgeKind:         string(ForgeKindNone),
		ForgeCredentialID: &ghCred,
		RemoteURL:         "git@ghe.example.com:team/proj.git",
	}
	tr, err = f.reg.TrackerFor(context.Background(), ghRepo)
	if err != nil {
		t.Fatalf("TrackerFor GHE: %v", err)
	}
	if _, ok := tr.(fakeGitHub); !ok {
		t.Fatalf("GHE routed to %T, want github", tr)
	}
}

func TestTrackerForForgeErrors(t *testing.T) {
	f := newRegistryFixture(t)
	forgeID := f.forgeCred(t, "git.cloonar.com", "tok")
	httpsID := f.cred(t, store.CredentialKindHTTPSToken, []byte("ignored"))
	badBlobID := f.cred(t, store.CredentialKindForgeToken, []byte("too-short-to-be-gcm"))

	for _, tc := range []struct {
		name    string
		repo    store.Repo
		wantErr error
	}{
		{
			// github host + forgejo credential: the mismatch tripwire fires
			// (ADR-0015) instead of the old "github unsupported" error.
			"github host with a forgejo credential is a mismatch",
			store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindGitHub), ForgeCredentialID: &forgeID, RemoteURL: "git@github.com:foo/bar.git"},
			ErrForgeFlavorMismatch,
		},
		{
			"forge binding without a forge credential",
			store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: nil, RemoteURL: "forgejo@git.cloonar.com:Cloonar/nixos.git"},
			ErrForgeCredentialMissing,
		},
		{
			"forge credential of the wrong kind",
			store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: &httpsID, RemoteURL: "forgejo@git.cloonar.com:Cloonar/nixos.git"},
			ErrForgeCredentialKind,
		},
		{
			"forge credential that fails to decrypt",
			store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: &badBlobID, RemoteURL: "forgejo@git.cloonar.com:Cloonar/nixos.git"},
			vault.ErrDecrypt,
		},
		{
			"forge remote with no owner/repo pair",
			store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: &forgeID, RemoteURL: "https://git.cloonar.com/Cloonar"},
			ErrRemotePath,
		},
		{
			"unknown tracker binding",
			store.Repo{ID: "r", TrackerBinding: "bogus"},
			ErrUnknownBinding,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := f.reg.TrackerFor(context.Background(), tc.repo)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("TrackerFor err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if tr != nil {
				t.Errorf("TrackerFor returned a non-nil tracker (%T) alongside an error", tr)
			}
		})
	}
}

// TestTrackerForMissingForgeCredentialNotFound covers the store lookup miss:
// a forge repo whose forge_credential_id points at a credential that no longer
// exists surfaces store.ErrNotFound (defensive — the FK normally prevents it).
func TestTrackerForMissingForgeCredentialNotFound(t *testing.T) {
	f := newRegistryFixture(t)
	missing := "cred_00000000000000000000000000000000"
	repo := store.Repo{
		ID:                "r",
		TrackerBinding:    store.TrackerBindingForge,
		ForgeKind:         string(ForgeKindForgejo),
		ForgeCredentialID: &missing,
		RemoteURL:         "forgejo@git.cloonar.com:Cloonar/nixos.git",
	}
	_, err := f.reg.TrackerFor(context.Background(), repo)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("TrackerFor err = %v, want errors.Is store.ErrNotFound", err)
	}
}
