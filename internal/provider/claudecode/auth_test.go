package claudecode

import (
	"context"
	"os"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Transcribed v0 contract: auth_test.go TestParseLoggedIn (complete),
// plus one assertion that the observed extras land in Email/Method.
func TestParseAuthStatus(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{`{"loggedIn":true}`, true, false},
		{`{"loggedIn":false}`, false, false},
		{`{"loggedIn":true,"authMethod":"claude.ai","email":"x@y.z"}`, true, false},
		{`{}`, false, false}, // missing field defaults to false
		{``, false, true},    // empty stdout is not valid JSON
		{`not json`, false, true},
	} {
		got, err := ParseAuthStatus([]byte(tc.in))
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseAuthStatus(%q) err = %v; wantErr %v", tc.in, err, tc.wantErr)
		}
		if got.LoggedIn != tc.want {
			t.Errorf("ParseAuthStatus(%q).LoggedIn = %v; want %v", tc.in, got.LoggedIn, tc.want)
		}
	}

	st, err := ParseAuthStatus([]byte(`{"loggedIn":true,"authMethod":"claude.ai","email":"x@y.z"}`))
	if err != nil {
		t.Fatalf("ParseAuthStatus: %v", err)
	}
	if st.Email != "x@y.z" || st.Method != "claude.ai" {
		t.Errorf("ParseAuthStatus extras = email %q method %q; want x@y.z / claude.ai", st.Email, st.Method)
	}
}

// Transcribed v0 contract: auth_test.go TestAuth_LoggedIn_injectedCommand
// (complete) — the injected command becomes a fake claude binary whose
// `auth status --json` output is the script's stdout. The decision order
// is load-bearing: JSON is authoritative regardless of exit code.
func TestAuthStatus_fakeBinaryDecisionOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		want    bool
		wantErr bool
	}{
		{"logged in", `echo '{"loggedIn":true}'`, true, false},
		{"logged out", `echo '{"loggedIn":false}'`, false, false},
		// claude may exit non-zero when logged out but still emit valid
		// JSON; the JSON is the authoritative answer, not the exit code.
		{"logged out, nonzero exit", `echo '{"loggedIn":false}'; exit 1`, false, false},
		{"extra fields ignored", `echo '{"loggedIn":true,"authMethod":"claude.ai"}'`, true, false},
		// no parseable JSON anywhere → a real error the caller can log.
		{"unparseable failure", `echo boom 1>&2; exit 1`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := testProvider(t, newFakeRunner())
			p.claudeBin = fakeClaude(t, tc.script)
			st, err := p.AuthStatus(context.Background(), true)
			if (err != nil) != tc.wantErr {
				t.Errorf("AuthStatus() err = %v; wantErr %v", err, tc.wantErr)
			}
			if st.LoggedIn != tc.want {
				t.Errorf("AuthStatus().LoggedIn = %v; want %v", st.LoggedIn, tc.want)
			}
			if st.CheckedAt.IsZero() {
				t.Errorf("AuthStatus().CheckedAt is zero; want stamped")
			}
		})
	}
}

// Transcribed v0 contract: handlers_test.go
// TestServer_authCacheRefreshesWhenStale — two reads within the TTL run
// the status command once; aging the cache past the TTL re-runs it; force
// ignores the TTL entirely.
func TestAuthStatus_cacheTTLAndForce(t *testing.T) {
	counter := t.TempDir() + "/calls"
	p, _ := testProvider(t, newFakeRunner())
	p.claudeBin = fakeClaude(t, `printf x >> '`+counter+`'; echo '{"loggedIn":true}'`)
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

// Error results are cached exactly like successes (port-spec §3.6): a
// failed check yields logged-out, and a cached read within the TTL
// neither re-runs the command nor resurfaces the error.
func TestAuthStatus_errorResultCachedAsLoggedOut(t *testing.T) {
	counter := t.TempDir() + "/calls"
	p, _ := testProvider(t, newFakeRunner())
	p.claudeBin = fakeClaude(t, `printf x >> '`+counter+`'; echo boom 1>&2; exit 1`)
	p.authTTL = time.Minute
	ctx := context.Background()

	st, err := p.AuthStatus(ctx, true)
	if err == nil {
		t.Fatal("expected error from unparseable status")
	}
	if st.LoggedIn {
		t.Fatal("error result must read as logged out")
	}
	st, err = p.AuthStatus(ctx, false)
	if err != nil {
		t.Fatalf("cached read after error: err = %v; want nil (cached)", err)
	}
	if st.LoggedIn {
		t.Fatal("cached error result must stay logged out")
	}
	if n := countCalls(t, counter); n != 1 {
		t.Fatalf("status calls = %d; want 1 (error result cached)", n)
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
	p.claudeBin = fakeClaude(t, `printf x >> '`+counter+`'; echo '{"loggedIn":true}'`)
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
	p.claudeBin = fakeClaude(t, `printf x >> '`+counter+`'; echo '{"loggedIn":false}'`)
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
