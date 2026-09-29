package fsx

// ReadSecretFile's decision table. The wrappers (onecli.LoadAPIKey,
// warpgate.LoadAdminToken) keep their own suites for the wording an operator
// reads; this one pins the rule itself and that label/want are threaded into
// the messages verbatim — the parameterization is what lets both wrappers keep
// byte-identical messages while the rule lives in one place.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testLabel = "test secret file"
	testWant  = "a single-line test secret"
)

func writeSecret(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod separately: WriteFile's mode is masked by umask, so a refusal case
	// on 0644 must set the mode explicitly or it may silently pass at 0600.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadSecretFileAccepts(t *testing.T) {
	const secret = "s3cr3t-VALUE"
	for _, tc := range []struct {
		name    string
		content string
		perm    os.FileMode
	}{
		{"0600 bare", secret, 0o600},
		{"0600 one trailing newline", secret + "\n", 0o600},
		{"0400", secret + "\n", 0o400},
		{"0700", secret, 0o700},
		// Inner whitespace is part of the secret: it is returned verbatim,
		// never trimmed (trimming would turn a paste error into a 401).
		{"leading space kept", " " + secret + "\n", 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadSecretFile(writeSecret(t, tc.content, tc.perm), testLabel, testWant)
			if err != nil {
				t.Fatalf("ReadSecretFile: %v", err)
			}
			if want := strings.TrimSuffix(tc.content, "\n"); got != want {
				t.Errorf("secret = %q, want %q", got, want)
			}
		})
	}
}

func TestReadSecretFileRefusesLoosePerms(t *testing.T) {
	for _, perm := range []os.FileMode{0o640, 0o644, 0o660, 0o604, 0o601, 0o666, 0o610} {
		path := writeSecret(t, "SECRET_CONTENT\n", perm)
		_, err := ReadSecretFile(path, testLabel, testWant)
		if err == nil {
			t.Fatalf("perm %04o: ReadSecretFile succeeded, want refusal", perm)
		}
		want := fmt.Sprintf("%s %s has permissions %04o, want 0600 or stricter; refusing to start", testLabel, path, perm)
		if err.Error() != want {
			t.Errorf("perm %04o: error = %q, want %q", perm, err, want)
		}
	}
}

func TestReadSecretFileRefusesMalformedContent(t *testing.T) {
	for _, tc := range []struct {
		name, content, wantSuffix string
	}{
		{"empty", "", "is empty; expected " + testWant},
		{"newline only", "\n", "is empty; expected " + testWant},
		{"whitespace only", " \t \n", "is empty; expected " + testWant},
		{"embedded newline", "SECRET_CONTENT\nmore\n", "must contain a single line without control characters, with at most one trailing newline"},
		{"two trailing newlines", "SECRET_CONTENT\n\n", "must contain a single line without control characters, with at most one trailing newline"},
		{"carriage return", "SECRET_CONTENT\r\n", "must contain a single line without control characters, with at most one trailing newline"},
		{"NUL", "SECRET_CONTENT\x00x", "must contain a single line without control characters, with at most one trailing newline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSecret(t, tc.content, 0o600)
			_, err := ReadSecretFile(path, testLabel, testWant)
			if err == nil {
				t.Fatal("ReadSecretFile succeeded, want refusal")
			}
			if want := testLabel + " " + path + " " + tc.wantSuffix; err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			if strings.Contains(err.Error(), "SECRET_CONTENT") {
				t.Errorf("error %q echoes the file content", err)
			}
		})
	}
}

func TestReadSecretFileRefusesMissingAndNonRegular(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := ReadSecretFile(missing, testLabel, testWant)
	if err == nil || !strings.HasPrefix(err.Error(), testLabel+": ") || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: error = %v, want a %q-prefixed not-exist error", err, testLabel)
	}

	dir := t.TempDir()
	_, err = ReadSecretFile(dir, testLabel, testWant)
	if want := testLabel + " " + dir + " is not a regular file"; err == nil || err.Error() != want {
		t.Errorf("directory: error = %v, want %q", err, want)
	}
}
