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
func (stubTracker) CreatePull(context.Context, string, string, string, string) (PullRef, error) {
	return PullRef{}, nil
}
func (stubTracker) CloseIssue(context.Context, int) error { return nil }
func (stubTracker) CreateIssue(context.Context, string, string, []string) (Issue, error) {
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
	)
	return registryFixture{reg: reg, store: st, vault: v}
}

// forgeCred stores a forge_token credential encrypted under the fixture's
// vault and returns its id.
func (f registryFixture) forgeCred(t *testing.T, host, token string) string {
	t.Helper()
	id := ids.NewID("cred")
	blob, err := f.vault.EncryptPayload(vault.ForgeTokenPayload{Host: host, Token: token})
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
			"github forge kind is unsupported (issue #1)",
			store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindGitHub), ForgeCredentialID: &forgeID, RemoteURL: "git@github.com:foo/bar.git"},
			ErrForgeUnsupported,
		},
		{
			"none forge kind is unsupported",
			store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindNone), ForgeCredentialID: &forgeID, RemoteURL: "forgejo@git.cloonar.com:Cloonar/nixos.git"},
			ErrForgeUnsupported,
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
