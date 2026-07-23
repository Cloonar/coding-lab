// Package tmuxx is lab's SessionRunner seam — the only interface to agent
// processes (design §4d; port of lab-v0 sessions.go). It wraps exactly eight
// tmux behaviours: start a detached session running an arbitrary argv in a
// working dir, stop it, ask whether it exists, list live names, send
// keystrokes (literal text, named keys, or a bracketed paste), and capture
// the pane (used by the provider's login flow to scrape the OAuth URL).
// tmux is the source of truth for liveness — no other state is held here.
//
// Deltas from v0 (per the sessions-spawn port spec §7): the spawn argv
// arrives complete from the AgentProvider (no "%s" substitution — the
// provider renders the session name into SpawnArgv itself); each session
// receives per-session environment entries (LAB_URL/LAB_TOKEN, the repo
// credential's git env) via `new-session -e KEY=VALUE` flags (tmux ≥ 3.2;
// the value never appears in the pane command's argv, so it stays out of
// `ps` output), and a session's environment is exactly those entries plus a
// fixed PATH/HOME/TERM/locale baseline forwarded from the lab process — the
// service environment (EnvironmentFile secrets such as a LAB_DB DSN) is
// scrubbed from every pane, since the tmux server the first wrapper call
// starts implicitly would otherwise seed its global environment from
// os.Environ() and hand it to every pane (issue #204); SendKeys takes an
// explicit enter flag; pane capture is a single-shot CapturePane — the poll
// loops (captureTimeout etc.) live in the provider.
//
// MUST-NOT-CHANGE v0 behaviours kept verbatim: prlimit wraps the INNER
// pane command with soft=hard and a "--" terminator; "=" exact-match
// prefix on has-session/kill-session but BARE names on send-keys/
// capture-pane (their target-pane parsers reject "=", verified tmux 3.6a);
// SendKeys is two calls (literal "-l --" text, then a separate non-literal
// Enter); capture flags are "-p -J -S -"; Start is idempotent with the
// 500 ms exited-immediately re-check; Stop is idempotent; an
// exec.ExitError means "absent" (has-session) / "no server" (list-sessions).
package tmuxx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// LoginSessionPrefix is the shared name prefix of every provider's login
// session ("lab-login-<providerID>"). Kept a single const: LoginSessionName
// derives per-provider names from it and IsLoginSession matches on it, so
// the two cannot drift (issue #77).
const LoginSessionPrefix = "lab-login"

// LoginSessionName returns the login-session name for providerID
// (lab-login-<providerID>, e.g. "lab-login-claude-code"). To this package
// the result is a session like any other; the exclusions — cap counting,
// branch ownership, stop-all — live in the callers via IsLoginSession
// (design §4d).
func LoginSessionName(providerID string) string {
	return LoginSessionPrefix + "-" + providerID
}

// IsLoginSession reports whether name is a provider login session — the
// one exclusion predicate callers key cap counting, branch ownership, and
// stop-all on (design §4d). The bare legacy name "lab-login" (written by
// pre-per-provider binaries) still matches, so a leftover login session
// from an older build isn't accidentally treated as a normal instance
// session.
func IsLoginSession(name string) bool {
	return name == LoginSessionPrefix || strings.HasPrefix(name, LoginSessionPrefix+"-")
}

// ErrSessionNotFound wraps a send/paste against a session that no longer
// exists (killed externally, or the whole server is gone) — a state callers
// can surface as "the instance's session is gone" rather than an opaque
// internal error. Detected from tmux's stderr; errors.Is-matchable through
// the wrapped returns below.
var ErrSessionNotFound = errors.New("tmux session not found")

// targetErr wraps a failed targeted tmux call, classifying a missing
// session/pane (or a dead server) as ErrSessionNotFound. tmux 3.x spells
// these "can't find session/pane/window …" and "no server running on …".
func targetErr(op string, err error, out []byte) error {
	msg := strings.TrimSpace(string(out))
	if strings.Contains(msg, "can't find") || strings.Contains(msg, "no server running") {
		return fmt.Errorf("%s: %w: %s", op, ErrSessionNotFound, msg)
	}
	return fmt.Errorf("%s: %v: %s", op, err, msg)
}

// startRecheckDelay is the pause between `new-session` and the
// exited-immediately re-check in Start (v0-pinned 500 ms): long enough
// for a command that can't even exec to die, short enough not to matter
// on the spawn path.
const startRecheckDelay = 500 * time.Millisecond

// SessionRunner is the seam over tmux (design §4d). The real
// implementation is Tmux; Fake is the behavioral test double.
type SessionRunner interface {
	// Start launches argv detached in dir under the session name, with
	// extraEnv ("KEY=VALUE" entries) added to the pane's environment and
	// the configured prlimit nofile cap wrapping the inner command.
	// Idempotent: an already-live name returns nil.
	Start(ctx context.Context, name, dir string, argv []string, extraEnv []string) error
	// Stop kills the session if it exists. Idempotent.
	Stop(ctx context.Context, name string) error
	// IsRunning reports whether the exact session name is live.
	IsRunning(ctx context.Context, name string) (bool, error)
	// List returns the live session names; a missing tmux server means
	// no sessions, not an error.
	List(ctx context.Context) ([]string, error)
	// SendKeys delivers text to the session as literal keystrokes, then —
	// when enter is true — a separate Enter keypress that submits the line.
	SendKeys(ctx context.Context, name, text string, enter bool) error
	// SendNamedKeys delivers tmux key names ("Escape", "Down", "Space",
	// "Enter", …) in one non-literal send-keys call, in order — the chat
	// surface's dialog/interrupt recipes (ADR-0016).
	SendNamedKeys(ctx context.Context, name string, keys ...string) error
	// PasteText delivers text to the session as one bracketed paste (tmux
	// load-buffer + paste-buffer -p): a TUI in bracketed-paste mode receives
	// embedded newlines as inserted lines instead of submissions. Submitting
	// is the caller's separate Enter.
	PasteText(ctx context.Context, name, text string) error
	// CapturePane returns the session's pane content, wrapped lines
	// joined, full scrollback included.
	CapturePane(ctx context.Context, name string) (string, error)
}

// Tmux is the real SessionRunner, shelling out to the tmux binary. The
// zero value is not usable; construct with New.
type Tmux struct {
	bin string // tmux binary path, resolved against the service PATH

	// socket, when non-empty, adds `-L <socket>` to every tmux call so
	// tests run against a private server instead of the user's.
	socket string

	// prlimitBin + nofile configure the RLIMIT_NOFILE cap every spawned
	// session is pinned to, so one agent's descriptor leak hits its own
	// EMFILE instead of driving the whole VM to system-wide ENFILE.
	// nofile 0 (the zero value) spawns bare, leaving tests and minimal
	// configs unaffected (v0 `-session-nofile`, 0 disables).
	prlimitBin string
	nofile     int
}

var _ SessionRunner = (*Tmux)(nil)

// Option configures a Tmux.
type Option func(*Tmux)

// WithSocket runs every tmux call against the named private server
// socket (`tmux -L <name>`). Test-only in practice.
func WithSocket(name string) Option {
	return func(t *Tmux) { t.socket = name }
}

// WithNofileCap pins every spawned session's soft AND hard RLIMIT_NOFILE
// to nofile by wrapping the inner pane command in
// `<prlimitBin> --nofile=<n>:<n> --`. nofile <= 0 disables the cap.
func WithNofileCap(prlimitBin string, nofile int) Option {
	return func(t *Tmux) { t.prlimitBin = prlimitBin; t.nofile = nofile }
}

// New returns a Tmux shelling out to the given tmux binary (usually just
// "tmux", resolved via PATH).
func New(bin string, opts ...Option) *Tmux {
	t := &Tmux{bin: bin}
	for _, o := range opts {
		o(t)
	}
	return t
}

// baselineVars is the fixed allow-list of process-environment variable names
// a spawned session may inherit from the lab process. PATH/HOME/TERM plus the
// locale (LANG/LANGUAGE/LC_*) are the grilled-spec baseline every pane needs
// to run a shell and render text; TERMINFO_DIRS and LOCALE_ARCHIVE are the
// NixOS mechanisms that make TERM and the locale actually *resolve* (the
// terminfo database and glibc's locale archive live in the store, not under
// /usr/share), so they are part of the baseline's meaning, not extras.
// Everything else in the lab process env — notably EnvironmentFile secrets
// like a LAB_DB DSN — is deliberately absent (issue #204).
var baselineVars = map[string]bool{
	"PATH":           true,
	"HOME":           true,
	"TERM":           true,
	"TERMINFO_DIRS":  true,
	"LANG":           true,
	"LANGUAGE":       true,
	"LOCALE_ARCHIVE": true,
}

// isBaselineVar reports whether name is a baseline pass-through variable: one
// of the fixed allow-list above, or any LC_* locale category (LC_ALL,
// LC_CTYPE, LC_TIME, …).
func isBaselineVar(name string) bool {
	return baselineVars[name] || strings.HasPrefix(name, "LC_")
}

// baselineEnv filters the lab process environment down to the baseline
// allow-list (isBaselineVar), passing through only variables that are
// actually present. It is the environment every tmux client invocation runs
// with — and, because the tmux server started implicitly by the first such
// call seeds its global environment from that client, the environment every
// pane is born into. Computed per call: cheap (a scan of os.Environ), and it
// picks up t.Setenv in tests.
func baselineEnv() []string {
	all := os.Environ()
	env := make([]string, 0, len(baselineVars))
	for _, kv := range all {
		name, _, ok := strings.Cut(kv, "=")
		if ok && isBaselineVar(name) {
			env = append(env, kv)
		}
	}
	return env
}

// cmd builds a tmux invocation, prefixing the private-socket flag when
// configured. Env is pinned to baselineEnv() — not left nil (which would
// inherit os.Environ() wholesale) — so no client call can leak a service
// secret into the tmux server's global environment, and thus into panes.
// gitx sets cmd.Env on its git subprocesses for the same reason; tmuxx
// historically did not, which was the #204 leak.
func (t *Tmux) cmd(ctx context.Context, args ...string) *exec.Cmd {
	if t.socket != "" {
		args = append([]string{"-L", t.socket}, args...)
	}
	c := exec.CommandContext(ctx, t.bin, args...)
	c.Env = baselineEnv()
	return c
}

// Start launches argv detached in dir under name. Idempotent: if a
// session by that name already exists, returns nil. After spawn, waits
// ~500 ms and re-checks — if the session vanished the inner command
// exited immediately, which is surfaced as an error (v0 quick-fail).
func (t *Tmux) Start(ctx context.Context, name, dir string, argv []string, extraEnv []string) error {
	ok, err := t.IsRunning(ctx, name)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	if err := t.scrubGlobalEnv(ctx); err != nil {
		return err
	}

	args := t.newSessionArgs(name, dir, argv, extraEnv)
	if out, err := t.cmd(ctx, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, strings.TrimSpace(string(out)))
	}

	select {
	case <-time.After(startRecheckDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	ok, err = t.IsRunning(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q exited immediately (check `tmux capture-pane -pt %s`)", name, name)
	}
	return nil
}

// scrubGlobalEnv sweeps a non-baseline variable out of an already-running
// tmux server's GLOBAL environment before Start spawns into it. A server born
// under a pre-#204 lab inherited os.Environ() wholesale as its global env, and
// — because that server outlives service restarts (parked sessions keep it
// alive) — a scrubbed client alone cannot fix it: new-session copies the
// server's global env into each pane. So we drop every non-baseline global
// var here. On a post-fix server the global env is already baseline-only, so
// this is one show-environment call that unsets nothing — the cheap happy
// path.
func (t *Tmux) scrubGlobalEnv(ctx context.Context) error {
	out, err := t.cmd(ctx, "show-environment", "-g").Output()
	if err != nil {
		// Same classification List uses for list-sessions: an ExitError means
		// no server is running, so there is no global env to scrub. Anything
		// else (couldn't exec tmux, etc.) is a real failure.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil
		}
		return fmt.Errorf("tmux show-environment: %v", err)
	}

	var unset []string
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "-") {
			continue // tmux marks a variable staged for removal "-NAME"
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			continue // not a NAME=... entry
		}
		// isEnvName screens out continuation lines of a multi-line value, which
		// show-environment prints unindented and so cannot be told apart from a
		// real entry except by shape.
		if !isEnvName(name) || isBaselineVar(name) {
			continue
		}
		unset = append(unset, name)
	}
	if len(unset) == 0 {
		return nil
	}

	// Chain the unsets into one client call: `set-environment -g -u NAME
	// [; set-environment -g -u NAME]...`, ";" a separate argv element.
	var args []string
	for _, name := range unset {
		if len(args) > 0 {
			args = append(args, ";")
		}
		args = append(args, "set-environment", "-g", "-u", name)
	}
	if out, err := t.cmd(ctx, args...).CombinedOutput(); err != nil {
		// An "unknown variable" is a harmless no-op — it can only come from a
		// multi-line value's continuation line that happened to look like an
		// entry above — and is ignored. Any OTHER failure would be a silently
		// unscrubbed secret if swallowed, so it fails Start loudly.
		if msg := strings.TrimSpace(string(out)); !strings.Contains(msg, "unknown variable") {
			return fmt.Errorf("tmux set-environment -g -u: %v: %s", err, msg)
		}
	}
	return nil
}

// isEnvName reports whether s is a syntactically valid POSIX environment
// variable name ([A-Za-z_][A-Za-z0-9_]*). scrubGlobalEnv uses it to reject
// the continuation lines of a multi-line global value, which cannot be real
// variable names, before treating a show-environment line as an entry.
func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// newSessionArgs builds the tmux `new-session` argv that launches argv in
// dir: `new-session -d -s <name> -c <dir> [-e K=V]... [prlimit
// --nofile=<n>:<n> --] <argv...>`. The prlimit wrapper is applied here —
// to the inner command tmux runs inside the pane, not to the tmux call
// itself — so the RLIMIT_NOFILE bound reaches the agent's whole process
// tree and survives `new-session -d` daemonizing the pane under the
// long-lived shared server (v0-pinned).
func (t *Tmux) newSessionArgs(name, dir string, argv, extraEnv []string) []string {
	args := []string{"new-session", "-d", "-s", name, "-c", dir}
	for _, kv := range extraEnv {
		args = append(args, "-e", kv)
	}
	args = append(args, t.nofileCapArgv()...)
	return append(args, argv...)
}

// nofileCapArgv returns the prlimit prefix that pins the spawned
// command's soft AND hard RLIMIT_NOFILE to t.nofile, or nil when no cap
// is configured. Both limits are set equal so the process can't raise
// its soft limit back toward the system ceiling; the trailing "--"
// terminates prlimit's own option parsing so the wrapped command's flags
// (e.g. claude's --remote-control) pass through untouched.
func (t *Tmux) nofileCapArgv() []string {
	if t.nofile <= 0 {
		return nil
	}
	return []string{t.prlimitBin, fmt.Sprintf("--nofile=%d:%d", t.nofile, t.nofile), "--"}
}

// Stop kills the session if it exists. Idempotent.
func (t *Tmux) Stop(ctx context.Context, name string) error {
	ok, err := t.IsRunning(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if out, err := t.cmd(ctx, "kill-session", "-t", "="+name).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux kill-session: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsRunning reports whether the exact session name is live. "=NAME"
// forces exact match — without it tmux does prefix matching. An
// exec.ExitError means the session (or the whole server) is absent.
func (t *Tmux) IsRunning(ctx context.Context, name string) (bool, error) {
	err := t.cmd(ctx, "has-session", "-t", "="+name).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session: %v", err)
}

// List returns the live session names. tmux exits 1 when no server is
// running — that's "no sessions", not an error.
func (t *Tmux) List(ctx context.Context) ([]string, error) {
	out, err := t.cmd(ctx, "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-sessions: %v", err)
	}
	var names []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// SendKeys delivers text to the session as literal keystrokes, then —
// when enter is true — a separate Enter keypress. The -l flag forces
// literal interpretation, so a space or the word "Enter" inside text
// isn't parsed as a tmux key name; the second call (without -l) sends
// the actual Return that submits the line. Bare session name (no "="):
// send-keys' target-pane parser rejects the exact-match prefix (verified
// on tmux 3.6a) — uniqueness relies on sanitised session names.
func (t *Tmux) SendKeys(ctx context.Context, name, text string, enter bool) error {
	if out, err := t.cmd(ctx, "send-keys", "-t", name, "-l", "--", text).CombinedOutput(); err != nil {
		return targetErr("tmux send-keys (literal)", err, out)
	}
	if !enter {
		return nil
	}
	if out, err := t.cmd(ctx, "send-keys", "-t", name, "--", "Enter").CombinedOutput(); err != nil {
		return targetErr("tmux send-keys (enter)", err, out)
	}
	return nil
}

// SendNamedKeys delivers key names in one non-literal send-keys call —
// tmux parses each argument as a key ("Escape", "Down", "Space", "Enter"),
// exactly the second-call semantics of SendKeys' Enter, generalized. Bare
// session name for the same target-pane parser reason as SendKeys.
func (t *Tmux) SendNamedKeys(ctx context.Context, name string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	args := append([]string{"send-keys", "-t", name, "--"}, keys...)
	if out, err := t.cmd(ctx, args...).CombinedOutput(); err != nil {
		return targetErr("tmux send-keys (named)", err, out)
	}
	return nil
}

// pasteSeq disambiguates concurrent PasteText buffer names on the shared
// tmux server; the buffer exists only between load-buffer and the -d
// deleting paste.
var pasteSeq atomic.Int64

// PasteText delivers text as one bracketed paste: load-buffer from stdin
// into a uniquely named buffer, then paste-buffer -p (bracket codes, when
// the pane's application requested bracketed-paste mode) -d (delete the
// buffer after). Bare session name, as with SendKeys.
func (t *Tmux) PasteText(ctx context.Context, name, text string) error {
	buf := fmt.Sprintf("lab-paste-%d", pasteSeq.Add(1))
	load := t.cmd(ctx, "load-buffer", "-b", buf, "-")
	load.Stdin = strings.NewReader(text)
	if out, err := load.CombinedOutput(); err != nil {
		return targetErr("tmux load-buffer", err, out)
	}
	if out, err := t.cmd(ctx, "paste-buffer", "-p", "-d", "-b", buf, "-t", name).CombinedOutput(); err != nil {
		// -d deletes only on a successful paste; reap the buffer ourselves so
		// a dead target doesn't leak it on the shared server.
		_ = t.cmd(ctx, "delete-buffer", "-b", buf).Run()
		return targetErr("tmux paste-buffer", err, out)
	}
	return nil
}

// CapturePane returns the session's pane content in one non-blocking
// pass. -J joins wrapped lines (URLs split across terminal-width breaks
// rejoin); -S - reads the full scrollback so text printed at session
// start stays findable. Bare session name — capture-pane's target-pane
// parser also rejects "=name" (tmux 3.6a). Poll loops over this (the
// provider's OAuth-URL capture) live in the caller.
func (t *Tmux) CapturePane(ctx context.Context, name string) (string, error) {
	out, err := t.cmd(ctx, "capture-pane", "-p", "-J", "-S", "-", "-t", name).Output()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %v", err)
	}
	return string(out), nil
}
