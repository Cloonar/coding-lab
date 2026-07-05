package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- pure helpers -----------------------------------------------------------

func TestHumanizeAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Minute, "just now"}, // future commit / clock skew
		{30 * time.Second, "just now"},
		{1 * time.Minute, "1m"},
		{59 * time.Minute, "59m"},
		{1 * time.Hour, "1h"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{6 * 24 * time.Hour, "6d"},
		{7 * 24 * time.Hour, "1w"},
		{30 * 24 * time.Hour, "4w"},
	} {
		if got := humanizeAge(tc.d); got != tc.want {
			t.Errorf("humanizeAge(%v) = %q; want %q", tc.d, got, tc.want)
		}
	}
}

// pullState matches a branch against PR heads exactly as the reaper does: an open
// or merged PR wins over a closed one sharing the head, and a branch with no PR
// (every never-pushed lab/ branch) yields "" so no badge renders.
func TestPullState(t *testing.T) {
	pulls := []PullRequest{
		{Head: "afk/7", State: pullOpen},
		{Head: "afk/12", State: pullMerged},
		{Head: "lab/old-20260608-1530", State: pullClosed},
		{Head: "afk/30", State: pullClosed},
		{Head: "afk/30", State: pullOpen}, // open wins over the closed one sharing the head
	}
	for _, tc := range []struct {
		branch, want string
	}{
		{"afk/7", pullOpen},
		{"afk/12", pullMerged},
		{"lab/old-20260608-1530", pullClosed},
		{"afk/30", pullOpen},
		{"lab/never-pushed-20260608-1530", ""}, // no matching head → no badge
	} {
		if got := pullState(pulls, tc.branch); got != tc.want {
			t.Errorf("pullState(%q) = %q; want %q", tc.branch, got, tc.want)
		}
	}
}

// --- enumeration (gatherParked) ---------------------------------------------

// gatherParked enumerates managed (lab//afk/) branches no live session owns, each
// paired with its worktree if one exists: it covers lab/ and afk/ entries, a bare
// branch vs a branch-with-worktree, and excludes a branch a live instance occupies.
func TestGatherParked(t *testing.T) {
	srv := newAFKServer(t, &fakeTracker{}, &fakeGit{origin: cloonarOrigin})
	gt := srv.git.(*fakeGit)
	dir, err := srv.projectDir("proj")
	if err != nil {
		t.Fatal(err)
	}

	const labFoo = "lab/foo-20260608-1530"
	// afk/7 parked (claim branch, no worktree — a finished run's bare branch);
	// afk/9 owned by a LIVE run below; lab/foo parked WITH a worktree.
	gt.branches = map[int]bool{7: true, 9: true}
	gt.labBranches = []string{labFoo}
	gt.worktrees = []Worktree{
		{Path: "/wt/proj-foo", Branch: labFoo},
		{Path: srv.worktreePath(afkID("proj", 9)), Branch: "afk/9"},
		{Path: dir, Branch: "main"}, // the reference checkout — not managed, never parked
	}

	// A live AFK run owns afk/9, so it must be excluded from the parked set.
	startAFKSession(t, srv, "proj", 9)
	live, err := srv.sessions.List()
	if err != nil {
		t.Fatal(err)
	}

	got, err := srv.gatherParked(dir, "proj", live)
	if err != nil {
		t.Fatalf("gatherParked: %v", err)
	}
	want := []parkedRef{
		{Branch: "afk/7", Worktree: ""},            // bare afk claim branch
		{Branch: labFoo, Worktree: "/wt/proj-foo"}, // manual lab branch with a worktree
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gatherParked = %+v; want %+v (afk/9 owned by a live run excluded; main never managed)", got, want)
	}
}

// A git read error fails enumeration loud rather than returning a partial/empty set
// silently — the caller turns this into the strip's fail-loud error.
func TestGatherParked_propagatesGitError(t *testing.T) {
	srv := newAFKServer(t, &fakeTracker{}, &fakeGit{origin: cloonarOrigin, branchesErr: errors.New("for-each-ref boom")})
	dir, _ := srv.projectDir("proj")
	if _, err := srv.gatherParked(dir, "proj", nil); err == nil {
		t.Fatal("gatherParked with a failing Branches() returned nil error; want the git error propagated")
	}
}

// --- snapshot count ---------------------------------------------------------

// The collapsed Parked count rides the snapshot/poll path but adds NO tea/network
// call: it is a local-only git enumeration, so a poll never lists pulls for the
// count. (The PR badges are the lazy endpoint's job.)
func TestSnapshot_parkedCountIsLocalOnly(t *testing.T) {
	srv := newAFKServer(t, &fakeTracker{}, &fakeGit{origin: cloonarOrigin})
	gt := srv.git.(*fakeGit)
	tr := srv.tracker.(*fakeTracker)
	gt.branches = map[int]bool{7: true}
	gt.labBranches = []string{"lab/foo-20260608-1530", "lab/bar-20260608-1531"}

	groups, _, err := srv.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d; want 1", len(groups))
	}
	if groups[0].ParkedCount != 3 {
		t.Errorf("ParkedCount = %d; want 3 (afk/7 + two lab/ branches)", groups[0].ParkedCount)
	}
	if tr.pullsCalls != 0 {
		t.Errorf("snapshot listed pulls %d times; want 0 — the count must add no tea call to the poll", tr.pullsCalls)
	}
}

// --- lazy endpoint (handleParked) -------------------------------------------

// renderParkedBody executes the parkedBody fragment for a crafted view, so the
// per-entry rendering can be asserted without driving git/tea/tmux.
func renderParkedBody(t *testing.T, srv *Server, view parkedView) string {
	t.Helper()
	var b strings.Builder
	if err := srv.tmpl.ExecuteTemplate(&b, "parkedBody", view); err != nil {
		t.Fatalf("render parkedBody: %v", err)
	}
	return b.String()
}

func TestParkedBody_render(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)

	out := renderParkedBody(t, srv, parkedView{
		Project: "proj",
		Items: []parkedItem{
			// A bare afk claim branch with an open PR, no worktree (so no dirty/clean,
			// no path), 3 ahead, all pushed (no unpushed warning).
			{Branch: "afk/7", AFK: true, Issue: 7, Ahead: 3, Age: "3h", PRState: pullOpen},
			// A manual lab branch with a dirty worktree, 1 ahead, 1 unpushed, copyable path.
			{Branch: "lab/foo-20260608-1530", HasWorktree: true, Worktree: "/wt/proj-foo", Dirty: true, Ahead: 1, Unpushed: 1, Age: "2d"},
		},
	})

	for _, want := range []string{
		"AFK #7", "lab/foo-20260608-1530", // kinds
		"● dirty",  // dirty worktree state (lab/foo)
		"↑3", "↑1", // commits ahead
		"3h", "2d", // ages
		"PR open",                                  // best-effort PR badge
		"⚠ 1 unpushed",                             // unpushed warning
		`data-copy="/wt/proj-foo"`, "/wt/proj-foo", // copyable worktree path
		`action="/parked/discard/proj"`, // discard form posts the project
		`name="branch" value="afk/7"`,   // branch carried as a form field (contains /)
		`name="branch" value="lab/foo-20260608-1530"`,
		"data-parked-discard", `data-confirm="1"`, // two-step confirm
	} {
		if !strings.Contains(out, want) {
			t.Errorf("parkedBody missing %q\n--- render ---\n%s", want, out)
		}
	}
	// The bare afk entry has no worktree → no clean/dirty chip and no path for it.
	if strings.Contains(out, `data-copy=""`) {
		t.Error("a worktree-less entry must not render an empty copyable path")
	}
	// A 0-unpushed entry shows no warning: only the lab/foo entry (1 unpushed)
	// renders the ⚠ glyph; afk/7 (0 unpushed) does not.
	if n := strings.Count(out, "⚠"); n != 1 {
		t.Errorf("unpushed warnings = %d; want exactly 1 (only the lab/foo entry)", n)
	}
}

func TestParkedBody_failLoudAndEmpty(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)

	// Fail loud: an Err renders the error, never a silent empty list.
	errOut := renderParkedBody(t, srv, parkedView{Project: "proj", Err: "for-each-ref boom"})
	if !strings.Contains(errOut, "for-each-ref boom") || !strings.Contains(errOut, "parked-error") {
		t.Errorf("Err view should fail loud with the message; got %q", errOut)
	}
	// No items, no error: a genuine "nothing parked".
	emptyOut := renderParkedBody(t, srv, parkedView{Project: "proj"})
	if !strings.Contains(emptyOut, "Nothing parked") {
		t.Errorf("empty view should read 'Nothing parked'; got %q", emptyOut)
	}
}

// The lazy endpoint enriches each entry (dirty, ahead, unpushed, age, PR badge) and
// is where the one tea ListPulls happens — Forgejo only.
func TestHandleParked_enrichesAndListsPulls(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{pulls: []PullRequest{{Head: "afk/7", State: pullOpen}}}
	srv := newAFKServer(t, tr, gt)
	srv.now = func() time.Time { return labClock }

	const labFoo = "lab/foo-20260608-1530"
	gt.branches = map[int]bool{7: true}
	gt.labBranches = []string{labFoo}
	gt.worktrees = []Worktree{{Path: "/wt/proj-foo", Branch: labFoo}}
	gt.dirty = map[string]bool{"/wt/proj-foo": true}
	gt.ahead = map[string]int{"afk/7": 5, labFoo: 2}
	gt.unpushed = map[string]int{"afk/7": 0, labFoo: 2}
	gt.lastCommit = map[string]time.Time{
		"afk/7": labClock.Add(-90 * time.Minute), // 1h
		labFoo:  labClock.Add(-50 * time.Hour),   // 2d
	}

	rec := httptest.NewRecorder()
	srv.handleParked(rec, httptest.NewRequest(http.MethodGet, "/parked/proj", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("handleParked status = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"AFK #7", labFoo, "● dirty", "↑5", "↑2", "1h", "2d", "PR open", "⚠ 2 unpushed", "/wt/proj-foo"} {
		if !strings.Contains(body, want) {
			t.Errorf("lazy endpoint missing %q\n%s", want, body)
		}
	}
	if tr.pullsCalls != 1 {
		t.Errorf("lazy endpoint listed pulls %d times; want exactly 1", tr.pullsCalls)
	}
}

// A failing enumeration shows the error in the strip, not an empty list (fail loud).
func TestHandleParked_failsLoud(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin, branchesErr: errors.New("for-each-ref boom")}
	srv := newAFKServer(t, &fakeTracker{}, gt)

	rec := httptest.NewRecorder()
	srv.handleParked(rec, httptest.NewRequest(http.MethodGet, "/parked/proj", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Couldn't load parked work") || !strings.Contains(body, "for-each-ref boom") {
		t.Errorf("a failing endpoint must show the error in the strip; got %q", body)
	}
	if strings.Contains(body, "Nothing parked") {
		t.Error("a failing endpoint must not render the empty state")
	}
}

func TestHandleParked_unknownProject404(t *testing.T) {
	srv := newAFKServer(t, &fakeTracker{}, &fakeGit{origin: cloonarOrigin})
	rec := httptest.NewRecorder()
	srv.handleParked(rec, httptest.NewRequest(http.MethodGet, "/parked/ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown project status = %d; want 404", rec.Code)
	}
}

// --- discard (handleDiscard) ------------------------------------------------

func discardReq(project, branch string) *http.Request {
	form := url.Values{"branch": {branch}}
	req := httptest.NewRequest(http.MethodPost, "/parked/discard/"+project, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// Discard is UNGUARDED: it force-removes the worktree and force-deletes the branch
// regardless of dirty/merged state — the guarded teardown would keep a dirty
// worktree, so this proves Discard bypasses it entirely.
func TestHandleDiscard_unguarded(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, &fakeTracker{}, gt)

	const labFoo = "lab/foo-20260608-1530"
	const wt = "/wt/proj-foo"
	gt.labBranches = []string{labFoo}
	gt.worktrees = []Worktree{{Path: wt, Branch: labFoo}}
	gt.dirty = map[string]bool{wt: true} // DIRTY — a guarded teardown would keep it
	// merged left empty → UNMERGED — a guarded teardown would keep the branch too.

	rec := httptest.NewRecorder()
	srv.handleDiscard(rec, discardReq("proj", labFoo))
	if rec.Code != http.StatusOK {
		t.Fatalf("discard status = %d body %q; want 200", rec.Code, rec.Body.String())
	}
	if len(gt.removed) != 1 || gt.removed[0].Path != wt {
		t.Errorf("removed = %+v; want the dirty worktree %q force-removed anyway", gt.removed, wt)
	}
	if !reflect.DeepEqual(gt.deleted, []string{labFoo}) {
		t.Errorf("deleted = %+v; want the unmerged branch %q force-deleted anyway", gt.deleted, labFoo)
	}
}

// Discarding a bare branch (no worktree) just deletes the branch — nothing to remove.
func TestHandleDiscard_bareBranchDeletesOnly(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, &fakeTracker{}, gt)
	gt.labBranches = []string{"lab/bare-20260608-1530"}

	rec := httptest.NewRecorder()
	srv.handleDiscard(rec, discardReq("proj", "lab/bare-20260608-1530"))
	if rec.Code != http.StatusOK {
		t.Fatalf("discard status = %d; want 200", rec.Code)
	}
	if len(gt.removed) != 0 {
		t.Errorf("removed = %+v; want none (the branch had no worktree)", gt.removed)
	}
	if !reflect.DeepEqual(gt.deleted, []string{"lab/bare-20260608-1530"}) {
		t.Errorf("deleted = %+v; want the bare branch deleted", gt.deleted)
	}
}

// Discarding an afk/<N> entry removes its claim branch, so the issue re-enters the
// claimable set (ADR-0013): AFKBranches no longer reports it.
func TestHandleDiscard_afkBranchBecomesClaimable(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, &fakeTracker{}, gt)
	gt.branches = map[int]bool{7: true} // afk/7 claimed
	dir, _ := srv.projectDir("proj")

	if claimed, _ := gt.AFKBranches(dir); !claimed[7] {
		t.Fatal("precondition: afk/7 should be claimed before discard")
	}
	rec := httptest.NewRecorder()
	srv.handleDiscard(rec, discardReq("proj", "afk/7"))
	if rec.Code != http.StatusOK {
		t.Fatalf("discard status = %d; want 200", rec.Code)
	}
	if !reflect.DeepEqual(gt.deleted, []string{"afk/7"}) {
		t.Errorf("deleted = %+v; want afk/7 deleted", gt.deleted)
	}
	if claimed, _ := gt.AFKBranches(dir); claimed[7] {
		t.Error("after discarding afk/7 the issue is still claimed; want it claimable again (ADR-0013)")
	}
}

// A crafted POST for a non-managed branch (main, a human's feature branch) is
// refused: Discard must never force-delete a branch lab did not create.
func TestHandleDiscard_rejectsNonManagedBranch(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, &fakeTracker{}, gt)

	for _, branch := range []string{"main", "feature/x", ""} {
		rec := httptest.NewRecorder()
		srv.handleDiscard(rec, discardReq("proj", branch))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("discard %q status = %d; want 400 (not a parked branch)", branch, rec.Code)
		}
		if len(gt.removed) != 0 || len(gt.deleted) != 0 {
			t.Fatalf("discard %q touched git: removed=%+v deleted=%+v; want nothing", branch, gt.removed, gt.deleted)
		}
	}
}

// The two parked routes dispatch correctly through the mux: GET /parked/<project>
// reaches the read-only lazy endpoint, POST /parked/discard/<project> reaches
// Discard. This pins the ServeMux longest-prefix behaviour — /parked/discard/ must
// not be swallowed by /parked/ — which the direct-handler tests above don't cover.
func TestParkedRoutes_dispatch(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, &fakeTracker{}, gt)
	const labFoo = "lab/foo-20260608-1530"
	gt.labBranches = []string{labFoo}
	gt.worktrees = []Worktree{{Path: "/wt/proj-foo", Branch: labFoo}}
	h := srv.Routes()

	// GET /parked/proj → lazy listing (reads, deletes nothing).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/parked/proj", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), labFoo) {
		t.Fatalf("GET /parked/proj: status %d body %q; want 200 listing the entry", rec.Code, rec.Body.String())
	}
	if len(gt.deleted) != 0 {
		t.Errorf("GET /parked/proj deleted %v; want a read-only listing", gt.deleted)
	}

	// POST /parked/discard/proj → Discard (deletes the branch), NOT the lazy endpoint.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, discardReq("proj", labFoo))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /parked/discard/proj: status %d; want 200", rec.Code)
	}
	if !reflect.DeepEqual(gt.deleted, []string{labFoo}) {
		t.Errorf("POST /parked/discard/proj deleted = %v; want the branch discarded (reached handleDiscard, not handleParked)", gt.deleted)
	}
}

// --- template: the card strip ----------------------------------------------

// The card renders a collapsed Parked strip only when ParkedCount > 0, and stamps
// the morph contract the lazy body relies on: data-key (so a poll patches the strip
// in place) and data-static on the body (so a poll never wipes the fetched content).
func TestLivePartial_parkedStrip(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)

	out := renderLive(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6,
		Groups: []projectGroup{
			{Name: "haswork", Path: "/p/haswork", ParkedCount: 2},
			{Name: "clean", Path: "/p/clean", ParkedCount: 0},
		},
	})

	for _, want := range []string{
		`data-parked="haswork"`,   // the strip targets its project's lazy endpoint
		`data-key="parked"`,       // keyed so the morph patches it in place
		`data-static`,             // the body is a client-owned subtree (survives polls)
		`class="parked-count">2<`, // server-rendered, live count
	} {
		if !strings.Contains(out, want) {
			t.Errorf("parked strip missing %q\n%s", want, out)
		}
	}
	// A project with no parked work renders no strip at all.
	if strings.Contains(out, `data-parked="clean"`) {
		t.Error("a project with ParkedCount 0 must render no Parked strip")
	}
}
