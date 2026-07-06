// Package labctl implements the agent-side CLI (brief §8.3). M1 ships the
// full command-line surface — parsing, env handling, exit codes — over a
// stub transport; M5 fills in the HTTP client against /agent/v1.
package labctl

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
)

// ErrNotImplemented is returned by every Client method until the agent API
// lands in M5.
var ErrNotImplemented = errors.New("agent API lands in M5")

const usage = `labctl — agent-side CLI for lab

Usage:
  labctl issue view [n]                 show the run's claimed issue (or issue n), with comments
  labctl issue list                     list open issues
  labctl issue comment <n> <body>       comment on issue n
  labctl pr create --title T --body B   open a PR/CR for the current branch
  labctl --version                      print version

Environment:
  LAB_URL    base URL of the lab server (set by lab in the session)
  LAB_TOKEN  run token (set by lab in the session)
`

// Env is the process environment Run needs; injectable for tests.
type Env struct {
	Getenv  func(string) string
	Stdout  io.Writer
	Stderr  io.Writer
	Version string
}

// Client is the transport to /agent/v1. M1: every method is a stub so the
// command layer (and its exit-code contract) is final before M5 fills in
// HTTP.
type Client struct {
	BaseURL string
	Token   string
}

// IssueView fetches the run's claimed issue, or issue *n when n is non-nil.
func (c *Client) IssueView(n *int) error { return ErrNotImplemented }

// IssueList lists open issues.
func (c *Client) IssueList() error { return ErrNotImplemented }

// IssueComment posts a comment on issue n.
func (c *Client) IssueComment(n int, body string) error { return ErrNotImplemented }

// PRCreate opens a PR/CR.
func (c *Client) PRCreate(title, body string) error { return ErrNotImplemented }

// Run executes one labctl invocation and returns the process exit code:
// 0 success, 2 usage/configuration/unimplemented errors.
func Run(args []string, env Env) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, usage)
		return 2
	}

	switch args[0] {
	case "--version", "-version", "version":
		_, _ = fmt.Fprintf(env.Stdout, "labctl %s\n", env.Version)
		return 0
	case "--help", "-help", "-h", "help":
		_, _ = fmt.Fprint(env.Stdout, usage)
		return 0
	case "issue":
		return runIssue(args[1:], env)
	case "pr":
		return runPR(args[1:], env)
	default:
		_, _ = fmt.Fprintf(env.Stderr, "labctl: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runIssue(args []string, env Env) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "view":
		var n *int
		switch len(args) {
		case 1:
		case 2:
			v, err := strconv.Atoi(args[1])
			if err != nil {
				_, _ = fmt.Fprintf(env.Stderr, "labctl issue view: issue number %q is not an integer\n", args[1])
				return 2
			}
			n = &v
		default:
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue view: too many arguments")
			return 2
		}
		return withClient(env, "issue view", func(c *Client) error { return c.IssueView(n) })
	case "list":
		if len(args) > 1 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue list: too many arguments")
			return 2
		}
		return withClient(env, "issue list", func(c *Client) error { return c.IssueList() })
	case "comment":
		if len(args) != 3 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue comment: want <n> <body>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl issue comment: issue number %q is not an integer\n", args[1])
			return 2
		}
		body := args[2]
		if body == "" {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue comment: body must not be empty")
			return 2
		}
		return withClient(env, "issue comment", func(c *Client) error { return c.IssueComment(n, body) })
	default:
		_, _ = fmt.Fprintf(env.Stderr, "labctl issue: unknown subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}

func runPR(args []string, env Env) int {
	if len(args) == 0 || args[0] != "create" {
		_, _ = fmt.Fprint(env.Stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("labctl pr create", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	title := fs.String("title", "", "PR title")
	body := fs.String("body", "", "PR body (must include Closes #N)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintln(env.Stderr, "labctl pr create: unexpected arguments")
		return 2
	}
	if *title == "" {
		_, _ = fmt.Fprintln(env.Stderr, "labctl pr create: --title is required")
		return 2
	}
	if *body == "" {
		_, _ = fmt.Fprintln(env.Stderr, "labctl pr create: --body is required")
		return 2
	}
	return withClient(env, "pr create", func(c *Client) error { return c.PRCreate(*title, *body) })
}

// withClient builds the Client from LAB_URL/LAB_TOKEN and runs fn, mapping
// errors to stderr + exit 2.
func withClient(env Env, cmd string, fn func(*Client) error) int {
	baseURL := env.Getenv("LAB_URL")
	token := env.Getenv("LAB_TOKEN")
	if baseURL == "" || token == "" {
		_, _ = fmt.Fprintf(env.Stderr, "labctl %s: LAB_URL and LAB_TOKEN must be set\n", cmd)
		return 2
	}
	if err := fn(&Client{BaseURL: baseURL, Token: token}); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "labctl %s: %v\n", cmd, err)
		return 2
	}
	return 0
}
