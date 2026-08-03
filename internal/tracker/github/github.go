// Package github is the GitHub REST implementation of the tracker.Tracker
// seam (ADR-0015). It is a structural twin of internal/tracker/forgejo — same
// one-do()-transport shape, same tracker-contract semantics that must survive
// (the ready queue is open issues carrying the ready-for-agent label; pulls
// are listed as the bounded open + recent-closed view (issue #176) with the
// three-valued open|merged|closed state, the done-signal reading PullsForHead;
// a call either yields the full result or an error carrying operation context,
// never partial data and never the forge token) — differing only where
// GitHub's API differs from Forgejo's:
//
//   - Auth is "Authorization: Bearer <token>" (not "token <token>"), plus the
//     pinned X-GitHub-Api-Version and the vnd.github+json Accept.
//   - GET /issues folds PRs into the list, so issues are filtered client-side
//     on the pull_request field (an issue that is really a PR carries one).
//   - Pull-request merged state is derived from merged_at != null (the list
//     endpoint has no `merged` boolean).
//   - Labels are addressed by NAME on the wire, but a create/attach still
//     resolves names against the repo's label set FIRST: GitHub auto-creates
//     unknown labels on some write paths, so strict pre-resolution is what
//     keeps a typo a loud ErrUnknownLabel instead of a permanent garbage
//     label (ADR-0014 parity).
//   - Pagination follows the Link rel="next" header (per_page=100), with the
//     same maxPages loud-truncation guard as the Forgejo client.
//   - A rate-limited 403/429 (X-RateLimit-Remaining: 0 or Retry-After) unwraps
//     to tracker.ErrRateLimited; a 404 unwraps to tracker.ErrNotFound.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

const (
	// defaultRequestTimeout bounds every REST call — applied per request as a
	// child context so pagination and the two-call Issue(n) read each get a
	// fresh budget (forgejo parity).
	defaultRequestTimeout = 30 * time.Second

	// pageLimit is the page size for list endpoints. GitHub caps per_page at
	// 100; we ask for the max and follow Link rel="next" until it is absent.
	pageLimit = 100

	// maxPages bounds fetchPages. A listing whose Link header never drops
	// rel="next" (a bug, or an endpoint that ignores pagination) aborts with an
	// explicit error instead of silently truncating the done-signal or looping
	// forever.
	maxPages = 100

	// errBodySnippetMax caps how much of a non-2xx response body is folded into
	// the error, keeping it a short diagnostic line.
	errBodySnippetMax = 512

	// apiVersion is the pinned GitHub REST API version header value.
	apiVersion = "2022-11-28"
)

// Client is a GitHub REST client scoped to a single {owner}/{repo}. It
// implements tracker.Tracker.
type Client struct {
	httpClient *http.Client
	baseURL    string // API origin, e.g. https://api.github.com or a GHE root https://ghe.example.com/api/v3 (no trailing slash)
	token      string // forge_token; sent as "Authorization: Bearer <token>", never logged or surfaced in errors
	owner      string
	repo       string
	timeout    time.Duration
}

var _ tracker.Tracker = (*Client)(nil)

// New builds a GitHub REST client. baseURL is the API origin (api.github.com
// for github.com; a GHE instance's real API root otherwise), token is the
// repo's vault-decrypted forge_token, owner/repo come from tracker.RepoPath. A
// nil httpClient defaults to a plain http.Client (call-level context deadlines
// do the timing).
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

// --- GitHub JSON shapes ----------------------------------------------------

type ghLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"` // 6 hex, no leading '#'
	Description string `json:"description"`
}

// ghPullMarker's mere presence on a /issues entry marks it as a pull request,
// which GitHub folds into the issues list and which must be filtered out.
type ghPullMarker struct {
	URL string `json:"url"`
}

type ghIssue struct {
	Number      int           `json:"number"`
	Title       string        `json:"title"`
	Body        string        `json:"body"`
	State       string        `json:"state"`
	Labels      []ghLabel     `json:"labels"`
	Comments    int           `json:"comments"` // GitHub's comment count — the list views' CommentsCount
	PullRequest *ghPullMarker `json:"pull_request"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

func (i ghIssue) isPull() bool { return i.PullRequest != nil }

type ghUser struct {
	Login string `json:"login"`
}

type ghComment struct {
	Body      string    `json:"body"`
	User      ghUser    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// ghReview is one row of the pull-reviews list: the reviewer under `user` and
// the review state (APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, or
// PENDING). GitHub has no `dismissed` boolean — a dismissed review is reported
// with state DISMISSED, which toReview maps to Dismissed=true.
type ghReview struct {
	User  ghUser `json:"user"`
	State string `json:"state"`
	Body  string `json:"body"`
}

type ghPullHead struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

type ghPull struct {
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	State    string     `json:"state"`
	MergedAt *time.Time `json:"merged_at"` // non-nil ⇒ merged (the list endpoint has no `merged` bool)
	Head     ghPullHead `json:"head"`
	Base     ghPullHead `json:"base"` // PullsForHead re-filters on it (issue #176)
	HTMLURL  string     `json:"html_url"`
}

// ghCheckRun is one row from the Checks API
// (commits/{sha}/check-runs) — a GitHub Actions workflow job, or any other
// GitHub App's check run, against a commit. This is the ONLY place Actions
// results appear: the legacy combined-status API below cannot see them.
type ghCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued|in_progress|completed|... — not "completed" ⇒ CheckPending
	Conclusion string `json:"conclusion"` // set only once status=="completed"
	HTMLURL    string `json:"html_url"`
	Output     struct {
		Title string `json:"title"`
	} `json:"output"`
}

// ghCheckRunsPage is one page of the Checks API response: the rows live
// nested under check_runs, not a bare JSON array — fetchPages assumes a bare
// array, so this endpoint goes through fetchNested instead.
type ghCheckRunsPage struct {
	TotalCount int          `json:"total_count"`
	CheckRuns  []ghCheckRun `json:"check_runs"`
}

// ghStatus is one row from the legacy combined-status API
// (commits/{sha}/status) — external CI services (many predating GitHub
// Actions/Checks) report ONLY here, never as a check run.
type ghStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// ghStatusPage is one page of the legacy combined-status API response: the
// rows live nested under statuses. State is decoded but deliberately never
// read — see Checks' doc comment for why lab computes its own aggregate
// instead of trusting GitHub's combined verdict.
type ghStatusPage struct {
	State    string     `json:"state"`
	Statuses []ghStatus `json:"statuses"`
}

// --- Tracker methods -------------------------------------------------------

// ReadyIssues returns the ready queue: open issues carrying the
// ready-for-agent label. state and labels are filtered server-side, but
// membership is re-checked client-side for parity with the Forgejo client
// (and PRs folded into /issues are dropped). No comments are loaded.
func (c *Client) ReadyIssues(ctx context.Context) ([]tracker.Issue, error) {
	q := url.Values{}
	q.Set("state", tracker.StateOpen)
	q.Set("labels", tracker.ReadyLabel)
	ghs, err := fetchPages[ghIssue](ctx, c, c.issuesPath(), q)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Issue, 0, len(ghs))
	for _, gh := range ghs {
		if gh.isPull() || !ghHasLabel(gh, tracker.ReadyLabel) {
			continue
		}
		out = append(out, toIssue(gh, nil))
	}
	return out, nil
}

// ghHasLabel reports whether the decoded issue carries a label named name
// (exact match — the tracker vocabulary treats label names as identifiers).
func ghHasLabel(gh ghIssue, name string) bool {
	for _, l := range gh.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// Issues returns the issues in the given state (open|closed|all) for the
// operator list view, with bounded reads (issue #176): the OPEN view walks
// the full open set (the working set caps it), while the closed view is ONE
// request for the tracker.RecentClosedWindow most recently updated closed
// issues via recentClosedWindow (GitHub has no recently-closed sort;
// updated-desc approximates it — see the helper). The all view is the open
// walk plus the closed window, open first (disjoint states — no dedupe). Any
// other filter is a loud error: the param now fans out into distinct reads,
// so an unrecognized value must not silently misquery. PRs folded into
// GitHub's /issues are dropped from BOTH sets — so the closed window carries
// up to the window of rows, fewer after the PR filtering — and no comments
// are loaded.
func (c *Client) Issues(ctx context.Context, state string) ([]tracker.Issue, error) {
	switch state {
	case tracker.StateOpen, tracker.StateClosed, tracker.StateAll:
	default:
		return nil, fmt.Errorf("github issues: invalid state filter %q", state)
	}
	var ghs []ghIssue
	if state != tracker.StateClosed {
		q := url.Values{}
		q.Set("state", tracker.StateOpen)
		open, err := fetchPages[ghIssue](ctx, c, c.issuesPath(), q)
		if err != nil {
			return nil, err
		}
		ghs = append(ghs, open...)
	}
	if state != tracker.StateOpen {
		closed, err := recentClosedWindow[ghIssue](ctx, c, c.issuesPath())
		if err != nil {
			return nil, err
		}
		ghs = append(ghs, closed...)
	}
	out := make([]tracker.Issue, 0, len(ghs))
	for _, gh := range ghs {
		if gh.isPull() {
			continue
		}
		out = append(out, toIssue(gh, nil))
	}
	return out, nil
}

// Issue returns a single issue fully populated with its comments (the issue,
// then its comments). GitHub's comments endpoint paginates like every list, so
// it goes through fetchPages (the Forgejo comment quirk — an un-paginated
// endpoint — does not apply here).
func (c *Client) Issue(ctx context.Context, number int) (tracker.Issue, error) {
	var gh ghIssue
	if _, err := c.do(ctx, http.MethodGet, c.issuePath(number), nil, nil, &gh); err != nil {
		return tracker.Issue{}, err
	}
	gcs, err := fetchPages[ghComment](ctx, c, c.issuePath(number)+"/comments", nil)
	if err != nil {
		return tracker.Issue{}, err
	}
	comments := make([]tracker.Comment, 0, len(gcs))
	for _, gc := range gcs {
		comments = append(comments, toComment(gc))
	}
	return toIssue(gh, comments), nil
}

// CreateComment posts a comment body on an issue.
func (c *Client) CreateComment(ctx context.Context, number int, body string) error {
	req := struct {
		Body string `json:"body"`
	}{Body: body}
	_, err := c.do(ctx, http.MethodPost, c.issuePath(number)+"/comments", nil, req, nil)
	return err
}

// Pulls returns the repo's OPEN pulls plus the tracker.RecentClosedWindow
// most recently closed ones (merged or not), open set first — the bounded
// list view of issue #176 (the Forgejo sibling documents the O(history) cost
// the old state=all walk paid). The open walk keeps Link-header pagination
// (open counts can legitimately exceed one page); the closed window is
// exactly ONE request via recentClosedWindow, which deliberately ignores any
// Link header. The done-signal read that once justified state=all lives on
// PullsForHead now; the three-valued state is still derived from
// state+merged_at per row, so merged pulls in the window map to
// tracker.PullMerged.
func (c *Client) Pulls(ctx context.Context) ([]tracker.PullRef, error) {
	q := url.Values{}
	q.Set("state", tracker.StateOpen)
	ghs, err := fetchPages[ghPull](ctx, c, c.pullsPath(), q)
	if err != nil {
		return nil, err
	}
	closed, err := recentClosedWindow[ghPull](ctx, c, c.pullsPath())
	if err != nil {
		return nil, err
	}
	ghs = append(ghs, closed...)
	out := make([]tracker.PullRef, 0, len(ghs))
	for _, gh := range ghs {
		out = append(out, toPullRef(gh))
	}
	return out, nil
}

// PullsForHead lists the pulls whose head branch is head AND base branch is
// base, across all states, in a bounded number of requests (issue #176) —
// the reaper's per-branch done-signal read, replacing the O(history) Pulls
// walk. GitHub's list endpoint filters server-side, so this is the plain
// pulls listing with head/base/state params: the head filter REQUIRES the
// owner: prefix (head=<owner>:<branch> — a bare branch name is silently
// IGNORED and the listing degenerates into the very unbounded walk this
// method exists to avoid; this is the syntax difference vs Forgejo's
// by-base-head path lookup). The response is re-filtered client-side on
// head+base defensively — a filter the server silently dropped must not
// leak foreign pulls into a done-signal — and pagination follows the Link
// header like every list (in practice one page: a single branch pair rarely
// accumulates 100 pulls). HeadBranch is normalized to the QUERIED name for
// seam uniformity (the pin every backend shares); on GitHub that is a no-op
// in practice, since it preserves the branch label on merged pulls. No
// match is an empty non-nil slice, success.
func (c *Client) PullsForHead(ctx context.Context, head, base string) ([]tracker.PullRef, error) {
	q := url.Values{}
	q.Set("state", tracker.StateAll)
	q.Set("head", c.owner+":"+head)
	q.Set("base", base)
	ghs, err := fetchPages[ghPull](ctx, c, c.pullsPath(), q)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.PullRef, 0, len(ghs))
	for _, gh := range ghs {
		if gh.Head.Ref != head || gh.Base.Ref != base {
			continue
		}
		ref := toPullRef(gh)
		ref.HeadBranch = head // the queried name — the seam's uniformity pin
		out = append(out, ref)
	}
	return out, nil
}

// Pull returns one pull request in full detail, body included — the read
// behind labctl pr view. An unknown number is GitHub's 404 → tracker.ErrNotFound;
// a throttled call unwraps to tracker.ErrRateLimited like every other read.
func (c *Client) Pull(ctx context.Context, number int) (tracker.PullDetail, error) {
	var gh ghPull
	if _, err := c.do(ctx, http.MethodGet, c.pullPath(number), nil, nil, &gh); err != nil {
		return tracker.PullDetail{}, err
	}
	return toPullDetail(gh), nil
}

// Checks returns the CI check/status rows for pull `number`'s CURRENT head
// commit. It resolves the head SHA AT QUERY TIME — a fresh GET of the pull
// (the same endpoint/decode Pull uses), so a push or rebase that landed
// moments before the read is reflected, never a memoized commit — then
// queries BOTH of GitHub's independent CI-reporting surfaces for that SHA and
// unions their rows, check-runs first then statuses:
//
//   - the Checks API (commits/{sha}/check-runs): GitHub Actions workflow jobs
//     — and any other GitHub App's check runs — report ONLY here; the legacy
//     status API below cannot see them.
//   - the legacy combined-status API (commits/{sha}/status): external CI
//     services (many predate Checks) report ONLY here, never as a check run.
//
// A repo wired to both (Actions plus an external service) is unremarkable,
// so both are always queried, unconditionally. The union performs NO
// deduplication across the two APIs: a system that reports to both for what
// a human would call "the same check" yields two rows in the result.
// Collapsing them would require guessing at name equivalence across two
// different vocabularies (a check run's `name` vs a status's `context`) —
// exactly the kind of implicit magic this package avoids elsewhere (ADR-0014's
// strict label-name resolution, the verbatim-error pattern) — so the union is
// left for the caller/agent to read as-is; that is the pinned decision, not
// an oversight. The combined-status endpoint's own top-level `state` verdict
// is decoded (ghStatusPage) but deliberately never consulted: aggregation is
// tracker.ChecksState's job, computed client-side from the union this method
// returns, never trusted from a forge's own combined badge (checks.go) — the
// same "reports pending with nothing really pending" trap the Forgejo sibling
// documents. An unknown pull number fails on the first GET (the pull lookup)
// exactly like Pull: the statusError 404 path unwraps to tracker.ErrNotFound.
// Both CI endpoints paginate like every other list on this client
// (per_page/Link rel="next"). Zero rows from both is success, not an error: a
// non-nil empty slice.
func (c *Client) Checks(ctx context.Context, number int) ([]tracker.Check, error) {
	var gh ghPull
	if _, err := c.do(ctx, http.MethodGet, c.pullPath(number), nil, nil, &gh); err != nil {
		return nil, err // 404 → tracker.ErrNotFound, same as Pull
	}
	sha := gh.Head.Sha

	runs, err := fetchNested(ctx, c, c.checkRunsPath(sha), func(p ghCheckRunsPage) []ghCheckRun { return p.CheckRuns })
	if err != nil {
		return nil, err
	}
	statuses, err := fetchNested(ctx, c, c.statusPath(sha), func(p ghStatusPage) []ghStatus { return p.Statuses })
	if err != nil {
		return nil, err
	}

	out := make([]tracker.Check, 0, len(runs)+len(statuses))
	for _, r := range runs {
		out = append(out, toCheckFromRun(r))
	}
	for _, s := range statuses {
		out = append(out, toCheckFromStatus(s))
	}
	return out, nil
}

// CheckLog is the deferred job-log surface on the GitHub backend: proxying
// GitHub Actions job logs is a server-credentialed read this slice does not
// implement yet (ADR-0032's deferred logs land first on the Forgejo binding),
// so rather than fake output it wraps tracker.ErrUnsupported naming the verb —
// the built-in tracker's refusal style — distinct from a forge failure.
func (c *Client) CheckLog(ctx context.Context, number int, name string) (tracker.CheckLogResult, error) {
	return tracker.CheckLogResult{}, fmt.Errorf("%w: check logs on the GitHub backend", tracker.ErrUnsupported)
}

// CreatePull opens a pull request from head into base and returns the created
// PullRef. (The agent API injects/validates Closes #N and applies incogni
// sanitization before this call.)
func (c *Client) CreatePull(ctx context.Context, head, base, title, body string) (tracker.PullRef, error) {
	req := struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}{Head: head, Base: base, Title: title, Body: body}
	var gh ghPull
	if _, err := c.do(ctx, http.MethodPost, c.pullsPath(), nil, req, &gh); err != nil {
		return tracker.PullRef{}, err
	}
	return toPullRef(gh), nil
}

// MergePull merges pull request `number` on GitHub and returns its merged
// PullRef. GitHub — not lab — decides mergeability: MergePull does NOT
// pre-check required status checks or branch protection. It first GETs the
// pull so an already-merged one is a convergent no-op success, then PUTs the
// fixed "merge" method (a merge commit; GitHub enforces its own configured
// allowed methods and branch protection). A refusal — a required check
// unsatisfied (405), a protected base, a changed head (409) — is GitHub's own
// non-2xx answer, wrapped in tracker.ErrMergeRejected with that answer's body
// verbatim; an unknown number stays tracker.ErrNotFound. The merge is
// authorized by this client's server-side forge token, so no forge credential
// ever reaches the agent session (ADR-0014). The head branch is not deleted.
func (c *Client) MergePull(ctx context.Context, number int) (tracker.PullRef, error) {
	var gh ghPull
	if _, err := c.do(ctx, http.MethodGet, c.pullPath(number), nil, nil, &gh); err != nil {
		return tracker.PullRef{}, err // 404 → tracker.ErrNotFound
	}
	if derivePullState(gh.State, gh.MergedAt != nil) == tracker.PullMerged {
		return toPullRef(gh), nil // convergent no-op: already merged
	}
	req := struct {
		MergeMethod string `json:"merge_method"`
	}{MergeMethod: "merge"}
	if _, err := c.do(ctx, http.MethodPut, c.pullPath(number)+"/merge", nil, req, nil); err != nil {
		// A 404 (unknown PR) and a throttle (GitHub's secondary limits target
		// mutating requests) are typed upstream conditions, not merge refusals
		// — surface them as-is so the agent gets a 404 / a retryable upstream
		// error, never a permanent, retry-proof "merge refused" 409.
		if errors.Is(err, tracker.ErrNotFound) || errors.Is(err, tracker.ErrRateLimited) {
			return tracker.PullRef{}, err
		}
		return tracker.PullRef{}, fmt.Errorf("%w: %s", tracker.ErrMergeRejected, err.Error())
	}
	// Merged. The head ref and web URL do not change on merge; report merged.
	return tracker.PullRef{
		Number:     gh.Number,
		HeadBranch: gh.Head.Ref,
		State:      tracker.PullMerged,
		URL:        gh.HTMLURL,
	}, nil
}

// Reviews lists the submitted reviews on pull `number`, oldest first — the read
// behind the human half of the autoland loop's hybrid rejected-state
// (ADR-0048). Each review's user login is the Reviewer,
// its state normalized onto lab's Review* vocabulary (a DISMISSED review sets
// Dismissed=true; an unrecognized state passes through lowercased). The endpoint
// paginates like every list (per_page/Link rel="next"). An unknown number's 404
// unwraps to tracker.ErrNotFound; a throttled call to tracker.ErrRateLimited.
func (c *Client) Reviews(ctx context.Context, number int) ([]tracker.Review, error) {
	ghs, err := fetchPages[ghReview](ctx, c, c.pullPath(number)+"/reviews", nil)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Review, 0, len(ghs))
	for _, gh := range ghs {
		out = append(out, toReview(gh))
	}
	return out, nil
}

// RerequestReview re-requests review from every reviewer whose LATEST
// verdict-bearing, non-dismissed review requests changes. It GETs the reviews,
// reduces them to the latest verdict per reviewer (submission order, last
// verdict wins; only APPROVED/CHANGES_REQUESTED rows carry a verdict —
// dismissed reviews and non-verdict rows like COMMENTED or PENDING are
// skipped), and if any reviewer's latest verdict is changes-requested POSTs
// their logins to /requested_reviewers. No such reviewer is a convergent no-op
// success, mirroring MergePull's already-merged no-op. Accepted consequence: a
// successful re-request does not change the reviewer's latest verdict (they
// ARE still changes-requested until they re-review), so a repeated call
// re-POSTs the same reviewers instead of no-opping — faithful, and the forge
// handles a duplicate request idempotently. A GitHub refusal wraps
// tracker.ErrReviewRejected with its own words; an unknown number stays
// tracker.ErrNotFound and a throttle tracker.ErrRateLimited.
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
	if _, err := c.do(ctx, http.MethodPost, c.pullPath(number)+"/requested_reviewers", nil, req, nil); err != nil {
		if errors.Is(err, tracker.ErrNotFound) || errors.Is(err, tracker.ErrRateLimited) {
			return err
		}
		return fmt.Errorf("%w: %s", tracker.ErrReviewRejected, err.Error())
	}
	return nil
}

// CommentPull posts a plain discussion comment on pull `number`. A PR shares the
// issue-comment number space on GitHub, so this posts to the same issue-comment
// endpoint CreateComment uses. An unknown number stays tracker.ErrNotFound; a
// throttle tracker.ErrRateLimited.
func (c *Client) CommentPull(ctx context.Context, number int, body string) error {
	req := struct {
		Body string `json:"body"`
	}{Body: body}
	_, err := c.do(ctx, http.MethodPost, c.issuePath(number)+"/comments", nil, req, nil)
	return err
}

// PullComments lists pull `number`'s discussion comments, oldest first — the
// READ counterpart of CommentPull, the autoland poller's route onto verdict
// markers (ADR-0048). A PR shares the issue-comment number space on GitHub,
// so this reads and maps the SAME endpoint Issue's comment thread does; the
// endpoint paginates like every list on this client (per_page/Link rel="next"),
// so it goes through fetchPages exactly as Issue does. An unknown number's 404
// unwraps to tracker.ErrNotFound; a throttled call to tracker.ErrRateLimited.
func (c *Client) PullComments(ctx context.Context, number int) ([]tracker.Comment, error) {
	gcs, err := fetchPages[ghComment](ctx, c, c.issuePath(number)+"/comments", nil)
	if err != nil {
		return nil, err
	}
	comments := make([]tracker.Comment, 0, len(gcs))
	for _, gc := range gcs {
		comments = append(comments, toComment(gc))
	}
	return comments, nil
}

// CloseIssue closes an issue via PATCH state=closed.
func (c *Client) CloseIssue(ctx context.Context, number int) error {
	req := struct {
		State string `json:"state"`
	}{State: "closed"}
	_, err := c.do(ctx, http.MethodPatch, c.issuePath(number), nil, req, nil)
	return err
}

// defaultLabelColor is the swatch EnsureLabel supplies when the caller omits
// one — the same value as the builtin binding's store default and the Forgejo
// client's, so a label ensured on any binding looks the same. GitHub requires
// a color on create.
const defaultLabelColor = "#6b7280"

// CreateIssue opens an issue with the named labels attached at creation. Names
// are resolved against the repo's label set FIRST: a name the repo does not
// define is tracker.ErrUnknownLabel before anything is created — GitHub would
// otherwise silently create the label on some write paths, turning a typo into
// a permanent garbage label (ADR-0014). GitHub addresses labels by name, so
// the resolved names go on the wire (the resolution is the guard, not an
// id translation).
func (c *Client) CreateIssue(ctx context.Context, title, body string, labels []string) (tracker.Issue, error) {
	names, err := c.resolveLabelNames(ctx, labels)
	if err != nil {
		return tracker.Issue{}, err
	}
	req := struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels,omitempty"`
	}{Title: title, Body: body, Labels: names}
	var gh ghIssue
	if _, err := c.do(ctx, http.MethodPost, c.issuesPath(), nil, req, &gh); err != nil {
		return tracker.Issue{}, err
	}
	return toIssue(gh, nil), nil
}

// EditIssue applies a title/body patch via PATCH /issues/{n} (GitHub's issue
// edit endpoint). Only the set fields go on the wire: *string with omitempty
// omits a nil pointer but keeps a non-nil pointer to "" (a cleared body), so a
// title-only edit sends no body key and a body-only edit no title key. GitHub
// judges nothing about emptiness — title-non-empty is the API layer's guard. A
// 404 unwraps to tracker.ErrNotFound and a throttled 403/429 to
// tracker.ErrRateLimited, like every other call on this client. The response
// maps through toIssue in LIST shape (no comments), matching CreateIssue.
func (c *Client) EditIssue(ctx context.Context, number int, edit tracker.IssueEdit) (tracker.Issue, error) {
	req := struct {
		Title *string `json:"title,omitempty"`
		Body  *string `json:"body,omitempty"`
	}{Title: edit.Title, Body: edit.Body}
	var gh ghIssue
	if _, err := c.do(ctx, http.MethodPatch, c.issuePath(number), nil, req, &gh); err != nil {
		return tracker.Issue{}, err
	}
	return toIssue(gh, nil), nil
}

// AddIssueLabels attaches the named labels to an issue in one POST, after the
// same strict client-side name resolution as CreateIssue.
func (c *Client) AddIssueLabels(ctx context.Context, number int, labels []string) error {
	names, err := c.resolveLabelNames(ctx, labels)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	req := struct {
		Labels []string `json:"labels"`
	}{Labels: names}
	_, err = c.do(ctx, http.MethodPost, c.issuePath(number)+"/labels", nil, req, nil)
	return err
}

// RemoveIssueLabels detaches the named labels from an issue — one DELETE per
// label, the only remove GitHub's API offers — after strict name resolution of
// the whole set, so a typo fails before the first detach. The label name is
// path-escaped (names may contain slashes, e.g. kind/feature).
func (c *Client) RemoveIssueLabels(ctx context.Context, number int, labels []string) error {
	names, err := c.resolveLabelNames(ctx, labels)
	if err != nil {
		return err
	}
	for _, name := range names {
		path := c.issuePath(number) + "/labels/" + url.PathEscape(name)
		if _, err := c.do(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// Labels lists the repo's labels ordered by name (the contract order; the
// forge's own order is unspecified).
func (c *Client) Labels(ctx context.Context) ([]tracker.Label, error) {
	ghs, err := c.repoLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.Label, 0, len(ghs))
	for _, gh := range ghs {
		out = append(out, toLabel(gh))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// EnsureLabel returns the repo's label of that name, creating it when absent.
// List-first keeps the op idempotent; a create that still answers a duplicate
// conflict (a concurrent ensure won the race) resolves by re-listing. An
// existing label's color/description are never overwritten.
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
	}{Name: name, Color: strings.TrimPrefix(color, "#"), Description: description}
	var created ghLabel
	_, err := c.do(ctx, http.MethodPost, c.labelsPath(), nil, req, &created)
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
	ghs, err := c.repoLabels(ctx)
	if err != nil {
		return tracker.Label{}, false, err
	}
	for _, gh := range ghs {
		if gh.Name == name {
			return toLabel(gh), true, nil
		}
	}
	return tracker.Label{}, false, nil
}

// repoLabels fetches the repo's full label set (paginated like every list).
func (c *Client) repoLabels(ctx context.Context) ([]ghLabel, error) {
	return fetchPages[ghLabel](ctx, c, c.labelsPath(), nil)
}

// resolveLabelNames verifies each name against the repo's label set,
// deduplicated and order-preserving, failing on the first name the repo does
// not define (tracker.ErrUnknownLabel, carrying the name). GitHub addresses
// labels by name, so the verified names are what goes on the wire; strict
// resolution is purely the loud-failure guard against GitHub's silent
// label-create. Nil in, nil out.
func (c *Client) resolveLabelNames(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	ghs, err := c.repoLabels(ctx)
	if err != nil {
		return nil, err
	}
	defined := make(map[string]struct{}, len(ghs))
	for _, gh := range ghs {
		defined[gh.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := defined[name]; !ok {
			return nil, fmt.Errorf("%w %q", tracker.ErrUnknownLabel, name)
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// --- path helpers ----------------------------------------------------------

// repoPath builds /repos/{owner}/{repo}{suffix} with owner and repo
// path-escaped: both come from the operator-supplied remote URL, so reserved
// characters must target the literal path instead of being reparsed as query,
// fragment, or extra segments when do() builds the request URL (forgejo
// parity).
func (c *Client) repoPath(suffix string) string {
	return "/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.repo) + suffix
}

func (c *Client) issuesPath() string     { return c.repoPath("/issues") }
func (c *Client) pullsPath() string      { return c.repoPath("/pulls") }
func (c *Client) labelsPath() string     { return c.repoPath("/labels") }
func (c *Client) issuePath(n int) string { return c.repoPath("/issues/" + strconv.Itoa(n)) }
func (c *Client) pullPath(n int) string  { return c.repoPath("/pulls/" + strconv.Itoa(n)) }

// checkRunsPath and statusPath are Checks' two CI-reporting endpoints for one
// commit SHA. The SHA comes from GitHub's own Pull response rather than
// operator input, but it is escaped like every other path segment built from
// server data.
func (c *Client) checkRunsPath(sha string) string {
	return c.repoPath("/commits/" + url.PathEscape(sha) + "/check-runs")
}

func (c *Client) statusPath(sha string) string {
	return c.repoPath("/commits/" + url.PathEscape(sha) + "/status")
}

// --- mapping ---------------------------------------------------------------

// derivePullState collapses GitHub's (state, merged) into the three-valued
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

func toIssue(gh ghIssue, comments []tracker.Comment) tracker.Issue {
	labels := make([]string, len(gh.Labels))
	for i, l := range gh.Labels {
		labels[i] = l.Name
	}
	return tracker.Issue{
		Number:        gh.Number,
		Title:         gh.Title,
		Body:          gh.Body,
		State:         gh.State,
		Labels:        labels,
		Comments:      comments,
		CommentsCount: gh.Comments,
		CreatedAt:     gh.CreatedAt,
		UpdatedAt:     gh.UpdatedAt,
	}
}

// toLabel maps a GitHub label onto the tracker vocabulary. GitHub stores
// colors WITHOUT the leading '#' ("6b7280"); lab's vocabulary carries it, so
// reads are normalized.
func toLabel(gh ghLabel) tracker.Label {
	color := gh.Color
	if color != "" && !strings.HasPrefix(color, "#") {
		color = "#" + color
	}
	return tracker.Label{Name: gh.Name, Color: color, Description: gh.Description}
}

func toComment(gc ghComment) tracker.Comment {
	return tracker.Comment{
		Author:    gc.User.Login,
		Body:      gc.Body,
		CreatedAt: gc.CreatedAt,
	}
}

// toReview maps one GitHub review row onto lab's Review vocabulary: the reviewer
// login, the state normalized onto the Review* vocabulary, the body, and the
// dismissed flag derived from GitHub's DISMISSED state.
func toReview(gh ghReview) tracker.Review {
	state, dismissed := mapReviewState(gh.State)
	return tracker.Review{
		Reviewer:  gh.User.Login,
		State:     state,
		Body:      gh.Body,
		Dismissed: dismissed,
	}
}

// mapReviewState normalizes a GitHub review state onto lab's Review* vocabulary
// and reports whether the review is dismissed. GitHub's states are APPROVED,
// CHANGES_REQUESTED, COMMENTED, DISMISSED, and PENDING. A DISMISSED review is
// reported with Dismissed=true and its state passed through lowercased
// ("dismissed") — GitHub collapses the original verdict into the DISMISSED
// state, so there is no pre-dismissal word to recover — while every other state
// maps onto the constants or passes through lowercased (PENDING → "pending", or
// any word this package does not know). GitHub has no REQUEST_REVIEW review
// state (a requested reviewer is not a submitted review), so lab's
// ReviewRequested never originates here.
func mapReviewState(state string) (string, bool) {
	switch state {
	case "APPROVED":
		return tracker.ReviewApproved, false
	case "CHANGES_REQUESTED":
		return tracker.ReviewChangesRequested, false
	case "COMMENTED":
		return tracker.ReviewCommented, false
	case "DISMISSED":
		return strings.ToLower(state), true
	default:
		return strings.ToLower(state), false
	}
}

// changesRequestedReviewers returns the reviewer logins whose LATEST
// verdict-bearing, non-dismissed review requests changes, in first-seen order.
// Reviews arrive oldest-first, so the last VERDICT seen per reviewer is their
// latest verdict: only approved/changes-requested rows carry one — GitHub's
// review decision is likewise unaffected by COMMENTED reviews — so a later
// comment row neither sets nor clears a reviewer's verdict (a reviewer who
// requested changes and then replied in the thread is STILL requesting
// changes). A dismissed review does not count (GitHub cleared it); a reviewer
// whose latest verdict is an approval drops out of the set. Operates on the
// already-normalized tracker.Review vocabulary (Reviews() maps it): dismissed
// is the DISMISSED-derived flag, changes-requested is
// tracker.ReviewChangesRequested.
func changesRequestedReviewers(reviews []tracker.Review) []string {
	latest := make(map[string]string, len(reviews))
	order := make([]string, 0, len(reviews))
	for _, r := range reviews {
		if r.Reviewer == "" || r.Dismissed {
			continue
		}
		if r.State != tracker.ReviewApproved && r.State != tracker.ReviewChangesRequested {
			continue // non-verdict row: commented/pending/unknown
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

func toPullRef(gh ghPull) tracker.PullRef {
	return tracker.PullRef{
		Number:     gh.Number,
		HeadBranch: gh.Head.Ref,
		State:      derivePullState(gh.State, gh.MergedAt != nil),
		URL:        gh.HTMLURL,
	}
}

func toPullDetail(gh ghPull) tracker.PullDetail {
	return tracker.PullDetail{
		Number:     gh.Number,
		Title:      gh.Title,
		Body:       gh.Body,
		State:      derivePullState(gh.State, gh.MergedAt != nil),
		HeadBranch: gh.Head.Ref,
		URL:        gh.HTMLURL,
	}
}

// toCheckFromRun maps one Checks API row onto lab's three-word vocabulary.
// Mapping table:
//
//	status != "completed" (queued, in_progress, or any other in-flight word) → CheckPending, RawState = the status word
//	status == "completed": conclusion decides, RawState = the conclusion word
//	  success, neutral, skipped                        → CheckSuccess
//	  cancelled, timed_out, action_required, failure,
//	  and anything unrecognized                         → CheckFailure
//
// RawState always carries GitHub's own word verbatim — the status word while
// in flight, the conclusion word once completed — never lab's collapsed
// State.
func toCheckFromRun(r ghCheckRun) tracker.Check {
	state, raw := tracker.CheckPending, r.Status
	if r.Status == "completed" {
		raw = r.Conclusion
		switch r.Conclusion {
		case "success", "neutral", "skipped":
			state = tracker.CheckSuccess
		default: // cancelled, timed_out, action_required, failure, or unrecognized
			state = tracker.CheckFailure
		}
	}
	return tracker.Check{
		Name:     r.Name,
		State:    state,
		RawState: raw,
		Summary:  r.Output.Title,
		URL:      r.HTMLURL,
	}
}

// toCheckFromStatus maps one legacy combined-status row onto lab's
// vocabulary: success → CheckSuccess, pending → CheckPending, everything
// else (error, failure, or an unrecognized word) → CheckFailure. RawState
// carries the state word verbatim.
func toCheckFromStatus(s ghStatus) tracker.Check {
	var state string
	switch s.State {
	case "success":
		state = tracker.CheckSuccess
	case "pending":
		state = tracker.CheckPending
	default: // error, failure, or unrecognized
		state = tracker.CheckFailure
	}
	return tracker.Check{
		Name:     s.Context,
		State:    state,
		RawState: s.State,
		Summary:  s.Description,
		URL:      s.TargetURL,
	}
}

// --- transport -------------------------------------------------------------

// fetchPages walks a paginated list endpoint (per_page=pageLimit) following
// the GitHub Link rel="next" header until it is absent, concatenating the
// decoded elements. Terminating on the absent next-link — GitHub's own
// pagination signal — costs no empty-page probe. maxPages bounds a pathological
// or non-paginating endpoint with an explicit error instead of silent
// truncation or an infinite loop. May return nil on zero results (callers
// re-slice through make()).
func fetchPages[T any](ctx context.Context, c *Client, path string, base url.Values) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("github GET %s: listing exceeds %d pages; refusing to silently truncate", path, maxPages)
		}
		q := url.Values{}
		for k, vs := range base {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("per_page", strconv.Itoa(pageLimit))
		q.Set("page", strconv.Itoa(page))
		var chunk []T
		hdr, err := c.do(ctx, http.MethodGet, path, q, nil, &chunk)
		if err != nil {
			return nil, err
		}
		all = append(all, chunk...)
		if !hasNextLink(hdr.Get("Link")) {
			return all, nil
		}
	}
}

// fetchNested walks a paginated GitHub endpoint whose response body is a JSON
// OBJECT with the page's rows nested under one field (the Checks API's
// check_runs, the legacy status API's statuses) — unlike fetchPages, which
// assumes a bare JSON array and so cannot decode either of these endpoints.
// It otherwise follows the exact same convention: per_page=pageLimit, walk
// while the Link header advertises rel="next", with the same maxPages
// loud-truncation guard as fetchPages against a pathological or
// non-paginating endpoint.
func fetchNested[P any, T any](ctx context.Context, c *Client, path string, rows func(P) []T) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("github GET %s: listing exceeds %d pages; refusing to silently truncate", path, maxPages)
		}
		q := url.Values{}
		q.Set("per_page", strconv.Itoa(pageLimit))
		q.Set("page", strconv.Itoa(page))
		var body P
		hdr, err := c.do(ctx, http.MethodGet, path, q, nil, &body)
		if err != nil {
			return nil, err
		}
		all = append(all, rows(body)...)
		if !hasNextLink(hdr.Get("Link")) {
			return all, nil
		}
	}
}

// recentClosedWindow performs the ONE bounded request every recent-closed
// window shares (issue #176): state=closed&sort=updated&direction=desc&
// per_page=RecentClosedWindow, deliberately IGNORING any Link header on the
// response — following rel="next" would resurrect the exact O(history) walk
// the window exists to remove, so a full page is a full window, never an
// invitation to fetch page 2. GitHub has no recently-closed sort (Forgejo's
// recentclose has no equivalent in the pulls/issues sort enums), so
// updated-desc approximates it: closing bumps updated_at, and a later
// comment bumping an old closed item into the window is harmless — the
// window is a recency view, not a completeness contract. May return nil on
// zero results (callers re-slice through make(), the fetchPages convention).
func recentClosedWindow[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	q := url.Values{}
	q.Set("state", tracker.StateClosed)
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	q.Set("per_page", strconv.Itoa(tracker.RecentClosedWindow))
	var chunk []T
	if _, err := c.do(ctx, http.MethodGet, path, q, nil, &chunk); err != nil {
		return nil, err
	}
	return chunk, nil
}

// hasNextLink reports whether a GitHub Link header advertises a rel="next"
// page. The header is a comma-separated list of <url>; rel="kind" segments.
func hasNextLink(link string) bool {
	if link == "" {
		return false
	}
	for _, part := range strings.Split(link, ",") {
		if strings.Contains(part, `rel="next"`) {
			return true
		}
	}
	return false
}

// do performs one REST call: it applies the per-request timeout, sets the
// Bearer token and pinned API-version headers, marshals reqBody (when non-nil)
// as JSON, and decodes a 2xx response into out (when non-nil). It returns the
// response headers (for pagination's Link). A non-2xx status becomes a
// *statusError carrying the method, path, status, and a short body snippet —
// and NEVER the token (which lives only in the Authorization header, not the
// URL or error).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, reqBody, out any) (http.Header, error) {
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
			return nil, fmt.Errorf("github %s %s: encode request: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("github %s %s: build request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Header, newStatusError(method, path, resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.Header, fmt.Errorf("github %s %s: decode response: %w", method, path, err)
		}
	}
	return resp.Header, nil
}

// statusError is a non-2xx GitHub answer: the diagnostic line do() always
// produces (method, path, status, body snippet — never the token) plus the
// machine-readable status and whether the upstream rate limiter throttled it.
// 404 unwraps to tracker.ErrNotFound; a throttled 403/429 unwraps to
// tracker.ErrRateLimited — so callers answer not-found / rate-limited instead
// of an opaque bad-gateway. EnsureLabel reads the status via errors.As to
// recognize a duplicate-name conflict.
type statusError struct {
	status      int
	rateLimited bool
	message     string
}

func (e *statusError) Error() string { return e.message }

func (e *statusError) Unwrap() error {
	switch {
	case e.rateLimited:
		return tracker.ErrRateLimited
	case e.status == http.StatusNotFound:
		return tracker.ErrNotFound
	default:
		return nil
	}
}

// newStatusError classifies a non-2xx response. A 403 or 429 is a rate limit
// only when GitHub says so — X-RateLimit-Remaining: 0 or a Retry-After header
// — so an ordinary 403 (e.g. the token cannot see a private repo) stays a
// plain upstream error, not a spurious ErrRateLimited that would make the
// scheduler skip forever.
func newStatusError(method, path string, resp *http.Response) *statusError {
	rateLimited := false
	resetHint := ""
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			rateLimited, resetHint = true, "retry after "+ra+"s"
		} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			rateLimited = true
			if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
				resetHint = "resets at " + reset
			}
		}
	}
	msg := fmt.Sprintf("github %s %s: unexpected status %d: %s",
		method, path, resp.StatusCode, bodySnippet(resp.Body))
	if rateLimited {
		msg = fmt.Sprintf("github %s %s: rate limited (status %d, %s)",
			method, path, resp.StatusCode, resetHint)
	}
	return &statusError{status: resp.StatusCode, rateLimited: rateLimited, message: msg}
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
