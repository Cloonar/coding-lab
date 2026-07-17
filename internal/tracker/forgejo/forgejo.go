// Package forgejo is the Forgejo REST implementation of the tracker.Tracker
// seam (design §4d, M4). It replaces v0's tea-CLI shell-outs
// (docs/reference/lab-v0/forgejo.go had no REST — only origin parsing; the
// endpoint mappings here are the M4 rewrite, port-spec §7) while preserving
// the tracker-contract semantics that must survive: the ready queue is open
// issues carrying the ready-for-agent label; the issues endpoint is queried
// with type=issues so Forgejo does not fold PRs into the list; pulls are
// listed across ALL states with the three-valued open|merged|closed state
// derived from Forgejo's state+merged fields (a merged afk/<N> PR is the M5
// done-signal and is no longer "open"); a tracker call either yields the full
// result or an error carrying operation context — never partial data, and
// never the forge token.
package forgejo

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
	// defaultRequestTimeout bounds every REST call. v0's tea shell-outs had no
	// deadline; the port-spec (§5) asks the rewrite to add a sane HTTP timeout —
	// 30s, applied per request as a child context so pagination and the
	// two-call Issue(n) read each get a fresh budget.
	defaultRequestTimeout = 30 * time.Second

	// pageLimit is the page size for list endpoints. Forgejo paginates issues
	// and pulls; a non-paginating client would silently drop everything past
	// the first page — the exact "state=all silently breaks the done-signal"
	// failure the contract warns against. We loop until an empty page (see
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
// operator list view. type=issues excludes PRs; no comments are loaded.
func (c *Client) Issues(ctx context.Context, state string) ([]tracker.Issue, error) {
	q := url.Values{}
	q.Set("type", "issues")
	q.Set("state", state)
	fjs, err := fetchPages[fjIssue](ctx, c, c.issuesPath(), q)
	if err != nil {
		return nil, err
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

// Pulls returns every pull request on the repo across all states. state=all is
// required: a merged afk/<N> PR is the success signal and is no longer "open",
// and the reaper still needs to see it. Matching to a run is client-side by
// head branch; the three-valued state is derived from Forgejo's state+merged.
func (c *Client) Pulls(ctx context.Context) ([]tracker.PullRef, error) {
	q := url.Values{}
	q.Set("state", tracker.StateAll)
	fjs, err := fetchPages[fjPull](ctx, c, c.pullsPath(), q)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.PullRef, 0, len(fjs))
	for _, fj := range fjs {
		out = append(out, toPullRef(fj))
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
	author := fc.User.Login
	if author == "" {
		author = fc.User.Username
	}
	return tracker.Comment{
		Author:    author,
		Body:      fc.Body,
		CreatedAt: fc.CreatedAt,
	}
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
