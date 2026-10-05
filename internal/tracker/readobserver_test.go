package tracker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// The read observer (issue #61): the outcome of every LIST read of a
// forge-bound repo's tracker, with the error and the credential version it
// ran under — what the readiness report's tracker check is built from.

// scriptedForge is a forge backend whose list reads answer from a script.
type scriptedForge struct {
	stubTracker
	err    error
	issues []Issue
}

func (s scriptedForge) ReadyIssues(context.Context) ([]Issue, error)    { return nil, s.err }
func (s scriptedForge) Issues(context.Context, string) ([]Issue, error) { return s.issues, s.err }
func (s scriptedForge) Pulls(context.Context) ([]PullRef, error)        { return nil, s.err }
func (s scriptedForge) PullsForHead(context.Context, string, string) ([]PullRef, error) {
	return nil, s.err
}
func (s scriptedForge) Issue(context.Context, int) (Issue, error)        { return Issue{}, s.err }
func (s scriptedForge) Pull(context.Context, int) (PullDetail, error)    { return PullDetail{}, s.err }
func (s scriptedForge) Checks(context.Context, int) ([]Check, error)     { return nil, s.err }
func (s scriptedForge) Labels(context.Context) ([]Label, error)          { return nil, s.err }
func (s scriptedForge) CreateComment(context.Context, int, string) error { return s.err }

// readLog collects what the read observer was told.
type readLog struct{ got []ListRead }

func (l *readLog) observe(r ListRead) { l.got = append(l.got, r) }

func (l *readLog) take() []ListRead {
	got := l.got
	l.got = nil
	return got
}

// forgeRepo is a forgejo-bound repo over a real stored credential.
func (f registryFixture) forgeRepo(t *testing.T) (store.Repo, string) {
	t.Helper()
	credID := f.forgeCred(t, "git.cloonar.com", "tok")
	return store.Repo{
		ID: "repo_forge", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo),
		ForgeCredentialID: &credID, RemoteURL: "forgejo@git.cloonar.com:Cloonar/nixos.git",
	}, credID
}

// The four list reads — and only those — are reported, attributed to the
// repo and to the exact version of the forge credential the client was
// built from.
func TestReadObserver_ReportsForgeListReads(t *testing.T) {
	f := newRegistryFixture(t)
	log := &readLog{}
	f.reg.SetReadObserver(log.observe)
	f.reg.newForgejo = func(ForgejoConfig) Tracker { return scriptedForge{} }
	repo, _ := f.forgeRepo(t)
	stamp := f.store.CredentialStampByID(context.Background(), repo.ForgeCredentialID)
	if stamp == "" || stamp == store.NoCredentialStamp {
		t.Fatalf("fixture credential stamp = %q", stamp)
	}

	trk, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	driveAll(t, trk) // every Tracker method once

	got := log.take()
	wantOps := []string{OpReadyIssues, OpIssues, OpPulls, OpPullsForHead}
	if len(got) != len(wantOps) {
		t.Fatalf("%d reads reported, want the %d list reads %v: %+v", len(got), len(wantOps), wantOps, got)
	}
	for i, r := range got {
		if r.Op != wantOps[i] || r.RepoID != repo.ID || r.Credential != stamp || r.Err != nil {
			t.Errorf("read %d = %+v, want op %s for %s under %s, no error", i, r, wantOps[i], repo.ID, stamp)
		}
	}
	// Only the open-set read carries a count (driveAll reads the open view).
	if got[0].OpenIssues != -1 || got[1].OpenIssues != 0 || got[2].OpenIssues != -1 || got[3].OpenIssues != -1 {
		t.Errorf("open issue counts = %d %d %d %d, want -1 0 -1 -1",
			got[0].OpenIssues, got[1].OpenIssues, got[2].OpenIssues, got[3].OpenIssues)
	}
}

// A failing list read reports the error itself — the operator needs the
// forge's own words, and errors.Is must still see through it — while a
// failing single-subject read (one issue, one pull) reports nothing: its
// failure is about that subject, not about the repo.
func TestReadObserver_ReportsTheErrorOfAFailingListRead(t *testing.T) {
	f := newRegistryFixture(t)
	log := &readLog{}
	f.reg.SetReadObserver(log.observe)
	forgeErr := fmt.Errorf("forgejo GET /repos/Cloonar/nixos/issues: unexpected status 404: %w", ErrNotFound)
	f.reg.newForgejo = func(ForgejoConfig) Tracker { return scriptedForge{err: forgeErr, issues: []Issue{{State: StateOpen}}} }
	repo, _ := f.forgeRepo(t)

	trk, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	ctx := context.Background()
	_, _ = trk.Issue(ctx, 1)
	_, _ = trk.Pull(ctx, 1)
	_, _ = trk.Checks(ctx, 1)
	_, _ = trk.Labels(ctx)
	_ = trk.CreateComment(ctx, 1, "x")
	if got := log.take(); len(got) != 0 {
		t.Fatalf("non-list reads were reported: %+v", got)
	}

	if _, err := trk.Issues(ctx, StateOpen); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Issues err = %v, want it passed through untouched", err)
	}
	got := log.take()
	if len(got) != 1 {
		t.Fatalf("reads reported = %+v, want one", got)
	}
	if got[0].Err != forgeErr || !errors.Is(got[0].Err, ErrNotFound) {
		t.Errorf("reported error = %v, want the forge client's own error", got[0].Err)
	}
	if got[0].OpenIssues != -1 {
		t.Errorf("a failed read carried an open issue count: %d", got[0].OpenIssues)
	}
}

// The open issue count is the open rows of an open or all view — the closed
// view carries none.
func TestReadObserver_OpenIssueCount(t *testing.T) {
	f := newRegistryFixture(t)
	log := &readLog{}
	f.reg.SetReadObserver(log.observe)
	f.reg.newForgejo = func(ForgejoConfig) Tracker {
		return scriptedForge{issues: []Issue{{Number: 1, State: StateOpen}, {Number: 2, State: StateClosed}, {Number: 3, State: StateOpen}}}
	}
	repo, _ := f.forgeRepo(t)
	trk, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	for state, want := range map[string]int{StateOpen: 2, StateAll: 2, StateClosed: -1} {
		if _, err := trk.Issues(context.Background(), state); err != nil {
			t.Fatal(err)
		}
		got := log.take()
		if len(got) != 1 || got[0].OpenIssues != want {
			t.Errorf("Issues(%s) reported %+v, want an open count of %d", state, got, want)
		}
	}
}

// Three reads are never reported: a builtin-bound repo's (a store query), one
// whose caller is already gone, and — covered above — anything not a list.
func TestReadObserver_BuiltinAndCancelledReadsAreNotReported(t *testing.T) {
	f := newRegistryFixture(t)
	log := &readLog{}
	f.reg.SetReadObserver(log.observe)

	// With ONLY the read observer wired, the builtin tracker stays unwrapped
	// (the compatibility contract of TestNoObserverReturnsBackendUnwrapped).
	builtin, err := f.reg.TrackerFor(context.Background(), store.Repo{ID: "repo_b", TrackerBinding: store.TrackerBindingBuiltin})
	if err != nil {
		t.Fatalf("TrackerFor builtin: %v", err)
	}
	if _, ok := builtin.(fakeBuiltin); !ok {
		t.Fatalf("builtin tracker is %T, want the bare backend", builtin)
	}
	// And with the metrics observer wired too, it is wrapped but still silent
	// on the read seam.
	metrics := &observerLog{}
	f.reg.SetObserver(metrics.observe)
	builtin, err = f.reg.TrackerFor(context.Background(), store.Repo{ID: "repo_b", TrackerBinding: store.TrackerBindingBuiltin})
	if err != nil {
		t.Fatalf("TrackerFor builtin: %v", err)
	}
	driveAll(t, builtin)
	if got := log.take(); len(got) != 0 {
		t.Fatalf("builtin reads were reported: %+v", got)
	}
	if len(metrics.got) != len(opOrder) {
		t.Fatalf("metrics observer saw %d calls, want %d", len(metrics.got), len(opOrder))
	}

	f.reg.newForgejo = func(ForgejoConfig) Tracker { return scriptedForge{err: context.Canceled} }
	repo, _ := f.forgeRepo(t)
	trk, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = trk.ReadyIssues(cancelled)
	_, _ = trk.Issues(cancelled, StateOpen)
	_, _ = trk.Pulls(cancelled)
	_, _ = trk.PullsForHead(cancelled, "afk/1", "main")
	if got := log.take(); len(got) != 0 {
		t.Fatalf("reads under a cancelled context were reported: %+v", got)
	}
	// The metrics seam still counts them — it is a different seam.
	if n := len(metrics.got); n != len(opOrder)+4 {
		t.Fatalf("metrics observer saw %d calls, want %d", n, len(opOrder)+4)
	}
}

// rescopedForge is a forge backend with the RunScoper seam.
type rescopedForge struct {
	scriptedForge
	runID string
}

func (r rescopedForge) ForRun(runID string) Tracker {
	r.runID = runID
	return r
}

// A run-scoped tracker (the agent API's) keeps reporting its list reads.
func TestReadObserver_SurvivesForRun(t *testing.T) {
	f := newRegistryFixture(t)
	log := &readLog{}
	f.reg.SetReadObserver(log.observe)
	f.reg.newForgejo = func(ForgejoConfig) Tracker { return rescopedForge{} }
	repo, _ := f.forgeRepo(t)
	trk, err := f.reg.TrackerFor(context.Background(), repo)
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	scoped := trk.(RunScoper).ForRun("run_1")
	if backend, ok := scoped.(*observed).t.(rescopedForge); !ok || backend.runID != "run_1" {
		t.Fatalf("rescoped backend = %#v", scoped.(*observed).t)
	}
	if _, err := scoped.ReadyIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := log.take()
	if len(got) != 1 || got[0].RepoID != repo.ID || got[0].Op != OpReadyIssues || got[0].Credential == "" {
		t.Fatalf("rescoped read reported %+v", got)
	}
}

// The credential stamp follows the credential row: a rotation changes what
// the NEXT resolved tracker attributes its reads to.
func TestReadObserver_CredentialStampFollowsRotation(t *testing.T) {
	f := newRegistryFixture(t)
	log := &readLog{}
	f.reg.SetReadObserver(log.observe)
	f.reg.newForgejo = func(ForgejoConfig) Tracker { return scriptedForge{} }
	repo, credID := f.forgeRepo(t)
	read := func() string {
		t.Helper()
		trk, err := f.reg.TrackerFor(context.Background(), repo)
		if err != nil {
			t.Fatalf("TrackerFor: %v", err)
		}
		if _, err := trk.ReadyIssues(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := log.take()
		if len(got) != 1 {
			t.Fatalf("reads = %+v", got)
		}
		return got[0].Credential
	}

	before := read()
	blob, err := f.vault.EncryptPayload(vault.ForgeTokenPayload{Host: "git.cloonar.com", Token: "rotated"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateCredential(context.Background(), credID, nil, blob, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	after := read()
	if after == before {
		t.Fatalf("stamp unchanged by a rotation: %q", after)
	}
	if want := f.store.CredentialStampByID(context.Background(), &credID); after != want {
		t.Fatalf("stamp after rotation = %q, want the store's %q", after, want)
	}
}

// CheckConfig is TrackerFor's validation without the tracker: the same
// errors for the same reasons, and no client is ever constructed — which is
// what lets a page view ask it.
func TestCheckConfig(t *testing.T) {
	f := newRegistryFixture(t)
	built := 0
	f.reg.newBuiltin = func(BuiltinConfig) Tracker { built++; return stubTracker{} }
	f.reg.newForgejo = func(ForgejoConfig) Tracker { built++; return stubTracker{} }
	f.reg.newGitHub = func(GitHubConfig) Tracker { built++; return stubTracker{} }
	ctx := context.Background()

	forgeID := f.forgeCred(t, "git.cloonar.com", "tok")
	githubID := f.forgeCredFlavor(t, "api.github.com", "tok", vault.ForgeGitHub)
	httpsID := f.cred(t, store.CredentialKindHTTPSToken, []byte("ignored"))
	badBlobID := f.cred(t, store.CredentialKindForgeToken, []byte("too-short-to-be-gcm"))
	badHostID := f.forgeCred(t, "http://git.cloonar.com", "tok")
	const forgejoRemote = "forgejo@git.cloonar.com:Cloonar/nixos.git"

	for _, tc := range []struct {
		name    string
		repo    store.Repo
		wantErr error // nil = drivable
	}{
		{"builtin", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingBuiltin}, nil},
		{"forgejo, valid", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: &forgeID, RemoteURL: forgejoRemote}, nil},
		{"github, valid", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindGitHub), ForgeCredentialID: &githubID, RemoteURL: "git@github.com:foo/bar.git"}, nil},
		{"no forge credential", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), RemoteURL: forgejoRemote}, ErrForgeCredentialMissing},
		{"wrong credential kind", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: &httpsID, RemoteURL: forgejoRemote}, ErrForgeCredentialKind},
		{"credential does not decrypt", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: &badBlobID, RemoteURL: forgejoRemote}, vault.ErrDecrypt},
		{"flavor mismatch", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindGitHub), ForgeCredentialID: &forgeID, RemoteURL: "git@github.com:foo/bar.git"}, ErrForgeFlavorMismatch},
		{"invalid credential host", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: &badHostID, RemoteURL: forgejoRemote}, ErrForgeHost},
		{"remote without owner/repo", store.Repo{ID: "r", TrackerBinding: store.TrackerBindingForge, ForgeKind: string(ForgeKindForgejo), ForgeCredentialID: &forgeID, RemoteURL: "https://git.cloonar.com/Cloonar"}, ErrRemotePath},
		{"unknown binding", store.Repo{ID: "r", TrackerBinding: "bogus"}, ErrUnknownBinding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.reg.CheckConfig(ctx, tc.repo)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("CheckConfig = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("CheckConfig = %v, want errors.Is %v", err, tc.wantErr)
			}
			if built != 0 {
				t.Fatalf("CheckConfig constructed %d tracker(s); it must build none", built)
			}
			// TrackerFor agrees on every case: same verdict, same error text.
			_, tfErr := f.reg.TrackerFor(ctx, tc.repo)
			if (tfErr == nil) != (err == nil) || (err != nil && tfErr.Error() != err.Error()) {
				t.Fatalf("TrackerFor err = %v, CheckConfig err = %v — the two must agree", tfErr, err)
			}
			built = 0
		})
	}
}
