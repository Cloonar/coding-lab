package tmuxx

import "testing"

// LoginSessionName derives the per-provider login session name — two
// providers must never collide on the same tmux session (issue #77).
func TestLoginSessionName(t *testing.T) {
	tests := []struct {
		providerID string
		want       string
	}{
		{"claude-code", "lab-login-claude-code"},
		{"codex", "lab-login-codex"},
	}
	for _, tt := range tests {
		if got := LoginSessionName(tt.providerID); got != tt.want {
			t.Errorf("LoginSessionName(%q) = %q, want %q", tt.providerID, got, tt.want)
		}
	}
}

// IsLoginSession must recognize every provider's derived name AND the bare
// legacy "lab-login" (written by pre-per-provider binaries), while rejecting
// anything that merely shares the prefix without the dash separator (issue
// #77: two providers' login sessions coexist, and a leftover legacy session
// is still excluded).
func TestIsLoginSession(t *testing.T) {
	trueCases := []string{
		"lab-login", // legacy, no provider suffix
		"lab-login-claude-code",
		"lab-login-codex",
	}
	for _, name := range trueCases {
		if !IsLoginSession(name) {
			t.Errorf("IsLoginSession(%q) = false, want true", name)
		}
	}

	falseCases := []string{
		"",
		"proj~afk-7",
		"lab-loginx", // prefix without the dash separator
		"xlab-login",
	}
	for _, name := range falseCases {
		if IsLoginSession(name) {
			t.Errorf("IsLoginSession(%q) = true, want false", name)
		}
	}
}
