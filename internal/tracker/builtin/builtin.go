// Package builtin is the store-backed implementation of the tracker.Tracker
// seam (design §4d, D11). It answers the same vocabulary as the Forgejo REST
// client from lab's own database, so a repo with tracker_binding=builtin gets
// the identical issue/comment/PR contract without any forge. Change requests
// (M6) stand in for pull requests: Pulls lists them across all states — the
// reaper's done-signal for builtin repos — and CreatePull opens one.
package builtin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/crmerge"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// Tracker is a store-backed tracker.Tracker bound to one repo. It carries the
// comment-author identity CreateComment writes with: New defaults to the
// operator; ForRun switches it to a specific run (the M5 agent path).
type Tracker struct {
	store  *store.Store
	repoID string
	now    func() time.Time
	author author
	// merger is the shared CR-merge service MergePull routes into (ADR-0011).
	// Nil when the wiring layer provided none — MergePull then fails loud
	// rather than pretending to merge.
	merger tracker.CRMerger
}

// author is the identity CreateComment stamps on a new comment.
type author struct {
	kind  string  // store.CommentAuthorOperator | store.CommentAuthorRun
	runID *string // set only when kind is run
}

// New builds a built-in tracker for one repo. It matches tracker.BuiltinFactory
// so the registry can inject it, and it defaults comments to the operator
// identity — M5 passes run identity via ForRun (per the M4 pinned contract).
func New(cfg tracker.BuiltinConfig) tracker.Tracker {
	return &Tracker{
		store:  cfg.Store,
		repoID: cfg.RepoID,
		now:    time.Now,
		author: author{kind: store.CommentAuthorOperator},
		merger: cfg.Merger,
	}
}

// ForRun returns a copy of t whose CreateComment authors comments as the given
// run (author_kind=run, run_id set). M5 uses it to attribute an AFK run's
// comments; the operator path keeps the default from New. It returns the
// interface (tracker.RunScoper's signature) so callers rescope through the
// seam without ever naming this concrete type — the registry may hand them
// a decorated tracker, not a *Tracker.
func (t *Tracker) ForRun(runID string) tracker.Tracker {
	c := *t
	c.author = author{kind: store.CommentAuthorRun, runID: &runID}
	return &c
}

var (
	_ tracker.Tracker        = (*Tracker)(nil)
	_ tracker.RunScoper      = (*Tracker)(nil)
	_ tracker.BuiltinFactory = New // the registry injects New as its builtin factory
)

// ReadyIssues lists open issues carrying tracker.ReadyLabel — the AFK ready
// queue, filtered client-side on the label names the store already loads.
func (t *Tracker) ReadyIssues(ctx context.Context) ([]tracker.Issue, error) {
	issues, err := t.store.IssuesByRepo(ctx, t.repoID, store.IssueStateOpen)
	if err != nil {
		return nil, fmt.Errorf("builtin ready issues: %w", err)
	}
	out := make([]tracker.Issue, 0)
	for _, is := range issues {
		if hasLabel(is.Labels, tracker.ReadyLabel) {
			out = append(out, toTrackerIssue(is))
		}
	}
	return out, nil
}

// Issues lists issues by state (a list view: no comments loaded).
func (t *Tracker) Issues(ctx context.Context, state string) ([]tracker.Issue, error) {
	issues, err := t.store.IssuesByRepo(ctx, t.repoID, state)
	if err != nil {
		return nil, fmt.Errorf("builtin issues: %w", err)
	}
	out := make([]tracker.Issue, 0, len(issues))
	for _, is := range issues {
		out = append(out, toTrackerIssue(is))
	}
	return out, nil
}

// Issue returns one issue in full, including its comment thread.
func (t *Tracker) Issue(ctx context.Context, number int) (tracker.Issue, error) {
	is, err := t.store.IssueByRepoNumber(ctx, t.repoID, number)
	if err != nil {
		return tracker.Issue{}, fmt.Errorf("builtin issue %d: %w", number, err)
	}
	out := toTrackerIssue(is)
	out.Comments = make([]tracker.Comment, 0, len(is.Comments))
	for _, c := range is.Comments {
		out.Comments = append(out.Comments, tracker.Comment{
			Author:    c.AuthorKind,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return out, nil
}

// CreateComment posts a comment on an issue, authored per this tracker's
// identity (operator by default, a run after ForRun).
func (t *Tracker) CreateComment(ctx context.Context, number int, body string) error {
	is, err := t.store.IssueByRepoNumber(ctx, t.repoID, number)
	if err != nil {
		return fmt.Errorf("builtin comment on issue %d: %w", number, err)
	}
	if _, err := t.store.CreateIssueComment(ctx, is.ID, t.author.kind, t.author.runID, body, t.now()); err != nil {
		return fmt.Errorf("builtin comment on issue %d: %w", number, err)
	}
	return nil
}

// Pulls lists ALL of the repo's change requests, across all three states —
// THE reaper done-signal for builtin repos: a CR's state maps 1:1 onto the
// tracker's PR vocabulary (open|merged|closed are the same strings), so
// tracker.PRPresent treats an open or merged CR whose head matches a run's
// branch as done, and a closed-unmerged one as "no PR", exactly like a forge.
func (t *Tracker) Pulls(ctx context.Context) ([]tracker.PullRef, error) {
	crs, err := t.store.CRsByRepo(ctx, t.repoID, store.CRStateAll)
	if err != nil {
		return nil, fmt.Errorf("builtin pulls: %w", err)
	}
	out := make([]tracker.PullRef, 0, len(crs))
	for _, cr := range crs {
		out = append(out, toPullRef(cr))
	}
	return out, nil
}

// Pull returns one change request in full detail, body included — the read
// behind labctl pr view. An unknown number surfaces store.ErrNotFound (the
// builtin not-found sentinel, like Issue).
func (t *Tracker) Pull(ctx context.Context, number int) (tracker.PullDetail, error) {
	cr, err := t.store.CRByRepoNumber(ctx, t.repoID, number)
	if err != nil {
		return tracker.PullDetail{}, fmt.Errorf("builtin pull %d: %w", number, err)
	}
	return toPullDetail(cr), nil
}

// Checks returns the CI check rows for change request `number`'s current
// head commit — always a non-nil empty slice: the built-in tracker has no CI
// machinery to report on, and an empty result is success (tracker.ChecksState
// reads it as tracker.ChecksNone), not an error — a built-in repo truthfully
// has no checks. It resolves the CR through the same store lookup Pull uses,
// so an unknown number surfaces store.ErrNotFound exactly like Pull.
func (t *Tracker) Checks(ctx context.Context, number int) ([]tracker.Check, error) {
	if _, err := t.store.CRByRepoNumber(ctx, t.repoID, number); err != nil {
		return nil, fmt.Errorf("builtin checks %d: %w", number, err)
	}
	return []tracker.Check{}, nil
}

// CreatePull opens a change request from head onto base — the builtin answer
// to a forge PR create (the agent API's POST /prs routes here for
// builtin-bound repos). The issues the body closes are parsed with the shared
// closing-keyword grammar (tracker.ParseCloses) and persisted as cr_closes
// rows, so the operator's later merge knows which built-in issues to close.
func (t *Tracker) CreatePull(ctx context.Context, head, base, title, body string) (tracker.PullRef, error) {
	// Forge parity: one OPEN CR per head branch. An agent whose first create
	// timed out client-side retries the identical call; without this guard the
	// retry files a duplicate CR (Forgejo answers 409 for the same sequence).
	// Check-then-insert is race-adequate here: builtin CRs are created only
	// through this path and the agent API serializes per run.
	if existing, err := t.store.OpenCRByHead(ctx, t.repoID, head); err == nil {
		return toPullRef(existing), fmt.Errorf("builtin create pull: change request #%d: %w",
			existing.Number, tracker.ErrDuplicateOpenPull)
	} else if !errors.Is(err, store.ErrNotFound) {
		return tracker.PullRef{}, fmt.Errorf("builtin create pull: duplicate check: %w", err)
	}
	cr, err := t.store.CreateCR(ctx, t.repoID, title, body, head, base, tracker.ParseCloses(body), t.now())
	if err != nil {
		return tracker.PullRef{}, fmt.Errorf("builtin create pull: %w", err)
	}
	return toPullRef(cr), nil
}

// MergePull lands the built-in change request `number` by routing into the
// shared crmerge service (ADR-0011 orchestration: per-CR serialization,
// cancellation-immune git window, Closes #N closure, cr.changed/issue.changed
// events) and returns the merged PullRef. Reusing that service — rather than
// reimplementing the merge here — is what keeps the operator route and this
// agent route byte-for-byte identical.
//
// Convergent: an already-merged CR is a no-op SUCCESS naming the merged state,
// not an error (the service returns store.ErrCRNotOpen without touching git; a
// re-read distinguishes merged → converge from closed-unmerged → reject),
// matching CreatePull's duplicate handling and gitx.CRMerge's own convergence.
// Every refusal the service surfaces — git head-missing / push-rejected /
// merge-conflict, a closed-unmerged CR, or no configured author identity —
// becomes tracker.ErrMergeRejected carrying the backend's OWN words; an
// unknown number surfaces store.ErrNotFound (→ 404). Without a merge service
// wired it fails loud rather than pretending to merge.
func (t *Tracker) MergePull(ctx context.Context, number int) (tracker.PullRef, error) {
	if t.merger == nil {
		return tracker.PullRef{}, fmt.Errorf("builtin merge pull %d: no CR-merge service wired", number)
	}
	merged, err := t.merger.Merge(ctx, t.repoID, number)
	if err == nil {
		return toPullRef(merged), nil
	}
	if errors.Is(err, store.ErrCRNotOpen) {
		// Not-open without a git run: converge iff it is already merged. A
		// transient re-read failure is NOT a refusal — surface it raw so the
		// agent gets a 500, not a spurious "merge refused" 409.
		cr, rerr := t.store.CRByRepoNumber(ctx, t.repoID, number)
		if rerr != nil {
			return tracker.PullRef{}, fmt.Errorf("builtin merge pull %d: %w", number, rerr)
		}
		if cr.State == store.CRStateMerged {
			return toPullRef(cr), nil // convergent no-op success
		}
		// Closed-unmerged (no reopen): a genuine refusal.
		return tracker.PullRef{}, fmt.Errorf("%w: %s", tracker.ErrMergeRejected, err.Error())
	}
	// Only the backend's REAL refusals become ErrMergeRejected (→ 409 with the
	// words verbatim), matching the operator route's whitelist. Everything else
	// — an unknown number (store.ErrNotFound → 404), a transient DB error, a
	// credential misconfig — stays a raw error so the agent answers 404/500
	// rather than a permanent, retry-proof "merge refused", and so internal
	// diagnostics never leak as a refusal message.
	if errors.Is(err, gitx.ErrHeadMissing) || errors.Is(err, gitx.ErrPushRejected) ||
		errors.Is(err, gitx.ErrMergeConflict) || errors.Is(err, crmerge.ErrNoAuthorIdentity) {
		return tracker.PullRef{}, fmt.Errorf("%w: %s", tracker.ErrMergeRejected, err.Error())
	}
	return tracker.PullRef{}, fmt.Errorf("builtin merge pull %d: %w", number, err)
}

// CloseIssue transitions an issue to closed (stamping closed_at).
func (t *Tracker) CloseIssue(ctx context.Context, number int) error {
	if _, err := t.store.UpdateIssue(ctx, t.repoID, number,
		store.IssueUpdate{State: store.Set(store.IssueStateClosed)}, t.now()); err != nil {
		return fmt.Errorf("builtin close issue %d: %w", number, err)
	}
	return nil
}

// CreateIssue opens an issue authored per this tracker's identity (operator
// by default, the run after ForRun — issues share the comments' author
// vocabulary since migration 0002). Create and label attach run in ONE store
// transaction after strict name resolution, so an unknown label leaves no
// issue behind and releases no issue number.
func (t *Tracker) CreateIssue(ctx context.Context, title, body string, labels []string) (tracker.Issue, error) {
	labelIDs, err := t.labelIDsByName(ctx, labels)
	if err != nil {
		return tracker.Issue{}, fmt.Errorf("builtin create issue: %w", err)
	}
	is, err := t.store.CreateIssueWithLabels(ctx, t.repoID, title, body, labelIDs,
		t.author.kind, t.author.runID, t.now())
	if err != nil {
		return tracker.Issue{}, fmt.Errorf("builtin create issue: %w", err)
	}
	return toTrackerIssue(is), nil
}

// EditIssue applies a title/body patch by riding store.UpdateIssue — the same
// accessor CloseIssue and the operator PATCH use. A nil pointer leaves the
// column untouched; a non-nil one is store.Set, so a non-nil empty Body clears
// the body. State is never touched here (title/body only). Last-write-wins on
// any issue, open or closed; an unknown number surfaces store.ErrNotFound,
// consistent with the package's other single-issue methods. The result maps
// through toTrackerIssue (Comments nil, CommentsCount from the store) — the
// LIST shape CreateIssue returns.
func (t *Tracker) EditIssue(ctx context.Context, number int, edit tracker.IssueEdit) (tracker.Issue, error) {
	var u store.IssueUpdate
	if edit.Title != nil {
		u.Title = store.Set(*edit.Title)
	}
	if edit.Body != nil {
		u.Body = store.Set(*edit.Body)
	}
	is, err := t.store.UpdateIssue(ctx, t.repoID, number, u, t.now())
	if err != nil {
		return tracker.Issue{}, fmt.Errorf("builtin edit issue %d: %w", number, err)
	}
	return toTrackerIssue(is), nil
}

// AddIssueLabels attaches the named labels to an issue. All names resolve
// strictly BEFORE the first attach (tracker.ErrUnknownLabel on a miss — never
// an implicit create); attaching an already attached label is the store's
// idempotent no-op.
func (t *Tracker) AddIssueLabels(ctx context.Context, number int, labels []string) error {
	labelIDs, err := t.labelIDsByName(ctx, labels)
	if err != nil {
		return fmt.Errorf("builtin add labels to issue %d: %w", number, err)
	}
	is, err := t.store.IssueByRepoNumber(ctx, t.repoID, number)
	if err != nil {
		return fmt.Errorf("builtin add labels to issue %d: %w", number, err)
	}
	for _, labelID := range labelIDs {
		if err := t.store.AddIssueLabel(ctx, is.ID, labelID, t.now()); err != nil {
			return fmt.Errorf("builtin add labels to issue %d: %w", number, err)
		}
	}
	return nil
}

// RemoveIssueLabels detaches the named labels from an issue, under the same
// strict name resolution as AddIssueLabels. Removing a defined-but-unattached
// label is the store's idempotent no-op.
func (t *Tracker) RemoveIssueLabels(ctx context.Context, number int, labels []string) error {
	labelIDs, err := t.labelIDsByName(ctx, labels)
	if err != nil {
		return fmt.Errorf("builtin remove labels from issue %d: %w", number, err)
	}
	is, err := t.store.IssueByRepoNumber(ctx, t.repoID, number)
	if err != nil {
		return fmt.Errorf("builtin remove labels from issue %d: %w", number, err)
	}
	for _, labelID := range labelIDs {
		if err := t.store.RemoveIssueLabel(ctx, is.ID, labelID, t.now()); err != nil {
			return fmt.Errorf("builtin remove labels from issue %d: %w", number, err)
		}
	}
	return nil
}

// Labels lists the repo's labels ordered by name.
func (t *Tracker) Labels(ctx context.Context) ([]tracker.Label, error) {
	labels, err := t.store.LabelsByRepo(ctx, t.repoID)
	if err != nil {
		return nil, fmt.Errorf("builtin labels: %w", err)
	}
	out := make([]tracker.Label, 0, len(labels))
	for _, l := range labels {
		out = append(out, toTrackerLabel(l))
	}
	return out, nil
}

// EnsureLabel creates the label unless the repo already defines the name, and
// returns the label that exists afterwards. Create-first keeps it atomic
// under the per-repo name uniqueness: a lost race (or an existing label)
// surfaces as ErrNameTaken, which resolves to the existing row — the
// existing label's color/description are never overwritten.
func (t *Tracker) EnsureLabel(ctx context.Context, name, color, description string) (tracker.Label, error) {
	l, err := t.store.CreateLabel(ctx, t.repoID, name, color, description)
	if err == nil {
		return toTrackerLabel(l), nil
	}
	if !errors.Is(err, store.ErrNameTaken) {
		return tracker.Label{}, fmt.Errorf("builtin ensure label %q: %w", name, err)
	}
	labels, err := t.store.LabelsByRepo(ctx, t.repoID)
	if err != nil {
		return tracker.Label{}, fmt.Errorf("builtin ensure label %q: %w", name, err)
	}
	for _, l := range labels {
		if l.Name == name {
			return toTrackerLabel(l), nil
		}
	}
	// Name taken a moment ago but gone now: a concurrent delete won the race.
	// Surface it instead of looping — the caller's retry recreates it.
	return tracker.Label{}, fmt.Errorf("builtin ensure label %q: label vanished after name conflict: %w",
		name, store.ErrNotFound)
}

// labelIDsByName resolves label names to store ids within the repo,
// deduplicated, failing on the first name the repo does not define
// (tracker.ErrUnknownLabel, carrying the name). Nil in, nil out.
func (t *Tracker) labelIDsByName(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	labels, err := t.store.LabelsByRepo(ctx, t.repoID)
	if err != nil {
		return nil, fmt.Errorf("loading labels: %w", err)
	}
	byName := make(map[string]string, len(labels))
	for _, l := range labels {
		byName[l.Name] = l.ID
	}
	seen := make(map[string]struct{}, len(names))
	ids := make([]string, 0, len(names))
	for _, name := range names {
		id, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("%w %q", tracker.ErrUnknownLabel, name)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// toPullRef reduces a store CR to the tracker's PullRef vocabulary. The URL
// is the lab-relative SPA route of the CR's detail view (there is no forge
// web URL for a lab-internal CR); the state strings are shared, so State
// carries over verbatim.
func toPullRef(cr store.CR) tracker.PullRef {
	return tracker.PullRef{
		Number:     cr.Number,
		HeadBranch: cr.HeadBranch,
		State:      cr.State,
		URL:        fmt.Sprintf("/repos/%s/crs/%d", cr.RepoID, cr.Number),
	}
}

// toPullDetail maps a store CR onto the tracker's full-detail PR vocabulary,
// under the same URL convention as toPullRef (the CR's lab-relative SPA route).
func toPullDetail(cr store.CR) tracker.PullDetail {
	return tracker.PullDetail{
		Number:     cr.Number,
		Title:      cr.Title,
		Body:       cr.Body,
		State:      cr.State,
		HeadBranch: cr.HeadBranch,
		URL:        fmt.Sprintf("/repos/%s/crs/%d", cr.RepoID, cr.Number),
	}
}

// toTrackerIssue maps a store issue onto the tracker vocabulary, leaving
// Comments nil (the caller loads them only for the detail view) and carrying
// the store's comment count.
func toTrackerIssue(is store.Issue) tracker.Issue {
	labels := is.Labels
	if labels == nil {
		labels = []string{}
	}
	return tracker.Issue{
		Number:        is.Number,
		Title:         is.Title,
		Body:          is.Body,
		State:         is.State,
		Labels:        labels,
		CommentsCount: is.CommentCount,
		CreatedAt:     is.CreatedAt,
		UpdatedAt:     is.UpdatedAt,
	}
}

// toTrackerLabel maps a store label onto the tracker vocabulary (ids stay
// behind the seam — callers speak names).
func toTrackerLabel(l store.Label) tracker.Label {
	return tracker.Label{Name: l.Name, Color: l.Color, Description: l.Description}
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
