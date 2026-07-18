# The chat stream's SSE reaction is tail-only with a content-hash identity: `run.messages.changed` carries state and a back-patch cursor, messages carry a server-computed hash, and the client never rebuilds DOM for content that did not change

Issue #175 (2026-07-18): with several live agents, the PWA felt sluggish
everywhere — sends, chat switching, navigation — while the server measured
healthy (messages p50 ~48ms, no host pressure). The cost was client-side, and
it compounded per streaming agent: every ~1s `run.messages.changed` made the
open chat fire an after-cursor tail fetch PLUS an unconditional latest-window
fetch, made the shell refetch the entire `GET /instances` list to flip one
state dot, and made the seq-keyed merge replace up to 60 message object
identities even when nothing changed — Solid's reference-keyed `<For>` then
tore down and rebuilt those subtrees (markdown parse included) every second.
Sibling-run `run.changed` events (repo-scoped on the wire) refetched the open
chat too. This ADR pins the protocol that removes the churn while keeping
ADR-0005's refetch-on-event architecture intact.

The decisions, pinned:

- **Events stay invalidation, but an invalidation may now say *what* changed —
  scalars only, never content.** ADR-0005's consequence line ("SSE payloads
  stay envelope-small forever") is amended, not repealed: `run.messages.changed`
  grows exactly two scalars — `state` (the run's **conversational state**, which
  the emit site already computed) and `backpatchSeq` (a seq number, present only
  when an already-served message mutated). `run.changed` grows `runID`
  (omitempty), present when the event concerns exactly one run. No payload
  carries message content, titles, or anything a client could render without a
  fetch — the API remains the single source of truth, a dropped event still
  costs staleness-until-next-event, never wrongness. Anything bigger than a
  scalar hint remains a refetchable resource.

- **Steady streaming costs one small tail fetch per event burst — the
  unconditional latest-window request is gone.** The open chat debounces
  `run.messages.changed` (300ms trailing, like the shell rail always has) and
  then fetches `after=min(cursor, backpatchSeq−1)`, accumulating the minimum
  `backpatchSeq` across the burst. Pure appends fetch from the cursor;
  a back-patched mutation (tool status flip, dialog outcome) moves the fetch
  start down to cover it — one request serves both cases, because the window
  endpoint already returns everything newer than `after`. Explicit actions
  (send, answer, interrupt), `resync`, rotation, and `run.changed` keep the
  full tail+latest protocol: they are rare, human-paced, and some of them
  (reconnect) genuinely cannot trust a cursor-relative signal.

- **`backpatchSeq` is computed where the change is observed: the tailer.** The
  per-run tailer goroutine keeps the previous successful read's per-seq hashes
  (in memory, dying with the goroutine) and publishes the lowest seq whose hash
  changed. A rotation resets the baseline and publishes no `backpatchSeq` — the
  client's `transcript_id` reset already owns that case. First tick likewise.
  The publish gate itself is unchanged.

- **Message identity is a server-computed content hash, not client deep
  equality.** Every served message carries `content_hash`: FNV-64a (the
  `transcriptID` precedent) over the message's canonical JSON with the hash
  field cleared, computed in core (`internal/chat`) after `scanAndRedact` on
  both serve paths — never in adapters, so every provider (and the test fake)
  gets it for free, and the hash always describes the *redacted* content the
  client actually receives (issue #108). The client merge keeps the previous
  object when hashes match, and returns the previous *array* when nothing
  changed at all, so a no-op refetch propagates nothing through Solid's
  reference-equality signals. Client-side deep equality was rejected: it
  re-parses and re-compares content the server already knows is unchanged, on
  every tick, on a phone.

- **Identity stability extends through the render-item layer.** Keeping message
  objects is not enough: `groupMessages` builds fresh wrapper objects per call,
  which alone would rebuild every subtree. The chat memoizes render items and
  reconciles them against the previous value — a wrapper survives when its
  message reference survives; a group survives when its key and every member
  reference survive — so `<For>` only touches DOM where content actually
  changed.

- **The rail dot is patched, not refetched.** `run.messages.changed` can only
  flip one run's conversational state, and now says which run and which state —
  so the shell patches its shared instances list in place (preserving the
  object identity of every untouched row, and the whole list when the state is
  already current) and issues zero `GET /instances` requests for that event
  type. `run.changed` (spawn/stop/outcome) and `resync` keep refetching the
  list, which is also the self-heal for any patch a dropped event lost.

- **`run.changed` is run-filtered where it can be, conservative where it
  can't.** Emit sites that concern exactly one run (stop, launch, deep-link
  capture, AFK stop, dead-session sweep, startup reconcile, pull, exposure
  redact) now carry `runID`, and the open chat ignores events for sibling
  runs. Genuinely repo-scoped emits (stop-all, the AFK reaper tick, CR merge,
  parked cleanup) stay bare, and a bare event still triggers the conservative
  refetch — correctness never depends on the new field.

- **Windowing/virtualizing the accumulated stream is explicitly deferred.**
  The unbounded per-tab DOM growth (issue #175's point 4) is real but
  orthogonal: this protocol removes the per-second rebuild; a long-lived tab's
  layout cost still grows with conversation length and remains a follow-up.

## Status

Accepted. Resolves issue #175; amends one consequence line of ADR-0005 and the
refetch protocol of ADR-0016 (the seq-keyed accumulate-and-merge itself
stands).

## Consequences

- The SSE contract is versioned by field presence, and the embedding (ADR-0005:
  the SPA ships inside the server binary) bounds the skew to one direction: a
  stale PWA-cached client against a newer server, which simply keeps its old
  chattier-but-correct behavior (full refetches, later-window-wins merges) and
  ignores the new fields. The new client's reliance on
  `state`/`backpatchSeq`/`content_hash` never meets a server that omits them;
  a `run.changed` without `runID` stays a conservative refetch by contract,
  not by accident.
- The tailer holds one hash map per live run (a few tens of KB for a long
  transcript) — bounded by the same lifecycle as the tailer goroutine itself.
- An idle open chat issues no periodic requests; a streaming chat costs one
  tail fetch per debounce window; the rail costs zero list fetches per
  message tick. The live verification protocol (devtools Network with 2–3 busy
  agents) is recorded in issue #175.
