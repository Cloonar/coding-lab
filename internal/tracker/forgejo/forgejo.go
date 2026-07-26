// Package forgejo is the Forgejo REST implementation of the tracker.Tracker
// seam (design §4d, M4). It replaces v0's tea-CLI shell-outs
// (docs/reference/lab-v0/forgejo.go had no REST — only origin parsing; the
// endpoint mappings here are the M4 rewrite, port-spec §7) while preserving
// the tracker-contract semantics that must survive: the ready queue is open
// issues carrying the ready-for-agent label; the issues endpoint is queried
// with type=issues so Forgejo does not fold PRs into the list; pulls are
// listed as the bounded open + recent-closed view (issue #176) with the
// three-valued open|merged|closed state derived from Forgejo's state+merged
// fields (the M5 done-signal — a merged afk/<N> PR is no longer "open" —
// reads PullsForHead, never this listing); a tracker call either yields the
// full result or an error carrying operation context — never partial data,
// and never the forge token.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

const (
	// defaultRequestTimeout bounds every REST call. v0's tea shell-outs had no
	// deadline; the port-spec (§5) asks the rewrite to add a sane HTTP timeout —
	// 30s, applied per request as a child context so pagination and the
	// two-call Issue(n) read each get a fresh budget.
	defaultRequestTimeout = 30 * time.Second

	// pageLimit is the page size for list endpoints. Forgejo paginates issues
	// and pulls; a non-paginating client would silently drop everything past
	// the first page — exactly the silent truncation the done-signal contract
	// (PullsForHead's fallback walk) warns against. We loop until an empty page (see
	// fetchPages — a short page proves nothing, because the server silently
	// clamps limit to its own max_response_items).
	pageLimit = 50

	// maxPages bounds fetchPages. A listing still not exhausted after maxPages
	// pages (5000 elements at pageLimit=50) aborts with an explicit error:
	// silent truncation would eat the done-signal, and an endpoint that ignores
	// pagination entirely must surface as a loud bug, not an infinite loop.
	maxPages = 100

	// errBodySnippetMax caps how much of a non-2xx response body is folded into
	// the error, keeping it a short diagnostic line.
	errBodySnippetMax = 512
)

// Client is a Forgejo REST client scoped to a single {owner}/{repo}. It
// implements tracker.Tracker.
type Client struct {
	httpClient *http.Client
	baseURL    string // https://<host>/api/v1 (no trailing slash)
	token      string // forge_token; sent as "Authorization: token <token>", never logged or surfaced in errors
	owner      string
	repo       string
	timeout    time.Duration
}

var _ tracker.Tracker = (*Client)(nil)

// New builds a Forgejo REST client. baseURL is the API root
// (https://<host>/api/v1); token is the repo's vault-decrypted forge_token;
// owner/repo come from tracker.Detect. A nil httpClient defaults to a plain
// http.Client (call-level context deadlines do the timing).
func New(httpClient *http.Client, baseURL, token, owner, repo string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		owner:      owner,
		repo:       repo,
		timeout:    defaultRequestTimeout,
	}
}

// --- Forgejo JSON shapes ---------------------------------------------------

type fjLabel struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type fjIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []fjLabel `json:"labels"`
	Comments  int       `json:"comments"` // Forgejo's comment count — the list views' CommentsCount
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type fjUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
}

type fjComment struct {
	Body      string    `json:"body"`
	User      fjUser    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// fjReview is one row of the pull-reviews list (swagger PullReview): the
// reviewer under `user`, the review-event word under `state` (ReviewStateType:
// APPROVED, REQUEST_CHANGES, COMMENT, REQUEST_REVIEW, PENDING, or the empty
// UNKNOWN), the review body, and the forge's `dismissed` flag.
type fjReview struct {
	User      fjUser `json:"user"`
	State     string `json:"state"`
	Body      string `json:"body"`
	Dismissed bool   `json:"dismissed"`
}

type fjPullHead struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

type fjPull struct {
	Number  int        `json:"number"`
	Title   string     `json:"title"`
	Body    string     `json:"body"`
	State   string     `json:"state"`
	Merged  bool       `json:"merged"`
	Head    fjPullHead `json:"head"`
	Base    fjPullHead `json:"base"` // PullsForHead's fallback filters on it (issue #176)
	HTMLURL string     `json:"html_url"`
}

// fjCommitStatus is one row of a combined-status response's `statuses` array
// — a single CI job/check reported against a commit (Forgejo Actions reports
// its own job results this way). The row's state word arrives under the JSON
// key `status` (swagger CommitStatus.status) — only the enclosing combined
// object keys its aggregate as `state`. The two keys diverging on the wire is
// exactly the trap that shipped this decoder reading `state` off the rows:
// every row decoded as "" and normalized to failure, so an all-green run
// reported red.
type fjCommitStatus struct {
	Context     string `json:"context"`
	State       string `json:"status"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// fjCombinedStatus is the decode shape of GET
// .../commits/{sha}/status: the forge's own aggregate State (see Checks'
// doc comment for why it is decoded but never used) plus the per-job Statuses
// rows that Checks actually maps.
type fjCombinedStatus struct {
	State    string           `json:"state"`
	Statuses []fjCommitStatus `json:"statuses"`
}

// --- Tracker methods -------------------------------------------------------

// ReadyIssues returns the ready queue: open issues carrying the
// ready-for-agent label. type=issues keeps PRs out of the list; state and
// labels are filtered server-side as an optimization, but label membership is
// re-checked client-side: Forgejo discards nonexistent label names from the
// labels= param, so until ready-for-agent exists on the forge repo the
// server-side filter is a no-op that would return EVERY open issue — handing
// the M5 scheduler work nobody marked ready. No comments are loaded.
func (c *Client) ReadyIssues(ctx context.Context) ([]tracker.Issue, error) {
	q := url.Values{}
	q.Set("type", "issues")
	q.Set("state", tracker.StateOpen)
	q.Set("labels", tracker.ReadyLabel)
	fjs, err := fetchPages[fjIssue](ctx, c, c.issuesPath(), q)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Issue, 0, len(fjs))
	for _, fj := range fjs {
		if !fjHasLabel(fj, tracker.ReadyLabel) {
			continue
		}
		out = append(out, toIssue(fj, nil))
	}
	return out, nil
}

// fjHasLabel reports whether the decoded issue carries a label named name
// (exact match — the tracker vocabulary treats label names as identifiers).
func fjHasLabel(fj fjIssue, name string) bool {
	for _, l := range fj.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// Issues returns the issues in the given state (open|closed|all) for the
// operator list view, with bounded reads (issue #176): the OPEN view walks
// the full open set (the working set caps it), while the closed view is ONE
// request for the tracker.RecentClosedWindow most recently UPDATED closed
// issues — Forgejo's issue sort enum carries recentupdate but no recentclose
// (that sort is pulls-only), and closing an issue bumps updated_at, so
// recentupdate approximates recently-closed; a later comment bumping an old
// closed issue into the window is harmless, the window being a recency view
// rather than a completeness contract. The all view is the open walk plus
// the closed window, open first (the states are disjoint — no dedupe). Any
// other filter is a loud error: the param now fans out into distinct reads,
// so an unrecognized value must not silently misquery. type=issues excludes
// PRs; no comments are loaded.
func (c *Client) Issues(ctx context.Context, state string) ([]tracker.Issue, error) {
	switch state {
	case tracker.StateOpen, tracker.StateClosed, tracker.StateAll:
	default:
		return nil, fmt.Errorf("forgejo issues: invalid state filter %q", state)
	}
	var fjs []fjIssue
	if state != tracker.StateClosed {
		q := url.Values{}
		q.Set("type", "issues")
		q.Set("state", tracker.StateOpen)
		open, err := fetchPages[fjIssue](ctx, c, c.issuesPath(), q)
		if err != nil {
			return nil, err
		}
		fjs = append(fjs, open...)
	}
	if state != tracker.StateOpen {
		closed, err := recentClosedWindow[fjIssue](ctx, c, c.issuesPath(),
			url.Values{"type": {"issues"}, "sort": {"recentupdate"}})
		if err != nil {
			return nil, err
		}
		fjs = append(fjs, closed...)
	}
	return mapIssues(fjs), nil
}

// Issue returns a single issue fully populated with its comments (two REST
// calls: the issue, then its comments in ONE un-paginated GET — the seed
// prompt tells the agent to read comments). The comments endpoint must not go
// through fetchPages: Forgejo's GET /issues/{index}/comments accepts only
// since/before and ignores page/limit, always returning the full list, so a
// pagination loop over it never terminates once an issue has >= pageLimit
// comments.
func (c *Client) Issue(ctx context.Context, number int) (tracker.Issue, error) {
	var fj fjIssue
	if err := c.do(ctx, http.MethodGet, c.issuePath(number), nil, nil, &fj); err != nil {
		return tracker.Issue{}, err
	}
	var fcs []fjComment
	if err := c.do(ctx, http.MethodGet, c.issuePath(number)+"/comments", nil, nil, &fcs); err != nil {
		return tracker.Issue{}, err
	}
	comments := make([]tracker.Comment, 0, len(fcs))
	for _, fc := range fcs {
		comments = append(comments, toComment(fc))
	}
	return toIssue(fj, comments), nil
}

// CreateComment posts a comment body on an issue.
func (c *Client) CreateComment(ctx context.Context, number int, body string) error {
	req := struct {
		Body string `json:"body"`
	}{Body: body}
	return c.do(ctx, http.MethodPost, c.issuePath(number)+"/comments", nil, req, nil)
}

// Pulls returns the repo's OPEN pulls plus the tracker.RecentClosedWindow
// most recently closed ones (merged or not), open set first — the bounded
// list view of issue #176. The old state=all walk cost one request per
// pageLimit of repo HISTORY plus the empty-page probe (6.7s for ~174 PRs
// measured on the live forge, growing forever) for a listing whose consumers
// (labctl pr list, the operator surface) only need the working set plus a
// recency view; the done-signal read that once justified state=all lives on
// PullsForHead now, and a merged run PR still appears here while recent. Two
// request shapes:
//
//   - the open walk: fetchPages with state=open — bounded by the open set,
//     pagination kept because open counts can legitimately exceed one page.
//   - the closed window: exactly ONE request via recentClosedWindow with
//     sort=recentclose (in this forge's swagger sort enum for pulls,
//     verified against the live Forgejo 15.0.5), newest-closed-first;
//     state=closed covers merged and closed-unmerged alike, the three-valued
//     state still derived from state+merged per row.
func (c *Client) Pulls(ctx context.Context) ([]tracker.PullRef, error) {
	q := url.Values{}
	q.Set("state", tracker.StateOpen)
	fjs, err := fetchPages[fjPull](ctx, c, c.pullsPath(), q)
	if err != nil {
		return nil, err
	}
	closed, err := recentClosedWindow[fjPull](ctx, c, c.pullsPath(),
		url.Values{"sort": {"recentclose"}})
	if err != nil {
		return nil, err
	}
	fjs = append(fjs, closed...)
	out := make([]tracker.PullRef, 0, len(fjs))
	for _, fj := range fjs {
		out = append(out, toPullRef(fj))
	}
	return out, nil
}

// PullsForHead lists the pulls whose head branch is head AND base branch is
// base, across all states, in a bounded number of requests (issue #176) —
// the reaper's per-branch done-signal read, replacing the O(history) Pulls
// walk. The fast path is Forgejo's by-base-head lookup
// (GET /pulls/{base}/{head}), which resolves the three hot reaper states in
// ONE request each:
//
//   - 404 → no pull for that head+base: empty non-nil slice, success (the
//     seam's "empty result is success" convention).
//   - An open or merged answer is definitive — it IS a done-signal, and the
//     only reason not to trust a single row (a closed one shadowing a live
//     sibling, below) cannot apply — so it returns as the one candidate.
//     Its HeadBranch is the QUERIED name, never the response's rendered
//     head.ref: Forgejo renders a merged pull whose head branch was since
//     deleted with a synthetic refs/pull/N/head (verified live on lab's
//     forge), and the by-name query key is authoritative (the seam pin).
//   - A closed-unmerged answer is the RARE case that cannot be trusted
//     alone: Forgejo's GetPullRequestByBaseHeadInfo fetches its row with a
//     plain xorm .Get() — NO ORDER BY — so when several pulls share
//     base+head (a branch re-used after a closed-unmerged PR) the returned
//     row is unspecified, and the closed one may shadow a live open or
//     merged sibling. Closed-unmerged is exactly the state the done-signal
//     reads as absence (DonePull's floor), so answering it blind would
//     falsely fail a run. Only this answer pays for the full state=all walk
//     (the one unbounded listing left in this client — Pulls itself is the
//     bounded open+recent-closed view now), filtered client-side to the head+base
//     pair — possibly returning the closed pull among the matches, possibly
//     none at all when every colliding branch was deleted and the rendered
//     refs no longer match; DonePull answers false either way.
//
// Any non-404 error surfaces as-is — no partial data (the seam convention).
func (c *Client) PullsForHead(ctx context.Context, head, base string) ([]tracker.PullRef, error) {
	var fj fjPull
	err := c.do(ctx, http.MethodGet, c.pullByBaseHeadPath(base, head), nil, nil, &fj)
	switch {
	case errors.Is(err, tracker.ErrNotFound):
		return []tracker.PullRef{}, nil
	case err != nil:
		return nil, err
	}
	if state := derivePullState(fj.State, fj.Merged); state != tracker.PullClosed {
		return []tracker.PullRef{{
			Number:     fj.Number,
			HeadBranch: head, // the queried name, never the rendered head.ref (see above)
			State:      state,
			URL:        fj.HTMLURL,
		}}, nil
	}
	// Closed-unmerged: the rare fallback walk (see the doc comment).
	q := url.Values{}
	q.Set("state", tracker.StateAll)
	fjs, err := fetchPages[fjPull](ctx, c, c.pullsPath(), q)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.PullRef, 0, 1)
	for _, p := range fjs {
		if p.Head.Ref == head && p.Base.Ref == base {
			out = append(out, toPullRef(p))
		}
	}
	return out, nil
}

// Pull returns one pull request in full detail, body included — the read
// behind labctl pr view. An unknown number is Forgejo's 404, which the
// statusError unwraps to tracker.ErrNotFound like every single-subject read.
func (c *Client) Pull(ctx context.Context, number int) (tracker.PullDetail, error) {
	var fj fjPull
	if err := c.do(ctx, http.MethodGet, c.pullPath(number), nil, nil, &fj); err != nil {
		return tracker.PullDetail{}, err
	}
	return toPullDetail(fj), nil
}

// Checks returns the CI check/status rows for pull `number`'s CURRENT head
// commit. It resolves the head SHA AT QUERY TIME — a fresh GET of the pull
// (the same endpoint/decode Pull uses), so a push or rebase that landed
// moments before the read is reflected, never a memoized commit — then GETs
// the combined-status endpoint for that SHA, where Forgejo Actions reports
// its own job results as commit statuses (one row per job). An unknown pull
// number fails on that first GET exactly like Pull: the statusError 404 path
// unwraps to tracker.ErrNotFound.
//
// Only the endpoint's `statuses` rows are mapped; its own combined `state`
// field is deliberately IGNORED. Forgejo reports that combined state as
// "pending" for a commit carrying ZERO registered statuses — there is no CI
// to be pending on — which would spin a waiting agent forever if lab trusted
// it as the aggregate instead of computing one. tracker.ChecksState computes
// the real aggregate client-side from the rows this method returns (see
// checks.go); Checks' only job is decoding those rows faithfully. Zero
// statuses is success, not an error: a non-nil empty slice.
func (c *Client) Checks(ctx context.Context, number int) ([]tracker.Check, error) {
	var fj fjPull
	if err := c.do(ctx, http.MethodGet, c.pullPath(number), nil, nil, &fj); err != nil {
		return nil, err // 404 → tracker.ErrNotFound, same as Pull
	}
	statuses, err := c.fetchCommitStatuses(ctx, fj.Head.Sha)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Check, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, toCheck(s))
	}
	return out, nil
}

// checkLogJobPath matches a Forgejo Actions job page's URL path —
// /{owner}/{repo}/actions/runs/{run}/jobs/{job} — the shape a commit status's
// target_url carries when the job's logs are served by THIS forge. Only the
// path is matched (target_url may arrive absolute or path-only, and the log
// route is rebuilt on the web origin either way), and only a match names logs
// lab can proxy: an external CI service reports some other URL, or none, whose
// output is not this forge's to serve.
var checkLogJobPath = regexp.MustCompile(`^/.+/actions/runs/[0-9]+/jobs/[0-9]+$`)

// checkLogAttemptCap bounds CheckLog's per-attempt log probe. A job with more
// rerun attempts than this is far past anything a real workflow produces, so
// walking to the cap without the terminating 404 is treated as the adapter no
// longer matching the forge — a loud mismatch — rather than an unbounded loop.
const checkLogAttemptCap = 20

// CheckLog fetches the raw plaintext log of the named check on pull `number`'s
// CURRENT head commit by proxying Forgejo's undocumented per-attempt web log
// route. It follows Checks' resolution exactly — GET the pull for head.sha,
// then the combined-status rows for that SHA — and finds the row whose
// `context` equals name; a name no row carries is tracker.ErrUnknownCheck.
// That row's target_url must be a Forgejo Actions job page
// (/{owner}/{repo}/actions/runs/{run}/jobs/{job}); anything else (an external
// CI URL, or none) is tracker.ErrUnsupported — those logs are not this forge's
// to serve.
//
// The log itself lives on the WEB origin (baseURL minus its /api/v1 suffix),
// not the REST API, at <job-path>/attempt/{k}/logs. lab probes attempts k=1,2,…
// with the same "Authorization: token" the REST calls send (the route serves
// public repos anonymously today, but the credential is sent so a private repo
// does not silently break), taking each 200 text/plain body and stopping on the
// FIRST 404 — the attempt past the last real one — to return the latest
// attempt's log (a rerun's newest attempt wins). A still-running job's route
// still answers 200 with the partial log so far, so a job in flight yields what
// it has, never an error. Any other answer is loud: a 404 on attempt 1 (the
// route the adapter is coupled to has moved), a non-200 status, a 200 whose
// media type is not text/plain (an HTML login page, say), or exhausting the
// attempt cap all wrap tracker.ErrLogAdapterMismatch with an actionable message
// — and the forge token, living only in the request header, never reaches it.
func (c *Client) CheckLog(ctx context.Context, number int, name string) ([]byte, error) {
	var fj fjPull
	if err := c.do(ctx, http.MethodGet, c.pullPath(number), nil, nil, &fj); err != nil {
		return nil, err // 404 → tracker.ErrNotFound, same as Checks
	}
	statuses, err := c.fetchCommitStatuses(ctx, fj.Head.Sha)
	if err != nil {
		return nil, err
	}
	target, found := "", false
	for _, s := range statuses {
		if s.Context == name {
			target, found = s.TargetURL, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("check %q: %w", name, tracker.ErrUnknownCheck)
	}
	u, err := url.Parse(target)
	if err != nil || !checkLogJobPath.MatchString(u.Path) {
		return nil, fmt.Errorf("%w: check %q does not point at a Forgejo Actions job (target_url %q)",
			tracker.ErrUnsupported, name, target)
	}

	webOrigin := strings.TrimSuffix(c.baseURL, "/api/v1")
	var last []byte
	for k := 1; ; k++ {
		if k > checkLogAttemptCap {
			return nil, fmt.Errorf("%w: GET %s answered no terminating 404 within %d attempts for check %q — lab's Forgejo log adapter does not match this forge version; file an issue on coding-lab, then debug from local repro",
				tracker.ErrLogAdapterMismatch, u.Path+"/attempt/N/logs", checkLogAttemptCap, name)
		}
		logPath := fmt.Sprintf("%s/attempt/%d/logs", u.Path, k)
		status, mediaType, body, err := c.doRawLog(ctx, webOrigin+logPath)
		if err != nil {
			return nil, err
		}
		switch {
		case status == http.StatusOK && mediaType == "text/plain":
			last = body
		case status == http.StatusNotFound:
			if k == 1 {
				return nil, fmt.Errorf("%w: GET %s answered 404 for check %q — lab's Forgejo log adapter does not match this forge version; file an issue on coding-lab, then debug from local repro",
					tracker.ErrLogAdapterMismatch, logPath, name)
			}
			return last, nil
		default:
			return nil, fmt.Errorf("%w: GET %s answered %d %q for check %q — lab's Forgejo log adapter does not match this forge version; file an issue on coding-lab, then debug from local repro",
				tracker.ErrLogAdapterMismatch, logPath, status, mediaType, name)
		}
	}
}

// fetchCommitStatuses walks the combined-status endpoint's embedded
// `statuses` array page by page, following the same page/limit and
// paginate-until-empty convention as every list endpoint in this client (see
// fetchPages) so a commit carrying many CI job rows is never silently
// truncated. It cannot go through fetchPages itself: that helper decodes a
// bare JSON array, while this endpoint's response is a single object with
// the rows embedded under `statuses`.
func (c *Client) fetchCommitStatuses(ctx context.Context, sha string) ([]fjCommitStatus, error) {
	path := c.commitStatusPath(sha)
	var all []fjCommitStatus
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("forgejo GET %s: listing exceeds %d pages; refusing to silently truncate", path, maxPages)
		}
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("limit", strconv.Itoa(pageLimit))
		var combined fjCombinedStatus
		if err := c.do(ctx, http.MethodGet, path, q, nil, &combined); err != nil {
			return nil, err
		}
		if len(combined.Statuses) == 0 {
			return all, nil
		}
		all = append(all, combined.Statuses...)
	}
}

// CreatePull opens a pull request from head into base and returns the created
// PullRef. (Exercised end-to-end in M5/M6; the agent API injects/validates
// Closes #N and applies incogni sanitization before this call.)
func (c *Client) CreatePull(ctx context.Context, head, base, title, body string) (tracker.PullRef, error) {
	req := struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}{Head: head, Base: base, Title: title, Body: body}
	var fj fjPull
	if err := c.do(ctx, http.MethodPost, c.pullsPath(), nil, req, &fj); err != nil {
		return tracker.PullRef{}, err
	}
	return toPullRef(fj), nil
}

// MergePull merges pull request `number` on the forge and returns its merged
// PullRef. The forge — not lab — decides mergeability: MergePull does NOT
// pre-check required status checks or branch protection. It first GETs the
// pull so an already-merged one is a convergent no-op success (naming the
// merged state), then POSTs the fixed "merge" method (a merge commit; the
// forge applies its own configured strategy and enforces its own protections).
// A refusal — a required check unsatisfied, a protected base, a conflict — is
// the forge's own non-2xx answer, wrapped in tracker.ErrMergeRejected with
// that answer's body verbatim (the actionable message); an unknown number
// stays tracker.ErrNotFound. The merge is authorized by this client's server-
// side forge token, so no forge credential ever reaches the agent session
// (ADR-0014). The head branch is not deleted (delete_branch_after_merge omitted
// → false).
func (c *Client) MergePull(ctx context.Context, number int) (tracker.PullRef, error) {
	var fj fjPull
	if err := c.do(ctx, http.MethodGet, c.pullPath(number), nil, nil, &fj); err != nil {
		return tracker.PullRef{}, err // 404 → tracker.ErrNotFound
	}
	if derivePullState(fj.State, fj.Merged) == tracker.PullMerged {
		return toPullRef(fj), nil // convergent no-op: already merged
	}
	req := struct {
		Do string `json:"Do"`
	}{Do: "merge"}
	if err := c.do(ctx, http.MethodPost, c.pullPath(number)+"/merge", nil, req, nil); err != nil {
		if errors.Is(err, tracker.ErrNotFound) {
			return tracker.PullRef{}, err
		}
		return tracker.PullRef{}, fmt.Errorf("%w: %s", tracker.ErrMergeRejected, err.Error())
	}
	// Merged. The head ref and web URL do not change on merge; report the
	// merged state over the pre-merge read.
	return tracker.PullRef{
		Number:     fj.Number,
		HeadBranch: fj.Head.Ref,
		State:      tracker.PullMerged,
		URL:        fj.HTMLURL,
	}, nil
}

// Reviews lists the submitted reviews on pull `number`, oldest first — the read
// behind the human half of the autoland loop's hybrid rejected-state
// (ADR-0048). Forgejo returns them submission-ordered;
// each review's user login is the Reviewer, its state normalized onto lab's
// Review* vocabulary (an unrecognized state passes through lowercased), and its
// `dismissed` flag carried through. The endpoint paginates like every list
// (page/limit, swagger-declared), so it walks through fetchPages — a single GET
// would silently truncate a pull with many reviews. An unknown number is
// Forgejo's 404, which statusError unwraps to tracker.ErrNotFound like every
// single-subject read.
func (c *Client) Reviews(ctx context.Context, number int) ([]tracker.Review, error) {
	fjs, err := fetchPages[fjReview](ctx, c, c.pullPath(number)+"/reviews", nil)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Review, 0, len(fjs))
	for _, fj := range fjs {
		out = append(out, toReview(fj))
	}
	return out, nil
}

// RerequestReview re-requests review from every reviewer whose LATEST
// verdict-bearing, non-dismissed review requests changes. It GETs the reviews,
// reduces them to the latest verdict per reviewer (submission order, last
// verdict wins; only APPROVED/REQUEST_CHANGES rows carry a verdict — dismissed
// reviews and non-verdict rows like COMMENT, REQUEST_REVIEW, or PENDING are
// skipped), and if any reviewer's latest verdict is changes-requested POSTs
// their logins to /requested_reviewers. No such reviewer is a convergent no-op
// success — nothing to re-request — mirroring MergePull's already-merged
// no-op. Accepted consequence: a successful re-request does not change the
// reviewer's latest verdict (they ARE still changes-requested until they
// re-review), so a repeated call re-POSTs the same reviewers instead of
// no-opping — faithful, and the forge handles a duplicate request
// idempotently. A forge refusal wraps tracker.ErrReviewRejected with its own
// words; an unknown number stays tracker.ErrNotFound.
func (c *Client) RerequestReview(ctx context.Context, number int) error {
	reviews, err := c.Reviews(ctx, number)
	if err != nil {
		return err
	}
	reviewers := changesRequestedReviewers(reviews)
	if len(reviewers) == 0 {
		return nil // convergent no-op: nobody is currently requesting changes
	}
	req := struct {
		Reviewers []string `json:"reviewers"`
	}{Reviewers: reviewers}
	if err := c.do(ctx, http.MethodPost, c.pullPath(number)+"/requested_reviewers", nil, req, nil); err != nil {
		if errors.Is(err, tracker.ErrNotFound) {
			return err
		}
		return fmt.Errorf("%w: %s", tracker.ErrReviewRejected, err.Error())
	}
	return nil
}

// CommentPull posts a plain discussion comment on pull `number`. A PR shares
// the issue-comment number space on Forgejo, so this posts to the same
// issue-comment endpoint CreateComment uses. An unknown number stays
// tracker.ErrNotFound.
func (c *Client) CommentPull(ctx context.Context, number int, body string) error {
	req := struct {
		Body string `json:"body"`
	}{Body: body}
	return c.do(ctx, http.MethodPost, c.issuePath(number)+"/comments", nil, req, nil)
}

// PullComments lists pull `number`'s discussion comments, oldest first — the
// READ counterpart of CommentPull, the autoland poller's route onto verdict
// markers (ADR-0048). A PR shares the issue-comment number space on Forgejo,
// so this reads and maps the SAME endpoint Issue's comment thread does. It
// cannot go through fetchPages for the same reason Issue doesn't: Forgejo's
// GET /issues/{index}/comments accepts only since/before and ignores
// page/limit, always returning the full list — a pagination loop over it
// never terminates once a pull has >= pageLimit comments. An unknown number
// is Forgejo's 404, which statusError unwraps to tracker.ErrNotFound like
// every single-subject read.
func (c *Client) PullComments(ctx context.Context, number int) ([]tracker.Comment, error) {
	var fcs []fjComment
	if err := c.do(ctx, http.MethodGet, c.issuePath(number)+"/comments", nil, nil, &fcs); err != nil {
		return nil, err
	}
	comments := make([]tracker.Comment, 0, len(fcs))
	for _, fc := range fcs {
		comments = append(comments, toComment(fc))
	}
	return comments, nil
}

// CloseIssue closes an issue via PATCH state=closed.
func (c *Client) CloseIssue(ctx context.Context, number int) error {
	req := struct {
		State string `json:"state"`
	}{State: "closed"}
	return c.do(ctx, http.MethodPatch, c.issuePath(number), nil, req, nil)
}

// defaultLabelColor is the swatch EnsureLabel supplies when the caller omits
// one — the same value as the builtin binding's store default
// (store.LabelDefaultColor), so a label ensured on either binding looks the
// same. Forgejo requires a color on create, hence the client-side default.
const defaultLabelColor = "#6b7280"

// CreateIssue opens an issue with the named labels attached at creation.
// Label names are resolved to Forgejo ids client-side (the labels-are-IDs
// quirk stays behind this seam); a name the repo does not define is
// tracker.ErrUnknownLabel before anything is created — Forgejo silently
// discards unknown entries, which would turn a typo into an unlabelled issue.
func (c *Client) CreateIssue(ctx context.Context, title, body string, labels []string) (tracker.Issue, error) {
	ids, err := c.labelIDsByName(ctx, labels)
	if err != nil {
		return tracker.Issue{}, err
	}
	req := struct {
		Title  string  `json:"title"`
		Body   string  `json:"body"`
		Labels []int64 `json:"labels,omitempty"`
	}{Title: title, Body: body, Labels: ids}
	var fj fjIssue
	if err := c.do(ctx, http.MethodPost, c.issuesPath(), nil, req, &fj); err != nil {
		return tracker.Issue{}, err
	}
	return toIssue(fj, nil), nil
}

// EditIssue applies a title/body patch via PATCH /issues/{n}. Only the set
// fields go on the wire: *string with omitempty omits a nil pointer but keeps a
// non-nil pointer to "" (a cleared body reaches Forgejo as `"body":""`), so a
// title-only edit sends no body key and a body-only edit no title key. The
// forge judges nothing about emptiness — title-non-empty is the API layer's
// guard; a 404 (unknown issue) unwraps to tracker.ErrNotFound like every
// single-subject call. The response maps through toIssue in LIST shape (no
// comments), matching CreateIssue.
func (c *Client) EditIssue(ctx context.Context, number int, edit tracker.IssueEdit) (tracker.Issue, error) {
	req := struct {
		Title *string `json:"title,omitempty"`
		Body  *string `json:"body,omitempty"`
	}{Title: edit.Title, Body: edit.Body}
	var fj fjIssue
	if err := c.do(ctx, http.MethodPatch, c.issuePath(number), nil, req, &fj); err != nil {
		return tracker.Issue{}, err
	}
	return toIssue(fj, nil), nil
}

// AddIssueLabels attaches the named labels to an issue in one POST, after the
// same strict client-side name resolution as CreateIssue.
func (c *Client) AddIssueLabels(ctx context.Context, number int, labels []string) error {
	ids, err := c.labelIDsByName(ctx, labels)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	req := struct {
		Labels []int64 `json:"labels"`
	}{Labels: ids}
	return c.do(ctx, http.MethodPost, c.issuePath(number)+"/labels", nil, req, nil)
}

// RemoveIssueLabels detaches the named labels from an issue — one DELETE per
// label, the only remove Forgejo's API offers — after strict name resolution
// of the whole set, so a typo fails before the first detach.
func (c *Client) RemoveIssueLabels(ctx context.Context, number int, labels []string) error {
	ids, err := c.labelIDsByName(ctx, labels)
	if err != nil {
		return err
	}
	for _, id := range ids {
		path := c.issuePath(number) + "/labels/" + strconv.FormatInt(id, 10)
		if err := c.do(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// Labels lists the repo's labels ordered by name (the contract order; the
// forge's own order is unspecified).
func (c *Client) Labels(ctx context.Context) ([]tracker.Label, error) {
	fjs, err := c.repoLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Label, 0, len(fjs))
	for _, fj := range fjs {
		out = append(out, toLabel(fj))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// EnsureLabel returns the repo's label of that name, creating it when absent.
// List-first keeps the op idempotent whatever the forge does with duplicate
// names; a create that still answers a duplicate conflict (a concurrent
// ensure won the race) resolves by re-listing. An existing label's
// color/description are never overwritten.
func (c *Client) EnsureLabel(ctx context.Context, name, color, description string) (tracker.Label, error) {
	if l, ok, err := c.labelByName(ctx, name); err != nil {
		return tracker.Label{}, err
	} else if ok {
		return l, nil
	}
	if color == "" {
		color = defaultLabelColor
	}
	req := struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}{Name: name, Color: color, Description: description}
	var created fjLabel
	err := c.do(ctx, http.MethodPost, c.labelsPath(), nil, req, &created)
	if err == nil {
		return toLabel(created), nil
	}
	var se *statusError
	if errors.As(err, &se) && (se.status == http.StatusConflict || se.status == http.StatusUnprocessableEntity) {
		if l, ok, lerr := c.labelByName(ctx, name); lerr == nil && ok {
			return l, nil
		}
	}
	return tracker.Label{}, err
}

// labelByName finds one repo label by exact name; ok=false when the repo does
// not define it.
func (c *Client) labelByName(ctx context.Context, name string) (tracker.Label, bool, error) {
	fjs, err := c.repoLabels(ctx)
	if err != nil {
		return tracker.Label{}, false, err
	}
	for _, fj := range fjs {
		if fj.Name == name {
			return toLabel(fj), true, nil
		}
	}
	return tracker.Label{}, false, nil
}

// repoLabels fetches the repo's full label set (paginated like every list).
func (c *Client) repoLabels(ctx context.Context) ([]fjLabel, error) {
	return fetchPages[fjLabel](ctx, c, c.labelsPath(), nil)
}

// labelIDsByName resolves label names to Forgejo label ids, deduplicated,
// failing on the first name the repo does not define (tracker.ErrUnknownLabel,
// carrying the name) — strict resolution is the seam contract: Forgejo
// discards unknown label entries instead of erroring, the same quirk that
// forces the ready queue's client-side verification. Nil in, nil out.
func (c *Client) labelIDsByName(ctx context.Context, names []string) ([]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	fjs, err := c.repoLabels(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]int64, len(fjs))
	for _, fj := range fjs {
		byName[fj.Name] = fj.ID
	}
	seen := make(map[int64]struct{}, len(names))
	ids := make([]int64, 0, len(names))
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

// --- path helpers ----------------------------------------------------------

// repoPath builds /repos/{owner}/{repo}{suffix} with the owner and repo
// segments path-escaped: both come from the operator-supplied remote URL, so
// reserved characters (?, #, %, /) must target the literal path instead of
// being reparsed as query, fragment, or extra segments when do() builds the
// request URL.
func (c *Client) repoPath(suffix string) string {
	return "/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.repo) + suffix
}

func (c *Client) issuesPath() string     { return c.repoPath("/issues") }
func (c *Client) pullsPath() string      { return c.repoPath("/pulls") }
func (c *Client) labelsPath() string     { return c.repoPath("/labels") }
func (c *Client) issuePath(n int) string { return c.repoPath("/issues/" + strconv.Itoa(n)) }
func (c *Client) pullPath(n int) string  { return c.repoPath("/pulls/" + strconv.Itoa(n)) }

// pullByBaseHeadPath is Forgejo's by-base-head pull lookup for one base/head
// branch pair (PullsForHead's fast path). Branch names may carry '/'
// (afk/63), so each name is escaped as ONE path segment — url.PathEscape
// turns the '/' into %2F, which Forgejo requires to keep the branch from
// splitting into extra segments (verified against the live forge, 15.0.5).
func (c *Client) pullByBaseHeadPath(base, head string) string {
	return c.repoPath("/pulls/" + url.PathEscape(base) + "/" + url.PathEscape(head))
}

// commitStatusPath is the combined-status endpoint for one commit SHA. The
// SHA comes from the forge's own Pull response rather than operator input,
// but it is escaped like every other path segment built from server data.
func (c *Client) commitStatusPath(sha string) string {
	return c.repoPath("/commits/" + url.PathEscape(sha) + "/status")
}

// --- mapping ---------------------------------------------------------------

// derivePullState collapses Forgejo's (state, merged) into the three-valued
// contract vocabulary: merged wins; a closed-unmerged PR is "closed"; anything
// else is "open".
func derivePullState(state string, merged bool) string {
	switch {
	case merged:
		return tracker.PullMerged
	case state == tracker.PullClosed:
		return tracker.PullClosed
	default:
		return tracker.PullOpen
	}
}

func toIssue(fj fjIssue, comments []tracker.Comment) tracker.Issue {
	labels := make([]string, len(fj.Labels))
	for i, l := range fj.Labels {
		labels[i] = l.Name
	}
	return tracker.Issue{
		Number:        fj.Number,
		Title:         fj.Title,
		Body:          fj.Body,
		State:         fj.State,
		Labels:        labels,
		Comments:      comments,
		CommentsCount: fj.Comments,
		CreatedAt:     fj.CreatedAt,
		UpdatedAt:     fj.UpdatedAt,
	}
}

func mapIssues(fjs []fjIssue) []tracker.Issue {
	out := make([]tracker.Issue, 0, len(fjs))
	for _, fj := range fjs {
		out = append(out, toIssue(fj, nil))
	}
	return out
}

// toLabel maps a Forgejo label onto the tracker vocabulary. Forgejo stores
// colors WITHOUT the leading '#' ("6b7280"); lab's vocabulary carries it
// (store.LabelDefaultColor), so reads are normalized — writes may send either
// form, Forgejo accepts both.
func toLabel(fj fjLabel) tracker.Label {
	color := fj.Color
	if color != "" && !strings.HasPrefix(color, "#") {
		color = "#" + color
	}
	return tracker.Label{Name: fj.Name, Color: color, Description: fj.Description}
}

func toComment(fc fjComment) tracker.Comment {
	return tracker.Comment{
		Author:    fjLogin(fc.User),
		Body:      fc.Body,
		CreatedAt: fc.CreatedAt,
	}
}

// fjLogin resolves a Forgejo user's display login, falling back to username
// when login is empty (some payloads populate only one of the two).
func fjLogin(u fjUser) string {
	if u.Login != "" {
		return u.Login
	}
	return u.Username
}

// toReview maps one Forgejo review row onto lab's Review vocabulary: the
// reviewer login, the state normalized onto the Review* vocabulary, the body,
// and the forge's dismissed flag.
func toReview(fj fjReview) tracker.Review {
	return tracker.Review{
		Reviewer:  fjLogin(fj.User),
		State:     mapReviewState(fj.State),
		Body:      fj.Body,
		Dismissed: fj.Dismissed,
	}
}

// mapReviewState normalizes a Forgejo review-event word onto lab's Review*
// vocabulary. Forgejo's ReviewStateType values are APPROVED, REQUEST_CHANGES,
// COMMENT, and REQUEST_REVIEW, plus PENDING and the empty UNKNOWN. The four lab
// recognizes map onto the constants; anything else (PENDING, "", or a word this
// package does not know) passes through lowercased, so the agent still sees
// exactly what the forge said (the "backend's own words" pattern, ADR-0024).
func mapReviewState(state string) string {
	switch state {
	case "APPROVED":
		return tracker.ReviewApproved
	case "REQUEST_CHANGES":
		return tracker.ReviewChangesRequested
	case "COMMENT":
		return tracker.ReviewCommented
	case "REQUEST_REVIEW":
		return tracker.ReviewRequested
	default:
		return strings.ToLower(state)
	}
}

// changesRequestedReviewers returns the reviewer logins whose LATEST
// verdict-bearing, non-dismissed review requests changes, in first-seen order.
// Reviews arrive oldest-first, so the last VERDICT seen per reviewer is their
// latest verdict: only approved/changes-requested rows carry one — the forge's
// own rule (Forgejo's preparePullReviewType recognizes only
// APPROVED/REQUEST_CHANGES as verdict events), so a later comment or
// re-request-review row neither sets nor clears a reviewer's verdict (a
// reviewer who requested changes and then replied in the thread is STILL
// requesting changes). A dismissed review does not count (the forge cleared
// it); a reviewer whose latest verdict is an approval drops out of the set.
// Operates on the already-normalized tracker.Review vocabulary (Reviews()
// maps it).
func changesRequestedReviewers(reviews []tracker.Review) []string {
	latest := make(map[string]string, len(reviews))
	order := make([]string, 0, len(reviews))
	for _, r := range reviews {
		if r.Reviewer == "" || r.Dismissed {
			continue
		}
		if r.State != tracker.ReviewApproved && r.State != tracker.ReviewChangesRequested {
			continue // non-verdict row: commented/review_requested/pending/unknown
		}
		if _, seen := latest[r.Reviewer]; !seen {
			order = append(order, r.Reviewer)
		}
		latest[r.Reviewer] = r.State
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		if latest[name] == tracker.ReviewChangesRequested {
			out = append(out, name)
		}
	}
	return out
}

func toPullRef(fj fjPull) tracker.PullRef {
	return tracker.PullRef{
		Number:     fj.Number,
		HeadBranch: fj.Head.Ref,
		State:      derivePullState(fj.State, fj.Merged),
		URL:        fj.HTMLURL,
	}
}

// toCheck maps one Forgejo commit-status row onto lab's Check vocabulary:
// Name from context, Summary from description, URL from target_url, and
// RawState the forge's own state word verbatim (ADR-0011/0024 "backend's own
// words" pattern). State normalizes onto lab's three-word vocabulary:
// "success" -> CheckSuccess, "pending" -> CheckPending, and EVERYTHING ELSE
// ("error", "failure", "warning", or any word this package does not
// recognize) -> CheckFailure — a row that is not affirmatively green or still
// running reads as red to the consumer, while RawState still carries exactly
// what Forgejo said.
func toCheck(s fjCommitStatus) tracker.Check {
	state := tracker.CheckFailure
	switch s.State {
	case "success":
		state = tracker.CheckSuccess
	case "pending":
		state = tracker.CheckPending
	}
	return tracker.Check{
		Name:     s.Context,
		State:    state,
		RawState: s.State,
		Summary:  s.Description,
		URL:      s.TargetURL,
	}
}

func toPullDetail(fj fjPull) tracker.PullDetail {
	return tracker.PullDetail{
		Number:     fj.Number,
		Title:      fj.Title,
		Body:       fj.Body,
		State:      derivePullState(fj.State, fj.Merged),
		HeadBranch: fj.Head.Ref,
		URL:        fj.HTMLURL,
	}
}

// --- transport -------------------------------------------------------------

// fetchPages walks a paginated list endpoint (page=1,2,… limit=pageLimit)
// until an EMPTY page, concatenating the decoded elements. Terminating on an
// empty page — not a short one — is deliberate (port-spec §7 "paginate until
// empty"): Forgejo silently clamps limit to its [api].MAX_RESPONSE_ITEMS, so
// a short page proves nothing about whether more pages exist; the empty-page
// probe costs one extra request per listing. maxPages bounds a pathological
// or non-paginating endpoint with an explicit error instead of silent
// truncation or an infinite loop. Returns a non-nil empty slice contract-side
// via the callers' make(); here it may return nil on zero results (callers
// re-slice through make()).
func fetchPages[T any](ctx context.Context, c *Client, path string, base url.Values) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("forgejo GET %s: listing exceeds %d pages; refusing to silently truncate", path, maxPages)
		}
		q := url.Values{}
		for k, vs := range base {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("limit", strconv.Itoa(pageLimit))
		var chunk []T
		if err := c.do(ctx, http.MethodGet, path, q, nil, &chunk); err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			return all, nil
		}
		all = append(all, chunk...)
	}
}

// recentClosedWindow performs the ONE bounded request every recent-closed
// window shares (issue #176): state=closed&page=1&limit=RecentClosedWindow
// plus the caller's endpoint-specific params (the pulls sort=recentclose, the
// issues type=issues&sort=recentupdate). Deliberately a single do(), NEVER
// fetchPages: pagination — and its guaranteed empty-page probe — is exactly
// the O(history) request growth the window exists to remove, so a full page
// is a full window, not an invitation to fetch page=2. May return nil on zero
// results (callers re-slice through make(), the fetchPages convention).
func recentClosedWindow[T any](ctx context.Context, c *Client, path string, extra url.Values) ([]T, error) {
	q := url.Values{}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("state", tracker.StateClosed)
	q.Set("page", "1")
	q.Set("limit", strconv.Itoa(tracker.RecentClosedWindow))
	var chunk []T
	if err := c.do(ctx, http.MethodGet, path, q, nil, &chunk); err != nil {
		return nil, err
	}
	return chunk, nil
}

// do performs one REST call: it applies the per-request timeout, sets the
// token auth header, marshals reqBody (when non-nil) as JSON, and decodes a
// 2xx response into out (when non-nil). A non-2xx status becomes an error
// carrying the method, path, status, and a short body snippet — and NEVER the
// token (which lives only in the Authorization header, not the URL or error).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, reqBody, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("forgejo %s %s: encode request: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return fmt.Errorf("forgejo %s %s: build request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("forgejo %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{
			status: resp.StatusCode,
			message: fmt.Sprintf("forgejo %s %s: unexpected status %d: %s",
				method, path, resp.StatusCode, bodySnippet(resp.Body)),
		}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("forgejo %s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// doRawLog performs one GET against a FULL url (CheckLog's web log route lives
// on the web origin, not c.baseURL — hence a whole URL rather than an API
// path) and returns the response status, parsed media type, and RAW body
// without JSON-decoding: the one call in this client that reads a body
// verbatim. It mirrors do()'s hygiene — the per-request timeout, the same
// "Authorization: token" header, and a token that lives ONLY in that header so
// it can never reach the URL or a returned error. It classifies nothing itself;
// CheckLog reads the triple and decides success from mismatch.
func (c *Client) doRawLog(ctx context.Context, rawURL string) (int, string, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, "", nil, fmt.Errorf("forgejo GET check log: build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", nil, fmt.Errorf("forgejo GET check log: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", nil, fmt.Errorf("forgejo GET check log: read body: %w", err)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return resp.StatusCode, mediaType, body, nil
}

// statusError is a non-2xx forge answer: the diagnostic line do() always
// produced (method, path, status, body snippet — never the token) plus the
// machine-readable status. 404 unwraps to tracker.ErrNotFound — the one
// upstream status that means "no such subject" rather than "forge failure" —
// so callers answer not-found instead of bad-gateway (Forgejo also 404s repos
// the token cannot see; that is still "not found" to us). EnsureLabel reads
// the status via errors.As to recognize a duplicate-name conflict.
type statusError struct {
	status  int
	message string
}

func (e *statusError) Error() string { return e.message }

func (e *statusError) Unwrap() error {
	if e.status == http.StatusNotFound {
		return tracker.ErrNotFound
	}
	return nil
}

// bodySnippet reads a bounded, whitespace-collapsed snippet of an error
// response body for diagnostics.
func bodySnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, errBodySnippetMax))
	s := strings.Join(strings.Fields(string(b)), " ")
	if s == "" {
		return "(empty body)"
	}
	return s
}
