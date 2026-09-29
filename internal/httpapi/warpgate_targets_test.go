package httpapi

// The per-repo SSH targets picker (issue #39 / ADR-0068). The handlers run
// against fakeWarpgateAPI, an in-memory, STATEFUL stand-in for the WarpgateAPI
// seam that keeps the identities and role assignments the handlers mutate and
// records every call, so an assertion checks what reached Warpgate rather
// than the handler's intent. The lab cache (repo_ssh_targets) is asserted
// through the real store. One test drives the real *warpgate.Client against
// a stub admin API that rejects the token, so the error mapping is proven on
// the client's actual error text.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// --- the fake ---------------------------------------------------------------

// fakeWarpgateAPI implements WarpgateAPI in memory. Identities are keyed by
// repo ID — the user and the role separately, so a partial identity (one
// half missing) can be seeded — and assignments by role ID.
type fakeWarpgateAPI struct {
	mu    sync.Mutex
	calls []string

	info      warpgate.Info
	healthErr error

	targets  []warpgate.Target          // Warpgate's SSH targets, in ListSSHTargets order
	users    map[string]string          // repo ID → user ID
	roles    map[string]string          // repo ID → role ID
	assigned map[string]map[string]bool // role ID → target IDs
	next     int

	// fail, when non-nil, makes every admin call (Health excepted) fail with
	// it — the "configured but Warpgate is failing" half of the contract.
	fail error
}

func newFakeWarpgateAPI(targets ...warpgate.Target) *fakeWarpgateAPI {
	return &fakeWarpgateAPI{
		info:     warpgate.Info{Version: "0.29.1", Authenticated: true},
		targets:  targets,
		users:    map[string]string{},
		roles:    map[string]string{},
		assigned: map[string]map[string]bool{},
	}
}

func (f *fakeWarpgateAPI) record(call string) error {
	f.calls = append(f.calls, call)
	return f.fail
}

func (f *fakeWarpgateAPI) Health(context.Context) (warpgate.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Health")
	return f.info, f.healthErr
}

func (f *fakeWarpgateAPI) identityLocked(repoID string) warpgate.Identity {
	var id warpgate.Identity
	if uid, ok := f.users[repoID]; ok {
		id.User = warpgate.User{ID: uid, Username: warpgate.RepoSlug(repoID)}
	}
	if rid, ok := f.roles[repoID]; ok {
		id.Role = warpgate.Role{ID: rid, Name: warpgate.RepoSlug(repoID)}
	}
	return id
}

func (f *fakeWarpgateAPI) FindRepoIdentity(_ context.Context, repoID string) (warpgate.Identity, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("FindRepoIdentity " + repoID); err != nil {
		return warpgate.Identity{}, false, err
	}
	id := f.identityLocked(repoID)
	return id, id.User.ID != "" && id.Role.ID != "", nil
}

func (f *fakeWarpgateAPI) EnsureRepoIdentity(_ context.Context, repoID, repoName string) (warpgate.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("EnsureRepoIdentity " + repoID + " " + repoName); err != nil {
		return warpgate.Identity{}, err
	}
	if _, ok := f.users[repoID]; !ok {
		f.next++
		f.users[repoID] = fmt.Sprintf("user_%d", f.next)
	}
	if _, ok := f.roles[repoID]; !ok {
		f.next++
		f.roles[repoID] = fmt.Sprintf("role_%d", f.next)
	}
	return f.identityLocked(repoID), nil
}

func (f *fakeWarpgateAPI) ListSSHTargets(context.Context) ([]warpgate.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ListSSHTargets"); err != nil {
		return nil, err
	}
	return append([]warpgate.Target(nil), f.targets...), nil
}

func (f *fakeWarpgateAPI) RoleSSHTargets(_ context.Context, roleID string) ([]warpgate.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("RoleSSHTargets " + roleID); err != nil {
		return nil, err
	}
	out := []warpgate.Target{}
	for _, t := range f.targets {
		if f.assigned[roleID][t.ID] {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeWarpgateAPI) AssignTargetRole(_ context.Context, targetID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("AssignTargetRole " + targetID + " " + roleID); err != nil {
		return err
	}
	if f.assigned[roleID] == nil {
		f.assigned[roleID] = map[string]bool{}
	}
	f.assigned[roleID][targetID] = true
	return nil
}

func (f *fakeWarpgateAPI) UnassignTargetRole(_ context.Context, targetID, roleID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("UnassignTargetRole " + targetID + " " + roleID); err != nil {
		return err
	}
	delete(f.assigned[roleID], targetID)
	return nil
}

// seedIdentity gives the repo a user and a role and returns the role's ID.
func (f *fakeWarpgateAPI) seedIdentity(repoID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.users[repoID] = fmt.Sprintf("user_%d", f.next)
	f.next++
	f.roles[repoID] = fmt.Sprintf("role_%d", f.next)
	return f.roles[repoID]
}

// seedRoleOnly gives the repo a role but no user — the partial identity.
func (f *fakeWarpgateAPI) seedRoleOnly(repoID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.roles[repoID] = fmt.Sprintf("role_%d", f.next)
	return f.roles[repoID]
}

func (f *fakeWarpgateAPI) assign(roleID string, targetIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.assigned[roleID] == nil {
		f.assigned[roleID] = map[string]bool{}
	}
	for _, id := range targetIDs {
		f.assigned[roleID][id] = true
	}
}

func (f *fakeWarpgateAPI) isAssigned(roleID, targetID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.assigned[roleID][targetID]
}

func (f *fakeWarpgateAPI) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

func (f *fakeWarpgateAPI) allCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// count reports how many recorded calls start with prefix (a method name, or
// a method name plus arguments).
func (f *fakeWarpgateAPI) count(prefix string) int {
	n := 0
	for _, c := range f.allCalls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// --- harness ----------------------------------------------------------------

var (
	targetAlpha = warpgate.Target{ID: "tgt-alpha", Name: "alpha", Description: "the alpha box"}
	targetBeta  = warpgate.Target{ID: "tgt-beta", Name: "beta", Description: ""}
	targetGamma = warpgate.Target{ID: "tgt-gamma", Name: "gamma", Description: "staging"}
)

// newWarpgateTargetServer builds a logged-in server wired to api (nil = the
// REST pair is unconfigured) plus one repo to hang the per-repo routes off.
func newWarpgateTargetServer(t *testing.T, api *fakeWarpgateAPI) (*testServer, store.Repo) {
	t.Helper()
	x := newTestServer(t, func(o *Options) {
		if api != nil {
			o.Warpgate = api
		}
	})
	x.setup("op", "password123")
	return x, seedTrackerRepo(t, x, "ssh-picker", nil)
}

func sshTargetsPath(repo store.Repo) string {
	return "/api/v1/repos/" + repo.ID + "/warpgate/targets"
}

func sshTargetPath(repo store.Repo, targetID string) string {
	return sshTargetsPath(repo) + "/" + targetID
}

// cachedTargets reads the repo's lab-side cache straight from the store.
func cachedTargets(t *testing.T, x *testServer, repo store.Repo) []store.SSHTarget {
	t.Helper()
	got, err := x.st.RepoSSHTargets(context.Background(), repo.ID)
	if err != nil {
		t.Fatalf("RepoSSHTargets: %v", err)
	}
	return got
}

func seedCache(t *testing.T, x *testServer, repo store.Repo, targets ...store.SSHTarget) {
	t.Helper()
	if err := x.st.ReplaceRepoSSHTargets(context.Background(), repo.ID, targets); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
}

func wantCache(t *testing.T, x *testServer, repo store.Repo, want ...store.SSHTarget) {
	t.Helper()
	got := cachedTargets(t, x, repo)
	if want == nil {
		want = []store.SSHTarget{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cached targets = %+v, want %+v", got, want)
	}
}

// --- the unconfigured lab ---------------------------------------------------

// TestWarpgateTargetsUnconfigured pins the default lab: the read answers a
// well-formed "off" body, the toggles refuse with a 409 naming the flags, and
// nothing touches the cache — no truth was read, so there is nothing to
// replace it with.
func TestWarpgateTargetsUnconfigured(t *testing.T) {
	x, repo := newWarpgateTargetServer(t, nil)
	stale := store.SSHTarget{ID: "tgt-stale", Name: "stale"}
	seedCache(t, x, repo, stale)

	resp := x.do("GET", sshTargetsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if raw := rawBody(t, resp); raw != `{"configured":false,"targets":[]}`+"\n" {
		t.Fatalf("unconfigured body = %q, want configured:false with an empty array", raw)
	}

	for _, method := range []string{"PUT", "DELETE"} {
		resp = x.do(method, sshTargetPath(repo, targetAlpha.ID), nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusConflict)
		msg := wantErrorBody(t, resp)
		if !strings.Contains(msg, "not configured") || !strings.Contains(msg, "--warpgate-url") || !strings.Contains(msg, "--warpgate-admin-token-file") {
			t.Errorf("%s error = %q, want it to say what is not configured and which flags fix it", method, msg)
		}
	}
	wantCache(t, x, repo, stale)
}

// --- the listing ------------------------------------------------------------

// TestWarpgateTargetListMarksAssigned pins the listing: every SSH target in
// the client's order, flagged by the role's assignments, descriptions carried
// (an empty one as ""), no write to Warpgate — and the lab cache REPLACED by
// exactly the assigned subset, a stale row included in what is dropped.
func TestWarpgateTargetListMarksAssigned(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha, targetBeta, targetGamma)
	x, repo := newWarpgateTargetServer(t, api)
	role := api.seedIdentity(repo.ID)
	api.assign(role, targetAlpha.ID, targetGamma.ID)
	seedCache(t, x, repo, store.SSHTarget{ID: "tgt-stale", Name: "stale"}, store.SSHTarget{ID: targetBeta.ID, Name: targetBeta.Name})

	resp := x.do("GET", sshTargetsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	raw := rawBody(t, resp)
	want := `{"configured":true,"targets":[` +
		`{"id":"tgt-alpha","name":"alpha","description":"the alpha box","assigned":true},` +
		`{"id":"tgt-beta","name":"beta","description":"","assigned":false},` +
		`{"id":"tgt-gamma","name":"gamma","description":"staging","assigned":true}]}` + "\n"
	if raw != want {
		t.Fatalf("body =\n%s\nwant\n%s", raw, want)
	}

	wantCache(t, x, repo, store.SSHTarget{ID: targetAlpha.ID, Name: "alpha"}, store.SSHTarget{ID: targetGamma.ID, Name: "gamma"})
	if n := api.count("EnsureRepoIdentity"); n != 0 {
		t.Fatalf("the read ensured an identity %d times (calls %v)", n, api.allCalls())
	}
	if n := api.count("RoleSSHTargets " + role); n != 1 {
		t.Fatalf("RoleSSHTargets(%s) called %d times, want 1 (calls %v)", role, n, api.allCalls())
	}
}

// TestWarpgateTargetListWithoutIdentity: a repo nobody has assigned anything
// to has no Warpgate identity, and opening its picker must not create one.
// Every target is unassigned, and the cache is cleared — the truth says the
// repo has nothing, so spawns must stop calling Warpgate for it.
func TestWarpgateTargetListWithoutIdentity(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha, targetBeta)
	x, repo := newWarpgateTargetServer(t, api)
	seedCache(t, x, repo, store.SSHTarget{ID: targetAlpha.ID, Name: targetAlpha.Name})

	resp := x.do("GET", sshTargetsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	targets := entriesOf(t, decodeBody(t, resp), "targets")
	if len(targets) != 2 {
		t.Fatalf("targets = %#v, want both", targets)
	}
	for _, tg := range targets {
		if tg["assigned"] != false {
			t.Fatalf("target %#v is assigned with no identity", tg)
		}
	}
	wantCache(t, x, repo)
	if n := api.count("EnsureRepoIdentity"); n != 0 {
		t.Fatalf("the read created an identity (calls %v)", api.allCalls())
	}
	if n := api.count("RoleSSHTargets"); n != 0 {
		t.Fatalf("RoleSSHTargets called with no role (calls %v)", api.allCalls())
	}
}

// TestWarpgateTargetListPartialIdentity: the ROLE carries the targets, so a
// role whose user is missing still has its assignments read and cached.
func TestWarpgateTargetListPartialIdentity(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha, targetBeta)
	x, repo := newWarpgateTargetServer(t, api)
	role := api.seedRoleOnly(repo.ID)
	api.assign(role, targetBeta.ID)

	resp := x.do("GET", sshTargetsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	targets := entriesOf(t, decodeBody(t, resp), "targets")
	if len(targets) != 2 || targets[0]["assigned"] != false || targets[1]["assigned"] != true {
		t.Fatalf("targets = %#v, want beta assigned", targets)
	}
	wantCache(t, x, repo, store.SSHTarget{ID: targetBeta.ID, Name: targetBeta.Name})
}

// TestWarpgateTargetListEmpty: Warpgate has no SSH targets — configured:true
// and an empty array, never null.
func TestWarpgateTargetListEmpty(t *testing.T) {
	x, repo := newWarpgateTargetServer(t, newFakeWarpgateAPI())

	resp := x.do("GET", sshTargetsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if raw := rawBody(t, resp); raw != `{"configured":true,"targets":[]}`+"\n" {
		t.Fatalf("body = %q, want configured:true with an empty array", raw)
	}
}

// --- Warpgate failures ------------------------------------------------------

// TestWarpgateTargetsUpstreamFailure: configured, but the admin API call
// failed. Every route answers 502 naming Warpgate, and the cache is left
// alone — a failed read is not a truth to replace it with.
func TestWarpgateTargetsUpstreamFailure(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha)
	x, repo := newWarpgateTargetServer(t, api)
	role := api.seedIdentity(repo.ID)
	api.assign(role, targetAlpha.ID)
	cached := store.SSHTarget{ID: targetAlpha.ID, Name: targetAlpha.Name}
	seedCache(t, x, repo, cached)
	api.setFail(errors.New("warpgate GET /@warpgate/admin/api/targets: dial tcp 127.0.0.1:8888: connect: connection refused"))

	cases := []struct {
		method, path string
		headers      map[string]string
	}{
		{"GET", sshTargetsPath(repo), nil},
		{"PUT", sshTargetPath(repo, targetAlpha.ID), csrfHeaders(x.ts.URL)},
		{"DELETE", sshTargetPath(repo, targetAlpha.ID), csrfHeaders(x.ts.URL)},
	}
	for _, tc := range cases {
		resp := x.do(tc.method, tc.path, nil, tc.headers)
		wantStatus(t, resp, http.StatusBadGateway)
		msg := wantErrorBody(t, resp)
		if !strings.Contains(msg, "Warpgate") || !strings.Contains(msg, "connection refused") {
			t.Errorf("%s %s error = %q, want it to name Warpgate and carry the cause", tc.method, tc.path, msg)
		}
	}
	wantCache(t, x, repo, cached)
}

// TestWarpgateTargetsRejectedToken drives the REAL client against an admin API
// that rejects the token: the 502 carries the client's own 401 text — which
// names the flag to fix — and never the token.
func TestWarpgateTargetsRejectedToken(t *testing.T) {
	const token = "wg-admin-token-must-not-leak"
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(stub.Close)
	client, err := warpgate.New(warpgate.Options{BaseURL: stub.URL, Token: token})
	if err != nil {
		t.Fatalf("warpgate.New: %v", err)
	}
	x := newTestServer(t, func(o *Options) {
		o.Warpgate = client
		o.WarpgateAPIURL = stub.URL
	})
	x.setup("op", "password123")
	repo := seedTrackerRepo(t, x, "ssh-picker", nil)

	for _, rq := range []struct {
		method, path string
		headers      map[string]string
	}{
		{"GET", sshTargetsPath(repo), nil},
		{"PUT", sshTargetPath(repo, "tgt-1"), csrfHeaders(x.ts.URL)},
		{"DELETE", sshTargetPath(repo, "tgt-1"), csrfHeaders(x.ts.URL)},
	} {
		resp := x.do(rq.method, rq.path, nil, rq.headers)
		wantStatus(t, resp, http.StatusBadGateway)
		msg := wantErrorBody(t, resp)
		if !strings.Contains(msg, "401") || !strings.Contains(msg, "--warpgate-admin-token-file") {
			t.Errorf("%s %s error = %q, want the client's 401 advice", rq.method, rq.path, msg)
		}
		if strings.Contains(msg, token) {
			t.Fatalf("%s %s leaked the admin token: %q", rq.method, rq.path, msg)
		}
	}

	// Health through the same real client: a reachability failure, never the
	// token.
	body, raw := x.warpgateHealth()
	wantWarpgateState(t, body, warpgateStateUnreachable)
	if strings.Contains(raw, token) {
		t.Fatalf("health leaked the admin token: %s", raw)
	}
}

// --- assign -----------------------------------------------------------------

// TestWarpgateTargetAssign walks the toggle's "on" side: the identity is
// ensured under the repo's ID and name, the role is assigned to the target,
// the cache gains the row, and a repeat is a harmless no-op.
func TestWarpgateTargetAssign(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha, targetBeta)
	x, repo := newWarpgateTargetServer(t, api)

	resp := x.do("PUT", sshTargetPath(repo, targetBeta.ID), nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	if body := rawBody(t, resp); body != "" {
		t.Errorf("204 carried a body: %q", body)
	}

	if n := api.count("EnsureRepoIdentity " + repo.ID + " " + repo.Name); n != 1 {
		t.Fatalf("EnsureRepoIdentity(%s, %s) called %d times (calls %v)", repo.ID, repo.Name, n, api.allCalls())
	}
	identity, found, _ := api.FindRepoIdentity(context.Background(), repo.ID)
	if !found {
		t.Fatal("the assign did not create the repo's identity")
	}
	if !api.isAssigned(identity.Role.ID, targetBeta.ID) {
		t.Fatalf("beta not assigned to %s (calls %v)", identity.Role.ID, api.allCalls())
	}
	// The target was validated against the SSH listing before anything was
	// written: list, then ensure, then assign.
	calls := api.allCalls()
	iList := slices.Index(calls, "ListSSHTargets")
	iEnsure := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "EnsureRepoIdentity") })
	iAssign := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "AssignTargetRole") })
	if iList < 0 || iEnsure < iList || iAssign < iEnsure {
		t.Fatalf("calls %v, want ListSSHTargets, then EnsureRepoIdentity, then AssignTargetRole", calls)
	}
	wantCache(t, x, repo, store.SSHTarget{ID: targetBeta.ID, Name: targetBeta.Name})

	// Idempotent.
	resp = x.do("PUT", sshTargetPath(repo, targetBeta.ID), nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	wantCache(t, x, repo, store.SSHTarget{ID: targetBeta.ID, Name: targetBeta.Name})

	// And the listing now reports it.
	resp = x.do("GET", sshTargetsPath(repo), nil, nil)
	wantStatus(t, resp, http.StatusOK)
	targets := entriesOf(t, decodeBody(t, resp), "targets")
	if len(targets) != 2 || targets[0]["assigned"] != false || targets[1]["assigned"] != true {
		t.Fatalf("targets after the assign = %#v, want beta assigned", targets)
	}
}

// TestWarpgateTargetAssignUnknownTarget: an id that is not in Warpgate's SSH
// listing — unknown, or an HTTP/database target the client filtered out — is
// a 404, refused before the identity is even ensured.
func TestWarpgateTargetAssignUnknownTarget(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha)
	x, repo := newWarpgateTargetServer(t, api)

	resp := x.do("PUT", sshTargetPath(repo, "tgt-http-dashboard"), nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNotFound)
	if msg := wantErrorBody(t, resp); !strings.Contains(msg, "not an SSH target known to Warpgate") {
		t.Fatalf("error = %q", msg)
	}
	if n := api.count("EnsureRepoIdentity") + api.count("AssignTargetRole"); n != 0 {
		t.Fatalf("a refused assign still wrote to Warpgate (calls %v)", api.allCalls())
	}
	wantCache(t, x, repo)
}

// --- unassign ---------------------------------------------------------------

// TestWarpgateTargetUnassign pins the toggle's "off" side, including the case
// that must never create anything.
func TestWarpgateTargetUnassign(t *testing.T) {
	t.Run("existing identity", func(t *testing.T) {
		api := newFakeWarpgateAPI(targetAlpha, targetBeta)
		x, repo := newWarpgateTargetServer(t, api)
		role := api.seedIdentity(repo.ID)
		api.assign(role, targetAlpha.ID, targetBeta.ID)
		seedCache(t, x, repo, store.SSHTarget{ID: targetAlpha.ID, Name: targetAlpha.Name}, store.SSHTarget{ID: targetBeta.ID, Name: targetBeta.Name})

		resp := x.do("DELETE", sshTargetPath(repo, targetAlpha.ID), nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusNoContent)
		if body := rawBody(t, resp); body != "" {
			t.Errorf("204 carried a body: %q", body)
		}
		if n := api.count("UnassignTargetRole " + targetAlpha.ID + " " + role); n != 1 {
			t.Fatalf("UnassignTargetRole(alpha, %s) called %d times (calls %v)", role, n, api.allCalls())
		}
		if api.isAssigned(role, targetAlpha.ID) || !api.isAssigned(role, targetBeta.ID) {
			t.Fatal("the unassign removed the wrong assignment")
		}
		wantCache(t, x, repo, store.SSHTarget{ID: targetBeta.ID, Name: targetBeta.Name})
		if n := api.count("EnsureRepoIdentity"); n != 0 {
			t.Fatalf("the unassign ensured an identity (calls %v)", api.allCalls())
		}
	})

	t.Run("no identity still clears the cache", func(t *testing.T) {
		api := newFakeWarpgateAPI(targetAlpha)
		x, repo := newWarpgateTargetServer(t, api)
		seedCache(t, x, repo, store.SSHTarget{ID: targetAlpha.ID, Name: targetAlpha.Name})

		resp := x.do("DELETE", sshTargetPath(repo, targetAlpha.ID), nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusNoContent)
		_ = resp.Body.Close()
		if n := api.count("UnassignTargetRole") + api.count("EnsureRepoIdentity"); n != 0 {
			t.Fatalf("an unassign with no identity still wrote to Warpgate (calls %v)", api.allCalls())
		}
		wantCache(t, x, repo)
	})
}

// --- validation, unknown repos, guards --------------------------------------

// TestWarpgateTargetInvalidID: an id carrying a (percent-encoded) slash or a
// control character never steers a Warpgate request; a truncated URL never
// matches the route.
func TestWarpgateTargetInvalidID(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha)
	x, repo := newWarpgateTargetServer(t, api)

	for _, id := range []string{"a%2Fb", "..%2F..%2Froles", "tgt%01x", "%20"} {
		for _, method := range []string{"PUT", "DELETE"} {
			resp := x.do(method, sshTargetPath(repo, id), nil, csrfHeaders(x.ts.URL))
			wantStatus(t, resp, http.StatusBadRequest)
			wantErrorBody(t, resp)
		}
	}
	for _, method := range []string{"PUT", "DELETE"} {
		resp := x.do(method, sshTargetsPath(repo)+"/", nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusNotFound)
		_ = resp.Body.Close()
	}
	if calls := api.allCalls(); len(calls) != 0 {
		t.Fatalf("an invalid target id reached Warpgate: %v", calls)
	}
}

// TestWarpgateTargetUnknownRepo: the path names a repo, and an unknown one is
// a 404 on every route, before Warpgate is asked anything.
func TestWarpgateTargetUnknownRepo(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha)
	x, _ := newWarpgateTargetServer(t, api)

	for _, rq := range []struct{ method, path string }{
		{"GET", "/api/v1/repos/repo_missing/warpgate/targets"},
		{"PUT", "/api/v1/repos/repo_missing/warpgate/targets/" + targetAlpha.ID},
		{"DELETE", "/api/v1/repos/repo_missing/warpgate/targets/" + targetAlpha.ID},
	} {
		resp := x.do(rq.method, rq.path, nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusNotFound)
		_ = resp.Body.Close()
	}
	if calls := api.allCalls(); len(calls) != 0 {
		t.Fatalf("an unknown repo reached Warpgate: %v", calls)
	}
}

// TestWarpgateTargetRoutesRequireAuthAndCSRF proves requireAuth is on all
// three routes (401 without a session, the normal answer with one), and that
// the two toggles are CSRF-guarded like every other mutation on the mux.
func TestWarpgateTargetRoutesRequireAuthAndCSRF(t *testing.T) {
	api := newFakeWarpgateAPI(targetAlpha)
	x, repo := newWarpgateTargetServer(t, api)

	routes := []struct {
		method, path string
		authed       int
	}{
		{"GET", sshTargetsPath(repo), http.StatusOK},
		{"PUT", sshTargetPath(repo, targetAlpha.ID), http.StatusNoContent},
		{"DELETE", sshTargetPath(repo, targetAlpha.ID), http.StatusNoContent},
	}
	for _, rt := range routes {
		resp := doWith(t, http.DefaultClient, x.ts.URL, rt.method, rt.path, nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, http.StatusUnauthorized)
		_ = resp.Body.Close()
	}
	for _, rt := range routes[1:] {
		resp := x.do(rt.method, rt.path, nil, nil)
		wantStatus(t, resp, http.StatusForbidden)
		_ = resp.Body.Close()
		resp = x.do(rt.method, rt.path, nil, csrfHeaders("https://evil.example"))
		wantStatus(t, resp, http.StatusForbidden)
		_ = resp.Body.Close()
	}
	if calls := api.allCalls(); len(calls) != 0 {
		t.Fatalf("a refused request reached Warpgate: %v", calls)
	}
	for _, rt := range routes {
		resp := x.do(rt.method, rt.path, nil, csrfHeaders(x.ts.URL))
		wantStatus(t, resp, rt.authed)
		_ = resp.Body.Close()
	}
}
