package fsx

import (
	"fmt"
	"os"
	"strings"
)

// ReadSecretFile reads a single-line credential from a permission-checked
// file. It is the shared body of the sidecar credential-file flags — OneCLI's
// --onecli-api-key-file (onecli.LoadAPIKey) and Warpgate's
// --warpgate-admin-token-file (warpgate.LoadAdminToken) — and it is a verbatim
// mirror of vault.Load's master-key-file contract (ADR-0006): stat, refuse a
// non-regular file, refuse permissions that grant anything to group or other
// naming the path and the ACTUAL mode, read, strip exactly one optional
// trailing newline. Credential files lab refuses to start on are refused for
// the same reasons with the same words, so an operator who has learned one
// rule already knows all of them.
//
// label names the file in every message ("onecli api key file"); want
// describes the expected content for the empty-file diagnosis ("a single-line
// API key (e.g. oc_proj_…)"). Both are the caller's, so each wrapper keeps its
// own operator-facing wording byte-for-byte while the rule lives here once.
//
// The permission check is the load-bearing part. Every credential read through
// here is sidecar-wide authority — a OneCLI project key mints any agent's
// proxy token, a Warpgate admin token creates users and hands them targets —
// so a 0644 file is a silent, permanent compromise and a startup failure
// rather than a warning.
//
// After the newline strip the content must be a non-empty SINGLE LINE: these
// credentials travel as HTTP header values, and an embedded CR, LF or NUL
// would either be rejected deep inside net/http or silently truncate the
// credential into a confusing 401 — the same reasoning vault applies to
// askpass-bound tokens.
//
// No error message ever echoes the file's content, not even a prefix: the
// content is the secret, and a malformed one is still a secret.
func ReadSecretFile(path, label, want string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s %s is not a regular file", label, path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("%s %s has permissions %04o, want 0600 or stricter; refusing to start", label, path, perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	secret := strings.TrimSuffix(string(raw), "\n")
	// Whitespace-only counts as empty for the diagnosis, but the secret itself
	// is returned verbatim: trimming a real credential would mask a copy-paste
	// error as a 401 from the sidecar instead of surfacing it here.
	if strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("%s %s is empty; expected %s", label, path, want)
	}
	if strings.ContainsAny(secret, "\r\n\x00") {
		return "", fmt.Errorf("%s %s must contain a single line without control characters, with at most one trailing newline", label, path)
	}
	return secret, nil
}
