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

// ErrReviewRejected marks a review operation the forge refused: a
// RerequestReview it would not accept. It mirrors ErrMergeRejected exactly —
// reviewability is the backend's call, never reasoned about agent-side: lab
// attempts the write and, if the forge refuses, wraps this sentinel around the
// refusal's OWN words verbatim (the forge's error body), because that text is
// what the caller needs to read to know why the write did not take (ADR-0024's
// verbatim-refusal pattern). The forge token never appears in the wrapped
// message. The rerequest handler surfaces it as a non-fatal warning
// (ADR-0048: the native re-request ping is best-effort).
var ErrReviewRejected = errors.New("tracker: review rejected")

// ErrUnsupported marks an operation a tracker backend does not implement — the
// built-in binding's review-adjacent write verbs (RerequestReview,
// CommentPull). Reviews are forge-observable state a lab-internal change
// request has no model for, so the built-in tracker defers the writes rather
// than faking a result; the wrapping error names the verb. Callers
// errors.Is-detect it to answer "not supported on this tracker" distinctly
// from a forge failure.
var ErrUnsupported = errors.New("tracker: operation not supported by this tracker backend")

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

// Review-state vocabulary a submitted Review's State is normalized into. The
// backends map their own forge's review-event words onto these four
// (Forgejo's APPROVED/REQUEST_CHANGES/COMMENT/REQUEST_REVIEW, GitHub's
// APPROVED/CHANGES_REQUESTED/COMMENTED); a state a backend does not recognize
// passes through lowercased, so an agent reading a review still sees exactly
// what the forge said (the RawState-style "backend's own words" pattern,
// ADR-0024). ReviewRequested is a pending request for review, distinct from a
// submitted verdict; the reject → re-queue loop reads ReviewChangesRequested
// to know a PR carries outstanding findings.
const (
	ReviewApproved         = "approved"
	ReviewChangesRequested = "changes_requested"
	ReviewCommented        = "commented"
	ReviewRequested        = "review_requested"
)

// Issue-state vocabulary and the Issues() state filter. StateOpen/StateClosed
// are also the values Issue.State can hold; StateAll is a filter only.
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateAll    = "all"
)

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

// Review is one submitted review on a pull/change request in lab's vocabulary:
// who submitted it (Reviewer), its normalized State (the Review* vocabulary; an
// unrecognized forge state passes through lowercased), its body prose, and
// whether the forge has since dismissed it. The reject → re-queue loop reads
// these to know whether a PR carries an outstanding changes-requested review
// and from which reviewer to re-request once a fix lands. Dismissed carries the
// forge's own dismissal signal (Forgejo's `dismissed` flag; GitHub's DISMISSED
// review state) so a superseded verdict is not mistaken for a live one.
type Review struct {
	Reviewer  string
	State     string
	Body      string
	Dismissed bool
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

	// Issues lists issues by state (StateOpen|StateClosed|StateAll). Open
	// issues in the list view carry no comments (a list is not a thread read).
	Issues(ctx context.Context, state string) ([]Issue, error)

	// Issue returns one issue in full, including its comment thread.
	Issue(ctx context.Context, number int) (Issue, error)

	// CreateComment posts a comment on an issue.
	CreateComment(ctx context.Context, number int, body string) error

	// Pulls lists every pull request / change request on the repo across ALL
	// states, so the reaper can match a run's head branch client-side. Listing
	// only open PRs silently breaks the M5 done-signal (a merged afk/<N> PR is
	// no longer open) — all states is required.
	Pulls(ctx context.Context) ([]PullRef, error)

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

	// Reviews lists the submitted reviews on pull number, oldest first — the
	// read behind the human half of the autoland loop's hybrid rejected-state
	// (ADR-0048). An unknown number wraps ErrNotFound. The built-in binding has
	// no forge reviews to report and answers an empty list (reviews are
	// forge-observable state a lab-internal change request has no model for),
	// never an error.
	Reviews(ctx context.Context, number int) ([]Review, error)

	// RerequestReview re-requests review from every reviewer whose latest
	// verdict-bearing (approved or changes-requested), non-dismissed review
	// requests changes; no such reviewer is a convergent no-op success (nothing
	// to re-request), mirroring MergePull's already-merged no-op. Non-verdict
	// review rows (commented, review_requested, pending, unknown) neither set
	// nor clear a reviewer's verdict. A forge refusal wraps ErrReviewRejected
	// with its own words; an unknown number wraps ErrNotFound; the built-in
	// binding wraps ErrUnsupported.
	RerequestReview(ctx context.Context, number int) error

	// CommentPull posts a plain discussion comment on pull number (a PR shares
	// the issue-comment number space). Body validation (non-empty) is the API
	// layer's job. An unknown number wraps ErrNotFound; the built-in binding
	// wraps ErrUnsupported.
	CommentPull(ctx context.Context, number int, body string) error

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
