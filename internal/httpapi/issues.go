package httpapi

// Issue surface (pinned M4 contract). Read views answer for BOTH tracker
// bindings through the tracker registry: a builtin-bound repo reads lab's own
// store, a forge-bound one proxies the Forgejo REST client. Mutations are no
// longer all builtin-only: a title/body PATCH rides the Tracker.EditIssue seam
// on EITHER binding (ADR-0046), so a forge-bound repo edits its issues on the
// forge. Every OTHER mutation — issue create, state change, label set, comment
// create — stays builtin-only and answers the pinned 409 on a forge-bound repo
// (a state change has the sibling pinned 400, forgeStateMessage, because it is
// understood but names a field this binding cannot patch). Every successful
// mutation publishes issue.changed on the bus, a forge-bound edit included
// (clients refetch on event, design §5; ADR-0076).

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

// EventIssueChanged is the SSE event name published on any issue/comment/label
// mutation lab makes (brief §8.1): every builtin mutation, and a forge-bound
// issue edit once the tracker call succeeds. Changes made directly on the
// forge publish nothing.
const EventIssueChanged = "issue.changed"

// forgeTrackerMessage is the pinned 409 body for builtin-only mutations
// attempted on a forge-bound repo.
const forgeTrackerMessage = "repository uses a forge tracker; manage issues on the forge"

// forgeStateMessage is the pinned 400 body for an issue STATE change attempted
// on a forge-bound repo. Unlike title/body, state has no EditIssue seam op — the
// Tracker exposes CloseIssue but no reopen — so a forge state change cannot ride
// the seam and is refused. It is a 400, not the 409 sibling: the request is
// understood, it just names a field this binding cannot patch. (The missing
// reopen op is a known, deliberately-unfiled follow-up; issue #168 pins this gap.)
const forgeStateMessage = "repository uses a forge tracker; issue state is managed on the forge"

// publishIssueChanged emits issue.changed for repoID. Same envelope shape as
// every repo-scoped event ({type, repoID}).
func (s *Server) publishIssueChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventIssueChanged, Payload: struct {
		Type   string `json:"type"`
		RepoID string `json:"repoID"`
	}{Type: EventIssueChanged, RepoID: repoID}})
}

// issueResponse is the pinned list-view issue JSON (comments_count, no
// comment bodies). Pull is the open pull request resolving the issue (issue
// #88) — a pointer so a row no open pull resolves serializes `"pull": null`
// rather than a zero object. Only handleIssueList fills it; every other view
// sharing this shape (the ready queue) spends no pulls read and answers null.
type issueResponse struct {
	Number        int                `json:"number"`
	Title         string             `json:"title"`
	Body          string             `json:"body"`
	State         string             `json:"state"`
	Labels        []string           `json:"labels"`
	CommentsCount int                `json:"comments_count"`
	CreatedAt     string             `json:"created_at"`
	UpdatedAt     string             `json:"updated_at"`
	Pull          *issuePullResponse `json:"pull"`
}

// issuePullResponse is an issue-list row's `pull` (issue #88): the OPEN pull
// request that resolves the issue — by claim-branch match or by a closing
// directive in its body (tracker.IssuePulls) — so the New run page can offer
// to land it from the row. URL is the forge web URL, or the CR's lab-relative
// SPA route on the built-in binding. Escalated says autoland has given up on
// the pull (ADR-0048) and not been re-armed since: autoland will not pick it
// up, so the page drops its "Autoland is on" collision note on Land.
type issuePullResponse struct {
	Number     int    `json:"number"`
	HeadBranch string `json:"head_branch"`
	URL        string `json:"url"`
	Escalated  bool   `json:"escalated"`
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
		if isTrackerConfigError(err) {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			s.internalError(w, "resolving tracker", err)
		}
		return nil, false
	}
	return tk, true
}

// isTrackerConfigError reports whether err is one of the typed tracker
// resolution sentinels — a repo/credential configuration conflict (unsupported
// forge kind, missing/wrong-kind forge credential, unparseable remote/host,
// unknown binding) that maps to a 409 with the diagnostic, never an opaque 500.
// Shared by the issues, ready, and AFK handlers so they refuse the same way.
func isTrackerConfigError(err error) bool {
	return errors.Is(err, tracker.ErrForgeUnsupported) ||
		errors.Is(err, tracker.ErrForgeCredentialMissing) ||
		errors.Is(err, tracker.ErrForgeCredentialKind) ||
		errors.Is(err, tracker.ErrForgeFlavorMismatch) ||
		errors.Is(err, tracker.ErrForgeHost) ||
		errors.Is(err, tracker.ErrRemotePath) ||
		errors.Is(err, tracker.ErrUnknownBinding)
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
	// A forge rate limit (GitHub) is a transient upstream throttle, not a lab
	// fault: answer 503 with the reset hint the client's message carries, so an
	// operator sees "rate limited" rather than a generic bad gateway. The AFK
	// engine treats it as a log-and-skip tick (ADR-0015).
	if errors.Is(err, tracker.ErrRateLimited) {
		s.log.Warn(doing, "component", "httpapi", "repo", repo.ID, "err", err)
		writeError(w, http.StatusServiceUnavailable, err.Error())
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
// open (the list view). On a forge binding the closed and all views carry
// the tracker's bounded recent window — the full open set plus the
// tracker.RecentClosedWindow most recently updated closed issues, not full
// closed history (issue #176); the builtin path reads the store directly
// and stays unbounded, a local read being cheap.
//
// Every row also carries `pull` (issue #88): the open pull request resolving
// that issue, or null. It costs ONE extra Tracker.Pulls read per list, on
// either binding, run concurrently with the issue read so the page's latency
// is the slower of the two rather than their sum, and joined onto the rows
// client-side (tracker.IssuePulls) — never a per-issue round-trip. The pulls
// half is an enrichment, not the list: a failing Pulls read (or a tracker
// that will not resolve for it) is logged and every row answers `pull: null`,
// while the issue read keeps exactly its own error mapping.
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
	var pulls <-chan []tracker.PullRef
	if repo.TrackerBinding == store.TrackerBindingBuiltin {
		pulls = s.startIssuePullsRead(r.Context(), repo, nil)
		issues, err := s.store.IssuesByRepo(r.Context(), repo.ID, state)
		if err != nil {
			<-pulls // let the pulls read land; see startIssuePullsRead
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
		pulls = s.startIssuePullsRead(r.Context(), repo, tk)
		issues, err := tk.Issues(r.Context(), state)
		if err != nil {
			<-pulls // let the pulls read land; see startIssuePullsRead
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
	s.attachIssuePulls(r.Context(), repo, items, <-pulls)
	writeJSON(w, http.StatusOK, map[string]any{
		"binding": repo.TrackerBinding,
		"issues":  items,
	})
}

// startIssuePullsRead starts the issue list's one Pulls read in the
// background and returns the channel its answer arrives on: the refs, or nil
// when the read failed — a failure the list absorbs (every row's pull null),
// so it is logged here and never reaches the caller as an error. tk is the
// tracker the forge path already resolved for its issue read (no second
// credential decrypt); nil resolves one, which is the builtin path — whose
// issue read goes to the store directly and so never needed a tracker.
//
// The handler drains the channel on EVERY path, its own issue-read failures
// included, though an error response has no use for the answer: the pulls
// read is a recorded list read (the readiness report judges every kind of
// tracker read, issue #61), and one abandoned mid-flight would be cancelled
// with the request, go unrecorded, and leave an older verdict standing — a
// forge 404 on the issue list judged beside a stale pulls success reads as
// "one listing failed" instead of "the forge does not show this repository".
// Both reads start together, so the wait costs the error path next to
// nothing. The buffer is only so a sender can never block on a path that
// does not drain. A read that still sees its context cancelled (the client
// went away) is no failure worth a log line. On the builtin binding both
// reads land on the store, and on sqlite's single connection they serialize
// rather than overlap — still correct, as neither holds rows open across a
// second query.
func (s *Server) startIssuePullsRead(ctx context.Context, repo store.Repo, tk tracker.Tracker) <-chan []tracker.PullRef {
	ch := make(chan []tracker.PullRef, 1)
	go func() {
		ch <- s.readIssuePulls(ctx, repo, tk)
	}()
	return ch
}

// readIssuePulls is startIssuePullsRead's body: resolve the tracker if the
// caller has none, read Pulls, and degrade any failure to nil with a Warn.
func (s *Server) readIssuePulls(ctx context.Context, repo store.Repo, tk tracker.Tracker) []tracker.PullRef {
	if tk == nil {
		var err error
		if tk, err = s.tracker.TrackerFor(ctx, repo); err != nil {
			if ctx.Err() == nil {
				s.log.Warn("issue list: resolving tracker for pulls", "component", "httpapi", "repo", repo.ID, "err", err)
			}
			return nil
		}
	}
	pulls, err := tk.Pulls(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("issue list: listing pulls", "component", "httpapi", "repo", repo.ID, "err", err)
		}
		return nil
	}
	return pulls
}

// attachIssuePulls fills each row's Pull from the issue→pull join over the
// one Pulls read (tracker.IssuePulls with the repo's afk_branch_pattern: a
// claim-branch head or a closing directive, open pulls only, newest wins). A
// nil read leaves every row null. Escalated is spent only on pulls actually
// attached to a row, once per pull however many issues it resolves.
func (s *Server) attachIssuePulls(ctx context.Context, repo store.Repo, items []issueResponse, pulls []tracker.PullRef) {
	byIssue := tracker.IssuePulls(pulls, repo.AFKBranchPattern)
	if len(byIssue) == 0 {
		return
	}
	escalated := make(map[int]bool)
	for i := range items {
		p, ok := byIssue[items[i].Number]
		if !ok {
			continue
		}
		esc, seen := escalated[p.Number]
		if !seen {
			esc = s.pullEscalated(ctx, repo, p.Number)
			escalated[p.Number] = esc
		}
		items[i].Pull = &issuePullResponse{
			Number:     p.Number,
			HeadBranch: p.HeadBranch,
			URL:        p.URL,
			Escalated:  esc,
		}
	}
}

// pullEscalated reports whether pull n's autoland escalation is still
// terminal by the STORE half of the poller's test (internal/afk/autoland.go):
// an escalated run row for the pull, newer than the pull's last re-arm —
// strictly after, so an escalation and a re-arm at the same stored instant
// read as re-armed, the human's gesture being the newer information. The
// marker half (an escalate word in the PR's comments) is deliberately not
// read: it would be a forge read per attached pull on every list render, and
// this field is a UI hint, not the gate — autoland's own pass stays the
// authority. The run row is the durable half anyway (a human can delete the
// marker comment, never the row). The rearm read is spent only once an
// escalated row exists, the rare case. A store error is logged and reads as
// not escalated: a hint never fails the list.
func (s *Server) pullEscalated(ctx context.Context, repo store.Repo, n int) bool {
	at, ok, err := s.store.EscalatedRunForPull(ctx, repo.ID, n)
	if err != nil {
		s.log.Warn("issue list: escalated run for pull", "component", "httpapi", "repo", repo.ID, "pull", n, "err", err)
		return false
	}
	if !ok {
		return false
	}
	rearmedAt, err := s.store.PullRearmedAt(ctx, repo.ID, n)
	if err != nil {
		s.log.Warn("issue list: pull rearm moment", "component", "httpapi", "repo", repo.ID, "pull", n, "err", err)
		return false
	}
	return at.After(rearmedAt)
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
// (Tracker.ReadyIssues, either binding) plus claimable_count — the "(N
// ready)" hint the SPA shows, counting ready issues WITHOUT an existing claim
// branch (parked/in-flight issues read as zero). With ?claimable=1 the
// {issues} list itself is filtered to the claimable set (same envelope — the
// SPA's hint is that list's length). The count/filter is best-effort by
// design: it is stale the moment it renders, and the authoritative claim
// check is remade inside the engine's locked claim path on the actual start.
// Without the AFK engine (or when the claim-branch read fails) both degrade
// to the raw ready queue.
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
	claimable := len(issues)
	if s.afk != nil {
		// Filter the ALREADY-FETCHED ready queue by the repo's claim branches:
		// the claim filter needs no round-trip, and the blocked-by gate
		// (ADR-0042) adds at most one lazy Issues(StateOpen) call — only when a
		// claimable issue carries `## Blocked by` refs — so a repo not using the
		// convention still derives count and list from the one ready snapshot.
		if cs, cerr := s.afk.FilterClaimable(r.Context(), repo, tk, issues); cerr != nil {
			s.log.Warn("counting claimable issues", "component", "httpapi", "repo", repo.ID, "err", cerr)
		} else {
			claimable = len(cs)
			if r.URL.Query().Get("claimable") == "1" {
				issues = cs
			}
		}
	}
	items := make([]issueResponse, 0, len(issues))
	for _, is := range issues {
		items = append(items, trackerIssueListJSON(is))
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": items, "claimable_count": claimable})
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
	is, err := s.store.CreateIssueWithLabels(r.Context(), repo.ID, req.Title, req.Body, labelIDs,
		store.CommentAuthorOperator, nil, s.now())
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

// handleIssueUpdate is PATCH /api/v1/repos/{id}/issues/{n}: a title/body/state
// patch. Field validation runs FIRST on every binding — decode into raw fields,
// patchString per key, whitespace-only title and a non-enum state both 400 with
// the pinned wording — so an invalid state value is refused with the enum
// message before any binding check, whatever the binding. After validation the
// patch splits by what it touches:
//
//   - A STATE change stays store-routed and builtin-only. The tracker seam has
//     no state op (it carries CloseIssue but no reopen — see forgeStateMessage),
//     so the store owns the open↔closed transition and its closed_at stamp; a
//     forge-bound state change is a 400 (not the 409 sibling: the request is
//     understood, it just names a field this binding cannot patch). On builtin
//     the whole patch — title/body ride along in the same transaction — goes
//     through store.UpdateIssue verbatim, exactly as before the seam split.
//   - A TITLE/BODY-only patch (including the legal empty-patch existence-verifying
//     no-op) rides Tracker.EditIssue on EITHER binding (ADR-0046). Last write
//     wins: no concurrency token, mirroring the seam contract and the agent
//     PATCH. EditIssue answers the LIST shape ("an edit is a write, not a thread
//     read"), but this endpoint's pinned response is the DETAIL shape, so a
//     follow-up tk.Issue read supplies the comment thread — on builtin that JSON
//     is byte-identical to the pre-seam storeIssueDetailJSON. issue.changed is
//     published on either binding once the edit succeeds (agentapi's
//     publishIssueChanged convention); a tracker error publishes nothing.
func (s *Server) handleIssueUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
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

	// A state change owns closed_at and has no seam op, so it stays store-routed
	// and builtin-only; title/body may ride along in the one store transaction.
	if u.State.Set {
		if repo.TrackerBinding != store.TrackerBindingBuiltin {
			writeError(w, http.StatusBadRequest, forgeStateMessage)
			return
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
		return
	}

	// Title/body-only patch: ride the seam on either binding. A non-nil empty
	// Body clears the body — that already works through EditIssue.
	tk, ok := s.trackerForRepo(w, r, repo)
	if !ok {
		return
	}
	var edit tracker.IssueEdit
	if u.Title.Set {
		edit.Title = &u.Title.Value
	}
	if u.Body.Set {
		edit.Body = &u.Body.Value
	}
	if _, err := tk.EditIssue(r.Context(), n, edit); err != nil {
		s.writeTrackerError(w, "editing issue", repo, err)
		return
	}
	// Either binding emits issue.changed once the edit lands — before the
	// re-read, so a failed read still lets subscribed views refetch (agentapi
	// convention).
	s.publishIssueChanged(repo.ID)
	// EditIssue answers the LIST shape by seam contract; re-read for the pinned
	// DETAIL response (the comment thread the endpoint has always returned).
	is, err := tk.Issue(r.Context(), n)
	if err != nil {
		s.writeTrackerError(w, "loading issue", repo, err)
		return
	}
	writeJSON(w, http.StatusOK, trackerIssueDetailJSON(is))
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
