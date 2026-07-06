package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Credential kinds (design §3a) — the only values the credentials table
// and the credentials API accept.
const (
	KindSSHKey     = "ssh_key"
	KindHTTPSToken = "https_token"
	KindForgeToken = "forge_token"
)

// Kinds returns all valid credential kinds, in the schema's CHECK order.
func Kinds() []string {
	return []string{KindSSHKey, KindHTTPSToken, KindForgeToken}
}

// ValidKind reports whether kind is a known credential kind.
func ValidKind(kind string) bool {
	switch kind {
	case KindSSHKey, KindHTTPSToken, KindForgeToken:
		return true
	}
	return false
}

// IsGitKind reports whether kind may serve as a repo's GIT credential
// (repos.credential_id — ssh_key|https_token only, design §3a).
func IsGitKind(kind string) bool {
	return kind == KindSSHKey || kind == KindHTTPSToken
}

// IsForgeKind reports whether kind may serve as a repo's forge credential
// (repos.forge_credential_id — forge_token only, design §3a). Forge
// tokens are server-side only: never materialized, never in session env.
func IsForgeKind(kind string) bool {
	return kind == KindForgeToken
}

// SSHKeyPayload is the payload for kind ssh_key. The passphrase is
// optional and is never written to disk by materialization.
type SSHKeyPayload struct {
	PrivateKey string `json:"private_key"`
	Passphrase string `json:"passphrase,omitempty"`
}

// HTTPSTokenPayload is the payload for kind https_token.
type HTTPSTokenPayload struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

// Forge flavors (ADR-0015) — the tracker family a forge_token drives. The
// values match the repos.forge_kind vocabulary (tracker.ForgeKind*), but the
// credential's flavor — not the repo's detected host — is the routing
// authority: it decides which REST client the registry builds.
const (
	ForgeForgejo = "forgejo"
	ForgeGitHub  = "github"
)

// ForgeTokenPayload is the payload for kind forge_token. Forge names the
// tracker family (ADR-0015): an ABSENT/empty field decodes as forgejo, which
// is correct by construction for every credential created before flavors
// existed (they were all Forgejo). Host is the API origin the registry builds
// the BaseURL from — a bare host[:port] for the forgejo flavor (registry
// appends /api/v1), or the verbatim API root for github (e.g. api.github.com,
// or a GHE root like ghe.example.com/api/v3).
type ForgeTokenPayload struct {
	Host  string `json:"host"`
	Token string `json:"token"`
	Forge string `json:"forge,omitempty"`
}

// ForgeFlavor returns the payload's flavor, defaulting an absent/empty field
// to forgejo (the pre-flavor default — every existing credential is Forgejo).
func (p ForgeTokenPayload) ForgeFlavor() string {
	if p.Forge == "" {
		return ForgeForgejo
	}
	return p.Forge
}

// ValidForgeFlavor reports whether flavor is empty (→ forgejo) or one of the
// known flavors. The credentials API 400s on anything else so a typo never
// reaches the registry's flavor router.
func ValidForgeFlavor(flavor string) bool {
	switch flavor {
	case "", ForgeForgejo, ForgeGitHub:
		return true
	}
	return false
}

// ValidateKindPayload checks that kind is known and that payload is a
// JSON object of exactly that kind's schema with all required fields
// non-empty. It is the shared validation for credential create and
// payload rotation (the API 400s with the returned message). Error
// strings never include payload bytes — neither values nor field names.
func ValidateKindPayload(kind string, payload []byte) error {
	switch kind {
	case KindSSHKey:
		var p SSHKeyPayload
		if err := decodeStrict(payload, &p, kind); err != nil {
			return err
		}
		if p.PrivateKey == "" {
			return fmt.Errorf("%s payload: missing private_key", kind)
		}
		// The passphrase travels via the line-based SSH_ASKPASS protocol;
		// an embedded line break would silently truncate it at op time.
		if err := singleLine(kind, "passphrase", p.Passphrase); err != nil {
			return err
		}
	case KindHTTPSToken:
		var p HTTPSTokenPayload
		if err := decodeStrict(payload, &p, kind); err != nil {
			return err
		}
		if p.Username == "" {
			return fmt.Errorf("%s payload: missing username", kind)
		}
		if p.Token == "" {
			return fmt.Errorf("%s payload: missing token", kind)
		}
		// Both flow through git's line-based askpass protocol.
		if err := singleLine(kind, "username", p.Username); err != nil {
			return err
		}
		if err := singleLine(kind, "token", p.Token); err != nil {
			return err
		}
	case KindForgeToken:
		var p ForgeTokenPayload
		if err := decodeStrict(payload, &p, kind); err != nil {
			return err
		}
		if p.Host == "" {
			return fmt.Errorf("%s payload: missing host", kind)
		}
		if p.Token == "" {
			return fmt.Errorf("%s payload: missing token", kind)
		}
		// The flavor enum is validated here; the flavor-aware host SHAPE
		// (https-only, path allowed only for github) is validated at the API
		// layer via tracker.NormalizeForgeHost, the same routine the registry
		// builds the BaseURL with — one source of truth, no create/resolve
		// divergence. An absent forge field is valid (→ forgejo).
		if !ValidForgeFlavor(p.Forge) {
			return fmt.Errorf("%s payload: forge must be %q or %q", kind, ForgeForgejo, ForgeGitHub)
		}
		// Host and token travel in HTTP request lines/headers where
		// CR/LF is fatal (or worse, header-injecting).
		if err := singleLine(kind, "host", p.Host); err != nil {
			return err
		}
		if err := singleLine(kind, "token", p.Token); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown credential kind %q", kind)
	}
	return nil
}

// singleLine rejects CR, LF, and NUL in fields that travel over
// line-oriented or header channels (git/ssh askpass stdout, HTTP
// headers): an embedded line break would silently truncate the secret at
// op time — a confusing remote 401 — so it is a clear validation error
// at create/rotate time instead. The message never echoes the value.
func singleLine(kind, field, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s payload: %s must be a single line without control characters", kind, field)
	}
	return nil
}

// decodeStrict unmarshals payload into out, rejecting unknown fields and
// trailing data. Decoder errors are replaced with generic messages so no
// payload bytes (values or field names) can leak into error strings.
func decodeStrict(payload []byte, out any, kind string) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s payload: not a JSON object with exactly the expected fields", kind)
	}
	if dec.More() {
		return fmt.Errorf("%s payload: trailing data after JSON object", kind)
	}
	return nil
}

// EncryptPayload JSON-encodes a typed payload (SSHKeyPayload,
// HTTPSTokenPayload or ForgeTokenPayload — or pre-validated raw JSON via
// json.RawMessage) and seals it: the form stored in
// credentials.encrypted_payload.
func (v *Vault) EncryptPayload(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		// Never wrap: json errors can quote input fragments.
		return nil, errors.New("vault: payload is not JSON-encodable")
	}
	return v.Encrypt(raw)
}

// DecryptPayload opens an encrypted payload blob and JSON-decodes it into
// out (a pointer to the kind's payload struct). This is the ONLY read
// path for stored payloads — the API never returns them (design §12).
func (v *Vault) DecryptPayload(blob []byte, out any) error {
	raw, err := v.Decrypt(blob)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// Never wrap: json errors can quote input fragments.
		return errors.New("vault: decrypted payload is not valid JSON")
	}
	return nil
}
