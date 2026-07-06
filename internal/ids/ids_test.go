package ids

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"
)

var (
	idRe    = regexp.MustCompile(`^run_[0-9a-f]{32}$`)
	tokenRe = regexp.MustCompile(`^lab_pat_[A-Za-z0-9_-]{43}$`)
	hexRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func TestNewIDFormat(t *testing.T) {
	for range 100 {
		id := NewID("run")
		if !idRe.MatchString(id) {
			t.Fatalf("NewID(run) = %q, want match for %s", id, idRe)
		}
	}
}

func TestNewIDUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id := NewID("usr")
		if seen[id] {
			t.Fatalf("NewID produced duplicate %q", id)
		}
		seen[id] = true
	}
}

func TestNewTokenFormatAndHash(t *testing.T) {
	token, hash := NewToken("pat")
	if !tokenRe.MatchString(token) {
		t.Fatalf("NewToken(pat) token = %q, want match for %s", token, tokenRe)
	}
	if !hexRe.MatchString(hash) {
		t.Fatalf("NewToken(pat) hash = %q, want 64 lowercase hex chars", hash)
	}
	if got := HashToken(token); got != hash {
		t.Fatalf("HashToken(token) = %q, want the hash NewToken returned %q", got, hash)
	}
	sum := sha256.Sum256([]byte(token))
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Fatalf("hash = %q, want sha256 hex %q", hash, want)
	}
}

func TestNewTokenUnique(t *testing.T) {
	a, _ := NewToken("run")
	b, _ := NewToken("run")
	if a == b {
		t.Fatal("two NewToken calls returned the same token")
	}
}

func TestHashTokenDeterministic(t *testing.T) {
	first, second := HashToken("lab_pat_x"), HashToken("lab_pat_x")
	if first != second {
		t.Fatal("HashToken not deterministic")
	}
	if HashToken("a") == HashToken("b") {
		t.Fatal("HashToken collision on trivially different inputs")
	}
}
