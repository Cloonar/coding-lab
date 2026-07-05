package main

import (
	"os/exec"
	"testing"
)

func TestParseLoggedIn(t *testing.T) {
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
		got, err := parseLoggedIn([]byte(tc.in))
		if (err != nil) != tc.wantErr {
			t.Errorf("parseLoggedIn(%q) err = %v; wantErr %v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("parseLoggedIn(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

// Auth shells out, so these use a stand-in command instead of a real claude
// binary — the same injection pattern as Sessions' tmux tests.
func TestAuth_LoggedIn_injectedCommand(t *testing.T) {
	requireSh(t)
	for _, tc := range []struct {
		name    string
		argv    []string
		want    bool
		wantErr bool
	}{
		{"logged in", []string{"sh", "-c", `echo '{"loggedIn":true}'`}, true, false},
		{"logged out", []string{"sh", "-c", `echo '{"loggedIn":false}'`}, false, false},
		// claude may exit non-zero when logged out but still emit valid JSON;
		// the JSON is the authoritative answer, not the exit code.
		{"logged out, nonzero exit", []string{"sh", "-c", `echo '{"loggedIn":false}'; exit 1`}, false, false},
		{"extra fields ignored", []string{"sh", "-c", `echo '{"loggedIn":true,"authMethod":"claude.ai"}'`}, true, false},
		// no parseable JSON anywhere → a real error the caller can log.
		{"unparseable failure", []string{"sh", "-c", `echo boom 1>&2; exit 1`}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewAuth(tc.argv).LoggedIn()
			if (err != nil) != tc.wantErr {
				t.Errorf("LoggedIn() err = %v; wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("LoggedIn() = %v; want %v", got, tc.want)
			}
		})
	}
}

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH — skipping injected-command test")
	}
}
