package onecli

// LoadAPIKey's decision table. It mirrors internal/vault's master-key-file
// suite deliberately: the two files are the same contract (ADR-0006), and a
// divergence in either direction is a bug in one of them.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// permOctal renders perms the way LoadAPIKey's error does (e.g. 0644), so the
// assertion checks the exact rendering an operator will read, not just the
// number. Same helper the vault suite uses.
func permOctal(perm os.FileMode) string {
	const digits = "01234567"
	s := make([]byte, 4)
	s[0] = '0'
	s[1] = digits[(perm>>6)&7]
	s[2] = digits[(perm>>3)&7]
	s[3] = digits[perm&7]
	return string(s)
}

// writeKeyFile writes content at mode perm and returns the path.
func writeKeyFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "onecli-api.key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod separately: WriteFile's mode is masked by umask, so a test asserting
	// a REFUSAL on 0644 must set the mode explicitly or it may silently land at
	// 0600 and pass for the wrong reason.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAPIKeyAccepts(t *testing.T) {
	const key = "oc_proj_abc123DEF456"
	for _, tc := range []struct {
		name    string
		content string
		perm    os.FileMode
	}{
		{"0600 without trailing newline", key, 0o600},
		{"0600 with one trailing newline", key + "\n", 0o600},
		{"stricter 0400", key + "\n", 0o400},
		// 0700 grants nothing to group or other, so it passes the perm&0o077
		// rule even though the execute bit is meaningless on a key file.
		// (0000 is deliberately not exercised: it is owner-only enough for the
		// rule, but a non-root test process cannot open it at all, which would
		// make this a test of the runner's uid rather than of the contract.)
		{"owner-execute 0700 is still owner-only", key + "\n", 0o700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadAPIKey(writeKeyFile(t, tc.content, tc.perm))
			if err != nil {
				t.Fatalf("LoadAPIKey: %v", err)
			}
			if got != key {
				t.Errorf("key = %q, want %q", got, key)
			}
		})
	}
}

// TestLoadAPIKeyRefusesLoosePerms: a OneCLI project key is authority over every
// credential in the project, so anything readable beyond the owner is a startup
// failure, and the message must name both the path and the ACTUAL mode — an
// operator fixes this with one chmod and needs to be told which file.
func TestLoadAPIKeyRefusesLoosePerms(t *testing.T) {
	for _, perm := range []os.FileMode{0o640, 0o644, 0o660, 0o604, 0o601, 0o666} {
		path := writeKeyFile(t, "oc_proj_abc\n", perm)
		_, err := LoadAPIKey(path)
		if err == nil {
			t.Fatalf("perm %04o: LoadAPIKey succeeded, want refusal", perm)
		}
		msg := err.Error()
		if !strings.Contains(msg, path) {
			t.Errorf("perm %04o: error %q does not name the path", perm, msg)
		}
		if !strings.Contains(msg, permOctal(perm)) {
			t.Errorf("perm %04o: error %q does not name the actual perms", perm, msg)
		}
		if !strings.Contains(msg, "0600 or stricter") {
			t.Errorf("perm %04o: error %q does not state the wanted perms", perm, msg)
		}
	}
}

// TestLoadAPIKeyRefusesMalformedContentWithoutEchoingIt: a malformed key file
// is still a key file, so no error may quote its content.
func TestLoadAPIKeyRefusesMalformedContent(t *testing.T) {
	for _, tc := range []struct {
		name, content, wantWord string
	}{
		{"empty", "", "is empty"},
		{"only a newline", "\n", "is empty"},
		{"whitespace only", "   \n", "is empty"},
		{"embedded newline", "oc_proj_SECRET\nsecond line\n", "single line"},
		{"two trailing newlines", "oc_proj_SECRET\n\n", "single line"},
		{"embedded carriage return", "oc_proj_SECRET\r\n", "single line"},
		{"embedded NUL", "oc_proj_SECRET\x00tail", "single line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeKeyFile(t, tc.content, 0o600)
			got, err := LoadAPIKey(path)
			if err == nil {
				t.Fatalf("LoadAPIKey succeeded with %q, want refusal", got)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantWord) {
				t.Errorf("error %q does not mention %q", msg, tc.wantWord)
			}
			if !strings.Contains(msg, path) {
				t.Errorf("error %q does not name the path", msg)
			}
			if strings.Contains(msg, "oc_proj_SECRET") {
				t.Errorf("error %q echoes the file content", msg)
			}
		})
	}
}

func TestLoadAPIKeyRefusesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.key")
	if _, err := LoadAPIKey(path); err == nil {
		t.Fatal("LoadAPIKey succeeded on a missing file")
	}
}

// TestLoadAPIKeyRefusesNonRegularFile: a directory (or a fifo/device) at the
// configured path is a misconfiguration, and reading it would either block or
// produce nonsense.
func TestLoadAPIKeyRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadAPIKey(dir)
	if err == nil {
		t.Fatal("LoadAPIKey succeeded on a directory")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error %q does not say the path is not a regular file", err)
	}
}

// TestLoadAPIKeyFeedsNew closes the loop: what the loader returns is what New
// accepts, so a key file that passes here cannot fail New's single-line check.
func TestLoadAPIKeyFeedsNew(t *testing.T) {
	key, err := LoadAPIKey(writeKeyFile(t, "oc_proj_abc123\n", 0o600))
	if err != nil {
		t.Fatalf("LoadAPIKey: %v", err)
	}
	if _, err := New(Options{BaseURL: "http://127.0.0.1:10254", APIKey: key}); err != nil {
		t.Fatalf("New with a loaded key: %v", err)
	}
}
