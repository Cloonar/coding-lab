// Package github is the GitHub REST implementation of the tracker.Tracker
// seam (ADR-0015). It is a structural twin of internal/tracker/forgejo — same
// one-do()-transport shape, same tracker-contract semantics that must survive
// (the ready queue is open issues carrying the ready-for-agent label; pulls
// are listed across ALL states with the three-valued open|merged|closed state;
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

type ghPullHead struct {
	Ref string `json:"ref"`
}

type ghPull struct {
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	State    string     `json:"state"`
	MergedAt *time.Time `json:"merged_at"` // non-nil ⇒ merged (the list endpoint has no `merged` bool)
	Head     ghPullHead `json:"head"`
	HTMLURL  string     `json:"html_url"`
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
// operator list view. PRs folded into GitHub's /issues are dropped; no
// comments are loaded.
func (c *Client) Issues(ctx context.Context, state string) ([]tracker.Issue, error) {
	q := url.Values{}
	q.Set("state", state)
	ghs, err := fetchPages[ghIssue](ctx, c, c.issuesPath(), q)
	if err != nil {
		return nil, err
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

// Pulls returns every pull request on the repo across all states. state=all is
// required: a merged afk/<N> PR is the success signal and is no longer "open",
// and the reaper still needs to see it. Matching to a run is client-side by
// head branch; the three-valued state is derived from state+merged_at.
func (c *Client) Pulls(ctx context.Context) ([]tracker.PullRef, error) {
	q := url.Values{}
	q.Set("state", tracker.StateAll)
	ghs, err := fetchPages[ghPull](ctx, c, c.pullsPath(), q)
	if err != nil {
		return nil, err
	}
	out := make([]tracker.PullRef, 0, len(ghs))
	for _, gh := range ghs {
		out = append(out, toPullRef(gh))
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
