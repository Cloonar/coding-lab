// Package labctl implements the agent-side CLI (brief §8.3): a thin, plain,
// parseable client for lab's /agent/v1 API. Sessions get LAB_URL and
// LAB_TOKEN in their environment; labctl is the run's ONLY tracker surface
// (D10 — it supersedes tea/gh entirely).
//
// Exit codes (pinned): 0 success · 1 API/HTTP error (message on stderr) ·
// 2 usage/configuration error. The `pr checks` verb DELIBERATELY overrides
// this (see runPRChecks): its 2 means exactly "checks are red", so every other
// failure — usage, env, transport, API — folds into 1, and a still-pending run
// is 3. The `secret exec` verb ALSO deviates (see runSecretExec): it execs a
// child and passes the child's exit code through VERBATIM, so 0/1/2 (or any
// other code) may be the child's own — only a pre-exec failure is labctl's: a
// bad NAME/`--`/command shape or missing env is 2, an API error or a command
// that fails to start is 1. The `secret scan` verb deviates as well (see
// runSecretScan): it backs the pre-push leak guard and fails CLOSED — findings
// AND every failure (git, transport, non-2xx) exit 1; only no rev-args or
// missing env is 2; a clean pass is a silent 0. No color, no spinner — output
// is for agents.
package labctl

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const usage = `labctl — agent-side CLI for lab

Usage:
  labctl issue view [n]                 show the run's claimed issue (or issue n), with comments
  labctl issue list                     list open issues (number, state, created, labels, title)
  labctl issue create --title T --body B [--labels a,b]
                                        file a new issue, labels attached at creation
  labctl issue edit <n> [--title T] [--body B]
                                        edit issue n's title and/or body (omitted flag untouched)
  labctl issue comment <n> <body>       comment on issue n
  labctl issue label add <n> <a,b>      add labels (comma-separated) to issue n
  labctl issue label remove <n> <a,b>   remove labels from issue n
  labctl issue close <n>                close issue n (comment the reason first)
  labctl label list                     list the repo's labels (name, color, description)
  labctl label create --name N [--color C --description D]
                                        create the label if missing (idempotent)
  labctl pr create --title T --body B   open a PR/CR for the current branch
  labctl pr view <n>                    show PR n (number, title, state, head, url, body)
  labctl pr list                        list open PRs plus the ~50 most recently closed (number, state, head, url)
  labctl pr merge <n>                   merge PR n (fixed method; the forge/base enforces mergeability)
  labctl pr checks <n> [--wait]         CI status of PR n; --wait polls until the aggregate leaves
                                        pending (exit 0 green/none · 2 red · 3 still pending)
  labctl pr reject <n> <body>           record a rejection verdict on PR n with body as the findings
  labctl pr approve <n> [body]          record a validation-passed verdict on PR n (body optional)
  labctl pr rerequest <n>               signal fix-done on PR n and re-request review from reviewers
                                        whose latest verdict requested changes
  labctl pr escalate <n> <body>         record the escalate-mode lander's terminal marker on PR n with
                                        body as the round-history digest
  labctl pr comment <n> <body>          post a plain discussion comment on PR n (no Closes # injection)
  labctl secret list                    list the repo's secrets (name, description; never values)
  labctl secret exec <NAME...> -- <cmd> [args...]
                                        run cmd with each named secret injected as $NAME in its
                                        env (values fetched at exec time; child's exit code passes through);
                                        child output is redacted — injected values appear as [REDACTED:NAME]
  labctl secret scan <rev-arg>...       scan the outgoing diff (git log -p over the given revisions)
                                        for secret values; findings or ANY failure exit 1 (fail closed)
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
	case "secret":
		return runSecret(args[1:], env)
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
	case "edit":
		// `labctl issue edit <n> [--title T] [--body B]`: n first, flags after.
		// Presence — not value — decides which fields patch: fs.Visit reports a
		// SET flag, so `--body ""` legally clears the body while an omitted flag
		// leaves the field untouched. At least one flag is required (an empty
		// patch is a server-side no-op, but the CLI wants a usage exit, not a
		// silent round-trip); a set-but-empty --title is rejected here for the
		// usage exit code (the server enforces it too).
		if len(args) < 2 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue edit: want <n> [--title T] [--body B]")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl issue edit: issue number %q is not an integer\n", args[1])
			return 2
		}
		fs := flag.NewFlagSet("labctl issue edit", flag.ContinueOnError)
		fs.SetOutput(env.Stderr)
		title := fs.String("title", "", "new issue title")
		body := fs.String("body", "", "new issue body (empty string clears it)")
		if err := fs.Parse(args[2:]); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue edit: unexpected arguments")
			return 2
		}
		var titleArg, bodyArg *string
		fs.Visit(func(fl *flag.Flag) {
			switch fl.Name {
			case "title":
				titleArg = title
			case "body":
				bodyArg = body
			}
		})
		if titleArg == nil && bodyArg == nil {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue edit: want at least one of --title or --body")
			return 2
		}
		if titleArg != nil && strings.TrimSpace(*titleArg) == "" {
			_, _ = fmt.Fprintln(env.Stderr, "labctl issue edit: title must not be empty")
			return 2
		}
		return withClient(env, "issue edit", func(c *Client) error {
			is, err := c.IssueEdit(n, titleArg, bodyArg)
			if err != nil {
				return err
			}
			printIssue(env.Stdout, is)
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
	if len(args) == 0 {
		_, _ = fmt.Fprint(env.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "view":
		if len(args) != 2 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr view: want <n>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl pr view: PR number %q is not an integer\n", args[1])
			return 2
		}
		return withClient(env, "pr view", func(c *Client) error {
			pd, err := c.PRView(n)
			if err != nil {
				return err
			}
			printPR(env.Stdout, pd)
			return nil
		})
	case "list":
		if len(args) > 1 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr list: too many arguments")
			return 2
		}
		return withClient(env, "pr list", func(c *Client) error {
			prs, err := c.PRList()
			if err != nil {
				return err
			}
			// One row per PR: number, state, head branch, URL — the same
			// columns the reaper matches on, readable in one call.
			for _, pr := range prs {
				_, _ = fmt.Fprintf(env.Stdout, "#%d\t%s\t%s\t%s\n",
					pr.Number, pr.State, pr.Head, pr.URL)
			}
			return nil
		})
	case "merge":
		if len(args) != 2 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr merge: want <n>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl pr merge: PR number %q is not an integer\n", args[1])
			return 2
		}
		return withClient(env, "pr merge", func(c *Client) error {
			pm, err := c.PRMerge(n)
			if err != nil {
				return err
			}
			// One parseable line: number, resulting state, URL.
			_, _ = fmt.Fprintf(env.Stdout, "#%d\t%s\t%s\n", pm.Number, pm.State, pm.URL)
			return nil
		})
	case "checks":
		// Hand-rolled env/client setup (NOT withClient) — this verb's exit
		// contract folds env/usage errors into 1, never withClient's 2.
		return runPRChecks(args[1:], env)
	case "reject":
		// Exactly <n> <body>: unlike `issue comment`, the body is ONE
		// positional argument (quote a multi-word body) — the findings are
		// required non-empty, checked here so a blank body is a usage error
		// (exit 2), not a round trip to the server's 400.
		if len(args) != 3 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr reject: want <n> <body>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl pr reject: PR number %q is not an integer\n", args[1])
			return 2
		}
		body := args[2]
		if body == "" {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr reject: body must not be empty")
			return 2
		}
		return withClient(env, "pr reject", func(c *Client) error {
			pr, err := c.PRReject(n, body)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Stdout, "#%d\trejected\n", pr.Number)
			return nil
		})
	case "approve":
		// <n> [body]: the body is optional — an approval needs no words.
		if len(args) != 2 && len(args) != 3 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr approve: want <n> [body]")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl pr approve: PR number %q is not an integer\n", args[1])
			return 2
		}
		var body string
		if len(args) == 3 {
			body = args[2]
		}
		return withClient(env, "pr approve", func(c *Client) error {
			pr, err := c.PRApprove(n, body)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Stdout, "#%d\tapproved\n", pr.Number)
			return nil
		})
	case "rerequest":
		if len(args) != 2 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr rerequest: want <n>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl pr rerequest: PR number %q is not an integer\n", args[1])
			return 2
		}
		return withClient(env, "pr rerequest", func(c *Client) error {
			pr, err := c.PRRerequest(n)
			if err != nil {
				return err
			}
			// A failed native reviewer ping is non-fatal — the done-signal
			// landed. Warn on stderr, keep the success output and exit 0.
			if pr.Warning != "" {
				_, _ = fmt.Fprintf(env.Stderr, "labctl pr rerequest: warning: %s\n", pr.Warning)
			}
			_, _ = fmt.Fprintf(env.Stdout, "#%d\trerequested\n", pr.Number)
			return nil
		})
	case "escalate":
		// Exactly <n> <body>, mirroring reject: the digest is the point, so a
		// blank body is a usage error (exit 2) here rather than a round trip
		// to the server's 400. Unlike rerequest there is no native ping to
		// warn about — escalation's forge-facing surface is this ONE comment.
		if len(args) != 3 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr escalate: want <n> <body>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl pr escalate: PR number %q is not an integer\n", args[1])
			return 2
		}
		body := args[2]
		if body == "" {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr escalate: body must not be empty")
			return 2
		}
		return withClient(env, "pr escalate", func(c *Client) error {
			pr, err := c.PREscalate(n, body)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(env.Stdout, "#%d\tescalated\n", pr.Number)
			return nil
		})
	case "comment":
		// Exactly <n> <body>, mirroring reject — a plain discussion comment,
		// required non-empty, no `Closes #N` injection.
		if len(args) != 3 {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr comment: want <n> <body>")
			return 2
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl pr comment: PR number %q is not an integer\n", args[1])
			return 2
		}
		body := args[2]
		if body == "" {
			_, _ = fmt.Fprintln(env.Stderr, "labctl pr comment: body must not be empty")
			return 2
		}
		// Silent on success, mirroring `labctl issue comment`'s confirmation
		// shape exactly: parseable silence, nothing to disambiguate.
		return withClient(env, "pr comment", func(c *Client) error { return c.PRComment(n, body) })
	case "create":
		// handled below
	default:
		_, _ = fmt.Fprintf(env.Stderr, "labctl pr: unknown subcommand %q\n\n%s", args[0], usage)
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

// printPR renders the plain-text PR view, mirroring printIssue: number,
// title, then one metadata line each for state, head, and url, then the
// body, then each submitted review under a "--- review by <reviewer>
// (<state>)" separator (dismissed reviews add ", dismissed" to the
// parenthetical) — the reject → re-queue loop's read. No reviews leaves the
// output exactly as it was before reviews existed.
func printPR(w io.Writer, pd PRDetail) {
	_, _ = fmt.Fprintf(w, "#%d %s\n", pd.Number, pd.Title)
	_, _ = fmt.Fprintf(w, "state: %s\n", pd.State)
	_, _ = fmt.Fprintf(w, "head: %s\n", pd.Head)
	_, _ = fmt.Fprintf(w, "url: %s\n", pd.URL)
	_, _ = fmt.Fprintf(w, "\n%s\n", pd.Body)
	for _, rv := range pd.Reviews {
		state := rv.State
		if rv.Dismissed {
			state += ", dismissed"
		}
		_, _ = fmt.Fprintf(w, "\n--- review by %s (%s)\n%s\n", rv.Reviewer, state, rv.Body)
	}
}

// checksPollInterval and checksWaitCap govern `labctl pr checks --wait`. It
// polls the aggregate CLIENT-SIDE: the agent harness blocks a foreground
// sleep, so labctl itself does the blocking — in ONE tool call — fetching the
// report every interval until the aggregate leaves pending or the cap elapses.
// They are vars, not consts, so tests can shrink them to milliseconds and keep
// the whole poll loop in sub-second wall-time.
var (
	checksPollInterval = 10 * time.Second
	checksWaitCap      = 5 * time.Minute
)

// Aggregate-state words `pr checks` branches its exit code on. They mirror
// tracker.ChecksState's vocabulary on the wire; labctl keeps its own copies so
// the CLI stays free of the internal/tracker dependency (like client.go's wire
// types). The aggregate is always one of these four — never recomputed here.
const (
	checksFailure = "failure"
	checksPending = "pending"
	checksSuccess = "success"
	checksNone    = "none"
)

// runPRChecks implements `labctl pr checks <n> [--wait]` (issue #72, the
// fix-the-red loop). Its exit codes are DELIBERATELY different from the
// binary-wide 2=usage convention (see the package doc): 2 must mean exactly one
// thing — the aggregate is red — so an agent can branch on it unambiguously.
// Every other failure (usage, missing env, transport, API) folds into 1; a
// still-pending run is 3; a green-or-none run is 0. Because a missing-env must
// be 1 (not withClient's 2), this hand-rolls the env/client setup rather than
// routing through withClient.
func runPRChecks(args []string, env Env) int {
	// --wait may sit in any position after `checks`; scan it out, then require
	// exactly one remaining arg — a positive PR number.
	wait := false
	var rest []string
	for _, a := range args {
		if a == "--wait" {
			wait = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) != 1 {
		return checksUsage(env, "want <n> [--wait]")
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil || n < 1 {
		return checksUsage(env, fmt.Sprintf("PR number %q is not a positive integer", rest[0]))
	}

	baseURL := env.Getenv("LAB_URL")
	token := env.Getenv("LAB_TOKEN")
	if baseURL == "" || token == "" {
		return checksErr(env, "LAB_URL and LAB_TOKEN must be set")
	}
	c := &Client{BaseURL: baseURL, Token: token}

	if !wait {
		// Single-shot: fetch once, print, exit by aggregate ("still pending at
		// exit" covers the single-shot pending case too).
		rep, err := c.PRChecks(n)
		if err != nil {
			return checksErr(env, err.Error())
		}
		printChecks(env.Stdout, rep)
		return checksExit(env, rep.State)
	}

	// --wait: block here (the harness blocks foreground sleep) polling until the
	// aggregate leaves pending or the cap elapses. Print only the FINAL report,
	// once — a per-poll dump would be stdout noise an agent has to parse past.
	deadline := time.Now().Add(checksWaitCap)
	for {
		rep, err := c.PRChecks(n)
		if err != nil {
			return checksErr(env, err.Error())
		}
		if rep.State != checksPending {
			printChecks(env.Stdout, rep)
			return checksExit(env, rep.State)
		}
		// Stop if the next sleep would carry us past the cap: surface the
		// still-pending report now and exit 3.
		if !time.Now().Add(checksPollInterval).Before(deadline) {
			printChecks(env.Stdout, rep)
			return 3
		}
		time.Sleep(checksPollInterval)
	}
}

// checksExit maps the server-computed aggregate to this verb's pinned exit
// code: red → 2, still pending → 3, success or none → 0. A word outside the
// four-word vocabulary is a malformed answer (a non-lab server, or version
// skew), reported as an API error (exit 1) — the same loud-over-silently-green
// rule tracker.ChecksState applies to row states; 0 must never be reachable by
// accident.
func checksExit(env Env, state string) int {
	switch state {
	case checksFailure:
		return 2
	case checksPending:
		return 3
	case checksSuccess, checksNone:
		return 0
	default:
		return checksErr(env, fmt.Sprintf("unexpected aggregate state %q in the server's answer", state))
	}
}

// checksUsage reports a `pr checks` argument error. Per this verb's pinned
// exit contract it returns 1 — NOT the binary-wide usage code 2 — so 2 stays
// reserved for the single meaning "checks are red". It prints usage so an
// agent that mis-called it sees the shape.
func checksUsage(env Env, msg string) int {
	_, _ = fmt.Fprintf(env.Stderr, "labctl pr checks: %s\n\n%s", msg, usage)
	return 1
}

// checksErr reports an env/transport/API failure for `pr checks` — also exit 1
// (only a red aggregate is 2), message on stderr in the house prefix.
func checksErr(env Env, msg string) int {
	_, _ = fmt.Fprintf(env.Stderr, "labctl pr checks: %s\n", msg)
	return 1
}

// printChecks renders the plain-text checks report, house style (a state line
// then tab-lists like `pr list`): first a `state: <aggregate>` line, then one
// tab-separated row per check in the wire column order
// name<TAB>state<TAB>rawState<TAB>summary<TAB>url. Zero rows print only the
// state line. Called once per invocation — never per poll (agents parse stdout).
func printChecks(w io.Writer, rep PRChecksReport) {
	_, _ = fmt.Fprintf(w, "state: %s\n", rep.State)
	for _, c := range rep.Checks {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.Name, c.State, c.RawState, c.Summary, c.URL)
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
