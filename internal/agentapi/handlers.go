package agentapi

// The /agent/v1 handlers (brief §8.2, pinned M5 contract). Repo scope comes
// STRICTLY from the authenticated run's row (RunTokenInfo.RepoID) — no URL
// carries a repo id here, so a run can only reach its own repo. Response
// shapes mirror the operator API's issue JSON byte-for-byte (labctl and the
// SPA speak one vocabulary); errors are the canonical {"error": …} envelope.
//
// Error mapping (pinned): upstream/store miss → 404; tracker configuration
// conflict (unsupported forge kind, missing/wrong-kind credential, bad
// remote/host, unknown binding) → 409; any other forge upstream failure →
// 502; builtin store failures → opaque 500.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
)

// EventCRChanged is the SSE event name a builtin PR create publishes: the
// created change request appears in the operator's CR list on the next
// refetch (brief §8.1). The name mirrors httpapi's constant — kept local,
// like jsonError, so agentapi stays free of an httpapi dependency.
const EventCRChanged = "cr.changed"

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

// writeTrackerError maps a tracker call failure: a miss is a 404 on either
// binding (builtin wraps store.ErrNotFound, the forgejo client wraps
// tracker.ErrNotFound around an upstream 404); any other forge-bound failure
// is an upstream error → 502 with the diagnostic (the forge client's errors
// never carry the token); builtin store failures stay an opaque 500.
func (s *Server) writeTrackerError(w http.ResponseWriter, doing string, repo store.Repo, err error) {
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
// token identity.
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
	if repo.TrackerBinding == store.TrackerBindingBuiltin {
		if bt, isBuiltin := tk.(*builtin.Tracker); isBuiltin {
			tk = bt.ForRun(info.RunID)
		}
	}
	if err := tk.CreateComment(r.Context(), n, req.Body); err != nil {
		s.writeTrackerError(w, "creating comment", repo, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
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
	body = sanitizeBody(repo, body)

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

// publishCRChanged emits cr.changed for repoID — the same {type, repoID}
// envelope every repo-scoped event carries. A nil bus (unit tests) is a no-op.
func (s *Server) publishCRChanged(repoID string) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(events.Event{Type: EventCRChanged, Payload: struct {
		Type   string `json:"type"`
		RepoID string `json:"repoID"`
	}{Type: EventCRChanged, RepoID: repoID}})
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
// for incogni repos, known attribution footers/trailers are stripped from
// the PR/CR body before it reaches the tracker. Non-incogni bodies pass
// through byte-identical. It runs AFTER ensureCloses, so an injected
// `Closes #N` is never touched.
func sanitizeBody(repo store.Repo, body string) string {
	if !repo.Incogni {
		return body
	}
	return stripAttribution(body)
}

// stripAttribution removes every line that is a known AI-attribution marker
// (attributionLine) and repairs ONLY the seam each removal leaves — the blank
// line immediately before a removed line is swallowed so a "text\n\nfooter"
// seam does not leave a doubled blank, and dangling trailing blanks (footers
// live at the end) are trimmed. Blank runs elsewhere — a deliberate gap
// inside a fenced code block, paragraph spacing far from the footer — are
// left byte-identical; the old whole-body collapse corrupted them. A body
// with no attribution lines is returned unchanged.
func stripAttribution(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	stripped := false
	for _, line := range lines {
		if attributionLine(line) {
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

// attributionLine reports whether a body line is a known AI-attribution
// marker (D15 §9 measure 3; key/footer shapes verified against Claude Code
// 2.1.198 — compat.md §4):
//
//   - a `Co-Authored-By: Claude…` trailer, any case;
//   - a generated-with footer mentioning claude — covers
//     `🤖 Generated with [Claude Code](…)` and the plain
//     `Generated with Claude Code` variants;
//   - a `Claude-Session:` trailer (the remote-control session link claude
//     appends when attribution.sessionUrl is on).
func attributionLine(line string) bool {
	l := strings.ToLower(strings.TrimSpace(line))
	if rest, ok := strings.CutPrefix(l, "co-authored-by:"); ok {
		// Match a "Claude" display name OR an @anthropic.com email — the
		// email is the stable discriminator across a model rename (compat.md
		// §4 pins the trailer as "Co-Authored-By: <model> <noreply@anthropic.com>").
		rest = strings.TrimSpace(rest)
		return strings.HasPrefix(rest, "claude") || strings.Contains(rest, "anthropic.com")
	}
	if strings.HasPrefix(l, "claude-session:") {
		return true
	}
	if i := strings.Index(l, "generated with"); i >= 0 {
		return strings.Contains(l[i:], "claude")
	}
	return false
}
