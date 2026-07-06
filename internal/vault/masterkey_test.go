package vault

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestGenerateThenLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")

	key, err := Generate(path)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(key) != KeySize {
		t.Fatalf("Generate returned %d bytes, want %d", len(key), KeySize)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("Generate wrote perms %04o, want 0600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(raw) {
		t.Errorf("file content is not 64 lowercase hex chars + newline (len %d)", len(raw))
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(loaded, key) {
		t.Error("Load returned different key than Generate")
	}
}

func TestGenerateLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(filepath.Join(dir, "master.key")); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "master.key" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contains %v, want only master.key", names)
	}
}

func TestGenerateRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if _, err := Generate(path); err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	before, _ := os.ReadFile(path)
	if _, err := Generate(path); err == nil {
		t.Fatal("second Generate succeeded, want refusal")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("second Generate modified the existing key file")
	}
}

// Regression (Generate stat-then-rename TOCTOU): two processes racing
// first-start Generate on the same path must produce exactly one winner
// (link(2) publish), and the key on disk must be the winner's — a loser
// silently clobbering it would orphan every credential encrypted under
// the winner's in-memory key.
func TestGenerateConcurrentExactlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	const n = 8

	type result struct {
		key []byte
		err error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key, err := Generate(path)
			results <- result{key: key, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winners [][]byte
	for r := range results {
		if r.err == nil {
			winners = append(winners, r.key)
			continue
		}
		if !strings.Contains(r.err.Error(), "already exists; refusing to overwrite") {
			t.Errorf("loser error = %v, want the refuse-to-overwrite error", r.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d concurrent Generates succeeded, want exactly 1", len(winners))
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after concurrent Generate: %v", err)
	}
	if !bytes.Equal(loaded, winners[0]) {
		t.Error("disk key does not match the successful Generate's key")
	}

	// No temp litter from the losers.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "master.key" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contains %v, want only master.key", names)
	}
}

// syncDir is the durability half of the atomic-write fix (fsync the
// parent directory after rename/link). Power-loss behavior cannot be
// unit-tested; this pins that the helper works on a real directory and
// surfaces a missing one.
func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir on a real dir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("syncDir on a missing dir succeeded, want error")
	}
}

func TestLoadAcceptsMissingTrailingNewlineAndStricterPerms(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, KeySize)
	for _, tc := range []struct {
		name    string
		content string
		perm    os.FileMode
	}{
		{"no trailing newline", hex.EncodeToString(key), 0o600},
		{"trailing newline", hex.EncodeToString(key) + "\n", 0o600},
		{"uppercase hex", strings.ToUpper(hex.EncodeToString(key)) + "\n", 0o600},
		{"stricter perms 0400", hex.EncodeToString(key) + "\n", 0o400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.perm); err != nil {
				t.Fatal(err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !bytes.Equal(got, key) {
				t.Error("loaded key mismatch")
			}
		})
	}
}

func TestLoadRefusesLoosePerms(t *testing.T) {
	for _, perm := range []os.FileMode{0o640, 0o644, 0o660, 0o604, 0o601} {
		path := filepath.Join(t.TempDir(), "master.key")
		if _, err := Generate(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatalf("perm %04o: Load succeeded, want refusal", perm)
		}
		msg := err.Error()
		if !strings.Contains(msg, path) {
			t.Errorf("perm %04o: error %q does not name the path", perm, msg)
		}
		if !strings.Contains(msg, permOctal(perm)) {
			t.Errorf("perm %04o: error %q does not name the actual perms", perm, msg)
		}
	}
}

// permOctal renders perms the way Load's error does (e.g. 0644).
func permOctal(perm os.FileMode) string {
	const digits = "01234567"
	s := make([]byte, 4)
	s[0] = '0'
	s[1] = digits[(perm>>6)&7]
	s[2] = digits[(perm>>3)&7]
	s[3] = digits[perm&7]
	return string(s)
}

func TestLoadMalformedContentNotEchoed(t *testing.T) {
	for _, tc := range []struct {
		name, content string
	}{
		{"empty", ""},
		{"short", "abcd1234"},
		{"long", strings.Repeat("ab", KeySize) + "ff"},
		{"not hex", strings.Repeat("zz", KeySize)},
		{"secret-looking not hex", "hunter2-SECRET-MATERIAL-" + strings.Repeat("x", 40)},
		{"double newline", strings.Repeat("ab", KeySize) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load succeeded on malformed content")
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("error %q does not name the path", msg)
			}
			trimmed := strings.TrimSpace(tc.content)
			if trimmed != "" && strings.Contains(msg, trimmed) {
				t.Errorf("error %q echoes the file content", msg)
			}
			if strings.Contains(msg, "hunter2") {
				t.Errorf("error %q echoes secret-looking content", msg)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.key")); err == nil {
		t.Fatal("Load succeeded on missing file")
	}
}

func TestLoadRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatal("Load succeeded on a directory")
	}
}
