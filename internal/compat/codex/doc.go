// Package codexcompat pins lab's known-fragile couplings to the Codex CLI
// against a concrete verified version — the committed Tier-2 compat record
// ADR-0036 requires for the codex adapter (issue #87).
//
// The couplings themselves are implemented in internal/provider/codex; this
// package holds the human-readable pin document (compat.md) and probe tests
// that exercise the coupling surface against captured fixtures in testdata/ —
// three REAL 0.133.0 rollout JSONLs plus a REAL 0.167.0 paginated-history
// `codex exec` rollout (base_instructions stubbed, every other field
// verbatim), the 0.167.0 device-auth login stdout, both login-status shapes,
// the trimmed `codex debug models` fallback catalog, and the 0.167.0
// slash-command listing extracted from source — plus the spawn-argv
// snapshots, the trust-append and AGENTS.md-bridge round-trips, and the
// scrub-marker samples. When a new Codex version changes any of these,
// update the fixture, the codex port, and compat.md together, in the same
// commit.
//
// Verified version: codex-cli 0.167.0 (re-verification 2026-11-10 — CLI
// probes, source extraction and a pty smoke on both the outgoing 0.133.0 and
// the incoming binary; the live TUI recipes still date from the 0.133.0
// spike sweep of 2026-07-10, issue #87 Amendment 2); see compat.md for the
// per-item provenance (live / fixture / cli extraction / source extraction).
package codexcompat
