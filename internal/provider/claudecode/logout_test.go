package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateCreds points CLAUDE_CONFIG_DIR at a temp dir seeded with a fake
// .credentials.json and returns the creds path. t.Setenv restores the env after
// the test, and the fakeClaude subprocess inherits CLAUDE_CONFIG_DIR — so both
// the provider's rm-escalation (credentialsPath) and the fake `claude auth
// status` script resolve the same file. Load-bearing: without isolation a
// logout test would delete the lab's real credentials.
func isolateCreds(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	creds := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(creds, []byte(`{"claudeAiOauth":{"accessToken":"fake"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return creds
}

// (a) Happy path: `claude auth logout` exits clean and status flips
// logged-out. Success is the status; no escalation fires.
func TestLogout_happyPath(t *testing.T) {
	isolateCreds(t)
	p, _ := testProvider(t, newFakeRunner())
	// logout succeeds; status reads logged-out afterwards.
	p.claudeBin = fakeClaude(t, `case "$1 $2" in
"auth logout") exit 0 ;;
"auth status") echo '{"loggedIn":false}' ;;
esac`)

	if err := p.Logout(context.Background()); err != nil {
		t.Fatalf("Logout happy path: %v", err)
	}
	// The forced refresh left the cache logged-out (invalidated).
	if st, _ := p.AuthStatus(context.Background(), false); st.LoggedIn {
		t.Error("auth cache still logged-in after logout; want the forced-refresh to have invalidated it")
	}
}

// (b) The command exits non-zero but status is logged-out ⇒ still a success:
// `claude auth logout`'s exit code is undocumented/unreliable, so the status
// is authoritative (upstream #30924).
func TestLogout_nonzeroExitButLoggedOut(t *testing.T) {
	isolateCreds(t)
	p, _ := testProvider(t, newFakeRunner())
	p.claudeBin = fakeClaude(t, `case "$1 $2" in
"auth logout") echo 'Failed to log out' 1>&2; exit 1 ;;
"auth status") echo '{"loggedIn":false}' ;;
esac`)

	if err := p.Logout(context.Background()); err != nil {
		t.Fatalf("Logout with non-zero exit but logged-out status should succeed: %v", err)
	}
}

// (c) The command leaves us logged in ⇒ the rm-escalation deletes the creds
// file and the second refresh reads logged-out ⇒ success. The fake status
// keys off the creds file's existence, exactly as a real logout would.
func TestLogout_escalationRemovesCredsThenSucceeds(t *testing.T) {
	creds := isolateCreds(t)
	p, _ := testProvider(t, newFakeRunner())
	// logout is a no-op (leaves the creds file); status mirrors the file.
	p.claudeBin = fakeClaude(t, `case "$1 $2" in
"auth logout") exit 0 ;;
"auth status") if [ -f "$CLAUDE_CONFIG_DIR/.credentials.json" ]; then echo '{"loggedIn":true}'; else echo '{"loggedIn":false}'; fi ;;
esac`)

	if err := p.Logout(context.Background()); err != nil {
		t.Fatalf("Logout escalation path: %v", err)
	}
	if _, err := os.Stat(creds); !os.IsNotExist(err) {
		t.Errorf("credentials file still present after escalation: stat err = %v", err)
	}
}

// (d) Escalation fails: status stays logged-in even after the creds removal
// (e.g. residual server-side state) ⇒ the error surfaces, carrying the
// command's captured stderr.
func TestLogout_escalationFailsSurfacesError(t *testing.T) {
	isolateCreds(t)
	p, _ := testProvider(t, newFakeRunner())
	// status is stubbornly logged-in regardless of the creds file; logout
	// emits a diagnostic on stderr.
	p.claudeBin = fakeClaude(t, `case "$1 $2" in
"auth logout") echo 'residual session remains' 1>&2; exit 1 ;;
"auth status") echo '{"loggedIn":true}' ;;
esac`)

	err := p.Logout(context.Background())
	if err == nil {
		t.Fatal("Logout should error when status stays logged-in after escalation")
	}
	if !strings.Contains(err.Error(), "residual session remains") {
		t.Errorf("Logout error = %q; want it to carry the captured stderr", err)
	}
}
