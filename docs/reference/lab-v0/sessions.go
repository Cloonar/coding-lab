package main

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Sessions wraps the subset of tmux behaviour lab needs: starting a detached
// session running an arbitrary command in a working dir, stopping it, asking
// whether it exists, listing live names, sending keystrokes to it, and scraping
// the pane for the login flow's OAuth link. (The remote-control deep link is
// NOT scraped from the pane anymore — claude stopped printing it; see
// registry.go.) tmux is the source of truth for liveness; no other state is
// held in this struct. URL persistence lives in Store, driven by the caller.
type Sessions struct {
	tmuxBin string
	// startCmd is the per-project command run inside a new session, as argv,
	// WITHOUT the --model/--effort flags. Any "%s" element is replaced by the
	// session name (so the caller can pass e.g. {"claude", "--remote-control",
	// "%s"}). The model + effort are appended fresh per spawn from spawnConfig (see
	// baseStartArgv / modelEffortArgv), not baked in here.
	startCmd []string
	// spawnConfig resolves the global model + effort for a new spawn. It is read
	// FRESH at each spawn (not at construction) so a change to the persisted
	// setting takes effect on the next session with no lab restart (#156). main
	// wires it to Store.SpawnConfig; a nil accessor (tests, minimal configs) falls
	// back to the documented defaults in modelEffortArgv. Both spawn paths — Start
	// (manual) and baseStartArgv (the AFK base argv) — go through it, so AFK is
	// covered for free and the two paths can't drift.
	spawnConfig func() (model, effort string)
	// prlimitBin is the prlimit(1) binary (util-linux) used to bound a spawned
	// session's RLIMIT_NOFILE. Only consulted when nofile > 0; resolved against
	// the service PATH like tmuxBin. Set by main from -prlimit.
	prlimitBin string
	// nofile, when > 0, is the soft+hard RLIMIT_NOFILE every spawned session is
	// pinned to, so one agent's descriptor leak hits its own EMFILE instead of
	// driving the whole VM to system-wide ENFILE. 0 (the zero value) spawns bare,
	// leaving tests and minimal configs unaffected. Set by main from
	// -session-nofile.
	nofile int
}

func NewSessions(tmuxBin string, startCmd []string) *Sessions {
	return &Sessions{tmuxBin: tmuxBin, startCmd: startCmd}
}

// Start launches the configured per-project command in a detached session named
// `name` running in dir, with the current global --model/--effort appended. Thin
// wrapper over StartCommand via baseStartArgv, so a manual Start and an AFK run
// resolve the spawn config through the one shared path.
func (s *Sessions) Start(name, dir string) error {
	return s.StartCommand(name, dir, s.baseStartArgv())
}

// baseStartArgv returns a fresh copy of the per-project start command with the
// resolved --effort/--model flags appended. Callers append further trailing
// arguments — e.g. an AFK run's seed prompt — AFTER the flags, so the prompt
// stays claude's positional argument; the fresh copy keeps the shared startCmd
// from being mutated by an append that reuses its backing array. The model +
// effort are read here, at spawn time, from spawnConfig — so every spawn (manual
// or AFK) honours the current global setting.
func (s *Sessions) baseStartArgv() []string {
	argv := append([]string(nil), s.startCmd...)
	return append(argv, s.modelEffortArgv()...)
}

// modelEffortArgv resolves the per-spawn --effort/--model flags from the injected
// spawnConfig accessor, or the documented defaults when it is unwired (nil). Read
// at spawn time, not construction, so a change to the persisted setting lands on
// the next spawn.
func (s *Sessions) modelEffortArgv() []string {
	model, effort := defaultSpawnModel, defaultSpawnEffort
	if s.spawnConfig != nil {
		model, effort = s.spawnConfig()
	}
	return []string{"--effort", effort, "--model", model}
}

// StartCommand launches a detached session named `name` running argv in dir.
// Any "%s" element of argv is replaced by name. Idempotent: if a session by
// that name already exists, returns nil. After spawn, waits ~500 ms and
// re-checks — if the session vanished the inner command exited immediately,
// which is surfaced as an error.
func (s *Sessions) StartCommand(name, dir string, argv []string) error {
	ok, err := s.IsRunning(name)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	args := s.newSessionArgs(name, dir, argv)
	if out, err := exec.Command(s.tmuxBin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, strings.TrimSpace(string(out)))
	}

	time.Sleep(500 * time.Millisecond)
	ok, err = s.IsRunning(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session %q exited immediately (check `tmux capture-pane -pt %s`)", name, name)
	}
	return nil
}

// newSessionArgs builds the tmux `new-session` argv that launches argv in dir,
// with each "%s" element replaced by name. When a descriptor cap is configured
// (nofile > 0) the substituted command is prefixed with the prlimit wrapper
// (see nofileCapArgv). The wrapper is applied here — to the inner command tmux
// runs inside the pane, not to the tmux call itself — so the RLIMIT_NOFILE
// bound reaches the agent's whole process tree and survives `new-session -d`
// daemonizing the pane under the long-lived shared server.
func (s *Sessions) newSessionArgs(name, dir string, argv []string) []string {
	cmd := make([]string, len(argv))
	for i, a := range argv {
		cmd[i] = strings.ReplaceAll(a, "%s", name)
	}
	cmd = append(s.nofileCapArgv(), cmd...)
	return append([]string{"new-session", "-d", "-s", name, "-c", dir}, cmd...)
}

// nofileCapArgv returns the prlimit prefix that pins the spawned command's soft
// AND hard RLIMIT_NOFILE to s.nofile, or nil when no cap is configured. Both
// limits are set equal so the process can't raise its soft limit back toward the
// system ceiling. The trailing "--" terminates prlimit's own option parsing so
// the wrapped command's flags (e.g. claude's --remote-control) pass through
// untouched.
func (s *Sessions) nofileCapArgv() []string {
	if s.nofile <= 0 {
		return nil
	}
	return []string{s.prlimitBin, fmt.Sprintf("--nofile=%d:%d", s.nofile, s.nofile), "--"}
}

// Stop kills the session if it exists. Idempotent.
func (s *Sessions) Stop(name string) error {
	ok, err := s.IsRunning(name)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if out, err := exec.Command(s.tmuxBin, "kill-session", "-t", "="+name).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux kill-session: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Sessions) IsRunning(name string) (bool, error) {
	// "=NAME" forces exact match — without it tmux does prefix matching.
	err := exec.Command(s.tmuxBin, "has-session", "-t", "="+name).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session: %v", err)
}

func (s *Sessions) List() ([]string, error) {
	out, err := exec.Command(s.tmuxBin, "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		// tmux exits 1 when no server is running — that's "no sessions".
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

// SendKeys delivers text to the session as literal keystrokes, then a separate
// Enter keypress. The -l flag forces literal interpretation, so a space or the
// word "Enter" inside text isn't parsed as a tmux key name; the second call
// (without -l) sends the actual Return that submits the line. Bare session name
// for the same reason as captureURL — send-keys' target-pane parser rejects the
// "=name" exact-match prefix (verified on tmux 3.6a).
func (s *Sessions) SendKeys(name, text string) error {
	if out, err := exec.Command(s.tmuxBin, "send-keys", "-t", name, "-l", "--", text).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys (literal): %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(s.tmuxBin, "send-keys", "-t", name, "--", "Enter").CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys (enter): %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CaptureOAuthURL polls the session's pane up to timeout for the
// claude.ai/oauth/ authorize link printed by `claude auth login`.
func (s *Sessions) CaptureOAuthURL(name string, timeout time.Duration) string {
	return s.captureURL(name, timeout, extractOAuthURL)
}

// ScrapeOAuthURL does a single, non-blocking capture pass for the OAuth link.
// Used on render to recover the URL from the live pane after lab restarts
// mid-login (the in-memory copy is gone but the tmux session survives).
func (s *Sessions) ScrapeOAuthURL(name string) string {
	return s.scrapeOnce(name, extractOAuthURL)
}

// captureURL polls the session's pane up to timeout, returning the first URL
// `extract` matches, or "" if none appears in time. -J joins wrapped lines
// (URLs split across terminal-width breaks rejoin); -S - reads the full
// scrollback so URLs printed at session start are findable later. The "=name"
// exact-match prefix is NOT supported by capture-pane's target-pane parser
// (unlike has-session's target-session — verified on tmux 3.6a), so we pass the
// bare session name and rely on sanitised, unique session names to avoid
// collisions.
func (s *Sessions) captureURL(name string, timeout time.Duration, extract func(string) string) string {
	deadline := time.Now().Add(timeout)
	for {
		if url := s.scrapeOnce(name, extract); url != "" {
			return url
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *Sessions) scrapeOnce(name string, extract func(string) string) string {
	out, err := exec.Command(s.tmuxBin, "capture-pane", "-p", "-J", "-S", "-", "-t", name).Output()
	if err != nil {
		return ""
	}
	return extract(string(out))
}

// Permissive on path/query characters; trailing sentence punctuation is trimmed
// below since claude (or surrounding prose) may end the URL line with ".", ",",
// etc.
//
// The OAuth authorize link from `claude auth login` (verified on claude 2.1.150)
// is served from claude.com under a /cai/ prefix, i.e.
// https://claude.com/cai/oauth/authorize?... — NOT claude.ai/oauth. So the host
// is pinned to claude.(com|ai) and an arbitrary path prefix before "oauth/" is
// allowed. The greedy \S* before "oauth/" still anchors on the single literal
// "oauth/" segment: the encoded redirect_uri ("%2Foauth%2F…") contains no
// literal "/oauth/", so it can't be mistaken for the real one.
var claudeOAuthURLRe = regexp.MustCompile(`https://claude\.(?:com|ai)/\S*oauth/\S+`)

func extractOAuthURL(s string) string { return extractURL(claudeOAuthURLRe, s) }

func extractURL(re *regexp.Regexp, s string) string {
	m := re.FindString(s)
	return strings.TrimRight(m, ".,;:!?'\")>]")
}
