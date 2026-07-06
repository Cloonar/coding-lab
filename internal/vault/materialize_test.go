package vault

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

func newTestMaterializer(t *testing.T) *Materializer {
	t.Helper()
	m, err := NewMaterializer(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	return m
}

func TestNewMaterializerCreatesDir0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	m, err := NewMaterializer(dir)
	if err != nil {
		t.Fatalf("NewMaterializer: %v", err)
	}
	if m.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", m.Dir(), dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("runtime dir mode %v, want directory 0700", fi.Mode())
	}
}

func TestNewMaterializerTightensExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMaterializer(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("existing dir left at %04o, want tightened to 0700", fi.Mode().Perm())
	}
}

func TestMaterializeSSHKey(t *testing.T) {
	m := newTestMaterializer(t)
	const keyBody = "-----BEGIN OPENSSH PRIVATE KEY-----\nabcdef\n-----END OPENSSH PRIVATE KEY-----"

	for name, in := range map[string]string{
		"without trailing newline": keyBody,
		"with trailing newline":    keyBody + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path, sshPass, err := m.MaterializeSSHKey("cred_aa11", "op_1", SSHKeyPayload{PrivateKey: in})
			if err != nil {
				t.Fatalf("MaterializeSSHKey: %v", err)
			}
			if path != filepath.Join(m.Dir(), "cred_aa11.op_1.key") {
				t.Errorf("path = %q", path)
			}
			if sshPass != "" {
				t.Errorf("sshAskpassPath = %q for a passphrase-less key, want empty", sshPass)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("key file mode %04o, want 0600", fi.Mode().Perm())
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != keyBody+"\n" {
				t.Errorf("key content %q, want body with exactly one trailing newline", got)
			}
		})
	}
}

// Regression (materialize.go CRLF finding): a Windows-pasted key arrives
// with \r\n line endings; OpenSSH rejects keys with \r bytes, so
// materialization must normalize them to LF.
func TestMaterializeSSHKeyNormalizesCRLF(t *testing.T) {
	m := newTestMaterializer(t)
	const want = "-----BEGIN OPENSSH PRIVATE KEY-----\nabcdef\n-----END OPENSSH PRIVATE KEY-----\n"
	for name, in := range map[string]string{
		"crlf":                 strings.ReplaceAll(want, "\n", "\r\n"),
		"crlf without final":   strings.TrimSuffix(strings.ReplaceAll(want, "\n", "\r\n"), "\r\n"),
		"lone cr line endings": strings.ReplaceAll(want, "\n", "\r"),
	} {
		t.Run(name, func(t *testing.T) {
			path, _, err := m.MaterializeSSHKey("cred_crlf", "op_1", SSHKeyPayload{PrivateKey: in})
			if err != nil {
				t.Fatalf("MaterializeSSHKey: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.ContainsRune(got, '\r') {
				t.Errorf("materialized key still contains \\r: %q", got)
			}
			if string(got) != want {
				t.Errorf("key content %q, want %q", got, want)
			}
		})
	}
}

func TestMaterializeSSHKeyReplaces(t *testing.T) {
	m := newTestMaterializer(t)
	if _, _, err := m.MaterializeSSHKey("cred_x", "op_1", SSHKeyPayload{PrivateKey: "old"}); err != nil {
		t.Fatal(err)
	}
	path, _, err := m.MaterializeSSHKey("cred_x", "op_1", SSHKeyPayload{PrivateKey: "new"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new\n" {
		t.Errorf("re-materialize left %q, want %q", got, "new\n")
	}
}

// Regression (design D9 passphrase wiring): a payload with a passphrase
// materializes an additional 0700 SSH askpass helper echoing it, and
// SSHEnvWithPassphrase wires it up. The passphrase must never land in
// the key file itself.
func TestMaterializeSSHKeyWithPassphrase(t *testing.T) {
	testutil.RequireTool(t, "sh")
	m := newTestMaterializer(t)
	// Hostile-looking passphrase: quotes, spaces, $, backticks.
	const passphrase = `pa'ss $(echo pwned) ` + "`id`" + ` end`
	keyPath, sshPass, err := m.MaterializeSSHKey("cred_pp", "op_7", SSHKeyPayload{
		PrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
		Passphrase: passphrase,
	})
	if err != nil {
		t.Fatalf("MaterializeSSHKey: %v", err)
	}
	if sshPass != filepath.Join(m.Dir(), "cred_pp.op_7.sshpass") {
		t.Errorf("sshAskpassPath = %q", sshPass)
	}
	fi, err := os.Stat(sshPass)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("sshpass helper mode %04o, want 0700", fi.Mode().Perm())
	}

	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(keyBytes), passphrase) {
		t.Error("passphrase was written into the key file")
	}

	// ssh invokes SSH_ASKPASS with the prompt as its only argument and
	// reads the passphrase from stdout.
	out, err := exec.Command(sshPass, "Enter passphrase for key '/x/id':").Output()
	if err != nil {
		t.Fatalf("sshpass helper: %v", err)
	}
	if string(out) != passphrase+"\n" {
		t.Errorf("helper output %q, want passphrase + newline", out)
	}
}

// Integration regression for the D9 passphrase wiring: a REAL
// passphrase-protected ed25519 key (ssh-keygen -N) is materialized and
// OpenSSH itself (ssh-keygen -y, which uses the same read_passphrase /
// SSH_ASKPASS machinery as ssh) must load it using only the env from
// SSHEnvWithPassphrase — and must fail with a wrong-passphrase helper.
func TestOpenSSHLoadsPassphraseProtectedKey(t *testing.T) {
	testutil.RequireTool(t, "ssh-keygen")
	testutil.RequireTool(t, "sh")

	const passphrase = "correct horse's battery staple"
	keyDir := t.TempDir()
	genPath := filepath.Join(keyDir, "id_ed25519")
	gen := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", passphrase, "-C", "lab-test", "-f", genPath)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	priv, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(genPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	m := newTestMaterializer(t)
	keyPath, sshPass, err := m.MaterializeSSHKey("cred_int", "op_int", SSHKeyPayload{
		PrivateKey: string(priv),
		Passphrase: passphrase,
	})
	if err != nil {
		t.Fatalf("MaterializeSSHKey: %v", err)
	}

	loadPub := func(helperPath string) ([]byte, error) {
		env := SSHEnvWithPassphrase(keyPath, m.KnownHostsPath(), helperPath)
		cmd := exec.Command("ssh-keygen", "-y", "-f", keyPath)
		// Hermetic env: PATH for sh, plus exactly the SSH_ASKPASS wiring
		// entries (env[0] is GIT_SSH_COMMAND, consumed by git, not ssh-keygen).
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + keyDir}, env[1:]...)
		return cmd.Output()
	}

	out, err := loadPub(sshPass)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("ssh-keygen -y with materialized passphrase helper: %v: %s", err, ee.Stderr)
		}
		t.Fatalf("ssh-keygen -y with materialized passphrase helper: %v", err)
	}
	gotFields := strings.Fields(string(out))
	wantFields := strings.Fields(string(pub))
	if len(gotFields) < 2 || len(wantFields) < 2 || gotFields[0] != wantFields[0] || gotFields[1] != wantFields[1] {
		t.Errorf("public key derived under passphrase helper = %q, want %q", out, pub)
	}

	// Negative control: a helper echoing the wrong passphrase must fail,
	// proving the key really is encrypted and the helper is what unlocks it.
	_, wrongHelper, err := m.MaterializeSSHKey("cred_int", "op_wrong", SSHKeyPayload{
		PrivateKey: string(priv),
		Passphrase: "not the passphrase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadPub(wrongHelper); err == nil {
		t.Error("ssh-keygen -y succeeded with a wrong-passphrase helper; key not actually passphrase-protected?")
	}
}

func TestMaterializeAskpass(t *testing.T) {
	testutil.RequireTool(t, "sh")
	m := newTestMaterializer(t)
	// Hostile-looking values: quotes, spaces, $, backticks, backslash.
	payload := HTTPSTokenPayload{
		Username: "lab user's-bot",
		Token:    `to'ken $(echo pwned) ` + "`id`" + ` \x END`,
	}
	path, err := m.MaterializeAskpass("cred_bb22", "op_1", payload)
	if err != nil {
		t.Fatalf("MaterializeAskpass: %v", err)
	}
	if path != filepath.Join(m.Dir(), "cred_bb22.op_1.askpass") {
		t.Errorf("path = %q", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("askpass mode %04o, want 0700", fi.Mode().Perm())
	}

	// git invokes GIT_ASKPASS with only the prompt as argv — the token
	// itself never appears in any argv (it lives in the 0700 script body,
	// the design-accepted variant).
	run := func(prompt string) string {
		t.Helper()
		out, err := exec.Command(path, prompt).Output()
		if err != nil {
			t.Fatalf("askpass %q: %v", prompt, err)
		}
		return string(out)
	}
	if got := run("Username for 'https://git.cloonar.com':"); got != payload.Username+"\n" {
		t.Errorf("username answer %q, want %q", got, payload.Username+"\n")
	}
	if got := run("Password for 'https://lab@git.cloonar.com':"); got != payload.Token+"\n" {
		t.Errorf("password answer %q, want %q", got, payload.Token+"\n")
	}
	if err := exec.Command(path, "Passphrase for key:").Run(); err == nil {
		t.Error("askpass answered an unrelated prompt, want exit 1")
	}
}

// Regression (shared-credential cleanup race): materialized files are
// keyed per (credID, opID), so one op's cleanup must never unlink a
// concurrent op's files for the same credential. Tested in both cleanup
// orders.
func TestCleanupPerOpDoesNotTouchOtherOps(t *testing.T) {
	const credID = "cred_shared"
	type opFiles struct {
		key, sshPass, askpass string
	}
	materialize := func(t *testing.T, m *Materializer, opID string) opFiles {
		t.Helper()
		key, sshPass, err := m.MaterializeSSHKey(credID, opID, SSHKeyPayload{
			PrivateKey: "key-" + opID,
			Passphrase: "pp-" + opID,
		})
		if err != nil {
			t.Fatal(err)
		}
		askpass, err := m.MaterializeAskpass(credID, opID, HTTPSTokenPayload{Username: "u", Token: "t-" + opID})
		if err != nil {
			t.Fatal(err)
		}
		return opFiles{key: key, sshPass: sshPass, askpass: askpass}
	}

	for _, order := range [][2]string{{"op_a", "op_b"}, {"op_b", "op_a"}} {
		first, second := order[0], order[1]
		t.Run("cleanup "+first+" first", func(t *testing.T) {
			m := newTestMaterializer(t)
			files := map[string]opFiles{
				"op_a": materialize(t, m, "op_a"),
				"op_b": materialize(t, m, "op_b"),
			}

			if err := m.Cleanup(credID, first); err != nil {
				t.Fatalf("Cleanup(%s): %v", first, err)
			}
			for _, p := range []string{files[first].key, files[first].sshPass, files[first].askpass} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s still exists after its op's Cleanup", p)
				}
			}
			// The other op's files survive with their exact content — the
			// race's failure mode was git/ssh finding them gone (or
			// replaced) mid-op.
			if got, err := os.ReadFile(files[second].key); err != nil {
				t.Errorf("other op's key gone after Cleanup(%s): %v", first, err)
			} else if want := "key-" + second + "\n"; string(got) != want {
				t.Errorf("other op's key content %q, want %q", got, want)
			}
			for _, p := range []string{files[second].sshPass, files[second].askpass} {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("other op's file gone after Cleanup(%s): %v", first, err)
				}
			}

			if err := m.Cleanup(credID, second); err != nil {
				t.Fatalf("Cleanup(%s): %v", second, err)
			}
			for _, p := range []string{files[second].key, files[second].sshPass, files[second].askpass} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s still exists after its op's Cleanup", p)
				}
			}
		})
	}
}

// Concurrent variant of the shared-credential regression: many ops on
// one credID materialize, read back, and clean up in parallel; any
// cross-op interference surfaces as a vanished file or foreign content
// (run with -race).
func TestConcurrentOpsOnSharedCredential(t *testing.T) {
	m := newTestMaterializer(t)
	const credID = "cred_conc"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opID := fmt.Sprintf("op%d", i)
			want := "key-" + opID + "\n"
			for j := 0; j < 25; j++ {
				path, _, err := m.MaterializeSSHKey(credID, opID, SSHKeyPayload{PrivateKey: "key-" + opID})
				if err != nil {
					t.Errorf("op %s: materialize: %v", opID, err)
					return
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("op %s: key vanished mid-op: %v", opID, err)
					return
				}
				if string(got) != want {
					t.Errorf("op %s: key content %q, want %q", opID, got, want)
					return
				}
				if err := m.Cleanup(credID, opID); err != nil {
					t.Errorf("op %s: cleanup: %v", opID, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestCleanup(t *testing.T) {
	m := newTestMaterializer(t)
	keyPath, sshPass, err := m.MaterializeSSHKey("cred_1", "op_1", SSHKeyPayload{PrivateKey: "k", Passphrase: "pp"})
	if err != nil {
		t.Fatal(err)
	}
	askPath, err := m.MaterializeAskpass("cred_1", "op_1", HTTPSTokenPayload{Username: "u", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	otherPath, _, err := m.MaterializeSSHKey("cred_2", "op_1", SSHKeyPayload{PrivateKey: "k2"})
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Cleanup("cred_1", "op_1"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for _, p := range []string{keyPath, sshPass, askPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Cleanup", p)
		}
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Errorf("Cleanup removed another credential's file: %v", err)
	}

	// Idempotent: nothing left to remove is not an error.
	if err := m.Cleanup("cred_1", "op_1"); err != nil {
		t.Errorf("second Cleanup: %v", err)
	}
}

// Path-part validation guards the <credID>.<opID>.<suffix> grammar (and
// the runtime dir boundary) against hand-fed values.
func TestMaterializeRejectsBadNameParts(t *testing.T) {
	m := newTestMaterializer(t)
	for _, tc := range [][2]string{
		{"", "op_1"}, {"cred_1", ""},
		{"cred.dot", "op_1"}, {"cred_1", "op.dot"},
		{"../escape", "op_1"}, {"cred_1", "op/1"},
	} {
		if _, _, err := m.MaterializeSSHKey(tc[0], tc[1], SSHKeyPayload{PrivateKey: "k"}); err == nil {
			t.Errorf("MaterializeSSHKey(%q, %q) accepted invalid name parts", tc[0], tc[1])
		}
		if _, err := m.MaterializeAskpass(tc[0], tc[1], HTTPSTokenPayload{Username: "u", Token: "t"}); err == nil {
			t.Errorf("MaterializeAskpass(%q, %q) accepted invalid name parts", tc[0], tc[1])
		}
		if err := m.Cleanup(tc[0], tc[1]); err == nil {
			t.Errorf("Cleanup(%q, %q) accepted invalid name parts", tc[0], tc[1])
		}
	}
}

func TestCleanupAllKeepsOnlyReferencedAndNeverTouchesKnownHosts(t *testing.T) {
	m := newTestMaterializer(t)
	keepKey, _, err := m.MaterializeSSHKey("cred_live", "op_live", SSHKeyPayload{PrivateKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	// Same credential, dead op: the keep-set is per (credID, opID), so
	// this one must be swept even though its credID is live.
	if _, _, err := m.MaterializeSSHKey("cred_live", "op_dead", SSHKeyPayload{PrivateKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MaterializeSSHKey("cred_orphan1", "op_x", SSHKeyPayload{PrivateKey: "k", Passphrase: "pp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MaterializeAskpass("cred_orphan2", "op_y", HTTPSTokenPayload{Username: "u", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.KnownHostsPath(), []byte("git.cloonar.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Age the files past credFileMinAge so the in-flight guard does not spare
	// them — this test asserts the keep-set semantics, not the age guard.
	ageCredFiles(t, m.Dir())

	// The design §6 restart rule: keep exactly the files referenced by
	// re-adopted live runs — per (credID, opID).
	live := map[[2]string]bool{{"cred_live", "op_live"}: true}
	err = m.CleanupAll(func(filename string) bool {
		credID, ok := CredIDFromFile(filename)
		if !ok {
			return false
		}
		opID, ok := OpIDFromFile(filename)
		return ok && live[[2]string{credID, opID}]
	})
	if err != nil {
		t.Fatalf("CleanupAll: %v", err)
	}

	if _, err := os.Stat(keepKey); err != nil {
		t.Errorf("live op's key file was removed: %v", err)
	}
	if _, err := os.Stat(m.KnownHostsPath()); err != nil {
		t.Errorf("CleanupAll touched known_hosts: %v", err)
	}
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Errorf("runtime dir contains %v, want only cred_live.op_live.key and known_hosts", names)
	}
}

func TestCleanupAllNilKeepRemovesAllCredentialFiles(t *testing.T) {
	m := newTestMaterializer(t)
	if _, _, err := m.MaterializeSSHKey("cred_a", "op_1", SSHKeyPayload{PrivateKey: "k", Passphrase: "pp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MaterializeAskpass("cred_b", "op_2", HTTPSTokenPayload{Username: "u", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	ageCredFiles(t, m.Dir())
	if err := m.CleanupAll(nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("runtime dir not empty after CleanupAll(nil): %v", entries)
	}
}

// ageCredFiles backdates every runtime file except known_hosts well past
// credFileMinAge, so CleanupAll's in-flight guard does not treat them as
// possibly-mid-op. Used by cleanup tests that assert immediate reaping.
func ageCredFiles(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-2 * credFileMinAge)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "known_hosts" {
			continue
		}
		if err := os.Chtimes(filepath.Join(dir, e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// Regression (in-flight op race): a credential is materialized at the start
// of a clone/instance Start (opID = repo/run id) but its run only enters the
// keep-set once the tmux session is live — after the AddWorktree fetch, seed,
// and spawn. A throttled sweep firing in that window must NOT unlink the file,
// or it strands the in-flight fetch / just-spawned live agent with a dangling
// GIT_SSH_COMMAND. CleanupAll therefore spares a not-kept file younger than
// credFileMinAge, and reaps it once it ages out.
func TestCleanupAllSparesFreshNotKeptFiles(t *testing.T) {
	m := newTestMaterializer(t)
	key, _, err := m.MaterializeSSHKey("cred_x", "op_inflight", SSHKeyPayload{PrivateKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	// keep=false for everything (the in-flight run isn't live yet).
	if err := m.CleanupAll(func(string) bool { return false }); err != nil {
		t.Fatalf("CleanupAll: %v", err)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("fresh not-kept file was reaped, stranding the in-flight op: %v", err)
	}
	// Once it ages past the window it is a genuine orphan and gets reaped.
	old := time.Now().Add(-2 * credFileMinAge)
	if err := os.Chtimes(key, old, old); err != nil {
		t.Fatal(err)
	}
	if err := m.CleanupAll(func(string) bool { return false }); err != nil {
		t.Fatalf("CleanupAll: %v", err)
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Errorf("aged-out orphan survived CleanupAll: %v", err)
	}
}

// Regression (crash-orphaned temp files): a kill between writeFileAtomic's
// CreateTemp and rename leaves a dot-prefixed temp file holding secret
// bytes. CleanupAll must reap such temps once stale, while leaving fresh
// ones (possibly an in-flight write) and unrelated files alone.
func TestCleanupAllRemovesStaleTempFiles(t *testing.T) {
	m := newTestMaterializer(t)
	old := time.Now().Add(-10 * time.Minute)

	staleTemps := []string{
		".cred_a.op_1.key.tmp-123456",
		".cred_b.op_2.askpass.tmp-777",
		".cred_c.op_3.sshpass.tmp-9",
	}
	freshTemp := ".cred_d.op_4.key.tmp-555"
	unrelated := ".unrelated.tmp-1" // not a credential temp: never touched, however old

	for _, name := range append(append([]string{}, staleTemps...), freshTemp, unrelated) {
		if err := os.WriteFile(filepath.Join(m.Dir(), name), []byte("secret bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range append(append([]string{}, staleTemps...), unrelated) {
		if err := os.Chtimes(filepath.Join(m.Dir(), name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(m.KnownHostsPath(), []byte("git.cloonar.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.CleanupAll(nil); err != nil {
		t.Fatalf("CleanupAll: %v", err)
	}

	for _, name := range staleTemps {
		if _, err := os.Stat(filepath.Join(m.Dir(), name)); !os.IsNotExist(err) {
			t.Errorf("stale temp %s survived CleanupAll", name)
		}
	}
	for _, name := range []string{freshTemp, unrelated, "known_hosts"} {
		if _, err := os.Stat(filepath.Join(m.Dir(), name)); err != nil {
			t.Errorf("CleanupAll removed %s: %v", name, err)
		}
	}
}

func TestCredAndOpIDFromFile(t *testing.T) {
	for _, tc := range []struct {
		filename   string
		wantCredID string
		wantOpID   string
		wantOK     bool
	}{
		{"cred_ab12.op_cd34.key", "cred_ab12", "op_cd34", true},
		{"cred_ab12.op_cd34.askpass", "cred_ab12", "op_cd34", true},
		{"cred_ab12.op_cd34.sshpass", "cred_ab12", "op_cd34", true},
		{"cred_ab12.key", "", "", false}, // pre-per-op format: no opID
		{"known_hosts", "", "", false},
		{".key", "", "", false},
		{".askpass", "", "", false},
		{".op_1.key", "", "", false},
		{"cred_a.b.c.key", "", "", false},                 // extra separator: not ours
		{".cred_ab12.op_cd34.key.tmp-123", "", "", false}, // writeFileAtomic temp file
		{"cred_ab12.op_cd34.key.bak", "", "", false},
	} {
		credID, okCred := CredIDFromFile(tc.filename)
		opID, okOp := OpIDFromFile(tc.filename)
		if credID != tc.wantCredID || okCred != tc.wantOK {
			t.Errorf("CredIDFromFile(%q) = (%q, %v), want (%q, %v)", tc.filename, credID, okCred, tc.wantCredID, tc.wantOK)
		}
		if opID != tc.wantOpID || okOp != tc.wantOK {
			t.Errorf("OpIDFromFile(%q) = (%q, %v), want (%q, %v)", tc.filename, opID, okOp, tc.wantOpID, tc.wantOK)
		}
	}
}

func TestGitEnvBuilders(t *testing.T) {
	// The exact design §6 env vars, verbatim.
	sshEnv := SSHEnv("/state/runtime/cred_1.op_9.key", "/state/runtime/known_hosts")
	wantSSH := []string{
		"GIT_SSH_COMMAND=ssh -i /state/runtime/cred_1.op_9.key" +
			" -o IdentitiesOnly=yes" +
			" -o UserKnownHostsFile=/state/runtime/known_hosts" +
			" -o StrictHostKeyChecking=accept-new",
	}
	if len(sshEnv) != 1 || sshEnv[0] != wantSSH[0] {
		t.Errorf("SSHEnv = %q, want %q", sshEnv, wantSSH)
	}

	// The D9 passphrase variant: SSHEnv plus exactly the askpass wiring.
	withPass := SSHEnvWithPassphrase("/state/runtime/cred_1.op_9.key", "/state/runtime/known_hosts", "/state/runtime/cred_1.op_9.sshpass")
	wantPass := append(append([]string{}, wantSSH...),
		"SSH_ASKPASS=/state/runtime/cred_1.op_9.sshpass",
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=:0",
	)
	if len(withPass) != len(wantPass) {
		t.Fatalf("SSHEnvWithPassphrase = %q, want %q", withPass, wantPass)
	}
	for i := range wantPass {
		if withPass[i] != wantPass[i] {
			t.Errorf("SSHEnvWithPassphrase[%d] = %q, want %q", i, withPass[i], wantPass[i])
		}
	}

	httpsEnv := HTTPSEnv("/state/runtime/cred_2.op_9.askpass")
	if len(httpsEnv) != 1 || httpsEnv[0] != "GIT_ASKPASS=/state/runtime/cred_2.op_9.askpass" {
		t.Errorf("HTTPSEnv = %q", httpsEnv)
	}

	// No env builder ever carries payload bytes — paths only.
	for _, e := range append(append(append([]string{}, sshEnv...), withPass...), httpsEnv...) {
		if strings.Contains(e, "token") || strings.Contains(e, "BEGIN") {
			t.Errorf("env %q looks like it carries payload material", e)
		}
	}
}

func TestMaterializeErrorsContainNoPayloadBytes(t *testing.T) {
	// Point the materializer at a path that cannot be written.
	dir := filepath.Join(t.TempDir(), "runtime")
	m, err := NewMaterializer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // no write permission
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	const secret = "SECRET-KEY-BYTES-42"
	const passphrase = "SECRET-PASSPHRASE-43"
	if _, _, err := m.MaterializeSSHKey("cred_z", "op_1", SSHKeyPayload{PrivateKey: secret, Passphrase: passphrase}); err == nil {
		t.Skip("running as root? write to 0500 dir succeeded")
	} else if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), passphrase) {
		t.Errorf("error %q contains payload bytes", err)
	}
	if _, err := m.MaterializeAskpass("cred_z", "op_1", HTTPSTokenPayload{Username: "u", Token: secret}); err == nil {
		t.Error("expected askpass materialization to fail in unwritable dir")
	} else if strings.Contains(err.Error(), secret) {
		t.Errorf("error %q contains payload bytes", err)
	}
}
