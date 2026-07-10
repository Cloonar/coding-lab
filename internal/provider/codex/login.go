package codex

// Device-code login flow (issue #87). `codex login --device-auth` prints a
// verification URL and a one-time user code; the operator opens the URL in
// ANY browser, signs in, and types the code there — the code never travels
// back into lab, so LoginSubmitCode is a hard ErrLoginCodeUnsupported.
// Completion is provider-driven: a single background poller watches the
// forced auth status until the CLI records the login (or the 15-minute
// device-code window expires) and publishes provider.auth.changed.
//
// Pinned pane output (verbatim, live 0.133.0 probe 2026-07-10):
//
//	Follow these steps to sign in with ChatGPT using device code authorization:
//
//	1. Open this link in your browser and sign in to your account
//	   https://auth.openai.com/codex/device
//
//	2. Enter this one-time code (expires in 15 minutes)
//	   Y9HC-QKI85
//
//	Device codes are a common phishing target. Never share this code.
//
// NEVER emit Ctrl-C at the login session (upstream: Ctrl-C on codex quits
// immediately) — the session is disposable, runner.Stop kills it, which is
// fine.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// codexLoginURLRe matches the device-auth verification link the login flow
// prints (pinned host auth.openai.com; live 0.133.0 prints
// https://auth.openai.com/codex/device).
var codexLoginURLRe = regexp.MustCompile(`https://auth\.openai\.com/\S+`)

// codexUserCodeRe matches the standalone one-time user code (observed shape
// Y9HC-QKI85: four uppercase alphanumerics, a dash, five to eight more). The
// boundaries exclude alphanumerics AND '-', so version strings, timestamps,
// and longer dashed tokens never match.
var codexUserCodeRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9-])([A-Z0-9]{4}-[A-Z0-9]{5,8})(?:[^A-Za-z0-9-]|$)`)

// ExtractLoginURL returns the first device-auth verification link in s, with
// trailing sentence punctuation trimmed (claudecode's pinned cutset).
// Exported for the compat regex test.
func ExtractLoginURL(s string) string {
	m := codexLoginURLRe.FindString(s)
	return strings.TrimRight(m, `.,;:!?'")>]`)
}

// ExtractUserCode returns the first standalone one-time user code in s, or
// "". Exported for the compat regex test.
func ExtractUserCode(s string) string {
	m := codexUserCodeRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

// loginArgv is the pinned device-code login command. --device-auth selects
// the browserless flow — the plain `codex login` spins up a localhost
// redirect server no phone-first operator can reach.
func (p *Provider) loginArgv() []string {
	return []string{p.codexBin, "login", "--device-auth"}
}

// LoginStart implements provider.AgentProvider. It starts the global
// device-code login in this provider's loginSession tmux session (cwd $HOME
// — login is global, one machine-level credential) and synchronously
// scrapes the verification URL + one-time user code from the pane (poll
// every 200ms up to captureTimeout; CapturePane returns rendered text, no
// ANSI codes).
//
// Idempotent while an attempt is live: a LoginStart with the login session
// already running never double-spawns — it returns the remembered URL, or
// re-scrapes the live pane when the in-memory copy is gone (lab restarted
// mid-login; the tmux session survived). Every LoginStart also ensures the
// single background completion poller is running, so a restarted lab picks
// the pending attempt's completion watch back up.
//
// A scrape miss is not an error: it returns "" with a loud warning, and a
// later LoginStart re-scrapes.
func (p *Provider) LoginStart(ctx context.Context) (string, error) {
	running, err := p.runner.IsRunning(ctx, loginSession)
	if err != nil {
		return "", fmt.Errorf("login session check: %w", err)
	}
	if running {
		if url := p.getLoginURL(); url != "" {
			p.ensureCompletionPoller()
			return url, nil
		}
	} else {
		p.setLoginState("", "")
		if err := p.runner.Start(ctx, loginSession, p.loginDir, p.loginArgv(), nil); err != nil {
			return "", fmt.Errorf("start login session: %w", err)
		}
	}
	url, code := p.captureLogin(ctx, p.captureTimeout)
	if url != "" || code != "" {
		p.setLoginState(url, code)
	}
	p.ensureCompletionPoller()
	if url == "" {
		p.log.Warn("device-auth url not found in login pane — retry login start to re-scrape",
			"component", "provider.codex", "session", loginSession, "timeout", p.captureTimeout)
		return "", nil
	}
	return url, nil
}

// LoginSubmitCode implements provider.AgentProvider: the device-code flow
// takes NO pasted code — the operator enters the one-time code browser-side
// at the verification URL (issue #51 decision 7's device-code contract).
func (p *Provider) LoginSubmitCode(context.Context, string) error {
	return provider.ErrLoginCodeUnsupported
}

// PendingLoginCode implements provider.LoginCodeReporter: the one-time user
// code of the pending login attempt, "" when none is pending.
func (p *Provider) PendingLoginCode() string {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	return p.loginCode
}

// captureLogin polls the login session's pane every 200ms up to timeout for
// the verification URL and the one-time user code, returning early once both
// are found and whatever it has at the deadline otherwise. Pane capture
// errors are quiet misses. The check runs before the deadline test, so at
// least one pass always happens.
func (p *Provider) captureLogin(ctx context.Context, timeout time.Duration) (url, code string) {
	deadline := time.Now().Add(timeout)
	for {
		if pane, err := p.runner.CapturePane(ctx, loginSession); err == nil {
			if u := ExtractLoginURL(pane); u != "" {
				url = u
			}
			if c := ExtractUserCode(pane); c != "" {
				code = c
			}
			if url != "" && code != "" {
				return url, code
			}
		}
		if time.Now().After(deadline) {
			return url, code
		}
		if !sleepOrDone(ctx, pollInterval) {
			return url, code
		}
	}
}

// ensureCompletionPoller starts the background completion poller unless one
// is already live (mutex + bool guard — a SINGLE poller per process, however
// many LoginStarts arrive). Reports whether this call started it, for tests.
func (p *Provider) ensureCompletionPoller() bool {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	if p.pollerLive {
		return false
	}
	p.pollerLive = true
	go p.pollLoginCompletion()
	return true
}

// pollLoginCompletion watches the forced auth status every loginPoll until
// the login lands or the device-code window (loginWindow, 15 minutes)
// expires. On success: stop the login session (idempotent — the CLI usually
// already exited), clear the pending URL/code, publish
// provider.auth.changed. On expiry: stop the session, clear state, warn.
// Detached from any request context — the operator's browser dance outlives
// every HTTP request.
func (p *Provider) pollLoginCompletion() {
	ctx, cancel := context.WithTimeout(context.Background(), p.loginWindow)
	defer cancel()
	defer func() {
		p.loginMu.Lock()
		p.pollerLive = false
		p.loginMu.Unlock()
	}()
	for {
		st, _ := p.AuthStatus(ctx, true) // error ⇒ treated as logged out
		if st.LoggedIn {
			p.teardownLogin(context.Background())
			p.publishAuthChanged()
			return
		}
		if !sleepOrDone(ctx, p.loginPoll) {
			// Window expired (or process shutdown): the device code is dead,
			// tear the disposable attempt down so the next LoginStart is fresh.
			p.teardownLogin(context.Background())
			p.log.Warn("device-code login window expired without a completed login",
				"component", "provider.codex", "session", loginSession, "window", p.loginWindow)
			return
		}
	}
}

// teardownLogin stops the login session (never Ctrl-C — Stop kills it) and
// clears the pending URL/code.
func (p *Provider) teardownLogin(ctx context.Context) {
	if err := p.runner.Stop(ctx, loginSession); err != nil {
		p.log.Warn("stop login session",
			"component", "provider.codex", "session", loginSession, "err", err)
	}
	p.setLoginState("", "")
}

func (p *Provider) getLoginURL() string {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	return p.loginURL
}

func (p *Provider) setLoginState(url, code string) {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	p.loginURL = url
	p.loginCode = code
}
