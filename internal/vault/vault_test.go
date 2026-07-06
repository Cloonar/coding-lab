package vault

import (
	"bytes"
	"crypto/rand"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func testVault(t *testing.T) *Vault {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	v, err := New(key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := New(make([]byte, n)); err == nil {
			t.Errorf("New accepted a %d-byte key", n)
		}
	}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	v := testVault(t)
	for _, plaintext := range [][]byte{
		[]byte(`{"username":"lab","token":"tok_secret"}`),
		[]byte(""),
		bytes.Repeat([]byte{0x00}, 4096),
	} {
		blob, err := v.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if want := 12 + len(plaintext) + 16; len(blob) != want {
			t.Errorf("blob length %d, want %d (nonce||ciphertext||tag)", len(blob), want)
		}
		if len(plaintext) > 0 && bytes.Contains(blob, plaintext) {
			t.Error("ciphertext contains the plaintext")
		}
		got, err := v.Decrypt(blob)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Error("roundtrip mismatch")
		}
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	v := testVault(t)
	plaintext := []byte("credential payload bytes")
	blob, err := v.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte at every position: nonce, ciphertext, and tag alike
	// must all be authenticated.
	for i := range blob {
		tampered := bytes.Clone(blob)
		tampered[i] ^= 0x01
		if _, err := v.Decrypt(tampered); err == nil {
			t.Fatalf("Decrypt accepted blob with byte %d flipped", i)
		}
	}
}

func TestDecryptRejectsShortAndForeignBlobs(t *testing.T) {
	v := testVault(t)
	for _, blob := range [][]byte{nil, {}, make([]byte, 11), make([]byte, 27)} {
		if _, err := v.Decrypt(blob); err == nil {
			t.Errorf("Decrypt accepted %d-byte blob", len(blob))
		}
	}

	other := testVault(t)
	blob, err := other.Encrypt([]byte("sealed under another key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Decrypt(blob); err == nil {
		t.Error("Decrypt accepted a blob sealed under a different master key")
	}
}

func TestEncryptNonceUniqueness(t *testing.T) {
	v := testVault(t)
	plaintext := []byte("same plaintext every time")
	nonces := make(map[string]bool)
	blobs := make(map[string]bool)
	for i := 0; i < 256; i++ {
		blob, err := v.Encrypt(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		nonce := string(blob[:12])
		if nonces[nonce] {
			t.Fatal("nonce reused across encryptions")
		}
		nonces[nonce] = true
		if blobs[string(blob)] {
			t.Fatal("identical blob produced twice for the same plaintext")
		}
		blobs[string(blob)] = true
	}
}

func TestDecryptErrorContainsNoPayloadBytes(t *testing.T) {
	v := testVault(t)
	secret := "SUPERSECRET-PRIVATE-KEY-MATERIAL"
	blob, err := v.Encrypt([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0x01
	_, err = v.Decrypt(blob)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("decrypt error %q contains payload bytes", err)
	}
}

// TestNoLoggingImports pins the design §12 invariant that nothing in this
// package logs: the package must not import any logging package.
func TestNoLoggingImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		filename := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(filename, ".go") ||
			strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filename, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "log" || strings.HasPrefix(path, "log/") ||
				strings.Contains(path, "logx") {
				t.Errorf("%s imports logging package %q", filename, path)
			}
		}
	}
}
