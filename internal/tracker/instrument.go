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

import "context"

// Operation vocabulary for the observer seam — one constant per Tracker
// method, bounded by construction (metric label values).
const (
	OpReadyIssues       = "ready"
	OpIssues            = "issues"
	OpIssue             = "issue"
	OpCreateComment     = "comment"
	OpPulls             = "pulls"
	OpPull              = "pull"
	OpCreatePull        = "create_pull"
	OpCloseIssue        = "close"
	OpCreateIssue       = "create_issue"
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

// instrument wraps t for the observer; a nil observer returns t unwrapped.
func (r *Registry) instrument(t Tracker, binding string) Tracker {
	if r.observe == nil {
		return t
	}
	return &observed{t: t, binding: binding, obs: r.observe}
}

// observed decorates a Tracker with per-call observer reports. Results and
// errors pass through untouched.
type observed struct {
	t       Tracker
	binding string
	obs     Observer
}

func (o *observed) report(op string, err error) { o.obs(o.binding, op, err == nil) }

// ForRun forwards the identity-rescoping seam (RunScoper) through the
// decorator: the re-scoped backend is re-wrapped so its calls keep
// reporting, and a backend without the seam stays wrapped unchanged.
// Without this, wiring the observer would hide the builtin tracker's
// ForRun from the agent API and agent comments would silently fall back
// to the operator identity.
func (o *observed) ForRun(runID string) Tracker {
	if rs, ok := o.t.(RunScoper); ok {
		return &observed{t: rs.ForRun(runID), binding: o.binding, obs: o.obs}
	}
	return o
}

var _ RunScoper = (*observed)(nil)

func (o *observed) ReadyIssues(ctx context.Context) ([]Issue, error) {
	issues, err := o.t.ReadyIssues(ctx)
	o.report(OpReadyIssues, err)
	return issues, err
}

func (o *observed) Issues(ctx context.Context, state string) ([]Issue, error) {
	issues, err := o.t.Issues(ctx, state)
	o.report(OpIssues, err)
	return issues, err
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
	return pulls, err
}

func (o *observed) Pull(ctx context.Context, number int) (PullDetail, error) {
	pull, err := o.t.Pull(ctx, number)
	o.report(OpPull, err)
	return pull, err
}

func (o *observed) CreatePull(ctx context.Context, head, base, title, body string) (PullRef, error) {
	pull, err := o.t.CreatePull(ctx, head, base, title, body)
	o.report(OpCreatePull, err)
	return pull, err
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
