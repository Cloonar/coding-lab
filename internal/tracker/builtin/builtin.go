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

// CloseIssue transitions an issue to closed (stamping closed_at).
func (t *Tracker) CloseIssue(ctx context.Context, number int) error {
	if _, err := t.store.UpdateIssue(ctx, t.repoID, number,
		store.IssueUpdate{State: store.Set(store.IssueStateClosed)}, t.now()); err != nil {
		return fmt.Errorf("builtin close issue %d: %w", number, err)
	}
	return nil
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

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
