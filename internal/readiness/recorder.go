// Package readiness answers "can a run start in this repo right now?" (issue
// #61) without asking anyone: the report is built from state lab already
// holds and from the outcome of the most recent fetch and tracker read, so a
// page view never causes a forge request, a git network operation, a
// provider CLI process or a podman process.
//
// It has two halves. The Recorder remembers outcomes of operations lab
// performs anyway — the credentialed fetch behind every spawn, /pull-base and
// change-request merge, the list reads the AFK engine and the operator views
// make against a forge, the claimable count whenever it is computed, the
// spawn-time pull-if-missing of a dev image — and announces a repo whose
// verdict changed. The Evaluator turns those records plus the store into the
// report, and a check it cannot evaluate from them is left out — never shown
// as passing.
//
// Records live in memory only. A restart forgets them, which costs nothing
// but a shorter report until the next fetch or read: no record, no verdict.
package readiness

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// EventRepoChanged is the SSE event the recorder publishes when a repo's
// recorded verdict or count changes (brief §8.1) — the same event, with the
// same {type, repoID} envelope, every other repo-scoped change publishes, so
// a client that refetches a repo on it needs nothing new. Each publisher
// owns its own constant by design (reposvc, afk, httpapi).
const EventRepoChanged = "repo.changed"

type repoChangedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
}

// FetchRecord is the outcome of the most recent credentialed fetch of one
// repo's reference repo (a completed clone counts as one).
type FetchRecord struct {
	OK bool
	// Error is the engine's error text of a failed fetch — git's stderr
	// verbatim. It never holds credential bytes: the vault materializes
	// secrets into files, so neither argv nor stderr carries them.
	Error string
	// Credential is the stamp of the git credential the fetch ran with
	// (gitx.FetchAttribution.Credential). The record is STALE — it says
	// nothing about the repo as configured now — once the repo's current
	// credential stamps differently.
	Credential string
	At         time.Time
}

// TrackerRecord is the outcome of the most recent list read of ONE kind
// (Op) of one forge-bound repo's tracker. The recorder keeps one per kind,
// not one per repo: a forge repository can answer one listing and refuse
// another for good (issues enabled and pull requests disabled, or the
// reverse), and with one record per repo the verdict would be whichever of
// the two the engine happened to read last — flipping, and announcing the
// flip, on every pass. Per kind, a verdict moves only when that read's own
// outcome does.
type TrackerRecord struct {
	OK bool
	// Error is the forge client's error text of a failed read (method, path,
	// status, bounded body snippet — never the token).
	Error string
	// NotFound marks a read the forge answered 404: the repository does not
	// exist there, or the token cannot see it.
	NotFound bool
	// Op is the tracker.Op* constant of the read.
	Op string
	// Credential is the stamp of the forge credential the REST client was
	// built with; the record is stale once the repo's current forge
	// credential stamps differently.
	Credential string
	At         time.Time
}

// ImageRecord is the outcome of the most recent spawn-time pull-if-missing
// of one dev image ref.
type ImageRecord struct {
	OK    bool
	Error string
	At    time.Time
}

// maxErrorBytes bounds the error text a record keeps. A report quotes one
// line of it (report.go), and the line that matters sits at the END of a
// git or podman transcript and at the START of a forge client's message —
// hence a tail for the first two and a head for the last.
const maxErrorBytes = 4096

func tailBytes(s string) string {
	if len(s) <= maxErrorBytes {
		return s
	}
	return s[len(s)-maxErrorBytes:]
}

func headBytes(s string) string {
	if len(s) <= maxErrorBytes {
		return s
	}
	return s[:maxErrorBytes]
}

// claimableRecord is a claimable count as last computed. binding is the
// tracker binding it was computed under: a count taken while the repo was
// bound to another tracker counts another tracker's issues.
type claimableRecord struct {
	count   int
	binding string
}

// Recorder is the in-memory memory of outcomes the readiness report is built
// from. One Recorder is shared by every service that feeds it (cmd/lab wires
// its methods in as their observers) and by the Evaluator that reads it. Safe
// for concurrent use; the zero value is not usable — construct with
// NewRecorder.
type Recorder struct {
	bus *events.Bus
	now func() time.Time

	mu         sync.Mutex
	fetches    map[string]FetchRecord              // by repo id
	reads      map[string]map[string]TrackerRecord // by repo id, then by op (forge-bound repos only)
	claimable  map[string]claimableRecord          // by repo id
	openIssues map[string]int                      // by repo id (forge-bound repos only)
	images     map[string]ImageRecord              // by dev image ref
}

// NewRecorder returns an empty Recorder publishing repo.changed on bus. A nil
// bus records without publishing (tests); a nil now is time.Now.
func NewRecorder(bus *events.Bus, now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{
		bus:        bus,
		now:        now,
		fetches:    make(map[string]FetchRecord),
		reads:      make(map[string]map[string]TrackerRecord),
		claimable:  make(map[string]claimableRecord),
		openIssues: make(map[string]int),
		images:     make(map[string]ImageRecord),
	}
}

// publish announces repo.changed for each distinct non-empty repo id. Always
// called WITHOUT r.mu held. Publishing happens only where a verdict or a
// count actually changed — never per read: the AFK engine reads every ready
// queue on every pass, and an event per read would have every open page
// refetching on that cadence.
func (r *Recorder) publish(repoIDs ...string) {
	if r.bus == nil {
		return
	}
	seen := make(map[string]bool, len(repoIDs))
	for _, id := range repoIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		r.bus.Publish(events.Event{Type: EventRepoChanged, Payload: repoChangedPayload{Type: EventRepoChanged, RepoID: id}})
	}
}

// Announce publishes repo.changed for repos whose readiness changed for a
// reason the recorder does not observe itself — the container preflight's
// verdict landing, which lives in an in-memory gate (cmd/lab calls this from
// the goroutine that publishes it). It records nothing.
func (r *Recorder) Announce(repoIDs ...string) { r.publish(repoIDs...) }

// ObserveFetch records the outcome of one attributed fetch; it has
// gitx.FetchObserver's signature and is wired as the shared git engine's
// observer. Only fetches that ran with the repo's own git credential arrive
// here (gitx reports nothing unattributed — the reconcile sweep's
// credential-less fetch, which fails on every private remote by design,
// never does), and none that died with its caller's context.
//
// It publishes repo.changed when the verdict flips — failed↔succeeded, a
// first record, or a record for a different credential version — for the
// fetched repo and for the repo the fetch ran on behalf of (the importing
// repo of a read-only import, whose own imports check reads this record).
func (r *Recorder) ObserveFetch(a gitx.FetchAttribution, err error) {
	if a.RepoID == "" || errors.Is(err, context.Canceled) {
		return
	}
	rec := FetchRecord{OK: err == nil, Credential: a.Credential, At: r.now()}
	if err != nil {
		rec.Error = tailBytes(err.Error())
	}
	r.mu.Lock()
	prev, had := r.fetches[a.RepoID]
	r.fetches[a.RepoID] = rec
	r.mu.Unlock()
	if !had || prev.OK != rec.OK || prev.Credential != rec.Credential {
		r.publish(a.RepoID, a.OnBehalfOf)
	}
}

// ObserveTrackerRead records the outcome of one list read of a forge-bound
// repo's tracker; it has tracker.ReadObserver's signature and is wired as the
// tracker registry's read observer.
//
// Two outcomes are evidence of nothing and leave the record as it was: a
// read cancelled by its caller, and a rate-limited one (tracker.
// ErrRateLimited) — the forge refused to answer at all, which neither
// confirms nor refutes that the token and the repository are right, and it
// heals by itself when the window resets (ADR-0015).
//
// It publishes repo.changed when the repo's tracker verdict under the read's
// credential version changes — nothing known → known, passing ↔ failing — or
// when the open issue count the read carried differs from the remembered one.
// The verdict is JudgeTrackerReads over every kind of read, exactly what the
// report shows, so an event is published precisely when the report changes.
func (r *Recorder) ObserveTrackerRead(l tracker.ListRead) {
	if l.RepoID == "" || errors.Is(l.Err, context.Canceled) || errors.Is(l.Err, tracker.ErrRateLimited) {
		return
	}
	rec := TrackerRecord{OK: l.Err == nil, Op: l.Op, Credential: l.Credential, At: r.now()}
	if l.Err != nil {
		rec.Error = headBytes(l.Err.Error())
		rec.NotFound = errors.Is(l.Err, tracker.ErrNotFound)
	}
	r.mu.Lock()
	ops := r.reads[l.RepoID]
	if ops == nil {
		ops = make(map[string]TrackerRecord, 4)
		r.reads[l.RepoID] = ops
	}
	before := trackerState(ops, rec.Credential)
	ops[l.Op] = rec
	changed := before != trackerState(ops, rec.Credential)
	if l.Err == nil && l.OpenIssues >= 0 {
		if n, known := r.openIssues[l.RepoID]; !known || n != l.OpenIssues {
			r.openIssues[l.RepoID] = l.OpenIssues
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		r.publish(l.RepoID)
	}
}

// ObserveClaimable records a repo's claimable count as just computed; it has
// afk.Options.OnClaimable's signature. It publishes repo.changed when the
// count differs from the remembered one (or is the first).
func (r *Recorder) ObserveClaimable(repo store.Repo, count int) {
	if repo.ID == "" || count < 0 {
		return
	}
	rec := claimableRecord{count: count, binding: repo.TrackerBinding}
	r.mu.Lock()
	prev, had := r.claimable[repo.ID]
	r.claimable[repo.ID] = rec
	r.mu.Unlock()
	if !had || prev != rec {
		r.publish(repo.ID)
	}
}

// ObserveImage records the outcome of a spawn-time pull-if-missing of a dev
// image; it has instance.Options.ImageEnsured's signature. The record is
// keyed by the image ref — every repo resolving to that ref shares it — and
// repo.changed is published for the repo whose spawn observed it when the
// ref's verdict flips or is the first.
func (r *Recorder) ObserveImage(repoID, ref string, err error) {
	if ref == "" || errors.Is(err, context.Canceled) {
		return
	}
	rec := ImageRecord{OK: err == nil, At: r.now()}
	if err != nil {
		rec.Error = tailBytes(err.Error())
	}
	r.mu.Lock()
	prev, had := r.images[ref]
	r.images[ref] = rec
	r.mu.Unlock()
	if !had || prev.OK != rec.OK {
		r.publish(repoID)
	}
}

// Fetch returns the recorded fetch outcome of a repo, if any.
func (r *Recorder) Fetch(repoID string) (FetchRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.fetches[repoID]
	return rec, ok
}

// TrackerReads returns the recorded list-read outcomes of a repo — the most
// recent one of each kind of read, ordered by Op — or nil when none is
// recorded. JudgeTrackerReads turns them into a verdict.
func (r *Recorder) TrackerReads(repoID string) []TrackerRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	ops := r.reads[repoID]
	if len(ops) == 0 {
		return nil
	}
	recs := make([]TrackerRecord, 0, len(ops))
	for _, rec := range ops {
		recs = append(recs, rec)
	}
	slices.SortFunc(recs, func(a, b TrackerRecord) int { return strings.Compare(a.Op, b.Op) })
	return recs
}

// JudgeTrackerReads is the tracker verdict a repo's recorded list reads add
// up to under the forge credential version credential: failed is the most
// recent failed read among those made with that version (nil when none
// failed), ok whether any of them succeeded. Records made with another
// version — a credential since rotated or replaced — are stale and ignored;
// an empty credential (the current one could not be read) matches nothing.
// The reads fail the check if any kind of read failed, and pass it only when
// none did: a forge that serves the issue list and refuses the pull list is
// not a tracker lab can work with.
func JudgeTrackerReads(recs []TrackerRecord, credential string) (failed *TrackerRecord, ok bool) {
	if credential == "" {
		return nil, false
	}
	for i := range recs {
		rec := &recs[i]
		switch {
		case rec.Credential != credential:
		case rec.OK:
			ok = true
		case failed == nil || rec.At.After(failed.At):
			failed = rec
		}
	}
	return failed, ok
}

// trackerState folds JudgeTrackerReads into the three states a publish
// decision compares: nothing known, passing, failing.
func trackerState(ops map[string]TrackerRecord, credential string) int {
	recs := make([]TrackerRecord, 0, len(ops))
	for _, rec := range ops {
		recs = append(recs, rec)
	}
	switch failed, ok := JudgeTrackerReads(recs, credential); {
	case failed != nil:
		return 2
	case ok:
		return 1
	}
	return 0
}

// Claimable returns a repo's claimable count as last computed under the
// given tracker binding; ok is false when none is remembered, or the
// remembered one was computed while the repo was bound to another tracker.
func (r *Recorder) Claimable(repoID, binding string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.claimable[repoID]
	if !ok || rec.binding != binding {
		return 0, false
	}
	return rec.count, true
}

// OpenIssues returns a forge-bound repo's open issue count as last read.
func (r *Recorder) OpenIssues(repoID string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.openIssues[repoID]
	return n, ok
}

// Image returns the recorded pull-if-missing outcome of a dev image ref.
func (r *Recorder) Image(ref string) (ImageRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.images[ref]
	return rec, ok
}

// Forget drops everything remembered about a repo — called when the repo is
// deleted. Image records are keyed by ref, not by repo, and stay.
func (r *Recorder) Forget(repoID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.fetches, repoID)
	delete(r.reads, repoID)
	delete(r.claimable, repoID)
	delete(r.openIssues, repoID)
}
