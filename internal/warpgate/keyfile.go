package warpgate

import "git.cloonar.com/Cloonar/coding-lab/internal/fsx"

// LoadAdminToken reads the Warpgate admin token from a permission-checked
// file — the --warpgate-admin-token-file surface of issue #39. It is the same
// contract as onecli.LoadAPIKey and vault's master-key file (ADR-0006), and
// shares its implementation, fsx.ReadSecretFile: refuse a non-regular file,
// refuse any group/other permission bit naming the path and the ACTUAL mode,
// strip exactly one trailing newline, require a non-empty single line, and
// never echo the content.
//
// The permission rule matters more here, not less: the recommended token is
// Warpgate's static admin token, which never expires and holds every admin
// permission — anything on the host that can read a 0644 copy can create a
// Warpgate user, give it every target, and log in through the bastion.
func LoadAdminToken(path string) (string, error) {
	return fsx.ReadSecretFile(path, "warpgate admin token file", "a single-line admin token (Warpgate's WARPGATE_ADMIN_TOKEN, or an API token of a Warpgate admin)")
}
