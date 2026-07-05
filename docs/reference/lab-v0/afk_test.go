package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fakes for the tea / git seams -----------------------------------------

// fakeTracker serves the ready queue and PR list without a live tea. It is
// read-only: lab claims via an afk/<N> branch (ADR-0013), not a tracker label,
// so there is nothing to record on the tracker side of a claim.
type fakeTracker struct {
	ready    []Issue
	readyErr error

	pulls      []PullRequest // returned by ListPulls (the reaper's PR-match input)
	pullsErr   error
	pullsCalls int // how many times the watcher listed pulls — proves (non-)evaluation
}

func (f *fakeTracker) ReadyIssues(string) ([]Issue, error) { return f.ready, f.readyErr }
func (f *fakeTracker) ListPulls(string) ([]PullRequest, error) {
	f.pullsCalls++
	return f.pulls, f.pullsErr
}

type worktreeCall struct{ RepoDir, Path, Branch string }

// fakeGit records worktree lifecycle calls and reports a fixed origin URL. It
// also models the afk/<N> branch set that is lab's claim record (ADR-0013):
// AddWorktree marks a branch present and DeleteBranch clears it, so AFKBranches
// reflects the claims a run actually created — and a test can pre-seed `branches`
// to stand up a parked / already-claimed issue.
//
// mu makes it safe under concurrent calls, mirroring realGit: there each method
// is an independent git subprocess with no shared Go state, so the scheduler's
// unlocked AFKBranches hint can run alongside a launchAFKRun AddWorktree. The
// fake shares one map, so it locks — and AFKBranches hands back a copy, so a
// caller ranging the result can't race a concurrent claim mutating the live map.
type fakeGit struct {
	mu          sync.Mutex
	origin      string
	originErr   error
	branches    map[int]bool // issues with a local afk/<N> branch (the claim set)
	branchesErr error
	added       []worktreeCall
	removed     []worktreeCall
	deleted     []string // branches passed to DeleteBranch
	addErr      error

	// dirty maps a worktree path to whether WorktreeDirty reports it dirty; merged
	// maps a branch to whether BranchMerged reports it merged. Absent keys read as
	// the zero value (clean / unmerged). dirtyErr / mergedErr force the
	// conservative "keep everything" guarded-teardown path.
	dirty     map[string]bool
	merged    map[string]bool
	dirtyErr  error
	mergedErr error

	// worktrees is what Worktrees returns — the reconciliation/sweep view of the
	// reference repo's `git worktree list`. labBranches adds non-afk branch names so
	// Branches surfaces lab/ branches alongside the afk/<N> ones it synthesises from
	// `branches`. fetched records the repoDirs Fetch was called with (the sweep's
	// best-effort refresh); fetchErr forces a fetch failure.
	worktrees    []Worktree
	worktreesErr error
	labBranches  []string
	fetched      []string
	fetchErr     error

	// ahead / unpushed / lastCommit back the Parked view's per-entry stats
	// (CommitsAhead, UnpushedCount, LastCommitTime), keyed by branch; absent keys
	// read as the zero value (0 / zero time). The *Err fields force the matching
	// call to fail, so a test can prove a per-entry stat error degrades just that
	// field rather than blanking the whole strip.
	ahead       map[string]int
	aheadErr    error
	unpushed    map[string]int
	unpushedErr error
	lastCommit  map[string]time.Time
	lastErr     error
}

func (f *fakeGit) OriginURL(string) (string, error) { return f.origin, f.originErr }
func (f *fakeGit) AFKBranches(string) (map[int]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branchesErr != nil {
		return nil, f.branchesErr
	}
	out := make(map[int]bool, len(f.branches))
	for n := range f.branches {
		out[n] = true
	}
	return out, nil
}
func (f *fakeGit) AddWorktree(repoDir, path, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, worktreeCall{repoDir, path, branch})
	if f.addErr != nil {
		return f.addErr // a failed add creates no branch
	}
	if n, ok := parseAFKBranch(branch); ok {
		if f.branches == nil {
			f.branches = map[int]bool{}
		}
		f.branches[n] = true
	}
	return nil
}
func (f *fakeGit) RemoveWorktree(repoDir, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, worktreeCall{repoDir, path, ""})
	return nil
}
func (f *fakeGit) DeleteBranch(repoDir, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, branch)
	if n, ok := parseAFKBranch(branch); ok {
		delete(f.branches, n)
	}
	return nil
}
func (f *fakeGit) WorktreeDirty(path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirtyErr != nil {
		return false, f.dirtyErr
	}
	return f.dirty[path], nil
}
func (f *fakeGit) BranchMerged(repoDir, branch string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mergedErr != nil {
		return false, f.mergedErr
	}
	return f.merged[branch], nil
}
func (f *fakeGit) Fetch(repoDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, repoDir)
	return f.fetchErr
}
func (f *fakeGit) Worktrees(string) ([]Worktree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.worktreesErr != nil {
		return nil, f.worktreesErr
	}
	return append([]Worktree(nil), f.worktrees...), nil
}
func (f *fakeGit) CommitsAhead(_ string, branch string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aheadErr != nil {
		return 0, f.aheadErr
	}
	return f.ahead[branch], nil
}
func (f *fakeGit) UnpushedCount(_ string, branch string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unpushedErr != nil {
		return 0, f.unpushedErr
	}
	return f.unpushed[branch], nil
}
func (f *fakeGit) LastCommitTime(_ string, branch string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastErr != nil {
		return time.Time{}, f.lastErr
	}
	return f.lastCommit[branch], nil
}

// Branches synthesises the afk/<N> names from the claim set (so it stays in step
// with AddWorktree / DeleteBranch, exactly like AFKBranches) and appends any
// labBranches, then filters to the requested prefixes — the same view realGit's
// for-each-ref gives the reconciliation/sweep. Sorted for a deterministic order.
func (f *fakeGit) Branches(repoDir string, prefixes ...string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branchesErr != nil {
		return nil, f.branchesErr
	}
	var all []string
	for n := range f.branches {
		all = append(all, afkBranch(n))
	}
	all = append(all, f.labBranches...)
	var out []string
	for _, b := range all {
		for _, p := range prefixes {
			if strings.HasPrefix(b, p) {
				out = append(out, b)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// afkID is the instance identity of an AFK run for issue n of a project — the
// argument worktreePath / instanceBranch now take. A test helper so the many
// worktree-path assertions read clearly.
func afkID(project string, n int) instanceID {
	return instanceID{Project: project, Label: afkLabel(n, false)}
}

// cloonarOrigin is a Forgejo-backed origin so forgejoFor() lets the AFK path run.
const cloonarOrigin = "git@git.cloonar.com:Cloonar/proj.git"

// newAFKServer builds a logged-in Server over one Forgejo project, wired to the
// given fakes and a private tmux server whose per-project command tolerates the
// appended seed prompt.
func newAFKServer(t *testing.T, tr Tracker, gt Git) *Server {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return newAFKServerAt(t, root, filepath.Join(t.TempDir(), "s.json"), tr, gt)
}

// newAFKServerAt is newAFKServer with the projects root and store path supplied,
// so a test can stand up a SECOND server over the same persisted store (a
// restart) and prove what survives it and what does not.
func newAFKServerAt(t *testing.T, root, storePath string, tr Tracker, gt Git) *Server {
	sessions := NewSessions(privateTmux(t), []string{"sh", "-c", "sleep 600"})
	srv := newTestServer(t, root, sessions, NewStore(storePath), true)
	srv.tracker = tr
	srv.git = gt
	srv.worktreeRoot = t.TempDir()
	srv.captureTimeout = 100 * time.Millisecond
	srv.bridgeTimeout = 100 * time.Millisecond
	srv.registryDir = t.TempDir()
	return srv
}

// --- handler tests ----------------------------------------------------------

func TestHandleAFKStart_noReadyIssuesNoOp(t *testing.T) {
	requireSh(t) // forceAuthRefresh runs the fake auth-status command
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &fakeTracker{ready: nil}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	srv.tracker = tr
	srv.git = gt
	srv.worktreeRoot = t.TempDir()

	// No-JS: lands on the index with the specific notice flag, claims nothing.
	rec := httptest.NewRecorder()
	srv.handleAFKStart(rec, httptest.NewRequest(http.MethodPost, "/afk/start/proj", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?notice=no-ready" {
		t.Fatalf("no-ready no-JS: status %d loc %q; want 303 /?notice=no-ready", rec.Code, rec.Header().Get("Location"))
	}
	if len(gt.added) != 0 {
		t.Errorf("no-ready must not claim (no worktree created): %+v", gt.added)
	}

	// AJAX: the specific notice text comes back in the body (distinct from the
	// generic action-error flash).
	ajax := httptest.NewRequest(http.MethodPost, "/afk/start/proj", nil)
	ajax.Header.Set(fragmentHeader, "1")
	rec = httptest.NewRecorder()
	srv.handleAFKStart(rec, ajax)
	if rec.Code != http.StatusConflict {
		t.Fatalf("no-ready AJAX status = %d; want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No ready-for-agent issues") {
		t.Errorf("no-ready AJAX body = %q; want the specific notice", rec.Body.String())
	}
}

func TestHandleAFKStart_rollsBackClaimOnSpawnFailure(t *testing.T) {
	requireSh(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A tmux that always exits non-zero makes the spawn fail after the claim.
	failTmux := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(failTmux, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &fakeTracker{ready: []Issue{{Index: 9, Title: "b"}, {Index: 7, Title: "a"}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newTestServer(t, root, NewSessions(failTmux, []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	srv.tracker = tr
	srv.git = gt
	srv.worktreeRoot = t.TempDir()

	rec := httptest.NewRecorder()
	srv.handleAFKStart(rec, httptest.NewRequest(http.MethodPost, "/afk/start/proj", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?error=action" {
		t.Fatalf("spawn-fail: status %d loc %q; want 303 /?error=action", rec.Code, rec.Header().Get("Location"))
	}
	// The claim is the worktree on the lowest issue (#7); when the spawn fails it
	// is fully rolled back — both the worktree and its branch — so the issue can
	// be claimed again. No tracker label is ever touched (ADR-0013).
	if len(gt.added) != 1 || gt.added[0].Branch != "afk/7" {
		t.Fatalf("added = %+v; want one worktree on afk/7", gt.added)
	}
	if len(gt.removed) != 1 {
		t.Errorf("removed = %+v; want the worktree removed during rollback", gt.removed)
	}
	if !reflect.DeepEqual(gt.deleted, []string{"afk/7"}) {
		t.Errorf("deleted branches = %+v; want [afk/7] removed during rollback", gt.deleted)
	}
}

func TestHandleAFKStart_happyPathClaimsSpawnsRenders(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 12}, {Index: 7, Title: "do the thing"}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)

	rec := httptest.NewRecorder()
	srv.handleAFKStart(rec, httptest.NewRequest(http.MethodPost, "/afk/start/proj", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("afk start: status %d body %q; want 303", rec.Code, rec.Body.String())
	}
	// The claim is the worktree on the lowest issue (#7): branch afk/7 at
	// <root>/proj-7, created exactly once, none removed (no rollback).
	if len(gt.added) != 1 || gt.added[0].Branch != "afk/7" {
		t.Fatalf("added = %+v; want one worktree on afk/7", gt.added)
	}
	if want := srv.worktreePath(afkID("proj", 7)); gt.added[0].Path != want {
		t.Errorf("worktree path = %q; want %q", gt.added[0].Path, want)
	}
	if len(gt.removed) != 0 || len(gt.deleted) != 0 {
		t.Errorf("happy path tore something down: removed=%+v deleted=%+v; want none", gt.removed, gt.deleted)
	}
	// Renders under the project, badged AFK #7, as a labelled instance (no slot).
	insts := instancesOf(t, srv, "proj")
	if len(insts) != 1 {
		t.Fatalf("instances = %+v; want 1", insts)
	}
	if got := insts[0]; !got.AFK || got.Issue != 7 || got.Name != "proj~afk-7" {
		t.Errorf("afk instance = %+v; want {Name:proj~afk-7 AFK:true Issue:7}", got)
	}
}

func TestHandleAFKStart_refusedOnNonForgejoProject(t *testing.T) {
	requireSh(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: "git@github.com:foo/bar.git"} // not git.cloonar.com
	srv := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	srv.tracker = tr
	srv.git = gt
	srv.worktreeRoot = t.TempDir()

	rec := httptest.NewRecorder()
	srv.handleAFKStart(rec, httptest.NewRequest(http.MethodPost, "/afk/start/proj", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?error=action" {
		t.Fatalf("non-forgejo: status %d loc %q; want 303 /?error=action", rec.Code, rec.Header().Get("Location"))
	}
	if len(gt.added) != 0 {
		t.Errorf("non-forgejo must touch nothing: added=%+v", gt.added)
	}
}

// Manual Start AFK run still works on a paused project: the three-strikes pause
// gates only the scheduler's auto loop, never the shared launchAFKRun claim path.
// Pins the CRITICAL invariant that the Paused term lives in shouldLaunchAuto only.
func TestHandleAFKStart_worksOnPausedProject(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	for i := 0; i < afkPauseThreshold; i++ {
		if _, err := srv.store.IncrementFailures("proj"); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	srv.handleAFKStart(rec, httptest.NewRequest(http.MethodPost, "/afk/start/proj", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("manual start on a paused project: status %d body %q; want 303", rec.Code, rec.Body.String())
	}
	// It claimed and spawned despite the pause: the claim is the worktree on afk/7.
	if len(gt.added) != 1 || gt.added[0].Branch != "afk/7" {
		t.Fatalf("added = %+v; want a manual claim of #7 (worktree on afk/7) despite the pause", gt.added)
	}
	insts := instancesOf(t, srv, "proj")
	if len(insts) != 1 || !insts[0].AFK {
		t.Fatalf("instances = %+v; want 1 AFK run on the paused project", insts)
	}
	// And it is a MANUAL run — the manual path is never the one that gets paused.
	if run, ok := parseAFKRun(insts[0].Name); !ok || run.Auto {
		t.Errorf("started run = %q; want a manual (non-auto) run", insts[0].Name)
	}
	// The counter is untouched by a manual start (only a reap or Reset moves it).
	if got := srv.store.ConsecutiveFailures("proj"); got != afkPauseThreshold {
		t.Errorf("after manual start, counter = %d; want %d (start doesn't touch it)", got, afkPauseThreshold)
	}
}

// An issue already claimed by a local afk/<N> branch (a parked/failed run) is
// skipped by selection (ADR-0013): re-applying ready-for-agent can never re-claim
// it. With another ready issue available the run drains *around* the parked one.
func TestHandleAFKStart_skipsClaimedIssueDrainsAround(t *testing.T) {
	// #7 is parked (its afk/7 branch survives a prior run); #8 is freshly ready.
	tr := &fakeTracker{ready: []Issue{{Index: 7}, {Index: 8}}}
	gt := &fakeGit{origin: cloonarOrigin, branches: map[int]bool{7: true}}
	srv := newAFKServer(t, tr, gt)

	rec := httptest.NewRecorder()
	srv.handleAFKStart(rec, httptest.NewRequest(http.MethodPost, "/afk/start/proj", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("start: status %d body %q; want 303", rec.Code, rec.Body.String())
	}
	// Drained around the parked #7 to claim #8 — #7 is never re-claimed.
	if len(gt.added) != 1 || gt.added[0].Branch != "afk/8" {
		t.Fatalf("added = %+v; want a single claim of #8 (drained around parked #7)", gt.added)
	}
	insts := instancesOf(t, srv, "proj")
	if len(insts) != 1 || insts[0].Issue != 8 {
		t.Errorf("instances = %+v; want one AFK run on #8", insts)
	}
}

// When the ONLY ready issue is already claimed by its afk/<N> branch, a Start is
// a no-op: the parked issue is never re-claimed however many times
// ready-for-agent is (re)applied, and no tracker label is read or written either
// way. This is the no-flapping guarantee at the selection layer (ADR-0013).
func TestHandleAFKStart_parkedOnlyIsNoReady(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin, branches: map[int]bool{7: true}}
	srv := newAFKServer(t, tr, gt)

	rec := httptest.NewRecorder()
	srv.handleAFKStart(rec, httptest.NewRequest(http.MethodPost, "/afk/start/proj", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?notice=no-ready" {
		t.Fatalf("parked-only: status %d loc %q; want 303 /?notice=no-ready", rec.Code, rec.Header().Get("Location"))
	}
	if len(gt.added) != 0 {
		t.Errorf("parked-only claimed %+v; want nothing (no re-claim)", gt.added)
	}
}

// A user-initiated Stop is neutral (#63 supersedes Slice 1): it keeps the
// worktree and branch (so the issue stays claimed/parked, ADR-0013), and the
// watcher must never later reap the now-dead session as a death-failure.
func TestHandleStop_afkRunNeutralKeepsWorktree(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{} // no PR — a broken-neutrality would classify this as death
	srv := newAFKServer(t, tr, gt)
	run := startAFKSession(t, srv, "proj", 7)

	// The watcher has already seen the run (its budget clock is stamped).
	srv.reapAFKRuns(time.Now())

	rec := httptest.NewRecorder()
	srv.handleStop(rec, httptest.NewRequest(http.MethodPost, "/stop/"+run.Name, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("stop afk: status %d; want 303", rec.Code)
	}
	// Neutral: the worktree is KEPT and the branch survives.
	if len(gt.removed) != 0 {
		t.Errorf("manual Stop removed worktrees %+v; want them kept (neutral)", gt.removed)
	}
	if len(gt.deleted) != 0 {
		t.Errorf("manual Stop deleted branches %+v; want them kept", gt.deleted)
	}
	// Session gone → the slot is freed.
	if got := instancesOf(t, srv, "proj"); len(got) != 0 {
		t.Errorf("after stop, instances = %+v; want 0", got)
	}
	// And the run is forgotten: a later sweep does not re-evaluate it (no pulls
	// list) and never reaps it as a death-failure.
	calls := tr.pullsCalls
	srv.reapAFKRuns(time.Now())
	if tr.pullsCalls != calls {
		t.Errorf("watcher re-evaluated a stopped run: pullsCalls %d → %d; want neutral", calls, tr.pullsCalls)
	}
	if len(gt.removed) != 0 {
		t.Errorf("watcher reaped a manually-stopped run: removed=%+v; want neutral", gt.removed)
	}
}

// --- reaper: classification + PR matching (pure) ----------------------------

func TestClassifyAFKRun(t *testing.T) {
	const budget = 45 * time.Minute
	for _, tc := range []struct {
		name         string
		prPresent    bool
		sessionAlive bool
		age          time.Duration
		want         afkOutcome
	}{
		{"alive, no PR, under budget → in progress", false, true, 10 * time.Minute, afkInProgress},
		{"alive with PR → success", true, true, time.Minute, afkSuccess},
		{"dead with PR → success (opened its PR then died)", true, false, time.Minute, afkSuccess},
		{"PR wins even over budget", true, true, 2 * budget, afkSuccess},
		{"dead, no PR → death failure", false, false, time.Minute, afkFailureDeath},
		{"alive, no PR, over budget → timeout failure", false, true, budget + time.Minute, afkFailureTimeout},
		{"alive, no PR, exactly at budget → timeout failure", false, true, budget, afkFailureTimeout},
		{"dead beats timeout when both apply, no PR → death", false, false, 2 * budget, afkFailureDeath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAFKRun(tc.prPresent, tc.sessionAlive, tc.age, budget); got != tc.want {
				t.Errorf("classifyAFKRun(pr=%v alive=%v age=%v) = %v; want %v",
					tc.prPresent, tc.sessionAlive, tc.age, got, tc.want)
			}
		})
	}
}

// afkPRPresent is the "done" signal: an open or merged afk/<N> PR counts, a
// closed-and-unmerged one does not (treated as no PR), and an unrelated branch
// never matches.
func TestAFKPRPresent(t *testing.T) {
	pulls := []PullRequest{
		{Head: "afk/7", State: pullClosed}, // closed-unmerged: NOT a success
		{Head: "afk/12", State: pullOpen},
		{Head: "afk/13", State: pullMerged},
		{Head: "feature/x", State: pullOpen}, // unrelated branch
	}
	for _, tc := range []struct {
		n    int
		want bool
	}{
		{12, true},  // open
		{13, true},  // merged
		{7, false},  // closed-unmerged → treated as no PR
		{99, false}, // no PR at all
	} {
		if got := afkPRPresent(pulls, tc.n); got != tc.want {
			t.Errorf("afkPRPresent(afk/%d) = %v; want %v", tc.n, got, tc.want)
		}
	}
}

// --- selection: branch parsing + claimable filter (pure) --------------------

// parseAFKBranch is the inverse of afkBranch: it reads the issue number out of an
// afk/<N> branch and rejects anything else, so a non-AFK branch (a different
// prefix, a non-numeric suffix) never registers as a claim (ADR-0013).
func TestParseAFKBranch(t *testing.T) {
	for _, tc := range []struct {
		branch string
		want   int
		wantOK bool
	}{
		{"afk/7", 7, true},
		{"afk/63", 63, true},
		{"afk/", 0, false},      // no number
		{"afk/7/x", 0, false},   // not a bare number
		{"afk/x", 0, false},     // non-numeric
		{"feature/7", 0, false}, // wrong prefix
		{"main", 0, false},      // unrelated branch
		{"", 0, false},
	} {
		if got, ok := parseAFKBranch(tc.branch); got != tc.want || ok != tc.wantOK {
			t.Errorf("parseAFKBranch(%q) = (%d,%v); want (%d,%v)", tc.branch, got, ok, tc.want, tc.wantOK)
		}
	}
	// Round-trips with afkBranch for any issue number.
	for _, n := range []int{1, 7, 88, 12345} {
		if got, ok := parseAFKBranch(afkBranch(n)); !ok || got != n {
			t.Errorf("parseAFKBranch(afkBranch(%d)) = (%d,%v); want (%d,true)", n, got, ok, n)
		}
	}
}

// claimableIssues drops any ready issue that already has a local afk/<N> branch —
// the branch-as-claim filter (ADR-0013) — and preserves the order of the rest, so
// a parked/in-flight issue can never be re-selected while its branch lives.
func TestClaimableIssues(t *testing.T) {
	ready := []Issue{{Index: 7, Title: "a"}, {Index: 8, Title: "b"}, {Index: 9, Title: "c"}}
	for _, tc := range []struct {
		name    string
		claimed map[int]bool
		want    []Issue
	}{
		{"none claimed → all claimable", nil, ready},
		{"empty set → all claimable", map[int]bool{}, ready},
		{"one parked is skipped", map[int]bool{8: true}, []Issue{{Index: 7, Title: "a"}, {Index: 9, Title: "c"}}},
		{"all parked → none claimable", map[int]bool{7: true, 8: true, 9: true}, []Issue{}},
		{"unrelated claim is ignored", map[int]bool{42: true}, ready},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimableIssues(ready, tc.claimed); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("claimableIssues(%+v, %v) = %+v; want %+v", ready, tc.claimed, got, tc.want)
			}
		})
	}
	// pickLowestIssue over the claimable set skips a parked lowest and takes the
	// next — the exact selection launchAFKRun relies on.
	if low, ok := pickLowestIssue(claimableIssues(ready, map[int]bool{7: true})); !ok || low.Index != 8 {
		t.Errorf("lowest claimable with #7 parked = (%+v,%v); want #8", low, ok)
	}
}

// --- scheduler: launch predicate (pure) -------------------------------------

// shouldLaunchAuto is the auto-scheduler counterpart of classifyAFKRun: a pure
// decision over five facts, testable with no tmux/tea/clock. The all-go base
// launches; flipping any single term to its blocking value must veto the launch,
// which pins that every term is load-bearing — including #65's three-strikes
// not-paused term.
func TestShouldLaunchAuto(t *testing.T) {
	base := afkAutoDecision{AutoEnabled: true, UnderCap: true, AutoInFlight: false, ReadyExists: true, Paused: false}
	if !shouldLaunchAuto(base) {
		t.Fatalf("all-go decision %+v should launch", base)
	}
	for _, tc := range []struct {
		name string
		mut  func(*afkAutoDecision)
	}{
		{"toggle off vetoes", func(d *afkAutoDecision) { d.AutoEnabled = false }},
		{"at cap vetoes", func(d *afkAutoDecision) { d.UnderCap = false }},
		{"auto already in flight vetoes", func(d *afkAutoDecision) { d.AutoInFlight = true }},
		{"no ready issue vetoes", func(d *afkAutoDecision) { d.ReadyExists = false }},
		{"paused vetoes", func(d *afkAutoDecision) { d.Paused = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.mut(&d)
			if shouldLaunchAuto(d) {
				t.Errorf("decision %+v should NOT launch", d)
			}
		})
	}
}

// afkStartHint is shouldLaunchAuto's display counterpart: a pure mapping from the
// three cache states (and the toggle) to how the Start AFK run control renders,
// testable with no tmux/tea/clock. The count is auto-feature UI, so it shows only
// when the toggle is on; a cached zero greys the control as a hint (the template
// keeps it clickable); an absent cache entry is "unknown" → a plain label.
func TestAFKStartHint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		autoEnabled bool
		count       int
		known       bool
		want        afkStartDisplay
	}{
		{"auto on, cached N>0 → suffix, not greyed", true, 3, true, afkStartDisplay{Suffix: " (3 ready)", Greyed: false}},
		{"auto on, cached one → still a count, not greyed", true, 1, true, afkStartDisplay{Suffix: " (1 ready)", Greyed: false}},
		{"auto on, cached zero → suffix, greyed", true, 0, true, afkStartDisplay{Suffix: " (0 ready)", Greyed: true}},
		{"auto on, no cache entry → unknown, plain", true, 0, false, afkStartDisplay{}},
		{"auto off but cached → no count (auto-feature UI)", false, 5, true, afkStartDisplay{}},
		{"auto off, no cache entry → plain", false, 0, false, afkStartDisplay{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := afkStartHint(tc.autoEnabled, tc.count, tc.known); got != tc.want {
				t.Errorf("afkStartHint(auto=%v count=%d known=%v) = %+v; want %+v",
					tc.autoEnabled, tc.count, tc.known, got, tc.want)
			}
		})
	}
}

// --- reaper: watcher sweep (integration over a private tmux) -----------------

// startAFKSession spawns a live MANUAL AFK-run session by hand (the same name
// scheme and seed-prompt-tolerant command launchAFKRun uses) and returns the run
// it represents, so a test can drive reapAFKRuns against a real tmux session.
func startAFKSession(t *testing.T, srv *Server, project string, issue int) afkRun {
	return startAFKSessionKind(t, srv, project, issue, false)
}

// startAFKSessionKind is startAFKSession with the manual/auto kind selectable, so
// scheduler tests can stand up a pre-existing auto run.
func startAFKSessionKind(t *testing.T, srv *Server, project string, issue int, auto bool) afkRun {
	t.Helper()
	name := composeSessionName(instanceID{Project: project, Label: afkLabel(issue, auto)})
	dir, err := srv.projectDir(project)
	if err != nil {
		t.Fatal(err)
	}
	argv := append(srv.sessions.baseStartArgv(), afkSeedPrompt(issue))
	if err := srv.sessions.StartCommand(name, dir, argv); err != nil {
		t.Fatalf("spawn afk session: %v", err)
	}
	run, ok := parseAFKRun(name)
	if !ok {
		t.Fatalf("parseAFKRun(%q) = !ok; want a recognised AFK run", name)
	}
	if run.Auto != auto {
		t.Fatalf("parseAFKRun(%q).Auto = %v; want %v", name, run.Auto, auto)
	}
	return run
}

// Success: an open afk/<N> PR exists → stop the session (freeing the slot) and
// remove the worktree, keeping the branch.
func TestReapAFKRuns_successStopsAndRemovesWorktree(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{pulls: []PullRequest{{Head: "afk/7", State: pullOpen}}}
	srv := newAFKServer(t, tr, gt)
	startAFKSession(t, srv, "proj", 7)

	srv.reapAFKRuns(time.Now())

	if got := instancesOf(t, srv, "proj"); len(got) != 0 {
		t.Errorf("after success reap, instances = %+v; want 0 (session stopped, slot freed)", got)
	}
	if len(gt.removed) != 1 || gt.removed[0].Path != srv.worktreePath(afkID("proj", 7)) {
		t.Errorf("removed = %+v; want the worktree at %q removed", gt.removed, srv.worktreePath(afkID("proj", 7)))
	}
	if len(gt.deleted) != 0 {
		t.Errorf("success deleted branches %+v; want the afk branch (and PR) kept", gt.deleted)
	}
	// Forgotten: a second sweep is a no-op — nothing tracked, no pulls listed.
	calls := tr.pullsCalls
	srv.reapAFKRuns(time.Now())
	if tr.pullsCalls != calls || len(gt.removed) != 1 {
		t.Errorf("reaped run re-evaluated: pullsCalls %d→%d removed=%+v; want forgotten", calls, tr.pullsCalls, gt.removed)
	}
}

// Success with an ALREADY-MERGED branch (slice 2): when a run's afk/<N> PR has
// landed by the time the reaper sees it, the guarded teardown removes the clean
// worktree AND deletes the merged branch — the GC the old reaper never did (it kept
// every afk/<N> branch forever). An open-PR success instead keeps the branch until
// it merges (TestReapAFKRuns_successStopsAndRemovesWorktree).
func TestReapAFKRuns_successMergedDeletesBranch(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin, merged: map[string]bool{"afk/7": true}}
	tr := &fakeTracker{pulls: []PullRequest{{Head: "afk/7", State: pullMerged}}}
	srv := newAFKServer(t, tr, gt)
	startAFKSession(t, srv, "proj", 7)

	srv.reapAFKRuns(time.Now())

	if len(gt.removed) != 1 || gt.removed[0].Path != srv.worktreePath(afkID("proj", 7)) {
		t.Errorf("removed = %+v; want the clean worktree removed", gt.removed)
	}
	if want := []string{"afk/7"}; !reflect.DeepEqual(gt.deleted, want) {
		t.Errorf("deleted = %+v; want %v (merged branch GC'd on success)", gt.deleted, want)
	}
}

// Timeout (slice 2): a live run with no PR that overruns the budget is stopped and
// gets the SAME guarded teardown as any other outcome — its CLEAN worktree is
// removed, but its unmerged afk/<N> branch is kept, so the issue stays
// parked/inspectable via the branch (ADR-0013). This is the slice-2 change: the
// reaper no longer keeps a failed run's worktree just because it failed; a DIRTY
// one is still kept (TestReapAFKRuns_failureDirtyWorktreeKept).
func TestReapAFKRuns_timeoutGuardedTeardown(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin} // clean worktree, unmerged branch
	tr := &fakeTracker{pulls: nil}        // no PR
	srv := newAFKServer(t, tr, gt)
	startAFKSession(t, srv, "proj", 7)

	t0 := time.Now()
	srv.reapAFKRuns(t0) // stamps the budget clock; alive, no PR, age 0 → in progress
	if got := instancesOf(t, srv, "proj"); len(got) != 1 {
		t.Fatalf("under budget, instances = %+v; want the run still alive", got)
	}
	if len(gt.removed) != 0 {
		t.Fatalf("under budget removed a worktree %+v; want none", gt.removed)
	}

	srv.reapAFKRuns(t0.Add(afkRunBudget + time.Minute)) // over budget → timeout failure
	if got := instancesOf(t, srv, "proj"); len(got) != 0 {
		t.Errorf("after timeout reap, instances = %+v; want 0 (session stopped)", got)
	}
	// Clean worktree → removed; unmerged branch → kept (the claim/park survives).
	if len(gt.removed) != 1 || gt.removed[0].Path != srv.worktreePath(afkID("proj", 7)) {
		t.Errorf("removed = %+v; want the clean timed-out worktree removed", gt.removed)
	}
	if len(gt.deleted) != 0 {
		t.Errorf("timeout deleted branches %+v; want the unmerged afk/<N> branch kept", gt.deleted)
	}
}

// Failure with a DIRTY worktree (slice 2): a run that dies leaving uncommitted work
// keeps BOTH its worktree and branch — the guarded teardown never destroys unsaved
// work, whatever the outcome (a clean failure instead reclaims the worktree, see
// TestReapAFKRuns_timeoutGuardedTeardown). Don't try to stop an already-dead
// session; the run is then forgotten, not re-classified.
func TestReapAFKRuns_failureDirtyWorktreeKept(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{pulls: nil}
	srv := newAFKServer(t, tr, gt)
	gt.dirty = map[string]bool{srv.worktreePath(afkID("proj", 7)): true} // unsaved work
	run := startAFKSession(t, srv, "proj", 7)

	t0 := time.Now()
	srv.reapAFKRuns(t0) // stamps; alive
	// Simulate a crash: kill the session directly (NOT via handleStop, which would
	// be a neutral user-stop).
	if err := srv.sessions.Stop(run.Name); err != nil {
		t.Fatalf("kill session: %v", err)
	}

	srv.reapAFKRuns(t0.Add(time.Minute)) // vanished, no PR → death failure
	// Dirty → the guarded teardown keeps both the worktree and the branch.
	if len(gt.removed) != 0 {
		t.Errorf("death with a dirty worktree removed %+v; want it kept (unsaved work)", gt.removed)
	}
	if len(gt.deleted) != 0 {
		t.Errorf("death with a dirty worktree deleted branches %+v; want them kept", gt.deleted)
	}
	// Forgotten: a later sweep no longer lists pulls for it.
	calls := tr.pullsCalls
	srv.reapAFKRuns(t0.Add(2 * time.Minute))
	if tr.pullsCalls != calls {
		t.Errorf("death run re-evaluated after reap: pullsCalls %d→%d; want forgotten", calls, tr.pullsCalls)
	}
}

// A present PR is success regardless of liveness: a run that opened its PR and
// then died is reaped as a success (worktree removed), not a death-failure.
func TestReapAFKRuns_deadButPRPresentIsSuccess(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{pulls: []PullRequest{{Head: "afk/7", State: pullMerged}}}
	srv := newAFKServer(t, tr, gt)
	run := startAFKSession(t, srv, "proj", 7)

	t0 := time.Now()
	srv.reapAFKRuns(t0) // stamp
	if err := srv.sessions.Stop(run.Name); err != nil {
		t.Fatalf("kill session: %v", err)
	}
	srv.reapAFKRuns(t0.Add(time.Minute)) // vanished but PR present → success

	if len(gt.removed) != 1 || gt.removed[0].Path != srv.worktreePath(afkID("proj", 7)) {
		t.Errorf("removed = %+v; want the worktree removed (PR present = success even when dead)", gt.removed)
	}
	if len(gt.deleted) != 0 {
		t.Errorf("success deleted branches %+v; want the afk branch kept", gt.deleted)
	}
}

// In progress: alive, no PR, under budget → the watcher leaves the run alone.
func TestReapAFKRuns_inProgressLeavesRunAlone(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{pulls: nil} // no PR
	srv := newAFKServer(t, tr, gt)
	startAFKSession(t, srv, "proj", 7)

	srv.reapAFKRuns(time.Now()) // alive, no PR, age 0 → in progress

	if got := instancesOf(t, srv, "proj"); len(got) != 1 {
		t.Errorf("in-progress run, instances = %+v; want it left alive", got)
	}
	if len(gt.removed) != 0 || len(gt.deleted) != 0 {
		t.Errorf("in-progress tore something down: removed=%+v deleted=%+v; want none", gt.removed, gt.deleted)
	}
}

// --- reaper: consecutive-failure counter (#65) ------------------------------

// The reap chokepoint maintains the per-project consecutive-failure counter the
// three-strikes auto-pause reads: a death or timeout failure increments it (and
// it accumulates), a PR-success reap zeroes it.
func TestReapAFKRun_failureIncrementsSuccessResets(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{pulls: nil} // no PR → failures
	srv := newAFKServer(t, tr, gt)

	// A death failure: start, stamp, kill the session, then reap → counter 1.
	death := startAFKSession(t, srv, "proj", 7)
	t0 := time.Now()
	srv.reapAFKRuns(t0) // stamp; alive, no PR → in progress
	if err := srv.sessions.Stop(death.Name); err != nil {
		t.Fatalf("kill session: %v", err)
	}
	srv.reapAFKRuns(t0.Add(time.Minute)) // gone, no PR → death failure
	if got := srv.store.ConsecutiveFailures("proj"); got != 1 {
		t.Fatalf("after death failure, counter = %d; want 1", got)
	}

	// A timeout failure on the next run accumulates → counter 2.
	startAFKSession(t, srv, "proj", 8)
	t1 := t0.Add(2 * time.Minute)
	srv.reapAFKRuns(t1)                                 // stamp #8; in progress
	srv.reapAFKRuns(t1.Add(afkRunBudget + time.Minute)) // alive, no PR, over budget → timeout
	if got := srv.store.ConsecutiveFailures("proj"); got != 2 {
		t.Fatalf("after timeout failure, counter = %d; want 2 (accumulates)", got)
	}

	// A success zeroes the counter, re-arming the loop.
	tr.pulls = []PullRequest{{Head: "afk/9", State: pullOpen}}
	startAFKSession(t, srv, "proj", 9)
	srv.reapAFKRuns(t1.Add(2 * afkRunBudget)) // PR present → success regardless of age
	if got := srv.store.ConsecutiveFailures("proj"); got != 0 {
		t.Errorf("after success, counter = %d; want 0 (reset re-arms)", got)
	}
}

// A user-initiated Stop leaves the consecutive-failure counter unchanged: it is
// neither a success (reset) nor a failure (increment). The run is killed and
// forgotten before the reaper classifies it, so the counter-writing chokepoint
// never runs for it. Seeded non-zero so "unchanged" is a real assertion, not 0==0.
func TestHandleStop_afkRunLeavesFailureCounterUnchanged(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{} // no PR anywhere
	srv := newAFKServer(t, tr, gt)
	if _, err := srv.store.IncrementFailures("proj"); err != nil {
		t.Fatal(err)
	}

	run := startAFKSession(t, srv, "proj", 7)
	srv.reapAFKRuns(time.Now()) // stamp the run's budget clock

	rec := httptest.NewRecorder()
	srv.handleStop(rec, httptest.NewRequest(http.MethodPost, "/stop/"+run.Name, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("stop afk: status %d; want 303", rec.Code)
	}
	// A later sweep must not reap the now-dead session as a death-failure.
	srv.reapAFKRuns(time.Now())
	if got := srv.store.ConsecutiveFailures("proj"); got != 1 {
		t.Errorf("after user Stop, counter = %d; want 1 (unchanged — Stop is neutral)", got)
	}
}

// classifyAndClaimAFKRun is the atomic gate that keeps a manual Stop neutral
// against a concurrent sweep: a run the user has stopped (killed + forgotten) is
// refused even when a stale snapshot would call it an over-budget timeout, while
// an untouched live over-budget run is still claimed as a timeout.
func TestClassifyAndClaimAFKRun_manualStopBeatsTimeout(t *testing.T) {
	srv := newAFKServer(t, &fakeTracker{}, &fakeGit{origin: cloonarOrigin})
	stopped := startAFKSession(t, srv, "proj", 7)
	live := startAFKSession(t, srv, "proj", 8)

	t0 := time.Now()
	srv.reapAFKRuns(t0) // stamp both runs' budget clocks (both in progress, age 0)
	overBudget := t0.Add(afkRunBudget + time.Minute)

	// The user stops #7: stopInstance kills the session and forgets the run under
	// afkRunsMu, atomically.
	rec := httptest.NewRecorder()
	srv.handleStop(rec, httptest.NewRequest(http.MethodPost, "/stop/"+stopped.Name, nil))

	// #7 is forgotten → the claim refuses it, even at a time well past budget.
	if outcome, _, ok := srv.classifyAndClaimAFKRun(stopped.Name, false, overBudget); ok {
		t.Errorf("classifyAndClaim(stopped) = (%v, ok=true); want ok=false (manual Stop is neutral)", outcome)
	}
	// #8 is the control: untouched, live, over budget → genuinely claimed as a timeout.
	if outcome, alive, ok := srv.classifyAndClaimAFKRun(live.Name, false, overBudget); !ok || outcome != afkFailureTimeout || !alive {
		t.Errorf("classifyAndClaim(live, over budget) = (%v, alive=%v, ok=%v); want (timeout, true, true)", outcome, alive, ok)
	}
}

// Driving reapAFKRuns concurrently with handleStop must stay race-free (run under
// -race). With DIRTY worktrees, every interleaving keeps the worktree: a neutral
// manual Stop keeps it, and a timeout/death guard-keeps a dirty one (slice 2) — so
// no interleaving removes a worktree or reaches a destructive path.
func TestReapAFKRuns_concurrentManualStopIsRaceFree(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, &fakeTracker{}, gt) // no PRs
	gt.dirty = map[string]bool{}
	var runs []afkRun
	for _, n := range []int{7, 8, 9} {
		runs = append(runs, startAFKSession(t, srv, "proj", n))
		gt.dirty[srv.worktreePath(afkID("proj", n))] = true // unsaved work → kept on any outcome
	}
	t0 := time.Now()
	srv.reapAFKRuns(t0) // stamp all three
	overBudget := t0.Add(afkRunBudget + time.Minute)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			srv.reapAFKRuns(overBudget) // would time out any still-live run
		}
	}()
	go func() {
		defer wg.Done()
		for _, run := range runs {
			rec := httptest.NewRecorder()
			srv.handleStop(rec, httptest.NewRequest(http.MethodPost, "/stop/"+run.Name, nil))
		}
	}()
	wg.Wait()

	if len(gt.removed) != 0 {
		t.Errorf("concurrent stop/reap removed worktrees %+v; want none (dirty = always kept)", gt.removed)
	}
}

// --- scheduler: sweep + toggle (integration over a private tmux) ------------

// mustAutoEnable turns a project's auto toggle on in the store.
func mustAutoEnable(t *testing.T, srv *Server, project string) {
	t.Helper()
	if err := srv.store.SetAutoEnabled(project, true); err != nil {
		t.Fatalf("SetAutoEnabled %q: %v", project, err)
	}
}

// A sweep over an auto-on Forgejo project with ready work and headroom launches
// the next run through the same claim path as a manual Start — but marked auto.
func TestScheduleAFKRuns_launchesAutoRun(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 12}, {Index: 7, Title: "x"}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")

	srv.scheduleAFKRuns()

	// Claimed the lowest ready issue (#7) exactly once via its worktree, no rollback.
	if len(gt.added) != 1 || gt.added[0].Branch != "afk/7" {
		t.Fatalf("added = %+v; want one worktree on afk/7", gt.added)
	}
	// Live as an AUTO run: the name carries the afk-auto- marker (slot >=2),
	// badged AFK #7, and parseAFKRun reads it back as auto.
	insts := instancesOf(t, srv, "proj")
	if len(insts) != 1 {
		t.Fatalf("instances = %+v; want 1 auto run", insts)
	}
	if got := insts[0]; got.Name != "proj~afk-auto-7" || !got.AFK || got.Issue != 7 {
		t.Errorf("auto instance = %+v; want {Name:proj~afk-auto-7 AFK:true Issue:7}", got)
	}
	if run, ok := parseAFKRun(insts[0].Name); !ok || !run.Auto {
		t.Errorf("parseAFKRun(%q) = (%+v, ok=%v); want a recognised auto run", insts[0].Name, run, ok)
	}
}

// A sweep records each swept auto-on project's ready-for-agent COUNT (not just a
// boolean) in the in-memory render-path cache, so the ⋯ menu can annotate Start
// AFK run with "(N ready)" — without any tea call on the render path (#66).
func TestScheduleAFKRuns_cachesReadyCount(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 12}, {Index: 7}, {Index: 9}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")

	if _, known := srv.readyCount("proj"); known {
		t.Fatal("before any sweep the count should be unknown")
	}
	srv.scheduleAFKRuns()

	if n, known := srv.readyCount("proj"); !known || n != 3 {
		t.Errorf("readyCount after sweep = (%d, known=%v); want (3, true)", n, known)
	}
}

// The count is captured BEFORE the launch gate, so a swept auto-on project gets
// its count cached even on a tick that launches nothing — here an auto run is
// already in flight, so the serial guard blocks a new launch, yet the hint still
// updates.
func TestScheduleAFKRuns_cachesReadyCountEvenWhenNoLaunch(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 8}, {Index: 5}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")
	startAFKSessionKind(t, srv, "proj", 7, true) // an auto run already in flight

	srv.scheduleAFKRuns()

	if len(gt.added) != 0 {
		t.Fatalf("serial guard should have blocked a launch; created worktree %+v", gt.added)
	}
	if n, known := srv.readyCount("proj"); !known || n != 2 {
		t.Errorf("readyCount = (%d, known=%v); want (2, true) — cached despite no launch", n, known)
	}
}

// The count cache is in-memory only: a restart (a fresh Server over the SAME
// persisted store) re-reads persisted state but starts with an empty count cache,
// so a project shows no count until the next sweep repopulates it (#66).
func TestScheduleAFKRuns_readyCountNotPersisted(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "s.json")
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin}

	srv := newAFKServerAt(t, root, storePath, tr, gt)
	mustAutoEnable(t, srv, "proj")
	srv.scheduleAFKRuns()
	if _, known := srv.readyCount("proj"); !known {
		t.Fatal("after a sweep the count should be cached")
	}

	// Restart: a new server over the same store file, with state reloaded.
	restarted := newAFKServerAt(t, root, storePath, tr, gt)
	if err := restarted.store.Load(); err != nil {
		t.Fatalf("reload store: %v", err)
	}
	// The persisted toggle survived — proving the store actually reloaded, so the
	// count's absence below is the cache being in-memory, not a dead/empty store.
	if !restarted.store.AutoEnabled("proj") {
		t.Fatal("auto toggle should survive a restart (it is persisted)")
	}
	if n, known := restarted.readyCount("proj"); known {
		t.Errorf("readyCount after restart = (%d, known=%v); want unknown — the count must not be persisted", n, known)
	}
}

// Serial per project: a sweep launches nothing for a project that already has an
// auto run in flight, even with ready work and cap headroom.
func TestScheduleAFKRuns_serialPerProject(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 8}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")
	startAFKSessionKind(t, srv, "proj", 7, true) // an auto run already in flight

	srv.scheduleAFKRuns()

	if len(gt.added) != 0 {
		t.Errorf("serial violated: created worktree %+v while an auto run was already in flight", gt.added)
	}
	if got := instancesOf(t, srv, "proj"); len(got) != 1 {
		t.Errorf("instances = %+v; want only the pre-existing auto run", got)
	}
}

// A manually-started run is additive: it does not count as the project's auto run,
// so the scheduler still launches one auto run alongside it (under the cap).
func TestScheduleAFKRuns_manualRunDoesNotBlockAuto(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 8}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")
	startAFKSessionKind(t, srv, "proj", 7, false) // a MANUAL run in flight (afk-7)

	srv.scheduleAFKRuns()

	if len(gt.added) != 1 || gt.added[0].Branch != "afk/8" {
		t.Fatalf("added = %+v; want the auto run to claim #8 (worktree on afk/8) despite a live manual run", gt.added)
	}
	insts := instancesOf(t, srv, "proj")
	if len(insts) != 2 {
		t.Fatalf("instances = %+v; want 2 (manual + auto)", insts)
	}
	autos := 0
	for _, in := range insts {
		if run, ok := parseAFKRun(in.Name); ok && run.Auto {
			autos++
		}
	}
	if autos != 1 {
		t.Errorf("auto runs = %d; want exactly 1 (a manual run must not register as auto)", autos)
	}
}

// Toggle off: an auto-off project never launches, however much ready work exists.
func TestScheduleAFKRuns_offProjectDoesNothing(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	// autoEnabled deliberately left at its default (off).

	srv.scheduleAFKRuns()

	if len(gt.added) != 0 {
		t.Errorf("auto-off project acted: added=%+v; want nothing", gt.added)
	}
	if got := instancesOf(t, srv, "proj"); len(got) != 0 {
		t.Errorf("instances = %+v; want 0 (toggle off)", got)
	}
}

// Three-strikes pause: a project at the failure threshold launches no auto run
// even with the toggle on, ready work, and cap headroom — while a sibling project
// below the threshold still launches. Pins both that the pause gates the auto loop
// AND that it is strictly per project.
func TestScheduleAFKRuns_pauseIsPerProject(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"healthy", "paused"} {
		if err := os.MkdirAll(filepath.Join(root, p, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin}
	sessions := NewSessions(privateTmux(t), []string{"sh", "-c", "sleep 600"})
	srv := newTestServer(t, root, sessions, NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	srv.tracker = tr
	srv.git = gt
	srv.worktreeRoot = t.TempDir()
	srv.captureTimeout = 100 * time.Millisecond
	mustAutoEnable(t, srv, "healthy")
	mustAutoEnable(t, srv, "paused")
	// Drive only "paused" to the threshold.
	for i := 0; i < afkPauseThreshold; i++ {
		if _, err := srv.store.IncrementFailures("paused"); err != nil {
			t.Fatal(err)
		}
	}

	srv.scheduleAFKRuns()

	if got := instancesOf(t, srv, "paused"); len(got) != 0 {
		t.Errorf("paused project launched %+v; want 0 (auto loop paused at the threshold)", got)
	}
	healthy := instancesOf(t, srv, "healthy")
	if len(healthy) != 1 {
		t.Fatalf("healthy project instances = %+v; want 1 (the pause is per project)", healthy)
	}
	if run, ok := parseAFKRun(healthy[0].Name); !ok || !run.Auto {
		t.Errorf("healthy launch = %q; want a recognised auto run", healthy[0].Name)
	}
}

// One short of the threshold is NOT paused: the auto loop still launches. Guards
// the boundary (>=, not >) so the third failure is what pauses, not the second.
func TestScheduleAFKRuns_belowThresholdStillLaunches(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")
	for i := 0; i < afkPauseThreshold-1; i++ {
		if _, err := srv.store.IncrementFailures("proj"); err != nil {
			t.Fatal(err)
		}
	}

	srv.scheduleAFKRuns()

	if got := instancesOf(t, srv, "proj"); len(got) != 1 {
		t.Errorf("one short of the threshold, instances = %+v; want 1 (not yet paused)", got)
	}
}

// An empty ready queue idles: nothing claimed, nothing launched, no error.
func TestScheduleAFKRuns_noReadyIdles(t *testing.T) {
	tr := &fakeTracker{ready: nil}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")

	srv.scheduleAFKRuns()

	if len(gt.added) != 0 {
		t.Errorf("empty queue claimed %+v; want nothing", gt.added)
	}
	if got := instancesOf(t, srv, "proj"); len(got) != 0 {
		t.Errorf("instances = %+v; want 0 (nothing ready)", got)
	}
}

// A project whose only ready issue is already claimed by its afk/<N> branch
// launches nothing and — crucially — does not loop: the claimable count is zero,
// so ReadyExists is false and the "(N ready)" hint reads 0 rather than the raw
// ready count of 1 (ADR-0013). Re-applying ready-for-agent changes nothing.
func TestScheduleAFKRuns_parkedIssueDoesNotLaunchOrLoop(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin, branches: map[int]bool{7: true}}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")

	srv.scheduleAFKRuns()

	if len(gt.added) != 0 {
		t.Errorf("parked-only auto project launched %+v; want nothing", gt.added)
	}
	if got := instancesOf(t, srv, "proj"); len(got) != 0 {
		t.Errorf("instances = %+v; want 0 (the only ready issue is parked)", got)
	}
	// The hint reflects the CLAIMABLE count (0), not the raw ready count (1).
	if n, known := srv.readyCount("proj"); !known || n != 0 {
		t.Errorf("readyCount = (%d, known=%v); want (0, true) — claimable, not raw ready", n, known)
	}
}

// At the global cap the sweep launches nothing and does not error — the predicate
// vetoes on UnderCap=false before any claim.
func TestScheduleAFKRuns_atCapLaunchesNothing(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	srv.maxInstances = 1
	mustAutoEnable(t, srv, "proj")
	// Fill the only slot with an unrelated live instance so the cap is reached.
	dir, err := srv.projectDir("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.sessions.Start("proj", dir); err != nil { // lone manual session, slot 1
		t.Fatalf("spawn: %v", err)
	}

	srv.scheduleAFKRuns() // at cap → no launch, no error

	if len(gt.added) != 0 {
		t.Errorf("at cap: claimed %+v; want nothing", gt.added)
	}
	if got := instancesOf(t, srv, "proj"); len(got) != 1 {
		t.Errorf("instances = %+v; want still just the one filling the cap", got)
	}
}

// Two sweeps at once — the ticker racing a toggle-on kick — must still produce
// exactly one auto run. The claim is single-flighted under afkMu and the
// one-auto-per-project check is remade there on fresh liveness, so the second
// sweep sees the first's just-spawned run and backs off. Run under -race.
func TestScheduleAFKRuns_concurrentSweepsStaySerial(t *testing.T) {
	tr := &fakeTracker{ready: []Issue{{Index: 7}, {Index: 8}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); srv.scheduleAFKRuns() }()
	}
	wg.Wait()

	if len(gt.added) != 1 {
		t.Errorf("concurrent sweeps claimed %+v; want exactly one claim (serial per project)", gt.added)
	}
	if got := instancesOf(t, srv, "proj"); len(got) != 1 {
		t.Errorf("concurrent sweeps produced %d instances %+v; want exactly 1 auto run", len(got), got)
	}
}

// Logged out, the scheduler claims nothing: an auto-launched remote-control
// session would die at the login wall and be reaped as a failure, parking its
// issue behind its surviving afk/<N> branch for nothing. So the whole tick is
// gated on a fresh login check, exactly as the manual Start is.
func TestScheduleAFKRuns_loggedOutDoesNotClaim(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &fakeTracker{ready: []Issue{{Index: 7}}}
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newTestServer(t, root, NewSessions(privateTmux(t), []string{"sh", "-c", "sleep 600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), false) // logged OUT
	srv.tracker = tr
	srv.git = gt
	srv.worktreeRoot = t.TempDir()
	mustAutoEnable(t, srv, "proj")

	srv.scheduleAFKRuns()

	if len(gt.added) != 0 {
		t.Errorf("logged out: claimed %+v; want nothing", gt.added)
	}
	if got := instancesOf(t, srv, "proj"); len(got) != 0 {
		t.Errorf("logged out: instances = %+v; want 0", got)
	}
}

// The reaper must recognise and reap an AUTO run exactly like a manual one. This
// is the guard for the afk-auto- marker hazard: a naive encoding that broke
// parseAFKLabel would leave parseAFKRun blind to auto runs, so the reaper would
// silently never free their slots.
func TestReapAFKRuns_reapsAutoRunOnSuccess(t *testing.T) {
	gt := &fakeGit{origin: cloonarOrigin}
	tr := &fakeTracker{pulls: []PullRequest{{Head: "afk/7", State: pullOpen}}}
	srv := newAFKServer(t, tr, gt)
	startAFKSessionKind(t, srv, "proj", 7, true) // an AUTO run (afk-auto-7)

	srv.reapAFKRuns(time.Now())

	if got := instancesOf(t, srv, "proj"); len(got) != 0 {
		t.Errorf("after success reap, instances = %+v; want 0 (auto run stopped, slot freed)", got)
	}
	if len(gt.removed) != 1 || gt.removed[0].Path != srv.worktreePath(afkID("proj", 7)) {
		t.Errorf("removed = %+v; want the auto run's worktree removed", gt.removed)
	}
}

// The ⋯ toggle flips and persists the per-project flag, refuses on a non-Forgejo
// project, and re-renders with a label the morph can sync (On/Off text).
func TestHandleAFKAuto_togglesPersistsAndRefusesNonForgejo(t *testing.T) {
	tr := &fakeTracker{ready: nil} // empty queue → the toggle-on kick is a harmless no-op
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)

	// Off → On: persisted, and the fragment now labels the button On.
	rec := httptest.NewRecorder()
	srv.handleAFKAuto(rec, httptest.NewRequest(http.MethodPost, "/afk/auto/proj", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("toggle on: status %d; want 303", rec.Code)
	}
	if !srv.store.AutoEnabled("proj") {
		t.Error("after toggle-on, store.AutoEnabled = false; want true (persisted)")
	}
	frec := httptest.NewRecorder()
	srv.handleFragment(frec, httptest.NewRequest(http.MethodGet, "/fragment", nil))
	if !strings.Contains(frec.Body.String(), "Auto AFK runs: On") {
		t.Error("fragment after toggle-on should render the button labelled On")
	}

	// On → Off: flips back and persists.
	rec = httptest.NewRecorder()
	srv.handleAFKAuto(rec, httptest.NewRequest(http.MethodPost, "/afk/auto/proj", nil))
	if srv.store.AutoEnabled("proj") {
		t.Error("second toggle should flip the flag back off")
	}

	// A direct POST against a non-Forgejo project is refused and never persists,
	// exactly like Start AFK run.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ext", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ext := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	ext.tracker = &fakeTracker{}
	ext.git = &fakeGit{origin: "git@github.com:foo/bar.git"} // not git.cloonar.com
	ext.worktreeRoot = t.TempDir()
	rec = httptest.NewRecorder()
	ext.handleAFKAuto(rec, httptest.NewRequest(http.MethodPost, "/afk/auto/ext", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?error=action" {
		t.Fatalf("non-forgejo toggle: status %d loc %q; want 303 /?error=action", rec.Code, rec.Header().Get("Location"))
	}
	if ext.store.AutoEnabled("ext") {
		t.Error("non-forgejo toggle must not persist the flag")
	}
}

// POST /afk/reset/<project> zeroes the consecutive-failure counter (re-arming the
// auto loop), works both no-JS (303 redirect) and via the fetch/morph path (200 +
// the re-rendered #live fragment), leaves the auto toggle untouched, and is
// refused on a non-Forgejo project — mirroring the auto-toggle action's plumbing.
func TestHandleAFKReset_clearsCounterRearmsAndRefusesNonForgejo(t *testing.T) {
	tr := &fakeTracker{ready: nil} // empty queue → the re-arm sweep is a harmless no-op
	gt := &fakeGit{origin: cloonarOrigin}
	srv := newAFKServer(t, tr, gt)
	mustAutoEnable(t, srv, "proj")
	for i := 0; i < afkPauseThreshold; i++ {
		if _, err := srv.store.IncrementFailures("proj"); err != nil {
			t.Fatal(err)
		}
	}

	// No-JS: 303 back to the index, counter zeroed, the auto toggle left on.
	rec := httptest.NewRecorder()
	srv.handleAFKReset(rec, httptest.NewRequest(http.MethodPost, "/afk/reset/proj", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("reset no-JS: status %d loc %q; want 303 /", rec.Code, rec.Header().Get("Location"))
	}
	if got := srv.store.ConsecutiveFailures("proj"); got != 0 {
		t.Errorf("after reset, counter = %d; want 0", got)
	}
	if !srv.store.AutoEnabled("proj") {
		t.Error("reset flipped the auto toggle; want it untouched (Reset only clears the counter)")
	}

	// fetch/morph path: re-pause, then reset with the fragment header → 200 plus
	// the re-rendered #live fragment (not a 303), counter zeroed again.
	for i := 0; i < afkPauseThreshold; i++ {
		if _, err := srv.store.IncrementFailures("proj"); err != nil {
			t.Fatal(err)
		}
	}
	ajax := httptest.NewRequest(http.MethodPost, "/afk/reset/proj", nil)
	ajax.Header.Set(fragmentHeader, "1")
	rec = httptest.NewRecorder()
	srv.handleAFKReset(rec, ajax)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset fetch path: status %d; want 200 (fragment)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Error("reset fetch path should return the #live fragment, not the full page")
	}
	if got := srv.store.ConsecutiveFailures("proj"); got != 0 {
		t.Errorf("after fetch reset, counter = %d; want 0", got)
	}

	// A direct POST against a non-Forgejo project is refused and clears nothing,
	// exactly like the auto toggle.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ext", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ext := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	ext.tracker = &fakeTracker{}
	ext.git = &fakeGit{origin: "git@github.com:foo/bar.git"} // not git.cloonar.com
	ext.worktreeRoot = t.TempDir()
	if _, err := ext.store.IncrementFailures("ext"); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	ext.handleAFKReset(rec, httptest.NewRequest(http.MethodPost, "/afk/reset/ext", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?error=action" {
		t.Fatalf("non-forgejo reset: status %d loc %q; want 303 /?error=action", rec.Code, rec.Header().Get("Location"))
	}
	if got := ext.store.ConsecutiveFailures("ext"); got != 1 {
		t.Errorf("non-forgejo reset cleared the counter (now %d); want it refused (still 1)", got)
	}
}

// --- render tests -----------------------------------------------------------

func TestLivePartial_afkMenuAndBadge(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)

	// Forgejo project with one AFK run alongside an ordinary instance.
	out := renderLive(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6, LiveCount: 2,
		Groups: []projectGroup{{
			Name: "proj", Path: "/p/proj", Forgejo: true,
			Instances: []instanceView{
				{Name: "proj~20260608-1530", Time: "15:30", URL: "https://claude.ai/code/X"},
				{Name: "proj~afk-7", AFK: true, Issue: 7, URL: "https://claude.ai/code/Y"},
			},
		}},
	})
	if !strings.Contains(out, `action="/afk/start/proj"`) {
		t.Error("forgejo card should expose the Start AFK run action")
	}
	if !strings.Contains(out, "Start AFK run") {
		t.Error("forgejo menu should offer Start AFK run")
	}
	if !strings.Contains(out, "AFK #7") {
		t.Error("afk instance should render the AFK #7 badge")
	}
	if strings.Contains(out, "needs a git.cloonar.com repo") {
		t.Error("forgejo card must not show the disabled line")
	}
	// The Auto AFK runs toggle is a form-button posting to /afk/auto/, and its
	// label reflects the (default-off) flag — NOT an <input type=checkbox>.
	if !strings.Contains(out, `action="/afk/auto/proj"`) {
		t.Error("forgejo card should expose the Auto AFK runs toggle action")
	}
	if !strings.Contains(out, "Auto AFK runs: Off") {
		t.Error("auto toggle should label Off when the flag is off")
	}
	if strings.Contains(out, `type=checkbox`) || strings.Contains(out, `type="checkbox"`) {
		t.Error("auto toggle must be a form-button, never a checkbox (the morph won't sync checked)")
	}

	// AutoEnabled on: the same control now reads On (server text the morph syncs).
	onOut := renderLive(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6,
		Groups: []projectGroup{{Name: "proj", Path: "/p/proj", Forgejo: true, AutoEnabled: true}},
	})
	if !strings.Contains(onOut, "Auto AFK runs: On") {
		t.Error("auto toggle should label On when the flag is on")
	}

	// Non-Forgejo project: disabled line, no Start AND no Auto action.
	out = renderLive(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6,
		Groups: []projectGroup{{Name: "ext", Path: "/p/ext", Forgejo: false}},
	})
	if !strings.Contains(out, "needs a git.cloonar.com repo") {
		t.Error("non-forgejo card should show the disabled menu line")
	}
	if strings.Contains(out, `action="/afk/start/ext"`) {
		t.Error("non-forgejo card must not expose the AFK action")
	}
	if strings.Contains(out, `action="/afk/auto/ext"`) || strings.Contains(out, "Auto AFK runs:") {
		t.Error("non-forgejo card must not expose the Auto AFK runs toggle")
	}
}

// TestLivePartial_afkStartReadyHint pins the ⋯ menu's "(N ready)" hint across the
// three cache states plus the auto-off gate, all server-rendered so the morph
// syncs them: a cached N>0 shows "(N ready)" (not greyed); a cached zero shows
// "(0 ready)" greyed but STILL a real submit button (never the HTML disabled
// attribute / the non-clickable .disabled span), so the authoritative click is
// never blocked; an unswept or auto-off project shows a plain "Start AFK run".
func TestLivePartial_afkStartReadyHint(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)
	render := func(g projectGroup) string {
		return renderLive(t, srv, pageData{LoggedIn: true, MaxInstances: 6, Groups: []projectGroup{g}})
	}
	base := projectGroup{Name: "proj", Path: "/p/proj", Forgejo: true, AutoEnabled: true}

	// cached N>0: enabled "(3 ready)", not greyed.
	g := base
	g.ReadyCount, g.ReadyKnown = 3, true
	out := render(g)
	if !strings.Contains(out, ">Start AFK run (3 ready)</button>") {
		t.Errorf("cached N>0 should show the count suffix; got:\n%s", out)
	}
	if strings.Contains(out, "greyed") {
		t.Error("cached N>0 must not grey the control")
	}

	// cached zero: greyed AND still a submit button posting to the action.
	g = base
	g.ReadyCount, g.ReadyKnown = 0, true
	out = render(g)
	if !strings.Contains(out, ">Start AFK run (0 ready)</button>") {
		t.Errorf("cached zero should show (0 ready); got:\n%s", out)
	}
	if !strings.Contains(out, `class="menu-item greyed" type="submit"`) {
		t.Error("cached zero should grey the control but keep it a submit button")
	}
	if !strings.Contains(out, `action="/afk/start/proj"`) {
		t.Error("cached zero must keep the Start AFK run action (the click stays authoritative)")
	}
	if strings.Contains(out, "Start AFK run (0 ready)</span>") {
		t.Error("cached zero must NOT render as the disabled <span> — it must stay clickable")
	}

	// unknown (auto on, not yet swept): plain label, no suffix, not greyed.
	g = base
	g.ReadyKnown = false
	out = render(g)
	if !strings.Contains(out, ">Start AFK run</button>") {
		t.Errorf("unknown should show a plain label; got:\n%s", out)
	}
	if strings.Contains(out, "ready)") || strings.Contains(out, "greyed") {
		t.Error("unknown must show neither a count nor the greyed hint")
	}

	// auto OFF with a stale cached count: the hint is gated off → plain label.
	g = base
	g.AutoEnabled = false
	g.ReadyCount, g.ReadyKnown = 5, true
	out = render(g)
	if !strings.Contains(out, ">Start AFK run</button>") {
		t.Errorf("auto-off should show a plain label; got:\n%s", out)
	}
	if strings.Contains(out, "ready)") {
		t.Error("auto-off must not show a count even with a stale cache entry")
	}
}

// A paused project's ⋯ menu surfaces the "Auto paused · N fails" indicator and a
// Reset form-button posting to /afk/reset/<project>; an un-paused project shows
// neither. Both are server-rendered text/forms the morph syncs, like the toggle.
func TestLivePartial_pausedShowsResetControl(t *testing.T) {
	srv := newTestServer(t, t.TempDir(), NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)

	// Paused: the indicator carries the live count and the Reset action appears.
	paused := renderLive(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6,
		Groups: []projectGroup{{
			Name: "proj", Path: "/p/proj", Forgejo: true,
			AutoEnabled: true, ConsecutiveFailures: 3, Paused: true,
		}},
	})
	if !strings.Contains(paused, "Auto paused · 3 fails") {
		t.Errorf("paused card should show the 'Auto paused · 3 fails' indicator; got:\n%s", paused)
	}
	if !strings.Contains(paused, `action="/afk/reset/proj"`) {
		t.Error("paused card should expose the Reset action")
	}
	if !strings.Contains(paused, ">Reset<") {
		t.Error("paused card should render a Reset button")
	}

	// Not paused: neither the indicator nor the Reset control renders.
	healthy := renderLive(t, srv, pageData{
		LoggedIn: true, MaxInstances: 6,
		Groups: []projectGroup{{
			Name: "proj", Path: "/p/proj", Forgejo: true,
			AutoEnabled: true, ConsecutiveFailures: 0, Paused: false,
		}},
	})
	if strings.Contains(healthy, "Auto paused") {
		t.Error("un-paused card must not show the paused indicator")
	}
	if strings.Contains(healthy, `action="/afk/reset/proj"`) {
		t.Error("un-paused card must not expose the Reset action")
	}
}

// TestLivePartial_noticeRendersInErrbar pins the no-JS no-ready notice: a full
// page render with Notice set must show the errbar carrying that exact text
// (not the generic action-error message).
func TestLivePartial_noticeRendersInErrbar(t *testing.T) {
	requireTmux(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, root, NewSessions("tmux", []string{"sleep", "600"}),
		NewStore(filepath.Join(t.TempDir(), "s.json")), true)

	rec := httptest.NewRecorder()
	srv.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/?notice=no-ready", nil))
	body := rec.Body.String()
	if !strings.Contains(body, noReadyNotice) {
		t.Errorf("index with ?notice=no-ready should render %q in the errbar", noReadyNotice)
	}
}

// --- worktree path ----------------------------------------------------------

// TestWorktreePath_no83ShortName guards against a regression of the bug this
// change fixes: worktreePath used to join project and issue with instanceSep
// ("~"), yielding e.g. "<root>/cloonar-cloonar-nixos~63". The "~63" component
// matches the Windows 8.3 short-name pattern (PROGRA~1), which claude flags as a
// path-confusion risk and forces manual approval for every file edit — silently
// stalling an unattended AFK run on its first edit, in any --permission-mode. The
// directory name must therefore stay "~"-free while still encoding project and
// issue (teardown reconstructs it via worktreePath).
func TestWorktreePath_no83ShortName(t *testing.T) {
	srv := &Server{worktreeRoot: "/wt"}
	got := srv.worktreePath(afkID("cloonar-cloonar-nixos", 63)) // the original failing case
	if want := "/wt/cloonar-cloonar-nixos-63"; got != want {
		t.Errorf("worktreePath = %q; want %q", got, want)
	}
	// The property that actually matters: no "~" anywhere, so no path component
	// can match the 8.3 short-name heuristic that gates edits.
	if strings.ContainsRune(got, '~') {
		t.Errorf("worktreePath = %q; must not contain '~' (matches 8.3 short-name, stalls AFK edits)", got)
	}
	// Still round-trippable: the basename carries both project and issue, so
	// teardownInstance can rebuild the exact path from the session name.
	if base := filepath.Base(got); !strings.Contains(base, "cloonar-cloonar-nixos") || !strings.Contains(base, "63") {
		t.Errorf("worktreePath base = %q; must encode project and issue", base)
	}
}
