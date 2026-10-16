package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// The credential-authority seam (issue #222): CredentialsSig (the opaque
// change-detector), AdoptCredentials (the atomic adopt-back), and
// RefreshCredentials (the blind non-interactive poke). The master store is
// isolated with t.Setenv("CLAUDE_CONFIG_DIR", …) exactly as logout_test.go's
// isolateCreds and inject_test.go's masterCreds do, so no test touches the
// host's real credentials; the refresh recipe runs against a stubbed `claude`
// (fakeClaude), never the live CLI.

// validCreds returns a full logged-in .credentials.json body (accessToken +
// refreshToken + expiresAt) with the given access token — the minimum shape
// credentialsLoggedIn accepts (issue #101). Varying the token length varies the
// file size, which the sig lifecycle test relies on.
func validCreds(access string) string {
	return `{"claudeAiOauth":{"accessToken":"` + access + `","refreshToken":"refresh-` + access + `","expiresAt":1893456000000}}`
}

// loggedOutBodies are the .credentials.json bodies credentialsLoggedIn must
// reject (issue #101): an empty object, the exact shape Claude Code leaves after
// an invalid_grant (block kept, tokens emptied), either token alone empty or
// missing, and non-JSON.
var loggedOutBodies = map[string]string{
	"empty object": `{}`,
	"cleared block": `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,` +
		`"scopes":["user:inference"],"subscriptionType":"max"}}`,
	"empty refresh":   `{"claudeAiOauth":{"accessToken":"a","refreshToken":"","expiresAt":1}}`,
	"empty access":    `{"claudeAiOauth":{"accessToken":"","refreshToken":"r","expiresAt":1}}`,
	"missing refresh": `{"claudeAiOauth":{"accessToken":"a"}}`,
	"null block":      `{"claudeAiOauth":null}`,
	"non-JSON":        `not json at all`,
	"empty file":      ``,
}

// checkSigLifecycle drives the full absent→present→stable→content-change→
// mtime-change lifecycle of CredentialsSig for the credential file at path,
// addressed via home ("" for the master store). It pins the SpoolSig-precedent
// contract: ("", zero) when absent; a non-empty sig whose returned mtime equals
// the file's once present; stability across a no-op re-read; and a changed sig
// on either a size change or an mtime-only bump.
func checkSigLifecycle(t *testing.T, p *Provider, home, path string) {
	t.Helper()

	if sig, mtime := p.CredentialsSig(home); sig != "" || !mtime.IsZero() {
		t.Fatalf("CredentialsSig(%q) absent = (%q, %v); want (\"\", zero time)", home, sig, mtime)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(validCreds("short")), 0o600); err != nil {
		t.Fatal(err)
	}
	sig1, m1 := p.CredentialsSig(home)
	if sig1 == "" {
		t.Fatalf("CredentialsSig(%q) empty after writing %s", home, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !m1.Equal(fi.ModTime()) {
		t.Errorf("CredentialsSig(%q) mtime = %v; want the file's mtime %v", home, m1, fi.ModTime())
	}

	// A no-op re-read is byte-for-byte identical.
	if sig2, _ := p.CredentialsSig(home); sig2 != sig1 {
		t.Errorf("CredentialsSig(%q) changed with no file change: %q -> %q", home, sig1, sig2)
	}

	// A content/size change flips the sig.
	if err := os.WriteFile(path, []byte(validCreds("a-much-longer-rotated-token-value")), 0o600); err != nil {
		t.Fatal(err)
	}
	sig3, _ := p.CredentialsSig(home)
	if sig3 == sig1 {
		t.Errorf("CredentialsSig(%q) unchanged after a size change: %q", home, sig3)
	}

	// An mtime-only bump (same bytes) flips the sig too — a self-refresh that
	// rewrites identical-length token material must still be detectable.
	fi3, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	future := fi3.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	sig4, m4 := p.CredentialsSig(home)
	if sig4 == sig3 {
		t.Errorf("CredentialsSig(%q) unchanged after an mtime bump: %q", home, sig4)
	}
	if fi4, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if !m4.Equal(fi4.ModTime()) {
		t.Errorf("CredentialsSig(%q) mtime after bump = %v; want the file's mtime %v", home, m4, fi4.ModTime())
	}
}

// CredentialsSig("") resolves the MASTER store (the CLAUDE_CONFIG_DIR-aware
// resolver shared with logout.go).
func TestCredentialsSig_master(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	p, _ := testProvider(t, newFakeRunner())
	checkSigLifecycle(t, p, "", filepath.Join(dir, ".credentials.json"))
}

// A non-empty home resolves the instance's private copy at
// <home>/.claude/.credentials.json — the same path InjectCredentials writes.
func TestCredentialsSig_home(t *testing.T) {
	// Point the master elsewhere so the home path is genuinely independent.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir()
	if got := credentialsPathFor(home); got != instanceCredsUnder(home) {
		t.Fatalf("credentialsPathFor(home) = %q; want %q", got, instanceCredsUnder(home))
	}
	checkSigLifecycle(t, p, home, instanceCredsUnder(home))
}

// AdoptCredentials copies the instance's .credentials.json back into the master
// store, atomically, replacing any existing master file, with mode 0600.
func TestAdoptCredentials_copiesInstanceCredsToMaster(t *testing.T) {
	masterDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", masterDir)
	p, _ := testProvider(t, newFakeRunner())

	// A stale master file must be REPLACED, not merged.
	dst := credentialsPath()
	if err := os.WriteFile(dst, []byte(validCreds("stale-master")), 0o600); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	src := instanceCredsUnder(home)
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte(validCreds("rotated-by-instance"))
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.AdoptCredentials(home); err != nil {
		t.Fatalf("AdoptCredentials: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read master creds after adopt: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("master creds = %q; want the instance copy %q", got, want)
	}
	if info, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("master creds mode = %o; want 0600", perm)
	}
}

// CredentialsSig reads a logged-out file exactly like an absent one (issue
// #101): ("", zero time), for an instance home AND the master, for every shape
// credentialsLoggedIn rejects. A valid body beside them still yields a sig.
func TestCredentialsSig_loggedOutReadsAsAbsent(t *testing.T) {
	masterDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", masterDir)
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir()
	targets := map[string]struct{ home, path string }{
		"instance": {home, instanceCredsUnder(home)},
		"master":   {"", filepath.Join(masterDir, ".credentials.json")},
	}
	for tname, tgt := range targets {
		if err := os.MkdirAll(filepath.Dir(tgt.path), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range loggedOutBodies {
			t.Run(tname+"/"+name, func(t *testing.T) {
				if err := os.WriteFile(tgt.path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if sig, mtime := p.CredentialsSig(tgt.home); sig != "" || !mtime.IsZero() {
					t.Errorf("CredentialsSig(%q) for %s = (%q, %v); want (\"\", zero time)", tgt.home, name, sig, mtime)
				}
			})
		}
		t.Run(tname+"/valid", func(t *testing.T) {
			if err := os.WriteFile(tgt.path, []byte(validCreds("ok")), 0o600); err != nil {
				t.Fatal(err)
			}
			if sig, mtime := p.CredentialsSig(tgt.home); sig == "" || mtime.IsZero() {
				t.Errorf("CredentialsSig(%q) for a valid block = (%q, %v); want a non-empty sig", tgt.home, sig, mtime)
			}
		})
	}
}

// AdoptCredentials refuses a logged-out instance file with an error wrapping
// provider.ErrCredentialsLoggedOut and leaves the master untouched (issue #101):
// existing master bytes and mtime are unchanged, and an absent master (dir
// included) is not created.
func TestAdoptCredentials_refusesLoggedOut(t *testing.T) {
	for name, body := range loggedOutBodies {
		t.Run(name+"/master-present", func(t *testing.T) {
			masterDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", masterDir)
			p, _ := testProvider(t, newFakeRunner())
			dst := credentialsPath()
			want := []byte(validCreds("healthy-master"))
			if err := os.WriteFile(dst, want, 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			if err := os.Chtimes(dst, old, old); err != nil {
				t.Fatal(err)
			}

			home := t.TempDir()
			src := instanceCredsUnder(home)
			if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			err := p.AdoptCredentials(home)
			if !errors.Is(err, provider.ErrCredentialsLoggedOut) {
				t.Fatalf("AdoptCredentials(%s) = %v; want errors.Is ErrCredentialsLoggedOut", name, err)
			}
			if body != "" && strings.Contains(err.Error(), body) {
				t.Errorf("error %q leaks the credential file content", err)
			}
			got, rerr := os.ReadFile(dst)
			if rerr != nil || string(got) != string(want) {
				t.Errorf("master after refused adopt = %q, %v; want untouched %q", got, rerr, want)
			}
			if fi, serr := os.Stat(dst); serr != nil || !fi.ModTime().Equal(old) {
				t.Errorf("master mtime after refused adopt = %v, %v; want untouched %v", fi.ModTime(), serr, old)
			}
		})
		t.Run(name+"/master-absent", func(t *testing.T) {
			masterDir := filepath.Join(t.TempDir(), "master-config") // does not exist
			t.Setenv("CLAUDE_CONFIG_DIR", masterDir)
			p, _ := testProvider(t, newFakeRunner())
			home := t.TempDir()
			src := instanceCredsUnder(home)
			if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := p.AdoptCredentials(home); !errors.Is(err, provider.ErrCredentialsLoggedOut) {
				t.Fatalf("AdoptCredentials(%s) = %v; want errors.Is ErrCredentialsLoggedOut", name, err)
			}
			if _, err := os.Stat(masterDir); !os.IsNotExist(err) {
				t.Errorf("master dir after refused adopt: stat err = %v; want not-exist (master must not be created)", err)
			}
		})
	}
}

// A MISSING instance credential file is an error — adoption is only requested
// after a sig proved the file existed, so a vanished file must surface.
func TestAdoptCredentials_missingSourceErrors(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir() // no .claude/.credentials.json under it
	if err := p.AdoptCredentials(home); err == nil {
		t.Error("AdoptCredentials with a missing instance credential file = nil; want an error")
	}
}

// A master config dir that does not exist yet is created (0700) before the copy.
func TestAdoptCredentials_createsMissingMasterDir(t *testing.T) {
	base := t.TempDir()
	masterDir := filepath.Join(base, "master-config") // does not exist yet
	t.Setenv("CLAUDE_CONFIG_DIR", masterDir)
	p, _ := testProvider(t, newFakeRunner())

	home := t.TempDir()
	src := instanceCredsUnder(home)
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(validCreds("x")), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.AdoptCredentials(home); err != nil {
		t.Fatalf("AdoptCredentials into a missing master dir: %v", err)
	}
	if info, err := os.Stat(masterDir); err != nil {
		t.Fatalf("master dir not created: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("master dir mode = %o; want 0700", perm)
	}
	if _, err := os.Stat(credentialsPath()); err != nil {
		t.Errorf("master creds not written after adopt: %v", err)
	}
}

// An empty instance home is a programmer error.
func TestAdoptCredentials_emptyHomeErrors(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if err := p.AdoptCredentials(""); err == nil {
		t.Error("AdoptCredentials(\"\") = nil; want a programmer error")
	}
}

// The former withMasterConfigDir helper's single-master-pin contract now
// lives on the CLI-runner seam (provider.CLIInvocation.Env's
// filter-then-append semantics, issue #206), unit-pinned in
// internal/provider/hostcli_test.go; TestRefreshCredentials_pokeArgvEnvAndCwd
// below still proves the end-to-end result — the stub sees exactly ONE
// master-pointing CLAUDE_CONFIG_DIR entry.

// refreshRecord parses the record file the refresh stub writes.
type refreshRecord struct {
	args []string
	cwd  string
	ccd  string // CLAUDE_CONFIG_DIR seen by the stub
	ccdN int    // number of CLAUDE_CONFIG_DIR entries in the stub's environment
}

func parseRefreshRecord(t *testing.T, path string) refreshRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read refresh record: %v", err)
	}
	var r refreshRecord
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "arg="):
			r.args = append(r.args, strings.TrimPrefix(line, "arg="))
		case strings.HasPrefix(line, "cwd="):
			r.cwd = strings.TrimPrefix(line, "cwd=")
		case strings.HasPrefix(line, "ccd="):
			r.ccd = strings.TrimPrefix(line, "ccd=")
		case strings.HasPrefix(line, "ccdN="):
			r.ccdN, _ = strconv.Atoi(strings.TrimPrefix(line, "ccdN="))
		}
	}
	return r
}

// The refresh poke runs the probe-confirmed non-interactive recipe (compat §3b):
// `claude -p --model haiku ok`, working dir + CLAUDE_CONFIG_DIR pinned to the
// master config dir, with exactly one (master-pointing) CLAUDE_CONFIG_DIR entry.
func TestRefreshCredentials_pokeArgvEnvAndCwd(t *testing.T) {
	masterDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", masterDir)
	record := filepath.Join(t.TempDir(), "poke.txt")
	t.Setenv("REFRESH_RECORD", record)

	p, _ := testProvider(t, newFakeRunner())
	p.claudeBin = fakeClaude(t, `: > "$REFRESH_RECORD"
for a in "$@"; do echo "arg=$a" >> "$REFRESH_RECORD"; done
echo "cwd=$(pwd -P)" >> "$REFRESH_RECORD"
echo "ccd=$CLAUDE_CONFIG_DIR" >> "$REFRESH_RECORD"
echo "ccdN=$(env | grep -c '^CLAUDE_CONFIG_DIR=')" >> "$REFRESH_RECORD"`)

	if err := p.RefreshCredentials(context.Background()); err != nil {
		t.Fatalf("RefreshCredentials: %v", err)
	}
	r := parseRefreshRecord(t, record)

	wantArgv := []string{"-p", "--model", "haiku", "ok"}
	if strings.Join(r.args, "|") != strings.Join(wantArgv, "|") {
		t.Errorf("poke argv = %v; want %v", r.args, wantArgv)
	}
	if r.ccd != masterDir {
		t.Errorf("stub CLAUDE_CONFIG_DIR = %q; want the master dir %q", r.ccd, masterDir)
	}
	if r.ccdN != 1 {
		t.Errorf("stub saw %d CLAUDE_CONFIG_DIR entries; want exactly 1 (the master pin, no inherited duplicate)", r.ccdN)
	}
	wantCwd, _ := filepath.EvalSymlinks(masterDir)
	if r.cwd != wantCwd {
		t.Errorf("poke cwd = %q; want the master dir %q", r.cwd, wantCwd)
	}
}

// A failing poke surfaces the CLI's stderr in the returned error.
func TestRefreshCredentials_failureCarriesStderr(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p, _ := testProvider(t, newFakeRunner())
	p.claudeBin = fakeClaude(t, `echo 'boom-refresh-failed' 1>&2; exit 1`)

	err := p.RefreshCredentials(context.Background())
	if err == nil {
		t.Fatal("RefreshCredentials with a failing poke = nil; want an error")
	}
	if !strings.Contains(err.Error(), "boom-refresh-failed") {
		t.Errorf("RefreshCredentials error = %q; want it to carry the stub's stderr", err)
	}
}

// The CLI prints its login error (`Not logged in · Please run /login`) on
// STDOUT with empty stderr (issue #101): the failure message must carry that
// stdout tail, labelled, instead of "no stderr captured". Stderr still wins when
// present, and a success never surfaces output.
func TestRefreshCredentials_failureCarriesStdoutWhenStderrEmpty(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p, _ := testProvider(t, newFakeRunner())
	p.claudeBin = fakeClaude(t, `echo 'Not logged in · Please run /login'; exit 1`)

	err := p.RefreshCredentials(context.Background())
	if err == nil {
		t.Fatal("RefreshCredentials with a failing poke = nil; want an error")
	}
	if !strings.Contains(err.Error(), "stdout: Not logged in · Please run /login") {
		t.Errorf("RefreshCredentials error = %q; want it to carry the stub's stdout tail", err)
	}
	if strings.Contains(err.Error(), "no stderr captured") {
		t.Errorf("RefreshCredentials error = %q; must not claim no output when stdout has the message", err)
	}
}

// With neither stream populated the fixed marker remains.
func TestRefreshCredentials_failureNoOutputMarker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p, _ := testProvider(t, newFakeRunner())
	p.claudeBin = fakeClaude(t, `exit 1`)
	err := p.RefreshCredentials(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no stderr captured") {
		t.Errorf("RefreshCredentials error = %v; want the no-output marker", err)
	}
}

// The detection path core relies on: when the poke rotates the master store, the
// master sig changes across the call. The stub stands in for the real CLI's
// in-place rotation by rewriting .credentials.json under the pinned config dir.
func TestRefreshCredentials_rotationChangesMasterSig(t *testing.T) {
	masterDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", masterDir)
	master := filepath.Join(masterDir, ".credentials.json")
	if err := os.WriteFile(master, []byte(validCreds("old")), 0o600); err != nil {
		t.Fatal(err)
	}

	p, _ := testProvider(t, newFakeRunner())
	// The poke rewrites the master store in place with a fresh (longer) family,
	// exactly as `claude -p` does when it decides the token is near expiry.
	p.claudeBin = fakeClaude(t, `echo '`+validCreds("rotated-and-noticeably-longer")+`' > "$CLAUDE_CONFIG_DIR/.credentials.json"`)

	before, _ := p.CredentialsSig("")
	if before == "" {
		t.Fatal("master sig empty before the poke; test setup is wrong")
	}
	if err := p.RefreshCredentials(context.Background()); err != nil {
		t.Fatalf("RefreshCredentials: %v", err)
	}
	after, _ := p.CredentialsSig("")
	if after == before {
		t.Errorf("master sig unchanged across a rotating poke (%q); core could not detect the rotation", after)
	}
}

// Full round-trip (issue #222): seed master → InjectCredentials(home) →
// non-empty home sig → an instance self-refresh diverges the home sig →
// AdoptCredentials brings the rotated family back into the master byte-for-byte.
func TestCredauthority_RoundTrip(t *testing.T) {
	master := masterCreds(t) // sets CLAUDE_CONFIG_DIR + writes the master creds
	p, _ := testProvider(t, newFakeRunner())
	home := t.TempDir()

	if _, err := p.InjectCredentials(home); err != nil {
		t.Fatalf("InjectCredentials: %v", err)
	}
	if got, err := os.ReadFile(instanceCredsUnder(home)); err != nil {
		t.Fatalf("read injected creds: %v", err)
	} else if string(got) != string(master) {
		t.Fatalf("injected creds = %q; want the master copy %q", got, master)
	}
	sigInjected, _ := p.CredentialsSig(home)
	if sigInjected == "" {
		t.Fatal("CredentialsSig(home) empty after InjectCredentials")
	}

	// Simulate the instance CLI self-refreshing its own private copy.
	rotated := []byte(validCreds("rotated-inside-the-instance-run"))
	if err := os.WriteFile(instanceCredsUnder(home), rotated, 0o600); err != nil {
		t.Fatal(err)
	}
	sigRotated, _ := p.CredentialsSig(home)
	if sigRotated == sigInjected {
		t.Errorf("home sig unchanged after a simulated self-refresh; core could not detect the divergence from the injected sig")
	}

	// Adopt the still-valid rotated family back into the master store.
	if err := p.AdoptCredentials(home); err != nil {
		t.Fatalf("AdoptCredentials: %v", err)
	}
	masterNow, err := os.ReadFile(credentialsPath())
	if err != nil {
		t.Fatalf("read master creds after adopt: %v", err)
	}
	if string(masterNow) != string(rotated) {
		t.Errorf("master creds after adopt = %q; want the instance's rotated copy %q", masterNow, rotated)
	}
}
