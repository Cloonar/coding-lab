package preview

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// changeLog records every onChange call as "repoID/runID", so tests can
// assert that each registry transition announced itself.
type changeLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *changeLog) record(repoID, runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, repoID+"/"+runID)
}

// count returns how many onChange calls have been recorded so far.
func (c *changeLog) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// last returns the most recent onChange call, or "" when there was none.
func (c *changeLog) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return ""
	}
	return c.calls[len(c.calls)-1]
}

// newTestRegistry builds a Registry for lab.internal on :8081 whose onChange
// feeds the returned changeLog.
func newTestRegistry(t *testing.T) (*Registry, *changeLog) {
	t.Helper()
	log := &changeLog{}
	return NewRegistry("lab.internal", 8081, log.record), log
}

// testSession is the labctl session id mustReserve claims with: one fixed
// "process" per run, which is what a reconnecting `labctl expose` looks like.
func testSession(runID string) string { return "session-" + runID }

// mustReserve asserts Reserve succeeds, as runID's testSession.
func mustReserve(t *testing.T, r *Registry, repoID, runID, name string, port int) {
	t.Helper()
	if err := r.Reserve(repoID, runID, name, testSession(runID), port); err != nil {
		t.Fatalf("Reserve(%q, %q, %q, %d): %v", repoID, runID, name, port, err)
	}
}

func TestValidName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"web", true},
		{"a", true},
		{"0", true},
		{"my-app-2", true},
		{"a-b", true},
		{strings.Repeat("a", 63), true},
		{"", false},
		{strings.Repeat("a", 64), false},
		{"Web", false},
		{"WEB", false},
		{"-web", false},
		{"web-", false},
		{"-", false},
		{"web.app", false},
		{"web.", false},
		{"web_app", false},
		{"web app", false},
		{"wéb", false},
	} {
		if got := ValidName(tc.name); got != tc.want {
			t.Errorf("ValidName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestURL(t *testing.T) {
	// The listener's port is appended only when a browser would need it.
	for _, tc := range []struct {
		port int
		want string
	}{
		{443, "https://web.lab.internal"},
		{0, "https://web.lab.internal"},
		{80, "https://web.lab.internal:80"},
		{8081, "https://web.lab.internal:8081"},
	} {
		r := NewRegistry("lab.internal", tc.port, nil)
		if got := r.URL("web"); got != tc.want {
			t.Errorf("port %d: URL = %q, want %q", tc.port, got, tc.want)
		}
	}
}

func TestReserveValidates(t *testing.T) {
	r, log := newTestRegistry(t)
	if err := r.Reserve("repo", "run1", "Web", "s1", 5173); !errors.Is(err, ErrInvalidName) {
		t.Errorf("uppercase name: err = %v, want ErrInvalidName", err)
	}
	for _, port := range []int{0, -1, 65536} {
		if err := r.Reserve("repo", "run1", "web", "s1", port); !errors.Is(err, ErrInvalidPort) {
			t.Errorf("port %d: err = %v, want ErrInvalidPort", port, err)
		}
	}
	if got := r.Exposures("run1"); len(got) != 0 {
		t.Errorf("Exposures after rejected reserves = %v, want none", got)
	}
	if n := log.count(); n != 0 {
		t.Errorf("onChange calls after rejected reserves = %d, want 0", n)
	}
}

func TestReserveConflictAndReclaim(t *testing.T) {
	r, log := newTestRegistry(t)
	mustReserve(t, r, "repo", "run1", "web", 5173)
	if got := log.last(); got != "repo/run1" {
		t.Errorf("onChange after reserve = %q, want repo/run1", got)
	}

	// Another run cannot take the name, and the attempt changes nothing.
	before := log.count()
	if err := r.Reserve("repo2", "run2", "web", "s2", 3000); !errors.Is(err, ErrConflict) {
		t.Fatalf("other run: err = %v, want ErrConflict", err)
	}
	if log.count() != before {
		t.Errorf("a conflicting reserve fired onChange")
	}

	// The holding run re-reserves (a reconnect) and may move the port.
	mustReserve(t, r, "repo", "run1", "web", 5174)
	got := r.Exposures("run1")
	want := []Exposure{{Name: "web", URL: "https://web.lab.internal:8081", Port: 5174, Live: false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Exposures = %+v, want %+v", got, want)
	}
	if n := log.count(); n != before+1 {
		t.Errorf("onChange calls = %d, want %d (re-reserve announces itself)", n, before+1)
	}
}

func TestReserveLimit(t *testing.T) {
	r, _ := newTestRegistry(t)
	for i := range MaxPerRun {
		mustReserve(t, r, "repo", "run1", fmt.Sprintf("app%d", i), 3000+i)
	}
	if err := r.Reserve("repo", "run1", "one-too-many", "s1", 4000); !errors.Is(err, ErrLimit) {
		t.Fatalf("9th exposure: err = %v, want ErrLimit", err)
	}
	// Re-reserving a name the run already holds is not a new exposure.
	mustReserve(t, r, "repo", "run1", "app0", 3100)
	// The limit is per run: another run is unaffected.
	mustReserve(t, r, "repo", "run2", "other", 4000)
	if n := len(r.Exposures("run1")); n != MaxPerRun {
		t.Errorf("run1 holds %d exposures, want %d", n, MaxPerRun)
	}
}

func TestStop(t *testing.T) {
	r, log := newTestRegistry(t)
	if err := r.Stop("run1", "web"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Stop of a never-reserved name: err = %v, want ErrUnknown", err)
	}
	mustReserve(t, r, "repo", "run1", "web", 5173)
	if err := r.Stop("run2", "web"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Stop by another run: err = %v, want ErrUnknown", err)
	}

	before := log.count()
	if err := r.Stop("run1", "web"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if n := log.count(); n != before+1 || log.last() != "repo/run1" {
		t.Errorf("onChange after Stop: %d calls, last %q; want one more, repo/run1", n, log.last())
	}
	if got := r.Exposures("run1"); len(got) != 0 {
		t.Errorf("Exposures after Stop = %v, want none", got)
	}
	// The name is free: another run can take it now.
	mustReserve(t, r, "repo", "run2", "web", 3000)
	if err := r.Stop("run1", "web"); !errors.Is(err, ErrUnknown) {
		t.Errorf("second Stop: err = %v, want ErrUnknown", err)
	}
}

func TestCloseRun(t *testing.T) {
	r, log := newTestRegistry(t)
	mustReserve(t, r, "repo", "run1", "web", 5173)
	mustReserve(t, r, "repo", "run1", "api", 8080)
	mustReserve(t, r, "repo", "run2", "docs", 3000)

	before := log.count()
	r.CloseRun("run1")
	if n := log.count(); n != before+1 || log.last() != "repo/run1" {
		t.Errorf("onChange after CloseRun: %d calls, last %q; want one more, repo/run1", n, log.last())
	}
	if got := r.Exposures("run1"); len(got) != 0 {
		t.Errorf("run1 Exposures after CloseRun = %v, want none", got)
	}
	if got := r.Exposures("run2"); len(got) != 1 {
		t.Errorf("run2 Exposures after CloseRun(run1) = %v, want its one exposure untouched", got)
	}
	// Both names are free for another run.
	mustReserve(t, r, "repo", "run3", "web", 1)
	mustReserve(t, r, "repo", "run3", "api", 2)

	// Idempotent, and silent for a run with nothing.
	before = log.count()
	r.CloseRun("run1")
	r.CloseRun("never-existed")
	if log.count() != before {
		t.Errorf("CloseRun on a run holding nothing fired onChange")
	}
}

// TestStopTombstone (issue #105): `labctl expose stop web` landing while the
// foreground `labctl expose web` is between tunnels must stay stopped. That
// very process's reconnect — same run, same session — gets ErrStopped; a new
// `labctl expose` the agent starts on purpose (a different session) wins the
// name back; and any successful Reserve clears the tombstone.
func TestStopTombstone(t *testing.T) {
	r, log := newTestRegistry(t)
	if err := r.Reserve("repo", "run1", "web", "fg", 5173); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := r.Stop("run1", "web"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	before := log.count()
	if err := r.Reserve("repo", "run1", "web", "fg", 5173); !errors.Is(err, ErrStopped) {
		t.Fatalf("reconnect of the stopped session: err = %v, want ErrStopped", err)
	}
	if got := r.Exposures("run1"); len(got) != 0 {
		t.Errorf("Exposures after refused reconnect = %v, want none", got)
	}
	if log.count() != before {
		t.Errorf("a refused reconnect fired onChange")
	}
	// The tombstone is per name: the same session may hold another one.
	if err := r.Reserve("repo", "run1", "api", "fg", 8080); err != nil {
		t.Errorf("same session, other name: %v", err)
	}

	// A fresh `labctl expose web` (new session) is the agent asking again.
	if err := r.Reserve("repo", "run1", "web", "fresh", 5173); err != nil {
		t.Fatalf("new session after stop: %v", err)
	}
	// ...and that cleared the tombstone: the old session is now just a
	// second labctl of the same run re-claiming (it would replace on attach).
	if err := r.Reserve("repo", "run1", "web", "fg", 5173); err != nil {
		t.Errorf("old session after a new holder: %v, want the ordinary re-claim", err)
	}

	// A tombstone never blocks another run, and that run's claim clears it.
	if err := r.Stop("run1", "web"); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := r.Reserve("repo", "run2", "web", "fg", 3000); err != nil {
		t.Fatalf("other run after stop: %v", err)
	}
	if err := r.Stop("run2", "web"); err != nil {
		t.Fatalf("Stop by run2: %v", err)
	}
	if err := r.Reserve("repo", "run1", "web", "fg", 5173); err != nil {
		t.Errorf("run1's old session after run2 held and stopped the name: %v, want ok (run2's stop left run2's tombstone)", err)
	}

	// An empty session (a labctl predating sessions) is never tombstoned.
	if err := r.Reserve("repo", "run3", "legacy", "", 5173); err != nil {
		t.Fatalf("Reserve with no session: %v", err)
	}
	if err := r.Stop("run3", "legacy"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := r.Reserve("repo", "run3", "legacy", "", 5173); err != nil {
		t.Errorf("sessionless reconnect after stop: %v, want today's re-claim", err)
	}
}

// CloseRun drops the ending run's tombstones, so the map is bounded by the
// names live runs have stopped.
func TestCloseRunDropsTombstones(t *testing.T) {
	r, _ := newTestRegistry(t)
	for _, run := range []string{"run1", "run2"} {
		if err := r.Reserve("repo", run, "web-"+run, "fg", 5173); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if err := r.Stop(run, "web-"+run); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	r.CloseRun("run1")
	r.mu.Lock()
	_, kept := r.tombstones["web-run2"]
	n := len(r.tombstones)
	r.mu.Unlock()
	if n != 1 || !kept {
		t.Errorf("tombstones after CloseRun(run1) = %d (run2's kept: %v), want only run2's", n, kept)
	}
}

// TestReserveAfterCloseRun: the agent API authenticates the run token before
// Reserve, so a run may end in between. Its Reserve must not re-claim a name
// no run-end site will ever release again — it gets ErrRunEnded, even for a
// run that held nothing when it ended.
func TestReserveAfterCloseRun(t *testing.T) {
	r, log := newTestRegistry(t)
	mustReserve(t, r, "repo", "run1", "web", 5173)
	r.CloseRun("run1")
	r.CloseRun("run2") // held nothing

	before := log.count()
	for _, run := range []string{"run1", "run2"} {
		if err := r.Reserve("repo", run, "web", testSession(run), 5173); !errors.Is(err, ErrRunEnded) {
			t.Errorf("%s: Reserve after CloseRun = %v, want ErrRunEnded", run, err)
		}
		// Checked before anything else: an ended run learns nothing more.
		if err := r.Reserve("repo", run, "Bad.Name", testSession(run), 0); !errors.Is(err, ErrRunEnded) {
			t.Errorf("%s: invalid Reserve after CloseRun = %v, want ErrRunEnded", run, err)
		}
	}
	if got := r.Exposures("run1"); len(got) != 0 {
		t.Errorf("Exposures of the ended run = %v, want none", got)
	}
	if log.count() != before {
		t.Errorf("a refused Reserve fired onChange")
	}
	// Other runs are unaffected, including for the released name.
	mustReserve(t, r, "repo", "run3", "web", 5173)
}

// recordingCloser is a RunCloser fake recording every CloseRun.
type recordingCloser struct{ runs []string }

func (c *recordingCloser) CloseRun(runID string) { c.runs = append(c.runs, runID) }

// CloseRunExposures is the run-end sites' one call (issue #105): it forwards
// to a real closer, and is a silent no-op for the nil interface AND for a
// typed-nil *Registry smuggled into the interface — the nil-interface trap a
// lab without a preview listener must never fall into.
func TestCloseRunExposures(t *testing.T) {
	rec := &recordingCloser{}
	CloseRunExposures(rec, "run1")
	if len(rec.runs) != 1 || rec.runs[0] != "run1" {
		t.Errorf("CloseRunExposures forwarded %v, want [run1]", rec.runs)
	}

	r, _ := newTestRegistry(t)
	mustReserve(t, r, "repo", "run1", "web", 5173)
	CloseRunExposures(r, "run1")
	if got := r.Exposures("run1"); len(got) != 0 {
		t.Errorf("Exposures after CloseRunExposures(registry) = %v, want none", got)
	}

	CloseRunExposures(nil, "run1") // must not panic
	var typedNil *Registry
	var c RunCloser = typedNil
	CloseRunExposures(c, "run1") // must not panic either
}

func TestExposuresSortedNeverNil(t *testing.T) {
	r, _ := newTestRegistry(t)
	if got := r.Exposures("run1"); got == nil || len(got) != 0 {
		t.Errorf("Exposures of an empty run = %#v, want a non-nil empty slice", got)
	}
	for _, name := range []string{"web", "api", "docs"} {
		mustReserve(t, r, "repo", "run1", name, 3000)
	}
	var names []string
	for _, e := range r.Exposures("run1") {
		names = append(names, e.Name)
	}
	if got, want := strings.Join(names, ","), "api,docs,web"; got != want {
		t.Errorf("Exposures order = %s, want %s", got, want)
	}
}
