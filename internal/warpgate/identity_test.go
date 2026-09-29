package warpgate

// The per-repo identity's decision table — the most load-bearing behaviour in
// the package. Lab calls EnsureRepoIdentity at spawn, concurrently, forever,
// and the "one user, one role, never a duplicate, never a destructive
// recreate" rules asserted here are what the whole bastion design rests on.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
)

const (
	testRepoID = "repo_0123456789abcdef0123456789abcdef"
	testSlug   = "repo-0123456789abcdef0123456789abcdef"
)

// TestRepoSlugMatchesOneCLIAgentIdentifier: one repo has ONE slug across both
// sidecars. The derivation is re-stated in this package (to keep it free of a
// OneCLI dependency); this test is what keeps the two copies identical.
func TestRepoSlugMatchesOneCLIAgentIdentifier(t *testing.T) {
	for _, id := range []string{
		testRepoID,
		"repo_ffffffffffffffffffffffffffffffff",
		"repo_00000000000000000000000000000000",
		"REPO_ABCDEF",
		"__weird..id__with spaces/and:colons#hash",
		"ticket-abc",
		strings.Repeat("a", 80),
		"-leading-hyphens",
		"ünïcødé",
		"",
	} {
		if got, want := RepoSlug(id), onecli.AgentIdentifier(id); got != want {
			t.Errorf("RepoSlug(%q) = %q, onecli.AgentIdentifier = %q", id, got, want)
		}
	}
	if got := RepoSlug(testRepoID); got != testSlug {
		t.Errorf("RepoSlug(%q) = %q, want %q", testRepoID, got, testSlug)
	}
}

func TestIdentityOperationsRejectBadRepoIDs(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	ctx := context.Background()
	for _, id := range []string{"", "   ", "___", "ticket_123"} {
		if _, err := c.EnsureRepoIdentity(ctx, id, "x"); err == nil {
			t.Errorf("EnsureRepoIdentity(%q) succeeded, want refusal", id)
		}
		if _, _, err := c.FindRepoIdentity(ctx, id); err == nil {
			t.Errorf("FindRepoIdentity(%q) succeeded, want refusal", id)
		}
		if _, err := c.DeleteRepoIdentity(ctx, id); err == nil {
			t.Errorf("DeleteRepoIdentity(%q) succeeded, want refusal", id)
		}
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Errorf("rejected calls reached the server: %v", reqs)
	}
}

// TestEnsureRepoIdentityCreatesEverything: the first ensure of a repo creates
// the user, the role and the grant — with the exact bodies upstream expects.
func TestEnsureRepoIdentityCreatesEverything(t *testing.T) {
	f := newFake(t)
	c := f.client(t)

	id, err := c.EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
	if err != nil {
		t.Fatalf("EnsureRepoIdentity: %v", err)
	}
	if id.User.Username != testSlug || id.User.Description != "Coding Lab" || id.User.ID == "" {
		t.Errorf("user = %+v", id.User)
	}
	if id.Role.Name != testSlug || id.Role.Description != "Coding Lab" || id.Role.ID == "" {
		t.Errorf("role = %+v", id.Role)
	}
	if !f.grantActive(id.User.ID, id.Role.ID) {
		t.Error("the user does not hold the role after ensure")
	}

	writes := f.writes()
	want := []struct{ method, path, body string }{
		{http.MethodPost, "/@warpgate/admin/api/users", `{"username":"` + testSlug + `","description":"Coding Lab"}`},
		{http.MethodPost, "/@warpgate/admin/api/roles", `{"name":"` + testSlug + `","description":"Coding Lab","is_default":false}`},
		{http.MethodPost, "/@warpgate/admin/api/users/" + id.User.ID + "/roles/" + id.Role.ID, `{}`},
	}
	if len(writes) != len(want) {
		t.Fatalf("writes = %v, want %d", writes, len(want))
	}
	for i, w := range want {
		got := writes[i]
		if got.Method != w.method || got.Path != w.path || strings.TrimSpace(got.Body) != w.body {
			t.Errorf("write %d = %s %s %s, want %s %s %s", i, got.Method, got.Path, got.Body, w.method, w.path, w.body)
		}
		if ct := got.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("write %d Content-Type = %q", i, ct)
		}
	}
	// The lookups narrow with ?search=<slug>.
	for _, r := range f.requests() {
		if r.Method == http.MethodGet && (r.Path == "/@warpgate/admin/api/users" || r.Path == "/@warpgate/admin/api/roles") {
			if got := r.Query.Get("search"); got != testSlug {
				t.Errorf("%v search = %q, want %q", r, got, testSlug)
			}
		}
	}
}

// TestEnsureRepoIdentitySteadyStateIsReadOnly: every ensure after the first
// is three GETs and no write.
func TestEnsureRepoIdentitySteadyStateIsReadOnly(t *testing.T) {
	f := newFake(t)
	uid := f.seedUser("", testSlug, "Coding Lab", nil)
	rid := f.seedRole("", testSlug, "Coding Lab", false)
	f.seedGrant(uid, rid, true)
	c := f.client(t)

	id, err := c.EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
	if err != nil {
		t.Fatalf("EnsureRepoIdentity: %v", err)
	}
	if id.User.ID != uid || id.Role.ID != rid {
		t.Errorf("identity = %+v, want user %s role %s", id, uid, rid)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("steady-state ensure wrote: %v", w)
	}
	if n := len(f.requests()); n != 3 {
		t.Errorf("steady-state ensure issued %d requests, want 3: %v", n, f.requests())
	}
}

// TestEnsureRepoIdentityMatchesExactly: upstream's search is a substring
// LIKE, so a lookup must never adopt a user or role whose name merely
// contains the slug.
func TestEnsureRepoIdentityMatchesExactly(t *testing.T) {
	f := newFake(t)
	f.seedUser("", testSlug+"-old", "Other", nil)
	f.seedUser("", "x"+testSlug, "Other", nil)
	f.seedRole("", testSlug+"-old", "Other", false)
	c := f.client(t)

	id, err := c.EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
	if err != nil {
		t.Fatalf("EnsureRepoIdentity: %v", err)
	}
	if id.User.Username != testSlug || id.Role.Name != testSlug {
		t.Errorf("identity = %+v, want the exact slug for both", id)
	}
	if f.count(http.MethodPost, rUsers) != 1 || f.count(http.MethodPost, rRoles) != 1 {
		t.Errorf("want one user and one role created, writes: %v", f.writes())
	}
}

// TestEnsureRepoIdentityHealsUserDescriptionPreservingEverythingElse: PUT
// replaces the whole user upstream, so the heal must echo every field it does
// not own — including ones lab does not even know about — byte for byte.
func TestEnsureRepoIdentityHealsUserDescriptionPreservingEverythingElse(t *testing.T) {
	f := newFake(t)
	extra := map[string]string{
		"credential_policy":           `{"http":null,"ssh":["PublicKey"],"mysql":null,"postgres":null,"vnc":null,"rdp":null}`,
		"rate_limit_bytes_per_second": `1048576`,
		"allowed_ip_ranges":           `["10.88.0.0/16","192.168.1.1/32"]`,
		"future_field":                `{"nested":[1,2,3]}`,
	}
	uid := f.seedUser("", testSlug, "the old repo name", extra)
	rid := f.seedRole("", testSlug, "Coding Lab", false)
	f.seedGrant(uid, rid, true)
	c := f.client(t)

	id, err := c.EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
	if err != nil {
		t.Fatalf("EnsureRepoIdentity: %v", err)
	}
	if id.User.Description != "Coding Lab" {
		t.Errorf("user description = %q, want the healed one", id.User.Description)
	}
	if got := f.userField(uid, "description"); got != `"Coding Lab"` {
		t.Errorf("stored description = %s", got)
	}

	var put *recordedRequest
	var sawGet bool
	for _, r := range f.requests() {
		if r.Path == "/@warpgate/admin/api/users/"+uid {
			switch r.Method {
			case http.MethodGet:
				sawGet = true
			case http.MethodPut:
				r := r
				put = &r
			}
		}
	}
	if !sawGet || put == nil {
		t.Fatalf("heal must GET then PUT the user; requests: %v", f.requests())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(put.Body), &body); err != nil {
		t.Fatalf("PUT body %q: %v", put.Body, err)
	}
	if string(body["username"]) != `"`+testSlug+`"` || string(body["description"]) != `"Coding Lab"` {
		t.Errorf("PUT username/description = %s / %s", body["username"], body["description"])
	}
	for field, want := range extra {
		if got := string(body[field]); got != want {
			t.Errorf("PUT %s = %s, want it passed through untouched as %s", field, got, want)
		}
		if field != "future_field" {
			if got := f.userField(uid, field); got != want {
				t.Errorf("stored %s after heal = %s, want %s", field, got, want)
			}
		}
	}
}

// TestEnsureRepoIdentityFailedUserHealIsCosmetic: a heal that fails must not
// fail the ensure — the spawn path is fail-closed on it.
func TestEnsureRepoIdentityFailedUserHealIsCosmetic(t *testing.T) {
	f := newFake(t)
	uid := f.seedUser("", testSlug, "stale", nil)
	rid := f.seedRole("", testSlug, "stale", false)
	f.seedGrant(uid, rid, true)
	f.forced["PUT "+rUser] = http.StatusInternalServerError
	f.forced["PUT "+rRole] = http.StatusInternalServerError
	c := f.client(t)

	id, err := c.EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
	if err != nil {
		t.Fatalf("EnsureRepoIdentity with failing heals: %v", err)
	}
	if id.User.Description != "stale" || id.Role.Description != "stale" {
		t.Errorf("identity = %+v, want the stale descriptions reported", id)
	}
}

// TestEnsureRepoIdentityHealsRole: description and the default flag are
// healed in place with the full create-shaped body; a default role that
// cannot be reset is an error, because that flag leaks targets across repos.
func TestEnsureRepoIdentityHealsRole(t *testing.T) {
	t.Run("description", func(t *testing.T) {
		f := newFake(t)
		uid := f.seedUser("", testSlug, "Coding Lab", nil)
		rid := f.seedRole("", testSlug, "old", false)
		f.seedGrant(uid, rid, true)
		id, err := f.client(t).EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
		if err != nil {
			t.Fatalf("EnsureRepoIdentity: %v", err)
		}
		if id.Role.Description != "Coding Lab" {
			t.Errorf("role = %+v", id.Role)
		}
		w := f.writes()
		if len(w) != 1 || w[0].Method != http.MethodPut || w[0].Path != "/@warpgate/admin/api/role/"+rid ||
			strings.TrimSpace(w[0].Body) != `{"name":"`+testSlug+`","description":"Coding Lab","is_default":false}` {
			t.Errorf("writes = %v %q", w, bodies(w))
		}
	})
	t.Run("default flag reset", func(t *testing.T) {
		f := newFake(t)
		uid := f.seedUser("", testSlug, "Coding Lab", nil)
		rid := f.seedRole("", testSlug, "Coding Lab", true)
		f.seedGrant(uid, rid, true)
		if _, err := f.client(t).EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab"); err != nil {
			t.Fatalf("EnsureRepoIdentity: %v", err)
		}
		if r := f.rolesNamed(testSlug); len(r) != 1 || r[0].IsDefault {
			t.Errorf("role after ensure = %+v, want is_default false", r)
		}
	})
	t.Run("default flag reset fails", func(t *testing.T) {
		f := newFake(t)
		uid := f.seedUser("", testSlug, "Coding Lab", nil)
		rid := f.seedRole("", testSlug, "Coding Lab", true)
		f.seedGrant(uid, rid, true)
		f.forced["PUT "+rRole] = http.StatusInternalServerError
		_, err := f.client(t).EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
		if err == nil || !strings.Contains(err.Error(), "marked default") {
			t.Errorf("error = %v, want the default-role refusal", err)
		}
	})
}

func bodies(rs []recordedRequest) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Body)
	}
	return out
}

// TestEnsureRepoIdentityPicksSmallestDuplicateRole: upstream allows duplicate
// role names; every caller must converge on the same one.
func TestEnsureRepoIdentityPicksSmallestDuplicateRole(t *testing.T) {
	f := newFake(t)
	uid := f.seedUser("", testSlug, "Coding Lab", nil)
	f.seedRole("bbbbbbbb-0000-4000-8000-000000000000", testSlug, "Coding Lab", false)
	f.seedRole("aaaaaaaa-0000-4000-8000-000000000000", testSlug, "Coding Lab", false)
	f.seedRole("cccccccc-0000-4000-8000-000000000000", testSlug, "Coding Lab", false)
	c := f.client(t)

	id, err := c.EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
	if err != nil {
		t.Fatalf("EnsureRepoIdentity: %v", err)
	}
	const smallest = "aaaaaaaa-0000-4000-8000-000000000000"
	if id.Role.ID != smallest {
		t.Errorf("role id = %s, want the smallest %s", id.Role.ID, smallest)
	}
	if !f.grantActive(uid, smallest) {
		t.Error("the grant did not go to the smallest-id role")
	}
	if f.count(http.MethodPost, rRoles) != 0 {
		t.Error("ensure created a role although three exist")
	}
	found, ok, err := c.FindRepoIdentity(context.Background(), testRepoID)
	if err != nil || !ok || found.Role.ID != smallest {
		t.Errorf("FindRepoIdentity = %+v, %v, %v; want the same smallest role", found, ok, err)
	}
}

// TestEnsureRepoIdentityConcurrentEnsuresCreateOnce: lab ensures at spawn,
// concurrently. Without the per-slug lock, goroutines would all miss the role
// in the (deliberately slowed) listing and each create one — upstream has no
// constraint to stop them. With it, exactly one of each write happens.
func TestEnsureRepoIdentityConcurrentEnsuresCreateOnce(t *testing.T) {
	f := newFake(t)
	f.listDelay = 5 * time.Millisecond
	c := f.client(t)

	const n = 8
	var wg sync.WaitGroup
	results := make([]Identity, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("ensure %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Errorf("ensure %d = %+v, ensure 0 = %+v", i, results[i], results[0])
		}
	}
	if got := f.count(http.MethodPost, rUsers); got != 1 {
		t.Errorf("POST /users happened %d times, want 1", got)
	}
	if got := f.count(http.MethodPost, rRoles); got != 1 {
		t.Errorf("POST /roles happened %d times, want 1", got)
	}
	if got := f.count(http.MethodPost, rUserRole); got != 1 {
		t.Errorf("role grant happened %d times, want 1", got)
	}
	if roles := f.rolesNamed(testSlug); len(roles) != 1 {
		t.Errorf("%d roles named %s exist, want 1", len(roles), testSlug)
	}
	if n := len(c.slugLocks.locks); n != 0 {
		t.Errorf("slug lock map holds %d entries after all ensures returned, want 0", n)
	}
}

// TestEnsureRepoIdentityUsernameRace: another process created the user between
// our list and our create; upstream answers 400 "username" and the ensure must
// adopt the winner rather than fail or duplicate.
func TestEnsureRepoIdentityUsernameRace(t *testing.T) {
	f := newFake(t)
	var winner string
	f.beforeCreateUser = func(f *fakeWarpgate) {
		if winner != "" {
			return
		}
		winner = f.newID()
		f.users[winner] = map[string]json.RawMessage{
			"id": jsonString(winner), "username": jsonString(testSlug), "description": jsonString("Coding Lab"),
		}
	}
	c := f.client(t)

	id, err := c.EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
	if err != nil {
		t.Fatalf("EnsureRepoIdentity: %v", err)
	}
	if id.User.ID != winner {
		t.Errorf("user id = %s, want the race winner %s", id.User.ID, winner)
	}
	if ids := f.usersNamed(testSlug); len(ids) != 1 {
		t.Errorf("%d users named %s, want 1", len(ids), testSlug)
	}
}

// TestEnsureRepoIdentityUsernameTakenByCaseVariant: upstream compares
// usernames case-insensitively; a case variant blocks the create yet never
// matches exactly — a loud error, never an adopted foreign user.
func TestEnsureRepoIdentityUsernameTakenByCaseVariant(t *testing.T) {
	f := newFake(t)
	f.seedUser("", strings.ToUpper(testSlug), "someone else", nil)
	_, err := f.client(t).EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
	if err == nil || !strings.Contains(err.Error(), "case-insensitively") {
		t.Errorf("error = %v, want the case-variant refusal", err)
	}
	if f.count(http.MethodPost, rRoles) != 0 {
		t.Error("ensure went on to create a role after the user step failed")
	}
}

// TestEnsureRepoIdentityGrant: an inactive (revoked/expired) assignment row is
// not a grant — the ensure re-grants; and the grant's 409 (unknown ROLE
// upstream) is an error, never "already granted".
func TestEnsureRepoIdentityGrant(t *testing.T) {
	t.Run("inactive row is re-granted", func(t *testing.T) {
		f := newFake(t)
		uid := f.seedUser("", testSlug, "Coding Lab", nil)
		rid := f.seedRole("", testSlug, "Coding Lab", false)
		f.seedGrant(uid, rid, false)
		if _, err := f.client(t).EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab"); err != nil {
			t.Fatalf("EnsureRepoIdentity: %v", err)
		}
		if !f.grantActive(uid, rid) {
			t.Error("inactive grant was not re-activated")
		}
		w := f.writes()
		if len(w) != 1 || w[0].Path != "/@warpgate/admin/api/users/"+uid+"/roles/"+rid || strings.TrimSpace(w[0].Body) != "{}" {
			t.Errorf("writes = %v %q, want one grant POST with {}", w, bodies(w))
		}
	})
	t.Run("409 is an error", func(t *testing.T) {
		f := newFake(t)
		f.seedUser("", testSlug, "Coding Lab", nil)
		f.seedRole("", testSlug, "Coding Lab", false)
		f.forced["POST "+rUserRole] = http.StatusConflict
		_, err := f.client(t).EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
		if !isStatus(err, http.StatusConflict) {
			t.Errorf("error = %v, want the 409 surfaced", err)
		}
	})
}

// TestEnsureRepoIdentityHonoursContextWhileWaiting: a spawn cancelled while
// another ensure of the same repo holds the lock gives up instead of queueing.
func TestEnsureRepoIdentityHonoursContextWhileWaiting(t *testing.T) {
	f := newFake(t)
	c := f.client(t)
	unlock, err := c.slugLocks.lock(context.Background(), testSlug)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.EnsureRepoIdentity(ctx, testRepoID, "Coding Lab"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the context deadline", err)
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Errorf("a cancelled waiter reached the server: %v", reqs)
	}
}

func TestFindRepoIdentity(t *testing.T) {
	t.Run("nothing", func(t *testing.T) {
		f := newFake(t)
		id, ok, err := f.client(t).FindRepoIdentity(context.Background(), testRepoID)
		if err != nil || ok || id != (Identity{}) {
			t.Errorf("= %+v, %v, %v; want zero, false, nil", id, ok, err)
		}
	})
	t.Run("user only", func(t *testing.T) {
		f := newFake(t)
		uid := f.seedUser("", testSlug, "Coding Lab", nil)
		id, ok, err := f.client(t).FindRepoIdentity(context.Background(), testRepoID)
		if err != nil || ok || id.User.ID != uid || id.Role != (Role{}) {
			t.Errorf("= %+v, %v, %v; want the user, no role, found false", id, ok, err)
		}
	})
	t.Run("role only", func(t *testing.T) {
		f := newFake(t)
		rid := f.seedRole("", testSlug, "Coding Lab", false)
		id, ok, err := f.client(t).FindRepoIdentity(context.Background(), testRepoID)
		if err != nil || ok || id.Role.ID != rid || id.User != (User{}) {
			t.Errorf("= %+v, %v, %v; want the role, no user, found false", id, ok, err)
		}
	})
	t.Run("both, read-only", func(t *testing.T) {
		f := newFake(t)
		uid := f.seedUser("", testSlug, "stale name", nil)
		rid := f.seedRole("", testSlug, "stale name", true)
		id, ok, err := f.client(t).FindRepoIdentity(context.Background(), testRepoID)
		want := Identity{
			User: User{ID: uid, Username: testSlug, Description: "stale name"},
			Role: Role{ID: rid, Name: testSlug, Description: "stale name"},
		}
		if err != nil || !ok || id != want {
			t.Errorf("= %+v, %v, %v; want %+v, true", id, ok, err, want)
		}
		if w := f.writes(); len(w) != 0 {
			t.Errorf("FindRepoIdentity wrote: %v", w)
		}
	})
	t.Run("upstream failure", func(t *testing.T) {
		f := newFake(t)
		f.forced["GET "+rRoles] = http.StatusInternalServerError
		if _, _, err := f.client(t).FindRepoIdentity(context.Background(), testRepoID); err == nil {
			t.Error("FindRepoIdentity succeeded against a failing role listing")
		}
	})
}

func TestDeleteRepoIdentity(t *testing.T) {
	t.Run("user and every duplicate role, nothing else", func(t *testing.T) {
		f := newFake(t)
		uid := f.seedUser("", testSlug, "Coding Lab", nil)
		other := f.seedUser("", testSlug+"-other", "Other", nil)
		r1 := f.seedRole("", testSlug, "Coding Lab", false)
		r2 := f.seedRole("", testSlug, "Coding Lab", false)
		keep := f.seedRole("", testSlug+"-other", "Other", false)

		deleted, err := f.client(t).DeleteRepoIdentity(context.Background(), testRepoID)
		if err != nil || !deleted {
			t.Fatalf("DeleteRepoIdentity = %v, %v; want true, nil", deleted, err)
		}
		var got []string
		for _, w := range f.writes() {
			got = append(got, w.Method+" "+w.Path)
		}
		want := []string{
			"DELETE /@warpgate/admin/api/users/" + uid,
			"DELETE /@warpgate/admin/api/role/" + r1,
			"DELETE /@warpgate/admin/api/role/" + r2,
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if len(f.usersNamed(testSlug+"-other")) != 1 || len(f.rolesNamed(testSlug+"-other")) != 1 {
			t.Errorf("a substring match (%s / %s) was deleted", other, keep)
		}
	})
	t.Run("absent", func(t *testing.T) {
		f := newFake(t)
		deleted, err := f.client(t).DeleteRepoIdentity(context.Background(), testRepoID)
		if err != nil || deleted {
			t.Errorf("= %v, %v; want false, nil", deleted, err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Errorf("an absent identity produced writes: %v", w)
		}
	})
	t.Run("404 on delete counts as gone", func(t *testing.T) {
		f := newFake(t)
		f.seedUser("", testSlug, "Coding Lab", nil)
		f.forced["DELETE "+rUser] = http.StatusNotFound
		deleted, err := f.client(t).DeleteRepoIdentity(context.Background(), testRepoID)
		if err != nil || deleted {
			t.Errorf("= %v, %v; want false, nil", deleted, err)
		}
	})
	t.Run("failure reports what was already deleted", func(t *testing.T) {
		f := newFake(t)
		f.seedUser("", testSlug, "Coding Lab", nil)
		f.seedRole("", testSlug, "Coding Lab", false)
		f.forced["DELETE "+rRole] = http.StatusInternalServerError
		deleted, err := f.client(t).DeleteRepoIdentity(context.Background(), testRepoID)
		if err == nil || !deleted {
			t.Errorf("= %v, %v; want true and the role error", deleted, err)
		}
	})
}

// TestUnknownListShapeIsLoud: a listing this package cannot read must never
// look like "no such user" — the ensure would then create a duplicate.
func TestUnknownListShapeIsLoud(t *testing.T) {
	for _, body := range []string{`{"users":[]}`, `"nope"`, `[{"id":1}]`} {
		s := newStub(t, answer(http.StatusOK, body))
		_, err := newTestClient(t, s.URL).EnsureRepoIdentity(context.Background(), testRepoID, "Coding Lab")
		if err == nil || !strings.Contains(err.Error(), "internal/warpgate/wire.go") {
			t.Errorf("body %s: error = %v, want a loud wire-shape error", body, err)
		}
		for _, r := range s.requests() {
			if r.Method != http.MethodGet {
				t.Errorf("body %s: a write followed an unreadable listing: %v", body, r)
			}
		}
	}
}

func TestKeyedMutexReleasesAndCancels(t *testing.T) {
	k := newKeyedMutex()
	unlockA, err := k.lock(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	// A different key is independent.
	unlockB, err := k.lock(context.Background(), "b")
	if err != nil {
		t.Fatalf("lock b while a is held: %v", err)
	}
	unlockB()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := k.lock(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("lock on a held key with a cancelled ctx = %v, want context.Canceled", err)
	}
	unlockA()
	unlockA() // idempotent
	if n := len(k.locks); n != 0 {
		t.Errorf("lock map holds %d entries, want 0", n)
	}
	unlock, err := k.lock(context.Background(), "a")
	if err != nil {
		t.Fatalf("relock after release: %v", err)
	}
	unlock()
}
