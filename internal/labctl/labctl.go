// Package labctl implements the agent-side CLI (brief §8.3): a thin, plain,
// parseable client for lab's /agent/v1 API. Sessions get LAB_URL and
// LAB_TOKEN in their environment; labctl is the run's ONLY tracker surface
// (D10 — it supersedes tea/gh entirely).
//
// Exit codes (pinned): 0 success · 1 API/HTTP error (message on stderr) ·
// 2 usage/configuration error. No color, no spinner — output is for agents.
package labctl

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

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

// Run executes one labctl invocation and returns the process exit code:
// 0 success, 1 API/HTTP error, 2 usage/configuration errors.
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
		return withClient(env, "issue view", func(c *Client) error {
			is, err := c.IssueView(n)
			if err != nil {
				return err
			}
			printIssue(env.Stdout, is)
			return nil
		})
	case "list":
		if len(args) > 1 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue list: too many arguments")
			return 2
		}
		return withClient(env, "issue list", func(c *Client) error {
			issues, err := c.IssueList()
			if err != nil {
				return err
			}
			for _, is := range issues {
				_, _ = fmt.Fprintf(env.Stdout, "#%d\t%s\t%s\n", is.Number, is.State, is.Title)
			}
			return nil
		})
	case "comment":
		// Join the trailing args as the body so an unquoted multi-word comment
		// ("labctl issue comment 7 tests are green") posts as-is; a quoted
		// single-arg body is unchanged (join over one element is a no-op).
		if len(args) < 3 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue comment: want <n> <body>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl issue comment: issue number %q is not an integer\n", args[1])
			return 2
		}
		body := strings.Join(args[2:], " ")
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
	return withClient(env, "pr create", func(c *Client) error {
		pr, err := c.PRCreate(*title, *body)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(env.Stdout, "%d\t%s\n", pr.Number, pr.URL)
		return nil
	})
}

// printIssue renders the pinned plain-text issue view: number, title, state,
// labels, body, then each comment under a "--- comment by <author> (<time>)"
// separator.
func printIssue(w io.Writer, is Issue) {
	_, _ = fmt.Fprintf(w, "#%d %s\n", is.Number, is.Title)
	_, _ = fmt.Fprintf(w, "state: %s\n", is.State)
	_, _ = fmt.Fprintf(w, "labels: %s\n", strings.Join(is.Labels, ", "))
	_, _ = fmt.Fprintf(w, "\n%s\n", is.Body)
	for _, c := range is.Comments {
		_, _ = fmt.Fprintf(w, "\n--- comment by %s (%s)\n%s\n", c.Author, c.CreatedAt, c.Body)
	}
}

// withClient builds the Client from LAB_URL/LAB_TOKEN and runs fn. Missing
// environment is a configuration error (exit 2, with usage); an fn error is
// an API/HTTP failure (message to stderr, exit 1).
func withClient(env Env, cmd string, fn func(*Client) error) int {
	baseURL := env.Getenv("LAB_URL")
	token := env.Getenv("LAB_TOKEN")
	if baseURL == "" || token == "" {
		_, _ = fmt.Fprintf(env.Stderr, "labctl %s: LAB_URL and LAB_TOKEN must be set\n\n%s", cmd, usage)
		return 2
	}
	if err := fn(&Client{BaseURL: baseURL, Token: token}); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "labctl %s: %v\n", cmd, err)
		return 1
	}
	return 0
}
