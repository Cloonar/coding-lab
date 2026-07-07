# Chat message rendering: hand-rolled markdown, tool-run grouping, copy-raw

ADR-0016 shipped the embedded **Chat** with a deliberately thin render: text
messages were plain `<p>` with `white-space: pre-wrap`, every tool call was its
own chip, and there was no way to lift a block of code or a whole reply off a
phone. That was the right scaffolding to prove the read-through/reply loop, but
three gaps made real transcripts hard to read on a phone: assistant prose that
is authored in markdown showed its `**`, `#`, backticks, and tables as literal
characters; a burst of ten tool calls between two sentences buried the prose in
a wall of chips; and there was no copy affordance for code or messages. This ADR
records how those three are closed — a **pure frontend/CSS** change that leaves
the message schema, the provider seam, the API, SSE, and the transcript fold
(ADR-0016) entirely untouched.

The decisions, pinned:

- **Markdown is rendered by a hand-rolled, zero-dependency parser that emits a
  node tree, not an HTML string.** `web/src/lib/markdown.ts` turns source into a
  plain data tree (`Block[]` / `Inline[]`); the view maps that tree to Solid JSX
  nodes directly — never `innerHTML`, never `dangerouslySetInnerHTML`. So the
  renderer is **XSS-safe by construction** and needs **no sanitizer**, and the
  SPA's dependency set stays `solid-js` + `@solidjs/router` (no `marked`,
  `markdown-it`, `remark`, or DOMPurify). This continues the repo's
  twice-documented "no markdown engine — no heavy deps" ethos
  (`web/src/routes/IssueDetail.tsx`) rather than contradicting it. The only
  injection channel a text-node renderer does not already close is the link
  `href`; a **scheme allowlist (http/https/mailto)** closes it — anything else
  (`javascript:`, `data:`, `file:`, relative paths) degrades to visible plain
  text. Supported constructs: headings, bold/italic, inline and fenced code,
  bullet/ordered/**nested** lists, **tables**, blockquotes, links (+ bare-URL
  autolink), horizontal rules. No syntax highlighting (it needs a grammar dep).
- **The parser is total: it never throws and never drops a character.**
  Transcript text arrives **partially** during live streaming (flushed
  mid-turn), so the parser routinely sees an unclosed fence, a dangling `**`, a
  `[text](` with no closing paren, or a half-streamed table. Every such case
  degrades to visible text: an unterminated fence is a code block to EOF, an
  unmatched emphasis delimiter is literal, a rejected link is its literal
  source. The guard is structural — the block scanner always advances, and every
  inline fallback emits the literal delimiter rather than eating it — and is held
  by an adversarial/partial-input test battery (`markdown.test.ts`).
- **Consecutive tool calls coalesce at render time into one "N tool calls"
  disclosure.** The server still emits a flat message list; grouping is a pure
  display concern in `web/src/lib/toolGroups.ts` (`groupMessages`), no
  backend/schema/API change. A run breaks on text, dialog, and lifecycle
  messages; **thinking folds into a run** (Claude interleaves thinking → tool →
  thinking → tool, and thinking is hidden-by-default noise). Threshold is **2+**:
  a lone tool call renders exactly as before. The collapsed summary **counts
  tools only** (folded thinking is not counted) and **rolls up errors** in the
  error color (`5 tool calls · 1 failed`) — a hidden failure is an unacceptable
  UX trap. A still-growing trailing run collapses like a sealed one and shows
  liveness (`· running…`), chosen over "stay expanded until sealed" to avoid the
  scroll-anchoring jank of a run shrinking mid-scroll.
- **The group's open state is controlled and keyed by the first tool's `seq`.**
  The group is a *derived* structure recomputed on every `run.messages.changed`
  refetch; with native `<details>` state alone, an expanded live group would slam
  shut on the next SSE tick. A signal in `RunChatView` keyed by the immutable
  `seq` cursor survives the recompute. The **inner** per-tool chips stay native
  `<details>` (ADR-0016 behavior, unchanged) — their reset on a tail refetch is
  the pre-existing accumulation limitation, out of scope here.
- **Copy-raw is claude.ai-style and source-faithful.** A fenced code block gets a
  header bar (language label left, copy button right, **always visible** —
  mobile has no hover) that copies the block's **raw fence content** (the parser
  retains it). Every **assistant** text message gets a whole-message copy button
  that copies its **raw markdown** (`m().text` is retained — the parsed view is
  never the source of truth). User replies are already plain and selectable;
  thinking is hidden noise — both are copy-less for v1. Icons are inline SVG (no
  icon-library dep); the clipboard is `navigator.clipboard.writeText` (the
  embedded server is a secure context), silent on a missing/blocked clipboard.
- **Mobile-first, and the message column never scrolls the page sideways.**
  Tables and code fences each get their **own** `overflow-x: auto` container
  (faithful shape, swipe sideways — no stacked-card reflow); everything else
  wraps with `overflow-wrap: anywhere`. Styles are plain global CSS with the
  existing custom properties (dark via `prefers-color-scheme`), keeping the v0
  design language (760px column, 44px targets).

## Status

Accepted. Extends ADR-0016's chat surface with no change to its schema, seam,
API, SSE, migrations, or transcript fold — the diff is `web/src/lib/markdown.ts`,
`web/src/lib/toolGroups.ts`, `web/src/routes/RunChat.tsx`, and `web/src/base.css`
plus tests. Adds **no** runtime npm dependency. A future Codex/Gemini provider
inherits all of this for free: it produces the same universal `text`/`tool`
schema, and rendering is provider-agnostic.

## Considered options

- **Pull in a markdown library (`marked`/`markdown-it`/`remark`) plus DOMPurify.**
  Rejected: it contradicts the repo's stated no-markdown-engine ethos, adds a
  meaningful bundle/attack surface (a sanitizer is only needed *because* those
  emit HTML strings), and buys constructs we don't want (raw HTML passthrough,
  which is exactly the XSS channel). The hand-rolled node-tree renderer is
  XSS-safe by construction and needs no sanitizer.
- **Render markdown to an HTML string and set `innerHTML` (with sanitizing).**
  Rejected for the same reason — it reopens the injection channel the node-tree
  approach never opens, and makes correctness depend on the sanitizer's blocklist
  staying ahead of bypasses.
- **Group tool runs on the backend / in the message schema.** Rejected: grouping
  is a display concern that varies with viewport and the thinking toggle; baking
  it into the schema would couple the provider seam to a UI decision and force a
  migration for a CSS-level change. The flat list stays the source of truth.
- **Keep the live group expanded until the run is sealed.** Rejected: a run that
  shrinks (collapses) mid-scroll fights scroll anchoring on a phone. Consistent
  collapse with a liveness hint keeps the scroll position stable; watching live
  detail is one tap.
- **Syntax highlighting in code blocks.** Deferred: it needs a highlighter dep
  and per-language grammars — a heavy addition for a phone-first reply surface.
  Plain monospace with a copy button covers the need.
- **Copy-raw on user and thinking messages.** Deferred: user replies are already
  plain selectable text and thinking is hidden-by-default noise; assistant
  messages are where the value is for v1.

## Consequences

- `web/src/lib` gains two pure, unit-tested helpers (`markdown.ts`,
  `toolGroups.ts`) matching the existing pure-helper convention
  (`chatStream.ts`, `conversation.ts`). The markdown parser carries an
  adversarial/partial-input test battery because live streaming *guarantees* it
  sees malformed input.
- `RunChatView` owns one new piece of state — the tool-group open-state signal —
  reset on run navigation alongside the rest of the chat state.
- The grouped render item and each text message are recomputed per refetch (the
  group is derived; markdown re-parses on the growing tail). At the 60-message
  window cap this is cheap; the open-state signal is the targeted mitigation for
  the one piece of state that must not reset. If it ever bites, memoizing render
  items by `seq` is the follow-up.
- No new fragile coupling: unlike ADR-0016's transcript/keystroke recipes, this
  is self-contained frontend with no dependency on a pinned Claude Code version.
