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
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	State    string    `json:"state"`
	Labels   []string  `json:"labels"`
	Comments []Comment `json:"comments"`
}

// PR is the agent API's POST /prs answer.
type PR struct {
	Number int    `json:"number"`
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
		httpClient = &http.Client{Timeout: httpTimeout}
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
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var envelope struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error != "" {
			return fmt.Errorf("%s", envelope.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}
