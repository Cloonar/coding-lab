package onecli

import "git.cloonar.com/Cloonar/coding-lab/internal/fsx"

// LoadAPIKey reads a OneCLI API key from a permission-checked file — the
// --onecli-api-key-file surface of issue #23. The rule itself lives in
// fsx.ReadSecretFile, shared with Warpgate's admin-token file, and is a
// verbatim mirror of vault.Load's master-key-file contract (ADR-0006): stat,
// refuse a non-regular file, refuse permissions that grant anything to group
// or other naming the path and the ACTUAL mode, read, strip exactly one
// optional trailing newline, require a non-empty single line. Two credential
// files that lab refuses to start on should be refused for the same reasons
// with the same words; an operator who has learned the master-key-file rule
// already knows this one.
//
// The permission check is the load-bearing part. A OneCLI project key is
// authority over every credential in the project — with it, anything on the
// host that can read the file can mint an agent's proxy token and use every
// granted credential. A 0644 key file is a silent, permanent compromise, so
// it is a startup failure rather than a warning.
//
// The single-line rule exists because the key travels as an HTTP header value
// (Authorization: Bearer …); New re-checks it for keys that did not come from
// a file. No error message ever echoes the file's content, not even a prefix.
func LoadAPIKey(path string) (string, error) {
	return fsx.ReadSecretFile(path, "onecli api key file", "a single-line API key (e.g. oc_proj_…)")
}
