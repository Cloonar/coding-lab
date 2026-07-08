package claudecode

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Transcribed v0 contract: sessions_test.go TestExtractOAuthURL
// (complete). Row 1 is the real captured authorize line (claude 2.1.150);
// it doubles as the greedy-matching guard — the encoded redirect_uri
// embeds "%2Foauth%2F…", which must not be mistaken for the literal
// "/oauth/" segment.
func TestExtractOAuthURL(t *testing.T) {
	const realLine = "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=3paXVTV6FPSlOHej-t8CPB3Azy7demaQ-wTe13tPw6E&code_challenge_method=S256&state=84xH4rqCmCJzD1NFDMGF8YMkafUHjFOIhzfLXzd8ERQ"
	const realURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile&code_challenge=3paXVTV6FPSlOHej-t8CPB3Azy7demaQ-wTe13tPw6E&code_challenge_method=S256&state=84xH4rqCmCJzD1NFDMGF8YMkafUHjFOIhzfLXzd8ERQ"
	for _, tc := range []struct{ in, want string }{
		{realLine, realURL},
		{"https://claude.ai/oauth/authorize?code=abc#state=xyz", "https://claude.ai/oauth/authorize?code=abc#state=xyz"},
		{"Use the url below to sign in https://claude.com/cai/oauth/authorize?a=b.", "https://claude.com/cai/oauth/authorize?a=b"},
		{"wraps in (https://claude.ai/oauth/foo)", "https://claude.ai/oauth/foo"},
		{"https://claude.ai/code/session_abc", ""}, // /code is not an oauth link
		{"no url here", ""},
	} {
		if got := ExtractOAuthURL(tc.in); got != tc.want {
			t.Errorf("ExtractOAuthURL(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// Transcribed v0 contract: handlers_test.go TestValidateLoginCode
// (complete). Only empty/oversize/control-char input is rejected — "#",
// "=", spaces inside (after trim), and base64url must pass.
func TestValidateLoginCode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain", "abc123", "abc123", false},
		{"trims surrounding whitespace", "  abc123  ", "abc123", false},
		{"code#state base64url passes", "aB3-_x#state=Zm9v-_", "aB3-_x#state=Zm9v-_", false},
		{"empty", "", "", true},
		{"whitespace only", "   \t  ", "", true},
		{"embedded newline rejected", "abc\ndef", "", true},
		{"embedded NUL rejected", "abc\x00def", "", true},
		{"too long", strings.Repeat("a", maxLoginCodeLen+1), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateLoginCode(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateLoginCode(%q) err = %v; wantErr %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("validateLoginCode(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// v0 TestHandleLoginStart_capturesOAuthURL, provider-shaped: LoginStart
// spawns `claude auth login --claudeai` in the login dir as the fixed
// lab-login session and synchronously scrapes the authorize URL from its
// pane. The pane content is scripted up front — the fake only serves it
// once the session is live, like a real pane.
func TestLoginStart_capturesOAuthURL(t *testing.T) {
	run := newLoginRunner()
	run.SetPane(LoginSession, "Use the url below to sign in https://claude.ai/oauth/authorize?code=xyz")
	p, _ := testProvider(t, run)

	url, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if want := "https://claude.ai/oauth/authorize?code=xyz"; url != want {
		t.Errorf("LoginStart url = %q; want %q", url, want)
	}
	sess, live := run.Session(LoginSession)
	if !live {
		t.Fatal("login session not running after LoginStart")
	}
	if sess.Dir != p.loginDir {
		t.Errorf("login session dir = %q; want loginDir %q", sess.Dir, p.loginDir)
	}
	wantArgv := []string{p.claudeBin, "auth", "login", "--claudeai"}
	if got := strings.Join(sess.Argv, " "); got != strings.Join(wantArgv, " ") {
		t.Errorf("login argv = %q; want %q", got, strings.Join(wantArgv, " "))
	}
}

// Pinned M3 contract: LoginStart while the login session is live
// re-scrapes instead of double-spawning.
func TestLoginStart_idempotentWhileLive(t *testing.T) {
	run := newLoginRunner()
	run.SetPane(LoginSession, "visit https://claude.ai/oauth/authorize?code=first")
	p, _ := testProvider(t, run)
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

// v0 §3.5 restart recovery: the login session survives a lab restart but
// the in-memory URL is gone — LoginStart against the live session
// recovers the URL from the pane without spawning anything.
func TestLoginStart_recoversURLFromLivePane(t *testing.T) {
	run := newLoginRunner()
	run.AddLive(LoginSession)
	run.SetPane(LoginSession, "still waiting: https://claude.com/cai/oauth/authorize?code=survivor.")
	p, _ := testProvider(t, run)

	url, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if want := "https://claude.com/cai/oauth/authorize?code=survivor"; url != want {
		t.Errorf("LoginStart url = %q; want %q (trailing '.' trimmed)", url, want)
	}
	if n := run.startCount(); n != 0 {
		t.Errorf("session starts = %d; want 0 (re-joined the live attempt)", n)
	}
}

// A bad code never reaches tmux and never tears the login session down
// (v0: the session is still waiting; the user just retries).
func TestLoginSubmitCode_invalidRejectedBeforeSendKeys(t *testing.T) {
	run := newLoginRunner()
	run.AddLive(LoginSession)
	p, _ := testProvider(t, run)

	for _, in := range []string{"", "   \t  ", "abc\ndef", strings.Repeat("a", maxLoginCodeLen+1)} {
		if err := p.LoginSubmitCode(context.Background(), in); !errors.Is(err, ErrInvalidCode) {
			t.Errorf("LoginSubmitCode(%.10q…) err = %v; want ErrInvalidCode", in, err)
		}
	}
	if n := len(run.Sent(LoginSession)); n != 0 {
		t.Errorf("send-keys calls = %d; want 0 for invalid codes", n)
	}
	if ok, _ := run.IsRunning(context.Background(), LoginSession); !ok {
		t.Error("login session torn down by an invalid code; must stay up")
	}
}

// Happy path: the code goes to the login session literally (one line +
// Enter), the forced status poll sees the login land, the login session
// is killed and provider.auth.changed is published.
func TestLoginSubmitCode_happyPath(t *testing.T) {
	state := t.TempDir() + "/state"
	if err := os.WriteFile(state, []byte(`{"loggedIn":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newLoginRunner()
	run.AddLive(LoginSession)
	p, bus := testProvider(t, run)
	p.claudeBin = fakeClaude(t, `cat '`+state+`'`)
	p.loginTimeout = 2 * time.Second

	// Accepting the code flips the fake claude's persisted auth state —
	// the same observable transition the real `claude auth login` makes.
	run.onSendKeys = func(_, _ string, _ bool) {
		if err := os.WriteFile(state, []byte(`{"loggedIn":true,"authMethod":"claude.ai","email":"x@y.z"}`), 0o600); err != nil {
			t.Error(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, unsub := bus.Subscribe(ctx)
	defer unsub()

	code := "aB3-_x#state=Zm9v-_"
	if err := p.LoginSubmitCode(ctx, "  "+code+"  "); err != nil {
		t.Fatalf("LoginSubmitCode: %v", err)
	}

	sent := run.Sent(LoginSession)
	if len(sent) != 1 || sent[0].Text != code || !sent[0].Enter {
		t.Errorf("sent keys = %+v; want one literal line %q + Enter to %s", sent, code, LoginSession)
	}
	if ok, _ := run.IsRunning(ctx, LoginSession); ok {
		t.Error("login session still running after successful login; want it killed")
	}
	select {
	case e := <-evs:
		if e.Type != EventAuthChanged {
			t.Errorf("event type = %q; want %q", e.Type, EventAuthChanged)
		}
		// The generalized payload carries the provider id (issue #51 dec 7).
		if pl, ok := e.Payload.(provider.AuthChangedPayload); !ok || pl.Provider != ID {
			t.Errorf("event payload = %#v; want AuthChangedPayload with provider %q", e.Payload, ID)
		}
	case <-time.After(2 * time.Second):
		t.Error("no provider.auth.changed event after successful login")
	}
	// The forced polling left the cache fresh and logged-in.
	if st, err := p.AuthStatus(ctx, false); err != nil || !st.LoggedIn {
		t.Errorf("AuthStatus after login = %+v, %v; want cached logged-in", st, err)
	}
}

// v0 TestHandleLoginCode_timeoutTearsDown, provider-shaped: auth never
// flips, so after loginTimeout the stuck attempt is torn down, the
// remembered URL cleared, and ErrLoginTimeout returned.
func TestLoginSubmitCode_timeoutTearsDown(t *testing.T) {
	run := newLoginRunner()
	run.AddLive(LoginSession)
	p, _ := testProvider(t, run)
	p.claudeBin = fakeClaude(t, `echo '{"loggedIn":false}'`)
	p.loginTimeout = 100 * time.Millisecond
	p.loginPoll = 20 * time.Millisecond
	p.setLoginURL("https://claude.ai/oauth/authorize?code=stale")

	err := p.LoginSubmitCode(context.Background(), "abc#state=1")
	if !errors.Is(err, ErrLoginTimeout) {
		t.Fatalf("LoginSubmitCode err = %v; want ErrLoginTimeout", err)
	}
	if ok, _ := run.IsRunning(context.Background(), LoginSession); ok {
		t.Error("login session still running after timeout; want it torn down")
	}
	if got := p.getLoginURL(); got != "" {
		t.Errorf("loginURL after timeout = %q; want cleared", got)
	}
}
