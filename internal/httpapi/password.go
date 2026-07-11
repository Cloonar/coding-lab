package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argonParams pins the argon2id cost (design §12: RFC 9106 second
// recommended set — time=3, memory=64MiB, threads=4, keyLen=32, 16B salt).
type argonParams struct {
	time      uint32
	memoryKiB uint32
	threads   uint8
	keyLen    uint32
	saltLen   int
}

var defaultArgonParams = argonParams{
	time:      3,
	memoryKiB: 64 * 1024,
	threads:   4,
	keyLen:    32,
	saltLen:   16,
}

// argonConcurrency caps simultaneous argon2id computations server-wide:
// each one holds ~64 MiB live (design §12), so unbounded parallel logins
// could OOM the host. Excess requests wait for a slot; they never error.
const argonConcurrency = 4

// verifyPasswordGated runs VerifyPassword under the global argon2
// concurrency cap.
func (s *Server) verifyPasswordGated(phcHash, password string) (bool, error) {
	s.argonSem <- struct{}{}
	defer func() { <-s.argonSem }()
	return VerifyPassword(phcHash, password)
}

// hashPasswordGated runs hashPasswordWith under the same cap.
func (s *Server) hashPasswordGated(password string) string {
	s.argonSem <- struct{}{}
	defer func() { <-s.argonSem }()
	return hashPasswordWith(s.argon, password)
}

// HashPassword returns the PHC-encoded argon2id hash of password using the
// pinned production parameters.
func HashPassword(password string) string {
	return hashPasswordWith(defaultArgonParams, password)
}

func hashPasswordWith(p argonParams, password string) string {
	salt := make([]byte, p.saltLen)
	if _, err := rand.Read(salt); err != nil {
		// Same stance as internal/ids: predictable salts would be worse
		// than dying.
		panic("httpapi: crypto/rand: " + err.Error())
	}
	key := argon2.IDKey([]byte(password), salt, p.time, p.memoryKiB, p.threads, p.keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memoryKiB, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// parsedPHC is the decoded, version-checked pieces of a PHC-encoded
// argon2id hash string: the cost params read from the hash (so old hashes
// keep verifying after a parameter bump — see VerifyPassword) plus the raw
// salt and key bytes.
type parsedPHC struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
	salt      []byte
	key       []byte
}

// parsePHC decodes and structurally validates a PHC-encoded argon2id hash:
// 6 dollar-separated parts, "argon2id" scheme, a v= matching the argon2
// package we build with, and m=/t=/p= params. Any PARSEABLE params are
// accepted — same tolerance VerifyPassword has always had, so a hash
// survives a future param bump or one minted by external tooling. It is the
// parsing half of VerifyPassword, factored out (issue #137) so
// ValidatePasswordHash can validate a hash structurally without ever
// touching the KDF, and so the two paths can never drift on what counts as
// a well-formed hash. Errors carry no "verify password: " prefix — callers
// that need it wrap themselves.
func parsePHC(phcHash string) (parsedPHC, error) {
	parts := strings.Split(phcHash, "$")
	// ["", "argon2id", "v=19", "m=…,t=…,p=…", "<salt>", "<key>"]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return parsedPHC{}, errors.New("malformed PHC hash")
	}
	var version int
	if n, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || n != 1 || version != argon2.Version {
		return parsedPHC{}, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	var p parsedPHC
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memoryKiB, &p.time, &p.threads); err != nil || n != 3 {
		return parsedPHC{}, fmt.Errorf("malformed parameters %q", parts[3])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return parsedPHC{}, fmt.Errorf("malformed salt: %w", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return parsedPHC{}, fmt.Errorf("malformed key: %w", err)
	}
	if len(key) == 0 {
		return parsedPHC{}, errors.New("empty key")
	}
	p.salt, p.key = salt, key
	return p, nil
}

// ValidatePasswordHash parse-validates a PHC-encoded argon2id hash without
// verifying anything — there's no plaintext to verify against at startup
// seed time (issue #137). It shares parsePHC with VerifyPassword, so a hash
// this accepts can never turn out to be structurally unparseable at login:
// the seed path and the login path are the same parser.
func ValidatePasswordHash(phcHash string) error {
	_, err := parsePHC(phcHash)
	return err
}

// VerifyPassword checks password against a PHC-encoded argon2id hash. The
// parameters are read from the hash, so old hashes keep verifying after a
// parameter bump. The compare is constant-time.
func VerifyPassword(phcHash, password string) (bool, error) {
	p, err := parsePHC(phcHash)
	if err != nil {
		return false, fmt.Errorf("verify password: %w", err)
	}
	got := argon2.IDKey([]byte(password), p.salt, p.time, p.memoryKiB, p.threads, uint32(len(p.key)))
	return subtle.ConstantTimeCompare(got, p.key) == 1, nil
}
