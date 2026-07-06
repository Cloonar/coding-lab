// Package compat pins lab's known-fragile couplings to the Claude Code
// CLI (brief §11 items 1–4) against a concrete verified version.
//
// The couplings themselves are implemented in internal/provider/claudecode;
// this package holds the human-readable pin document (compat.md) and probe
// tests that exercise the coupling surface against captured fixtures in
// testdata/ — a real registry-file shape, a real auth-status JSON shape,
// the spawn argv snapshot, the OAuth authorize URL regex, and the trust-key
// round-trip. When a new Claude Code version changes any of these, update
// the fixture, the claudecode port, and compat.md together.
//
// Verified version: Claude Code 2.1.198 (live probe 2026-07-05); see
// compat.md for the per-item live-vs-fixture provenance.
package compat
