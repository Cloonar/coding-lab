package tracker

// The Tracker seam: lab's single issue/PR vocabulary shared by the Forgejo
// REST client (internal/tracker/forgejo), the GitHub REST client
// (internal/tracker/github), and the built-in store-backed tracker
// (internal/tracker/builtin). Every backend imports this package for the
// types and implements this interface; the registry (registry.go) resolves
// which one answers for a given repo — routing on the forge credential's
// flavor for a forge binding (ADR-0015).
//
// The Comment/Issue/PullRef types and the Tracker interface below are the
// pinned M4 contract — the sibling forge/builtin packages compile against
// exactly these definitions — extended by the agent triage surface
// (ADR-0014): issue create, label ops, and the Label type. What stays dead
// is the ENGINE writing tracker state implicitly; a deliberate agent
// workflow action flows through this seam to whichever backend the repo's
// binding names, exactly as agent comments and PR creation always have.

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound marks a tracker call whose subject does not exist on the
// backend — e.g. the forge answers 404 for an unknown issue number. The
// forgejo client wraps it into its diagnostic error so callers can
// errors.Is-detect "no such issue" without parsing status text; the builtin
// tracker surfaces store.ErrNotFound for the same condition, so API layers
// check both.
var ErrNotFound = errors.New("tracker: not found")

// ErrDuplicateOpenPull marks a CreatePull whose head branch already carries
// an OPEN pull/change request. The builtin tracker refuses the duplicate for
// forge parity -- Forgejo answers 409 "pull request already exists" for the
// same retry -- so an agent whose first create timed out client-side gets a
// clean conflict instead of a second identical CR. The wrapping error names
// the existing number.
var ErrDuplicateOpenPull = errors.New("an open pull request for this head branch already exists")

// ErrUnknownLabel marks a label-name argument that resolves to no label of
// the repo. Applying (or creating an issue with) a nonexistent label is an
// error on BOTH bindings, never an implicit create — a typo must become a
// loud failure instead of a permanent garbage label (ADR-0014). The wrapping
// error names the offending label.
var ErrUnknownLabel = errors.New("tracker: unknown label")

// ErrMergeRejected marks a MergePull the backend refused to land: a forge's
// branch-protection or unsatisfied-required-check answer, a merge conflict,
// or (built-in) a git push a pre-receive hook declined. It mirrors gitx's
// ErrPushRejected property — the wrapped message carries the backend's OWN
// words verbatim (the forge's error body, or git's stderr), because that text
// is exactly what the agent needs to read to know why the merge did not take.
// Mergeability is the backend's call, never reasoned about agent-side: lab
// attempts the merge and surfaces the rejection as-is with a non-zero exit
// (ADR-0011's "push refusals surface git's stderr verbatim", widened to the
// agent surface). The agent API answers 409 with the message.
var ErrMergeRejected = errors.New("tracker: merge rejected")

// ErrRateLimited marks a forge call throttled by the upstream rate limiter
// (GitHub answers 403/429 with X-RateLimit-Remaining: 0 or a Retry-After
// header when the token's hourly budget is spent). The GitHub client wraps it
// into its diagnostic error — with the reset time in the message — and
// unwraps to it like ErrNotFound, so a rate limit is a distinct, typed state
// rather than an opaque bad-gateway. Nothing retries in-client: the AFK
// scheduler and reaper already log-and-skip per tick, so a throttled repo
// self-heals on a later tick once the window resets, and a spent budget never
// counts as a run failure (ADR-0015, decision 6). The Forgejo client never
// produces it (lab's single Forgejo instance is not rate-limited this way).
var ErrRateLimited = errors.New("tracker: rate limited by the forge")

// Issue/PR state vocabulary. A merged PR is distinct from a closed-unmerged
// one (v0 pin): the reaper treats open|merged as a done-signal but a
// closed-unmerged head-matching PR as "no PR", so the run fails on
// death/timeout rather than being falsely reaped as a success. The forge REST
// client derives these three from Forgejo's {state, merged} pair; the built-in
// tracker maps change-request state onto the same three.
const (
	PullOpen   = "open"
	PullMerged = "merged"
	PullClosed = "closed"
)

// Issue-state vocabulary and the Issues() state filter. StateOpen/StateClosed
// are also the values Issue.State can hold; StateAll is a filter only.
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateAll    = "all"
)

// RecentClosedWindow is the number of most-recently-closed items a bounded
// listing carries beyond the full open set: Pulls() and the Issues()
// closed/all views return the complete OPEN set (capped by the working set,
// which stays small) plus at most this many recently closed items. The window
// exists to bound forge round-trips (issue #176): a full-history state=all
// walk costs O(every item the repo ever had) requests per call — measured
// 6.7s for ~174 PRs on the live forge, growing forever — for list views whose
// consumers only ever need the working set plus a recency view. 50 is one
// page on either forge flavor (Forgejo's limit, GitHub's per_page), so the
// window always costs exactly ONE extra request.
const RecentClosedWindow = 50

// ReadyLabel is the queue label ReadyIssues filters on: an open issue carrying
// it is fully specified and waiting for an AFK agent. lab never writes it — an
// AFK run's claim is its local afk/<N> branch, not a tracker label (ADR-0013),
// so triage and humans can (re)apply it freely without clobbering a claim. It
// is also one of the five seeded triage labels; the exact spelling is API
// surface (triage skill, humans, seed docs all depend on it).
const ReadyLabel = "ready-for-agent"

// Comment is one comment on an issue in lab's vocabulary: who wrote it, its
// body, and when. The forge client fills Author from the comment's user; the
// built-in tracker fills it from author_kind (operator or the run's identity).
type Comment struct {
	Author    string
	Body      string
	CreatedAt time.Time
}

// Issue is one tracker issue in lab's single vocabulary, populated the same
// way by both backends. List views (Issues, ReadyIssues) leave Comments nil
// to stay cheap and report the thread size via CommentsCount instead (the
// forge client maps Forgejo's `comments` field, the builtin tracker the
// store's count); Issue(n) loads the full comment thread. State is StateOpen
// or StateClosed.
type Issue struct {
	Number        int
	Title         string
	Body          string
	State         string
	Labels        []string
	Comments      []Comment
	CommentsCount int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// IssueEdit is a patch over an issue's editable content: a nil pointer leaves
// that field untouched, a non-nil pointer fully replaces it — and a non-nil
// pointer to "" legally CLEARS the field (only nil means "leave alone"). It is
// title/body only: state is CloseIssue's, never an edit's (the open↔closed
// transition owns closed_at). It carries no run/author identity — an edit
// re-writes existing content, it does not re-author the issue.
type IssueEdit struct {
	Title *string
	Body  *string
}

// PullRef is one pull request (forge) or change request (built-in) reduced to
// what the reaper matches on: its number, head branch, three-valued state
// (PullOpen|PullMerged|PullClosed), and web URL. Head is the bare branch name
// (e.g. afk/63), never owner:branch — matching against a run's branch is
// client-side (PullState / PRPresent).
type PullRef struct {
	Number     int
	HeadBranch string
	State      string
	URL        string
}

// PullDetail is one pull request / change request in full: what PullRef
// deliberately drops (title, BODY) plus the same number/state/head/URL. It
// exists because PullRef is pinned to the reaper's hot path — Pulls() stays a
// cheap fan-out — while the agent read surface (labctl pr view) needs the
// body, e.g. a captured card YAML living in a PR body on the forge. State is
// the same three-valued PullOpen|PullMerged|PullClosed vocabulary.
type PullDetail struct {
	Number     int
	Title      string
	Body       string
	State      string
	HeadBranch string
	URL        string
}

// Check-state vocabulary a Checks() row's State is normalized into. The
// normalization itself happens in the backends (the forge REST clients
// collapse their own multi-word CI states onto these three; the builtin
// tracker has no CI to normalize); RawState on the Check carries the forge's
// own word verbatim, so an agent that wants more than pending/success/failure
// can read exactly what the forge said instead of trusting lab's collapse.
// ChecksNone is never a row's State — it is the AGGREGATE ChecksState (see
// checks.go) returns for zero rows, computed lab-side and never taken from a
// forge's own combined verdict.
const (
	CheckPending = "pending"
	CheckSuccess = "success"
	CheckFailure = "failure"
	ChecksNone   = "none" // aggregate only: zero rows
)

// Check is one CI check / commit-status row for a pull/change request's
// current head commit — a GitHub Check Run or a Forgejo commit status,
// normalized onto lab's three-word vocabulary by the backend that read it.
type Check struct {
	Name     string // check/status context name, e.g. a CI job
	State    string // normalized: CheckPending|CheckSuccess|CheckFailure
	RawState string // the forge's own state word verbatim (ADR-0011/0024 "backend's own words" pattern)
	Summary  string // the check's description/output title, may be empty
	URL      string // for humans reading transcripts; the agent cannot browse
}

// Label is one repo label in lab's vocabulary. Callers speak label NAMES
// only — the forge client's name-to-ID resolution and the builtin tracker's
// label ids stay behind the seam. Color is a #rrggbb swatch; both bindings
// supply the same default when a create omits it.
type Label struct {
	Name        string
	Color       string
	Description string
}

// Tracker is lab's read-mostly view of a repo's issue tracker — one vocabulary
// over the Forgejo REST API and the built-in store-backed tracker. Operator
// read views use it now; the agent API (M5) uses it for the AFK run's
// ready-queue/comment/PR flow. Every call is repo-scoped by the concrete
// backend (a Registry.TrackerFor result is already bound to one repo). A
// failed call returns (nil/zero, err) with no partial data — the scheduler or
// reaper simply skips the tick; an empty result is success (a non-nil empty
// slice), not an error.
type Tracker interface {
	// ReadyIssues lists open issues carrying ReadyLabel — the queue the AFK
	// scheduler claims the lowest-numbered issue from. Comments are not loaded.
	ReadyIssues(ctx context.Context) ([]Issue, error)

	// Issues lists issues by state (StateOpen|StateClosed|StateAll). The open
	// view is the full open set; the closed and all views carry a BOUNDED
	// recent window — the RecentClosedWindow most recently updated closed
	// issues — instead of the full closed history (issue #176), with the all
	// view listing the open set first (the states are disjoint, so no
	// dedupe). Issues in the list view carry no comments (a list is not a
	// thread read); Issue(n) remains the unbounded-by-number single read.
	Issues(ctx context.Context, state string) ([]Issue, error)

	// Issue returns one issue in full, including its comment thread.
	Issue(ctx context.Context, number int) (Issue, error)

	// CreateComment posts a comment on an issue.
	CreateComment(ctx context.Context, number int, body string) error

	// Pulls lists the repo's pull/change requests as a BOUNDED view: all OPEN
	// pulls, plus the RecentClosedWindow most recently closed (merged or not)
	// pulls — the open set first in the backend's native list order, then the
	// recent-closed window newest-closed-first. This is the operator/agent
	// list view (labctl pr list), bounded by design (issue #176): the open
	// set is capped by the working set while closed/merged history grows
	// forever, so listing every PR across all states cost O(history) requests
	// per call. The reaper's done-signal no longer reads through this method
	// — it reads PullsForHead, which inherited the old "state=all is required
	// for the done-signal" rationale — and a merged run PR still appears here
	// while recent; Pull(n) (labctl pr view) remains the unbounded-by-number
	// single read for anything older.
	Pulls(ctx context.Context) ([]PullRef, error)

	// PullsForHead lists the pull/change requests whose head branch is head
	// AND base branch is base, across ALL states, with a request count
	// bounded independent of repo history (issue #176): the reaper's
	// done-signal question — "does an open-or-merged PR exist for this run's
	// branch?" — must not cost O(every PR the repo ever had) per tick, which
	// is what answering it through the old full-history Pulls() walk did
	// (Pulls itself is a bounded open+recent-closed view now, and its closed
	// window may age a run's merged PR out — this method is the read that
	// never does). This method only ENUMERATES
	// candidates; callers project the winner with the existing pure functions
	// (DonePull / PullState), so a head-reuse collision may legally return
	// several refs. Semantic pins:
	//
	//   - No match is ([]PullRef{}, nil) — an empty non-nil slice, success,
	//     the seam's "empty result is success" convention.
	//   - Every returned ref's HeadBranch equals the queried head VERBATIM.
	//     Forgejo renders a merged pull whose head branch was since deleted
	//     with a synthetic head.ref of refs/pull/N/head (verified live on
	//     lab's forge); the by-name query key is authoritative, so a backend
	//     fills HeadBranch from the queried branch, never a rendered ref.
	//   - A backend that can only resolve ONE candidate cheaply must never
	//     answer a closed-unmerged pull while an open or merged sibling
	//     exists on the same head+base: closed-unmerged is the one state the
	//     done-signal reads as absence (DonePull's floor), so a shadowed
	//     live pull would falsely fail a run. The forgejo client handles
	//     this with a rare-case fallback walk.
	//   - The contract covers LIVE head branches only: the reaper queries
	//     branches of ACTIVE runs (the branch outlives the run), so a
	//     merged-and-since-deleted branch is not required to be findable —
	//     backends may still find one, but callers must not rely on it.
	//   - A pull from head onto a DIFFERENT base does not match: the read is
	//     base-scoped by design — the reaper passes the repo default branch,
	//     the base the agent API pins for every lab-created PR.
	PullsForHead(ctx context.Context, head, base string) ([]PullRef, error)

	// Pull returns one pull request / change request in full detail,
	// including its body — the agent read surface behind labctl pr view. An
	// unknown number wraps ErrNotFound (the builtin backend surfaces
	// store.ErrNotFound, like Issue).
	Pull(ctx context.Context, number int) (PullDetail, error)

	// Checks returns the CI check/status rows for the pull/change request's
	// CURRENT head commit — the backend resolves the head SHA at query time,
	// so a push or rebase that lands before the read is reflected, never a
	// memoized commit. An unknown number wraps ErrNotFound (the builtin
	// backend surfaces store.ErrNotFound, like Pull). Zero rows is success —
	// a non-nil empty slice, not an error: a repo with no CI configured
	// truthfully has no checks to report. Aggregation over the returned rows
	// is client-side via ChecksState, NEVER trusted from the forge's own
	// combined-status verdict (checks.go explains why).
	Checks(ctx context.Context, number int) ([]Check, error)

	// CreatePull opens a pull request / change request from head onto base.
	// Exercised in M5/M6; implemented now for symmetry.
	CreatePull(ctx context.Context, head, base, title, body string) (PullRef, error)

	// MergePull lands pull/change request `number` and returns its resulting
	// PullRef (State PullMerged on success). The merge method is fixed — no
	// caller flag: fast-forward when possible else a merge commit on the
	// built-in binding, a single fixed forge merge method on a forge binding
	// (the forge applies its own configured strategy). Mergeability is the
	// backend's call: MergePull does NOT pre-check required status checks or
	// branch protection; it attempts the merge and, if the backend refuses,
	// wraps ErrMergeRejected around the refusal's own words. The head branch
	// is never deleted (branch lifecycle stays owned by the sweep/teardown).
	// Convergent: merging an already-merged pull is a no-op success naming
	// the existing merged state, not an error. An unknown number wraps
	// ErrNotFound. On a forge binding the merge is a server-credentialed
	// call — no forge token ever reaches the agent session (ADR-0014).
	MergePull(ctx context.Context, number int) (PullRef, error)

	// CloseIssue transitions an issue to closed.
	CloseIssue(ctx context.Context, number int) error

	// CreateIssue opens an issue with the given labels attached at creation
	// (the to-issues path: a plan becomes labelled tracker issues in one call
	// each). Labels are names; any name the repo does not define wraps
	// ErrUnknownLabel and nothing is created.
	CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error)

	// EditIssue applies a title/body patch (IssueEdit) to an issue and returns
	// the updated issue. Last write wins: no guard, no concurrency token — an
	// issue open or closed, claimed or not, edits the same way, and the forge's
	// own edit history is the audit trail. The seam does NOT validate title
	// non-emptiness; that guard is the API layer's (it mirrors the operator
	// PATCH in internal/httpapi/issues.go), so backends pass the patch straight
	// through. An unknown number wraps ErrNotFound (the builtin backend surfaces
	// store.ErrNotFound, like Issue/Pull). The returned Issue is in LIST shape —
	// Comments nil, CommentsCount populated the way the backend's list mapping
	// does it — because an edit is a write, not a thread read (CreateIssue's
	// shape convention).
	EditIssue(ctx context.Context, number int, edit IssueEdit) (Issue, error)

	// AddIssueLabels attaches the named labels to an issue. Names are
	// resolved strictly BEFORE anything is applied: an unknown name wraps
	// ErrUnknownLabel, never an implicit label create. Re-adding an already
	// attached label is not an error (forge behavior).
	AddIssueLabels(ctx context.Context, number int, labels []string) error

	// RemoveIssueLabels detaches the named labels from an issue, under the
	// same strict name resolution as AddIssueLabels. Removing a defined label
	// that is not attached is a no-op, not an error.
	RemoveIssueLabels(ctx context.Context, number int, labels []string) error

	// Labels lists the repo's labels ordered by name.
	Labels(ctx context.Context) ([]Label, error)

	// EnsureLabel creates the named label if the repo does not define it and
	// returns the label that exists afterwards — idempotent by name, so
	// skills run it unconditionally and a retry after a timed-out call is
	// safe. Empty color/description take the binding's defaults; an existing
	// label's color/description are never overwritten.
	EnsureLabel(ctx context.Context, name, color, description string) (Label, error)
}

// RunScoper is the optional identity-rescoping seam on a Tracker: a backend
// whose CreateComment identity can be re-bound to a specific run implements
// it (the builtin tracker today), and every decorator the registry may wrap
// around a backend forwards it. Callers assert THIS interface — never the
// concrete backend type, which a decorator (the metrics observer) would
// mask, silently dropping the run identity.
type RunScoper interface {
	// ForRun returns a Tracker whose comments are authored as the given run.
	ForRun(runID string) Tracker
}
