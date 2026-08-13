package onecli

import (
	"fmt"
	"os"
	"strings"
)

// LoadAPIKey reads a OneCLI API key from a permission-checked file — the
// --onecli-api-key-file surface of issue #23. It is deliberately a verbatim
// mirror of vault.Load's master-key-file contract (ADR-0006): stat, refuse a
// non-regular file, refuse permissions that grant anything to group or other
// naming the path and the ACTUAL mode, read, strip exactly one optional
// trailing newline. Two credential files that lab refuses to start on should
// be refused for the same reasons with the same words; an operator who has
// learned the master-key-file rule already knows this one.
//
// The permission check is the load-bearing part. A OneCLI project key is
// authority over every credential in the project — with it, anything on the
// host that can read the file can mint an agent's proxy token and use every
// granted credential. A 0644 key file is a silent, permanent compromise, so
// it is a startup failure rather than a warning.
//
// After the newline strip the content must be a non-empty SINGLE LINE: the key
// travels as an HTTP header value (Authorization: Bearer …), and an embedded
// CR, LF or NUL would either be rejected deep inside net/http or silently
// truncate the credential into a confusing 401 — the same reasoning vault
// applies to askpass-bound tokens. New re-checks this for keys that did not
// come from a file.
//
// No error message ever echoes the file's content, not even a prefix: the
// content is the secret, and a malformed one is still a secret.
func LoadAPIKey(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("onecli api key file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("onecli api key file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("onecli api key file %s has permissions %04o, want 0600 or stricter; refusing to start", path, perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("onecli api key file: %w", err)
	}
	key := strings.TrimSuffix(string(raw), "\n")
	// Whitespace-only counts as empty for the diagnosis, but the key itself is
	// returned verbatim: trimming a real key would mask a copy-paste error as a
	// 401 from OneCLI instead of surfacing it here.
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("onecli api key file %s is empty; expected a single-line API key (e.g. oc_proj_…)", path)
	}
	if strings.ContainsAny(key, "\r\n\x00") {
		return "", fmt.Errorf("onecli api key file %s must contain a single line without control characters, with at most one trailing newline", path)
	}
	return key, nil
}
