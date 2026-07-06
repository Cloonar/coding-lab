// Package vault implements lab's credential vault (design §6): master key
// file management, AES-256-GCM encryption of credential payloads, the
// typed payload schemas per credential kind (design §3a), and
// materialization of GIT credentials under the runtime dir so git
// subprocesses can authenticate without secrets ever appearing in argv,
// URLs, repo config, or logs.
//
// Nothing in this package logs, and error strings never contain key
// material or payload bytes (design §12; enforced by tests).
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize is the master key length in bytes (AES-256).
const KeySize = 32

// ErrDecrypt is returned for any blob that fails authenticated
// decryption: wrong key, truncated blob, or tampered nonce/ciphertext.
// It is deliberately uninformative — decrypt failures carry no detail
// that could echo key material or payload bytes.
var ErrDecrypt = errors.New("vault: decrypt failed")

// Vault encrypts and decrypts credential payloads with AES-256-GCM.
// Blobs are nonce||ciphertext with a fresh random 12-byte nonce per
// encryption (design §6); they are what the store persists in
// credentials.encrypted_payload.
type Vault struct {
	aead cipher.AEAD
}

// New returns a Vault sealed by the given 32-byte master key (from Load
// or Generate).
func New(key []byte) (*Vault, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("vault: master key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault: init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault: init gcm: %w", err)
	}
	return &Vault{aead: aead}, nil
}

// Encrypt seals plaintext under a fresh random nonce and returns
// nonce||ciphertext.
func (v *Vault) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize(), v.aead.NonceSize()+len(plaintext)+v.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("vault: nonce: %w", err)
	}
	return v.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens a nonce||ciphertext blob produced by Encrypt. Any
// tampering or key mismatch yields ErrDecrypt.
func (v *Vault) Decrypt(blob []byte) ([]byte, error) {
	ns := v.aead.NonceSize()
	if len(blob) < ns+v.aead.Overhead() {
		return nil, ErrDecrypt
	}
	plaintext, err := v.aead.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
