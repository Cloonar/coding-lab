# Attribution markers become a single provider-declared pattern set, boot-validated and unioned across every registered provider, enforced identically by the pre-push hook and the body sanitizer

Incogni (ADR-0012, D15) has two enforcement points against AI-attribution
leaks: the pre-push guard in the bare reference repo (measure 7) and the
agent-API body sanitizer (measure 3). Issue #51 decision 8 made the hook
provider-declared — `seeder.InstallPrePushHook` renders its `grep -i` scan
from the resolving provider's `SeedMeta.ScrubPatterns`/`SeededPathPatterns`
— but the sanitizer (`sanitizeBody` → `attributionLine`, internal/agentapi)
stayed a hardcoded claude shape in core: a `Co-Authored-By: Claude…` trailer
and a bare `@anthropic.com` mention, baked into ADR-0026's otherwise
provider-neutral seam. Triage of #2 (Codex) exposed why that's a bug, not a
cosmetic gap: a `Co-authored-by: ChatGPT <noreply@openai.com>` footer sails
straight through `attributionLine` untouched even on an incogni repo,
because core knows only Anthropic's shape.

A second gap sits underneath the first. `incogniSeedMeta`
(internal/reposvc) resolves the hook's patterns from the repo's **effective
default** provider only (`repos.provider` → `provider_default` skip-layer,
per ADR-0030) — the provider a repo is stamped with, not the provider a
given session actually ran. ADR-0030 made the agent CLI a per-spawn
override: any registered provider can run against any repo for one session.
A repo defaulted to claude-code but spawned once against Codex would push
through a hook still scrubbing only claude's markers, and the sanitizer had
no session context to consult at all — it ran off `repo.Incogni` alone.
Both gaps are closed the same way: attribution markers are a **declared**
provider property, and every registered provider's declaration must be
consulted at both enforcement points, unconditionally.

Decisions, pinned:

- **One declaration, two enforcement points.** `SeedMeta.ScrubPatterns`
  feeds BOTH the pre-push hook (`grep -i` POSIX BREs in `sh`, unchanged) and
  the body sanitizer, where each pattern is compiled once as a
  case-insensitive Go regexp (a `(?i)` prefix) and applied per line. This
  binds the two mechanisms to the common BRE∩RE2 dialect — claude-code's
  four patterns already live there (`[[:space:]]`, `[^>]*` parse under
  both engines), so no pattern rewrite is needed today. No new interface
  method is added to `AgentProvider`: a provider *declares* its markers,
  core owns *both* mechanisms that act on the declaration — the seam-v2
  idiom (ADR-0026), not a capability per mechanism. A future provider whose
  attribution isn't line-shaped is the case the ADR-0017 optional-capability
  escape hatch exists for; it is not built speculatively here.
- **Boot-time validation.** `provider.NewRegistry` compiles every
  registered provider's `ScrubPatterns` as Go regexps and rejects
  construction if any fail — an unenforceable marker set is a wiring bug,
  caught at boot next to the existing empty-id/duplicate-id checks
  (internal/provider/registry.go), never a silent enforcement gap
  discovered in production. The compiled union is cached on the registry
  (`ScrubRegexps`) so neither enforcement point recompiles per call.
- **Union across all registered providers, at both enforcement points.**
  The hook's scrub patterns AND seeded-path patterns become the union over
  every provider in the registry (registration order, exact-string dedup)
  — `incogniSeedMeta`'s per-repo effective-provider resolution is deleted
  in favor of `registry.List()`. Rationale: since ADR-0030, any registered
  provider can run on any repo via the per-spawn override, so a guard keyed
  to one repo's default provider is exactly the race described above — the
  guard must screen every registered provider's markers regardless of which
  one the pushing session ran. Over-matching is safe: ADR-0012 already
  pins each provider's `SeededPathPatterns` to a deliberately narrow list —
  only what lab itself seeds for that provider — so the union of narrow
  lists stays narrow; a repo is over-rejected only for paths lab would seed
  for *some* registered provider, which is acceptable and symmetric with
  today's single-provider behavior. Side effect: the hook install path no
  longer reads `repos.provider` or the `provider_default` setting at all;
  an empty registry now yields a content-inert guard (both pattern lists
  empty, matching seeder.InstallPrePushHook's existing empty-list handling)
  instead of `incogniSeedMeta`'s current "no agent providers registered"
  error — consistent with the rest of the codebase's nil-registry degraded
  boot.
- **Sanitizer pattern-set reconciliation.** `attributionLine` previously
  stripped a `co-authored-by:` line carrying a bare `anthropic.com` mention
  anywhere after the prefix; claude-code's declared pattern requires the
  bracketed email (`co-authored-by:.*<[^>]*@anthropic\.com>`) — ADR-0012's
  own review pinned the bracketed email, not the bare domain or the
  "Claude" display name, as the stable discriminator. The declared set is
  adopted unchanged (the hook's golden output stays byte-identical); the
  sanitizer's coverage narrows to match the hook's shape, pinned by a test
  case asserting an unbracketed `anthropic.com` mention is now kept. The
  sanitizer's seam-repair mechanics — blank-line collapse at a removal seam,
  code-fence blank runs left untouched, byte-identity for non-incogni
  bodies, running after `ensureCloses` — are unchanged; only the per-line
  predicate moves from `attributionLine`'s hardcoded claude cases to the
  registry's declared union.
- **Core neutrality made testable.** A Go arch test in internal/provider
  scans non-test core string literals for attribution-marker tokens
  (`anthropic`, `claude-session`, `co-authored-by`, `generated with`),
  exempting internal/provider/claudecode and internal/provider/providertest
  (where the tokens are the declaration and its fakes, not a leak) and every
  `_test.go`/testdata file (fixture literals). This is the Go twin of the
  SPA's `providerNeutral.test.ts` grep-guard (ADR-0026). The exemption is
  deliberately narrow: a bare `claude` literal stays legitimate in core for
  CLI/config UX unrelated to attribution — flag names, `~/.claude.json`
  paths — only the attribution-marker token set is forbidden outside the
  two exempted packages. There is no allowlist for a core hit that matches
  anyway; it must be refactored into provider-declared metadata instead.

## Status

Accepted. Amends ADR-0012 (D15 measures 3 and 7 both now read from one
provider-declared, boot-validated, registry-wide union instead of a
core-hardcoded sanitizer and a per-repo-resolved hook) and issue #51
decision 8 (`SeedMeta.ScrubPatterns` gains a second consumer). Applies
ADR-0026's seam-v2 idiom (provider declares, core owns the mechanism) and
keeps ADR-0017's optional-capability pattern as the reserved escape hatch
for a future non-line-shaped attribution scheme. Closes the ADR-0030 race
between a per-session provider override and a guard keyed to one repo's
default provider. No new `AgentProvider` method, no schema change.

## Considered options

- **Re-render the hook per session with the session's own provider, and
  keep the sanitizer as-is.** Rejected: still misses the race between two
  concurrent sessions of different providers pushing through the same bare
  repo at once, adds churn to a file rewritten on every spawn, and the
  sanitizer has no session context available to it at all — the agent API
  only ever sees `repo.Incogni`, not which provider pushed which run.
- **A new interface method or capability for body sanitizing, one per
  provider.** Rejected: no second mechanism exists that would justify a
  provider-owned sanitizer — declaration plus one core-owned mechanism
  (now two: hook and sanitizer) is exactly the seam-v2 idiom ADR-0026
  already established, and ADR-0017's escape hatch remains available if a
  provider ever needs to override the declarative path.
- **Separate pattern languages per enforcement point — two declarations,
  one for the hook, one for the sanitizer.** Rejected: drift between the
  two sets is precisely the bug this ADR fixes (claude-code's sanitizer
  shape had already drifted from its own hook's declared pattern). One
  declaration, compiled twice, cannot drift.
- **Widen the declared pattern to match the sanitizer's old bare-domain
  breadth (`anthropic\.com` unbracketed).** Rejected: it would re-pin the
  hook's golden output for no real-world shape — git trailers carry
  bracketed emails, not bare domain mentions — and ADR-0012's review
  already chose the bracketed email as the durable discriminator over a
  model/display-name string.

## Consequences

- internal/provider/registry.go: `NewRegistry` gains regexp-compilation
  validation and the cached `ScrubRegexps` union; `Registry` gains a way to
  answer the unioned scrub/seeded-path pattern sets by iterating
  `List()`.
- internal/reposvc/reposvc.go: `incogniSeedMeta` and `installIncogniHook`
  stop resolving a single effective provider; the hook is rendered from the
  registry's union. An empty registry degrades to a content-inert guard
  rather than erroring.
- internal/agentapi/handlers.go: `attributionLine` is replaced by a
  predicate driven by the registry's compiled `ScrubRegexps`; `sanitizeBody`
  is otherwise unchanged. A previously-passing bare-`anthropic.com` test
  case is repinned to assert the line is now kept.
- internal/provider gains an arch test guarding core neutrality for
  attribution-marker tokens, alongside the existing seam-neutrality
  discipline (no concrete provider import in httpapi, ADR-0026). Its first
  run caught the incogni seed-prompt sentence (internal/afk/decide.go,
  measure 2) spelling `Co-Authored-By` literally; the sentence is reworded
  to name the shapes generically ("no co-author trailers, no tool-credit
  footers, no session links") so core prose carries no marker token either.
- Registering #2 (Codex) with its own `ScrubPatterns` (e.g. a `ChatGPT` /
  `@openai.com` trailer shape) requires no core change: both enforcement
  points pick it up automatically as soon as it joins the registry.
