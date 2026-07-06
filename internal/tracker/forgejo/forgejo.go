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
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	Name string `json:"name"`
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
}

type fjPull struct {
	Number  int        `json:"number"`
	State   string     `json:"state"`
	Merged  bool       `json:"merged"`
	Head    fjPullHead `json:"head"`
	HTMLURL string     `json:"html_url"`
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

// CloseIssue closes an issue via PATCH state=closed.
func (c *Client) CloseIssue(ctx context.Context, number int) error {
	req := struct {
		State string `json:"state"`
	}{State: "closed"}
	return c.do(ctx, http.MethodPatch, c.issuePath(number), nil, req, nil)
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
func (c *Client) issuePath(n int) string { return c.repoPath("/issues/" + strconv.Itoa(n)) }

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
		if resp.StatusCode == http.StatusNotFound {
			// A typed sentinel ONLY for 404 — the one upstream status that
			// means "no such subject" rather than "forge failure" — so callers
			// can answer not-found instead of bad-gateway. (Forgejo also 404s
			// repos the token cannot see; that is still "not found" to us.)
			return fmt.Errorf("forgejo %s %s: unexpected status %d: %s: %w",
				method, path, resp.StatusCode, bodySnippet(resp.Body), tracker.ErrNotFound)
		}
		return fmt.Errorf("forgejo %s %s: unexpected status %d: %s", method, path, resp.StatusCode, bodySnippet(resp.Body))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("forgejo %s %s: decode response: %w", method, path, err)
		}
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
