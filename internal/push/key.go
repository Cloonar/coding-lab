// Package push implements lab's Web Push sender plumbing (issue #98) per the
// Web Push standards: application-server identity is a P-256 keypair used for
// VAPID (RFC 8292), the signed JWT that authenticates lab to a browser push
// service. This file owns the VAPID key file — the same load-or-generate,
// refuse-loose-perms, never-overwrite contract as the vault master key — and
// exposes the keypair in the base64url form the webpush library's options
// take.
//
// Nothing in this package logs, and error strings never contain key material
// or file bytes: they name the path and the expected format only.
package push

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/fsx"
)

// scalarLen is the P-256 private scalar length in bytes. On disk it is the
// same format as the vault master key — 64 lowercase hex characters (an
// optional trailing newline) — so the two key files share one contract.
const scalarLen = 32

// Key is a VAPID application-server keypair (P-256). The public half is the
// applicationServerKey browsers subscribe against; the private scalar signs
// the VAPID JWT. It is loaded from or generated into the VAPID key file.
type Key struct {
	priv *ecdh.PrivateKey
}

// LoadKey reads the VAPID key file: exactly 64 hex characters (the 32-byte
// P-256 private scalar), optionally followed by a single trailing newline.
// It refuses files whose permissions grant anything to group or other,
// naming the path and the actual permissions. Malformed content — wrong
// length, non-hex, or a scalar outside the P-256 group order — is reported
// without echoing it.
func LoadKey(path string) (Key, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Key{}, fmt.Errorf("vapid key file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Key{}, fmt.Errorf("vapid key file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return Key{}, fmt.Errorf("vapid key file %s has permissions %04o, want 0600 or stricter; refusing to start", path, perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Key{}, fmt.Errorf("vapid key file: %w", err)
	}
	content := strings.TrimSuffix(string(raw), "\n")
	if len(content) != hex.EncodedLen(scalarLen) {
		return Key{}, malformedKey(path)
	}
	scalar, err := hex.DecodeString(content)
	if err != nil {
		return Key{}, malformedKey(path)
	}
	// NewPrivateKey enforces 1 <= scalar < n (the P-256 group order), so a
	// hex-valid but out-of-range scalar (e.g. all-"f") is rejected here as
	// malformed rather than yielding an unusable key.
	priv, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		return Key{}, malformedKey(path)
	}
	return Key{priv: priv}, nil
}

// malformedKey describes the expected format without echoing what the file
// actually contained (it may be a secret in the wrong format).
func malformedKey(path string) error {
	return fmt.Errorf("vapid key file %s is malformed: want exactly %d hex characters (a P-256 private scalar) with an optional trailing newline", path, hex.EncodedLen(scalarLen))
}

// GenerateKey creates a fresh VAPID keypair at path — the P-256 private
// scalar as 64 lowercase hex characters plus a trailing newline, mode 0600,
// written atomically (same-dir temp file published via link(2)) so a crash
// cannot leave a partial key. Used on first start when the key file is
// absent. It refuses to overwrite an existing file — link(2) fails with
// EEXIST if path appears between the existence check and the publish, so
// even two processes racing GenerateKey get exactly one winner: silently
// replacing a VAPID key would invalidate every browser subscription minted
// against the old public key.
func GenerateKey(path string) (Key, error) {
	if _, err := os.Stat(path); err == nil {
		return Key{}, errKeyExists(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Key{}, fmt.Errorf("vapid key file: %w", err)
	}
	// GenerateKey draws a valid scalar in [1, n-1] from the reader, so the
	// generated key always survives NewPrivateKey on the next load.
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, fmt.Errorf("vapid key file: random key: %w", err)
	}
	content := hex.EncodeToString(priv.Bytes()) + "\n"
	if err := fsx.WriteFileExclusive(path, []byte(content), 0o600); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Key{}, errKeyExists(path) // lost the race: another process published first
		}
		return Key{}, fmt.Errorf("vapid key file: %w", err)
	}
	return Key{priv: priv}, nil
}

func errKeyExists(path string) error {
	return fmt.Errorf("vapid key file %s already exists; refusing to overwrite", path)
}

// PublicKeyB64 returns the VAPID public key as base64url (unpadded) of the
// 65-byte uncompressed P-256 point — the applicationServerKey the webpush
// library and browsers expect. It is not secret: operators need it to
// register a push subscription, which is why boot logs it.
func (k Key) PublicKeyB64() string {
	return base64.RawURLEncoding.EncodeToString(k.priv.PublicKey().Bytes())
}

// PrivateKeyB64 returns the VAPID private key as base64url (unpadded) of the
// 32-byte scalar — the form the webpush library's private-key option takes.
// Secret; never logged.
func (k Key) PrivateKeyB64() string {
	return base64.RawURLEncoding.EncodeToString(k.priv.Bytes())
}
