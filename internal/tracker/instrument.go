package tracker

// The registry's observer seam (M8): every Tracker the registry resolves is
// wrapped in a thin decorator that reports one (binding, op, ok) triple per
// call — the lab_tracker_requests_total source. The seam deliberately
// carries NO error value and no call arguments: nothing that could hold
// token bytes, credential payloads, or issue text ever crosses it, so the
// metrics layer cannot leak what the tracker layer protects.
//
// With no observer set (tests, degraded wiring) TrackerFor returns the
// backend unwrapped — existing type assertions on the concrete backends
// keep working.
//
// A second, separate seam rides the same decorator (issue #61): the READ
// observer, which does carry an error — see ReadObserver for why that is
// safe there and still wrong for the metrics seam above.

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// Operation vocabulary for the observer seam — one constant per Tracker
// method, bounded by construction (metric label values).
const (
	OpReadyIssues       = "ready"
	OpIssues            = "issues"
	OpIssue             = "issue"
	OpCreateComment     = "comment"
	OpPulls             = "pulls"
	OpPullsForHead      = "pulls_for_head"
	OpPull              = "pull"
	OpChecks            = "checks"
	OpCheckLog          = "check_log"
	OpCreatePull        = "create_pull"
	OpMergePull         = "merge_pull"
	OpReviews           = "reviews"
	OpRerequestReview   = "rerequest_review"
	OpCommentPull       = "comment_pull"
	OpPullComments      = "pull_comments"
	OpCloseIssue        = "close"
	OpCreateIssue       = "create_issue"
	OpEditIssue         = "edit_issue"
	OpAddIssueLabels    = "label_add"
	OpRemoveIssueLabels = "label_remove"
	OpLabels            = "labels"
	OpEnsureLabel       = "label_ensure"
)

// Observer receives one report per tracker call resolved through the
// registry: the repo's binding (forge|builtin), the operation (Op*
// constants), and whether the call succeeded. ok is false for ANY non-nil
// error, domain conditions like ErrNotFound included — the caller decides
// what to make of the rate.
type Observer func(binding, op string, ok bool)

// SetObserver wires the observer. Call once during startup wiring, before
// any TrackerFor — the field is read without a lock.
func (r *Registry) SetObserver(obs Observer) { r.observe = obs }

// ListRead is the outcome of one LIST read of a forge-bound repo's tracker —
// ReadyIssues, Issues, Pulls or PullsForHead — as the read observer receives
// it (issue #61). A list read is the one kind of call whose failure says
// something about the repo as a whole rather than about one issue or pull:
// it needs nothing but a reachable forge, a token the forge accepts, and a
// repository that token can see.
type ListRead struct {
	// RepoID is the repo the tracker was resolved for.
	RepoID string
	// Credential names the exact version of the forge credential the REST
	// client was built with (store.CredentialStamp over the row TrackerFor
	// decrypted), so the outcome goes stale when the operator picks or
	// rotates another.
	Credential string
	// Op is the Op* constant of the read.
	Op string
	// Err is the read's error, nil on success. A forge client's error holds
	// the method, path, status and a bounded body snippet, and never the
	// token — which travels only in the Authorization header (the forgejo
	// and github `do` helpers pin that).
	Err error
	// OpenIssues is the size of the repo's open issue set when this read
	// returned all of it (a successful Issues read of the open or all
	// view); -1 when the read does not carry that.
	OpenIssues int
}

// ReadObserver receives one ListRead per list read of a FORGE-bound repo's
// tracker resolved through the registry. It is what lets the readiness report
// (issue #61) say whether the tracker answers without asking the forge per
// page view: the report is built from the most recent read lab made anyway.
//
// Unlike Observer it carries the error, because "the last read failed" is
// useless to an operator without the forge's own words. That is safe on THIS
// seam for the reason given on ListRead.Err, and it stays off the metrics
// seam, whose label values must be bounded. Three kinds of read are never
// reported: a builtin-bound repo's (a store query — it cannot fail the way a
// forge does), one whose caller's context is already done (a dropped request
// says nothing about the forge), and anything that is not a list read.
type ReadObserver func(ListRead)

// SetReadObserver wires the read observer. Call once during startup wiring,
// before any TrackerFor — the field is read without a lock.
func (r *Registry) SetReadObserver(obs ReadObserver) { r.observeRead = obs }

// instrument wraps t for the observers; with neither set it returns t
// unwrapped. credential is the stamp of the forge credential t was built
// with, "" for the builtin tracker — whose reads the read observer never
// sees.
func (r *Registry) instrument(t Tracker, binding, repoID, credential string) Tracker {
	reads := r.observeRead
	if binding != store.TrackerBindingForge {
		reads = nil
	}
	if r.observe == nil && reads == nil {
		return t
	}
	return &observed{t: t, binding: binding, obs: r.observe, reads: reads, repoID: repoID, credential: credential}
}

// observed decorates a Tracker with per-call observer reports. Results and
// errors pass through untouched.
type observed struct {
	t       Tracker
	binding string
	obs     Observer // nil when only the read observer is wired

	// The read observer and what it attributes a list read to (nil reads =
	// not reported: no observer wired, or a builtin-bound repo).
	reads      ReadObserver
	repoID     string
	credential string
}

func (o *observed) report(op string, err error) {
	if o.obs != nil {
		o.obs(o.binding, op, err == nil)
	}
}

// reportRead hands one list read to the read observer. openIssues is the
// open-set size the read carried, -1 for none.
func (o *observed) reportRead(ctx context.Context, op string, err error, openIssues int) {
	if o.reads == nil || ctx.Err() != nil {
		return
	}
	if err != nil {
		openIssues = -1
	}
	o.reads(ListRead{RepoID: o.repoID, Credential: o.credential, Op: op, Err: err, OpenIssues: openIssues})
}

// ForRun forwards the identity-rescoping seam (RunScoper) through the
// decorator: the re-scoped backend is re-wrapped so its calls keep
// reporting, and a backend without the seam stays wrapped unchanged.
// Without this, wiring the observer would hide the builtin tracker's
// ForRun from the agent API and agent comments would silently fall back
// to the operator identity.
func (o *observed) ForRun(runID string) Tracker {
	if rs, ok := o.t.(RunScoper); ok {
		scoped := *o
		scoped.t = rs.ForRun(runID)
		return &scoped
	}
	return o
}

var _ RunScoper = (*observed)(nil)

func (o *observed) ReadyIssues(ctx context.Context) ([]Issue, error) {
	issues, err := o.t.ReadyIssues(ctx)
	o.report(OpReadyIssues, err)
	o.reportRead(ctx, OpReadyIssues, err, -1)
	return issues, err
}

func (o *observed) Issues(ctx context.Context, state string) ([]Issue, error) {
	issues, err := o.t.Issues(ctx, state)
	o.report(OpIssues, err)
	o.reportRead(ctx, OpIssues, err, openIssueCount(issues, state))
	return issues, err
}

// openIssueCount is the size of the open issue set an Issues(state) result
// carries, -1 when it carries none. Both forge backends return the COMPLETE
// open set for the open and all views (the bounded window of issue #176
// applies to closed issues only), so counting the open rows of either is the
// repo's open issue count; the closed view holds no open issue at all.
func openIssueCount(issues []Issue, state string) int {
	if state != StateOpen && state != StateAll {
		return -1
	}
	n := 0
	for _, is := range issues {
		if is.State == StateOpen {
			n++
		}
	}
	return n
}

func (o *observed) Issue(ctx context.Context, number int) (Issue, error) {
	issue, err := o.t.Issue(ctx, number)
	o.report(OpIssue, err)
	return issue, err
}

func (o *observed) CreateComment(ctx context.Context, number int, body string) error {
	err := o.t.CreateComment(ctx, number, body)
	o.report(OpCreateComment, err)
	return err
}

func (o *observed) Pulls(ctx context.Context) ([]PullRef, error) {
	pulls, err := o.t.Pulls(ctx)
	o.report(OpPulls, err)
	o.reportRead(ctx, OpPulls, err, -1)
	return pulls, err
}

func (o *observed) PullsForHead(ctx context.Context, head, base string) ([]PullRef, error) {
	pulls, err := o.t.PullsForHead(ctx, head, base)
	o.report(OpPullsForHead, err)
	o.reportRead(ctx, OpPullsForHead, err, -1)
	return pulls, err
}

func (o *observed) Pull(ctx context.Context, number int) (PullDetail, error) {
	pull, err := o.t.Pull(ctx, number)
	o.report(OpPull, err)
	return pull, err
}

func (o *observed) Checks(ctx context.Context, number int) ([]Check, error) {
	checks, err := o.t.Checks(ctx, number)
	o.report(OpChecks, err)
	return checks, err
}

func (o *observed) CheckLog(ctx context.Context, number int, name string) (CheckLogResult, error) {
	res, err := o.t.CheckLog(ctx, number, name)
	o.report(OpCheckLog, err)
	return res, err
}

func (o *observed) CreatePull(ctx context.Context, head, base, title, body string) (PullRef, error) {
	pull, err := o.t.CreatePull(ctx, head, base, title, body)
	o.report(OpCreatePull, err)
	return pull, err
}

func (o *observed) MergePull(ctx context.Context, number int, opts MergeOptions) (MergeResult, error) {
	pull, err := o.t.MergePull(ctx, number, opts)
	o.report(OpMergePull, err)
	return pull, err
}

func (o *observed) Reviews(ctx context.Context, number int) ([]Review, error) {
	reviews, err := o.t.Reviews(ctx, number)
	o.report(OpReviews, err)
	return reviews, err
}

func (o *observed) RerequestReview(ctx context.Context, number int) error {
	err := o.t.RerequestReview(ctx, number)
	o.report(OpRerequestReview, err)
	return err
}

func (o *observed) CommentPull(ctx context.Context, number int, body string) error {
	err := o.t.CommentPull(ctx, number, body)
	o.report(OpCommentPull, err)
	return err
}

func (o *observed) PullComments(ctx context.Context, number int) ([]Comment, error) {
	comments, err := o.t.PullComments(ctx, number)
	o.report(OpPullComments, err)
	return comments, err
}

func (o *observed) CloseIssue(ctx context.Context, number int) error {
	err := o.t.CloseIssue(ctx, number)
	o.report(OpCloseIssue, err)
	return err
}

func (o *observed) CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error) {
	issue, err := o.t.CreateIssue(ctx, title, body, labels)
	o.report(OpCreateIssue, err)
	return issue, err
}

func (o *observed) EditIssue(ctx context.Context, number int, edit IssueEdit) (Issue, error) {
	issue, err := o.t.EditIssue(ctx, number, edit)
	o.report(OpEditIssue, err)
	return issue, err
}

func (o *observed) AddIssueLabels(ctx context.Context, number int, labels []string) error {
	err := o.t.AddIssueLabels(ctx, number, labels)
	o.report(OpAddIssueLabels, err)
	return err
}

func (o *observed) RemoveIssueLabels(ctx context.Context, number int, labels []string) error {
	err := o.t.RemoveIssueLabels(ctx, number, labels)
	o.report(OpRemoveIssueLabels, err)
	return err
}

func (o *observed) Labels(ctx context.Context) ([]Label, error) {
	labels, err := o.t.Labels(ctx)
	o.report(OpLabels, err)
	return labels, err
}

func (o *observed) EnsureLabel(ctx context.Context, name, color, description string) (Label, error) {
	label, err := o.t.EnsureLabel(ctx, name, color, description)
	o.report(OpEnsureLabel, err)
	return label, err
}
