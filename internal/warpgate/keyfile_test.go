package warpgate

// LoadAdminToken is fsx.ReadSecretFile under Warpgate's wording; these cases
// pin the wording an operator reads and that the result feeds New.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTokenFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "warpgate-admin.token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod separately: WriteFile's mode is masked by umask.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAdminTokenAccepts(t *testing.T) {
	for _, perm := range []os.FileMode{0o600, 0o400} {
		for _, content := range []string{testToken, testToken + "\n"} {
			got, err := LoadAdminToken(writeTokenFile(t, content, perm))
			if err != nil || got != testToken {
				t.Errorf("perm %04o content %q: = %q, %v", perm, content, got, err)
			}
		}
	}
}

func TestLoadAdminTokenRefusesLoosePerms(t *testing.T) {
	for _, perm := range []os.FileMode{0o640, 0o644, 0o604, 0o660} {
		path := writeTokenFile(t, testToken+"\n", perm)
		_, err := LoadAdminToken(path)
		if err == nil {
			t.Fatalf("perm %04o: LoadAdminToken succeeded", perm)
		}
		msg := err.Error()
		for _, want := range []string{"warpgate admin token file", path, "0600 or stricter", "refusing to start"} {
			if !strings.Contains(msg, want) {
				t.Errorf("perm %04o: error %q does not mention %q", perm, msg, want)
			}
		}
		if strings.Contains(msg, testToken) {
			t.Errorf("perm %04o: error %q echoes the token", perm, msg)
		}
	}
}

func TestLoadAdminTokenRefusesMalformedContent(t *testing.T) {
	for _, tc := range []struct{ content, wantWord string }{
		{"", "is empty; expected a single-line admin token"},
		{"\n", "is empty"},
		{testToken + "\nsecond\n", "single line"},
		{testToken + "\r\n", "single line"},
	} {
		path := writeTokenFile(t, tc.content, 0o600)
		_, err := LoadAdminToken(path)
		if err == nil || !strings.Contains(err.Error(), tc.wantWord) {
			t.Errorf("content %q: error = %v, want %q", tc.content, err, tc.wantWord)
			continue
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("content %q: error %q echoes the token", tc.content, err)
		}
	}
	if _, err := LoadAdminToken(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("LoadAdminToken on a missing file succeeded")
	}
	if _, err := LoadAdminToken(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("LoadAdminToken on a directory: %v", err)
	}
}

// TestLoadAdminTokenFeedsNew: a token file that passes cannot fail New.
func TestLoadAdminTokenFeedsNew(t *testing.T) {
	tok, err := LoadAdminToken(writeTokenFile(t, testToken+"\n", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{BaseURL: "https://localhost:8888", Token: tok}); err != nil {
		t.Errorf("New with a loaded token: %v", err)
	}
}
