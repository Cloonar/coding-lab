package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- CredentialsSig ---------------------------------------------------

// (a) Master resolution: missing → ("", zero); write auth.json → non-empty
// sig with mtime matching the file; unchanged → identical; a size change and
// a bump-only mtime change each move the sig.
func TestCredentialsSig_master(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	p, _ := testProvider(t, newFakeRunner())

	if sig, mtime := p.CredentialsSig(""); sig != "" || !mtime.IsZero() {
		t.Fatalf("missing master: sig=%q mtime=%v; want (\"\", zero)", sig, mtime)
	}

	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sig1, mtime1 := p.CredentialsSig("")
	if sig1 == "" {
		t.Fatal("sig empty after writing master auth.json")
	}
	fi, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if !mtime1.Equal(fi.ModTime()) {
		t.Errorf("mtime = %v; want the file's mtime %v", mtime1, fi.ModTime())
	}

	if sig1b, _ := p.CredentialsSig(""); sig1b != sig1 {
		t.Errorf("sig changed with no write: %q vs %q", sig1, sig1b)
	}

	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"a-much-longer-token-value"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sig2, _ := p.CredentialsSig("")
	if sig2 == sig1 {
		t.Error("sig unchanged after a content/size change")
	}

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(authPath, future, future); err != nil {
		t.Fatal(err)
	}
	sig3, _ := p.CredentialsSig("")
	if sig3 == sig2 {
		t.Error("sig unchanged after an mtime-only bump")
	}
}

// (b) Instance-home resolution: the same missing/non-empty/unchanged/changed
// behavior, keyed off <home>/.codex/auth.json (InjectCredentials' own path).
func TestCredentialsSig_instanceHome(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir()

	if sig, mtime := p.CredentialsSig(home); sig != "" || !mtime.IsZero() {
		t.Fatalf("missing instance home: sig=%q mtime=%v; want (\"\", zero)", sig, mtime)
	}

	codexHome := instanceCodexHome(home)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sig1, mtime1 := p.CredentialsSig(home)
	if sig1 == "" {
		t.Fatal("sig empty after writing instance auth.json")
	}
	fi, err := os.Stat(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if !mtime1.Equal(fi.ModTime()) {
		t.Errorf("mtime = %v; want the file's mtime %v", mtime1, fi.ModTime())
	}

	if sig1b, _ := p.CredentialsSig(home); sig1b != sig1 {
		t.Errorf("sig changed with no write: %q vs %q", sig1, sig1b)
	}

	if err := os.WriteFile(authPath, []byte(`{"tokens":{"access_token":"x-much-longer-token-value"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sig2, _ := p.CredentialsSig(home)
	if sig2 == sig1 {
		t.Error("sig unchanged after a content/size change")
	}

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(authPath, future, future); err != nil {
		t.Fatal(err)
	}
	sig3, _ := p.CredentialsSig(home)
	if sig3 == sig2 {
		t.Error("sig unchanged after an mtime-only bump")
	}
}

// --- AdoptCredentials ---------------------------------------------------

// (a) Happy path: an instance home's auth.json is copied into the master
// store byte-identical, at 0600, and the master dir is created 0700 when it
// did not already exist.
func TestAdoptCredentials_happyPath(t *testing.T) {
	masterDir := filepath.Join(t.TempDir(), "not-yet-created")
	t.Setenv("CODEX_HOME", masterDir)
	p, _ := testProvider(t, newFakeRunner())

	home := t.TempDir()
	codexHome := instanceCodexHome(home)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"tokens":{"access_token":"instance-rotated"}}`)
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.AdoptCredentials(home); err != nil {
		t.Fatalf("AdoptCredentials: %v", err)
	}

	dst := filepath.Join(masterDir, "auth.json")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("master auth.json = %q; want %q", got, data)
	}
	if info, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("master auth.json mode = %o; want 0600", perm)
	}
	if info, err := os.Stat(masterDir); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("master dir mode = %o; want 0700 (created fresh by AdoptCredentials)", perm)
	}
}

// (b) An existing master auth.json is replaced, not merged/appended.
func TestAdoptCredentials_replacesExisting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("stale-master-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ := testProvider(t, newFakeRunner())

	home := t.TempDir()
	codexHome := instanceCodexHome(home)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("fresh-instance-token")
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.AdoptCredentials(home); err != nil {
		t.Fatalf("AdoptCredentials: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("master auth.json = %q; want the instance's %q", got, data)
	}
}

// (c) A missing instance auth.json is an error — adoption is only ever
// requested after a sig proved the file existed.
func TestAdoptCredentials_missingSourceErrors(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir() // no .codex/auth.json under it

	if err := p.AdoptCredentials(home); err == nil {
		t.Error("AdoptCredentials with no instance auth.json = nil error; want an error")
	}
}

// (d) An empty instance home is a programmer error.
func TestAdoptCredentials_emptyHomeErrors(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if err := p.AdoptCredentials(""); err == nil {
		t.Error("AdoptCredentials(\"\") = nil error; want a programmer error")
	}
}

// --- RefreshCredentials -------------------------------------------------

// (a) The exact argv lands, and CODEX_HOME in the child's env is forced to
// the master codex home directory — even though the provider under test
// carries OTHER, unrelated directories on its LoginDir/ConfigPath fields
// (testProvider's own construction), proving RefreshCredentials resolves the
// master dynamically via credentialsPath() rather than reusing a
// construction-time-cached field. Also asserts the override actually
// REPLACES any inherited CODEX_HOME rather than appending a same-key
// duplicate (the getenv-first-match hazard overrideEnv defends against).
func TestRefreshCredentials_argvAndEnv(t *testing.T) {
	isolateAuth(t) // CODEX_HOME -> a temp master dir carrying auth.json
	p, _ := testProvider(t, newFakeRunner())

	argvFile := filepath.Join(t.TempDir(), "argv.txt")
	envFile := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("LAB_TEST_ARGV_FILE", argvFile)
	t.Setenv("LAB_TEST_ENV_FILE", envFile)

	p.codexBin = fakeCodex(t, `printf '%s\n' "$@" > "$LAB_TEST_ARGV_FILE"
env > "$LAB_TEST_ENV_FILE"
exit 0`)

	if err := p.RefreshCredentials(context.Background()); err != nil {
		t.Fatalf("RefreshCredentials: %v", err)
	}

	argvBytes, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	gotArgv := strings.Split(strings.TrimRight(string(argvBytes), "\n"), "\n")
	wantArgv := []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "ok"}
	if !slices.Equal(gotArgv, wantArgv) {
		t.Errorf("argv = %v; want %v", gotArgv, wantArgv)
	}

	masterDir := filepath.Dir(p.credentialsPath())
	envBytes, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	var codexHomeVals []string
	for _, line := range strings.Split(string(envBytes), "\n") {
		if v, ok := strings.CutPrefix(line, "CODEX_HOME="); ok {
			codexHomeVals = append(codexHomeVals, v)
		}
	}
	if len(codexHomeVals) != 1 {
		t.Fatalf("child env carries %d CODEX_HOME entries; want exactly 1: %v", len(codexHomeVals), codexHomeVals)
	}
	if codexHomeVals[0] != masterDir {
		t.Errorf("child CODEX_HOME = %q; want the master dir %q", codexHomeVals[0], masterDir)
	}
	if codexHomeVals[0] == p.loginDir || codexHomeVals[0] == filepath.Dir(p.configPath) {
		t.Errorf("child CODEX_HOME %q accidentally matched an unrelated provider field (loginDir=%q configPath=%q)",
			codexHomeVals[0], p.loginDir, p.configPath)
	}
}

// (b) A failing stub surfaces its stderr in the returned error.
func TestRefreshCredentials_failureSurfacesStderr(t *testing.T) {
	isolateAuth(t)
	p, _ := testProvider(t, newFakeRunner())
	p.codexBin = fakeCodex(t, `echo 'boom: usage limit exceeded' 1>&2
exit 1`)

	err := p.RefreshCredentials(context.Background())
	if err == nil {
		t.Fatal("RefreshCredentials with a failing stub = nil error; want the stub's stderr surfaced")
	}
	if !strings.Contains(err.Error(), "boom: usage limit exceeded") {
		t.Errorf("RefreshCredentials error = %q; want it to carry the stub's stderr", err)
	}
}

// (c) A stub that rewrites the master auth.json (standing in for a real CLI
// self-refresh) changes CredentialsSig("") across the call — the observation
// mechanism callers rely on, since RefreshCredentials' own return carries no
// rotation signal.
func TestRefreshCredentials_rewriteChangesMasterSig(t *testing.T) {
	isolateAuth(t)
	p, _ := testProvider(t, newFakeRunner())
	sigBefore, _ := p.CredentialsSig("")
	if sigBefore == "" {
		t.Fatal("precondition: master sig should be non-empty before the probe")
	}

	t.Setenv("LAB_TEST_NEW_TOKEN", `{"tokens":{"access_token":"rotated-by-stub"}}`)
	p.codexBin = fakeCodex(t, `printf '%s' "$LAB_TEST_NEW_TOKEN" > "$CODEX_HOME/auth.json"
exit 0`)

	if err := p.RefreshCredentials(context.Background()); err != nil {
		t.Fatalf("RefreshCredentials: %v", err)
	}
	sigAfter, _ := p.CredentialsSig("")
	if sigAfter == sigBefore {
		t.Error("CredentialsSig(\"\") unchanged after the stub rewrote the master auth.json")
	}
}

// --- Round-trip ----------------------------------------------------------

// Seed the master, InjectCredentials into an instance home, simulate that
// instance's CLI self-refreshing its token in place, and confirm
// AdoptCredentials pulls the rotated bytes back into the master
// byte-for-byte — the full loop the credential-authority seam exists to
// drive (issue #222).
func TestCredentialAuthority_roundTrip(t *testing.T) {
	creds := isolateAuth(t)
	master, err := os.ReadFile(creds)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir()

	if _, err := p.InjectCredentials(home); err != nil {
		t.Fatalf("InjectCredentials: %v", err)
	}
	homeAuth := filepath.Join(instanceCodexHome(home), "auth.json")
	got, err := os.ReadFile(homeAuth)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(master) {
		t.Fatalf("home auth.json after inject = %q; want the master copy %q", got, master)
	}

	sig, _ := p.CredentialsSig(home)
	if sig == "" {
		t.Fatal("sig empty after InjectCredentials")
	}

	// Simulate the instance CLI self-refreshing its token in place.
	rotated := []byte(`{"tokens":{"access_token":"self-refreshed"}}`)
	if err := os.WriteFile(homeAuth, rotated, 0o600); err != nil {
		t.Fatal(err)
	}
	sig2, _ := p.CredentialsSig(home)
	if sig2 == sig {
		t.Error("sig unchanged after the simulated self-refresh")
	}

	if err := p.AdoptCredentials(home); err != nil {
		t.Fatalf("AdoptCredentials: %v", err)
	}
	gotMaster, err := os.ReadFile(creds)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMaster) != string(rotated) {
		t.Errorf("master auth.json after adopt = %q; want the home's rotated bytes %q", gotMaster, rotated)
	}
}
