// Package ids generates lab's prefixed random identifiers and API/run
// tokens (design §2). IDs are `<prefix>_<32 lowercase hex>` from 16 bytes of
// crypto/rand; tokens are `lab_<prefix>_<43 base64url chars>` from 32 bytes,
// stored only as sha256 hex.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// NewID returns `<prefix>_<32 lowercase hex>` built from 16 bytes of
// crypto/rand. Canonical prefixes: usr, cred, repo, run, tok, rtok, iss,
// cmt, lbl, cr.
func NewID(prefix string) string {
	var b [16]byte
	mustRead(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// NewToken returns a fresh secret token `lab_<prefix>_<43 base64url chars>`
// (32 random bytes, unpadded base64url) together with its sha256 hex — the
// only form that may be persisted. The plaintext token is shown once and
// never stored.
func NewToken(prefix string) (token, sha256hex string) {
	var b [32]byte
	mustRead(b[:])
	token = "lab_" + prefix + "_" + base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashToken(token)
}

// HashToken returns the sha256 hex of a token, the constant-time-comparable
// lookup key for stored tokens.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func mustRead(b []byte) {
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read never fails on supported platforms; if it ever
		// does, generating predictable IDs/tokens would be worse than dying.
		panic("ids: crypto/rand: " + err.Error())
	}
}
