package httpapi

// Issue surface (pinned M4 contract). Read views answer for BOTH tracker
// bindings through the tracker registry: a builtin-bound repo reads lab's own
// store, a forge-bound one proxies the Forgejo REST client. Mutations are
// builtin-only — on a forge-bound repo issues are managed on the forge, so
// every mutation answers the pinned 409. Every builtin mutation publishes
// issue.changed on the bus (clients refetch on event, design §5).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// EventIssueChanged is the SSE event name published on any built-in
// issue/comment/label mutation (brief §8.1).
const EventIssueChanged = "issue.changed"

// forgeTrackerMessage is the pinned 409 body for builtin-only mutations
// attempted on a forge-bound repo.
const forgeTrackerMessage = "repository uses a forge tracker; manage issues on the forge"

// publishIssueChanged emits issue.changed for repoID. Same envelope shape as
// every repo-scoped event ({type, repoID}).
func (s *Server) publishIssueChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventIssueChanged, Payload: struct {
		Type   string `json:"type"`
		RepoID string `json:"repoID"`
	}{Type: EventIssueChanged, RepoID: repoID}})
}

// issueResponse is the pinned list-view issue JSON (comments_count, no
// comment bodies).
type issueResponse struct {
	Number        int      `json:"number"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	State         string   `json:"state"`
	Labels        []string `json:"labels"`
	CommentsCount int      `json:"comments_count"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// issueDetailResponse is the pinned detail JSON: the full comment thread
// instead of a count.
type issueDetailResponse struct {
	Number    int               `json:"number"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	State     string            `json:"state"`
	Labels    []string          `json:"labels"`
	Comments  []commentResponse `json:"comments"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

type commentResponse struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// trackerIssueListJSON maps a tracker-vocabulary issue onto the list shape.
// List views load no comments (pinned contract); the real thread size travels
// in the vocabulary's CommentsCount, populated by both backends.
func trackerIssueListJSON(is tracker.Issue) issueResponse {
	labels := is.Labels
	if labels == nil {
		labels = []string{}
	}
	return issueResponse{
		Number:        is.Number,
		Title:         is.Title,
		Body:          is.Body,
		State:         is.State,
		Labels:        labels,
		CommentsCount: is.CommentsCount,
		CreatedAt:     store.FormatTime(is.CreatedAt),
		UpdatedAt:     store.FormatTime(is.UpdatedAt),
	}
}

func trackerIssueDetailJSON(is tracker.Issue) issueDetailResponse {
	labels := is.Labels
	if labels == nil {
		labels = []string{}
	}
	comments := make([]commentResponse, 0, len(is.Comments))
	for _, c := range is.Comments {
		comments = append(comments, commentResponse{
			Author:    c.Author,
			Body:      c.Body,
			CreatedAt: store.FormatTime(c.CreatedAt),
		})
	}
	return issueDetailResponse{
		Number:    is.Number,
		Title:     is.Title,
		Body:      is.Body,
		State:     is.State,
		Labels:    labels,
		Comments:  comments,
		CreatedAt: store.FormatTime(is.CreatedAt),
		UpdatedAt: store.FormatTime(is.UpdatedAt),
	}
}

// storeIssueListJSON maps a store issue (builtin list view) onto the list
// shape — the store carries the real comment count.
func storeIssueListJSON(is store.Issue) issueResponse {
	labels := is.Labels
	if labels == nil {
		labels = []string{}
	}
	return issueResponse{
		Number:        is.Number,
		Title:         is.Title,
		Body:          is.Body,
		State:         is.State,
		Labels:        labels,
		CommentsCount: is.CommentCount,
		CreatedAt:     store.FormatTime(is.CreatedAt),
		UpdatedAt:     store.FormatTime(is.UpdatedAt),
	}
}

// storeIssueDetailJSON renders a builtin mutation's response: the store row
// with its thread. Comment authors are the author_kind vocabulary (operator |
// run) — exactly what the builtin tracker reports for the same issue.
func storeIssueDetailJSON(is store.Issue) issueDetailResponse {
	labels := is.Labels
	if labels == nil {
		labels = []string{}
	}
	comments := make([]commentResponse, 0, len(is.Comments))
	for _, c := range is.Comments {
		comments = append(comments, commentResponse{
			Author:    c.AuthorKind,
			Body:      c.Body,
			CreatedAt: store.FormatTime(c.CreatedAt),
		})
	}
	return issueDetailResponse{
		Number:    is.Number,
		Title:     is.Title,
		Body:      is.Body,
		State:     is.State,
		Labels:    labels,
		Comments:  comments,
		CreatedAt: store.FormatTime(is.CreatedAt),
		UpdatedAt: store.FormatTime(is.UpdatedAt),
	}
}

// loadRepo resolves {id} to a repo, answering 404/500 itself. ok=false means
// the response was already written.
func (s *Server) loadRepo(w http.ResponseWriter, r *http.Request) (store.Repo, bool) {
	repo, err := s.store.RepoByID(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			s.internalError(w, "loading repo", err)
		}
		return store.Repo{}, false
	}
	return repo, true
}

// trackerForRepo resolves repo's Tracker via the registry. The typed
// resolution sentinels (unsupported forge kind, missing/wrong-kind forge
// credential, unparseable remote, unknown binding) are configuration
// conflicts → 409 with the diagnostic; anything else (vault, store) is a 500.
func (s *Server) trackerForRepo(w http.ResponseWriter, r *http.Request, repo store.Repo) (tracker.Tracker, bool) {
	tk, err := s.tracker.TrackerFor(r.Context(), repo)
	if err != nil {
		switch {
		case errors.Is(err, tracker.ErrForgeUnsupported),
			errors.Is(err, tracker.ErrForgeCredentialMissing),
			errors.Is(err, tracker.ErrForgeCredentialKind),
			errors.Is(err, tracker.ErrForgeHost),
			errors.Is(err, tracker.ErrRemotePath),
			errors.Is(err, tracker.ErrUnknownBinding):
			writeError(w, http.StatusConflict, err.Error())
		default:
			s.internalError(w, "resolving tracker", err)
		}
		return nil, false
	}
	return tk, true
}

// writeTrackerError maps a tracker call failure: a miss is a 404 on EITHER
// binding (the builtin tracker wraps store.ErrNotFound, the forgejo client
// wraps tracker.ErrNotFound around an upstream 404 — a stale issue link is
// not a forge outage); any other forge-bound failure is an upstream error →
// 502 carrying the diagnostic (the forgejo client's errors hold operation
// context and never the token).
func (s *Server) writeTrackerError(w http.ResponseWriter, doing string, repo store.Repo, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, tracker.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if repo.TrackerBinding == store.TrackerBindingForge {
		s.log.Warn(doing, "component", "httpapi", "repo", repo.ID, "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.internalError(w, doing, err)
}

// requireBuiltinTracker answers the pinned 409 when repo is not builtin-bound.
// ok=false means the response was written.
func (s *Server) requireBuiltinTracker(w http.ResponseWriter, repo store.Repo) bool {
	if repo.TrackerBinding != store.TrackerBindingBuiltin {
		writeError(w, http.StatusConflict, forgeTrackerMessage)
		return false
	}
	return true
}

// issueNumber parses the {n} path segment; a non-numeric or non-positive
// value names no issue → the caller answers 404.
func issueNumber(r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// filterTrackerIssuesByLabel keeps the issues carrying label (the client-side
// label filter for the forge list view — the pinned contract).
func filterTrackerIssuesByLabel(issues []tracker.Issue, label string) []tracker.Issue {
	out := make([]tracker.Issue, 0, len(issues))
	for _, is := range issues {
		for _, l := range is.Labels {
			if l == label {
				out = append(out, is)
				break
			}
		}
	}
	return out
}

// handleIssueList is GET /api/v1/repos/{id}/issues?state=open|closed|all&label=<name>.
// Builtin repos read the store (real comment counts); forge repos proxy the
// REST client with the label filter applied client-side. state defaults to
// open (the list view).
func (s *Server) handleIssueList(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	state := r.URL.Query().Get("state")
	if state == "" {
		state = tracker.StateOpen
	}
	switch state {
	case tracker.StateOpen, tracker.StateClosed, tracker.StateAll:
	default:
		writeError(w, http.StatusBadRequest, "state must be open, closed, or all")
		return
	}
	label := r.URL.Query().Get("label")

	items := make([]issueResponse, 0)
	if repo.TrackerBinding == store.TrackerBindingBuiltin {
		issues, err := s.store.IssuesByRepo(r.Context(), repo.ID, state)
		if err != nil {
			s.internalError(w, "listing issues", err)
			return
		}
		for _, is := range issues {
			if label != "" && !hasLabelName(is.Labels, label) {
				continue
			}
			items = append(items, storeIssueListJSON(is))
		}
	} else {
		tk, ok := s.trackerForRepo(w, r, repo)
		if !ok {
			return
		}
		issues, err := tk.Issues(r.Context(), state)
		if err != nil {
			s.writeTrackerError(w, "listing issues", repo, err)
			return
		}
		if label != "" {
			issues = filterTrackerIssuesByLabel(issues, label)
		}
		for _, is := range issues {
			items = append(items, trackerIssueListJSON(is))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"binding": repo.TrackerBinding,
		"issues":  items,
	})
}

func hasLabelName(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// handleIssueGet is GET /api/v1/repos/{id}/issues/{n}: one issue in full,
// with comments, through the Tracker for either binding.
func (s *Server) handleIssueGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	tk, ok := s.trackerForRepo(w, r, repo)
	if !ok {
		return
	}
	is, err := tk.Issue(r.Context(), n)
	if err != nil {
		s.writeTrackerError(w, "loading issue", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, trackerIssueDetailJSON(is))
}

// handleReadyList is GET /api/v1/repos/{id}/ready: the ready-for-agent queue
// (Tracker.ReadyIssues, either binding).
func (s *Server) handleReadyList(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	tk, ok := s.trackerForRepo(w, r, repo)
	if !ok {
		return
	}
	issues, err := tk.ReadyIssues(r.Context())
	if err != nil {
		s.writeTrackerError(w, "listing ready issues", repo, err)
		return
	}
	items := make([]issueResponse, 0, len(issues))
	for _, is := range issues {
		items = append(items, trackerIssueListJSON(is))
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": items})
}

type issueCreateRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// handleIssueCreate is POST /api/v1/repos/{id}/issues (builtin only): 201
// with the issue; numbers run 1,2,… per repo.
func (s *Server) handleIssueCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinTracker(w, repo) {
		return
	}
	var req issueCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	labelIDs, err := s.labelIDsByName(r.Context(), repo.ID, req.Labels)
	if err != nil {
		s.writeLabelResolveError(w, err)
		return
	}

	// Create + label attach run in ONE store transaction: a failed label
	// attach rolls the whole create back (no committed issue without its
	// issue.changed, no duplicate number on retry).
	is, err := s.store.CreateIssueWithLabels(r.Context(), repo.ID, req.Title, req.Body, labelIDs, s.now())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound) && len(labelIDs) > 0:
			// The repo was resolved by loadRepo moments ago, so the not-found
			// is a label deleted between name resolution and the transaction.
			writeError(w, http.StatusBadRequest, "unknown label")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		default:
			s.internalError(w, "creating issue", err)
		}
		return
	}
	s.publishIssueChanged(repo.ID)
	writeJSON(w, http.StatusCreated, storeIssueDetailJSON(is))
}

// handleIssueUpdate is PATCH /api/v1/repos/{id}/issues/{n} (builtin only):
// title/body/state, with closed_at stamped on close and cleared on reopen
// (in the store accessor).
func (s *Server) handleIssueUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinTracker(w, repo) {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var body map[string]json.RawMessage
	if decodeJSON(w, r, &body) != nil {
		return
	}
	var u store.IssueUpdate
	for key, raw := range body {
		var err error
		switch key {
		case "title":
			u.Title, err = patchString(raw, key)
			if err == nil && strings.TrimSpace(u.Title.Value) == "" {
				err = fmt.Errorf("title must not be empty")
			}
		case "body":
			u.Body, err = patchString(raw, key)
		case "state":
			u.State, err = patchString(raw, key)
			if err == nil && u.State.Value != store.IssueStateOpen && u.State.Value != store.IssueStateClosed {
				err = fmt.Errorf("state must be open or closed")
			}
		default:
			err = fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	is, err := s.store.UpdateIssue(r.Context(), repo.ID, n, u, s.now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "updating issue", err)
		return
	}
	s.publishIssueChanged(repo.ID)
	writeJSON(w, http.StatusOK, storeIssueDetailJSON(is))
}

type commentCreateRequest struct {
	Body string `json:"body"`
}

// handleIssueComment is POST /api/v1/repos/{id}/issues/{n}/comments (builtin
// only): 201, authored as the operator.
func (s *Server) handleIssueComment(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinTracker(w, repo) {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req commentCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	is, err := s.store.IssueByRepoNumber(r.Context(), repo.ID, n)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "loading issue", err)
		return
	}
	c, err := s.store.CreateIssueComment(r.Context(), is.ID, store.CommentAuthorOperator, nil, req.Body, s.now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "creating comment", err)
		return
	}
	s.publishIssueChanged(repo.ID)
	writeJSON(w, http.StatusCreated, commentResponse{
		Author:    c.AuthorKind,
		Body:      c.Body,
		CreatedAt: store.FormatTime(c.CreatedAt),
	})
}

// handleIssueLabels is PUT /api/v1/repos/{id}/issues/{n}/labels (builtin
// only): replace the issue's label set; an unknown label name is a 400. The
// body must carry the "labels" key — this PUT is destructive (an empty set
// clears every label), so an absent key (e.g. a typoed field name) is a 400,
// never a silent clear; unknown keys are rejected like the sibling PATCHes.
func (s *Server) handleIssueLabels(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinTracker(w, repo) {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var body map[string]json.RawMessage
	if decodeJSON(w, r, &body) != nil {
		return
	}
	for key := range body {
		if key != "labels" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown field %q", key))
			return
		}
	}
	raw, ok := body["labels"]
	if !ok {
		writeError(w, http.StatusBadRequest, "labels is required")
		return
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil || names == nil {
		writeError(w, http.StatusBadRequest, "labels must be an array of strings")
		return
	}
	labelIDs, err := s.labelIDsByName(r.Context(), repo.ID, names)
	if err != nil {
		s.writeLabelResolveError(w, err)
		return
	}
	is, err := s.store.IssueByRepoNumber(r.Context(), repo.ID, n)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "loading issue", err)
		return
	}
	if err := s.store.SetIssueLabels(r.Context(), is.ID, labelIDs, s.now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "setting issue labels", err)
		return
	}
	full, err := s.store.IssueByRepoNumber(r.Context(), repo.ID, n)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "loading issue", err)
		return
	}
	s.publishIssueChanged(repo.ID)
	writeJSON(w, http.StatusOK, storeIssueDetailJSON(full))
}

// errUnknownLabel marks the caller's error in label-name resolution: a name
// that resolves to no label of the repo → the API's 400. Every other
// labelIDsByName failure is a store failure → the callers' opaque 500.
var errUnknownLabel = errors.New("unknown label")

// writeLabelResolveError maps a labelIDsByName failure: the unknown-name user
// error is a 400 carrying the offending name; a store failure stays an opaque
// logged 500 like every other store failure (never internal diagnostics in
// the error envelope).
func (s *Server) writeLabelResolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, errUnknownLabel) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.internalError(w, "loading labels", err)
}

// labelIDsByName resolves label names to ids within a repo (deduplicated).
// An unknown name wraps errUnknownLabel and carries the offending name.
func (s *Server) labelIDsByName(ctx context.Context, repoID string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	labels, err := s.store.LabelsByRepo(ctx, repoID)
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
			return nil, fmt.Errorf("%w %q", errUnknownLabel, name)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}
