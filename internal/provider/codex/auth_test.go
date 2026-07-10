package codex

import (
	"context"
	"os"
	"testing"
	"time"
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
