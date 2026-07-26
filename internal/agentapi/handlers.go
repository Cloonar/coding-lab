package agentapi

// The /agent/v1 handlers (brief §8.2, pinned M5 contract). Repo scope comes
// STRICTLY from the authenticated run's row (RunTokenInfo.RepoID) — no URL
// carries a repo id here, so a run can only reach its own repo. Response
// shapes mirror the operator API's issue JSON byte-for-byte (labctl and the
// SPA speak one vocabulary); errors are the canonical {"error": …} envelope.
//
// Error mapping (pinned): a write whose content matches a repo secret value →
// 400 naming the secret (issue #107, reject-not-redact — the run-token scan
// rejects the write rather than mangling it); upstream/store miss → 404;
// unknown label name → 400 naming the label (a typo must fail loudly, never
// create a garbage label); tracker configuration conflict (unsupported forge
// kind, missing/wrong-kind credential, bad remote/host, unknown binding) →
// 409; any other forge upstream failure → 502; builtin store failures →
// opaque 500.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/secrets"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/secretscan"
)

// EventCRChanged is the SSE event name a builtin PR create publishes: the
// created change request appears in the operator's CR list on the next
// refetch (brief §8.1). The name mirrors httpapi's constant — kept local,
// like jsonError, so agentapi stays free of an httpapi dependency.
const EventCRChanged = "cr.changed"

// EventIssueChanged is the SSE event name every builtin issue/label mutation
// publishes (create, edit, label add/remove, close, label ensure), exactly like
// the operator API's builtin mutations: the operator UI refetches on event.
// Forge-bound mutations publish nothing — the forge is the source of truth
// and the operator UI reads it on navigation.
const EventIssueChanged = "issue.changed"

// noClaimedIssueMessage answers GET /issue for a run without a claimed issue
// (manual instances have no issue_number on their run row).
const noClaimedIssueMessage = "run has no claimed issue"

// maxJSONBody bounds request bodies on the agent JSON endpoints.
const maxJSONBody = 1 << 20 // 1 MiB

// --- response shapes (operator-API parity) ----------------------------------

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

func issueListJSON(is tracker.Issue) issueResponse {
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

func issueDetailJSON(is tracker.Issue) issueDetailResponse {
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

// --- shared plumbing ---------------------------------------------------------

// writeJSON writes v with the given status in the canonical content type.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON reads the request body into dst; on failure it writes the 400
// and returns false (the caller just returns).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// internalError logs err and answers the canonical opaque 500.
func (s *Server) internalError(w http.ResponseWriter, doing string, err error) {
	s.log.Error(doing, "component", "agentapi", "err", err)
	jsonError(w, http.StatusInternalServerError, "internal error")
}

// runRepo resolves the authenticated run and its repo. ok=false means the
// response was already written. The repo id comes from the run row only —
// that IS the agent API's authorization scope.
func (s *Server) runRepo(w http.ResponseWriter, r *http.Request) (store.RunTokenInfo, store.Repo, bool) {
	info, ok := RunFromContext(r.Context())
	if !ok {
		// Unreachable behind AuthMiddleware; a bare handler is a wiring bug.
		s.internalError(w, "run missing from context", errors.New("agentapi: handler mounted without auth middleware"))
		return store.RunTokenInfo{}, store.Repo{}, false
	}
	repo, err := s.store.RepoByID(r.Context(), info.RepoID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The repo delete cascade removes runs and tokens, so this is a
			// vanishing race at worst.
			jsonError(w, http.StatusNotFound, "not found")
		} else {
			s.internalError(w, "loading repo", err)
		}
		return store.RunTokenInfo{}, store.Repo{}, false
	}
	return info, repo, true
}

// trackerFor resolves repo's Tracker. Typed resolution sentinels are
// configuration conflicts → 409 with the diagnostic (mirrors the operator
// API); anything else is a 500. ok=false means the response was written.
func (s *Server) trackerFor(w http.ResponseWriter, r *http.Request, repo store.Repo) (tracker.Tracker, bool) {
	tk, err := s.trackers.TrackerFor(r.Context(), repo)
	if err != nil {
		switch {
		case errors.Is(err, tracker.ErrForgeUnsupported),
			errors.Is(err, tracker.ErrForgeCredentialMissing),
			errors.Is(err, tracker.ErrForgeCredentialKind),
			errors.Is(err, tracker.ErrForgeHost),
			errors.Is(err, tracker.ErrRemotePath),
			errors.Is(err, tracker.ErrUnknownBinding):
			jsonError(w, http.StatusConflict, err.Error())
		default:
			s.internalError(w, "resolving tracker", err)
		}
		return nil, false
	}
	return tk, true
}

// writeTrackerError maps a tracker call failure: a run-token write whose
// content carries a repo secret value is a 400 whose message names the
// matching secret/field/form (issue #107 — the secretscan decorator rejects
// rather than redacts, and its error is safe to echo, carrying names only,
// never a value in any encoding); a miss is a 404 on either binding (builtin
// wraps store.ErrNotFound, the forgejo client wraps tracker.ErrNotFound around
// an upstream 404); an unknown label name is a 400 carrying the name (strict
// resolution on both bindings — the caller's typo, not an upstream failure);
// any other forge-bound failure is an upstream error → 502 with the diagnostic
// (the forge client's errors never carry the token); builtin store failures
// stay an opaque 500.
func (s *Server) writeTrackerError(w http.ResponseWriter, doing string, repo store.Repo, err error) {
	var blocked *secretscan.BlockedError
	if errors.As(err, &blocked) {
		// The message names the matched secret/field/form ONLY (secretscan
		// never renders a value), so it is safe to echo verbatim and there is
		// nothing diagnostic to log — mirror the ErrUnknownLabel branch. Placed
		// FIRST, ahead of the forge-generic 502 fold and the builtin opaque-500
		// fold, so a rejected leak is never miscoded as an upstream fault.
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, tracker.ErrUnknownLabel) {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, tracker.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	if errors.Is(err, tracker.ErrDuplicateOpenPull) {
		// Agent-retry after a timed-out create: the CR/PR exists; 409 with the
		// existing number in the message (forge parity - Forgejo 409s too).
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, tracker.ErrMergeRejected) {
		// A merge the backend refused (required check red, protected branch,
		// conflict, closed-unmerged): 409 with the backend's own words
		// verbatim — mergeability is the backend's call, not lab's.
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, tracker.ErrReviewRejected) {
		// A review operation the forge refused (a re-request it would not
		// accept): the ErrMergeRejected twin — 409 with the forge's own words
		// verbatim, reviewability being the backend's call, not lab's. The
		// rerequest handler's best-effort ping downgrades its own refusals to a
		// warning before reaching here; this branch keeps the mapping for any
		// path that surfaces the sentinel fatally. Placed beside
		// ErrMergeRejected, its documented mirror.
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, tracker.ErrUnsupported) {
		// A PR-comment or review-ping write on the built-in binding (the verdict
		// verbs compose over CommentPull; rerequest pings reviewers): the tracker
		// has no forge review model or CR comment thread yet, so it defers rather
		// than fakes — 409 with the "not supported on this tracker" message, the
		// wrong-binding-for-this-operation convention. Must precede the binding-
		// generic 502/500 folds below, or the built-in binding's refusal would
		// miscode as an opaque 500.
		jsonError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, tracker.ErrUnknownCheck) {
		// A CheckLog naming a context no head-commit Checks row carries — a typo,
		// or a check not yet registered against the current head (ADR-0060): 404
		// with the error's OWN message, which names the offending context. Unlike
		// the generic ErrNotFound fold above (opaque "not found"), this keeps the
		// name in the body so a reading agent sees WHICH check had no log to serve.
		// Placed with the other typed-miss branches, ahead of the binding-generic
		// folds.
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, tracker.ErrLogAdapterMismatch) {
		// The version-coupled Forgejo web log route answered a shape lab's log
		// adapter no longer recognizes — a forge upgrade moved the undocumented
		// endpoint out from under the coupling (ADR-0060): 502 with the error's
		// own message VERBATIM. It is deliberately loud and actionable (it names
		// the route, what it asked, what came back, and says to file an issue then
		// debug from a local repro), so it is NEVER paraphrased away into a generic
		// bad-gateway — an agent must always tell "lab is broken" from "no logs".
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	if errors.Is(err, tracker.ErrRateLimited) {
		// A forge rate limit (GitHub 403/429 with a spent hourly budget) is a
		// transient upstream throttle, not a lab fault: 503 with the reset hint
		// the client's message carries — a distinct status the caller simply
		// comes back later on, never a retry in-client (ADR-0015, typed error;
		// mirrors the operator API's httpapi precedent). Placed before the
		// generic forge branch so a throttle never folds into the opaque 502.
		s.log.Warn(doing, "component", "agentapi", "repo", repo.ID, "err", err)
		jsonError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if repo.TrackerBinding == store.TrackerBindingForge {
		s.log.Warn(doing, "component", "agentapi", "repo", repo.ID, "err", err)
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.internalError(w, doing, err)
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

// --- handlers ----------------------------------------------------------------

// handleClaimedIssue is GET /agent/v1/issue: the run's claimed issue in full
// (issue_number on the run row), with comments. A run without a claimed issue
// (manual instances) is a 404.
func (s *Server) handleClaimedIssue(w http.ResponseWriter, r *http.Request) {
	info, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	if info.IssueNumber == nil {
		jsonError(w, http.StatusNotFound, noClaimedIssueMessage)
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	is, err := tk.Issue(r.Context(), *info.IssueNumber)
	if err != nil {
		s.writeTrackerError(w, "loading claimed issue", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, issueDetailJSON(is))
}

// handleIssueList is GET /agent/v1/issues: the repo's open issues (list view,
// no comment bodies) — what `labctl issue list` renders.
func (s *Server) handleIssueList(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	issues, err := tk.Issues(r.Context(), tracker.StateOpen)
	if err != nil {
		s.writeTrackerError(w, "listing issues", repo, err)
		return
	}
	items := make([]issueResponse, 0, len(issues))
	for _, is := range issues {
		items = append(items, issueListJSON(is))
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": items})
}

// handleIssueGet is GET /agent/v1/issues/{n}: one issue of the run's repo in
// full, with comments.
func (s *Server) handleIssueGet(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	is, err := tk.Issue(r.Context(), n)
	if err != nil {
		s.writeTrackerError(w, "loading issue", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, issueDetailJSON(is))
}

type commentCreateRequest struct {
	Body string `json:"body"`
}

// handleCommentCreate is POST /agent/v1/issues/{n}/comments: post a comment
// as the run. On a builtin-bound repo the comment is authored
// author_kind=run with the run's id (the builtin tracker's ForRun identity
// seam); on a forge-bound repo it is a plain comment under the repo's forge
// token identity. The body passes the incogni sanitizer like every
// agent-authored body.
func (s *Server) handleCommentCreate(w http.ResponseWriter, r *http.Request) {
	info, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	var req commentCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		jsonError(w, http.StatusBadRequest, "body is required")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	tk = runScoped(tk, repo, info.RunID)
	if err := tk.CreateComment(r.Context(), n, s.sanitizeBody(repo, req.Body)); err != nil {
		s.writeTrackerError(w, "creating comment", repo, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

// runScoped rescopes tk to the run identity on builtin-bound repos, so the
// mutation is attributed to the run instead of the operator. Always through
// the RunScoper seam, never a concrete type assertion: the registry wraps
// trackers (metrics observer) and a concrete assertion against the wrapper
// would silently skip ForRun, misattributing the write to the operator.
func runScoped(tk tracker.Tracker, repo store.Repo, runID string) tracker.Tracker {
	if repo.TrackerBinding != store.TrackerBindingBuiltin {
		return tk
	}
	if rs, ok := tk.(tracker.RunScoper); ok {
		return rs.ForRun(runID)
	}
	return tk
}

// --- triage surface (ADR-0014) ------------------------------------------------

type issueCreateRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// handleIssueCreate is POST /agent/v1/issues {title, body, labels} → 201 with
// the created issue (list shape — no comments yet by construction). Labels
// attach at creation under strict name resolution (unknown → 400, nothing
// created). On a builtin-bound repo the issue is authored as the run
// (author_kind=run via the ForRun seam); the body passes the incogni
// sanitizer like every agent-authored body.
func (s *Server) handleIssueCreate(w http.ResponseWriter, r *http.Request) {
	info, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	var req issueCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		jsonError(w, http.StatusBadRequest, "title is required")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	tk = runScoped(tk, repo, info.RunID)
	is, err := tk.CreateIssue(r.Context(), req.Title, s.sanitizeBody(repo, req.Body), req.Labels)
	if err != nil {
		s.writeTrackerError(w, "creating issue", repo, err)
		return
	}
	s.publishIssueChanged(repo)
	writeJSON(w, http.StatusCreated, issueListJSON(is))
}

// patchString reads a required-string PATCH field (null rejected), mirroring
// the operator API's helper (internal/httpapi/repos.go) so the two issue-edit
// surfaces reject a non-string value with byte-identical wording.
func patchString(raw json.RawMessage, field string) (string, error) {
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return "", fmt.Errorf("field %s must be a string", field)
	}
	return *v, nil
}

// handleIssueEdit is PATCH /agent/v1/issues/{n}: apply a title/body patch to
// issue n and answer 200 with the updated issue (list shape — an edit is a
// write, not a thread read). A patch KEY's presence means "replace this field",
// its absence "leave it untouched", so the body decodes into
// map[string]json.RawMessage to tell absent from empty — the operator PATCH's
// contract (internal/httpapi/issues.go). "title" is a string that must not be
// whitespace-only (the seam does NOT guard this — the API does, mirroring the
// operator), "body" any string (""=clear), a non-string value a 400 in
// patchString's wording, any other key a 400 naming it. The body passes the
// incogni sanitizer (ADR-0014: no unsanitized agent write path); the title does
// not (mirrors create). An empty patch {} is a legal existence-verifying no-op
// (store.UpdateIssue documents it; a forge PATCH {} is a harmless no-op) — the
// ≥1-flag rule is labctl's client-side check. No runScope: an edit re-writes
// existing content and carries no author identity (see tracker.IssueEdit).
func (s *Server) handleIssueEdit(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	var patch map[string]json.RawMessage
	if !decodeJSON(w, r, &patch) {
		return
	}
	var edit tracker.IssueEdit
	for key, raw := range patch {
		switch key {
		case "title":
			title, err := patchString(raw, key)
			if err != nil {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			if strings.TrimSpace(title) == "" {
				jsonError(w, http.StatusBadRequest, "title must not be empty")
				return
			}
			edit.Title = &title
		case "body":
			body, err := patchString(raw, key)
			if err != nil {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			body = s.sanitizeBody(repo, body)
			edit.Body = &body
		default:
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("unknown field %q", key))
			return
		}
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	is, err := tk.EditIssue(r.Context(), n, edit)
	if err != nil {
		s.writeTrackerError(w, "editing issue", repo, err)
		return
	}
	s.publishIssueChanged(repo)
	writeJSON(w, http.StatusOK, issueListJSON(is))
}

type issueLabelsRequest struct {
	Labels []string `json:"labels"`
}

// handleIssueLabelAdd is POST /agent/v1/issues/{n}/labels {labels: [...]}:
// attach the named labels to issue n. Strict resolution before anything is
// applied — an unknown name is a 400 and no label is attached.
func (s *Server) handleIssueLabelAdd(w http.ResponseWriter, r *http.Request) {
	s.handleIssueLabels(w, r, "adding issue labels", tracker.Tracker.AddIssueLabels)
}

// handleIssueLabelRemove is DELETE /agent/v1/issues/{n}/labels {labels: […]}:
// detach the named labels from issue n. The label set rides in the JSON body
// (mirroring the add) rather than a path segment, so label names containing
// path metacharacters ("kind/bug") never fight URL escaping.
func (s *Server) handleIssueLabelRemove(w http.ResponseWriter, r *http.Request) {
	s.handleIssueLabels(w, r, "removing issue labels", tracker.Tracker.RemoveIssueLabels)
}

// handleIssueLabels is the shared add/remove body: both ops take {labels},
// answer 200 {ok:true}, and differ only in the seam method.
func (s *Server) handleIssueLabels(w http.ResponseWriter, r *http.Request, doing string,
	op func(tracker.Tracker, context.Context, int, []string) error,
) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	var req issueLabelsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Labels) == 0 {
		jsonError(w, http.StatusBadRequest, "labels is required")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	if err := op(tk, r.Context(), n, req.Labels); err != nil {
		s.writeTrackerError(w, doing, repo, err)
		return
	}
	s.publishIssueChanged(repo)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleIssueClose is POST /agent/v1/issues/{n}/close: transition issue n to
// closed. No closing-comment sugar — skills post the explanation first, then
// close (two calls, the existing convention).
func (s *Server) handleIssueClose(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	if err := tk.CloseIssue(r.Context(), n); err != nil {
		s.writeTrackerError(w, "closing issue", repo, err)
		return
	}
	s.publishIssueChanged(repo)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type labelResponse struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func labelJSON(l tracker.Label) labelResponse {
	return labelResponse{Name: l.Name, Color: l.Color, Description: l.Description}
}

// handleLabelList is GET /agent/v1/labels: the repo's labels ordered by name.
func (s *Server) handleLabelList(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	labels, err := tk.Labels(r.Context())
	if err != nil {
		s.writeTrackerError(w, "listing labels", repo, err)
		return
	}
	items := make([]labelResponse, 0, len(labels))
	for _, l := range labels {
		items = append(items, labelJSON(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"labels": items})
}

type labelCreateRequest struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// handleLabelEnsure is POST /agent/v1/labels {name, color?, description?}:
// idempotent label create — a 200 with the label that exists afterwards,
// whether this call created it or an earlier one did, so skills ensure the
// triage set unconditionally and retries after a timed-out call are safe.
// An existing label is returned untouched (color/description not updated).
func (s *Server) handleLabelEnsure(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	var req labelCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Trimmed like the operator's label create: label matching is exact, so
	// "bug " must not coexist with a distinct "bug".
	name := strings.TrimSpace(req.Name)
	if name == "" {
		jsonError(w, http.StatusBadRequest, "name is required")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	l, err := tk.EnsureLabel(r.Context(), name, req.Color, req.Description)
	if err != nil {
		s.writeTrackerError(w, "ensuring label", repo, err)
		return
	}
	s.publishIssueChanged(repo)
	writeJSON(w, http.StatusOK, labelJSON(l))
}

type prCreateRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// handlePRCreate is POST /agent/v1/prs {title, body} → 201 {number, url}.
// Head is always the RUN's branch, base the repo's default branch — the
// caller names neither. For AFK runs the server validates the pinned
// `Closes #<N>` (injecting "\n\nCloses #<N>" when missing; N = the run's
// claimed issue); sanitizeBody then strips AI attribution on incogni repos.
// On a builtin-bound repo the tracker is the store-backed builtin tracker,
// so CreatePull opens a change request (M6): the injected/validated Closes
// directives flow through tracker.ParseCloses into cr_closes rows — the
// issues the operator's later merge auto-closes — and cr.changed is
// published so the operator's CR list refetches.
func (s *Server) handlePRCreate(w http.ResponseWriter, r *http.Request) {
	info, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	var req prCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		jsonError(w, http.StatusBadRequest, "title is required")
		return
	}

	body := req.Body
	if (info.Kind == store.RunKindAFKManual || info.Kind == store.RunKindAFKAuto) && info.IssueNumber != nil {
		body = ensureCloses(body, *info.IssueNumber)
	}
	body = s.sanitizeBody(repo, body)

	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	pr, err := tk.CreatePull(r.Context(), info.Branch, repo.DefaultBranch, req.Title, body)
	if err != nil {
		s.writeTrackerError(w, "creating pull request", repo, err)
		return
	}
	if repo.TrackerBinding == store.TrackerBindingBuiltin {
		s.publishCRChanged(repo.ID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"number": pr.Number,
		"url":    pr.URL,
	})
}

// prDetailResponse is the GET /agent/v1/prs/{n} shape: PR metadata plus the
// full BODY — the read labctl pr view renders, so an agent can retrieve
// PR-carried content (e.g. a captured card YAML) without any raw forge
// fallback (D10). head is the bare branch name, state the three-valued
// open|merged|closed vocabulary. reviews carries the submitted reviews (the
// reject → re-queue loop's read); it ALWAYS marshals as an array — [] when the
// PR carries none, never null — so an agent parsing it never special-cases a
// nil, and a built-in binding (no forge review model) simply reports [].
// comments carries the PR's discussion thread — whatever tracker.PullComments
// returns, oldest first (discussion comments only; inline review comments are
// out of scope) — so `labctl pr view` can render the conversation and a fix
// agent or lander catching up on a PR reads it in one call instead of raw
// forge fallback (issue #191). Content is verbatim and unfiltered, including
// autoland verdict-marker comments (ADR-0048): a reject's body IS the
// findings a fix agent needs, so nothing here is stripped or annotated. Same
// always-[]-never-null contract as reviews, for the same reason (see
// handlePRGet: the built-in binding has no CR comment thread yet and reports
// [] there too).
type prDetailResponse struct {
	Number   int               `json:"number"`
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	State    string            `json:"state"`
	Head     string            `json:"head"`
	URL      string            `json:"url"`
	Reviews  []reviewResponse  `json:"reviews"`
	Comments []commentResponse `json:"comments"`
}

// reviewResponse is one submitted review in the GET /agent/v1/prs/{n} payload:
// who submitted it, its normalized state (the Review* vocabulary), its prose
// body, and whether the forge has since dismissed it — what the reject →
// re-queue loop reads to know a PR carries an outstanding changes-requested
// verdict and from which reviewer to re-request once a fix lands.
type reviewResponse struct {
	Reviewer  string `json:"reviewer"`
	State     string `json:"state"`
	Body      string `json:"body"`
	Dismissed bool   `json:"dismissed"`
}

// prListItem is one row of GET /agent/v1/prs — the PullRef vocabulary (no
// title/body; the list is the reader counterpart of the reaper's Pulls()
// fan-out, and PullRef deliberately stays that cheap).
type prListItem struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   string `json:"head"`
	URL    string `json:"url"`
}

// prChecksResponse is the GET /agent/v1/prs/{n}/checks shape (issue #72, the
// fix-the-red loop): the per-check rows plus the single-word aggregate. The
// aggregate is computed SERVER-SIDE via the pure tracker.ChecksState over the
// rows and shipped in the payload, so the client never recomputes it and never
// trusts a forge's own combined verdict — a forge reports "pending" for a head
// carrying zero registered statuses, which would spin a waiting agent forever
// (checks.go). State is the aggregate vocabulary failure|pending|success|none
// (none iff zero rows); each row's State is the three-word normalized
// vocabulary, with rawState carrying the forge's own word verbatim beside it.
type prChecksResponse struct {
	State  string        `json:"state"`
	Checks []prCheckItem `json:"checks"`
}

// prCheckItem is one row of GET /agent/v1/prs/{n}/checks — one CI check /
// commit-status for the PR/CR's current head commit. rawState (camelCase on
// the wire) is the forge's own state word verbatim, beside lab's normalized
// State, so an agent wanting more than pending/success/failure reads exactly
// what the forge said instead of trusting lab's collapse.
type prCheckItem struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	RawState string `json:"rawState"`
	Summary  string `json:"summary"`
	URL      string `json:"url"`
}

// handlePRGet is GET /agent/v1/prs/{n}: one PR/CR of the run's repo in full,
// body, submitted reviews, AND its discussion comment thread included. An
// unknown number is the canonical 404 envelope (either binding's typed
// not-found), never a panic. Reviews are read after the Pull, then comments
// are read after Reviews; if either read fails the whole response fails
// through writeTrackerError (no partial detail), so the caller never sees a
// PR whose reviews or thread silently dropped — EXCEPT PullComments wrapping
// ErrUnsupported, which is not a failure: it is the built-in binding's "no CR
// comment thread yet" (ADR-0048, builtin.go), and renders as the same []
// empty array a commentless forge PR yields rather than 409ing every builtin
// `pr view`. On a built-in binding Reviews is likewise a harmless empty read,
// so that array is [] there too — the same shape a reviewless forge PR
// yields.
func (s *Server) handlePRGet(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	pd, err := tk.Pull(r.Context(), n)
	if err != nil {
		s.writeTrackerError(w, "loading pull request", repo, err)
		return
	}
	rev, err := tk.Reviews(r.Context(), n)
	if err != nil {
		s.writeTrackerError(w, "loading pull request reviews", repo, err)
		return
	}
	// Non-nil so the array marshals as [] on zero reviews, never null.
	reviews := make([]reviewResponse, 0, len(rev))
	for _, v := range rev {
		reviews = append(reviews, reviewResponse{
			Reviewer:  v.Reviewer,
			State:     v.State,
			Body:      v.Body,
			Dismissed: v.Dismissed,
		})
	}
	cs, err := tk.PullComments(r.Context(), n)
	if err != nil && !errors.Is(err, tracker.ErrUnsupported) {
		s.writeTrackerError(w, "loading pull request comments", repo, err)
		return
	}
	// Non-nil so the array marshals as [] on zero comments, never null — same
	// contract as reviews, and how ErrUnsupported (the built-in binding's "no
	// CR comment thread yet") lands here too.
	comments := make([]commentResponse, 0, len(cs))
	for _, c := range cs {
		comments = append(comments, commentResponse{
			Author:    c.Author,
			Body:      c.Body,
			CreatedAt: store.FormatTime(c.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, prDetailResponse{
		Number:   pd.Number,
		Title:    pd.Title,
		Body:     pd.Body,
		State:    pd.State,
		Head:     pd.HeadBranch,
		URL:      pd.URL,
		Reviews:  reviews,
		Comments: comments,
	})
}

// handlePRChecks is GET /agent/v1/prs/{n}/checks: the CI status of PR/CR n's
// current head commit — the reader of the fix-the-red loop (issue #72). It is
// NOT a merge gate: pr merge stays attempt-and-surface (ADR-0024), so this is
// purely advisory. The aggregate is computed SERVER-SIDE via the pure
// tracker.ChecksState and shipped in the payload, so the client never
// recomputes it or trusts a forge's own combined verdict (checks.go). The
// rows always marshal as [] — never null — when empty, so an agent parsing the
// array never special-cases a nil. An unknown number is the canonical 404
// envelope (either binding's typed not-found); a throttled forge is a 503 (the
// ErrRateLimited branch in writeTrackerError).
func (s *Server) handlePRChecks(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	rows, err := tk.Checks(r.Context(), n)
	if err != nil {
		s.writeTrackerError(w, "loading pull request checks", repo, err)
		return
	}
	// Non-nil so the array marshals as [] on zero rows, never null.
	checks := make([]prCheckItem, 0, len(rows))
	for _, c := range rows {
		checks = append(checks, prCheckItem{
			Name:     c.Name,
			State:    c.State,
			RawState: c.RawState,
			Summary:  c.Summary,
			URL:      c.URL,
		})
	}
	writeJSON(w, http.StatusOK, prChecksResponse{
		State:  tracker.ChecksState(rows),
		Checks: checks,
	})
}

// unknownCheckResponse is the 404 body GET /agent/v1/prs/{n}/logs answers when
// ?check=<ctx> names no Checks row on the PR's head: the error names the
// unknown context and available lists every context the head DOES carry, in row
// order, so a reading agent can correct a typo in one round trip instead of
// re-fetching /checks. Available is non-nil so it marshals as [] (never null)
// when the head has zero checks — the array-never-nil contract the checks
// surface already holds.
type unknownCheckResponse struct {
	Error     string   `json:"error"`
	Available []string `json:"available"`
}

// handlePRLogs is GET /agent/v1/prs/{n}/logs[?check=<context>]: the redacted raw
// CI logs of PR/CR n's current head — the diagnose leg of the fix-the-red loop
// (ADR-0060, closing ADR-0032's deferral of the log text behind the red rows).
// Default mode serves the log of every FAILING check, each under a
// `=== logs: <name> ===` header line; ?check= serves that one named check's log
// bare (green or pending included). Zero failing checks in default mode is a
// truthful 204, not an error (ADR-0032's truthful-empty stance); an unknown
// ?check= context is a 404 carrying the available contexts.
//
// WHICH checks to fetch is policy, decided HERE, not in the backend: the tracker
// exposes CheckLog as a per-check primitive, and this handler holds the Checks()
// rows and their normalized states, so the failing-subset default lives once,
// server-side — the ADR-0032 pattern (backends fetch, lab decides), mirroring
// handlePRChecks' server-side aggregate.
//
// Every target's log is fetched and BUFFERED before the first body byte is
// written: a mid-fetch failure must become a real status code through
// writeTrackerError (404 unknown check, 409 unsupported, 502 adapter mismatch,
// 503 rate-limited), never a torn 200 whose truncated body a reading agent would
// mistake for the whole log. The redactor is likewise built before any write.
//
// Redaction is FAIL-CLOSED (ADR-0060): the response passes through the
// internal/secrets derived-forms redactor so every lab-vault secret value, in
// every encoding, becomes [REDACTED:NAME] before a byte leaves the server. A
// redactor that cannot be built (store or vault error) is a 500 — NEVER a
// raw-log fallback, because a log endpoint that leaks a vault value even once
// poisons the well. A nil redactor is the seam's zero-secret fast path (the repo
// has no secrets), so those logs pass through unredacted.
func (s *Server) handlePRLogs(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	rows, err := tk.Checks(r.Context(), n)
	if err != nil {
		s.writeTrackerError(w, "loading pull request checks", repo, err)
		return
	}

	// Selection: ?check= names one row (any state); absent targets the failing
	// subset, and zero failing checks is a truthful 204 (ADR-0032).
	check := r.URL.Query().Get("check")
	var targets []tracker.Check
	if check != "" {
		for _, row := range rows {
			if row.Name == check {
				targets = append(targets, row)
				break
			}
		}
		if len(targets) == 0 {
			available := make([]string, 0, len(rows))
			for _, row := range rows {
				available = append(available, row.Name)
			}
			writeJSON(w, http.StatusNotFound, unknownCheckResponse{
				Error:     fmt.Sprintf("unknown check context %q", check),
				Available: available,
			})
			return
		}
	} else {
		for _, row := range rows {
			if row.State == tracker.CheckFailure {
				targets = append(targets, row)
			}
		}
		if len(targets) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// Fetch-and-buffer BEFORE writing any body byte: a fetch failure here is a
	// real status code (writeTrackerError), never a truncated 200.
	logs := make([][]byte, len(targets))
	for i, target := range targets {
		b, err := tk.CheckLog(r.Context(), n, target.Name)
		if err != nil {
			s.writeTrackerError(w, "loading pull request check logs", repo, err)
			return
		}
		logs[i] = b
	}

	// Fail-closed redaction, built before any write: a redactor that cannot be
	// built is a 500, never a raw-log fallback (ADR-0060). A nil redactor is the
	// seam's zero-secret fast path — the repo has no secrets, so pass through.
	red, err := (&secrets.Source{Values: s.store.AllRepoSecretValues, Decrypt: s.vault.Decrypt}).Redactor(r.Context(), repo.ID)
	if err != nil {
		s.internalError(w, "building the log redactor", err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for i, target := range targets {
		if check == "" {
			// Default mode delimits each section so multiple logs never run
			// together; ?check= mode dumps the one raw log bare.
			_, _ = fmt.Fprintf(w, "=== logs: %s ===\n", target.Name)
		}
		text := string(logs[i])
		if red != nil {
			text, _ = red.Redact(text) // ignore the hit names; the masked text is all that ships
		}
		_, _ = io.WriteString(w, text)
		if !strings.HasSuffix(text, "\n") {
			// One added newline so a log with no trailing newline never runs into
			// the next section's header (or the end of the bare log).
			_, _ = io.WriteString(w, "\n")
		}
	}
}

// handlePRList is GET /agent/v1/prs: the repo's PRs/CRs as the tracker's
// bounded list view — every OPEN one plus the tracker.RecentClosedWindow
// most recently closed (issue #176), open first — what `labctl pr list`
// renders (the reader counterpart to POST /prs). The handler maps 1:1 over
// Pulls(); the bound lives in the backends, so a merged run PR still shows
// while recent and `labctl pr view <n>` reads anything older by number.
func (s *Server) handlePRList(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	pulls, err := tk.Pulls(r.Context())
	if err != nil {
		s.writeTrackerError(w, "listing pull requests", repo, err)
		return
	}
	items := make([]prListItem, 0, len(pulls))
	for _, p := range pulls {
		items = append(items, prListItem{Number: p.Number, State: p.State, Head: p.HeadBranch, URL: p.URL})
	}
	writeJSON(w, http.StatusOK, map[string]any{"prs": items})
}

// prMergeResponse is the POST /agent/v1/prs/{n}/merge answer: the merged PR's
// number, three-valued state (merged on success), head branch, and web/lab
// URL — what `labctl pr merge` prints.
type prMergeResponse struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   string `json:"head"`
	URL    string `json:"url"`
}

// handlePRMerge is POST /agent/v1/prs/{n}/merge: land PR/CR n of the run's
// repo and report the result. The merge method is fixed — no flag. Lab does
// not reason about mergeability: the backend (the forge, or the built-in git
// push under a pre-receive hook) enforces required checks and branch
// protection, and a refusal surfaces verbatim as a 409 with a non-zero
// labctl exit. Convergent: merging an already-merged PR is a no-op success.
// On a forge-bound repo the merge runs under the SERVER's forge token — the
// run-token repo scope stays the only agent-side boundary, and no forge
// credential ever reaches the session (ADR-0014). The head branch is not
// deleted (teardown/sweep GCs a merged head, not merge).
func (s *Server) handlePRMerge(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	pr, err := tk.MergePull(r.Context(), n)
	if err != nil {
		s.writeTrackerError(w, "merging pull request", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, prMergeResponse{
		Number: pr.Number,
		State:  pr.State,
		Head:   pr.HeadBranch,
		URL:    pr.URL,
	})
}

// prReviewRequest is the {body} payload of the verdict/comment verbs (reject,
// approve, comment). Reject and comment require a non-empty body; approve's is
// optional (a pass verdict needs no words).
type prReviewRequest struct {
	Body string `json:"body"`
}

// Verdict markers — the first line of the PR comment each verdict verb
// composes. The grammar itself (ADR-0048: `[autoland] verdict: <word>`,
// parsed first-line-only with an exact prefix match) lives in
// internal/tracker (verdict.go), so the #181 poller reading these same
// comments shares one definition instead of a second copy here; this package
// only injects it. Injection is SERVER-SIDE, in these handlers (precedent:
// handlePRCreate's Closes-#N injection): agents and skills speak verbs only
// and never see the grammar, so a verdict verb's own body can never reach
// line 1 — it lands below the composed marker and is inert.
//
// That covers the verdict verbs, but NOT the plain comment verb, which
// prepends nothing: without a guard, `pr comment` could post a body whose
// first line IS a marker and forge a verdict no lander ever reached (the
// marker is a trust anchor, and a run token is repo-scoped, so the forgery
// would not even be limited to the run's own PR). handlePRComment therefore
// rejects a body that opens with tracker.VerdictMarkerPrefix — the grammar
// stays writable only through the verbs that own it.

// opensWithVerdictMarker reports whether body's FIRST line is a verdict
// marker. Leading whitespace is trimmed before the match even though
// tracker.ParseVerdict's pinned parse rule is an exact prefix (a marker
// indented by a space is inert against that rule): the guard is deliberately
// stricter than the parser, so a future consumer that trims before matching
// cannot turn an accepted body into a forged verdict retroactively.
func opensWithVerdictMarker(body string) bool {
	first, _, _ := strings.Cut(body, "\n")
	_, ok := tracker.ParseVerdict(strings.TrimSpace(first))
	return ok
}

// verdictComment composes the comment a verdict verb posts: the marker line,
// then the (already sanitized) body below a blank line; an empty body is the
// bare marker.
func verdictComment(marker, body string) string {
	if body == "" {
		return marker
	}
	return marker + "\n\n" + body
}

// handlePRReject is POST /agent/v1/prs/{n}/reject {body}: record a rejection
// verdict on PR/CR n by posting a PR comment whose first line is the reject
// marker with body as the findings below (body REQUIRED — blank/missing is a
// 400, mirroring the comment create's wording; the findings are the point),
// returning {number}. An unknown number is a 404; the built-in binding (no PR
// comment write yet) is a 409 (ErrUnsupported). On a forge binding the write
// runs under the SERVER's forge token — no forge credential ever reaches the
// session (ADR-0014). The body passes the incogni sanitizer like every
// agent-authored body (ADR-0014: no unsanitized write path) before the marker
// is prepended; the secretscan decorator scans the composed comment at the
// seam like any other PR comment.
func (s *Server) handlePRReject(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	var req prReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Emptiness is judged on the SANITIZED body, not the raw one: on an
	// incogni repo stripAttribution can reduce an all-attribution body to
	// nothing, and a rejection that posts a bare marker with no findings
	// defeats the very gate this check is here to be — the findings are the
	// point of a reject.
	body := s.sanitizeBody(repo, req.Body)
	if strings.TrimSpace(body) == "" {
		jsonError(w, http.StatusBadRequest, "body is required")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	comment := verdictComment(tracker.VerdictReject, body)
	if err := tk.CommentPull(r.Context(), n, comment); err != nil {
		s.writeTrackerError(w, "rejecting pull request", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"number": n})
}

// handlePREscalate is POST /agent/v1/prs/{n}/escalate {body}: record the
// escalate-mode lander's TERMINAL marker on PR/CR n by posting a PR comment
// whose first line is the escalate marker with body as the history digest
// below (body REQUIRED — blank/missing is a 400, mirroring reject; the digest
// is the point). Unlike rerequest, there is no native ping: escalation hands
// the PR to a human via the ready-for-human label (the escalate seed's own
// labctl issue label steps), not a forge review request, so this handler is
// reject's twin, not rerequest's. ADR-0048 reserved the `escalate` word for
// exactly this marker (issue #182); the poller's Escalated fold (decide.go)
// treats its presence as permanent — rule 1 blocks every candidate kind on
// this branch forever after. The autoland ENGINE never writes to the forge
// (it only spawns runs), so this agent-executed verb is the escalation
// outcome's only PR write. Error mapping, sanitize, and credentialing all
// match reject: unknown → 404, built-in binding → 409 (ErrUnsupported),
// server-credentialed write (ADR-0014), sanitized body before the marker is
// prepended.
func (s *Server) handlePREscalate(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	var req prReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Emptiness is judged on the SANITIZED body, exactly like reject: an
	// escalation that posts a bare marker with no digest defeats the point of
	// escalating — the digest is what a human picks up the PR with.
	body := s.sanitizeBody(repo, req.Body)
	if strings.TrimSpace(body) == "" {
		jsonError(w, http.StatusBadRequest, "body is required")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	comment := verdictComment(tracker.VerdictEscalate, body)
	if err := tk.CommentPull(r.Context(), n, comment); err != nil {
		s.writeTrackerError(w, "escalating pull request", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"number": n})
}

// handlePRApprove is POST /agent/v1/prs/{n}/approve {body?}: record a
// validation-passed verdict ("validated, awaiting confirm") on PR/CR n by
// posting a PR comment whose first line is the pass marker, returning
// {number}. The body is OPTIONAL — CONCERNS prose goes below the marker; an
// empty body, or an absent request body entirely, both post the bare marker.
// Error mapping is reject's: unknown → 404, built-in binding → 409
// (ErrUnsupported). Server-credentialed and sanitized like reject.
func (s *Server) handlePRApprove(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	// Body optional: an absent body (EOF) or an empty {} both decode to the zero
	// value — no error, unlike reject/comment. Only a genuinely malformed body
	// 400s. Decoded inline (not via decodeJSON) precisely because EOF is legal
	// here.
	var req prReviewRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	comment := verdictComment(tracker.VerdictPass, s.sanitizeBody(repo, req.Body))
	if err := tk.CommentPull(r.Context(), n, comment); err != nil {
		s.writeTrackerError(w, "approving pull request", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"number": n})
}

// handlePRRerequest is POST /agent/v1/prs/{n}/rerequest (no body): the fix
// run's done-signal. It posts the fix-done marker as a PR comment FIRST — the
// comment IS the signal — and THEN best-effort native re-requests every
// reviewer whose latest verdict-bearing review is changes-requested (the human
// forge ping). Answers {number}; a failed ping is NON-FATAL — the response
// carries it as {number, warning} (200, labctl exits 0 and prints the warning)
// and it is logged server-side, because the done-signal already landed and the
// human ping is best-effort by design (ADR-0048). A comment failure IS fatal:
// unknown number → 404, built-in binding → 409 (ErrUnsupported), and the ping
// is never attempted.
func (s *Server) handlePRRerequest(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	if err := tk.CommentPull(r.Context(), n, tracker.VerdictFixDone); err != nil {
		s.writeTrackerError(w, "re-requesting review", repo, err)
		return
	}
	resp := map[string]any{"number": n}
	if err := tk.RerequestReview(r.Context(), n); err != nil {
		// Only a forge REFUSAL degrades to a warning: the done-signal comment
		// already landed, and a forge declining the ping is not a failed
		// re-request. Typed upstream conditions (a PR deleted between the two
		// calls, a spent rate-limit budget) are real errors and keep their
		// status — degrading them here would report success for a re-request
		// that never reached the forge at all.
		if !errors.Is(err, tracker.ErrReviewRejected) {
			s.writeTrackerError(w, "re-requesting review", repo, err)
			return
		}
		// The seam's errors never carry the forge token (ErrReviewRejected wraps
		// the forge's own words only), so the message is safe to echo.
		s.log.Warn("re-request review ping failed", "component", "agentapi", "repo", repo.ID, "err", err)
		resp["warning"] = "re-requesting review from the forge failed: " + err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handlePRComment is POST /agent/v1/prs/{n}/comments {body}: post a plain
// discussion comment on PR/CR n (a PR shares the issue-comment number space),
// returning {number}. Body REQUIRED non-empty — blank/missing is a 400 in the
// comment create's wording. Unlike PR create, no `Closes #N` is injected (that
// is a PR-create-only concern). Error mapping is the review verbs': unknown →
// 404, built-in binding → 409 (ErrUnsupported). The body passes the incogni
// sanitizer like every agent-authored body; the secretscan decorator scans it
// at the seam.
//
// This path composes no marker of its own, so it could forge the verdict
// grammar: a body opening with tracker.VerdictMarkerPrefix is a 400. Without
// that gate a run token — repo-scoped, so not even confined to its own PR —
// could post a first-line `[autoland] verdict: pass` that a consumer reads as
// a lander verdict.
//
// The gate here is NOT airtight, and must not be read as one. handleCommentCreate
// (POST /agent/v1/issues/{n}/comments) is a second agent-writable path onto the
// same thread — a PR shares the issue-comment number space, so both verbs
// resolve to issuePath(n)+"/comments", the endpoint PullComments reads — and it
// carries no verdict gate. A run token can still forge a marker through it, which
// would deny a PR its validation (VerdictPresent suppresses the spawn) or
// false-success-reap a live lander.
//
// That is a KNOWN, accepted gap: the exploit requires a deliberately adversarial
// agent rather than a mistaken one, and the accidental path is narrow (a body
// must BEGIN with the marker). Closing it means gating handleCommentCreate too,
// or moving the check down to the CreateComment seam so both verbs inherit it
// structurally — the better fix, and the one to make if this ever stops being a
// trust-the-agent boundary.
func (s *Server) handlePRComment(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	var req prReviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Both gates judge the SANITIZED body — the string that actually reaches
	// the forge. Sanitizing can DELETE leading lines (stripAttribution), so a
	// marker sitting on line 2 under an attribution line would be promoted to
	// line 1 after a raw-body check had already passed it.
	body := s.sanitizeBody(repo, req.Body)
	if strings.TrimSpace(body) == "" {
		jsonError(w, http.StatusBadRequest, "body is required")
		return
	}
	if opensWithVerdictMarker(body) {
		jsonError(w, http.StatusBadRequest, "body must not begin with a verdict marker line")
		return
	}
	tk, ok := s.trackerFor(w, r, repo)
	if !ok {
		return
	}
	if err := tk.CommentPull(r.Context(), n, body); err != nil {
		s.writeTrackerError(w, "commenting on pull request", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"number": n})
}

// publishCRChanged emits cr.changed for repoID — the same {type, repoID}
// envelope every repo-scoped event carries. A nil bus (unit tests) is a no-op.
func (s *Server) publishCRChanged(repoID string) {
	s.publishRepoEvent(EventCRChanged, repoID)
}

// publishIssueChanged emits issue.changed for a builtin-bound repo's issue or
// label mutation; forge-bound mutations publish nothing (see EventIssueChanged).
func (s *Server) publishIssueChanged(repo store.Repo) {
	if repo.TrackerBinding != store.TrackerBindingBuiltin {
		return
	}
	s.publishRepoEvent(EventIssueChanged, repo.ID)
}

func (s *Server) publishRepoEvent(eventType, repoID string) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(events.Event{Type: eventType, Payload: struct {
		Type   string `json:"type"`
		RepoID string `json:"repoID"`
	}{Type: eventType, RepoID: repoID}})
}

// ensureCloses returns body guaranteed to carry a real `Closes #<n>` directive.
// The check is the shared closing-keyword grammar (tracker.ContainsCloses):
// the full GitHub/Forgejo keyword set, case-insensitive and word/number-
// bounded ("Closes #70", "Closes #7abc" and "discloses #7" do not satisfy
// issue 7); when a real directive for THIS issue is absent, the pinned
// "\n\nCloses #<n>" is appended (bare "Closes #<n>" on an empty body).
func ensureCloses(body string, n int) string {
	if tracker.ContainsCloses(body, n) {
		return body
	}
	closes := "Closes #" + strconv.Itoa(n)
	if body == "" {
		return closes
	}
	return body + "\n\n" + closes
}

// sanitizeBody is the incogni sanitization seam (design §5: "applies
// incogni sanitization server-side before forwarding to tracker"; D15 §9
// measure 3, defense in depth behind the seeded attribution-off settings):
// for incogni repos, every line matching a provider-declared attribution
// pattern (the compiled cross-provider union carried on the Server, issue
// #75 / ADR-0033) is stripped from EVERY agent-authored body — PR/CR, issue,
// and comment create alike (ADR-0014: no unsanitized write path) — before it
// reaches the tracker. Non-incogni bodies pass through byte-identical. On the
// PR path it runs AFTER ensureCloses, so an injected `Closes #N` is never
// touched.
func (s *Server) sanitizeBody(repo store.Repo, body string) string {
	if !repo.Incogni {
		return body
	}
	return stripAttribution(body, s.scrub)
}

// stripAttribution removes every line matching any provider-declared
// attribution pattern in scrub — the compiled `(?i)` cross-provider union
// that replaced core's hardcoded claude shapes (issue #75 / ADR-0033, D15 §9
// measure 3). It repairs ONLY the seam each removal leaves — the blank line
// immediately before a removed line is swallowed so a "text\n\nfooter" seam
// does not leave a doubled blank, and dangling trailing blanks (footers live
// at the end) are trimmed. Blank runs elsewhere — a deliberate gap inside a
// fenced code block, paragraph spacing far from the footer — are left
// byte-identical; the old whole-body collapse corrupted them. A body with no
// matching lines is returned unchanged, and an empty scrub (no registered
// providers → no known markers) matches nothing and strips nothing, the same
// content-inert degradation as the pre-push hook on an empty registry.
func stripAttribution(body string, scrub []*regexp.Regexp) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	stripped := false
	for _, line := range lines {
		if matchesAttribution(line, scrub) {
			stripped = true
			// Collapse the seam: drop a blank immediately preceding the
			// removed line so the removal doesn't widen a paragraph gap.
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, line)
	}
	if !stripped {
		return body
	}
	// Edge trims are harmless (a body's leading/trailing blank lines carry no
	// meaning) and clean up blanks a leading/trailing footer removal left. The
	// interior is only touched at seams (above), so blank runs inside a fenced
	// code block survive byte-identical.
	for len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// matchesAttribution reports whether line matches any provider-declared scrub
// pattern — the per-line predicate of the incogni sanitizer (issue #75 /
// ADR-0033, D15 §9 measure 3). The regexps are already case-insensitive
// (`(?i)`) and unanchored, so the raw line is matched as-is — leading
// indentation needs no special casing.
func matchesAttribution(line string, scrub []*regexp.Regexp) bool {
	for _, re := range scrub {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}
