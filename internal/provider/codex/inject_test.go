package codex

import (
	"os"
	"path/filepath"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// assertInjected asserts the common happy-path shape shared by both master
// resolutions: the env pin is exactly [CODEX_HOME=<home>/.codex], the copied
// auth.json matches master byte-for-byte at 0600, and its parent dir
// (<home>/.codex) is 0700 — a credential's directory, not codex's usual
// world-readable config dir.
func assertInjected(t *testing.T, p *Provider, home string, master []byte) {
	t.Helper()
	env, err := p.InjectCredentials(home)
	if err != nil {
		t.Fatalf("InjectCredentials: %v", err)
	}
	codexHome := instanceCodexHome(home)
	if len(env) != 1 || env[0] != "CODEX_HOME="+codexHome {
		t.Errorf("env = %v; want [CODEX_HOME=%s]", env, codexHome)
	}
	dst := filepath.Join(codexHome, "auth.json")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read instance auth.json: %v", err)
	}
	if string(got) != string(master) {
		t.Errorf("instance auth.json = %q; want the master copy %q", got, master)
	}
	if info, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("instance auth.json mode = %o; want 0600 (credential, not world-readable config)", perm)
	}
	if info, err := os.Stat(codexHome); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("instance codexHome dir mode = %o; want 0700 (a credential's parent dir)", perm)
	}
}

// (a) Happy path, $CODEX_HOME master resolution: the master auth.json is
// copied into <home>/.codex/auth.json (0600, parent 0700) and
// CODEX_HOME=<home>/.codex is returned.
func TestInjectCredentials_happyPath(t *testing.T) {
	creds := isolateAuth(t) // CODEX_HOME points at a temp master carrying auth.json
	master, err := os.ReadFile(creds)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir()
	assertInjected(t, p, home, master)
}

// (a2) Happy path, the OTHER master resolution: no $CODEX_HOME set, so
// credentialsPath falls back to <loginDir>/.codex/auth.json — the default a
// host that never set $CODEX_HOME uses. Both master resolutions must feed the
// same injector behavior.
func TestInjectCredentials_happyPath_loginDirDefault(t *testing.T) {
	t.Setenv("CODEX_HOME", "") // force the LoginDir-relative default
	p, _ := testProvider(t, newFakeRunner())
	masterDir := filepath.Join(p.loginDir, ".codex")
	if err := os.MkdirAll(masterDir, 0o755); err != nil {
		t.Fatal(err)
	}
	master := []byte(`{"tokens":{"access_token":"fake-logindir-default"}}`)
	if err := os.WriteFile(filepath.Join(masterDir, "auth.json"), master, 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	assertInjected(t, p, home, master)
}

// (a3) Order independence (regression guard): SeedWorkspace and
// InjectCredentials both write under the same <home>/.codex directory, and
// SeedWorkspace's writeAtomic creates it at 0o755 (appropriate for its own
// world-readable config.toml/AGENTS.md) if it runs first. InjectCredentials
// must still leave that directory at 0700 — the credential's own mode
// requirement must not depend on which of the two ran first.
func TestInjectCredentials_parentDirModeIndependentOfSeedOrder(t *testing.T) {
	creds := isolateAuth(t)
	master, err := os.ReadFile(creds)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := testProvider(t, newFakeRunner())
	worktree := t.TempDir()
	home := t.TempDir()

	if err := p.SeedWorkspace(worktree, provider.SeedOpts{Home: home}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}
	codexHome := instanceCodexHome(home)
	if info, err := os.Stat(codexHome); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("precondition: codexHome after SeedWorkspace alone = %o; want 0755 (world-readable config)", perm)
	}

	assertInjected(t, p, home, master)
}

// (e) A master-store read failure that is NOT "file absent" — auth.json
// exists but is a directory — must surface as an error, not be swallowed by
// the missing-master tolerance (which is specifically fs.ErrNotExist-scoped).
func TestInjectCredentials_masterReadErrorSurfaces(t *testing.T) {
	master := t.TempDir()
	t.Setenv("CODEX_HOME", master)
	if err := os.MkdirAll(filepath.Join(master, "auth.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir()

	if _, err := p.InjectCredentials(home); err == nil {
		t.Error("InjectCredentials with a directory at the master auth.json path = nil error; want the read failure to surface")
	}
	if _, err := os.Stat(filepath.Join(instanceCodexHome(home), "auth.json")); !os.IsNotExist(err) {
		t.Errorf("instance auth.json written despite the master read error (%v)", err)
	}
}

// (b) Missing-master tolerance: no master auth.json ⇒ no error and no auth.json
// written, but the CODEX_HOME pin is STILL returned — the pin governs path
// resolution, not authentication (issue #202).
func TestInjectCredentials_missingMasterStillPins(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir()) // empty ⇒ no master auth.json
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir()

	env, err := p.InjectCredentials(home)
	if err != nil {
		t.Fatalf("InjectCredentials with no master: %v", err)
	}
	codexHome := instanceCodexHome(home)
	if len(env) != 1 || env[0] != "CODEX_HOME="+codexHome {
		t.Errorf("env = %v; want the CODEX_HOME pin even with no master auth.json", env)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); !os.IsNotExist(err) {
		t.Errorf("instance auth.json written despite a missing master (%v)", err)
	}
}

// (c) An empty instance home is a programmer error.
func TestInjectCredentials_emptyHomeErrors(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if _, err := p.InjectCredentials(""); err == nil {
		t.Error("InjectCredentials(\"\") = nil error; want a programmer error")
	}
}
