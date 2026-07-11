package main

// Startup seeding of the initial operator user (issue #134). This runs after
// store.Open (migrations + store init) and strictly BEFORE the HTTP listener
// opens in run(), so no visitor can race the seed and grab the first-run setup
// page: by the time /api/auth/state is reachable a user row already exists.
// It creates the user row ONLY — no session — so /api/auth/state reports
// setup_required=false, authenticated=false and the SPA lands on login rather
// than the setup wizard.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/config"
	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// seedInitialUser provisions the declared operator user on an empty database so
// declarative deployments never surface the first-run setup page (issue #134).
// config.Parse already enforces all-or-nothing on the seed fields, so here
// SeedUser=="" means "no seeding configured" and a non-empty SeedUser is
// guaranteed to carry at least one password source.
func seedInitialUser(ctx context.Context, st *store.Store, cfg config.Config, logger *slog.Logger) error {
	if cfg.SeedUser == "" {
		return nil
	}

	// Bootstrap-only gate FIRST, before resolving the password: seeding only
	// ever applies to an empty DB. Doing the CountUsers check ahead of the
	// password-file read is deliberate — a stale or missing SeedPasswordFile
	// must never brick an already-provisioned deployment, so on a non-empty DB
	// the password source is never even touched. This is also why the issue's
	// acceptance qualifies loud failure with "on an EMPTY DB".
	n, err := st.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("seed user: counting users: %w", err)
	}
	if n > 0 {
		logger.Info("seed user configured but users already exist; skipping", "component", "main")
		return nil
	}

	// Resolve the password. A file source wins over the inline value when both
	// are set (matches the -seed-password-file > -seed-password precedence
	// documented in usage). Strip EXACTLY ONE trailing '\n' — the common shape
	// of a secret file written with a trailing newline — and nothing more, so
	// the stored password is byte-exact (TrimSpace/TrimRight would silently
	// mangle passwords that legitimately end in whitespace).
	password := cfg.SeedPassword
	if cfg.SeedPasswordFile != "" {
		b, err := os.ReadFile(cfg.SeedPasswordFile)
		if err != nil {
			return fmt.Errorf("seed user: reading password file %q: %w", cfg.SeedPasswordFile, err)
		}
		password, _ = strings.CutSuffix(string(b), "\n")
	}

	// Same rules as the first-run setup endpoint (shared validator) so the two
	// paths can never drift on what counts as a valid credential.
	if err := httpapi.ValidateNewCredentials(cfg.SeedUser, password); err != nil {
		return fmt.Errorf("seed user: %w", err)
	}

	hash := httpapi.HashPassword(password)
	if _, err := st.CreateUser(ctx, cfg.SeedUser, hash); err != nil {
		return fmt.Errorf("seed user: creating user: %w", err)
	}
	// Never log the password, the file contents, or the hash — only the username.
	logger.Info("seeded initial user", "component", "main", "username", cfg.SeedUser)
	return nil
}
