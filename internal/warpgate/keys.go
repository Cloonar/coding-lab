package warpgate

// A repo user's public-key credentials — one ephemeral key per run. Lab
// generates the key pair at spawn, registers the public half here, writes the
// private half into the run's runtime dir, and removes the credential when
// the run is wiped (ADR-0068 decisions 7 and 9). The bastion then accepts
// that key for the repo's user, and only for the targets its role carries.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// runKeyLabelPrefix marks a credential as a lab run key. Warpgate strips the
// OpenSSH key comment on store (wire.go point 9), so the credential's LABEL
// is the only lab-owned metadata that survives; the orphan sweep keys off it.
const runKeyLabelPrefix = "lab-run:"

// runIDPattern is the exact shape ids.NewID("run") produces:
// run_<32 lowercase hex> (the same pattern internal/instancehome matches run
// directories by). RunIDFromLabel accepts nothing else, so a label an
// operator typed that merely starts with "lab-run:" is never mistaken for a
// lab run key — the sweep deletes what parses, and must parse only what lab
// wrote.
var runIDPattern = regexp.MustCompile(`^run_[0-9a-f]{32}$`)

// PublicKey is one public-key credential of a Warpgate user: its credential
// id (what removal is addressed by), its label, and the key in authorized_keys
// form as Warpgate stores it (comment stripped).
type PublicKey struct{ ID, Label, OpenSSH string }

// RunKeyLabel is the credential label lab gives a run's key: "lab-run:" +
// runID.
func RunKeyLabel(runID string) string { return runKeyLabelPrefix + runID }

// RunIDFromLabel inverts RunKeyLabel. ok is true only for "lab-run:" followed
// by a well-formed lab run ID; every other label — an operator's own key, a
// mangled or truncated lab label — is not lab's to touch.
func RunIDFromLabel(label string) (runID string, ok bool) {
	id, found := strings.CutPrefix(label, runKeyLabelPrefix)
	if !found || !runIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

// AddPublicKey registers authorizedKey (one authorized_keys line) as a
// public-key credential of userID under label, returning the stored
// credential.
//
// The key is parsed locally first: upstream answers a key it cannot parse
// with an opaque 500 (wire.go point 9), and lab generates these keys itself,
// so a malformed one is a lab bug that should fail here, attributably. What
// is sent is the canonical "<type> <base64>[ <comment>]" re-rendering of the
// parsed key — authorized_keys options, which upstream's parser does not
// accept, are dropped — so the key Warpgate stores is exactly the key that
// was validated.
func (c *Client) AddPublicKey(ctx context.Context, userID, label, authorizedKey string) (PublicKey, error) {
	if userID == "" {
		return PublicKey{}, errors.New("warpgate: user ID must not be empty")
	}
	if strings.TrimSpace(label) == "" || strings.ContainsAny(label, "\r\n\x00") {
		return PublicKey{}, errors.New("warpgate: public key label must be a non-empty single line")
	}
	line, err := canonicalAuthorizedKey(authorizedKey)
	if err != nil {
		return PublicKey{}, err
	}
	body, err := c.do(ctx, http.MethodPost, c.publicKeysURL(userID), wireNewPublicKey{Label: label, OpenSSHPublicKey: line}, plainBody)
	if err != nil {
		return PublicKey{}, err
	}
	row, err := decodeOne[wirePublicKey](body, "create public key")
	if err != nil {
		return PublicKey{}, err
	}
	if row.ID == "" {
		return PublicKey{}, errors.New("warpgate: the create public key answer carries no id; see internal/warpgate/wire.go")
	}
	return publicKeyFromWire(row), nil
}

// canonicalAuthorizedKey parses exactly one authorized_keys entry and renders
// it back as "<type> <base64>" plus the comment when there is one.
func canonicalAuthorizedKey(authorizedKey string) (string, error) {
	pub, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return "", fmt.Errorf("warpgate: public key is not a valid authorized_keys entry: %w", err)
	}
	if strings.TrimSpace(string(rest)) != "" {
		return "", errors.New("warpgate: public key must be exactly one authorized_keys entry")
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		line += " " + comment
	}
	return line, nil
}

// ListPublicKeys returns userID's public-key credentials — the orphan sweep's
// input. Upstream answers an unknown user with an empty list, not a 404.
func (c *Client) ListPublicKeys(ctx context.Context, userID string) ([]PublicKey, error) {
	if userID == "" {
		return nil, errors.New("warpgate: user ID must not be empty")
	}
	body, err := c.do(ctx, http.MethodGet, c.publicKeysURL(userID), nil, plainBody)
	if err != nil {
		return nil, err
	}
	rows, err := decodeList[wirePublicKey](body, "public keys", plainBody)
	if err != nil {
		return nil, err
	}
	out := make([]PublicKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, publicKeyFromWire(r))
	}
	return out, nil
}

// RemovePublicKey deletes one credential. Already gone (404) is success:
// revocation runs from every wipe path and the orphan sweep, and they may
// race or repeat.
func (c *Client) RemovePublicKey(ctx context.Context, userID, keyID string) error {
	if userID == "" || keyID == "" {
		return errors.New("warpgate: user ID and key ID must not be empty")
	}
	_, err := c.deleteIdempotent(ctx, c.publicKeyURL(userID, keyID))
	return err
}

func publicKeyFromWire(w wirePublicKey) PublicKey {
	return PublicKey{ID: w.ID, Label: w.Label, OpenSSH: w.OpenSSHPublicKey}
}
