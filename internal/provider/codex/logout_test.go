package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// isolateAuth points CODEX_HOME at a temp dir seeded with a fake auth.json
// and returns the creds path. t.Setenv restores the env after the test, and
// the fakeCodex subprocess inherits CODEX_HOME — so both the provider's
// rm-escalation (credentialsPath) and the fake `codex login status` script
// resolve the same file. Load-bearing: without isolation a logout test would
// delete the lab's real credentials.
func isolateAuth(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	creds := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(creds, []byte(`{"tokens":{"access_token":"fake"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return creds
}

// (a) Happy path: `codex logout` exits clean and status flips logged-out.
// Success is the status; no escalation fires; provider.auth.changed lands.
func TestLogout_happyPath(t *testing.T) {
	isolateAuth(t)
	p, bus := testProvider(t, newFakeRunner())
	p.codexBin = fakeCodex(t, `case "$1" in
logout) exit 0 ;;
login) echo 'Not logged in'; exit 1 ;;
esac`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, unsub := bus.Subscribe(ctx)
	defer unsub()

	if err := p.Logout(ctx); err != nil {
		t.Fatalf("Logout happy path: %v", err)
	}
	// The forced refresh left the cache logged-out (invalidated).
	if st, _ := p.AuthStatus(ctx, false); st.LoggedIn {
		t.Error("auth cache still logged-in after logout; want the forced-refresh to have invalidated it")
	}
	select {
	case e := <-evs:
		if e.Type != provider.EventAuthChanged {
			t.Errorf("event type = %q; want %q", e.Type, provider.EventAuthChanged)
		}
	case <-time.After(2 * time.Second):
		t.Error("no provider.auth.changed event after the account dropped")
	}
}

// (b) The command exits non-zero but status is logged-out ⇒ still a success:
// the status is authoritative, never the exit code (the seam contract).
func TestLogout_nonzeroExitButLoggedOut(t *testing.T) {
	isolateAuth(t)
	p, _ := testProvider(t, newFakeRunner())
	p.codexBin = fakeCodex(t, `case "$1" in
logout) echo 'Failed to log out' 1>&2; exit 1 ;;
login) echo 'Not logged in'; exit 1 ;;
esac`)

	if err := p.Logout(context.Background()); err != nil {
		t.Fatalf("Logout with non-zero exit but logged-out status should succeed: %v", err)
	}
}

// (c) The command leaves us logged in ⇒ the rm-escalation deletes
// <codexHome>/auth.json and the second refresh reads logged-out ⇒ success.
// The fake status keys off the creds file's existence, exactly as a real
// logout would.
func TestLogout_escalationRemovesAuthJSONThenSucceeds(t *testing.T) {
	creds := isolateAuth(t)
	p, _ := testProvider(t, newFakeRunner())
	// logout is a no-op (leaves the creds file); status mirrors the file.
	p.codexBin = fakeCodex(t, `case "$1" in
logout) exit 0 ;;
login) if [ -f "$CODEX_HOME/auth.json" ]; then echo 'Logged in using ChatGPT'; else echo 'Not logged in'; exit 1; fi ;;
esac`)

	if err := p.Logout(context.Background()); err != nil {
		t.Fatalf("Logout escalation path: %v", err)
	}
	if _, err := os.Stat(creds); !os.IsNotExist(err) {
		t.Errorf("auth.json still present after escalation: stat err = %v", err)
	}
}

// (d) Escalation fails: status stays logged-in even after the creds removal
// ⇒ the error surfaces, carrying the command's captured stderr.
func TestLogout_escalationFailsSurfacesError(t *testing.T) {
	isolateAuth(t)
	p, _ := testProvider(t, newFakeRunner())
	p.codexBin = fakeCodex(t, `case "$1" in
logout) echo 'residual session remains' 1>&2; exit 1 ;;
login) echo 'Logged in using ChatGPT' ;;
esac`)

	err := p.Logout(context.Background())
	if err == nil {
		t.Fatal("Logout should error when status stays logged-in after escalation")
	}
	if !strings.Contains(err.Error(), "residual session remains") {
		t.Errorf("Logout error = %q; want it to carry the captured stderr", err)
	}
}

// credentialsPath mirrors codex's own home resolution: $CODEX_HOME when set,
// else <loginDir>/.codex.
func TestCredentialsPath_resolution(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())

	t.Setenv("CODEX_HOME", "/srv/codex-home")
	if got, want := p.credentialsPath(), filepath.Join("/srv/codex-home", "auth.json"); got != want {
		t.Errorf("credentialsPath with CODEX_HOME = %q; want %q", got, want)
	}

	t.Setenv("CODEX_HOME", "")
	if got, want := p.credentialsPath(), filepath.Join(p.loginDir, ".codex", "auth.json"); got != want {
		t.Errorf("credentialsPath without CODEX_HOME = %q; want %q", got, want)
	}
}
