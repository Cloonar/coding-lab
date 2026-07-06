// Package builtin is the store-backed implementation of the tracker.Tracker
// seam (design §4d, D11). It answers the same vocabulary as the Forgejo REST
// client from lab's own database, so a repo with tracker_binding=builtin gets
// the identical issue/comment/PR contract without any forge. Change requests
// stand in for pull requests, but they land in M6 — in M4 Pulls is empty and
// CreatePull is not yet available.
package builtin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// ErrChangeRequestsUnavailable is returned by CreatePull in M4: the built-in
// change-request surface (the PR analogue) lands in M6. It is a clear, typed
// signal rather than a silent stub so a premature caller fails loudly.
var ErrChangeRequestsUnavailable = errors.New("builtin tracker: change requests are not available until M6")

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
// comments; the operator path keeps the default from New.
func (t *Tracker) ForRun(runID string) *Tracker {
	c := *t
	c.author = author{kind: store.CommentAuthorRun, runID: &runID}
	return &c
}

var (
	_ tracker.Tracker        = (*Tracker)(nil)
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

// Pulls is empty in M4: built-in change requests (the PR analogue) land in M6.
// The reaper's done-signal over these arrives with them.
func (t *Tracker) Pulls(ctx context.Context) ([]tracker.PullRef, error) {
	return []tracker.PullRef{}, nil
}

// CreatePull is not available until M6 (built-in change requests).
func (t *Tracker) CreatePull(ctx context.Context, head, base, title, body string) (tracker.PullRef, error) {
	return tracker.PullRef{}, ErrChangeRequestsUnavailable
}

// CloseIssue transitions an issue to closed (stamping closed_at).
func (t *Tracker) CloseIssue(ctx context.Context, number int) error {
	if _, err := t.store.UpdateIssue(ctx, t.repoID, number,
		store.IssueUpdate{State: store.Set(store.IssueStateClosed)}, t.now()); err != nil {
		return fmt.Errorf("builtin close issue %d: %w", number, err)
	}
	return nil
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
