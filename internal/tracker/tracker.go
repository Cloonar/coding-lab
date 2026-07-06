package tracker

// The Tracker seam: lab's single issue/PR vocabulary shared by the Forgejo
// REST client (internal/tracker/forgejo) and the built-in store-backed
// tracker (internal/tracker/builtin). Both backends import this package for
// the types and implement this interface; the registry (registry.go) resolves
// which one answers for a given repo. GitHub is fast-follow (issue #1).
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

	// CreatePull opens a pull request / change request from head onto base.
	// Exercised in M5/M6; implemented now for symmetry.
	CreatePull(ctx context.Context, head, base, title, body string) (PullRef, error)

	// CloseIssue transitions an issue to closed.
	CloseIssue(ctx context.Context, number int) error

	// CreateIssue opens an issue with the given labels attached at creation
	// (the to-issues path: a plan becomes labelled tracker issues in one call
	// each). Labels are names; any name the repo does not define wraps
	// ErrUnknownLabel and nothing is created.
	CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error)

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
