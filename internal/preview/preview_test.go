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

// mustReserve asserts Reserve succeeds.
func mustReserve(t *testing.T, r *Registry, repoID, runID, name string, port int) {
	t.Helper()
	if err := r.Reserve(repoID, runID, name, port); err != nil {
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
		{80, "http://web.lab.internal"},
		{0, "http://web.lab.internal"},
		{8081, "http://web.lab.internal:8081"},
	} {
		r := NewRegistry("lab.internal", tc.port, nil)
		if got := r.URL("web"); got != tc.want {
			t.Errorf("port %d: URL = %q, want %q", tc.port, got, tc.want)
		}
	}
}

func TestReserveValidates(t *testing.T) {
	r, log := newTestRegistry(t)
	if err := r.Reserve("repo", "run1", "Web", 5173); !errors.Is(err, ErrInvalidName) {
		t.Errorf("uppercase name: err = %v, want ErrInvalidName", err)
	}
	for _, port := range []int{0, -1, 65536} {
		if err := r.Reserve("repo", "run1", "web", port); !errors.Is(err, ErrInvalidPort) {
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
	if err := r.Reserve("repo2", "run2", "web", 3000); !errors.Is(err, ErrConflict) {
		t.Fatalf("other run: err = %v, want ErrConflict", err)
	}
	if log.count() != before {
		t.Errorf("a conflicting reserve fired onChange")
	}

	// The holding run re-reserves (a reconnect) and may move the port.
	mustReserve(t, r, "repo", "run1", "web", 5174)
	got := r.Exposures("run1")
	want := []Exposure{{Name: "web", URL: "http://web.lab.internal:8081", Port: 5174, Live: false}}
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
	if err := r.Reserve("repo", "run1", "one-too-many", 4000); !errors.Is(err, ErrLimit) {
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
