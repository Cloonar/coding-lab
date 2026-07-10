package secrets

import (
	"context"
	"fmt"
)

// Source builds per-repo Redactors from sealed secret values. It is the
// narrow seam between the vault-coupled world (store + vault, wired in
// cmd/lab) and consumers like the chat tailer, which receive a Redactor and
// never see plumbing, encrypted blobs, or plaintext handling directly.
type Source struct {
	// Values returns repoID's secrets as encrypted blobs (vault
	// nonce||ciphertext) keyed by name. An empty map means the repo has no
	// secrets; see Redactor's zero-secret fast path.
	Values func(ctx context.Context, repoID string) (map[string][]byte, error)

	// Decrypt opens one encrypted blob and returns the plaintext secret
	// value. Typically a bound *vault.Vault.Decrypt.
	Decrypt func(blob []byte) ([]byte, error)
}

// Redactor builds a fresh Redactor for repoID's current secrets.
//
// A repo with no secrets returns (nil, nil) — THE zero-secret fast path.
// Callers must treat a nil Redactor as "nothing to scan": skip calling
// Redact entirely rather than holding a no-op Redactor, which lets the
// common case (most repos have no secrets) cost nothing beyond the Values
// lookup.
//
// There is no caching in this slice: every call re-fetches and re-decrypts,
// so a rotated or newly-added secret is picked up by the very next call with
// no invalidation to wire up. This trades a small amount of repeated
// decrypt work for that simplicity; it is fine because callers are expected
// to invoke Redactor only when they have new content worth scanning (e.g. a
// transcript that just changed), not on every render.
//
// Errors from Values or Decrypt are wrapped with context but never carry a
// blob or a decrypted value: a Values failure names only repoID, and a
// Decrypt failure additionally names the secret whose blob failed to open —
// never the blob's bytes.
func (s *Source) Redactor(ctx context.Context, repoID string) (*Redactor, error) {
	blobs, err := s.Values(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("secrets for repo %q: %w", repoID, err)
	}
	if len(blobs) == 0 {
		return nil, nil
	}

	values := make(map[string]string, len(blobs))
	for name, blob := range blobs {
		plaintext, err := s.Decrypt(blob)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %q for repo %q: %w", name, repoID, err)
		}
		values[name] = string(plaintext)
	}
	return NewRedactor(values), nil
}
