package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

// masterCreds points CLAUDE_CONFIG_DIR at a temp dir seeded with a fake master
// .credentials.json (mirroring logout_test.go's isolateCreds) and returns the
// bytes it wrote, so a copy can be asserted byte-for-byte. Load-bearing: without
// isolation the injector would copy the lab's real credentials.
func masterCreds(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	data := []byte(`{"claudeAiOauth":{"accessToken":"fake-token"}}`)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

// (a) Happy path: the master credentials are copied into
// <home>/.claude/.credentials.json (0600), the master ~/.claude.json's
// oauthAccount is mirrored into <home>/.claude.json (and only that key), and
// the env pins CLAUDE_CONFIG_DIR to EMPTY — the CLI resolves that variable
// over HOME (compat §3a), so a tmux-inherited value would point the instance
// back at the master store; empty = unset keeps resolution on HOME alone.
func TestInjectCredentials_happyPath(t *testing.T) {
	creds := masterCreds(t)
	p, _ := testProvider(t, newFakeRunner())
	// A master config carrying oauthAccount (the CLI's "logged in" metadata)
	// plus an unrelated key that must NOT be dragged into the instance home.
	writeCfg(t, p.configPath, map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "op@example.invalid"},
		"someOtherKey": "master-only",
	})
	home := t.TempDir()

	env, err := p.InjectCredentials(home)
	if err != nil {
		t.Fatalf("InjectCredentials: %v", err)
	}
	if len(env) != 1 || env[0] != "CLAUDE_CONFIG_DIR=" {
		t.Errorf("env = %v; want [CLAUDE_CONFIG_DIR=] (empty pin neutralizing an inherited master pointer)", env)
	}

	dst := instanceCredsUnder(home)
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read instance creds: %v", err)
	}
	if string(got) != string(creds) {
		t.Errorf("instance creds = %q; want the master copy %q", got, creds)
	}
	if info, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("instance creds mode = %o; want 0600", perm)
	}

	homeCfg := readCfg(t, globalConfigUnder(home))
	if acct, _ := homeCfg["oauthAccount"].(map[string]any); acct == nil || acct["emailAddress"] != "op@example.invalid" {
		t.Errorf("oauthAccount not mirrored into the instance config; got %+v", homeCfg)
	}
	if _, present := homeCfg["someOtherKey"]; present {
		t.Errorf("a non-oauthAccount master key leaked into the instance config: %+v", homeCfg)
	}
}

// (b) Missing-master tolerance: no master creds and no master config ⇒ no
// error, and nothing is written into the instance home (the CLI shows its own
// login prompt in the pane). The CLAUDE_CONFIG_DIR= pin still fires — it
// governs PATH RESOLUTION, not authentication.
func TestInjectCredentials_missingMasterTolerated(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // empty ⇒ no master creds
	p, _ := testProvider(t, newFakeRunner())   // p.configPath (master ~/.claude.json) does not exist
	home := t.TempDir()

	env, err := p.InjectCredentials(home)
	if err != nil {
		t.Fatalf("InjectCredentials with no master: %v", err)
	}
	if len(env) != 1 || env[0] != "CLAUDE_CONFIG_DIR=" {
		t.Errorf("env = %v; want [CLAUDE_CONFIG_DIR=] (the pin fires even with no master credential)", env)
	}
	if _, err := os.Stat(instanceCredsUnder(home)); !os.IsNotExist(err) {
		t.Errorf("instance creds written despite a missing master (%v)", err)
	}
	if _, err := os.Stat(globalConfigUnder(home)); !os.IsNotExist(err) {
		t.Errorf("instance config written despite a missing master oauthAccount (%v)", err)
	}
}

// (c) An empty instance home is a programmer error.
func TestInjectCredentials_emptyHomeErrors(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if _, err := p.InjectCredentials(""); err == nil {
		t.Error("InjectCredentials(\"\") = nil error; want a programmer error")
	}
}

// (d) Order independence: the oauthAccount mirror and SeedWorkspace's trust seed
// each preserve the other's keys, so injector-then-seed and seed-then-injector
// both yield an instance config carrying BOTH the account metadata AND the trust
// grants.
func TestInjectCredentials_oauthMergePreservesTrustAndViceVersa(t *testing.T) {
	masterCreds(t)
	worktree := t.TempDir()

	assertBoth := func(t *testing.T, home string) {
		t.Helper()
		cfg := readCfg(t, globalConfigUnder(home))
		if acct, _ := cfg["oauthAccount"].(map[string]any); acct == nil || acct["emailAddress"] != "op@example.invalid" {
			t.Errorf("oauthAccount missing after the combined writes; got %+v", cfg)
		}
		if !trustOf(cfg, worktree) {
			t.Errorf("folder-trust key missing after the combined writes; got %+v", cfg)
		}
		if v, _ := cfg["hasCompletedOnboarding"].(bool); !v {
			t.Errorf("onboarding key missing after the combined writes; got %+v", cfg)
		}
	}

	t.Run("inject then seed", func(t *testing.T) {
		p, _ := testProvider(t, newFakeRunner())
		writeCfg(t, p.configPath, map[string]any{"oauthAccount": map[string]any{"emailAddress": "op@example.invalid"}})
		home := t.TempDir()
		if _, err := p.InjectCredentials(home); err != nil {
			t.Fatalf("InjectCredentials: %v", err)
		}
		if err := seedGlobalConfig(globalConfigUnder(home), worktree); err != nil {
			t.Fatalf("seedGlobalConfig: %v", err)
		}
		assertBoth(t, home)
	})

	t.Run("seed then inject", func(t *testing.T) {
		p, _ := testProvider(t, newFakeRunner())
		writeCfg(t, p.configPath, map[string]any{"oauthAccount": map[string]any{"emailAddress": "op@example.invalid"}})
		home := t.TempDir()
		if err := seedGlobalConfig(globalConfigUnder(home), worktree); err != nil {
			t.Fatalf("seedGlobalConfig: %v", err)
		}
		if _, err := p.InjectCredentials(home); err != nil {
			t.Fatalf("InjectCredentials: %v", err)
		}
		assertBoth(t, home)
	})
}
