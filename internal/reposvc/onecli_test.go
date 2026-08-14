package reposvc

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// The three repo-lifecycle agent hooks (issue #35): eager creation in Add, the
// one-way convergence in StartupHeal, the delete in Delete. All three are
// best-effort over a seam that is nil on most labs, so what these tests pin is
// as much what does NOT happen — no calls when unconfigured, no error when the
// sidecar is down — as what does.

// agentCall is one recorded EnsureAgent: the match key and the display string,
// kept apart because confusing the two is the failure issue #35 exists to
// prevent.
type agentCall struct{ identifier, displayName string }

// stubAgents is a hand-written OneCLIAgents double. It records every call in
// order and can be scripted to fail, so all three hooks are driven with no HTTP
// and no sidecar. Locked because Add's hook runs on the caller's goroutine
// while a clone job runs on another — the recorder must not be the thing that
// makes a test flaky under -race.
type stubAgents struct {
	mu      sync.Mutex
	ensured []agentCall
	deleted []string

	// ensureErr/deleteErr fail every call when set — the sidecar-down shape both
	// best-effort paths have to survive.
	ensureErr error
	deleteErr error
	// deleteFound is DeleteAgent's bool answer. False is the ordinary "no agent
	// carried that identifier" outcome (a repo predating OneCLI), which must
	// never be treated as a failure.
	deleteFound bool
}

func (s *stubAgents) EnsureAgent(_ context.Context, identifier, displayName string) (onecli.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensured = append(s.ensured, agentCall{identifier: identifier, displayName: displayName})
	if s.ensureErr != nil {
		return onecli.Agent{}, s.ensureErr
	}
	// A token rides on the real answer, so the stub carries one too: the point
	// of the assertions below is that reposvc drops it on the floor.
	return onecli.Agent{ID: "agt_" + identifier, Identifier: identifier, Name: displayName, Token: "gateway-token"}, nil
}

func (s *stubAgents) DeleteAgent(_ context.Context, identifier string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, identifier)
	if s.deleteErr != nil {
		return false, s.deleteErr
	}
	return s.deleteFound, nil
}

func (s *stubAgents) ensureCalls() []agentCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentCall(nil), s.ensured...)
}

func (s *stubAgents) deleteCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// agentEnv is a Service with just the seams these hooks touch (no provider
// registry, no pinner — the agent lifecycle is orthogonal to both).
type agentEnv struct {
	svc  *Service
	st   *store.Store
	home string
}

// newAgentEnv builds the Service with exactly the seam the caller passes: the
// stub for a configured lab, an untyped nil for the unconfigured one. The nil
// has to be untyped AT THE CALL SITE — a (*stubAgents)(nil) handed through this
// interface parameter would be a non-nil interface and would defeat the very
// gate the no-op tests are about.
func newAgentEnv(t *testing.T, agents OneCLIAgents) *agentEnv {
	t.Helper()
	testutil.RequireTool(t, "git")

	st := testutil.TempStore(t)
	home := t.TempDir()
	stateDir := t.TempDir()
	mat, err := vault.NewMaterializer(filepath.Join(stateDir, "runtime"))
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	v, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	svc, err := New(Options{
		Store: st, Vault: v, Materializer: mat, Git: gitx.New("git"), Bus: events.NewBus(),
		Logger: logx.New(io.Discard), ReposDir: filepath.Join(stateDir, "repos"),
		GitEnv: testutil.HermeticGitEnv(home),
		OneCLI: agents,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// LIFO: cancel and drain Add's clone jobs before the temp dirs and the store
	// go away under them.
	t.Cleanup(svc.Close)
	return &agentEnv{svc: svc, st: st, home: home}
}

// repoRow inserts a ready repo straight into the store — the startup and delete
// hooks act on rows, never on the clone.
func (e *agentEnv) repoRow(t *testing.T, name string) store.Repo {
	t.Helper()
	r, err := e.st.CreateRepo(context.Background(), store.Repo{
		ID: ids.NewID("repo"), Name: name, RemoteURL: "/tmp/" + name,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return r
}

// addRepo drives the real Add over a fixture origin, so the hook is exercised
// where it actually sits — after the row, the publish and the clone job.
func (e *agentEnv) addRepo(t *testing.T, name string) store.Repo {
	t.Helper()
	origin := makeOrigin(t, e.home, "main", 1)
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin, Name: name})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return repo
}

func TestAddEnsuresOneCLIAgent(t *testing.T) {
	stub := &stubAgents{}
	e := newAgentEnv(t, stub)

	repo := e.addRepo(t, "my-project")

	calls := stub.ensureCalls()
	if len(calls) != 1 {
		t.Fatalf("EnsureAgent called %d times, want exactly 1: %+v", len(calls), calls)
	}
	// The two arguments are not interchangeable (issue #35): the identifier is
	// the DERIVED slug — the immutable match key a repo's grants hang off — and
	// the display name is the repo's own name, so the OneCLI dashboard reads as
	// a list of repositories rather than of store ids.
	want := onecli.AgentIdentifier(repo.ID)
	if calls[0].identifier != want {
		t.Errorf("identifier = %q, want the derived slug %q", calls[0].identifier, want)
	}
	if calls[0].identifier == repo.ID {
		t.Errorf("identifier = %q, the RAW store id — EnsureAgent refuses anything that is not already a slug", calls[0].identifier)
	}
	if calls[0].displayName != repo.Name {
		t.Errorf("display name = %q, want the repo name %q", calls[0].displayName, repo.Name)
	}
	if calls[0].displayName == repo.ID {
		t.Errorf("display name = %q, the store id — the pre-#35 shape, unreadable in the dashboard", calls[0].displayName)
	}
}

func TestAddWithoutOneCLIIsSilentNoOp(t *testing.T) {
	// The stub is built but NOT wired: an unconfigured lab must not reach a seam
	// it does not have, and this is the assertion that says so.
	stub := &stubAgents{}
	e := newAgentEnv(t, nil)

	repo := e.addRepo(t, "my-project")

	if _, err := e.st.RepoByID(t.Context(), repo.ID); err != nil {
		t.Fatalf("repo row missing after Add on an unconfigured lab: %v", err)
	}
	if n := len(stub.ensureCalls()); n != 0 {
		t.Errorf("EnsureAgent called %d times with no OneCLI configured", n)
	}
}

func TestAddSurvivesOneCLIFailure(t *testing.T) {
	// The sidecar is down. Repo creation must still succeed: the spawn is the
	// fail-closed enforcement point (ADR-0067), not the repo row.
	stub := &stubAgents{ensureErr: errors.New("connection refused")}
	e := newAgentEnv(t, stub)

	repo := e.addRepo(t, "my-project")

	if repo.ID == "" || repo.Name != "my-project" {
		t.Fatalf("Add returned %+v, want the created repo", repo)
	}
	if _, err := e.st.RepoByID(t.Context(), repo.ID); err != nil {
		t.Fatalf("repo row missing after a failed agent ensure: %v", err)
	}
	if n := len(stub.ensureCalls()); n != 1 {
		t.Errorf("EnsureAgent called %d times, want 1", n)
	}
}

func TestDeleteRemovesOneCLIAgent(t *testing.T) {
	stub := &stubAgents{deleteFound: true}
	e := newAgentEnv(t, stub)
	repo := e.repoRow(t, "doomed")

	if err := e.svc.Delete(t.Context(), repo.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	calls := stub.deleteCalls()
	if len(calls) != 1 {
		t.Fatalf("DeleteAgent called %d times, want exactly 1: %v", len(calls), calls)
	}
	// Delete derives the identifier from the store id it was handed — the row is
	// already gone by then, so any other source would need a read that cannot
	// succeed.
	if want := onecli.AgentIdentifier(repo.ID); calls[0] != want {
		t.Errorf("identifier = %q, want the derived slug %q", calls[0], want)
	}
	if _, err := e.st.RepoByID(t.Context(), repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo row survived Delete: %v", err)
	}
}

func TestDeleteOrphansAgentOnOneCLIFailure(t *testing.T) {
	// Warn-and-orphan: a leaked agent is the status quo of every repo deleted
	// before this hook existed, so it can never hold the repo back.
	stub := &stubAgents{deleteErr: errors.New("connection refused")}
	e := newAgentEnv(t, stub)
	repo := e.repoRow(t, "doomed")

	if err := e.svc.Delete(t.Context(), repo.ID, false); err != nil {
		t.Fatalf("Delete err = %v, want nil despite the failing agent delete", err)
	}
	if n := len(stub.deleteCalls()); n != 1 {
		t.Errorf("DeleteAgent called %d times, want 1", n)
	}
	if _, err := e.st.RepoByID(t.Context(), repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo row survived Delete: %v", err)
	}
}

func TestDeleteWithNoAgentToRemoveSucceeds(t *testing.T) {
	// (false, nil): nothing carried that identifier — a repo created before
	// OneCLI was configured. An ordinary outcome, not a failure.
	stub := &stubAgents{deleteFound: false}
	e := newAgentEnv(t, stub)
	repo := e.repoRow(t, "never-had-one")

	if err := e.svc.Delete(t.Context(), repo.ID, false); err != nil {
		t.Fatalf("Delete err = %v, want nil when no agent carried the identifier", err)
	}
	if n := len(stub.deleteCalls()); n != 1 {
		t.Errorf("DeleteAgent called %d times, want 1", n)
	}
}

func TestDeleteWithoutOneCLIIsSilentNoOp(t *testing.T) {
	stub := &stubAgents{}
	e := newAgentEnv(t, nil)
	repo := e.repoRow(t, "doomed")

	if err := e.svc.Delete(t.Context(), repo.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := len(stub.deleteCalls()); n != 0 {
		t.Errorf("DeleteAgent called %d times with no OneCLI configured", n)
	}
}

func TestStartupHealEnsuresEveryRepoAgent(t *testing.T) {
	stub := &stubAgents{}
	e := newAgentEnv(t, stub)
	want := map[string]string{}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		repo := e.repoRow(t, name)
		want[onecli.AgentIdentifier(repo.ID)] = repo.Name
	}

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal: %v", err)
	}

	calls := stub.ensureCalls()
	if len(calls) != len(want) {
		t.Fatalf("EnsureAgent called %d times, want one per repo (%d): %+v", len(calls), len(want), calls)
	}
	// Compared as a set: the sweep's order is the store's, and pinning it here
	// would test store.Repos rather than the hook. What matters is that each
	// repo contributed its OWN (identifier, name) pair — a crossed pair is how a
	// repo ends up holding another repo's credentials.
	got := map[string]string{}
	for _, c := range calls {
		got[c.identifier] = c.displayName
	}
	for identifier, name := range want {
		if got[identifier] != name {
			t.Errorf("agent %q ensured with display name %q, want %q", identifier, got[identifier], name)
		}
	}
}

func TestStartupHealStopsAtFirstOneCLIFailure(t *testing.T) {
	// A sidecar that is not up yet is the EXPECTED case (same compose stack), so
	// boot must survive it — and one outage must be one warning, not one per
	// repo, which is why the loop stops instead of grinding through the rest.
	stub := &stubAgents{ensureErr: errors.New("connection refused")}
	e := newAgentEnv(t, stub)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		e.repoRow(t, name)
	}

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal err = %v, want nil — this error return fails lab boot", err)
	}
	if n := len(stub.ensureCalls()); n != 1 {
		t.Errorf("EnsureAgent called %d times, want 1 (the loop must stop at the first failure)", n)
	}
}

func TestStartupHealWithoutOneCLIIsSilentNoOp(t *testing.T) {
	stub := &stubAgents{}
	e := newAgentEnv(t, nil)
	e.repoRow(t, "alpha")

	if err := e.svc.StartupHeal(t.Context()); err != nil {
		t.Fatalf("StartupHeal: %v", err)
	}
	if n := len(stub.ensureCalls()); n != 0 {
		t.Errorf("EnsureAgent called %d times with no OneCLI configured", n)
	}
}
