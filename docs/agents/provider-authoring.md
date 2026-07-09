# Provider Authoring

How to add a new `AgentProvider` adapter (Codex, Gemini, …) and prove it's done: implement the seam, then pass the two-tier conformance bar (issue #80 / ADR-0036).

## Hard requirements — no escape hatch

- **A durable, locatable transcript.** `LocateTranscript` must find a real file — verified available for Codex's rollout JSONLs and Gemini CLI's session recording. An adapter may not ship without one.
- **The lab-owned `.local` context-file name.** `SeedMeta.ContextFileName` is a bare `.local`-suffixed name, never a repo-tracked one (issue #79 / ADR-0035). Making the CLI actually *read* it is the adapter's own `SeedWorkspace` job, live-verified — never solved by declaring a tracked name.
- **Live verification before merge.** Tier 2 below is required, not optional, even though it never runs in CI.
- **Core stays adapter-agnostic.** No provider names or markers outside the adapter package — enforced by `internal/provider/neutrality_test.go` and `web/src/providerNeutral.test.ts`.

## Author path

1. Implement the seam in `internal/provider/<id>/` — the full `AgentProvider` interface (`internal/provider/provider.go`).
2. Declare `SeedMeta()`: context-file name (`.local`-suffixed), skills dir, exclude entries, and `ScrubPatterns`/`SeededPathPatterns` in the BRE∩RE2 common dialect (issue #75 / ADR-0033).
3. Wire host config from the generic `-provider-bin`/`-provider-config` maps, with the adapter owning its own defaults (issue #78 / ADR-0034) — no provider-named field goes into `internal/config`.
4. Add the one entry to the registration table in `internal/provider/conformance_test.go` (`TestConformance`, package `provider_test`), constructing the adapter hermetically (temp dirs, a fake runner) with its `providertest.Fixture`. Get Tier 1 green.
5. Run the four Tier-2 live spikes below on a host with the real CLI.
6. Commit the compat record at `internal/compat/<providerID>/compat.md`.
7. Verify core neutrality: `internal/provider/neutrality_test.go` and `web/src/providerNeutral.test.ts` must both stay clean.
8. Register the provider in `cmd/lab/main.go`.

Definition of done: **Tier 1 green in CI, Tier 2 evidenced in the committed compat record** (ADR-0036). Issue #2 (Codex) adopts this bar.

## Tier 1 — the conformance suite (CI, every adapter)

Entry point: `providertest.Conformance(t *testing.T, p provider.AgentProvider, fx providertest.Fixture)` (`internal/provider/providertest/conformance.go`). `Fixture.AttributionSamples` (required, ≥1) are real single-line marker strings the CLI writes into commits/PRs — your adapter's ground-truth test vectors; `Fixture.CleanSamples` (optional) must NOT match, guarding against an over-broad pattern.

Subtests assert:

- **patterns-dialect** — `ScrubPatterns`/`SeededPathPatterns` compile via `provider.CompileScrubPatterns` (issue #75 / ADR-0033); `ScrubPatterns` non-empty.
- **context-file-lab-owned** — `ContextFileName` is a bare `.local`-suffixed name (issue #79 / ADR-0035).
- **seedmeta-clone** — `SeedMeta`/`Models`/`Efforts`/`SpawnOptions` return defensively-cloned slices.
- **catalogs** — model/effort catalogs are non-empty with unique non-empty values; spawn-option specs are typed with valid defaults.
- **spawn-argv** — `argv(prompt) == argv(no prompt) + [prompt]`; every declared spawn option round-trips.
- **auth-flow** — `AuthFlow().Kind` is a recognized `AuthFlow*` constant.
- **login-session** — the id charset and `tmuxx.IsLoginSession(tmuxx.LoginSessionName(ID()))` hold (issue #77 / ADR-0034).
- **seeding-exclude-coverage** — in a real temp linked worktree, the provider's `SeedWorkspace` plus the real `internal/seeder` run, in production order, leave `git status --porcelain` empty; the context file/skills dir exist; every seeded path matches a `SeededPathPattern`.
- **seeding-incogni** — same, with `SeedOpts{Incogni: true}`: no tracked-file writes.
- **scrub-markers** — every `AttributionSample` is stripped by the adapter's own `ScrubPatterns` through both real engines (RE2 via `provider.CompileScrubPatterns`, POSIX BRE via a real `grep -i`, skipped when absent) and by `provider.NewRegistry(p).ScrubRegexps()` — the union path the agent-API sanitizer actually runs (ADR-0033).

A deliberately-broken fake adapter lives in the suite's own tests; failures name the obligation they broke.

## Tier 2 — live spikes (required, documented, not CI-run)

Held to the ADR-0008 live-verification bar: real CLI, real host, no credentials in CI. Evidenced by the committed compat record, not by a test that runs on every push.

1. **transcript** — locate by cwd, read/map to the universal schema, confirm a NEW transcript identity on context clear (native rotation or a synthesized epoch — see `LocateTranscript`'s doc).
2. **reply / dialog-answer / interrupt** — hazard-check the send-keys recipes against the real TUI; double-Esc/Ctrl-C must never be able to kill the session.
3. **context-file discovery** — prove the CLI actually READS the lab-owned context file (e.g. Codex ↔ `AGENTS.local.md` via project config).
4. **incogni attribution ground truth** — capture what the CLI actually writes into commits/PRs and reconcile it against the adapter's `ScrubPatterns`; these live samples become `Fixture.AttributionSamples`.

## Compat record convention

Follow claude-code's worked example, `internal/compat/compat.md`: numbered sections, each carrying a provenance tag (live / fixture / schema extraction); captured fixtures under `testdata/` tagged with the CLI version (e.g. `-2.1.198`); opt-in live tests gated by `LAB_COMPAT_LIVE=1` (`internal/compat/live_recipes_test.go`, no build tags). A new adapter's record lives at `internal/compat/<providerID>/compat.md`, same shape; claude-code's stays where it is.
