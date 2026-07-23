package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// devicePane is the pinned real login pane (verbatim, live 0.133.0 probe
// 2026-07-10) the scrape tests run against.
const devicePane = `Follow these steps to sign in with ChatGPT using device code authorization:

1. Open this link in your browser and sign in to your account
   https://auth.openai.com/codex/device

2. Enter this one-time code (expires in 15 minutes)
   Y9HC-QKI85

Device codes are a common phishing target. Never share this code.
`

// loginRunner wraps the shared tmuxx.Fake with a fresh-start counter (the
// Fake's Start is idempotent, so double-spawns would be invisible).
type loginRunner struct {
	*tmuxx.Fake

	mu     sync.Mutex
	starts int
}

func newLoginRunner() *loginRunner { return &loginRunner{Fake: tmuxx.NewFake()} }

func (r *loginRunner) Start(ctx context.Context, name, dir string, argv []string, extraEnv []string, opts ...tmuxx.StartOpt) error {
	live, err := r.IsRunning(ctx, name)
	if err != nil {
		return err
	}
	if err := r.Fake.Start(ctx, name, dir, argv, extraEnv, opts...); err != nil {
		return err
	}
	if !live {
		r.mu.Lock()
		r.starts++
		r.mu.Unlock()
	}
	return nil
}

func (r *loginRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

func TestExtractLoginURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{devicePane, "https://auth.openai.com/codex/device"},
		{"visit https://auth.openai.com/codex/device.", "https://auth.openai.com/codex/device"},
		{"(https://auth.openai.com/codex/device)", "https://auth.openai.com/codex/device"},
		{"https://chatgpt.com/codex/device", ""}, // wrong host — the pin is auth.openai.com
		{"no url here", ""},
	} {
		if got := ExtractLoginURL(tc.in); got != tc.want {
			t.Errorf("ExtractLoginURL(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractUserCode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{devicePane, "Y9HC-QKI85"},
		{"code: ABCD-EFGHI", "ABCD-EFGHI"},
		{"  Z1X2-A2B3C4D5\n", "Z1X2-A2B3C4D5"},
		// Non-alphanumeric boundaries: version strings, timestamps, and
		// dash-joined tokens must never match.
		{"codex-cli 0.133.0", ""},
		{"rollout-2026-07-10T03-18-27-019f499a.jsonl", ""},
		{"v2.1.198-BETA", ""},
		{"ABCD-EFGHI-JKLMN", ""}, // longer dashed token, not a standalone code
		{"abcd-efghi", ""},       // lowercase is not a code
		{"ABC-DEFGH", ""},        // first group too short
		{"ABCD-EFG", ""},         // second group too short
	} {
		if got := ExtractUserCode(tc.in); got != tc.want {
			t.Errorf("ExtractUserCode(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// LoginStart spawns `codex login --device-auth` in the login dir as the
// lab-login-codex session and synchronously scrapes the verification URL and
// the one-time user code from the pane; the code is surfaced through
// PendingLoginCode (provider.LoginCodeReporter), never submitted back.
func TestLoginStart_capturesURLAndCode(t *testing.T) {
	run := newLoginRunner()
	run.SetPane(loginSession, devicePane)
	p, _ := testProvider(t, run)
	// Status stays logged out so the completion poller never tears the
	// session down mid-assertion.
	p.codexBin = fakeCodex(t, `echo 'Not logged in'; exit 1`)

	url, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if want := "https://auth.openai.com/codex/device"; url != want {
		t.Errorf("LoginStart url = %q; want %q", url, want)
	}
	if code := p.PendingLoginCode(); code != "Y9HC-QKI85" {
		t.Errorf("PendingLoginCode() = %q; want Y9HC-QKI85", code)
	}
	sess, live := run.Session(loginSession)
	if !live {
		t.Fatal("login session not running after LoginStart")
	}
	if sess.Dir != p.loginDir {
		t.Errorf("login session dir = %q; want loginDir %q", sess.Dir, p.loginDir)
	}
	wantArgv := strings.Join([]string{p.codexBin, "login", "--device-auth"}, " ")
	if got := strings.Join(sess.Argv, " "); got != wantArgv {
		t.Errorf("login argv = %q; want %q", got, wantArgv)
	}
}

// LoginStart while the login session is live re-serves the remembered URL
// instead of double-spawning.
func TestLoginStart_idempotentWhileLive(t *testing.T) {
	run := newLoginRunner()
	run.SetPane(loginSession, devicePane)
	p, _ := testProvider(t, run)
	p.codexBin = fakeCodex(t, `echo 'Not logged in'; exit 1`)
	ctx := context.Background()

	first, err := p.LoginStart(ctx)
	if err != nil {
		t.Fatalf("LoginStart #1: %v", err)
	}
	second, err := p.LoginStart(ctx)
	if err != nil {
		t.Fatalf("LoginStart #2: %v", err)
	}
	if first != second {
		t.Errorf("second LoginStart url = %q; want the first attempt's %q", second, first)
	}
	if n := run.startCount(); n != 1 {
		t.Errorf("session starts = %d; want 1 (no double-spawn)", n)
	}
}

// Restart recovery: the login session survives a lab restart but the
// in-memory URL/code are gone — LoginStart against the live session
// re-scrapes both from the pane without spawning anything.
func TestLoginStart_recoversFromLivePane(t *testing.T) {
	run := newLoginRunner()
	run.AddLive(loginSession)
	run.SetPane(loginSession, devicePane)
	p, _ := testProvider(t, run)
	p.codexBin = fakeCodex(t, `echo 'Not logged in'; exit 1`)

	url, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if want := "https://auth.openai.com/codex/device"; url != want {
		t.Errorf("LoginStart url = %q; want %q", url, want)
	}
	if code := p.PendingLoginCode(); code != "Y9HC-QKI85" {
		t.Errorf("PendingLoginCode() = %q; want re-scraped Y9HC-QKI85", code)
	}
	if n := run.startCount(); n != 0 {
		t.Errorf("session starts = %d; want 0 (re-joined the live attempt)", n)
	}
}

// A scrape miss is not an error: "" comes back and a later LoginStart
// re-scrapes.
func TestLoginStart_scrapeMissReturnsEmpty(t *testing.T) {
	run := newLoginRunner()
	run.SetPane(loginSession, "codex starting up...")
	p, _ := testProvider(t, run)
	p.codexBin = fakeCodex(t, `echo 'Not logged in'; exit 1`)
	p.captureTimeout = 50 * time.Millisecond

	url, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if url != "" {
		t.Errorf("LoginStart url = %q; want \"\" on a scrape miss", url)
	}
	if code := p.PendingLoginCode(); code != "" {
		t.Errorf("PendingLoginCode() = %q; want \"\" on a scrape miss", code)
	}
}

// The device-code flow takes NO pasted code — the code is entered
// browser-side at the verification URL.
func TestLoginSubmitCode_unsupported(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	err := p.LoginSubmitCode(context.Background(), "Y9HC-QKI85")
	if !errors.Is(err, provider.ErrLoginCodeUnsupported) {
		t.Errorf("LoginSubmitCode err = %v; want ErrLoginCodeUnsupported", err)
	}
}

// The background completion poller watches the forced auth status: when the
// CLI records the login it stops the login session, clears the pending
// URL/code, and publishes provider.auth.changed.
func TestLoginCompletionPoller_flipsOnStatus(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "logged-in")
	run := newLoginRunner()
	run.SetPane(loginSession, devicePane)
	p, bus := testProvider(t, run)
	p.codexBin = fakeCodex(t,
		`if [ -f '`+flag+`' ]; then echo 'Logged in using ChatGPT'; else echo 'Not logged in'; exit 1; fi`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, unsub := bus.Subscribe(ctx)
	defer unsub()

	if _, err := p.LoginStart(ctx); err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	// The operator finishes the browser dance: the CLI records the login.
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-evs:
		if e.Type != provider.EventAuthChanged {
			t.Errorf("event type = %q; want %q", e.Type, provider.EventAuthChanged)
		}
		if pl, ok := e.Payload.(provider.AuthChangedPayload); !ok || pl.Provider != ID {
			t.Errorf("event payload = %#v; want AuthChangedPayload with provider %q", e.Payload, ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no provider.auth.changed event after the login landed")
	}
	if ok, _ := run.IsRunning(ctx, loginSession); ok {
		t.Error("login session still running after completed login; want it stopped")
	}
	if code := p.PendingLoginCode(); code != "" {
		t.Errorf("PendingLoginCode() = %q after completion; want cleared", code)
	}
}

// The 15-minute device-code window (shrunk here) expires without a login:
// the poller tears the disposable session down and clears the pending state.
func TestLoginCompletionPoller_windowExpiryTearsDown(t *testing.T) {
	run := newLoginRunner()
	run.SetPane(loginSession, devicePane)
	p, _ := testProvider(t, run)
	p.codexBin = fakeCodex(t, `echo 'Not logged in'; exit 1`)
	p.loginWindow = 100 * time.Millisecond

	if _, err := p.LoginStart(context.Background()); err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		running, _ := run.IsRunning(context.Background(), loginSession)
		if !running && p.PendingLoginCode() == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("login session running=%v code=%q after window expiry; want torn down and cleared",
				running, p.PendingLoginCode())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The completion poller is a singleton: however many LoginStarts arrive,
// only one goroutine watches the status (mutex + bool guard).
func TestEnsureCompletionPoller_singleton(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	p.codexBin = fakeCodex(t, `echo 'Not logged in'; exit 1`)
	p.loginWindow = 200 * time.Millisecond

	if !p.ensureCompletionPoller() {
		t.Fatal("first ensureCompletionPoller() = false; want it to start the poller")
	}
	if p.ensureCompletionPoller() {
		t.Error("second ensureCompletionPoller() = true; want the live poller to be reused")
	}
}
