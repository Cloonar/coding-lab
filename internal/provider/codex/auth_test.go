package codex

import (
	"context"
	"os"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Pinned `codex login status` output shapes (live 0.133.0): logged in →
// exit 0 + "Logged in using ChatGPT"; logged out → exit 1 + "Not logged in".
func TestParseAuthStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		out        string
		exitOK     bool
		want       bool
		wantMethod string
	}{
		{"pinned logged in", "Logged in using ChatGPT\n", true, true, "chatgpt"},
		{"pinned logged out", "Not logged in\n", false, false, ""},
		// The exit code is half the verdict: "Logged in" text with a failing
		// exit must not read as logged in.
		{"logged-in text with failing exit", "Logged in using ChatGPT\n", false, false, "chatgpt"},
		{"empty output", "", true, false, ""},
		{"method lowercased and trimmed", "Logged in using API key.\n", true, true, "api key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := ParseAuthStatus([]byte(tc.out), tc.exitOK)
			if st.LoggedIn != tc.want {
				t.Errorf("LoggedIn = %v; want %v", st.LoggedIn, tc.want)
			}
			if st.Method != tc.wantMethod {
				t.Errorf("Method = %q; want %q", st.Method, tc.wantMethod)
			}
			if st.Email != "" {
				t.Errorf("Email = %q; want empty (codex prints none)", st.Email)
			}
		})
	}
}

// The injected command becomes a fake codex binary whose `login status`
// output/exit is the script's. A plain non-zero exit is a definitive
// logged-out answer, not an error; only a run failure (missing binary) is.
func TestAuthStatus_fakeBinaryDecisionOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		want    bool
		method  string
		wantErr bool
	}{
		{"logged in", `echo 'Logged in using ChatGPT'`, true, "chatgpt", false},
		{"logged out, exit 1", `echo 'Not logged in'; exit 1`, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := testProvider(t, newFakeRunner())
			p.codexBin = fakeCodex(t, tc.script)
			st, err := p.AuthStatus(context.Background(), true)
			if (err != nil) != tc.wantErr {
				t.Errorf("AuthStatus() err = %v; wantErr %v", err, tc.wantErr)
			}
			if st.LoggedIn != tc.want {
				t.Errorf("AuthStatus().LoggedIn = %v; want %v", st.LoggedIn, tc.want)
			}
			if st.Method != tc.method {
				t.Errorf("AuthStatus().Method = %q; want %q", st.Method, tc.method)
			}
			if st.CheckedAt.IsZero() {
				t.Errorf("AuthStatus().CheckedAt is zero; want stamped")
			}
		})
	}

	t.Run("missing binary is an error, read as logged out", func(t *testing.T) {
		p, _ := testProvider(t, newFakeRunner())
		p.codexBin = "/nonexistent/codex-missing"
		st, err := p.AuthStatus(context.Background(), true)
		if err == nil {
			t.Error("expected error from a missing binary")
		}
		if st.LoggedIn {
			t.Error("a run failure must read as logged out")
		}
	})
}

// Two reads within the TTL run the status command once; aging the cache past
// the TTL re-runs it; force ignores the TTL entirely (claudecode's pinned
// cache discipline).
func TestAuthStatus_cacheTTLAndForce(t *testing.T) {
	counter := t.TempDir() + "/calls"
	p, _ := testProvider(t, newFakeRunner())
	p.codexBin = fakeCodex(t, `printf x >> '`+counter+`'; echo 'Logged in using ChatGPT'`)
	p.authTTL = time.Minute
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		st, err := p.AuthStatus(ctx, false)
		if err != nil || !st.LoggedIn {
			t.Fatalf("AuthStatus #%d = %+v, %v; want logged in", i+1, st, err)
		}
	}
	if n := countCalls(t, counter); n != 1 {
		t.Fatalf("within TTL: status calls = %d; want 1 (second read cached)", n)
	}

	// Age the cache past the TTL: the next read must re-run the check.
	p.authMu.Lock()
	p.authChecked = time.Now().Add(-2 * time.Minute)
	p.authMu.Unlock()
	if st, _ := p.AuthStatus(ctx, false); !st.LoggedIn {
		t.Fatal("expected logged in after refresh")
	}
	if n := countCalls(t, counter); n != 2 {
		t.Fatalf("after staleness: status calls = %d; want 2", n)
	}

	// force ignores the TTL entirely.
	if _, err := p.AuthStatus(ctx, true); err != nil {
		t.Fatalf("force AuthStatus: %v", err)
	}
	if n := countCalls(t, counter); n != 3 {
		t.Fatalf("after force-refresh: status calls = %d; want 3", n)
	}
}

// Error results are cached exactly like successes: a failed check yields
// logged-out, and a cached read within the TTL neither re-runs the command
// nor resurfaces the error.
func TestAuthStatus_errorResultCachedAsLoggedOut(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	p.codexBin = "/nonexistent/codex-missing"
	p.authTTL = time.Minute
	ctx := context.Background()

	st, err := p.AuthStatus(ctx, true)
	if err == nil {
		t.Fatal("expected error from a missing binary")
	}
	if st.LoggedIn {
		t.Fatal("error result must read as logged out")
	}
	// Swap in a working binary: the cached error result must still serve.
	p.codexBin = fakeCodex(t, `echo 'Logged in using ChatGPT'`)
	st, err = p.AuthStatus(ctx, false)
	if err != nil {
		t.Fatalf("cached read after error: err = %v; want nil (cached)", err)
	}
	if st.LoggedIn {
		t.Fatal("cached error result must stay logged out until refreshed")
	}
}

func countCalls(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(b)
}

// LastAuthStatus (provider.AuthPeeker, issue #61) reports the last KNOWN
// login state and never looks: it runs no status command — not before a
// first check, not once the render cache has gone stale, not when the truth
// has changed underneath — and it does not wait for a check in flight. That
// is what lets the readiness report read it on every page view.
func TestLastAuthStatus_peeksAndNeverChecks(t *testing.T) {
	counter := t.TempDir() + "/calls"
	p, _ := testProvider(t, newFakeRunner())
	p.codexBin = fakeCodex(t, `printf x >> '`+counter+`'; echo 'Logged in using ChatGPT'`)
	p.authTTL = time.Minute
	ctx := context.Background()
	var _ provider.AuthPeeker = p

	// Nothing checked since the process started: unknown, and still unchecked.
	if st, known := p.LastAuthStatus(); known || st.LoggedIn || !st.CheckedAt.IsZero() {
		t.Fatalf("LastAuthStatus before any check = %+v, known=%v; want the zero status, unknown", st, known)
	}
	if n := countCalls(t, counter); n != 0 {
		t.Fatalf("a peek ran the status command %d time(s)", n)
	}

	checked, err := p.AuthStatus(ctx, true)
	if err != nil || !checked.LoggedIn {
		t.Fatalf("AuthStatus = %+v, %v; want logged in", checked, err)
	}
	for range 50 {
		if st, known := p.LastAuthStatus(); !known || st != checked {
			t.Fatalf("LastAuthStatus = %+v, known=%v; want the checked status %+v", st, known, checked)
		}
	}

	// The render cache ages out. AuthStatus would refresh now; a peek does not.
	p.authMu.Lock()
	p.authChecked = time.Now().Add(-2 * time.Minute)
	p.authMu.Unlock()
	// And the account logs out underneath. The peek keeps the last known
	// answer until something actually checks.
	p.codexBin = fakeCodex(t, `printf x >> '`+counter+`'; echo 'Not logged in'; exit 1`)
	if st, known := p.LastAuthStatus(); !known || !st.LoggedIn {
		t.Fatalf("LastAuthStatus after the cache aged out = %+v, known=%v; want the last known (logged in)", st, known)
	}
	if n := countCalls(t, counter); n != 1 {
		t.Fatalf("status command ran %d times, want 1 — only the explicit check", n)
	}

	// A check in flight holds authMu for the whole status command; the peek
	// answers anyway, with the previous result.
	p.authMu.Lock()
	done := make(chan provider.AuthStatus, 1)
	go func() {
		st, _ := p.LastAuthStatus()
		done <- st
	}()
	select {
	case st := <-done:
		if !st.LoggedIn {
			t.Errorf("peek during a check = %+v, want the previous result", st)
		}
	case <-time.After(5 * time.Second):
		t.Error("LastAuthStatus blocked behind an in-flight check")
	}
	p.authMu.Unlock()

	// The next real check is what moves it.
	if st, _ := p.AuthStatus(ctx, false); st.LoggedIn {
		t.Fatal("AuthStatus after the logout still reads logged in")
	}
	if st, known := p.LastAuthStatus(); !known || st.LoggedIn {
		t.Fatalf("LastAuthStatus after the re-check = %+v, known=%v; want logged out", st, known)
	}
	if n := countCalls(t, counter); n != 2 {
		t.Fatalf("status command ran %d times, want 2", n)
	}
}

// Only a check that ran to completion with a pinned verdict moves the peek
// (issue #61). The status process killed by its caller's context exits
// non-zero — formerly read as the definitive "Not logged in" — and a
// non-zero exit with output that is no verdict at all are both errors now:
// still logged out to the caller (spawn safety), never evidence of a logout
// for the peek, which keeps the previous answer or stays "never checked". A
// completed logged-out check still flips it.
func TestLastAuthStatus_onlyACompletedCheckMovesThePeek(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	p.authTTL = time.Minute
	outlives := fakeCodex(t, `exec sleep 5`)
	garbage := fakeCodex(t, `echo 'thread main panicked at src/main.rs:1:1'; exit 101`)

	interrupted := func() {
		t.Helper()
		p.codexBin = outlives
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		st, err := p.AuthStatus(ctx, true)
		if err == nil || st.LoggedIn {
			t.Fatalf("interrupted check = %+v, %v; want an error read as logged out", st, err)
		}
		if time.Since(start) > 4*time.Second {
			t.Fatal("the check outlived its context")
		}
	}
	unreadable := func() {
		t.Helper()
		p.codexBin = garbage
		st, err := p.AuthStatus(context.Background(), true)
		if err == nil || st.LoggedIn {
			t.Fatalf("unreadable check = %+v, %v; want an error read as logged out", st, err)
		}
	}

	interrupted()
	unreadable()
	if st, known := p.LastAuthStatus(); known || st.LoggedIn {
		t.Fatalf("peek after failed checks only = %+v, known=%v; want never checked", st, known)
	}

	p.codexBin = fakeCodex(t, `echo 'Logged in using ChatGPT'`)
	checked, err := p.AuthStatus(context.Background(), true)
	if err != nil || !checked.LoggedIn {
		t.Fatalf("AuthStatus = %+v, %v; want logged in", checked, err)
	}

	interrupted()
	if st, known := p.LastAuthStatus(); !known || st != checked {
		t.Fatalf("peek after an interrupted check = %+v, known=%v; want the previous %+v", st, known, checked)
	}
	unreadable()
	if st, known := p.LastAuthStatus(); !known || st != checked {
		t.Fatalf("peek after an unreadable check = %+v, known=%v; want the previous %+v", st, known, checked)
	}

	p.codexBin = fakeCodex(t, `echo 'Not logged in'; exit 1`)
	if st, err := p.AuthStatus(context.Background(), true); err != nil || st.LoggedIn {
		t.Fatalf("AuthStatus = %+v, %v; want a clean logged-out verdict", st, err)
	}
	if st, known := p.LastAuthStatus(); !known || st.LoggedIn {
		t.Fatalf("peek after a completed logged-out check = %+v, known=%v; want logged out", st, known)
	}
}

// The status command's output is a verdict only in the two pinned shapes;
// anything else is an error the caller reads as logged out.
func TestAuthStatus_onlyPinnedShapesAreVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		want    bool
		wantErr bool
	}{
		{"pinned logged in", `echo 'Logged in using ChatGPT'`, true, false},
		{"pinned logged out", `echo 'Not logged in'; exit 1`, false, false},
		{"exit 0, no verdict", `echo 'something else entirely'`, false, true},
		{"exit 0, empty", `true`, false, true},
		{"exit 1, no verdict", `echo 'Error: config.toml is malformed' 1>&2; exit 1`, false, true},
		{"logged-in text, failing exit", `echo 'Logged in using ChatGPT'; exit 1`, false, true},
		{"not-logged-in text, clean exit", `echo 'Not logged in'`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := testProvider(t, newFakeRunner())
			p.codexBin = fakeCodex(t, tc.script)
			st, err := p.AuthStatus(context.Background(), true)
			if (err != nil) != tc.wantErr || st.LoggedIn != tc.want {
				t.Fatalf("AuthStatus = %+v, %v; want logged in %v, error %v", st, err, tc.want, tc.wantErr)
			}
			if _, known := p.LastAuthStatus(); known == tc.wantErr {
				t.Fatalf("peek known = %v after a check with error %v", known, err)
			}
		})
	}
}
