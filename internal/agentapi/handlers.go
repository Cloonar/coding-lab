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
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
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
// open|merged|closed vocabulary.
type prDetailResponse struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Head   string `json:"head"`
	URL    string `json:"url"`
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
// body included. An unknown number is the canonical 404 envelope (either
// binding's typed not-found), never a panic.
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
	writeJSON(w, http.StatusOK, prDetailResponse{
		Number: pd.Number,
		Title:  pd.Title,
		Body:   pd.Body,
		State:  pd.State,
		Head:   pd.HeadBranch,
		URL:    pd.URL,
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

// handlePRList is GET /agent/v1/prs: the repo's PRs/CRs across ALL states —
// what `labctl pr list` renders (the reader counterpart to POST /prs).
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
