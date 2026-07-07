# Optional capability interfaces on the provider seam

ADR-0008 built the `AgentProvider` seam with deep-link capture as a **mandatory**
method: every provider had to implement `CaptureDeepLink`, and on a miss it
returned the provider's *generic* fallback link. That shape encoded a
claude-code assumption — that every session has a claude.ai web surface — into
the seam itself, and it leaked out both ends: `internal/instance` imported the
`claudecode` package to recognise a miss (comparing the captured URL against the
package's exported `GenericDeepLink` constant), and the SPA hardcoded
`https://claude.ai/code` and a claude-specific tooltip as the universal Open
fallback for every instance row.

Triage of #2 (the Codex provider) closed the assumption: OpenAI's
remote-connections feature cannot attach a headless CLI session on a Linux
server (it requires the Codex desktop App as host, macOS/Windows, QR pairing),
so a Codex session has **no web Open link at all**. Deep-link capture is
therefore not universal — it is a property of providers with a web surface. This
ADR makes it one, following the `ConnectingReporter` pattern ADR-0008 already
established for an optional render-state extension.

The decisions, pinned:

- **Deep-link capture is an optional capability, not a seam method.** A new
  `provider.DeepLinker` interface carries `CaptureDeepLink` plus the
  provider-owned `FallbackOpen` metadata; `AgentProvider` loses `CaptureDeepLink`
  entirely. Providers advertise the capability structurally (a type assertion at
  the call site, exactly like `ConnectingReporter`). For a provider that does not
  implement it, **no capture machinery ever arms** — not at Start, not at startup
  re-adoption — and its runs keep `deep_link_url` NULL. claude-code implements it;
  a future headless Codex will not.
- **A capture miss returns `""`, never a fallback.** The old contract returned
  the generic link on a miss, which forced the caller to compare against a
  provider-specific constant to honour the write-only-on-hit rule (persist a
  captured link, never overwrite it with a generic one). With a miss = empty
  string, the caller's rule is just "non-empty → persist" — no cross-package
  constant, no `claudecode` import in core. The loud miss log (brief §11.2,
  ADR-0008) stays.
- **The Open fallback is provider-owned metadata, surfaced through the providers
  API.** `FallbackOpen()` returns an `OpenAffordance {url, title}`: the generic
  web link (claude.ai's session picker) and its explanatory tooltip, both moved
  out of the SPA and out of core into the claude-code provider. `GET
  /api/v1/providers` gains an additive, optional `fallback_open` field, present
  only for providers that implement `DeepLinker`. The SPA's open-state helper is
  driven entirely by that data — it contains no hardcoded provider URL or
  tooltip.
- **A link-less provider's rows get a copyable tmux-attach affordance.** When an
  instance's provider exposes no `fallback_open` and no exact link was captured,
  the row (and the chat header) render a copyable `tmux attach -t <session_name>`
  command with a tooltip explaining the session is driven from a terminal on the
  lab host — the session name is already in the instances API. Connecting-pulse
  and exact-link behaviour for claude-code stay exactly as before.

## Status

Accepted. Extends the `AgentProvider` seam (ADR-0008) with the optional
`DeepLinker` capability and `OpenAffordance` metadata; keeps `ConnectingReporter`
as its sibling pattern. Reaffirms the write-only-on-hit deep-link rule (ADR-0008
§ source-of-truth layering) under the simplified miss-is-empty contract. No
schema change — `deep_link_url` was already nullable. Unblocks #2 (Codex), which
is now one new `AgentProvider` implementation that simply omits `DeepLinker`,
with zero further refactor. The embedded chat's open affordance (ADR-0016) reads
from the same provider-owned metadata rather than a hardcoded link.

## Considered options

- **Keep `CaptureDeepLink` mandatory; let link-less providers return `""`
  forever.** Rejected: a provider that structurally cannot deep-link would still
  arm a capture goroutine at every Start and re-adoption that can only ever miss,
  and the seam would keep asserting a web surface every provider must pretend to
  have. Making the capability optional states the truth and saves the wasted
  poll.
- **Put the fallback-open URL on `AgentProvider` as a mandatory method returning
  `""` for link-less providers.** Rejected: it splits one capability (a web
  surface ⇒ both exact capture *and* a generic picker link) across a mandatory
  method and an optional one, and still forces every provider to answer a
  question only web providers have. Pairing `CaptureDeepLink` and `FallbackOpen`
  on one optional interface keeps the capability cohesive — a provider has both
  or neither.
- **Leave the claude.ai fallback hardcoded in the SPA, gated on provider id.**
  Rejected: it moves the provider-specific string from core into the client
  instead of eliminating it, and every new provider would mean another SPA
  branch. Provider-owned metadata over the API is the same isolation rule D14
  applies to model/effort catalogs.
- **Build the embedded remote interface (terminal in lab's UI) now for link-less
  providers.** Deferred (separate roadmap item, ADR-0005/0016): the copyable
  `tmux attach` command is the honest interim affordance for a headless session
  and precludes nothing.

## Consequences

- `internal/provider` gains `DeepLinker` and the `OpenAffordance` struct;
  `AgentProvider` shrinks by one method. `internal/instance` no longer imports a
  concrete provider package — it arms capture only for providers that implement
  `DeepLinker`, and treats an empty capture result as a miss.
- `GET /api/v1/providers` responses gain an optional `fallback_open {url,
  title}`. The SPA resolves each instance's open affordance from that data; a new
  `attach` open-state renders the copyable command.
- The provider test double splits: the scriptable `Fake` implements `DeepLinker`
  (claude-code-like), and a new `NoLinkFake` implements only `AgentProvider` (a
  headless-CLI stand-in) so the no-arm / NULL-link / tmux-attach paths are
  covered end to end.
- Adding a link-less provider is now zero-refactor: implement `AgentProvider`,
  omit `DeepLinker`, done.
