// Package codexcompat pins lab's known-fragile couplings to the Codex CLI
// against a concrete verified version — the committed Tier-2 compat record
// ADR-0036 requires for the codex adapter (issue #87).
//
// The couplings themselves are implemented in internal/provider/codex; this
// package holds the human-readable pin document (compat.md) and probe tests
// that exercise the coupling surface against captured fixtures in testdata/ —
// three REAL 0.133.0 rollout JSONLs (base_instructions stubbed, every other
// field verbatim), the device-auth login stdout, both login-status shapes,
// the trimmed `codex debug models` catalog, and the raw slash-popup scrape —
// plus the spawn-argv snapshots, the trust-append and AGENTS.md-bridge
// round-trips, and the scrub-marker samples. When a new Codex version
// changes any of these, update the fixture, the codex port, and compat.md
// together, in the same commit.
//
// Verified version: codex-cli 0.133.0 (live spike sweep 2026-07-10, issue
// #87 Amendment 2); see compat.md for the per-item provenance
// (live / fixture / cli extraction).
package codexcompat
