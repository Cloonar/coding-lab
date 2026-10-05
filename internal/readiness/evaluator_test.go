package readiness

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// These tests drive the Evaluator over a REAL store (so the staleness rules
// are exercised against real credential rows and their real updated_at) with
// scripted stand-ins for the three seams it asks: the spawn path's
// resolvers, the tracker registry's local validation and the AFK engine's
// local count.

var evalClock = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// fakeSpawner scripts the instance-service seam: one provider for runs the
// operator starts, an optional other one for AFK runs, and a container gate.
type fakeSpawner struct {
	manual, afk provider.AgentProvider
	resolveErr  error
	gate        func(providerID string, repo store.Repo) instance.ContainerGate
	gateCalls   []string
}

func (f *fakeSpawner) ResolveProvider(_ context.Context, _ store.Repo, kind, _ string) (provider.AgentProvider, error) {
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	if kind == store.RunKindAFKAuto && f.afk != nil {
		return f.afk, nil
	}
	return f.manual, nil
}

func (f *fakeSpawner) ContainerGate(_ context.Context, providerID string, repo store.Repo) instance.ContainerGate {
	f.gateCalls = append(f.gateCalls, providerID)
	return f.gate(providerID, repo)
}

// fakeTrackerConfig scripts the tracker binding's local validation.
type fakeTrackerConfig struct{ err error }

func (f fakeTrackerConfig) CheckConfig(context.Context, store.Repo) error { return f.err }

// fakeCounter scripts the AFK engine's local claimable count and records who
// was asked.
type fakeCounter struct {
	counts map[string]int
	err    error
	asked  []string
}

func (f *fakeCounter) LocalClaimableCount(_ context.Context, repo store.Repo) (int, error) {
	f.asked = append(f.asked, repo.ID)
	if repo.TrackerBinding != store.TrackerBindingBuiltin {
		return 0, errors.New("a forge-bound repo was counted locally")
	}
	return f.counts[repo.ID], f.err
}

// countingStore counts the evaluator's store calls by method.
type countingStore struct {
	*store.Store
	calls map[string]int
}

func (c *countingStore) GetSetting(ctx context.Context, key string) (string, error) {
	c.calls["GetSetting:"+key]++
	return c.Store.GetSetting(ctx, key)
}
func (c *countingStore) Credentials(ctx context.Context) ([]store.CredentialMeta, error) {
	c.calls["Credentials"]++
	return c.Store.Credentials(ctx)
}
func (c *countingStore) RepoImports(ctx context.Context, repoID string) ([]store.Repo, error) {
	c.calls["RepoImports"]++
	return c.Store.RepoImports(ctx, repoID)
}
func (c *countingStore) AllRepoImports(ctx context.Context) (map[string][]string, error) {
	c.calls["AllRepoImports"]++
	return c.Store.AllRepoImports(ctx)
}
func (c *countingStore) OpenIssueCounts(ctx context.Context, label string) (map[string]int, error) {
	c.calls["OpenIssueCounts:"+label]++
	return c.Store.OpenIssueCounts(ctx, label)
}

type evalFixture struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	rec *Recorder
	ev  *Evaluator
}

func newEvalFixture(t *testing.T) *evalFixture {
	t.Helper()
	st := testutil.TempStore(t)
	ctx := context.Background()
	if err := st.SeedDefaultSettings(ctx, 6, "claude-code"); err != nil {
		t.Fatalf("SeedDefaultSettings: %v", err)
	}
	rec := NewRecorder(nil, func() time.Time { return evalClock })
	return &evalFixture{t: t, ctx: ctx, st: st, rec: rec, ev: &Evaluator{Store: st, Recorder: rec}}
}

func (f *evalFixture) credential(name, kind string, at time.Time) string {
	f.t.Helper()
	id := ids.NewID("cred")
	if _, err := f.st.CreateCredential(f.ctx, id, name, kind, []byte("sealed"), at); err != nil {
		f.t.Fatalf("CreateCredential: %v", err)
	}
	return id
}

func (f *evalFixture) repo(name string, mod func(*store.Repo)) store.Repo {
	f.t.Helper()
	r := store.Repo{
		ID: ids.NewID("repo"), Name: name, RemoteURL: "https://forge.example.com/acme/" + name + ".git",
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: evalClock,
	}
	if mod != nil {
		mod(&r)
	}
	created, err := f.st.CreateRepo(f.ctx, r)
	if err != nil {
		f.t.Fatalf("CreateRepo %s: %v", name, err)
	}
	return created
}

// reload re-reads a repo row, as a handler does per request.
func (f *evalFixture) reload(id string) store.Repo {
	f.t.Helper()
	r, err := f.st.RepoByID(f.ctx, id)
	if err != nil {
		f.t.Fatalf("RepoByID: %v", err)
	}
	return r
}

func (f *evalFixture) report(repoID string) Report {
	f.t.Helper()
	r, err := f.ev.Report(f.ctx, f.reload(repoID))
	if err != nil {
		f.t.Fatalf("Report: %v", err)
	}
	return r
}

func (f *evalFixture) summary(repoID string) Summary {
	f.t.Helper()
	s, err := f.ev.Summary(f.ctx, f.reload(repoID))
	if err != nil {
		f.t.Fatalf("Summary: %v", err)
	}
	return s
}

// issue files a built-in issue, optionally carrying the ready label.
func (f *evalFixture) issue(repoID, title, body string, ready bool) store.Issue {
	f.t.Helper()
	var labelIDs []string
	if ready {
		labels, err := f.st.LabelsByRepo(f.ctx, repoID)
		if err != nil {
			f.t.Fatalf("LabelsByRepo: %v", err)
		}
		for _, l := range labels {
			if l.Name == tracker.ReadyLabel {
				labelIDs = append(labelIDs, l.ID)
			}
		}
		if len(labelIDs) != 1 {
			f.t.Fatalf("repo has no %s label", tracker.ReadyLabel)
		}
	}
	is, err := f.st.CreateIssueWithLabels(f.ctx, repoID, title, body, labelIDs, store.CommentAuthorOperator, nil, evalClock)
	if err != nil {
		f.t.Fatalf("CreateIssueWithLabels: %v", err)
	}
	return is
}

func wantState(t *testing.T, r Report, id string, state State) {
	t.Helper()
	c := find(r, id)
	if c == nil {
		t.Fatalf("%s check is missing, want %s; report = %+v", id, state, r)
	}
	if c.State != state {
		t.Fatalf("%s = %s (%q), want %s", id, c.State, c.Detail, state)
	}
}

func wantAbsent(t *testing.T, r Report, id string) {
	t.Helper()
	if c := find(r, id); c != nil {
		t.Fatalf("%s = %+v, want it left out", id, *c)
	}
}

// A fetch outcome is only as good as the credential it was observed with:
// rotating the credential, or pointing the repo at another one, makes a
// recorded failure stale — the check disappears instead of nagging about a
// credential that is no longer in use.
func TestEvaluator_GitCredentialStaleAfterCredentialChange(t *testing.T) {
	f := newEvalFixture(t)
	credID := f.credential("deploy key", store.CredentialKindSSHKey, evalClock)
	repo := f.repo("widget", func(r *store.Repo) { r.CredentialID = &credID })

	wantAbsent(t, f.report(repo.ID), CheckGitCredential) // nothing fetched yet

	// A spawn's fetch fails with the credential as it is now.
	attr := gitx.FetchAttribution{RepoID: repo.ID, Credential: f.st.CredentialStampByID(f.ctx, repo.CredentialID)}
	f.rec.ObserveFetch(attr, errors.New("git fetch origin: exit status 128: fatal: Authentication failed for 'https://forge.example.com/acme/widget.git/'"))
	r := f.report(repo.ID)
	wantState(t, r, CheckGitCredential, Failing)
	if c := find(r, CheckGitCredential); *c.Fix != (Fix{Scope: ScopeRepo, Section: "integrations", Field: "credential_id"}) {
		t.Fatalf("fix = %+v", *c.Fix)
	}
	if r.State != Failing {
		t.Fatalf("roll-up = %s, want failing", r.State)
	}

	// The operator rotates the credential: the failure was the OLD payload's.
	if err := f.st.UpdateCredential(f.ctx, credID, nil, []byte("resealed"), evalClock.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	r = f.report(repo.ID)
	wantAbsent(t, r, CheckGitCredential)
	if r.State != Passing {
		t.Fatalf("roll-up after the rotation = %s, want passing (nothing known to be wrong)", r.State)
	}

	// The next fetch, with the rotated credential, is evidence again.
	attr.Credential = f.st.CredentialStampByID(f.ctx, repo.CredentialID)
	f.rec.ObserveFetch(attr, nil)
	wantState(t, f.report(repo.ID), CheckGitCredential, Passing)

	// The repo is pointed at ANOTHER credential: the success says nothing
	// about that one.
	other := f.credential("other key", store.CredentialKindSSHKey, evalClock)
	if _, err := f.st.UpdateRepoSettings(f.ctx, repo.ID, store.RepoSettingsUpdate{CredentialID: store.Set(&other)}); err != nil {
		t.Fatalf("UpdateRepoSettings: %v", err)
	}
	wantAbsent(t, f.report(repo.ID), CheckGitCredential)

	// Back to no credential at all: stale too.
	if _, err := f.st.UpdateRepoSettings(f.ctx, repo.ID, store.RepoSettingsUpdate{CredentialID: store.Set[*string](nil)}); err != nil {
		t.Fatalf("UpdateRepoSettings: %v", err)
	}
	wantAbsent(t, f.report(repo.ID), CheckGitCredential)

	// A credential of the wrong kind is a local fact and needs no fetch.
	forgeTok := f.credential("forge token", store.CredentialKindForgeToken, evalClock)
	if _, err := f.st.UpdateRepoSettings(f.ctx, repo.ID, store.RepoSettingsUpdate{CredentialID: store.Set(&forgeTok)}); err != nil {
		t.Fatalf("UpdateRepoSettings: %v", err)
	}
	wantState(t, f.report(repo.ID), CheckGitCredential, Failing)
}

// The tracker check: local validation first, then the last list read, stale
// once the forge credential or the binding changes.
func TestEvaluator_TrackerRecordAndStaleness(t *testing.T) {
	f := newEvalFixture(t)
	forgeCred := f.credential("forge", store.CredentialKindForgeToken, evalClock)
	repo := f.repo("widget", func(r *store.Repo) {
		r.TrackerBinding, r.ForgeKind, r.ForgeCredentialID = store.TrackerBindingForge, "forgejo", &forgeCred
	})

	// Without a registry to validate with, a forge binding is not evaluable.
	wantAbsent(t, f.report(repo.ID), CheckTracker)

	f.ev.Tracker = fakeTrackerConfig{}
	wantAbsent(t, f.report(repo.ID), CheckTracker) // valid config, nothing read yet

	stamp := f.st.CredentialStampByID(f.ctx, repo.ForgeCredentialID)
	f.rec.ObserveTrackerRead(tracker.ListRead{RepoID: repo.ID, Credential: stamp, Op: tracker.OpReadyIssues,
		Err: errors.New("forgejo GET /repos/acme/widget/issues: unexpected status 401: bad token"), OpenIssues: -1})
	r := f.report(repo.ID)
	wantState(t, r, CheckTracker, Failing)
	if c := find(r, CheckTracker); *c.Fix != (Fix{Scope: ScopeRepo, Section: "integrations", Field: "forge_credential_id"}) {
		t.Fatalf("fix = %+v", *c.Fix)
	}

	// Rotating the forge credential makes the failed read stale.
	if err := f.st.UpdateCredential(f.ctx, forgeCred, nil, []byte("resealed"), evalClock.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	wantAbsent(t, f.report(repo.ID), CheckTracker)

	stamp = f.st.CredentialStampByID(f.ctx, repo.ForgeCredentialID)
	f.rec.ObserveTrackerRead(tracker.ListRead{RepoID: repo.ID, Credential: stamp, Op: tracker.OpReadyIssues, OpenIssues: -1})
	wantState(t, f.report(repo.ID), CheckTracker, Passing)

	// A config problem outranks whatever was recorded.
	f.ev.Tracker = fakeTrackerConfig{err: fmt.Errorf("tracker for repo: %w", tracker.ErrForgeFlavorMismatch)}
	wantState(t, f.report(repo.ID), CheckTracker, Failing)
	f.ev.Tracker = fakeTrackerConfig{}

	// Rebinding to the built-in tracker: passing, records irrelevant.
	if _, err := f.st.UpdateRepoSettings(f.ctx, repo.ID, store.RepoSettingsUpdate{
		TrackerBinding: store.Set(store.TrackerBindingBuiltin)}); err != nil {
		t.Fatalf("UpdateRepoSettings: %v", err)
	}
	wantState(t, f.report(repo.ID), CheckTracker, Passing)
}

// The agent login check reads the last KNOWN status and never checks: an
// evaluation leaves the provider's AuthStatus call count untouched.
func TestEvaluator_AgentLoginPeeksNeverChecks(t *testing.T) {
	f := newEvalFixture(t)
	repo := f.repo("widget", nil)
	manual := providertest.New()
	manual.SetDisplayName("Agent One")
	sp := &fakeSpawner{manual: manual}
	f.ev.Spawner = sp

	// Never checked since lab started: nothing to say.
	wantAbsent(t, f.report(repo.ID), CheckAgentLogin)

	// Something else checks (a spawn, the login card): now it is known.
	if _, err := manual.AuthStatus(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	checks := manual.AuthChecks()
	r := f.report(repo.ID)
	wantState(t, r, CheckAgentLogin, Passing)
	if c := find(r, CheckAgentLogin); c.Detail != "Agent One is logged in." {
		t.Fatalf("detail = %q", c.Detail)
	}

	// The account silently logs out. Until something checks again the report
	// keeps the last known answer — it must not go and look.
	manual.SetLoggedIn(false)
	wantState(t, f.report(repo.ID), CheckAgentLogin, Passing)

	if _, err := manual.AuthStatus(f.ctx, false); err != nil {
		t.Fatal(err)
	}
	checks++
	r = f.report(repo.ID)
	wantState(t, r, CheckAgentLogin, Failing)
	if c := find(r, CheckAgentLogin); c.Detail != "Agent One is logged out on this server." || *c.Fix != (Fix{Scope: ScopeCredentials}) {
		t.Fatalf("failing check = %+v", *c)
	}

	for range 20 {
		f.report(repo.ID)
		f.summary(repo.ID)
	}
	if got := manual.AuthChecks(); got != checks {
		t.Fatalf("evaluations ran %d AuthStatus checks; the report must only peek", got-checks)
	}

	// A provider that cannot be peeked has no login state to read for free.
	sp.manual = &providertest.NoLinkFake{}
	wantAbsent(t, f.report(repo.ID), CheckAgentLogin)

	// No provider resolves at all.
	sp.resolveErr = errors.New("no agent providers registered")
	wantAbsent(t, f.report(repo.ID), CheckAgentLogin)
}

// While Auto is on and AFK runs resolve to a different agent, that agent's
// login counts too — and a logged-out one fails the check.
func TestEvaluator_AgentLoginIncludesTheAFKAgentWhileAutoIsOn(t *testing.T) {
	f := newEvalFixture(t)
	repo := f.repo("widget", nil)
	manual, afkProv := providertest.New(), providertest.New()
	manual.SetDisplayName("Agent One")
	afkProv.SetID("agent-two")
	afkProv.SetDisplayName("Agent Two")
	afkProv.SetLoggedIn(false)
	for _, p := range []*providertest.Fake{manual, afkProv} {
		if _, err := p.AuthStatus(f.ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	f.ev.Spawner = &fakeSpawner{manual: manual, afk: afkProv}

	// Auto off: only runs the operator starts matter.
	r := f.report(repo.ID)
	wantState(t, r, CheckAgentLogin, Passing)
	if c := find(r, CheckAgentLogin); c.Detail != "Agent One is logged in." {
		t.Fatalf("detail with Auto off = %q", c.Detail)
	}

	if _, err := f.st.UpdateRepoSettings(f.ctx, repo.ID, store.RepoSettingsUpdate{AFKAutoEnabled: store.Set(true)}); err != nil {
		t.Fatal(err)
	}
	r = f.report(repo.ID)
	wantState(t, r, CheckAgentLogin, Failing)
	if c := find(r, CheckAgentLogin); c.Detail != "Agent Two is logged out on this server." {
		t.Fatalf("detail with Auto on = %q", c.Detail)
	}

	afkProv.SetLoggedIn(true)
	if _, err := afkProv.AuthStatus(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	r = f.report(repo.ID)
	wantState(t, r, CheckAgentLogin, Passing)
	if c := find(r, CheckAgentLogin); c.Detail != "Agent One and Agent Two are logged in." {
		t.Fatalf("detail with both logged in = %q", c.Detail)
	}

	// The same agent for both run classes is reported once.
	f.ev.Spawner = &fakeSpawner{manual: manual, afk: manual}
	if c := find(f.report(repo.ID), CheckAgentLogin); c.Detail != "Agent One is logged in." {
		t.Fatalf("detail with one agent for both = %q", c.Detail)
	}
}

// The dev image check exists only for a repo whose EFFECTIVE Runner is
// container — pinned or inherited — follows the spawn gate, and past an open
// gate reads the recorded pull-if-missing of the image the gate resolved.
func TestEvaluator_DevImage(t *testing.T) {
	f := newEvalFixture(t)
	const image = "ghcr.io/acme/dev:1@sha256:0123"
	manual := providertest.New()
	gate := instance.ContainerGate{Stage: instance.ContainerGateOpen, Image: image}
	sp := &fakeSpawner{manual: manual, gate: func(string, store.Repo) instance.ContainerGate { return gate }}
	f.ev.Spawner = sp

	inherits := f.repo("inherits", nil) // runner NULL → the seeded runner_default (host)
	pinned := f.repo("pinned", func(r *store.Repo) { r.Runner = ptr(store.RunnerContainer) })

	wantAbsent(t, f.report(inherits.ID), CheckDevImage)
	if len(sp.gateCalls) != 0 {
		t.Fatalf("the container gate was asked for a host-Runner repo: %v", sp.gateCalls)
	}

	// Pinned container, gate open, image never ensured: left out.
	wantAbsent(t, f.report(pinned.ID), CheckDevImage)
	if len(sp.gateCalls) != 1 || sp.gateCalls[0] != manual.ID() {
		t.Fatalf("gate calls = %v, want one for the manual provider", sp.gateCalls)
	}

	f.rec.ObserveImage(pinned.ID, image, nil)
	wantState(t, f.report(pinned.ID), CheckDevImage, Passing)
	f.rec.ObserveImage(pinned.ID, image, errors.New("pulling dev image: exit status 125: manifest unknown"))
	r := f.report(pinned.ID)
	wantState(t, r, CheckDevImage, Failing)
	if c := find(r, CheckDevImage); *c.Fix != (Fix{Scope: ScopeRepo, Section: "runner", Field: "image_ref"}) {
		t.Fatalf("fix = %+v", *c.Fix)
	}

	// The record belongs to the image ref: resolving to another one starts
	// from no record at all.
	gate.Image = "ghcr.io/acme/dev:2@sha256:4567"
	wantAbsent(t, f.report(pinned.ID), CheckDevImage)

	// A closed gate is the report, whatever was recorded.
	gate = instance.ContainerGate{Stage: instance.ContainerGatePreflightPending}
	r = f.report(pinned.ID)
	wantState(t, r, CheckDevImage, Pending)
	if r.State != Pending {
		t.Fatalf("roll-up = %s, want pending", r.State)
	}

	// The inheriting repo follows the global default live.
	if err := f.st.SetSetting(f.ctx, store.SettingRunnerDefault, store.RunnerContainer); err != nil {
		t.Fatal(err)
	}
	wantState(t, f.report(inherits.ID), CheckDevImage, Pending)

	// An unresolvable default is a failing check, not a silent host.
	if err := f.st.SetSetting(f.ctx, store.SettingRunnerDefault, "podman"); err != nil {
		t.Fatal(err)
	}
	r = f.report(inherits.ID)
	wantState(t, r, CheckDevImage, Failing)
	if c := find(r, CheckDevImage); *c.Fix != (Fix{Scope: ScopeRepo, Section: "runner", Field: "runner"}) {
		t.Fatalf("fix = %+v", *c.Fix)
	}
	wantState(t, f.report(pinned.ID), CheckDevImage, Pending) // the pin does not read the default

	// Without the instance service the gate cannot be asked: left out.
	f.ev.Spawner = nil
	wantAbsent(t, f.report(pinned.ID), CheckDevImage)
}

// With Auto on and another AFK agent, the gate is asked for both; the first
// closed one is reported, naming that agent.
func TestEvaluator_DevImageAsksTheGateForEveryAgent(t *testing.T) {
	f := newEvalFixture(t)
	manual, afkProv := providertest.New(), providertest.New()
	afkProv.SetID("agent-two")
	afkProv.SetDisplayName("Agent Two")
	sp := &fakeSpawner{manual: manual, afk: afkProv, gate: func(providerID string, _ store.Repo) instance.ContainerGate {
		if providerID == "agent-two" {
			return instance.ContainerGate{Stage: instance.ContainerGateNoToolsImage, Err: errors.New("no tools image")}
		}
		return instance.ContainerGate{Stage: instance.ContainerGateOpen, Image: "img"}
	}}
	f.ev.Spawner = sp
	repo := f.repo("widget", func(r *store.Repo) {
		r.Runner, r.AFKAutoEnabled = ptr(store.RunnerContainer), true
	})

	r := f.report(repo.ID)
	wantState(t, r, CheckDevImage, Failing)
	if c := find(r, CheckDevImage); c.Detail != "No agent-tools image is configured for Agent Two on this server." {
		t.Fatalf("detail = %q", c.Detail)
	}
	if fmt.Sprint(sp.gateCalls) != fmt.Sprint([]string{manual.ID(), "agent-two"}) {
		t.Fatalf("gate calls = %v", sp.gateCalls)
	}
}

// The imports check reads each target's clone state and the TARGET's fetch
// record, stale against the TARGET's credential.
func TestEvaluator_Imports(t *testing.T) {
	f := newEvalFixture(t)
	libCred := f.credential("lib key", store.CredentialKindSSHKey, evalClock)
	app := f.repo("app", nil)
	lib := f.repo("lib", func(r *store.Repo) { r.CredentialID = &libCred })
	proto := f.repo("proto", func(r *store.Repo) { r.CloneStatus = store.CloneStatusCloning })

	r := f.report(app.ID)
	wantState(t, r, CheckImports, Passing)
	if c := find(r, CheckImports); c.Detail != "No read-only imports declared." {
		t.Fatalf("detail = %q", c.Detail)
	}

	for _, target := range []string{lib.ID, proto.ID} {
		if err := f.st.AddRepoImport(f.ctx, app.ID, target); err != nil {
			t.Fatalf("AddRepoImport: %v", err)
		}
	}
	r = f.report(app.ID)
	wantState(t, r, CheckImports, Failing)
	if c := find(r, CheckImports); c.Detail != `The read-only import "proto" is still being cloned.` ||
		*c.Fix != (Fix{Scope: ScopeRepo, Section: "imports"}) {
		t.Fatalf("failing check = %+v (fix %+v)", *c, c.Fix)
	}

	if err := f.st.UpdateRepoCloneStatus(f.ctx, proto.ID, store.CloneStatusReady, ""); err != nil {
		t.Fatal(err)
	}
	wantAbsent(t, f.report(app.ID), CheckImports) // both ready, neither fetched yet

	libStamp := f.st.CredentialStampByID(f.ctx, lib.CredentialID)
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: lib.ID, Credential: libStamp, OnBehalfOf: app.ID},
		errors.New("git fetch origin: exit status 128: fatal: Authentication failed for 'https://forge.example.com/acme/lib.git/'"))
	r = f.report(app.ID)
	wantState(t, r, CheckImports, Failing)
	if c := find(r, CheckImports); c.Detail != `The last fetch of the read-only import "lib" failed: Authentication failed for 'https://forge.example.com/acme/lib.git/'.` {
		t.Fatalf("detail = %q", c.Detail)
	}

	// Rotating the TARGET's credential makes that failure stale.
	if err := f.st.UpdateCredential(f.ctx, libCred, nil, []byte("resealed"), evalClock.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	wantAbsent(t, f.report(app.ID), CheckImports)

	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: lib.ID, Credential: f.st.CredentialStampByID(f.ctx, lib.CredentialID)}, nil)
	wantAbsent(t, f.report(app.ID), CheckImports) // proto still unfetched
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: proto.ID, Credential: store.NoCredentialStamp}, nil)
	r = f.report(app.ID)
	wantState(t, r, CheckImports, Passing)
	if c := find(r, CheckImports); c.Detail != "All 2 read-only imports were fetched successfully." {
		t.Fatalf("detail = %q", c.Detail)
	}

	// The targets' own reports are untouched by being imported.
	wantState(t, f.report(lib.ID), CheckImports, Passing)
}

// claimable and open_issues: fresh for the built-in tracker, the last known
// value for a forge, null when not known.
func TestEvaluator_SummaryCounts(t *testing.T) {
	f := newEvalFixture(t)
	counter := &fakeCounter{counts: map[string]int{}}
	f.ev.Claimable = counter

	builtin := f.repo("builtin", nil)
	idle := f.repo("idle", nil)
	forge := f.repo("forge", func(r *store.Repo) { r.TrackerBinding, r.ForgeKind = store.TrackerBindingForge, "forgejo" })

	// An idle builtin repo: both counts are a known zero, and nobody was
	// asked to count — an empty ready queue needs no claim-branch read.
	s := f.summary(idle.ID)
	if s.Claimable == nil || *s.Claimable != 0 || s.OpenIssues == nil || *s.OpenIssues != 0 {
		t.Fatalf("idle builtin summary = %+v, want known zeros", s)
	}
	if len(counter.asked) != 0 {
		t.Fatalf("the counter was asked for a repo with an empty ready queue: %v", counter.asked)
	}

	// Three open issues, two of them ready, one closed.
	f.issue(builtin.ID, "plain", "", false)
	f.issue(builtin.ID, "ready one", "", true)
	f.issue(builtin.ID, "ready two", "", true)
	closed := f.issue(builtin.ID, "done", "", true)
	if _, err := f.st.UpdateIssue(f.ctx, builtin.ID, closed.Number, store.IssueUpdate{State: store.Set(store.IssueStateClosed)}, evalClock); err != nil {
		t.Fatal(err)
	}
	counter.counts[builtin.ID] = 1 // one of the two ready issues is claimed
	s = f.summary(builtin.ID)
	if s.OpenIssues == nil || *s.OpenIssues != 3 {
		t.Fatalf("builtin open_issues = %v, want 3", s.OpenIssues)
	}
	if s.Claimable == nil || *s.Claimable != 1 {
		t.Fatalf("builtin claimable = %v, want the freshly counted 1", s.Claimable)
	}
	if fmt.Sprint(counter.asked) != fmt.Sprint([]string{builtin.ID}) {
		t.Fatalf("counter asked for %v, want only the repo with a ready queue", counter.asked)
	}

	// The count cannot be computed (clone not ready, say): unknown, not zero.
	counter.err = errors.New("git for-each-ref: not a git repository")
	if s = f.summary(builtin.ID); s.Claimable != nil {
		t.Fatalf("builtin claimable = %d after a failed count, want null", *s.Claimable)
	}
	counter.err = nil

	// A forge-bound repo: null until something read it, then the last value —
	// and never a local count.
	counter.asked = nil
	s = f.summary(forge.ID)
	if s.Claimable != nil || s.OpenIssues != nil {
		t.Fatalf("forge summary before any read = %+v, want nulls", s)
	}
	f.rec.ObserveClaimable(f.reload(forge.ID), 4)
	f.rec.ObserveTrackerRead(tracker.ListRead{RepoID: forge.ID, Op: tracker.OpIssues, OpenIssues: 9})
	s = f.summary(forge.ID)
	if s.Claimable == nil || *s.Claimable != 4 || s.OpenIssues == nil || *s.OpenIssues != 9 {
		t.Fatalf("forge summary = %+v, want claimable 4 and open_issues 9", s)
	}
	if len(counter.asked) != 0 {
		t.Fatalf("a forge-bound repo was counted locally: %v", counter.asked)
	}

	// Without the AFK engine a builtin repo's claimable count is unknown.
	f.ev.Claimable = nil
	if s = f.summary(builtin.ID); s.Claimable != nil || s.OpenIssues == nil || *s.OpenIssues != 3 {
		t.Fatalf("summary without an engine = %+v, want claimable null and open_issues 3", s)
	}
}

// The list's batch evaluation answers exactly what the single-repo one does,
// from a bounded number of store reads that does not grow with the list.
func TestEvaluator_SummariesMatchSingleAndBatchTheirReads(t *testing.T) {
	f := newEvalFixture(t)
	cs := &countingStore{Store: f.st, calls: map[string]int{}}
	counter := &fakeCounter{counts: map[string]int{}}
	manual := providertest.New()
	if _, err := manual.AuthStatus(f.ctx, true); err != nil {
		t.Fatal(err)
	}
	f.ev = &Evaluator{Store: cs, Recorder: f.rec, Tracker: fakeTrackerConfig{}, Claimable: counter,
		Spawner: &fakeSpawner{manual: manual, gate: func(string, store.Repo) instance.ContainerGate {
			return instance.ContainerGate{Stage: instance.ContainerGateOpen, Image: "img"}
		}}}

	gitCred := f.credential("deploy key", store.CredentialKindSSHKey, evalClock)
	forgeCred := f.credential("forge", store.CredentialKindForgeToken, evalClock)
	var repos []store.Repo
	for i := range 12 {
		repos = append(repos, f.repo(fmt.Sprintf("forge-%02d", i), func(r *store.Repo) {
			r.TrackerBinding, r.ForgeKind = store.TrackerBindingForge, "forgejo"
			r.CredentialID, r.ForgeCredentialID = &gitCred, &forgeCred
		}))
	}
	builtin := f.repo("builtin", func(r *store.Repo) { r.Runner = ptr(store.RunnerContainer) })
	cloning := f.repo("cloning", func(r *store.Repo) { r.CloneStatus = store.CloneStatusCloning })
	repos = append(repos, builtin, cloning)
	f.issue(builtin.ID, "ready", "", true)
	counter.counts[builtin.ID] = 1
	// Imports in both directions, plus records of every kind.
	for _, pair := range [][2]string{{repos[0].ID, builtin.ID}, {repos[0].ID, repos[1].ID}, {builtin.ID, repos[0].ID}} {
		if err := f.st.AddRepoImport(f.ctx, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	gitStamp, forgeStamp := f.st.CredentialStampByID(f.ctx, &gitCred), f.st.CredentialStampByID(f.ctx, &forgeCred)
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: repos[0].ID, Credential: gitStamp}, nil)
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: repos[1].ID, Credential: gitStamp}, errors.New("fatal: no"))
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: builtin.ID, Credential: store.NoCredentialStamp}, nil)
	f.rec.ObserveTrackerRead(tracker.ListRead{RepoID: repos[0].ID, Credential: forgeStamp, Op: tracker.OpIssues, OpenIssues: 5})
	f.rec.ObserveTrackerRead(tracker.ListRead{RepoID: repos[2].ID, Credential: forgeStamp, Op: tracker.OpReadyIssues, Err: errors.New("401"), OpenIssues: -1})
	f.rec.ObserveClaimable(repos[0], 2)
	f.rec.ObserveImage(builtin.ID, "img", nil)

	all, err := f.st.Repos(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(repos) {
		t.Fatalf("store lists %d repos, want %d", len(all), len(repos))
	}
	cs.calls = map[string]int{}
	got, err := f.ev.Summaries(f.ctx, all)
	if err != nil {
		t.Fatalf("Summaries: %v", err)
	}
	listCalls := cs.calls
	if len(got) != len(all) {
		t.Fatalf("Summaries returned %d entries for %d repos", len(got), len(all))
	}

	// The batch's store reads: one of each, however long the list is.
	wantCalls := map[string]int{
		"Credentials": 1, "AllRepoImports": 1,
		"OpenIssueCounts:": 1, "OpenIssueCounts:" + tracker.ReadyLabel: 1,
		"GetSetting:" + store.SettingRunnerDefault: 1,
	}
	if !reflect.DeepEqual(listCalls, wantCalls) {
		t.Fatalf("store calls for a %d-repo list = %v, want %v", len(all), listCalls, wantCalls)
	}

	for i, repo := range all {
		single, err := f.ev.Summary(f.ctx, repo)
		if err != nil {
			t.Fatalf("Summary(%s): %v", repo.Name, err)
		}
		if !reflect.DeepEqual(got[i], single) {
			t.Errorf("%s: list summary differs from the single-repo one\n list:   %+v\n single: %+v", repo.Name, got[i], single)
		}
		report, err := f.ev.Report(f.ctx, repo)
		if err != nil {
			t.Fatalf("Report(%s): %v", repo.Name, err)
		}
		if !reflect.DeepEqual(got[i].Readiness, report) {
			t.Errorf("%s: summary.readiness differs from the readiness report", repo.Name)
		}
	}

	// Spot-check that the batch is not vacuous: the records landed where
	// they should.
	byName := map[string]Summary{}
	for i, repo := range all {
		byName[repo.Name] = got[i]
	}
	s0 := byName["forge-00"]
	wantState(t, s0.Readiness, CheckGitCredential, Passing)
	wantState(t, s0.Readiness, CheckTracker, Passing)
	wantState(t, s0.Readiness, CheckAgentLogin, Passing)
	wantState(t, s0.Readiness, CheckImports, Failing) // imports forge-01, whose fetch failed
	if s0.Claimable == nil || *s0.Claimable != 2 || s0.OpenIssues == nil || *s0.OpenIssues != 5 {
		t.Errorf("forge-00 counts = %v / %v, want 2 / 5", s0.Claimable, s0.OpenIssues)
	}
	wantState(t, byName["forge-01"].Readiness, CheckGitCredential, Failing)
	wantState(t, byName["forge-02"].Readiness, CheckTracker, Failing)
	wantAbsent(t, byName["forge-03"].Readiness, CheckGitCredential)
	sb := byName["builtin"]
	wantState(t, sb.Readiness, CheckDevImage, Passing)
	wantState(t, sb.Readiness, CheckImports, Passing) // imports forge-00, fetched fine
	if sb.Claimable == nil || *sb.Claimable != 1 || sb.OpenIssues == nil || *sb.OpenIssues != 1 {
		t.Errorf("builtin counts = %v / %v, want 1 / 1", sb.Claimable, sb.OpenIssues)
	}
	if byName["cloning"].Readiness.State != Pending {
		t.Errorf("cloning repo roll-up = %s, want pending", byName["cloning"].Readiness.State)
	}
}

// With nothing but a store and an empty recorder — the degraded boot, or a
// server right after a restart — every repo still gets a report: the checks
// that rest on stored state alone, and nothing invented.
func TestEvaluator_BareMinimum(t *testing.T) {
	f := newEvalFixture(t)
	builtin := f.repo("builtin", nil)
	forge := f.repo("forge", func(r *store.Repo) {
		r.TrackerBinding, r.ForgeKind, r.Runner = store.TrackerBindingForge, "forgejo", ptr(store.RunnerContainer)
	})

	s := f.summary(builtin.ID)
	if got, want := checkIDs(s.Readiness), []string{CheckClone, CheckTracker, CheckImports}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("builtin checks = %v, want %v", got, want)
	}
	if s.Readiness.State != Passing || s.Claimable != nil || s.OpenIssues == nil {
		t.Fatalf("builtin summary = %+v", s)
	}

	s = f.summary(forge.ID)
	if got, want := checkIDs(s.Readiness), []string{CheckClone, CheckImports}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("forge checks = %v, want %v", got, want)
	}
	if s.Claimable != nil || s.OpenIssues != nil {
		t.Fatalf("forge counts = %v / %v, want nulls", s.Claimable, s.OpenIssues)
	}
}
