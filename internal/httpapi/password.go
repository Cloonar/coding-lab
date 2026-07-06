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

// VerifyPassword checks password against a PHC-encoded argon2id hash. The
// parameters are read from the hash, so old hashes keep verifying after a
// parameter bump. The compare is constant-time.
func VerifyPassword(phcHash, password string) (bool, error) {
	parts := strings.Split(phcHash, "$")
	// ["", "argon2id", "v=19", "m=…,t=…,p=…", "<salt>", "<key>"]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("verify password: malformed PHC hash")
	}
	var version int
	if n, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || n != 1 || version != argon2.Version {
		return false, fmt.Errorf("verify password: unsupported argon2 version %q", parts[2])
	}
	var (
		memoryKiB, timeCost uint32
		threads             uint8
	)
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memoryKiB, &timeCost, &threads); err != nil || n != 3 {
		return false, fmt.Errorf("verify password: malformed parameters %q", parts[3])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("verify password: malformed salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("verify password: malformed key: %w", err)
	}
	if len(want) == 0 {
		return false, errors.New("verify password: empty key")
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memoryKiB, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
