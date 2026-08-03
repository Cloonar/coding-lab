package main

// `lab autoland rearm` (issue #188 / ADR-0048's amendment): the operator CLI
// half of re-arm — the human gesture that supersedes an escalation and returns
// a PR to the autoland poller's view, with its fix/escalate budgets restored.
// It is a thin POST to the human-authenticated operator API
// (POST /api/v1/repos/{id}/autoland/pulls/{pull}/rearm, internal/httpapi/
// autoland.go); every rule about what re-arm MEANS lives server-side, and this
// file only carries the gesture and prints what the server recorded.
//
// WHY THIS VERB LIVES ON cmd/lab AND NOT ON labctl — a security boundary, not
// a filing decision. internal/labctl + cmd/labctl are the RUN-TOKEN agent CLI:
// a session gets LAB_TOKEN=lab_run_… and talks to /agent/v1, a mux that is
// disjoint from the operator API and deliberately carries no re-arm route.
// Escalation means "agents could not finish this"; an agent able to lift its
// own terminal hand-off would make the bound decorative, so re-arm must be
// reachable only by a credential a run does not possess. cmd/lab — the server
// binary an operator runs on the host, with a `lab_pat_…` PAT — is that
// caller. Do NOT move, mirror, or re-export this verb into internal/labctl,
// cmd/labctl, or internal/agentapi; the absence there IS the feature.
//
// Two independent locks keep the run-token surface out of this route even when
// a session's env is inherited verbatim: the agent unix socket serves ONLY the
// /agent/v1 mux (cmd/lab/main.go's agentSrv), so /api/v1/… 404s there, and
// httpapi's resolveBearer refuses any Bearer token without the `lab_pat_`
// prefix. The `lab_run_` guard below is a third, purely diagnostic one — it
// turns an inherited-env mistake into a sentence instead of a 401.
//
// This is the first HTTP client cmd/lab has ever had; it is intentionally one
// verb's worth of transport and nothing more — no config file, no login flow,
// no reusable command framework. The next verb can grow one if it needs one.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// autolandUsage is the verb-scoped usage, printed on every exit-2 path. The
// package-level `usage` const is the SERVER's flag reference (dozens of
// lines); dumping it at someone who mistyped `-pull` would bury the answer.
const autolandUsage = `Usage: lab autoland rearm -repo <id> -pull <n> [-url <base>] [-token <pat> | -token-file <path>]

  -repo string        repo id the pull belongs to (required)
  -pull int           pull request number, >= 1 (required)
  -url string         lab base URL: http(s)://host[:port] or unix:///abs/path (LAB_URL)
  -token-file string  file holding an operator PAT (lab_pat_…); one trailing newline
                      stripped (LAB_PAT_FILE; wins over -token)
  -token string       operator PAT inline (LAB_PAT); prefer the env var or -token-file —
                      argv is world-readable via ps and lands in shell history
`

// rearmTimeout bounds the one request this CLI makes. Never
// http.DefaultClient: its zero Timeout means a wedged or blackholed server
// hangs the operator's terminal forever. Generous rather than tight — the
// handler kicks a spawn pass and publishes repo.changed before it answers —
// but finite.
const rearmTimeout = 30 * time.Second

// maxRearmBody bounds the response read: a proxy in the path can answer with
// something arbitrarily large, and neither the success envelope nor the error
// envelope is ever more than a few hundred bytes.
const maxRearmBody = 1 << 20 // 1 MiB

// runAutoland dispatches the `autoland` subcommand group. Mirrors
// runHashPassword's shape (args after the verb, writers injected so tests
// drive it in-process, exit code returned rather than os.Exit'd) plus a
// getenv, which is this verb's other ambient input — injected for the same
// reason the writers are, so the env-default tests need no process-global
// mutation.
//
// Exit codes are the repo's pinned convention (internal/labctl's package doc,
// `lab hash-password`): 0 success · 1 the operation failed · 2 usage or
// configuration error.
func runAutoland(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintf(stderr, "lab autoland: missing subcommand\n\n%s", autolandUsage)
		return 2
	}
	switch args[0] {
	case "rearm":
		return runAutolandRearm(args[1:], getenv, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "lab autoland: unknown subcommand %q\n\n%s", args[0], autolandUsage)
		return 2
	}
}

// runAutolandRearm implements `lab autoland rearm`: POST the re-arm and print
// one line. Its own flag.FlagSet, never the global one — the same reason
// hash-password stays off it: cmd/lab's package-level flags belong to the
// server's config.Parse, and a subcommand that registered there would collide
// with them.
func runAutolandRearm(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lab autoland rearm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Override flag's default Usage, and treat that as load-bearing rather
	// than cosmetic: PrintDefaults renders every flag's DEFAULT VALUE, and
	// -token's default IS whatever LAB_PAT holds — so `lab autoland rearm -h`
	// under a normal operator environment would print the PAT itself to
	// stderr, into scrollback, CI logs, and anything capturing them. The
	// hand-written autolandUsage names the env vars without ever rendering
	// their values. Any future flag defaulted from a secret env var inherits
	// this protection for free; one defaulted through fs.PrintDefaults would
	// not.
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, autolandUsage) }
	// Env values are the flag DEFAULTS, which is exactly the repo's documented
	// flag > env > default precedence (internal/config's pick): an explicitly
	// passed flag overrides the environment for free, with no ordering logic.
	repo := fs.String("repo", "", "repo id the pull belongs to")
	pull := fs.Int("pull", 0, "pull request number")
	baseURL := fs.String("url", getenv("LAB_URL"), "lab base URL (LAB_URL)")
	// The credential is offered THREE ways, in the order they should be
	// reached for — and the inline flag is last on purpose. `lab
	// hash-password` refuses a command-line password outright because argv is
	// visible to every process on the host via /proc/<pid>/cmdline (`ps`) and
	// lands in the invoking shell's history; a PAT is exactly as sensitive.
	// The verb cannot go stdin-only the way hash-password did (it must stay
	// usable inside a pipeline whose stdin is something else), so it follows
	// the OTHER precedent this binary already sets: -seed-password-hash-file
	// beside -seed-password-hash, file wins. LAB_PAT is the everyday path
	// (env is not in argv and not in history), -token-file is the systemd /
	// secrets-manager path, and -token exists because a one-off interactive
	// invocation is a real thing — documented as the discouraged one.
	tokenFile := fs.String("token-file", getenv("LAB_PAT_FILE"), "file holding the operator PAT (LAB_PAT_FILE; wins over -token)")
	token := fs.String("token", getenv("LAB_PAT"), "operator PAT inline (LAB_PAT); prefer LAB_PAT or -token-file")
	if err := fs.Parse(args); err != nil {
		// ContinueOnError already printed the reason (and -h printed the flag
		// list) to stderr; a bad flag is a usage error either way.
		return 2
	}

	// usageErr prints one line naming what is wrong plus the verb's usage —
	// the shape labctl's withClient uses for missing env — and yields 2.
	usageErr := func(msg string) int {
		_, _ = fmt.Fprintf(stderr, "lab autoland rearm: %s\n\n%s", msg, autolandUsage)
		return 2
	}

	if fs.NArg() > 0 {
		return usageErr("unexpected arguments; re-arm takes flags only")
	}
	if *repo == "" {
		return usageErr("-repo is required")
	}
	// The server 400s pull < 1 as well; checking here means an obvious typo
	// costs no round trip and no auth, and the message names the flag rather
	// than the wire field.
	if *pull < 1 {
		return usageErr("-pull is required and must be a positive integer")
	}
	if *baseURL == "" {
		return usageErr("-url is required (or set LAB_URL)")
	}

	pat := *token
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			// Name only the PATH, never the (possibly partial) contents —
			// seed.go's rule for the hash file. A file we were pointed at and
			// cannot read is a configuration error (2), not a failed
			// operation (1): nothing was attempted.
			_, _ = fmt.Fprintf(stderr, "lab autoland rearm: reading token file %q: %v\n", *tokenFile, err)
			return 2
		}
		// Strip EXACTLY ONE trailing "\n" — seed.go's byte-exact contract for
		// the seed hash file, for the same reason: a token is compared by
		// hash, so TrimSpace-style over-trimming would silently mint a
		// different credential.
		pat, _ = strings.CutSuffix(string(b), "\n")
	}
	if pat == "" {
		return usageErr("an operator PAT is required (set LAB_PAT, or pass -token-file / -token)")
	}
	// Diagnostic only — the server is the authority on what a credential is.
	// A run token here means someone ran this inside an agent session where
	// LAB_TOKEN/LAB_PAT got crossed; the honest answer is that re-arm is not
	// an agent's to make, not a bare 401.
	if strings.HasPrefix(pat, "lab_run_") {
		return usageErr("that is a run token (lab_run_…), not an operator PAT (lab_pat_…); re-arm is a human gesture and is not exposed to agent runs")
	}

	base, err := rearmRequestBase(*baseURL)
	if err != nil {
		return usageErr(err.Error())
	}
	// PathEscape the repo id: it is opaque caller input on a path segment, and
	// an id carrying '/' or a space would otherwise silently address a
	// different route. strconv.Itoa for {pull} — already validated numeric.
	path := "/api/v1/repos/" + url.PathEscape(*repo) + "/autoland/pulls/" + strconv.Itoa(*pull) + "/rearm"
	req, err := http.NewRequest(http.MethodPost, base+path, nil)
	if err != nil {
		return usageErr(fmt.Sprintf("building request for %s: %v", *baseURL, err))
	}
	// Bearer PAT and nothing else. No CSRF header is needed and none is sent:
	// httpapi's csrfMiddleware guards only AMBIENT credentials (session
	// cookie, proxy header) because those ride along without the caller's
	// involvement — authPAT is an explicit credential and bypasses the check
	// by design (internal/httpapi/auth.go: `authMethod.ambient`). The route
	// takes no body, so there is no Content-Type either.
	req.Header.Set("Authorization", "Bearer "+pat)

	resp, err := rearmHTTPClient(*baseURL).Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lab autoland rearm: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRearmBody))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lab autoland rearm: reading response: %v\n", err)
		return 1
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "lab autoland rearm: %s\n", rearmErrorMessage(resp.StatusCode, data))
		return 1
	}

	var out struct {
		RepoID     string `json:"repo_id"`
		PullNumber int    `json:"pull_number"`
		RearmedAt  string `json:"rearmed_at"`
	}
	// A 200 whose body is not the envelope is never a real answer — the
	// classic shape is an SSO proxy serving its login page inline with a 200
	// (issue #30). Failing here rather than printing a half-empty line means a
	// lost write can never read as success.
	if err := json.Unmarshal(data, &out); err != nil {
		_, _ = fmt.Fprintf(stderr, "lab autoland rearm: decoding response: %v\n", err)
		return 1
	}

	// One tab-separated line, in labctl's output register (`#12\tescalated`):
	// terse, no color, greppable, cut-able. The columns are scope, subject,
	// verb, moment — repo first because, unlike labctl, this CLI is not
	// repo-scoped by its credential, so the repo is what disambiguates the
	// line; `#<pull>` in labctl's spelling for a PR; `rearmed` as the
	// past-tense verb its siblings use (`rejected`, `approved`, `escalated`),
	// so the line reads as a record of what happened rather than a status; and
	// the supersession instant last, because it is the one value the caller
	// cannot derive and the exact value the terminality gate compares
	// escalation signals against. Every column is echoed from the SERVER's
	// answer, never from the flags, so the line is evidence of what was
	// recorded rather than a restatement of what was asked.
	_, _ = fmt.Fprintf(stdout, "%s\t#%d\trearmed\t%s\n", out.RepoID, out.PullNumber, out.RearmedAt)
	return 0
}

// rearmErrorMessage renders a non-200 for the operator: the API's canonical
// {"error": …} envelope when the body is one (writeError in
// internal/httpapi), otherwise a bare "HTTP <status>" — a proxy's HTML page,
// an empty body, or a truncated read must produce a status line, never a
// panic and never a misleading empty message.
func rearmErrorMessage(status int, data []byte) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error != "" {
		return envelope.Error
	}
	return fmt.Sprintf("HTTP %d", status)
}

// rearmRequestBase returns the prefix the request URL is built on. http(s) is
// the URL with any trailing slash trimmed; unix:///abs/path names a socket the
// transport dials directly, so its "host" is never resolved and a fixed dummy
// origin keeps the http.Request well-formed. The approach mirrors
// internal/labctl's client (LAB_URL has accepted unix:// since issue #201) —
// deliberately re-derived rather than imported, because importing the
// run-token client into the operator binary is precisely the coupling this
// file exists to prevent. A relative path is rejected here, before a dial can
// turn it into a confusing ENOENT.
func rearmRequestBase(baseURL string) (string, error) {
	sock, ok := strings.CutPrefix(baseURL, "unix://")
	if !ok {
		return strings.TrimRight(baseURL, "/"), nil
	}
	if !strings.HasPrefix(sock, "/") {
		return "", fmt.Errorf("-url unix:// socket path must be absolute (unix:///abs/path)")
	}
	return "http://lab", nil
}

// rearmHTTPClient builds the one client this verb uses: a finite timeout, no
// redirect-following, and a unix dialer when the base URL names a socket.
//
// Redirects are stopped rather than followed: the operator API never
// redirects, so a 3xx means something in front of lab answered — typically an
// SSO proxy bouncing a machine request to a login page — and following it
// would turn a lost write into an HTML "success" (issue #30). Stopping makes
// it a plain "HTTP 302" on stderr with exit 1.
//
// The unix branch is a transport detail only: what listens on lab's agent
// socket is the /agent/v1 mux, which has no /api/v1 routes at all, so this is
// a way to reach an operator API bound to a socket — never a path from a
// session's socket to a route the session cannot have.
func rearmHTTPClient(baseURL string) *http.Client {
	client := &http.Client{
		Timeout:       rearmTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if sock, ok := strings.CutPrefix(baseURL, "unix://"); ok {
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}
	}
	return client
}
