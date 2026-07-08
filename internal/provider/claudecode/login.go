package claudecode

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// maxLoginCodeLen caps a pasted OAuth code before it reaches tmux
// send-keys (v0-pinned: stops a runaway paste).
const maxLoginCodeLen = 4096

// ErrInvalidCode / ErrLoginTimeout are thin aliases of the provider-generic
// sentinels (issue #51 decision 7 — httpapi maps their status codes without
// importing claudecode); the v0 message texts moved with them. See the
// provider package for the contract docs.
var (
	ErrInvalidCode  = provider.ErrInvalidCode
	ErrLoginTimeout = provider.ErrLoginTimeout
)

// claudeOAuthURLRe matches the authorize link `claude auth login` prints.
// Verified on claude 2.1.150 (re-pinned in internal/compat): the real link
// is served from claude.com under a /cai/ prefix —
// https://claude.com/cai/oauth/authorize?... — NOT claude.ai/oauth. So the
// host is pinned to claude.(com|ai) and an arbitrary path prefix before
// "oauth/" is allowed. The greedy \S* still anchors on the single literal
// "oauth/" segment: the encoded redirect_uri ("%2Foauth%2F…") contains no
// literal "/oauth/", so it cannot be mistaken for the real one.
var claudeOAuthURLRe = regexp.MustCompile(`https://claude\.(?:com|ai)/\S*oauth/\S+`)

// ExtractOAuthURL returns the first OAuth authorize link in s, with
// trailing sentence punctuation trimmed (claude or surrounding prose may
// end the URL line with ".", ",", a closing paren, …). Exported for the
// compat regex test.
func ExtractOAuthURL(s string) string {
	m := claudeOAuthURLRe.FindString(s)
	return strings.TrimRight(m, `.,;:!?'")>]`)
}

// validateLoginCode trims and sanity-checks a pasted OAuth code before it
// is handed to tmux send-keys. The real value is a URL-safe code#state
// string, so "#", "=", and base64url characters must pass; only empty
// input, control characters (which could inject a premature Enter), and
// absurd lengths are rejected.
func validateLoginCode(raw string) (string, error) {
	code := strings.TrimSpace(raw)
	if code == "" {
		return "", errors.New("empty code")
	}
	if len(code) > maxLoginCodeLen {
		return "", errors.New("code too long")
	}
	for _, r := range code {
		if unicode.IsControl(r) {
			return "", errors.New("code contains a control character")
		}
	}
	return code, nil
}

// loginArgv is the pinned login flow command; --claudeai skips the
// subscription-vs-console picker. --model/--effort/--permission-mode do
// NOT apply to login (those are remote-control flags).
func (p *Provider) loginArgv() []string {
	return []string{p.claudeBin, "auth", "login", "--claudeai"}
}

// LoginStart implements provider.AgentProvider. It starts the global login
// flow in the fixed LoginSession tmux session (cwd $HOME — login is
// global, one machine-level credential) and synchronously scrapes the
// OAuth authorize URL from the pane (poll every 200ms up to
// captureTimeout).
//
// Idempotent while an attempt is live (pinned M3 contract): a LoginStart
// with the login session already running never double-spawns — it returns
// the remembered URL, or re-scrapes the live pane when the in-memory copy
// is gone (lab restarted mid-login; the tmux session survived).
//
// A scrape miss is not an error: it returns "" with a loud warning, and a
// later LoginStart re-scrapes.
func (p *Provider) LoginStart(ctx context.Context) (string, error) {
	running, err := p.runner.IsRunning(ctx, LoginSession)
	if err != nil {
		return "", fmt.Errorf("login session check: %w", err)
	}
	if running {
		if url := p.getLoginURL(); url != "" {
			return url, nil
		}
	} else {
		p.setLoginURL("")
		if err := p.runner.Start(ctx, LoginSession, p.loginDir, p.loginArgv(), nil); err != nil {
			return "", fmt.Errorf("start login session: %w", err)
		}
	}
	url := p.captureOAuthURL(ctx, p.captureTimeout)
	if url == "" {
		p.log.Warn("oauth url not found in login pane — retry login start to re-scrape",
			"component", "provider.claudecode", "session", LoginSession, "timeout", p.captureTimeout)
		return "", nil
	}
	p.setLoginURL(url)
	return url, nil
}

// LoginSubmitCode implements provider.AgentProvider: validate the pasted
// code, deliver it to the waiting login session as literal keystrokes plus
// a separate Enter, then poll auth status (force-refreshing past the
// cache) every loginPoll up to loginTimeout.
//
// A bad code never tears the session down — the login session is still
// waiting, so the user just retries. On success the login session is
// killed (idempotent; `claude auth login` usually already exited) and
// provider.auth.changed is published. On timeout the stuck attempt is torn
// down and ErrLoginTimeout returned.
func (p *Provider) LoginSubmitCode(ctx context.Context, raw string) error {
	code, err := validateLoginCode(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCode, err)
	}
	if err := p.runner.SendKeys(ctx, LoginSession, code, true); err != nil {
		return fmt.Errorf("send login code: %w", err)
	}
	deadline := time.Now().Add(p.loginTimeout)
	for {
		st, _ := p.AuthStatus(ctx, true) // error ⇒ treated as logged out
		if st.LoggedIn {
			if err := p.runner.Stop(ctx, LoginSession); err != nil {
				p.log.Warn("stop login session after login",
					"component", "provider.claudecode", "session", LoginSession, "err", err)
			}
			p.setLoginURL("")
			p.publishAuthChanged()
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		if !sleepOrDone(ctx, p.loginPoll) {
			return ctx.Err()
		}
	}
	// Timed out: tear down the stuck attempt and hint the user to retry.
	if err := p.runner.Stop(ctx, LoginSession); err != nil {
		p.log.Warn("stop login session after login timeout",
			"component", "provider.claudecode", "session", LoginSession, "err", err)
	}
	p.setLoginURL("")
	return ErrLoginTimeout
}

// captureOAuthURL polls the login session's pane every 200ms up to timeout
// for the authorize link, returning "" if none appears in time. Pane
// capture errors are quiet misses (v0 scrapeOnce). The check runs before
// the deadline test, so at least one pass always happens.
func (p *Provider) captureOAuthURL(ctx context.Context, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		if pane, err := p.runner.CapturePane(ctx, LoginSession); err == nil {
			if url := ExtractOAuthURL(pane); url != "" {
				return url
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		if !sleepOrDone(ctx, pollInterval) {
			return ""
		}
	}
}

func (p *Provider) getLoginURL() string {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	return p.loginURL
}

func (p *Provider) setLoginURL(url string) {
	p.loginMu.Lock()
	defer p.loginMu.Unlock()
	p.loginURL = url
}
