# SolidJS SPA over a JSON API with SSE; clients refetch on event

The UI is a SolidJS + TypeScript SPA built by Vite, with `@solidjs/router` and vitest as the only additions — no other runtime dependencies — and hand-rolled CSS carrying v0's design language (phone-first, max-width 760px, 44px touch targets, 16px inputs, system fonts, dark via `prefers-color-scheme`). It replaces v0's server-rendered html/template fragments and their ~4s client poll (D5). Node exists only at build time.

The SPA is embedded so the deployable stays one static binary: `internal/webui/embed_ui.go`, behind build tag `ui`, contains `//go:embed all:dist` serving `internal/webui/dist/` — copied there from `web/dist` by `make build-ui` and by the nix package's preBuild, because `go:embed` cannot reach outside the package directory. Building `-tags ui` without the copy fails loudly ("no matching files"); an untagged build serves a minimal "UI not embedded" page while the API keeps working, which keeps plain `go build`/`go test` fast and Node-free. The SPA fallback serves `index.html` for non-API paths.

The API is the extension surface, not an implementation detail: `/api/v1` (operator), `/agent/v1` (labctl with run tokens), and `GET /api/v1/events` (SSE) are the same surface the roadmap builds on — the embedded remote interface (chat/terminal inside lab's UI), a Codex provider, CLI automation. The MVP interaction model is deliberate: lab manages lifecycle, the operator drives sessions in the claude.ai app via captured deep links; the API/SSE architecture exists so the embedded interface later needs no redesign.

**SSE carries invalidation, not state.** Events are named per brief §8.1 (`repo.changed`, `run.changed`, `parked.changed`, `clone.progress`, `claude.auth.changed`, `issue.changed`, `cr.changed`) plus a `heartbeat` every 25s; payloads are small JSON envelopes (`{type, repoID?, …}`). Clients refetch the affected resource on event — no state diffing over SSE. A diff protocol makes the client a replica that diverges the moment one event drops; refetch-on-event keeps the API the single source of truth and turns events into mere hints, so a dropped event costs staleness-until-next-event, never wrongness.

## Status

Accepted. Implements D5 and the build design §8.

## Considered options

- **Keep html/template + polling (v0's shape).** Rejected: a phone-first product UI with live updates, PWA groundwork, and a future embedded terminal needs a real client; a ~4s poll per open page is exactly the idle load an orchestrator with a tens-of-MB footprint budget shouldn't carry.
- **React or Vue.** Rejected: SolidJS compiles reactivity away instead of shipping a VDOM runtime — the runtime-light choice consistent with the performance priority; TypeScript + Vite tooling is equivalent.
- **WebSockets.** Rejected: every update flows server→client; SSE is plain HTTP (proxy- and Authelia-friendly), and `EventSource` reconnects for free. Bidirectional needs (embedded terminal) can add a socket later without touching this surface.
- **State diffs/patches over SSE.** Rejected: see above — replica drift on any dropped event, plus a protocol to version. Refetch-on-event is the simplest thing that stays correct.
- **Serving the SPA as a separate artifact.** Rejected: one static binary is the deployment shape (D1/D3); `go:embed` behind the `ui` tag gives that without making Node a `go test` dependency.

## Consequences

- Dev loop: `go run ./cmd/lab` beside `npm run dev` in `web/` — the Vite dev server proxies `/api` and `/healthz` to :8080; no embedding during development.
- `internal/webui/dist/` and `web/dist/` are gitignored; the Makefile and nix preBuild are the only writers.
- SSE payloads stay envelope-small forever; anything tempted to grow a payload should become a refetchable resource instead.
- PWA manifest/icons/offline shell land in M8 on this foundation; the embedded remote interface (roadmap) starts from the API as-is.
