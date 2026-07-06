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
  labctl issue list                     list open issues (number, state, created, labels, title)
  labctl issue create --title T --body B [--labels a,b]
                                        file a new issue, labels attached at creation
  labctl issue comment <n> <body>       comment on issue n
  labctl issue label add <n> <a,b>      add labels (comma-separated) to issue n
  labctl issue label remove <n> <a,b>   remove labels from issue n
  labctl issue close <n>                close issue n (comment the reason first)
  labctl label list                     list the repo's labels (name, color, description)
  labctl label create --name N [--color C --description D]
                                        create the label if missing (idempotent)
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
	case "label":
		return runLabel(args[1:], env)
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
			// One row per issue: number, state, created-at, comma-joined
			// labels, title — everything the triage buckets (unlabeled /
			// needs-triage / needs-info, oldest first) need from ONE call.
			// Title stays last: it is the one free-text column.
			for _, is := range issues {
				_, _ = fmt.Fprintf(env.Stdout, "#%d\t%s\t%s\t%s\t%s\n",
					is.Number, is.State, is.CreatedAt, strings.Join(is.Labels, ","), is.Title)
			}
			return nil
		})
	case "create":
		fs := flag.NewFlagSet("labctl issue create", flag.ContinueOnError)
		fs.SetOutput(env.Stderr)
		title := fs.String("title", "", "issue title")
		body := fs.String("body", "", "issue body")
		labels := fs.String("labels", "", "comma-separated label names to attach at creation")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue create: unexpected arguments")
			return 2
		}
		if *title == "" {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue create: --title is required")
			return 2
		}
		if *body == "" {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue create: --body is required")
			return 2
		}
		return withClient(env, "issue create", func(c *Client) error {
			is, err := c.IssueCreate(*title, *body, splitLabels(*labels))
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Stdout, "%d\n", is.Number)
			return nil
		})
	case "label":
		return runIssueLabel(args[1:], env)
	case "close":
		if len(args) != 2 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue close: want <n>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl issue close: issue number %q is not an integer\n", args[1])
			return 2
		}
		return withClient(env, "issue close", func(c *Client) error { return c.IssueClose(n) })
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

// runIssueLabel handles `labctl issue label add|remove <n> <labels>`.
func runIssueLabel(args []string, env Env) int {
	if len(args) == 0 || (args[0] != "add" && args[0] != "remove") {
		_, _ = fmt.Fprintf(env.Stderr, "labctl issue label: want add|remove <n> <labels>\n\n%s", usage)
		return 2
	}
	op := args[0]
	cmd := "issue label " + op
	if len(args) != 3 {
		_, _ = fmt.Fprintf(env.Stderr, "labctl %s: want <n> <labels>\n", cmd)
		return 2
	}
	n, err := strconv.Atoi(args[1])
	if err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "labctl %s: issue number %q is not an integer\n", cmd, args[1])
		return 2
	}
	labels := splitLabels(args[2])
	if len(labels) == 0 {
		_, _ = fmt.Fprintf(env.Stderr, "labctl %s: labels must not be empty\n", cmd)
		return 2
	}
	return withClient(env, cmd, func(c *Client) error {
		if op == "add" {
			return c.IssueLabelAdd(n, labels)
		}
		return c.IssueLabelRemove(n, labels)
	})
}

// runLabel handles the repo-level label commands: list and idempotent create.
func runLabel(args []string, env Env) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "list":
		if len(args) > 1 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl label list: too many arguments")
			return 2
		}
		return withClient(env, "label list", func(c *Client) error {
			labels, err := c.LabelList()
			if err != nil {
				return err
			}
			for _, l := range labels {
				_, _ = fmt.Fprintf(env.Stdout, "%s\t%s\t%s\n", l.Name, l.Color, l.Description)
			}
			return nil
		})
	case "create":
		fs := flag.NewFlagSet("labctl label create", flag.ContinueOnError)
		fs.SetOutput(env.Stderr)
		name := fs.String("name", "", "label name")
		color := fs.String("color", "", "label color (#rrggbb; the server default when omitted)")
		description := fs.String("description", "", "label description")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl label create: unexpected arguments")
			return 2
		}
		if *name == "" {
			_, _ = fmt.Fprintln(env.Stderr, "labctl label create: --name is required")
			return 2
		}
		return withClient(env, "label create", func(c *Client) error {
			// Prints the label that exists afterwards — created now or
			// already there (idempotent ensure; retries are safe).
			l, err := c.LabelCreate(*name, *color, *description)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Stdout, "%s\t%s\n", l.Name, l.Color)
			return nil
		})
	default:
		_, _ = fmt.Fprintf(env.Stderr, "labctl label: unknown subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}

// splitLabels parses a comma-separated label list, trimming whitespace and
// dropping empty entries ("a, b," → [a b]).
func splitLabels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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
