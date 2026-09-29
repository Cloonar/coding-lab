package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/config"
)

// TestUsageDocumentsGenericProviderFlags pins the issue #78 / ADR-0034
// acceptance that `-help` documents the generic per-provider flags and marks
// the pre-#78 spellings as deprecated aliases. usage is the package-level
// const printed on flag.ErrHelp, so assert against it directly.
func TestUsageDocumentsGenericProviderFlags(t *testing.T) {
	for _, want := range []string{
		"-provider-bin",
		"-provider-config",
		"LAB_PROVIDER_BIN_<ID>",
		"LAB_PROVIDER_CONFIG_<ID>",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not document %q", want)
		}
	}
	// The deprecated aliases must appear AND be labelled deprecated.
	if !strings.Contains(usage, "-claude") || !strings.Contains(usage, "-claude-config") {
		t.Error("usage no longer mentions the -claude / -claude-config aliases")
	}
	if !strings.Contains(usage, "deprecated") {
		t.Error("usage does not mark -claude / -claude-config as deprecated")
	}
}

// TestUsageDocumentsSeedFlags pins the issue #137 acceptance that `-help`
// documents the reworked hash-based seed flags, their env overrides, and the
// new `lab hash-password` subcommand, in the style of
// TestUsageDocumentsGenericProviderFlags. It also pins that the pre-#137
// plaintext-password spellings are gone.
func TestUsageDocumentsSeedFlags(t *testing.T) {
	for _, want := range []string{
		"-seed-user",
		"-seed-password-hash",
		"-seed-password-hash-file",
		"LAB_SEED_USER",
		"LAB_SEED_PASSWORD_HASH",
		"LAB_SEED_PASSWORD_HASH_FILE",
		"hash-password",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not document %q", want)
		}
	}

	// The old (#134) plaintext-password spellings must be gone. Plain
	// Contains checks on the bare old forms would pass vacuously (they're
	// prefixes of the new "-seed-password-hash"/"LAB_SEED_PASSWORD_HASH"
	// spellings, so they're always "found" as substrings) or, for the
	// "-file"/"_FILE" suffix forms, never actually distinguish old from new.
	// Delimit each old form with the character that immediately follows it
	// in the old usage text so it can only match the old spelling.
	for _, gone := range []string{
		"-seed-password ",        // old inline flag; trailing space rules out "-seed-password-hash"
		"-seed-password-file",    // old file flag: "-hash-file" is the new one, so bare "-file" can't match it
		"LAB_SEED_PASSWORD)",     // old inline env, closing paren rules out "LAB_SEED_PASSWORD_HASH"
		"LAB_SEED_PASSWORD_FILE", // old file env: "_HASH_FILE" is the new one, so this can't match it
	} {
		if strings.Contains(usage, gone) {
			t.Errorf("usage still documents the old (#134) spelling %q", gone)
		}
	}
}

// TestLabURL pins the session-facing LAB_URL precedence (issue #201): an
// explicit agent URL wins verbatim; otherwise the agent unix socket under the
// state dir. BaseURL and the pre-#201 loopback fallback play no part —
// routing agent traffic through the external origin was the issue #30
// failure mode.
func TestLabURL(t *testing.T) {
	tests := []struct {
		name     string
		agentURL string
		baseURL  string
		addr     string
		stateDir string
		want     string
	}{
		{
			name:     "socket default from state dir",
			stateDir: "/var/lib/lab",
			addr:     ":8080",
			// The socket's own dir since issue #205 (agentapi.SocketDir).
			want: "unix:///var/lib/lab/agent/agent.sock",
		},
		{
			name:     "socket default ignores base url and addr",
			stateDir: "/srv/lab",
			baseURL:  "https://lab.example.com",
			addr:     "0.0.0.0:9090",
			want:     "unix:///srv/lab/agent/agent.sock",
		},
		{
			name:     "agent url wins verbatim",
			agentURL: "http://127.0.0.1:8080",
			baseURL:  "https://lab.example.com",
			stateDir: "/srv/lab",
			addr:     ":8080",
			want:     "http://127.0.0.1:8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := labURL(config.Config{
				AgentURL: tt.agentURL,
				BaseURL:  tt.baseURL,
				Addr:     tt.addr,
				StateDir: tt.stateDir,
			})
			if got != tt.want {
				t.Errorf("labURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRetryUntilComplete pins the startup Warpgate key sweep's retry (issue
// #39 / ADR-0068): a pass that does not complete is retried after 1m, 2m,
// 4m, 8m and then every 15m until one completes; a pass that completes first
// time is never retried and never waits; and a done context stops the loop
// at the wait it interrupts, without another pass.
func TestRetryUntilComplete(t *testing.T) {
	t.Run("retries with capped backoff until a pass completes", func(t *testing.T) {
		passes := 0
		pass := func(context.Context) bool { passes++; return passes == 7 }
		var waits []time.Duration
		wait := func(_ context.Context, d time.Duration) bool { waits = append(waits, d); return true }

		attempts, done := retryUntilComplete(t.Context(), pass, bastionSweepRetryFirst, bastionSweepRetryMax, wait)
		if attempts != 7 || !done {
			t.Errorf("retryUntilComplete = (%d, %v), want (7, true)", attempts, done)
		}
		want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
		if !slices.Equal(waits, want) {
			t.Errorf("waits = %v, want %v", waits, want)
		}
	})

	t.Run("a complete first pass is the only pass", func(t *testing.T) {
		wait := func(context.Context, time.Duration) bool { t.Error("waited after a complete pass"); return true }
		attempts, done := retryUntilComplete(t.Context(), func(context.Context) bool { return true }, time.Minute, time.Hour, wait)
		if attempts != 1 || !done {
			t.Errorf("retryUntilComplete = (%d, %v), want (1, true)", attempts, done)
		}
	})

	t.Run("a done context stops the retries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		passes := 0
		pass := func(context.Context) bool { passes++; return false }
		wait := func(_ context.Context, _ time.Duration) bool {
			if passes == 2 {
				cancel() // shutdown during the second wait
				return false
			}
			return true
		}
		attempts, done := retryUntilComplete(ctx, pass, time.Minute, time.Hour, wait)
		if attempts != 2 || done || passes != 2 {
			t.Errorf("retryUntilComplete = (%d, %v) after %d passes, want (2, false) after 2", attempts, done, passes)
		}
		if n, _ := retryUntilComplete(ctx, pass, time.Minute, time.Hour, wait); n != 0 {
			t.Errorf("a pass ran on an already-done context (%d attempts)", n)
		}
	})

	t.Run("waitCtx returns early on a done context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if waitCtx(ctx, time.Hour) {
			t.Error("waitCtx reported a full wait on a cancelled context")
		}
		if !waitCtx(t.Context(), time.Millisecond) {
			t.Error("waitCtx reported an early return for an elapsed wait")
		}
	})
}
