package readiness

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// forgeErr is a forge client's status error as the REST clients build it:
// the diagnostic line as its text, the classification sentinel (or nil) as
// what it unwraps to.
type forgeErr struct {
	msg string
	is  error
}

func (e forgeErr) Error() string { return e.msg }
func (e forgeErr) Unwrap() error { return e.is }

// refused is a forge's 401/403 refusal of the token — a definitive failure.
func refused(msg string) error { return forgeErr{msg: msg, is: tracker.ErrAccessDenied} }

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
	unauthorized := refused("forgejo GET /repos/acme/widget/issues: unexpected status 401: bad token")

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

	// A refused read keeps the last known count: it is "as last read".
	f.rec.ObserveTrackerRead(read(99, refused("forgejo GET /repos/a/b/issues: unexpected status 401: bad token")))
	f.wantPublished("read refused", "repo_a") // the verdict flipped; the count did not move
	if n, _ := f.rec.OpenIssues("repo_a"); n != 0 {
		t.Fatalf("open issues = %d after a refused read, want the last known 0", n)
	}
	// So does a read the forge failed to answer — which is not even news.
	f.rec.ObserveTrackerRead(read(99, errors.New("forgejo GET /repos/a/b/issues: unexpected status 502: bad gateway")))
	f.wantPublished("read failed upstream")
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
		Err: refused("forgejo GET /repos/a/b/issues: unexpected status 403: " + strings.Repeat("x", 3*maxErrorBytes))})
	if tr, _ := readOf(rec, "repo_a", tracker.OpIssues); len(tr.Error) != maxErrorBytes || !strings.HasPrefix(tr.Error, "forgejo GET /repos/a/b/issues: unexpected status 403: ") {
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

// Only a DEFINITIVE answer is evidence about the forge credential or the
// repository: a success, a 404, a 401 or an unthrottled 403. Everything a
// forge does when it merely fails to answer — a 5xx, a dropped connection, a
// client timeout, a rate limit, a cancelled request, a body that would not
// decode — is not, and is classified by trackerEvidence alone.
func TestTrackerEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"success", nil, true},
		{"404", forgeErr{"forgejo GET /x: unexpected status 404: nope", tracker.ErrNotFound}, true},
		{"401", refused("github GET /x: unexpected status 401: Bad credentials"), true},
		{"403", refused("github GET /x: unexpected status 403: Resource not accessible by integration"), true},
		{"wrapped 404", fmt.Errorf("listing: %w", forgeErr{"forgejo GET /x: unexpected status 404", tracker.ErrNotFound}), true},
		{"502", forgeErr{"github GET /x: unexpected status 502: bad gateway", nil}, false},
		{"500", errors.New("forgejo GET /x: unexpected status 500: internal error"), false},
		{"network", &url.Error{Op: "Get", URL: "https://forge.test/x", Err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused")}, false},
		{"client timeout", fmt.Errorf("forgejo GET /x: %w", context.DeadlineExceeded), false},
		{"rate limited", forgeErr{"github GET /x: rate limited (status 403, retry after 30s)", tracker.ErrRateLimited}, false},
		{"cancelled", fmt.Errorf("forgejo GET /x: %w", context.Canceled), false},
		{"undecodable body", errors.New("github GET /x: decode response: unexpected EOF"), false},
	} {
		if got := trackerEvidence(tc.err); got != tc.want {
			t.Errorf("%s: trackerEvidence(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// At the recorder seam: an answer that is not evidence leaves no record, does
// not overwrite a success, does not overwrite a definitive failure, and is
// never announced. A definitive failure is recorded per kind of read and
// fails the check until that kind is read successfully again.
func TestRecorderTrackerRead_OnlyDefinitiveOutcomesAreEvidence(t *testing.T) {
	noise := []error{
		forgeErr{"github GET /repos/a/b/issues: unexpected status 502: bad gateway", nil},
		errors.New("forgejo GET /repos/a/b/issues: unexpected status 503: maintenance"),
		&url.Error{Op: "Get", URL: "https://forge.test/api/v1/repos/a/b/issues", Err: errors.New("dial tcp: lookup forge.test: no such host")},
		fmt.Errorf("forgejo GET /repos/a/b/issues: %w", context.DeadlineExceeded),
		forgeErr{"github GET /repos/a/b/issues: rate limited (status 403, retry after 30s)", tracker.ErrRateLimited},
		fmt.Errorf("forgejo GET /repos/a/b/issues: %w", context.Canceled),
	}
	f := newRecorderFixture(t)
	read := func(op string, err error) tracker.ListRead {
		return tracker.ListRead{RepoID: "repo_a", Credential: "forge@1", Op: op, Err: err, OpenIssues: -1}
	}
	verdict := func() (failed *TrackerRecord, ok bool) {
		return JudgeTrackerReads(f.rec.TrackerReads("repo_a"), "forge@1")
	}

	// Nothing known: the noise leaves nothing known.
	for _, err := range noise {
		f.rec.ObserveTrackerRead(read(tracker.OpIssues, err))
	}
	f.wantPublished("noise before any record")
	if recs := f.rec.TrackerReads("repo_a"); recs != nil {
		t.Fatalf("noise left records: %+v", recs)
	}

	// Passing: the noise neither overwrites it nor announces anything — the
	// one 502 on the operator's Issues view is the case in point.
	f.rec.ObserveTrackerRead(read(tracker.OpIssues, nil))
	f.rec.ObserveTrackerRead(read(tracker.OpReadyIssues, nil))
	f.wantPublished("the first successes", "repo_a")
	for _, err := range noise {
		f.rec.ObserveTrackerRead(read(tracker.OpIssues, err))
	}
	f.wantPublished("noise after a success")
	if failed, ok := verdict(); failed != nil || !ok {
		t.Fatalf("verdict after noise = failed %+v, ok %v; want still passing", failed, ok)
	}
	if rec, _ := f.trackerRead("repo_a", tracker.OpIssues); !rec.OK {
		t.Fatalf("the success was overwritten: %+v", rec)
	}

	// A definitive refusal of ONE kind of read fails the check, for good —
	// the other kind still succeeding does not hide it — and the noise does
	// not overwrite it either.
	denied := refused("forgejo GET /repos/a/b/pulls: unexpected status 403: token lacks read:repository")
	f.rec.ObserveTrackerRead(read(tracker.OpPulls, denied))
	f.wantPublished("a kind of read refused", "repo_a")
	for _, err := range noise {
		f.rec.ObserveTrackerRead(read(tracker.OpPulls, err))
		f.rec.ObserveTrackerRead(read(tracker.OpIssues, nil))
	}
	f.wantPublished("noise and successes beside a refused kind")
	failed, ok := verdict()
	if failed == nil || failed.Op != tracker.OpPulls || failed.Error != denied.Error() || !ok {
		t.Fatalf("verdict = failed %+v, ok %v; want the refused pull listing failing", failed, ok)
	}

	// 401 and 404 are definitive too.
	for _, err := range []error{
		refused("forgejo GET /repos/a/b/issues: unexpected status 401: invalid token"),
		forgeErr{"forgejo GET /repos/a/b/issues: unexpected status 404: not found", tracker.ErrNotFound},
	} {
		f.rec.ObserveTrackerRead(read(tracker.OpIssues, err))
		if rec, _ := f.trackerRead("repo_a", tracker.OpIssues); rec.OK || rec.Error != err.Error() {
			t.Fatalf("a definitive failure %v was not recorded: %+v", err, rec)
		}
	}

	// The refused kind answers again: passing.
	f.rec.ObserveTrackerRead(read(tracker.OpPulls, nil))
	f.rec.ObserveTrackerRead(read(tracker.OpIssues, nil))
	if failed, ok := verdict(); failed != nil || !ok {
		t.Fatalf("verdict after recovery = failed %+v, ok %v", failed, ok)
	}
}

// fanoutFixture is a recorder fixture whose Fanout lookups answer from maps
// and count their calls.
type fanoutFixture struct {
	*recorderFixture
	importers   map[string][]string
	imageRepos  []string
	failLookups bool
	calls       int
}

func newFanoutFixture(t *testing.T) *fanoutFixture {
	f := &fanoutFixture{recorderFixture: newRecorderFixture(t), importers: map[string][]string{}}
	f.rec.SetFanout(Fanout{
		Importers: func(ctx context.Context, repoID string) ([]string, error) {
			f.calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Error("an importer lookup ran without a deadline")
			}
			if f.failLookups {
				return nil, errors.New("database is locked")
			}
			return f.importers[repoID], nil
		},
		ImageRepos: func(ctx context.Context) ([]string, error) {
			f.calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Error("an image-repo lookup ran without a deadline")
			}
			if f.failLookups {
				return nil, errors.New("database is locked")
			}
			return f.imageRepos, nil
		},
	})
	return f
}

// A target's fetch verdict is read by every repo importing it: when it flips
// — whoever's fetch it was — they are announced with it. Only on a flip: the
// lookup is not even made for a repeated verdict.
func TestRecorderFetch_FlipAnnouncesImporters(t *testing.T) {
	f := newFanoutFixture(t)
	f.importers["repo_lib"] = []string{"repo_app", "repo_tool"}
	own := gitx.FetchAttribution{RepoID: "repo_lib", Credential: "cred@1"}

	// The target's own fetch (its spawn, its /pull-base, its clone).
	f.rec.ObserveFetch(own, nil)
	f.wantPublished("first record of an imported repo", "repo_lib", "repo_app", "repo_tool")
	for range 5 {
		f.rec.ObserveFetch(own, nil)
	}
	f.wantPublished("repeated success")
	if f.calls != 1 {
		t.Fatalf("importer lookups = %d, want 1 (flips only)", f.calls)
	}

	// An importer's snapshot fetch of the target: the importer is named once.
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: "repo_lib", Credential: "cred@1", OnBehalfOf: "repo_app"}, errFetch)
	f.wantPublished("snapshot fetch failed", "repo_lib", "repo_app", "repo_tool")

	// A repo nobody imports announces itself alone.
	f.rec.ObserveFetch(gitx.FetchAttribution{RepoID: "repo_solo"}, nil)
	f.wantPublished("not imported", "repo_solo")

	// A failed lookup announces what the observation names, nothing more.
	f.failLookups = true
	f.rec.ObserveFetch(own, nil)
	f.wantPublished("lookup failed", "repo_lib")
}

// An image record is shared by every repo resolving to its ref: a flip
// announces the spawning repo and every repo that may read it — once, on the
// flip only.
func TestRecorderImage_FlipAnnouncesImageRepos(t *testing.T) {
	f := newFanoutFixture(t)
	f.imageRepos = []string{"repo_b", "repo_a", "repo_c"}
	const ref = "ghcr.io/acme/dev:1@sha256:0123"
	pull := errors.New("pulling dev image " + ref + ": exit status 125: manifest unknown")

	f.rec.ObserveImage("repo_a", ref, pull)
	f.wantPublished("first ensure", "repo_a", "repo_b", "repo_c")
	f.rec.ObserveImage("repo_b", ref, pull)
	f.wantPublished("same verdict from another repo")
	if f.calls != 1 {
		t.Fatalf("image-repo lookups = %d, want 1 (flips only)", f.calls)
	}
	f.rec.ObserveImage("repo_b", ref, nil)
	f.wantPublished("failed → present", "repo_b", "repo_a", "repo_c")

	f.failLookups = true
	f.rec.ObserveImage("repo_c", ref, pull)
	f.wantPublished("lookup failed", "repo_c")
}
