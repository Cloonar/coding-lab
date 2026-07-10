package push

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestGenerateThenLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.key")

	key, err := GenerateKey(path)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("GenerateKey wrote perms %04o, want 0600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(raw) {
		t.Errorf("file content is not 64 lowercase hex chars + newline (len %d)", len(raw))
	}

	loaded, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if loaded.PrivateKeyB64() != key.PrivateKeyB64() {
		t.Error("LoadKey returned different key than GenerateKey")
	}

	// The applicationServerKey is the uncompressed P-256 point: 65 bytes
	// beginning with the 0x04 (uncompressed) marker.
	pub, err := base64.RawURLEncoding.DecodeString(key.PublicKeyB64())
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(pub) != 65 || pub[0] != 0x04 {
		t.Errorf("public key is %d bytes (first %#x), want 65 bytes starting 0x04", len(pub), firstByte(pub))
	}
}

// firstByte avoids indexing an empty slice when the length assertion above
// already failed.
func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

func TestGenerateLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateKey(filepath.Join(dir, "vapid.key")); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "vapid.key" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contains %v, want only vapid.key", names)
	}
}

func TestGenerateRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.key")
	if _, err := GenerateKey(path); err != nil {
		t.Fatalf("first GenerateKey: %v", err)
	}
	before, _ := os.ReadFile(path)
	if _, err := GenerateKey(path); err == nil {
		t.Fatal("second GenerateKey succeeded, want refusal")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("second GenerateKey modified the existing key file")
	}
}

// Regression (GenerateKey stat-then-rename TOCTOU): two processes racing
// first-start GenerateKey on the same path must produce exactly one winner
// (link(2) publish), and the key on disk must be the winner's — a loser
// silently clobbering it would invalidate every subscription minted against
// the winner's public key.
func TestGenerateConcurrentExactlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.key")
	const n = 8

	type result struct {
		key Key
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
			key, err := GenerateKey(path)
			results <- result{key: key, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winners []Key
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
		t.Fatalf("%d concurrent GenerateKeys succeeded, want exactly 1", len(winners))
	}

	loaded, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey after concurrent GenerateKey: %v", err)
	}
	if loaded.PrivateKeyB64() != winners[0].PrivateKeyB64() {
		t.Error("disk key does not match the successful GenerateKey's key")
	}

	// No temp litter from the losers.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "vapid.key" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contains %v, want only vapid.key", names)
	}
}

func TestLoadAcceptsMissingTrailingNewlineAndStricterPerms(t *testing.T) {
	// 0xab repeated is a valid P-256 scalar (well below the group order).
	scalar := bytes.Repeat([]byte{0xab}, scalarLen)
	wantB64 := base64.RawURLEncoding.EncodeToString(scalar)
	for _, tc := range []struct {
		name    string
		content string
		perm    os.FileMode
	}{
		{"no trailing newline", hex.EncodeToString(scalar), 0o600},
		{"trailing newline", hex.EncodeToString(scalar) + "\n", 0o600},
		{"uppercase hex", strings.ToUpper(hex.EncodeToString(scalar)) + "\n", 0o600},
		{"stricter perms 0400", hex.EncodeToString(scalar) + "\n", 0o400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vapid.key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.perm); err != nil {
				t.Fatal(err)
			}
			got, err := LoadKey(path)
			if err != nil {
				t.Fatalf("LoadKey: %v", err)
			}
			if got.PrivateKeyB64() != wantB64 {
				t.Error("loaded key mismatch")
			}
		})
	}
}

func TestLoadRefusesLoosePerms(t *testing.T) {
	for _, perm := range []os.FileMode{0o640, 0o644, 0o660, 0o604, 0o601} {
		path := filepath.Join(t.TempDir(), "vapid.key")
		if _, err := GenerateKey(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKey(path)
		if err == nil {
			t.Fatalf("perm %04o: LoadKey succeeded, want refusal", perm)
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

// permOctal renders perms the way LoadKey's error does (e.g. 0644).
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
		{"long", strings.Repeat("ab", scalarLen) + "ff"},
		{"not hex", strings.Repeat("zz", scalarLen)},
		{"secret-looking not hex", "hunter2-SECRET-MATERIAL-" + strings.Repeat("x", 40)},
		{"double newline", strings.Repeat("ab", scalarLen) + "\n\n"},
		// 64 hex chars that decode to a scalar >= the P-256 group order:
		// well-formed on the surface, rejected by NewPrivateKey.
		{"scalar at or above group order", strings.Repeat("f", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vapid.key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadKey(path)
			if err == nil {
				t.Fatal("LoadKey succeeded on malformed content")
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
	if _, err := LoadKey(filepath.Join(t.TempDir(), "absent.key")); err == nil {
		t.Fatal("LoadKey succeeded on missing file")
	}
}

func TestLoadRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadKey(dir); err == nil {
		t.Fatal("LoadKey succeeded on a directory")
	}
}
