package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a Server with a fake `claude auth status` command
// (loggedIn picks its canned reply) and harmless login defaults. Call sites
// that exercise tmux still need a real tmux (requireTmux).
func newTestServer(t *testing.T, root string, sessions *Sessions, store *Store, loggedIn bool) *Server {
	t.Helper()
	return NewServer(root, filepath.Join(t.TempDir(), ".claude.json"), sessions, store,
		NewAuth(fakeAuthArgv(loggedIn)), []string{"sleep", "600"}, t.TempDir())
}

func fakeAuthArgv(loggedIn bool) []string {
	body := `{"loggedIn":false}`
	if loggedIn {
		body = `{"loggedIn":true}`
	}
	return []string{"sh", "-c", "echo '" + body + "'"}
}

func TestSnapshot_recencyDescUnstampedAlphaTail(t *testing.T) {
	requireTmux(t)
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		if err := os.MkdirAll(filepath.Join(root, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	mustStamp(t, store, "beta", time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC))
	mustStamp(t, store, "alpha", time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC))
	mustStamp(t, store, "gamma", time.Date(2026, 5, 21, 8, 0, 0, 0, time.UTC))
	// delta + epsilon: unstamped, expected at the tail in alphabetical order.

	srv := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}), store, true)

	rows, _, err := srv.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.Name
	}
	want := []string{"beta", "alpha", "gamma", "delta", "epsilon"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot order = %v; want %v", got, want)
	}
}

func TestSnapshot_allUnstampedKeepsAlphabetical(t *testing.T) {
	requireTmux(t)
	root := t.TempDir()
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		if err := os.MkdirAll(filepath.Join(root, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	store := NewStore(filepath.Join(t.TempDir(), "state.json"))
	srv := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}), store, true)

	rows, _, err := srv.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.Name
	}
	want := []string{"alpha", "bravo", "charlie"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot order = %v; want %v", got, want)
	}
}

// TestSnapshot_repoURLForForgejoOnly proves the ⋯ menu's repo link is wired from
// the (cached) Forgejo detection: a git.cloonar.com origin yields the repo home,
// any other origin yields "" so the template renders no link.
func TestSnapshot_repoURLForForgejoOnly(t *testing.T) {
	t.Run("forgejo origin", func(t *testing.T) {
		// newAFKServer stands up one project "proj" on cloonarOrigin
		// (git@git.cloonar.com:Cloonar/proj.git).
		srv := newAFKServer(t, &fakeTracker{}, &fakeGit{origin: cloonarOrigin})
		rows, _, err := srv.snapshot()
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d; want 1", len(rows))
		}
		if !rows[0].Forgejo {
			t.Fatal("want Forgejo true for a git.cloonar.com origin")
		}
		if want := "https://git.cloonar.com/Cloonar/proj"; rows[0].RepoURL != want {
			t.Errorf("RepoURL = %q; want %q", rows[0].RepoURL, want)
		}
	})
	t.Run("non-forgejo origin", func(t *testing.T) {
		srv := newAFKServer(t, &fakeTracker{}, &fakeGit{origin: "git@github.com:foo/bar.git"})
		rows, _, err := srv.snapshot()
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d; want 1", len(rows))
		}
		if rows[0].Forgejo {
			t.Fatal("want Forgejo false for a github.com origin")
		}
		if rows[0].RepoURL != "" {
			t.Errorf("RepoURL = %q; want empty for a non-Forgejo origin", rows[0].RepoURL)
		}
	})
}

func TestValidateLoginCode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain", "abc123", "abc123", false},
		{"trims surrounding whitespace", "  abc123  ", "abc123", false},
		{"code#state base64url passes", "aB3-_x#state=Zm9v-_", "aB3-_x#state=Zm9v-_", false},
		{"empty", "", "", true},
		{"whitespace only", "   \t  ", "", true},
		{"embedded newline rejected", "abc\ndef", "", true},
		{"embedded NUL rejected", "abc\x00def", "", true},
		{"too long", strings.Repeat("a", maxLoginCodeLen+1), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateLoginCode(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v; wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

// The guard runs the (fake) auth-status command but returns before any tmux
// call, so this needs sh but not tmux.
func TestHandleStart_blockedWhenLoggedOut(t *testing.T) {
	requireSh(t)
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), false)

	rec := httptest.NewRecorder()
	srv.handleStart(rec, httptest.NewRequest(http.MethodPost, "/start/anything", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q; want / (back to the banner)", loc)
	}
}

// Regression: if the render cache holds a stale logged-in result (say the token
// silently expired since the last poll), handleStart must still refuse — using
// the cache here lets a doomed remote-control session spawn for up to authTTL.
func TestHandleStart_refusesWhenCacheStaleButActuallyLoggedOut(t *testing.T) {
	requireSh(t)
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), false)
	// Poison the cache as if a prior poll had seen logged-in: fresh timestamp,
	// state = true. loggedIn() would believe this; forceAuthRefresh re-runs the
	// (fake, logged-out) status command and overwrites it.
	srv.authMu.Lock()
	srv.authState = true
	srv.authChecked = time.Now()
	srv.authMu.Unlock()

	rec := httptest.NewRecorder()
	srv.handleStart(rec, httptest.NewRequest(http.MethodPost, "/start/anything", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("stale-cache start: status %d loc %q; want 303 /", rec.Code, rec.Header().Get("Location"))
	}
	if srv.loggedIn() {
		t.Error("cache should have been refreshed to the real (logged-out) state")
	}
}

func TestIndex_loggedOutRendersBanner(t *testing.T) {
	requireTmux(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "foo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), false)
	_ = srv.sessions.Stop(loginSession) // ensure no leftover login session → idle banner

	rec := httptest.NewRecorder()
	srv.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "Not logged in to Claude") {
		t.Error("expected the logged-out banner")
	}
	if !strings.Contains(body, `action="/login/start"`) {
		t.Error("expected the Log in form")
	}
	if !strings.Contains(body, "start-disabled") {
		t.Error("expected the Start button to be guarded")
	}
}

func TestIndex_loggedInEnablesStart(t *testing.T) {
	requireTmux(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "foo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)

	rec := httptest.NewRecorder()
	srv.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if strings.Contains(body, "Not logged in to Claude") {
		t.Error("logged-in render should not show the banner")
	}
	if !strings.Contains(body, `action="/start/foo"`) {
		t.Error("logged-in render should expose the Start form")
	}
}

func TestHandleLoginStart_capturesOAuthURL(t *testing.T) {
	requireTmux(t)
	// Stand-in for `claude auth login`: prints the authorize URL then idles.
	loginArgv := []string{"sh", "-c", "echo 'Use the url below to sign in https://claude.ai/oauth/authorize?code=xyz'; sleep 600"}
	srv := NewServer(t.TempDir(), filepath.Join(t.TempDir(), ".claude.json"),
		NewSessions("tmux", []string{"sleep", "600"}), NewStore(filepath.Join(t.TempDir(), "s.json")),
		NewAuth(fakeAuthArgv(false)), loginArgv, t.TempDir())
	t.Cleanup(func() { _ = srv.sessions.Stop(loginSession) })

	rec := httptest.NewRecorder()
	srv.handleLoginStart(rec, httptest.NewRequest(http.MethodPost, "/login/start", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; want 303", rec.Code)
	}
	if got := srv.getLoginURL(); got != "https://claude.ai/oauth/authorize?code=xyz" {
		t.Errorf("captured login URL = %q; want the authorize link", got)
	}
	if ok, _ := srv.sessions.IsRunning(loginSession); !ok {
		t.Error("expected lab-login session running after /login/start")
	}
}

func TestHandleLoginCode_timeoutTearsDown(t *testing.T) {
	requireTmux(t)
	// Session that consumes the pasted line then idles; auth never flips to
	// logged-in, so the poll must time out and tear the session down.
	loginArgv := []string{"sh", "-c", "IFS= read -r x; sleep 600"}
	srv := NewServer(t.TempDir(), filepath.Join(t.TempDir(), ".claude.json"),
		NewSessions("tmux", []string{"sleep", "600"}), NewStore(filepath.Join(t.TempDir(), "s.json")),
		NewAuth(fakeAuthArgv(false)), loginArgv, t.TempDir())
	srv.loginTimeout = 300 * time.Millisecond
	srv.loginPoll = 30 * time.Millisecond
	t.Cleanup(func() { _ = srv.sessions.Stop(loginSession) })

	if err := srv.sessions.StartCommand(loginSession, t.TempDir(), loginArgv); err != nil {
		t.Fatalf("start login session: %v", err)
	}
	form := url.Values{"code": {"abc#state=1"}}
	req := httptest.NewRequest(http.MethodPost, "/login/code", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handleLoginCode(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?error=login" {
		t.Errorf("Location = %q; want /?error=login", loc)
	}
	if ok, _ := srv.sessions.IsRunning(loginSession); ok {
		t.Error("expected lab-login torn down after the login timeout")
	}
}

func TestServer_authCacheRefreshesWhenStale(t *testing.T) {
	requireSh(t)
	// Each status invocation appends a byte to counter; loggedIn output stays
	// "true". Byte count = number of times the command actually ran.
	counter := filepath.Join(t.TempDir(), "calls")
	auth := NewAuth([]string{"sh", "-c", "printf x >> '" + counter + "'; echo '{\"loggedIn\":true}'"})
	srv := NewServer(t.TempDir(), filepath.Join(t.TempDir(), ".claude.json"),
		NewSessions("tmux", []string{"sleep", "600"}), NewStore(filepath.Join(t.TempDir(), "s.json")),
		auth, []string{"sleep", "600"}, t.TempDir())
	srv.authTTL = time.Minute

	if !srv.loggedIn() {
		t.Fatal("expected logged in")
	}
	if !srv.loggedIn() {
		t.Fatal("expected logged in (cached)")
	}
	if n := countCalls(t, counter); n != 1 {
		t.Fatalf("within TTL: status calls = %d; want 1 (second read cached)", n)
	}

	// Age the cache past the TTL: the next read must re-run the check.
	srv.authMu.Lock()
	srv.authChecked = time.Now().Add(-2 * time.Minute)
	srv.authMu.Unlock()
	if !srv.loggedIn() {
		t.Fatal("expected logged in after refresh")
	}
	if n := countCalls(t, counter); n != 2 {
		t.Fatalf("after staleness: status calls = %d; want 2", n)
	}

	// forceAuthRefresh ignores the TTL entirely.
	srv.forceAuthRefresh()
	if n := countCalls(t, counter); n != 3 {
		t.Fatalf("after force-refresh: status calls = %d; want 3", n)
	}
}

// privateTmux writes a tmux wrapper bound to a throwaway server socket and
// returns its path; the server is killed on cleanup. A private socket matters on
// two counts: the instance cap counts every live session by design, so a shared
// server would let the developer's own sessions blow the count; and teardown can
// kill the whole private server without ever touching a real session.
func privateTmux(t *testing.T) string {
	requireTmux(t)
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH — skipping integration test")
	}
	socket := fmt.Sprintf("labtest-%d-%d", os.Getpid(), time.Now().UnixNano())
	wrapper := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+tmuxPath+" -L "+socket+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command(tmuxPath, "-L", socket, "kill-server").Run() })
	return wrapper
}

// labClock is the fixed wall-clock newLiveServer pins so a manual instance's
// <timestamp> label — and the minute-bump on a same-minute collision — is
// deterministic under test (15:30 → 15:31 → … for same-minute siblings).
var labClock = time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)

// newLiveServer builds a logged-in Server over a temp projects root holding the
// named project dirs, backed by a private tmux server (see privateTmux). The
// per-project command is `sh -c "sleep 600"`: it stays live but prints no
// claude.ai link, and — unlike a bare `sleep 600` — tolerates an appended
// trailing argument, so an AFK run's seed prompt doesn't break the spawn.
// bridgeTimeout is shrunk so each spawn's background deep-link capture gives up
// fast against the (empty by default) scratch registryDir instead of polling
// for the production 30s; captureTimeout likewise for the login scrape.
// Manual Start now creates a per-instance worktree
// (ADR-0017), so the server is wired with the git fake (origin present, so it's a
// usable repo), a worktree root, and a pinned clock for deterministic labels.
func newLiveServer(t *testing.T, maxInstances int, projects ...string) *Server {
	root := t.TempDir()
	for _, p := range projects {
		if err := os.MkdirAll(filepath.Join(root, p, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sessions := NewSessions(privateTmux(t), []string{"sh", "-c", "sleep 600"})
	srv := newTestServer(t, root, sessions, NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	srv.maxInstances = maxInstances
	srv.captureTimeout = 100 * time.Millisecond
	srv.bridgeTimeout = 100 * time.Millisecond
	srv.registryDir = t.TempDir()
	srv.git = &fakeGit{origin: cloonarOrigin}
	srv.worktreeRoot = t.TempDir()
	srv.now = func() time.Time { return labClock }
	return srv
}

// mustStart drives a real /start/<project> POST (optionally with a label) and
// fails unless it redirects. Note a 303 also covers the *blocked* responses
// (logged-out, over-cap), so callers that need to prove a start actually spawned
// a session must additionally assert the instance count.
func mustStart(t *testing.T, srv *Server, project, label string) {
	t.Helper()
	form := url.Values{}
	if label != "" {
		form.Set("label", label)
	}
	req := httptest.NewRequest(http.MethodPost, "/start/"+project, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handleStart(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("handleStart %s (label=%q): status %d; body %q", project, label, rec.Code, rec.Body.String())
	}
}

// instancesOf returns one project's live instances from a fresh snapshot.
func instancesOf(t *testing.T, srv *Server, project string) []instanceView {
	t.Helper()
	groups, _, err := srv.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, g := range groups {
		if g.Name == project {
			return g.Instances
		}
	}
	t.Fatalf("project %q not in snapshot", project)
	return nil
}

// Acceptance (#134): two concurrent instances of one project run in SEPARATE
// worktrees and never share a working tree / index / branch. Three manual starts
// must yield three distinct worktrees on three distinct lab/<label> branches, all
// grouped under the one project card, and the login session must never surface.
func TestHandleStart_eachInstanceGetsOwnWorktree(t *testing.T) {
	srv := newLiveServer(t, defaultMaxInstances, "grp-proj")
	gt := srv.git.(*fakeGit)

	mustStart(t, srv, "grp-proj", "")      // unlabelled → grp-proj~20260608-1530
	mustStart(t, srv, "grp-proj", "")      // same minute → bumped to ~20260608-1531
	mustStart(t, srv, "grp-proj", "debug") // labelled → grp-proj~debug-20260608-1530
	// A login session must never surface as an instance of any project.
	if err := srv.sessions.StartCommand(loginSession, t.TempDir(), []string{"sleep", "600"}); err != nil {
		t.Fatalf("start login: %v", err)
	}

	insts := instancesOf(t, srv, "grp-proj")
	if len(insts) != 3 {
		t.Fatalf("instances = %+v; want 3", insts)
	}
	// Each instance got its own worktree on its own branch — the isolation
	// guarantee. Three AddWorktree calls, three distinct paths, three distinct
	// lab/<label> branches (never an afk/ branch, never a shared one).
	if len(gt.added) != 3 {
		t.Fatalf("AddWorktree calls = %d; want 3 (one isolated worktree per instance)", len(gt.added))
	}
	paths, branches := map[string]bool{}, map[string]bool{}
	for _, a := range gt.added {
		paths[a.Path] = true
		branches[a.Branch] = true
		if !strings.HasPrefix(a.Branch, "lab/") {
			t.Errorf("manual instance branch = %q; want a lab/<label> branch", a.Branch)
		}
	}
	if len(paths) != 3 || len(branches) != 3 {
		t.Errorf("instances shared state: %d distinct paths, %d distinct branches; want 3 each", len(paths), len(branches))
	}
	// All manual (never AFK); exactly one carries the user label "debug".
	labelled := 0
	for _, in := range insts {
		if in.AFK {
			t.Errorf("manual start produced an AFK row: %+v", in)
		}
		if in.Label == "debug" {
			labelled++
		}
	}
	if labelled != 1 {
		t.Errorf("instances carrying label 'debug' = %d; want exactly 1", labelled)
	}

	groups, _, err := srv.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, g := range groups {
		if g.Name == loginSession {
			t.Errorf("login session must not appear as a project group")
		}
		for _, in := range g.Instances {
			if in.Name == loginSession {
				t.Errorf("login session leaked into %q's instances", g.Name)
			}
		}
	}
}

func TestHandleStart_capBlocksDirectPostAndExcludesLogin(t *testing.T) {
	srv := newLiveServer(t, 2, "cap-proj")

	// A live login session must not consume a slot: if it counted, the second
	// project start below would be refused and only one instance would exist.
	if err := srv.sessions.StartCommand(loginSession, t.TempDir(), []string{"sleep", "600"}); err != nil {
		t.Fatalf("start login: %v", err)
	}
	mustStart(t, srv, "cap-proj", "") // slot 1 → count 1
	mustStart(t, srv, "cap-proj", "") // slot 2 → count 2 == cap

	_, atCap, err := srv.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !atCap {
		t.Error("expected atCap once the live instance count reaches the cap")
	}
	if got := instancesOf(t, srv, "cap-proj"); len(got) != 2 {
		t.Fatalf("instances = %d; want 2 (login must not count against the cap)", len(got))
	}

	// A direct over-cap POST must be refused server-side, spawning no session.
	rec := httptest.NewRecorder()
	srv.handleStart(rec, httptest.NewRequest(http.MethodPost, "/start/cap-proj", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("over-cap start: status %d loc %q; want 303 /", rec.Code, rec.Header().Get("Location"))
	}
	if got := instancesOf(t, srv, "cap-proj"); len(got) != 2 {
		t.Errorf("after blocked start, instances = %d; want still 2", len(got))
	}
}

func TestHandleStopAll_confinedToTargetProject(t *testing.T) {
	srv := newLiveServer(t, defaultMaxInstances, "soa", "soa-extra")

	mustStart(t, srv, "soa", "")       // soa
	mustStart(t, srv, "soa", "")       // soa~2
	mustStart(t, srv, "soa-extra", "") // shares the "soa" prefix but must survive

	rec := httptest.NewRecorder()
	srv.handleStopAll(rec, httptest.NewRequest(http.MethodPost, "/stop-all/soa", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("stop-all: status %d; want 303", rec.Code)
	}

	if got := instancesOf(t, srv, "soa"); len(got) != 0 {
		t.Errorf("soa after stop-all = %d instances; want 0", len(got))
	}
	if got := instancesOf(t, srv, "soa-extra"); len(got) != 1 {
		t.Errorf("soa-extra after stop-all soa = %d; want 1 (prefix sibling must not be caught)", len(got))
	}
}

func TestHandleStop_forgetsOnlyThatInstanceURL(t *testing.T) {
	srv := newLiveServer(t, defaultMaxInstances, "url-proj")

	mustStart(t, srv, "url-proj", "") // url-proj~20260608-1530
	mustStart(t, srv, "url-proj", "") // same minute → url-proj~20260608-1531
	insts := instancesOf(t, srv, "url-proj")
	if len(insts) != 2 {
		t.Fatalf("instances = %+v; want 2", insts)
	}
	keep, stop := insts[0].Name, insts[1].Name
	// sleep prints no link, so seed both deep links by hand to prove the forget
	// is targeted at exactly the stopped instance.
	if err := srv.store.SetURL(keep, "https://claude.ai/code/A"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetURL(stop, "https://claude.ai/code/B"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.handleStop(rec, httptest.NewRequest(http.MethodPost, "/stop/"+stop, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("stop: status %d; want 303", rec.Code)
	}

	if got := srv.store.URL(stop); got != "" {
		t.Errorf("stopped instance URL = %q; want forgotten", got)
	}
	if got := srv.store.URL(keep); got != "https://claude.ai/code/A" {
		t.Errorf("surviving instance URL = %q; want preserved", got)
	}
	if got := instancesOf(t, srv, "url-proj"); len(got) != 1 || got[0].Name != keep {
		t.Errorf("after stopping one, instances = %+v; want just %q", got, keep)
	}
}

// Regression for the claude 2.1.156→2.1.170 bump: claude no longer prints the
// remote-control deep link into the pane (the bridge_status transcript message
// was removed), so lab must capture it from claude's session registry instead.
// Drive a real /start, materialise the registry entry the spawned claude would
// have written (cwd = the instance's worktree, pid live), and assert the deep
// link lands in the store under the session's name — without anything ever
// printing a URL into the pane (the stand-in command is a bare sleep).
func TestHandleStart_capturesDeepLinkFromSessionRegistry(t *testing.T) {
	srv := newLiveServer(t, defaultMaxInstances, "reg-proj")
	// Generous window: this test writes the registry entry only after the
	// start returns, like a real claude connecting its bridge seconds in.
	srv.bridgeTimeout = 5 * time.Second

	name, wt, _ := startOneManual(t, srv, "reg-proj", "")
	writeRegistryEntry(t, srv.registryDir, "4242.json", registryEntry{
		PID: os.Getpid(), Cwd: wt, StartedAt: 1, BridgeSessionID: "cse_regress",
	})

	want := "https://claude.ai/code/session_regress"
	deadline := time.Now().Add(5 * time.Second)
	for srv.store.URL(name) != want {
		if time.Now().After(deadline) {
			t.Fatalf("store URL = %q; want %q (deep link never captured from registry)", srv.store.URL(name), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startOneManual starts a single manual instance and returns its session name
// plus the worktree path and lab/<label> branch the guarded teardown will target,
// derived from the name exactly as teardownManual does.
func startOneManual(t *testing.T, srv *Server, project, label string) (name, wt, branch string) {
	t.Helper()
	mustStart(t, srv, project, label)
	insts := instancesOf(t, srv, project)
	if len(insts) != 1 {
		t.Fatalf("want 1 instance after start, got %+v", insts)
	}
	name = insts[0].Name
	id := parseSessionName(name)
	return name, srv.worktreePath(id), instanceBranch(id)
}

func stopOK(t *testing.T, srv *Server, name string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleStop(rec, httptest.NewRequest(http.MethodPost, "/stop/"+name, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("stop %s: status %d body %q; want 303", name, rec.Code, rec.Body.String())
	}
}

// TestHandleStop_manualGuardedTeardown drives the guarded teardown (ADR-0017)
// through a real manual Start→Stop for each cell of the decision table:
// acceptance #4 — a clean instance's worktree is removed and its branch deleted
// only if merged; a dirty one keeps both.
func TestHandleStop_manualGuardedTeardown(t *testing.T) {
	t.Run("clean + merged removes worktree and deletes branch", func(t *testing.T) {
		srv := newLiveServer(t, defaultMaxInstances, "gt-proj")
		gt := srv.git.(*fakeGit)
		name, wt, branch := startOneManual(t, srv, "gt-proj", "")
		gt.merged = map[string]bool{branch: true} // clean (dirty defaults false), merged

		stopOK(t, srv, name)

		if len(gt.removed) != 1 || gt.removed[0].Path != wt {
			t.Errorf("removed = %+v; want the clean worktree %q removed", gt.removed, wt)
		}
		if !reflect.DeepEqual(gt.deleted, []string{branch}) {
			t.Errorf("deleted = %+v; want the merged branch %q deleted", gt.deleted, branch)
		}
		if got := instancesOf(t, srv, "gt-proj"); len(got) != 0 {
			t.Errorf("after stop, instances = %+v; want 0", got)
		}
	})

	t.Run("clean + unmerged removes worktree but keeps branch", func(t *testing.T) {
		srv := newLiveServer(t, defaultMaxInstances, "gt-proj")
		gt := srv.git.(*fakeGit)
		name, wt, _ := startOneManual(t, srv, "gt-proj", "")
		// clean, not merged: both maps empty/default.

		stopOK(t, srv, name)

		if len(gt.removed) != 1 || gt.removed[0].Path != wt {
			t.Errorf("removed = %+v; want the clean worktree removed", gt.removed)
		}
		if len(gt.deleted) != 0 {
			t.Errorf("deleted = %+v; want the unmerged branch KEPT", gt.deleted)
		}
	})

	t.Run("dirty keeps worktree and branch", func(t *testing.T) {
		srv := newLiveServer(t, defaultMaxInstances, "gt-proj")
		gt := srv.git.(*fakeGit)
		name, wt, branch := startOneManual(t, srv, "gt-proj", "")
		gt.dirty = map[string]bool{wt: true}
		gt.merged = map[string]bool{branch: true} // merged, but dirty must win

		stopOK(t, srv, name)

		if len(gt.removed) != 0 {
			t.Errorf("removed = %+v; want the dirty worktree KEPT", gt.removed)
		}
		if len(gt.deleted) != 0 {
			t.Errorf("deleted = %+v; want the dirty instance's branch KEPT", gt.deleted)
		}
	})

	t.Run("unreadable status keeps everything (conservative)", func(t *testing.T) {
		srv := newLiveServer(t, defaultMaxInstances, "gt-proj")
		gt := srv.git.(*fakeGit)
		name, _, _ := startOneManual(t, srv, "gt-proj", "")
		gt.dirtyErr = errors.New("status boom")

		stopOK(t, srv, name)

		if len(gt.removed) != 0 || len(gt.deleted) != 0 {
			t.Errorf("on a status error, removed=%+v deleted=%+v; want everything kept", gt.removed, gt.deleted)
		}
	})
}

func TestDecideTeardown(t *testing.T) {
	for _, tc := range []struct {
		dirty, merged bool
		want          teardownAction
	}{
		{dirty: true, merged: false, want: teardownAction{}},                                         // dirty → keep both
		{dirty: true, merged: true, want: teardownAction{}},                                          // dirty wins even if merged
		{dirty: false, merged: false, want: teardownAction{RemoveWorktree: true}},                    // clean+unmerged → drop worktree, keep branch
		{dirty: false, merged: true, want: teardownAction{RemoveWorktree: true, DeleteBranch: true}}, // clean+merged → drop both
	} {
		if got := decideTeardown(tc.dirty, tc.merged); got != tc.want {
			t.Errorf("decideTeardown(dirty=%v, merged=%v) = %+v; want %+v", tc.dirty, tc.merged, got, tc.want)
		}
	}
}

// A spawn failure AFTER the worktree is created rolls the whole claim back —
// worktree removed AND branch deleted — so a failed Start leaves nothing behind
// (ADR-0017).
func TestHandleStart_rollsBackWorktreeOnSpawnFailure(t *testing.T) {
	requireSh(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "rb-proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	failTmux := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(failTmux, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newTestServer(t, root, NewSessions(failTmux, []string{"sh", "-c", "sleep 600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	srv.git = gt
	srv.worktreeRoot = t.TempDir()
	srv.now = func() time.Time { return labClock }

	rec := httptest.NewRecorder()
	srv.handleStart(rec, httptest.NewRequest(http.MethodPost, "/start/rb-proj", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?error=action" {
		t.Fatalf("spawn-fail: status %d loc %q; want 303 /?error=action", rec.Code, rec.Header().Get("Location"))
	}
	if len(gt.added) != 1 {
		t.Fatalf("added = %+v; want one worktree creation", gt.added)
	}
	branch := gt.added[0].Branch
	if len(gt.removed) != 1 || gt.removed[0].Path != gt.added[0].Path {
		t.Errorf("removed = %+v; want the partial worktree %q rolled back", gt.removed, gt.added[0].Path)
	}
	if !reflect.DeepEqual(gt.deleted, []string{branch}) {
		t.Errorf("deleted = %+v; want the branch %q rolled back", gt.deleted, branch)
	}
	if got := instancesOf(t, srv, "rb-proj"); len(got) != 0 {
		t.Errorf("after failed start, instances = %+v; want 0", got)
	}
}

// A repo with no usable origin (or a failing fetch) fails AddWorktree: Start
// shows the git cause and leaves nothing behind, with no fallback base
// (acceptance #5 / ADR-0017). A failed add created nothing, so there is nothing
// to roll back.
func TestHandleStart_failsLoudWhenWorktreeCreationFails(t *testing.T) {
	requireSh(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "no-origin", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	gt := &fakeGit{origin: cloonarOrigin, addErr: errors.New("fatal: 'origin' does not appear to be a git repository")}
	srv := newTestServer(t, root, NewSessions(privateTmux(t), []string{"sh", "-c", "sleep 600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	srv.git = gt
	srv.worktreeRoot = t.TempDir()
	srv.now = func() time.Time { return labClock }

	req := httptest.NewRequest(http.MethodPost, "/start/no-origin", nil)
	req.Header.Set(fragmentHeader, "1") // AJAX so the real git cause reaches the body
	rec := httptest.NewRecorder()
	srv.handleStart(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("worktree-fail status = %d; want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "does not appear to be a git repository") {
		t.Errorf("error body = %q; want the git cause surfaced", rec.Body.String())
	}
	if len(gt.removed) != 0 || len(gt.deleted) != 0 {
		t.Errorf("failed add rolled something back: removed=%+v deleted=%+v; want none", gt.removed, gt.deleted)
	}
	if got := instancesOf(t, srv, "no-origin"); len(got) != 0 {
		t.Errorf("after failed start, instances = %+v; want 0", got)
	}
}

func countCalls(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read counter %s: %v", path, err)
	}
	return len(b)
}

func mustStamp(t *testing.T, s *Store, name string, at time.Time) {
	t.Helper()
	if err := s.StampOpened(name, at); err != nil {
		t.Fatalf("StampOpened %q: %v", name, err)
	}
}

// renderLive executes just the "live" partial against a crafted pageData, so
// the three instance render states can be asserted without driving real tmux.
func renderLive(t *testing.T, srv *Server, data pageData) string {
	t.Helper()
	var b strings.Builder
	if err := srv.tmpl.ExecuteTemplate(&b, "live", data); err != nil {
		t.Fatalf("render live: %v", err)
	}
	return b.String()
}

func TestLivePartial_instanceStates(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	out := renderLive(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6, LiveCount: 3,
		Groups: []projectGroup{{
			Name: "proj", Path: "/p/proj",
			Instances: []instanceView{
				{Name: "proj~20260608-1530", Time: "15:30", URL: "https://claude.ai/code/REAL"}, // captured
				{Name: "proj~20260608-1531", Time: "15:31", Connecting: true},                   // scrape in flight
				{Name: "proj~dbg-20260608-1532", Label: "dbg", Time: "15:32"},                   // gave up → generic
			},
		}},
	})

	if !strings.Contains(out, "https://claude.ai/code/REAL") {
		t.Error("captured instance should render its real deep link")
	}
	if !strings.Contains(out, "connecting…") {
		t.Error("in-flight instance should render the connecting state")
	}
	if !strings.Contains(out, `href="https://claude.ai/code"`) {
		t.Error("gave-up instance should fall back to the generic claude.ai link")
	}
	if !strings.Contains(out, "exact deep link wasn't captured") {
		t.Error("generic fallback should carry the 'session picker' hint")
	}
	// Manual identity chips: an unlabelled instance renders the bare time, a
	// labelled one "<label> · 15:32".
	if !strings.Contains(out, ">15:30<") {
		t.Error("unlabelled instance should render its bare clock time")
	}
	if !strings.Contains(out, "dbg · 15:32") {
		t.Error("labelled instance should render '<label> · 15:32'")
	}
	if strings.Contains(out, "<!doctype html>") {
		t.Error("the live partial must not include full-page chrome")
	}
}

// TestLivePartial_keyContract pins the server-rendered key attributes the
// client-side morph matches nodes on: data-instance (instance rows), data-name
// (project cards), and data-key (the four top-level live blocks). The in-place
// update reuses and patches nodes by these keys instead of rebuilding #live; a
// template edit that silently dropped one would degrade live updates back to a
// full-rebuild swap that wipes typed input. lab has no JS test harness
// (ADR-0004), so this render-level assertion is the guard for that contract.
func TestLivePartial_keyContract(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), false)

	// Logged out (so the banner block renders) with one project carrying one
	// instance (so the card and instance-row keys render).
	out := renderLive(t, srv, pageData{
		LoggedIn: false, MaxInstances: 6, LiveCount: 1,
		Groups: []projectGroup{{
			Name: "proj", Path: "/p/proj",
			Instances: []instanceView{{Name: "proj~dbg-20260608-1530", Label: "dbg", Time: "15:30", URL: "https://claude.ai/code/X"}},
		}},
	})
	for _, want := range []string{
		`data-key="banner"`,                      // login banner — keyed so it can appear/disappear without misaligning the rest
		`data-key="status"`,                      // status line
		`data-key="hint"`,                        // hint paragraph
		`data-key="projects"`,                    // projects block
		`data-name="proj"`,                       // project card key
		`data-instance="proj~dbg-20260608-1530"`, // instance row key — the unique tmux session name
	} {
		if !strings.Contains(out, want) {
			t.Errorf("live fragment missing morph key %s — in-place updates depend on it", want)
		}
	}

	// The empty state swaps the projects <div> for a <p>; the morph matches the
	// two across that tag change by key, so the empty block must carry the same
	// data-key or a "no projects" render would rebuild instead of morph.
	empty := renderLive(t, srv, pageData{LoggedIn: true, MaxInstances: 6})
	if !strings.Contains(empty, `data-key="projects"`) {
		t.Error(`empty state must keep data-key="projects" so the morph swaps projects↔empty by key`)
	}
}

// TestLivePartial_repositoryLink pins the ⋯ menu's repo link: a Forgejo card
// renders one "Repository ↗" anchor opening its RepoURL in a new tab, under a
// .menu-sep divider; a non-Forgejo card renders none, keeping only its disabled
// "needs a git.cloonar.com repo" line. lab has no JS test harness (ADR-0004), so
// this render-level assertion is the guard for the menu's repo-link contract.
func TestLivePartial_repositoryLink(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	out := renderLive(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6,
		Groups: []projectGroup{
			{Name: "fj", Path: "/p/fj", Forgejo: true, RepoURL: "https://git.cloonar.com/Cloonar/fj"},
			{Name: "local", Path: "/p/local", Forgejo: false},
		},
	})

	// Forgejo card: exactly one repo link, to the repo home, in a new tab.
	if n := strings.Count(out, "Repository ↗"); n != 1 {
		t.Errorf("Repository row count = %d; want exactly 1 (Forgejo card only)", n)
	}
	if !strings.Contains(out, `<a class="menu-item" href="https://git.cloonar.com/Cloonar/fj" target="_blank" rel="noopener">Repository ↗</a>`) {
		t.Error("Forgejo menu should link to the repo home in a new tab")
	}
	if !strings.Contains(out, `<div class="menu-sep"`) {
		t.Error("the repo link should sit under a .menu-sep divider")
	}
	// Non-Forgejo card: unchanged — disabled line, no repo link (count==1 above
	// already proves the link isn't on this card too).
	if !strings.Contains(out, "needs a git.cloonar.com repo") {
		t.Error("non-Forgejo card should keep its disabled line")
	}
}

func TestHandleFragment_rendersPartialNotFullPage(t *testing.T) {
	requireTmux(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "foo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)

	rec := httptest.NewRecorder()
	srv.handleFragment(rec, httptest.NewRequest(http.MethodGet, "/fragment", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Error("/fragment must return only the #live partial, not the full page")
	}
	if !strings.Contains(body, "statusline") || !strings.Contains(body, `data-name="foo"`) {
		t.Error("/fragment should carry the status line and project list")
	}
}

func TestHandleStart_ajaxReturnsFragmentNotRedirect(t *testing.T) {
	srv := newLiveServer(t, defaultMaxInstances, "neg")

	form := url.Values{}
	req := httptest.NewRequest(http.MethodPost, "/start/neg", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(fragmentHeader, "1")
	rec := httptest.NewRecorder()
	srv.handleStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ajax start status = %d; want 200 (fragment, not a 303 redirect)", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<!doctype html>") {
		t.Error("ajax action should return the fragment, not the full page")
	}
	if !strings.Contains(body, `data-name="neg"`) {
		t.Error("returned fragment should reflect the started project")
	}
}

func TestHandleStart_ajaxErrorCarriesMessage_noJSFlashes(t *testing.T) {
	requireSh(t) // loggedIn() runs the fake auth-status command before the project lookup
	newSrv := func() *Server {
		return newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
			NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	}

	// AJAX: real error message in the body at the right status, for the banner.
	ajax := httptest.NewRequest(http.MethodPost, "/start/ghost", nil)
	ajax.Header.Set(fragmentHeader, "1")
	rec := httptest.NewRecorder()
	newSrv().handleStart(rec, ajax)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ajax unknown-project status = %d; want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown project") {
		t.Errorf("ajax error body = %q; want the real message", rec.Body.String())
	}

	// No-JS: bounced back to the index with the generic action flash.
	rec = httptest.NewRecorder()
	newSrv().handleStart(rec, httptest.NewRequest(http.MethodPost, "/start/ghost", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?error=action" {
		t.Fatalf("no-JS error: status %d loc %q; want 303 /?error=action", rec.Code, rec.Header().Get("Location"))
	}
}
