package labctl

// The HTTP transport to /agent/v1. Every request carries the run token as a
// Bearer header; every non-2xx answer is decoded from the canonical
// {"error": …} envelope into a plain error (the command layer prints it to
// stderr and exits 1). Wire shapes mirror internal/agentapi's responses.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// httpTimeout bounds every labctl request; a CLI must never hang forever on
// a wedged server (forge-proxied reads can be slow, so it is generous).
const httpTimeout = 60 * time.Second

// maxResponseBody bounds response reads (matches the server's request bound).
const maxResponseBody = 1 << 20 // 1 MiB

// Comment is one issue comment as the agent API renders it.
type Comment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// Issue is the agent API's issue shape; list responses leave Comments empty.
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []string  `json:"labels"`
	Comments  []Comment `json:"comments"`
	CreatedAt string    `json:"created_at"`
}

// Label is the agent API's label shape.
type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// PR is the agent API's POST /prs answer.
type PR struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// PRDetail is the agent API's GET /prs/{n} shape: metadata plus the full body.
type PRDetail struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Head   string `json:"head"`
	URL    string `json:"url"`
}

// PRRef is one row of the agent API's GET /prs list (no title/body).
type PRRef struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   string `json:"head"`
	URL    string `json:"url"`
}

// Client is the transport to /agent/v1, built from LAB_URL/LAB_TOKEN.
type Client struct {
	BaseURL string
	Token   string
	// HTTP overrides the default client (tests); nil uses a 60s-timeout client.
	HTTP *http.Client
}

// IssueView fetches the run's claimed issue (n == nil → GET /issue), or
// issue *n (GET /issues/{n}).
func (c *Client) IssueView(n *int) (Issue, error) {
	path := "/agent/v1/issue"
	if n != nil {
		path = "/agent/v1/issues/" + strconv.Itoa(*n)
	}
	var is Issue
	err := c.do(http.MethodGet, path, nil, &is)
	return is, err
}

// IssueList lists the repo's open issues.
func (c *Client) IssueList() ([]Issue, error) {
	var resp struct {
		Issues []Issue `json:"issues"`
	}
	if err := c.do(http.MethodGet, "/agent/v1/issues", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Issues, nil
}

// IssueComment posts a comment on issue n.
func (c *Client) IssueComment(n int, body string) error {
	return c.do(http.MethodPost, "/agent/v1/issues/"+strconv.Itoa(n)+"/comments",
		map[string]string{"body": body}, nil)
}

// PRCreate opens a PR/CR for the run's branch.
func (c *Client) PRCreate(title, body string) (PR, error) {
	var pr PR
	err := c.do(http.MethodPost, "/agent/v1/prs",
		map[string]string{"title": title, "body": body}, &pr)
	return pr, err
}

// PRView fetches PR n in full detail, body included.
func (c *Client) PRView(n int) (PRDetail, error) {
	var pd PRDetail
	err := c.do(http.MethodGet, "/agent/v1/prs/"+strconv.Itoa(n), nil, &pd)
	return pd, err
}

// PRList lists the repo's PRs across all states.
func (c *Client) PRList() ([]PRRef, error) {
	var resp struct {
		PRs []PRRef `json:"prs"`
	}
	if err := c.do(http.MethodGet, "/agent/v1/prs", nil, &resp); err != nil {
		return nil, err
	}
	return resp.PRs, nil
}

// IssueCreate files a new issue with labels attached at creation.
func (c *Client) IssueCreate(title, body string, labels []string) (Issue, error) {
	var is Issue
	err := c.do(http.MethodPost, "/agent/v1/issues",
		map[string]any{"title": title, "body": body, "labels": labels}, &is)
	return is, err
}

// IssueLabelAdd attaches labels to issue n.
func (c *Client) IssueLabelAdd(n int, labels []string) error {
	return c.do(http.MethodPost, "/agent/v1/issues/"+strconv.Itoa(n)+"/labels",
		map[string][]string{"labels": labels}, nil)
}

// IssueLabelRemove detaches labels from issue n. The label set rides in the
// request body, mirroring the add (label names may contain '/').
func (c *Client) IssueLabelRemove(n int, labels []string) error {
	return c.do(http.MethodDelete, "/agent/v1/issues/"+strconv.Itoa(n)+"/labels",
		map[string][]string{"labels": labels}, nil)
}

// IssueClose closes issue n.
func (c *Client) IssueClose(n int) error {
	return c.do(http.MethodPost, "/agent/v1/issues/"+strconv.Itoa(n)+"/close", nil, nil)
}

// LabelList lists the repo's labels.
func (c *Client) LabelList() ([]Label, error) {
	var resp struct {
		Labels []Label `json:"labels"`
	}
	if err := c.do(http.MethodGet, "/agent/v1/labels", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Labels, nil
}

// LabelCreate ensures a label exists (idempotent by name) and returns the
// label that exists afterwards.
func (c *Client) LabelCreate(name, color, description string) (Label, error) {
	var l Label
	err := c.do(http.MethodPost, "/agent/v1/labels",
		map[string]string{"name": name, "color": color, "description": description}, &l)
	return l, err
}

// do performs one request. Non-2xx answers become errors carrying the
// server's {"error"} message (or "HTTP <status>" when the body is not the
// envelope); out, when non-nil, receives the decoded 2xx body.
func (c *Client) do(method, path string, reqBody, out any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout, CheckRedirect: noRedirect}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	// The agent API never redirects. A 3xx means the request hit something else
	// in front of lab — typically an SSO/auth proxy that bounces unauthenticated
	// machine traffic to a login page. Surface the redirect target instead of
	// following it and choking on HTML (issue #30).
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return c.redirectError(resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var envelope struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error != "" {
			return fmt.Errorf("%s", envelope.Error)
		}
		if isHTML(resp, data) {
			return c.proxyHintError(resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// A 2xx whose body is HTML — a proxy serving its login page inline with a
	// 200 rather than a redirect — is never a real agent-API response. Reject it
	// for every command, including mutations (out == nil) that would otherwise
	// return nil here and report a lost write as success (issue #30).
	if isHTML(resp, data) {
		return c.proxyHintError(resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// noRedirect stops the client at the first redirect so do can report it,
// rather than following it out to a login page and decoding HTML as JSON.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// redirectError describes a 3xx from the agent API: the status, the redirect
// target, and the likely cause (LAB_URL aimed at an auth proxy).
func (c *Client) redirectError(resp *http.Response) error {
	loc := resp.Header.Get("Location")
	if loc == "" {
		loc = "(no Location header)"
	}
	return fmt.Errorf("agent API returned HTTP %d redirect to %s; LAB_URL (%s) may be pointed at an auth proxy instead of the lab server",
		resp.StatusCode, loc, strings.TrimRight(c.BaseURL, "/"))
}

// proxyHintError describes an HTML response where JSON was expected — the
// tell-tale of an SSO/auth proxy answering in lab's place.
func (c *Client) proxyHintError(status int) error {
	return fmt.Errorf("agent API returned an HTML page (HTTP %d), not JSON; LAB_URL (%s) may be pointed at an auth proxy instead of the lab server",
		status, strings.TrimRight(c.BaseURL, "/"))
}

// isHTML reports whether a response body is an HTML document rather than the
// JSON envelope — by Content-Type or a leading '<' once whitespace is trimmed.
func isHTML(resp *http.Response, data []byte) bool {
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return true
	}
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == '<'
}
