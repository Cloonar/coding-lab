package afk

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// The claimable observer (issue #61): every claimable count the engine or an
// operator view computes is handed to Options.OnClaimable, so the repo list
// can show a forge-bound repo's count without a forge request per repo.

// claimableLog is an OnClaimable recording what it was told.
type claimableLog struct{ got []string }

func (l *claimableLog) observe(repo store.Repo, count int) {
	l.got = append(l.got, fmt.Sprintf("%s=%d", repo.Name, count))
}

func (l *claimableLog) take() []string {
	got := l.got
	l.got = nil
	return got
}

// observeClaimable wires a log into the fixture's engine. The fixture builds
// its engine itself, so the test sets the field Options.OnClaimable lands in.
func observeClaimable(f *fixture) *claimableLog {
	log := &claimableLog{}
	f.svc.onClaimable = log.observe
	return log
}

func wantReports(t *testing.T, what string, log *claimableLog, want ...string) {
	t.Helper()
	if got := log.take(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: claimable reports = %v, want %v", what, got, want)
	}
}

// FilterClaimable is the one choke point of every claimable computation, so
// it is the one place a count is reported from: each success reports the size
// of its result, a failure reports nothing.
func TestFilterClaimable_reportsTheCount(t *testing.T) {
	f := newFixture(t)
	log := observeClaimable(f)
	f.trk.setReady(1, 2, 3)
	f.createClaimBranch(f.repo, "afk/2") // #2 is claimed

	ready, _ := f.trk.ReadyIssues(t.Context())
	claimable, err := f.svc.FilterClaimable(t.Context(), f.repo, f.trk, ready)
	if err != nil {
		t.Fatalf("FilterClaimable: %v", err)
	}
	if len(claimable) != 2 {
		t.Fatalf("claimable = %d issues, want 2", len(claimable))
	}
	wantReports(t, "three ready, one claimed", log, "proj=2")

	// An empty ready queue is a count too.
	if _, err := f.svc.FilterClaimable(t.Context(), f.repo, f.trk, nil); err != nil {
		t.Fatalf("FilterClaimable(empty): %v", err)
	}
	wantReports(t, "empty queue", log, "proj=0")

	// The blocked-by gate's open-set read fails: no count is known.
	f.trk.setReady()
	f.trk.setReadyIssue(87, "## Blocked by\n\n- #74\n")
	f.trk.failIssues(errors.New("forge is down"))
	ready, _ = f.trk.ReadyIssues(t.Context())
	if _, err := f.svc.FilterClaimable(t.Context(), f.repo, f.trk, ready); err == nil {
		t.Fatal("FilterClaimable succeeded despite the failed open-set read")
	}
	wantReports(t, "failed filter", log)

	// The hint path the scheduler uses goes through the same choke point.
	f.trk.failIssues(nil)
	f.trk.setOpen(74) // #74 is still open: #87 is blocked
	if _, err := f.svc.ClaimableIssuesFor(t.Context(), f.repo); err != nil {
		t.Fatalf("ClaimableIssuesFor: %v", err)
	}
	wantReports(t, "blocked issue", log, "proj=0")
}

// The locked claim path knows the count at two more moments no filter call
// covers: an empty ready queue (it returns before filtering) and right after
// it claimed one issue of the set it picked from.
func TestLaunch_reportsEmptyQueueAndTheClaim(t *testing.T) {
	f := newFixture(t)
	log := observeClaimable(f)

	if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); !errors.Is(err, ErrNoReady) {
		t.Fatalf("StartManualAFK on an empty queue = %v, want ErrNoReady", err)
	}
	wantReports(t, "empty ready queue", log, "proj=0")

	f.trk.setReady(5, 6, 7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if run.IssueNumber == nil || *run.IssueNumber != 5 {
		t.Fatalf("claimed issue = %v, want #5", run.IssueNumber)
	}
	// Three were claimable when it picked; it took one.
	wantReports(t, "launch", log, "proj=3", "proj=2")

	// The next computation agrees with what the launch already reported.
	if _, err := f.svc.ClaimableIssuesFor(t.Context(), f.repo); err != nil {
		t.Fatal(err)
	}
	wantReports(t, "after the claim", log, "proj=2")
}

// The spawn pass reports through the same path: an auto-enabled repo's count
// is remembered on every pass, which is what keeps the repo list's number
// current without the list asking anyone.
func TestSpawnOnce_reportsClaimable(t *testing.T) {
	f := newFixture(t)
	log := observeClaimable(f)
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{AFKAutoEnabled: store.Set(true)}); err != nil {
		t.Fatal(err)
	}
	f.trk.setReady(3, 4)

	f.svc.SpawnOnce(t.Context())
	// The gather's hint (2), the locked claim path's own filter (2), then the
	// claim (1).
	wantReports(t, "first pass", log, "proj=2", "proj=2", "proj=1")

	// Second pass: the auto run is in flight, so nothing launches — but the
	// gather still computed, and reported, the count.
	f.svc.SpawnOnce(t.Context())
	wantReports(t, "second pass", log, "proj=1")
}

// LocalClaimableCount counts a builtin-bound repo from scratch, through the
// same two gates, WITHOUT reporting (a page view must not announce a change
// to the pages that are reading) — and refuses a forge-bound repo outright
// instead of reading its tracker.
func TestLocalClaimableCount(t *testing.T) {
	f := newFixture(t)
	log := observeClaimable(f)
	ctx := t.Context()

	// Empty ready queue: zero, and no claim-branch read at all — proven by
	// taking the reference repo away.
	bare := f.bare(f.repo)
	if err := os.Rename(bare, bare+".away"); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.LocalClaimableCount(ctx, f.repo); err != nil || n != 0 {
		t.Fatalf("empty queue without a reference repo = %d, %v; want 0, nil", n, err)
	}
	// With a ready issue the claim refs ARE needed, and their absence is an
	// error — an unknown count, never a guessed one.
	f.trk.setReady(1)
	if _, err := f.svc.LocalClaimableCount(ctx, f.repo); err == nil {
		t.Fatal("LocalClaimableCount succeeded without a reference repo to read claims from")
	}
	if err := os.Rename(bare+".away", bare); err != nil {
		t.Fatal(err)
	}

	// Both gates: #2 is claimed, #87 is blocked by the still-open #74.
	f.trk.setReady(1, 2, 3)
	f.trk.setReadyIssue(87, "## Blocked by\n\n- #74\n")
	f.trk.setOpen(74)
	f.createClaimBranch(f.repo, "afk/2")
	n, err := f.svc.LocalClaimableCount(ctx, f.repo)
	if err != nil || n != 2 {
		t.Fatalf("LocalClaimableCount = %d, %v; want 2 (#1 and #3)", n, err)
	}
	// It agrees with the reporting path on the number…
	claimable, err := f.svc.ClaimableIssuesFor(ctx, f.repo)
	if err != nil || len(claimable) != n {
		t.Fatalf("ClaimableIssuesFor = %d issues, %v; LocalClaimableCount said %d", len(claimable), err, n)
	}
	// …and is the only one of the two that stayed silent.
	wantReports(t, "local counts report nothing", log, "proj=2")

	// A forge-bound repo is refused before its tracker is asked anything: the
	// scripted read error would surface if it were.
	forge, forgeTrk := f.addRepo("forged", "afk/<N>")
	forge.TrackerBinding = store.TrackerBindingForge
	forgeTrk.readyErr = errors.New("the forge was asked")
	if _, err := f.svc.LocalClaimableCount(ctx, forge); !errors.Is(err, ErrNotLocalTracker) {
		t.Fatalf("LocalClaimableCount(forge-bound) = %v, want ErrNotLocalTracker", err)
	}
	wantReports(t, "refused forge-bound count", log)
}

// Without an observer the engine behaves exactly as before.
func TestClaimable_noObserver(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(1)
	if _, err := f.svc.ClaimableIssuesFor(t.Context(), f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
}
