package main

// Startup reconciliation of the initial operator user from config (issue #137,
// reworking the bootstrap-only seed of #134). This runs after store.Open
// (migrations + store init) and strictly BEFORE the HTTP listener opens in
// run(), so no visitor can race it and grab the first-run setup page: by the
// time /api/auth/state is reachable the user row already exists. It touches the
// user row ONLY — never a web session — so /api/auth/state reports
// setup_required=false, authenticated=false and the SPA lands on login rather
// than the setup wizard.
//
// Unlike #134 this is a reconcile, not a one-shot bootstrap: it runs on EVERY
// boot and treats config as the credential's source of truth. On an empty DB it
// creates the user; on a DB that already holds the seeded user it rewrites the
// stored hash when config changed and leaves it alone otherwise. The password
// is supplied pre-hashed as a PHC-encoded argon2id string (issue #137): a hash
// is not a secret, so it is safe to carry inline in config, and seeding never
// needs the plaintext (the ≥8-char password rule now lives with the two paths
// that still see plaintext — first-run setup and `lab hash-password`).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/config"
	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// seedInitialUser reconciles the declared operator user against the database so
// declarative deployments never surface the first-run setup page and so a
// rotated seed hash lands on the next restart (issue #137). config.Parse
// already enforces all-or-nothing on the seed fields, so here SeedUser=="" means
// "no seeding configured" and a non-empty SeedUser is guaranteed to carry at
// least one hash source (inline or file).
//
// The signature is fixed so cmd/lab/main.go's call site compiles unchanged.
func seedInitialUser(ctx context.Context, st *store.Store, cfg config.Config, logger *slog.Logger) error {
	if cfg.SeedUser == "" {
		return nil
	}

	// Resolve the configured hash: the inline value, overridden by the file
	// contents when SeedPasswordHashFile is set (file wins, matching config's
	// documented precedence). The read happens on EVERY boot, BEFORE any DB
	// gate. This deliberately REVERSES #135's "gate on an empty DB first so a
	// stale/missing password file can't brick a provisioned deployment": under
	// reconcile, config is the credential's source of truth, so a hash file the
	// operator pointed us at but we cannot read is a real misconfiguration that
	// SHOULD fail the boot loudly rather than be silently skipped on the second
	// and later starts. Strip EXACTLY ONE trailing "\n" (the common shape of a
	// file written with a trailing newline) and nothing more, so the stored hash
	// stays byte-exact; TrimSpace/TrimRight would corrupt an otherwise-valid PHC
	// string in edge cases.
	hash := cfg.SeedPasswordHash
	if cfg.SeedPasswordHashFile != "" {
		b, err := os.ReadFile(cfg.SeedPasswordHashFile)
		if err != nil {
			// Name only the PATH, never the (possibly partial) contents.
			return fmt.Errorf("seed user: reading password hash file %q: %w", cfg.SeedPasswordHashFile, err)
		}
		hash, _ = strings.CutSuffix(string(b), "\n")
	}

	// Parse-validate the hash structurally before touching the DB. This shares
	// the exact PHC parser with the login path's VerifyPassword (issue #137), so
	// a hash that seeds accepts can never turn out to be structurally
	// unverifiable at login — the seed path and the login path are one parser.
	// ANY parseable params are accepted (the parser reads m=/t=/p= from the hash
	// itself), so a hash minted by external tooling or by a future param bump
	// still seeds. A truncated hash, a wrong scheme, or bad base64 refuses
	// startup here. The wrapped reason carries only structural detail (scheme /
	// version / param shape) — never the salt or key bytes — so it stays safe to
	// log; we still never echo the hash string itself.
	if err := httpapi.ValidatePasswordHash(hash); err != nil {
		return fmt.Errorf("seed user %q: configured password hash is not a valid argon2id PHC string: %w", cfg.SeedUser, err)
	}
	// The 1-64 char username rule (the ≥8-char password rule cannot apply to a
	// hash and moved to `lab hash-password`).
	if err := httpapi.ValidateUsername(cfg.SeedUser); err != nil {
		return fmt.Errorf("seed user %q: %w", cfg.SeedUser, err)
	}

	n, err := st.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("seed user: counting users: %w", err)
	}
	if n == 0 {
		// Empty DB: create the operator user with the configured hash.
		if _, err := st.CreateUser(ctx, cfg.SeedUser, hash); err != nil {
			return fmt.Errorf("seed user: creating user: %w", err)
		}
		logger.Info("seeded initial user", "component", "main", "username", cfg.SeedUser)
		return nil
	}

	// The DB already has users. Reconcile the seeded user against config.
	u, err := st.UserByUsername(ctx, cfg.SeedUser)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Config names a seed user the DB does not have, but other users do
			// exist: a typo in -seed-user, or a rename since the last boot. Under
			// the config-is-truth model this is a misconfiguration we refuse
			// loudly rather than paper over. Silently creating a second account
			// would leave a live credential the operator never asked for; a
			// warn-and-skip would leave the configured credential permanently
			// dead with no signal. Name the existing user(s) so the operator can
			// reconcile config to reality instead of guessing.
			names, nerr := st.Usernames(ctx)
			if nerr != nil {
				return fmt.Errorf("seed user %q: %w", cfg.SeedUser, nerr)
			}
			return fmt.Errorf("seed user %q: database has no such user; existing user(s): %s — seed config is the credential's source of truth; fix -seed-user or remove the seed config", cfg.SeedUser, strings.Join(names, ", "))
		}
		return fmt.Errorf("seed user: looking up user: %w", err)
	}

	// The seeded user exists. Rewrite the stored hash only when config changed
	// it; an identical hash is a no-op — no write, no log line. The store method
	// never touches web_sessions, so an existing operator session stays valid
	// across a hash rotation either way.
	if u.PasswordHash == hash {
		return nil
	}
	if err := st.UpdateUserPasswordHash(ctx, u.ID, hash); err != nil {
		return fmt.Errorf("seed user: updating password hash: %w", err)
	}
	// Never log the hash, the file contents, or anything derived from them —
	// only the username.
	logger.Info("reconciled seed user password hash", "component", "main", "username", cfg.SeedUser)
	return nil
}
