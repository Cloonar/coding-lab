package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- 1. zero-secret repo: the fast path -------------------------------------

func TestSource_EmptyMapReturnsNilRedactor(t *testing.T) {
	s := &Source{
		Values: func(ctx context.Context, repoID string) (map[string][]byte, error) {
			return map[string][]byte{}, nil
		},
		Decrypt: func(blob []byte) ([]byte, error) {
			t.Fatalf("Decrypt must not be called for a zero-secret repo")
			return nil, nil
		},
	}
	r, err := s.Redactor(context.Background(), "repo-empty")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r != nil {
		t.Fatalf("want nil Redactor for a zero-secret repo, got %+v", r)
	}
}

// A nil map must behave the same as an empty one.
func TestSource_NilMapReturnsNilRedactor(t *testing.T) {
	s := &Source{
		Values: func(ctx context.Context, repoID string) (map[string][]byte, error) {
			return nil, nil
		},
		Decrypt: func(blob []byte) ([]byte, error) {
			t.Fatalf("Decrypt must not be called for a zero-secret repo")
			return nil, nil
		},
	}
	r, err := s.Redactor(context.Background(), "repo-nil")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r != nil {
		t.Fatalf("want nil Redactor for a nil values map, got %+v", r)
	}
}

// --- 2. happy path: decrypt then build a working Redactor -------------------

func TestSource_HappyPath(t *testing.T) {
	plaintexts := map[string]string{
		"API_KEY": "s3cr3t-Val",
		"OTHER":   "another-value!",
	}
	blobs := make(map[string][]byte, len(plaintexts))
	for name, v := range plaintexts {
		blobs[name] = []byte("sealed:" + v) // stand-in for vault nonce||ciphertext
	}

	var gotRepoID string
	s := &Source{
		Values: func(ctx context.Context, repoID string) (map[string][]byte, error) {
			gotRepoID = repoID
			return blobs, nil
		},
		Decrypt: func(blob []byte) ([]byte, error) {
			return []byte(strings.TrimPrefix(string(blob), "sealed:")), nil
		},
	}

	r, err := s.Redactor(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r == nil {
		t.Fatalf("want a non-nil Redactor")
	}
	if gotRepoID != "repo1" {
		t.Fatalf("Values called with repoID %q, want %q", gotRepoID, "repo1")
	}

	out, names := r.Redact("leaked: s3cr3t-Val end")
	if out != "leaked: [REDACTED:API_KEY] end" {
		t.Fatalf("Redact output = %q", out)
	}
	if len(names) != 1 || names[0] != "API_KEY" {
		t.Fatalf("names = %v, want [API_KEY]", names)
	}
}

// --- 3. Values error propagates ---------------------------------------------

func TestSource_ValuesErrorPropagates(t *testing.T) {
	wantErr := errors.New("values backend unavailable")
	s := &Source{
		Values: func(ctx context.Context, repoID string) (map[string][]byte, error) {
			return nil, wantErr
		},
		Decrypt: func(blob []byte) ([]byte, error) {
			t.Fatalf("Decrypt must not be called when Values errors")
			return nil, nil
		},
	}
	r, err := s.Redactor(context.Background(), "repo7")
	if err == nil {
		t.Fatalf("want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "repo7") {
		t.Fatalf("err %q does not mention the repo id", err.Error())
	}
	if r != nil {
		t.Fatalf("want nil Redactor on error, got %+v", r)
	}
}

// --- 4. Decrypt error propagates, naming the secret but never the blob -----

func TestSource_DecryptErrorPropagates(t *testing.T) {
	// Distinctive, non-printable-adjacent bytes: if these ever leaked into
	// an error string, the Contains check below would catch it.
	blob := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02}
	wantErr := errors.New("vault: decrypt failed")

	s := &Source{
		Values: func(ctx context.Context, repoID string) (map[string][]byte, error) {
			return map[string][]byte{"DB_PASSWORD": blob}, nil
		},
		Decrypt: func(b []byte) ([]byte, error) {
			return nil, wantErr
		},
	}

	r, err := s.Redactor(context.Background(), "repoX")
	if err == nil {
		t.Fatalf("want an error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Fatalf("err %q does not mention the secret name", err.Error())
	}
	if strings.Contains(err.Error(), string(blob)) {
		t.Fatalf("err %q leaks the encrypted blob bytes", err.Error())
	}
	if r != nil {
		t.Fatalf("want nil Redactor on error, got %+v", r)
	}
}
