package readiness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// recorderFixture is a Recorder on a real bus with a subscriber collecting
// what it publishes. Bus.Publish delivers into the subscriber's buffered
// channel before it returns, so the events of every Observe* call are
// readable — without a timing window — the moment the call is back.
type recorderFixture struct {
	t   *testing.T
	rec *Recorder
	ch  <-chan events.Event
	now time.Time
}

func newRecorderFixture(t *testing.T) *recorderFixture {
	t.Helper()
	bus := events.NewBus()
	ch, cancel := bus.Subscribe(context.Background())
	t.Cleanup(cancel)
	f := &recorderFixture{t: t, ch: ch, now: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	f.rec = NewRecorder(bus, func() time.Time { return f.now })
	return f
}

// published drains the events published since the last call and returns the
// repo ids of the repo.changed ones, in order. Anything else on the bus is a
// failure: the recorder publishes nothing but repo.changed.
func (f *recorderFixture) published() []string {
	f.t.Helper()
	var ids []string
	for {
		select {
		case e := <-f.ch:
			p, ok := e.Payload.(repoChangedPayload)
			if e.Type != EventRepoChanged || !ok || p.Type != EventRepoChanged {
				f.t.Fatalf("published %q with payload %#v, want repo.changed", e.Type, e.Payload)
			}
			ids = append(ids, p.RepoID)
		default:
			return ids
		}
	}
}

func (f *recorderFixture) wantPublished(what string, want ...string) {
	f.t.Helper()
	got := f.published()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		f.t.Fatalf("%s: published repo.changed for %v, want %v", what, got, want)
	}
}

var errFetch = errors.New("git fetch origin: exit status 128: fatal: Authentication failed")

// trackerRead returns a repo's recorded read of one kind.
func (f *recorderFixture) trackerRead(repoID, op string) (TrackerRecord, bool) {
	f.t.Helper()
	return readOf(f.rec, repoID, op)
}

func readOf(rec *Recorder, repoID, op string) (TrackerRecord, bool) {
	for _, r := range rec.TrackerReads(repoID) {
		if r.Op == op {
			return r, true
		}
	}
	return TrackerRecord{}, false
}

// A fetch verdict is announced when it flips, not when it repeats — the
// whole difference between "publish, don't poll" and an event storm.
func TestRecorderFetch_PublishesOnFlipOnly(t *testing.T) {
	f := newRecorderFixture(t)
	a := gitx.FetchAttribution{RepoID: "repo_a", Credential: "cred@1"}

	f.rec.ObserveFetch(a, errFetch)
	f.wantPublished("first record after none", "repo_a")
	rec, ok := f.rec.Fetch("repo_a")
	if !ok || rec.OK || rec.Error != errFetch.Error() || rec.Credential != "cred@1" || !rec.At.Equal(f.now) {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}

	// The same verdict again — and again — is silent, however often lab fetches.
	for range 5 {
		f.rec.ObserveFetch(a, errFetch)
	}
	f.wantPublished("repeated failure")

	// A different error message is still the same verdict.
	f.rec.ObserveFetch(a, errors.New("git fetch origin: exit status 128: fatal: something else"))
	f.wantPublished("failure with another message")

	f.rec.ObserveFetch(a, nil)
	f.wantPublished("failed → succeeded", "repo_a")
	if rec, _ := f.rec.Fetch("repo_a"); !rec.OK || rec.Error != "" {
		t.Fatalf("record after success = %+v", rec)
	}
	f.rec.ObserveFetch(a, nil)
	f.wantPublished("repeated success")

	f.rec.ObserveFetch(a, errFetch)
	f.wantPublished("succeeded → failed", "repo_a")

	// The same verdict under ANOTHER credential version is news: the previous
	// record was stale for the repo as now configured, so its check was
	// absent and now it is not.
	a.Credential = "cred@2"
	f.rec.ObserveFetch(a, errFetch)
	f.wantPublished("same verdict, new credential version", "repo_a")
}

// A read-only import's fetch concerns two repos: the target whose reference
// repo it is, and the importing repo whose spawn it decided.
func TestRecorderFetch_OnBehalfOfPublishesBothRepos(t *testing.T) {
	f := newRecorderFixture(t)
	a := gitx.FetchAttribution{RepoID: "repo_target", Credential: store.NoCredentialStamp, OnBehalfOf: "repo_importer"}
	f.rec.ObserveFetch(a, errFetch)
	f.wantPublished("import fetch failed", "repo_target", "repo_importer")
	if _, ok := f.rec.Fetch("repo_importer"); ok {
		t.Fatal("the importing repo got a fetch record of its own")
	}
	if rec, ok := f.rec.Fetch("repo_target"); !ok || rec.OK {
		t.Fatalf("target record = %+v ok=%v", rec, ok)
	}
	// A repo fetching itself on its own behalf is announced once.
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: "repo_x", OnBehalfOf: "repo_x"}, nil)
	f.wantPublished("self", "repo_x")
}

// A fetch that was cancelled says nothing about the remote: it leaves no
// record and a previous verdict stands.
func TestRecorderFetch_CancelledIsNotAFailure(t *testing.T) {
	f := newRecorderFixture(t)
	a := gitx.FetchAttribution{RepoID: "repo_a", Credential: "cred@1"}

	f.rec.ObserveFetch(a, fmt.Errorf("git fetch origin: %w", context.Canceled))
	f.wantPublished("cancelled, nothing recorded before")
	if _, ok := f.rec.Fetch("repo_a"); ok {
		t.Fatal("a cancelled fetch left a record")
	}

	f.rec.ObserveFetch(a, nil)
	f.published()
	f.rec.ObserveFetch(a, fmt.Errorf("git fetch origin: %w", context.Canceled))
	f.wantPublished("cancelled after a success")
	if rec, _ := f.rec.Fetch("repo_a"); !rec.OK {
		t.Fatalf("a cancelled fetch overwrote the success: %+v", rec)
	}

	// Without a repo there is nothing to attribute the outcome to.
	f.rec.ObserveFetch(gitx.FetchAttribution{}, errFetch)
	f.wantPublished("no repo id")
}

// A tracker read verdict flips exactly once per flip, whatever the engine's
// read cadence.
func TestRecorderTrackerRead_FlipPublishesExactlyOnce(t *testing.T) {
	f := newRecorderFixture(t)
	read := func(err error) tracker.ListRead {
		return tracker.ListRead{RepoID: "repo_a", Credential: "forge@1", Op: tracker.OpReadyIssues, Err: err, OpenIssues: -1}
	}
	unauthorized := errors.New("forgejo GET /repos/acme/widget/issues: unexpected status 401: bad token")

	f.rec.ObserveTrackerRead(read(nil))
	f.wantPublished("first read", "repo_a")
	for range 10 { // the spawn pass, every 45 seconds, forever
		f.rec.ObserveTrackerRead(read(nil))
	}
	f.wantPublished("ten more successful reads")

	f.rec.ObserveTrackerRead(read(unauthorized))
	f.wantPublished("succeeded → failed", "repo_a")
	rec, ok := f.trackerRead("repo_a", tracker.OpReadyIssues)
	if !ok || rec.OK || rec.NotFound || rec.Error != unauthorized.Error() || rec.Op != tracker.OpReadyIssues || rec.Credential != "forge@1" {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
	for range 10 {
		f.rec.ObserveTrackerRead(read(unauthorized))
	}
	f.wantPublished("ten more failed reads")

	f.rec.ObserveTrackerRead(read(nil))
	f.wantPublished("failed → succeeded", "repo_a")

	// A forge 404 on a list read is a failure, and flagged as "not found".
	notFound := fmt.Errorf("forgejo GET /repos/acme/widget/issues: unexpected status 404: %w", tracker.ErrNotFound)
	f.rec.ObserveTrackerRead(read(notFound))
	f.wantPublished("succeeded → not found", "repo_a")
	if rec, _ := f.trackerRead("repo_a", tracker.OpReadyIssues); rec.OK || !rec.NotFound {
		t.Fatalf("404 record = %+v", rec)
	}

	// Another forge credential version: the verdict is news again.
	next := read(notFound)
	next.Credential = "forge@2"
	f.rec.ObserveTrackerRead(next)
	f.wantPublished("same verdict, new credential version", "repo_a")
}

// A rate-limited or cancelled read is evidence of nothing: no record, and a
// previous verdict stands.
func TestRecorderTrackerRead_RateLimitAndCancelLeaveTheRecordAlone(t *testing.T) {
	f := newRecorderFixture(t)
	read := func(err error) tracker.ListRead {
		return tracker.ListRead{RepoID: "repo_a", Credential: "forge@1", Op: tracker.OpIssues, Err: err, OpenIssues: -1}
	}
	limited := fmt.Errorf("github GET /repos/a/b/issues: 403: %w", tracker.ErrRateLimited)
	cancelled := fmt.Errorf("forgejo GET /x: %w", context.Canceled)

	f.rec.ObserveTrackerRead(read(limited))
	f.rec.ObserveTrackerRead(read(cancelled))
	f.wantPublished("nothing recorded before")
	if recs := f.rec.TrackerReads("repo_a"); recs != nil {
		t.Fatal("a rate-limited or cancelled read left a record")
	}

	f.rec.ObserveTrackerRead(read(nil))
	f.published()
	f.rec.ObserveTrackerRead(read(limited))
	f.rec.ObserveTrackerRead(read(cancelled))
	f.wantPublished("after a success")
	if rec, _ := f.trackerRead("repo_a", tracker.OpIssues); !rec.OK {
		t.Fatalf("the success was overwritten: %+v", rec)
	}
}

// The open issue count rides a successful open-set read and is announced
// when its value changes.
func TestRecorderOpenIssues_PublishesOnChange(t *testing.T) {
	f := newRecorderFixture(t)
	read := func(n int, err error) tracker.ListRead {
		return tracker.ListRead{RepoID: "repo_a", Credential: "forge@1", Op: tracker.OpIssues, Err: err, OpenIssues: n}
	}
	if _, ok := f.rec.OpenIssues("repo_a"); ok {
		t.Fatal("a count is known before any read")
	}

	f.rec.ObserveTrackerRead(read(7, nil))
	f.wantPublished("first read with a count", "repo_a")
	if n, ok := f.rec.OpenIssues("repo_a"); !ok || n != 7 {
		t.Fatalf("open issues = %d ok=%v, want 7", n, ok)
	}
	f.rec.ObserveTrackerRead(read(7, nil))
	f.wantPublished("same count")
	// A read that carries no count (ready queue, pulls) leaves it alone.
	f.rec.ObserveTrackerRead(read(-1, nil))
	f.wantPublished("a read without a count")
	if n, _ := f.rec.OpenIssues("repo_a"); n != 7 {
		t.Fatalf("open issues = %d after a count-less read, want 7", n)
	}

	f.rec.ObserveTrackerRead(read(8, nil))
	f.wantPublished("count changed", "repo_a")
	f.rec.ObserveTrackerRead(read(0, nil))
	f.wantPublished("count dropped to zero", "repo_a")
	if n, ok := f.rec.OpenIssues("repo_a"); !ok || n != 0 {
		t.Fatalf("open issues = %d ok=%v, want a known 0", n, ok)
	}

	// A failed read keeps the last known count: it is "as last read".
	f.rec.ObserveTrackerRead(read(99, errors.New("502")))
	f.wantPublished("read failed", "repo_a") // the verdict flipped; the count did not move
	if n, _ := f.rec.OpenIssues("repo_a"); n != 0 {
		t.Fatalf("open issues = %d after a failed read, want the last known 0", n)
	}
}

// The claimable count is announced when its value changes, and is only
// served for the tracker binding it was computed under.
func TestRecorderClaimable(t *testing.T) {
	f := newRecorderFixture(t)
	forge := store.Repo{ID: "repo_a", TrackerBinding: store.TrackerBindingForge}
	if _, ok := f.rec.Claimable("repo_a", store.TrackerBindingForge); ok {
		t.Fatal("a count is known before any computation")
	}

	f.rec.ObserveClaimable(forge, 3)
	f.wantPublished("first count", "repo_a")
	for range 10 {
		f.rec.ObserveClaimable(forge, 3)
	}
	f.wantPublished("same count, ten passes")
	f.rec.ObserveClaimable(forge, 2)
	f.wantPublished("count changed", "repo_a")
	f.rec.ObserveClaimable(forge, 0)
	f.wantPublished("queue drained", "repo_a")
	if n, ok := f.rec.Claimable("repo_a", store.TrackerBindingForge); !ok || n != 0 {
		t.Fatalf("claimable = %d ok=%v, want a known 0", n, ok)
	}

	// Rebinding the repo makes the old count another tracker's.
	if _, ok := f.rec.Claimable("repo_a", store.TrackerBindingBuiltin); ok {
		t.Fatal("a forge-era count was served for the builtin binding")
	}
	builtin := forge
	builtin.TrackerBinding = store.TrackerBindingBuiltin
	f.rec.ObserveClaimable(builtin, 0)
	f.wantPublished("same number under another binding", "repo_a")

	f.rec.ObserveClaimable(store.Repo{}, 4)
	f.rec.ObserveClaimable(forge, -1)
	f.wantPublished("no repo id / negative count")
}

// A dev image's pull-if-missing outcome is keyed by its ref and announced
// for the repo whose spawn observed it.
func TestRecorderImage(t *testing.T) {
	f := newRecorderFixture(t)
	const ref = "ghcr.io/acme/dev:1@sha256:0123"
	pull := errors.New("pulling dev image " + ref + ": exit status 125: manifest unknown")

	f.rec.ObserveImage("repo_a", ref, pull)
	f.wantPublished("first ensure", "repo_a")
	f.rec.ObserveImage("repo_a", ref, pull)
	f.wantPublished("same verdict")
	// Another repo resolving to the same ref shares the record: no flip, no event.
	f.rec.ObserveImage("repo_b", ref, pull)
	f.wantPublished("same ref from another repo")
	if rec, ok := f.rec.Image(ref); !ok || rec.OK || rec.Error != pull.Error() {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}

	f.rec.ObserveImage("repo_b", ref, nil)
	f.wantPublished("failed → present", "repo_b")
	if rec, _ := f.rec.Image(ref); !rec.OK {
		t.Fatalf("record after a successful ensure = %+v", rec)
	}
	f.rec.ObserveImage("repo_a", ref, context.Canceled)
	f.wantPublished("cancelled ensure")
	if rec, _ := f.rec.Image(ref); !rec.OK {
		t.Fatal("a cancelled ensure overwrote the record")
	}
	if _, ok := f.rec.Image("another/ref"); ok {
		t.Fatal("a record exists for a ref never ensured")
	}
}

// Forget drops a deleted repo's records; Announce records nothing.
func TestRecorderForgetAndAnnounce(t *testing.T) {
	f := newRecorderFixture(t)
	repo := store.Repo{ID: "repo_a", TrackerBinding: store.TrackerBindingForge}
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: "repo_a"}, nil)
	f.rec.ObserveTrackerRead(tracker.ListRead{RepoID: "repo_a", Op: tracker.OpIssues, OpenIssues: 4})
	f.rec.ObserveClaimable(repo, 2)
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: "repo_b"}, nil)
	f.published()

	f.rec.Forget("repo_a")
	f.wantPublished("forget is silent")
	if _, ok := f.rec.Fetch("repo_a"); ok {
		t.Error("fetch record survived Forget")
	}
	if recs := f.rec.TrackerReads("repo_a"); recs != nil {
		t.Error("tracker record survived Forget")
	}
	if _, ok := f.rec.Claimable("repo_a", store.TrackerBindingForge); ok {
		t.Error("claimable count survived Forget")
	}
	if _, ok := f.rec.OpenIssues("repo_a"); ok {
		t.Error("open issue count survived Forget")
	}
	if _, ok := f.rec.Fetch("repo_b"); !ok {
		t.Error("Forget dropped another repo's record")
	}

	f.rec.Announce("repo_x", "", "repo_y", "repo_x")
	f.wantPublished("announce", "repo_x", "repo_y")
	if _, ok := f.rec.Fetch("repo_x"); ok {
		t.Error("Announce recorded something")
	}
}

// A recorder without a bus records and stays silent (tests, degraded wiring).
func TestRecorderWithoutBus(t *testing.T) {
	rec := NewRecorder(nil, nil)
	rec.ObserveFetch(gitx.FetchAttribution{RepoID: "repo_a"}, nil)
	rec.Announce("repo_a")
	if r, ok := rec.Fetch("repo_a"); !ok || !r.OK || r.At.IsZero() {
		t.Fatalf("record = %+v ok=%v", r, ok)
	}
}

// A record keeps a bounded slice of the error text — the end of a git or
// podman transcript, the start of a forge client's message — so a chatty
// failure cannot grow the recorder, and the line a report quotes survives.
func TestRecorderBoundsErrorText(t *testing.T) {
	rec := NewRecorder(nil, nil)
	noise := strings.Repeat("remote: noise\n", 2000)

	rec.ObserveFetch(gitx.FetchAttribution{RepoID: "repo_a"}, errors.New("git fetch origin: exit status 128: "+noise+"fatal: the verdict"))
	f, _ := rec.Fetch("repo_a")
	if len(f.Error) != maxErrorBytes || !strings.HasSuffix(f.Error, "fatal: the verdict") {
		t.Fatalf("fetch error kept %d bytes ending %q; want the last %d", len(f.Error), f.Error[len(f.Error)-20:], maxErrorBytes)
	}
	if got := gitReason(f.Error); got != "the verdict" {
		t.Fatalf("gitReason over the bounded text = %q", got)
	}

	rec.ObserveImage("repo_a", "ref", errors.New("pulling dev image ref: "+noise+"Error: manifest unknown"))
	if img, _ := rec.Image("ref"); len(img.Error) != maxErrorBytes || !strings.HasSuffix(img.Error, "Error: manifest unknown") {
		t.Fatalf("image error kept %d bytes", len(img.Error))
	}

	rec.ObserveTrackerRead(tracker.ListRead{RepoID: "repo_a", Op: tracker.OpIssues, OpenIssues: -1,
		Err: errors.New("forgejo GET /repos/a/b/issues: unexpected status 500: " + strings.Repeat("x", 3*maxErrorBytes))})
	if tr, _ := readOf(rec, "repo_a", tracker.OpIssues); len(tr.Error) != maxErrorBytes || !strings.HasPrefix(tr.Error, "forgejo GET /repos/a/b/issues: unexpected status 500: ") {
		t.Fatalf("tracker error kept %d bytes starting %q", len(tr.Error), tr.Error[:40])
	}
}

// A forge repository can answer one listing and refuse another for good —
// issues enabled, pull requests disabled. The engine reads both on its own
// cadences; with one record per KIND of read the verdict is stable (failing,
// for as long as one kind fails) and is announced once — not flipped, and
// re-announced, by whichever read happened last.
func TestRecorderTrackerRead_KindsOfReadDoNotFlap(t *testing.T) {
	f := newRecorderFixture(t)
	read := func(op string, err error) tracker.ListRead {
		return tracker.ListRead{RepoID: "repo_a", Credential: "forge@1", Op: op, Err: err, OpenIssues: -1}
	}
	pullsOff := fmt.Errorf("forgejo GET /repos/a/b/pulls: unexpected status 404: %w", tracker.ErrNotFound)

	f.rec.ObserveTrackerRead(read(tracker.OpReadyIssues, nil))
	f.wantPublished("ready queue read", "repo_a")
	f.rec.ObserveTrackerRead(read(tracker.OpPullsForHead, pullsOff))
	f.wantPublished("the pull listing fails", "repo_a") // passing → failing

	// The spawn pass and the reaper keep alternating, forever.
	for i := range 20 {
		f.now = f.now.Add(15 * time.Second)
		if i%2 == 0 {
			f.rec.ObserveTrackerRead(read(tracker.OpReadyIssues, nil))
		} else {
			f.rec.ObserveTrackerRead(read(tracker.OpPullsForHead, pullsOff))
		}
	}
	f.wantPublished("twenty alternating reads")
	failed, ok := JudgeTrackerReads(f.rec.TrackerReads("repo_a"), "forge@1")
	if failed == nil || failed.Op != tracker.OpPullsForHead || !ok {
		t.Fatalf("verdict = failed %+v, ok %v; want the pull listing failing beside a working issue listing", failed, ok)
	}

	// The pull listing starts answering: failing → passing, once.
	f.rec.ObserveTrackerRead(read(tracker.OpPullsForHead, nil))
	f.wantPublished("the pull listing recovers", "repo_a")
	if failed, ok := JudgeTrackerReads(f.rec.TrackerReads("repo_a"), "forge@1"); failed != nil || !ok {
		t.Fatalf("verdict after recovery = failed %+v, ok %v", failed, ok)
	}

	// The forge credential is rotated. The first read with the new version
	// is news (nothing was known about it); the old version's records no
	// longer count for it.
	rotated := read(tracker.OpReadyIssues, nil)
	rotated.Credential = "forge@2"
	f.rec.ObserveTrackerRead(rotated)
	f.wantPublished("first read with the rotated credential", "repo_a")
	if failed, ok := JudgeTrackerReads(f.rec.TrackerReads("repo_a"), "forge@2"); failed != nil || !ok {
		t.Fatalf("verdict under the rotated credential = failed %+v, ok %v", failed, ok)
	}
	if _, ok := JudgeTrackerReads(f.rec.TrackerReads("repo_a"), ""); ok {
		t.Fatal("an unreadable current credential matched a record")
	}
	// Reads come back ordered by kind.
	var ops []string
	for _, r := range f.rec.TrackerReads("repo_a") {
		ops = append(ops, r.Op)
	}
	if !slices.IsSorted(ops) || len(ops) != 2 {
		t.Fatalf("TrackerReads ops = %v, want the two kinds in order", ops)
	}
}
