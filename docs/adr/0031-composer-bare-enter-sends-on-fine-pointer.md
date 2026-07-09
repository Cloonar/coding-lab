# Composer bare Enter sends on fine-pointer devices, reversing ADR-0022/0029's always-newline rule

ADR-0022 pinned "Cmd/Ctrl+Enter sends … never bare Enter, so a phone's return
key inserts a newline instead of firing a send" and even weighed and rejected
"Bare Enter sends (with Shift+Enter for a newline)" outright. ADR-0029 carried
the same clause forward unchanged when it reversed everything else about the
composer: "bare Enter stays a newline in every state." Both readings treated
the composer as phone-first across the board (ADR-0005), but the two web
composers are used from laptops and desktops at least as often as phones, and
on those devices a chat-shaped textarea that eats Enter as a no-op reads as
broken, not careful — chat surfaces conventionally send on Enter when a
physical keyboard is present. Issue #70 asked
for the obvious fix: stop pretending every client is a phone, and ask the
platform what kind of pointer it actually has.

This ADR is a **pure frontend diff** — no colour/type/radius/shadow change, no
backend/API/SSE/message-schema/provider/migration change, the same class of
change as ADR-0019/0022/0029/0030. It **partially reverses** ADR-0022's and
ADR-0029's bare-Enter clause (including ADR-0022's rejected-option entry, which
is now overturned rather than merely superseded) and otherwise leaves both
ADRs untouched: ADR-0029's always-Send contract, no-lab-managed-queue
decision, and header-relocated Interrupt all stand exactly as pinned.

The decisions, pinned (settled with the maintainer 2026-07-09 via /grill-me —
do not re-litigate):

- **Bare Enter sends — gated on fine-pointer, evaluated fresh per keydown.**
  The gate is `window.matchMedia('(hover: hover) and (pointer: fine)').matches`,
  called inline inside the keydown handler — **never** snapshotted once at
  mount or module load — so a hybrid device (a tablet with a dockable
  trackpad, a Chromebook with a detachable keyboard) picks up the current
  pointer situation on every keystroke rather than freezing whatever was true
  when the component first rendered. On a touch-only device the gate reads
  `false`: the virtual keyboard's return key keeps inserting a newline exactly
  as before, and the on-screen Send button remains the one submit affordance —
  the phone-first rationale of ADR-0005/0022 is **preserved on phones**, not
  reversed; only the fine-pointer case flips.
- **Shift+Enter and Alt+Enter stay an explicit newline everywhere.** The
  implementation is simply *not intercepting* those chords — no
  `preventDefault`, so the textarea's native newline-insertion runs unmodified.
  This holds on every pointer type, including fine-pointer, so a mouse-and-
  keyboard user retains an explicit escape hatch for a multi-line reply
  without reaching for the mouse.
- **Cmd/Ctrl+Enter keeps sending everywhere**, exactly as ADR-0022 first
  established and ADR-0029 carried forward: touch + hardware-keyboard alike,
  regardless of what the pointer-type gate says. It was never conditioned on
  pointer type and stays that way — the fine-pointer gate is additive, not a
  replacement for the modifier chord.
- **Enter sends in every unlocked agent state** — `idle`, `needs_input`, and
  `working` alike — inheriting ADR-0029's always-Send contract verbatim: no
  state where the key silently changes meaning. The composer never re-reads
  `working` to decide whether Enter sends, the same untrusted-signal
  discipline ADR-0029 established for the button itself.
- **Slash-command popover precedence is unchanged.** While the popover is
  open, Enter/Tab accept the highlighted command (the existing acceptance
  branch in `RunChat.tsx`'s `onKeyDown`); the send gate is only reached once
  the popover is closed. Cmd/Ctrl+Enter still sends over an open popover,
  matching the pre-existing `!(e.metaKey || e.ctrlKey)` guard on that branch.
- **New IME guard.** Enter fired while `e.isComposing` is true, or with the
  legacy `e.keyCode === 229` (the commit keydown some engines still report
  this way), never sends. This was **absent everywhere in `web/src`** before
  this ADR — it was safe to omit while only Cmd/Ctrl+Enter could send (CJK
  input methods do not commit compositions with a chorded Enter), but it is
  not safe once bare Enter sends: committing a composed string with Enter is
  the single most common keystroke in CJK input and must never also submit
  the form.
- **Empty composer + bare Enter: no send, and `preventDefault` regardless.**
  An empty or whitespace-only textarea does not send on bare Enter (matching
  the existing non-empty guard on `send()`/`canSend` in `RunChat.tsx`), and
  the keydown is still prevented so an empty composer never gains a stray
  leading newline. `NewRun.tsx` differs from the chat composer here on
  purpose: an empty body is a **valid, existing** "plain spawn" affordance
  there (see the comment above `send()` in `NewRun.tsx`), reachable via the
  Start button and via Cmd/Ctrl+Enter — that explicit path is unchanged — but
  a bare Enter landing on an empty box by accident must not launch an
  instance. `NewRun.tsx`'s `onKeyDown` therefore adds its own
  `text().trim() === ''` guard specifically for the un-modified case, on top
  of the shared gate.
- **One shared gate, not two.** `web/src/lib/composerKeys.ts` exports
  `isComposerSend(e: KeyboardEvent): boolean`, a pure event→bool predicate
  (the caller still owns `preventDefault()` and any empty/busy check) —
  encoding the IME guard, the modifier-chord always-sends rule, the
  Shift/Alt-passthrough, and the fine-pointer `matchMedia` gate in one place.
  Both composers — the chat reply textarea's `onKeyDown` in
  `web/src/routes/RunChat.tsx` and the new-run composer's `onKeyDown` in
  `web/src/routes/NewRun.tsx` — call it instead of each re-deriving the rule,
  so the two cannot silently drift the way two independently-hand-rolled
  keydown handlers eventually would.
- **`enterkeyhint="send"` stays out of scope, explicitly rejected.** A mobile
  keyboard offers exactly one labelled action key; relabelling it "send" would
  cost every phone user the newline their return key currently inserts, with
  no way to get a newline back short of Shift+Enter on a keyboard that no
  longer advertises it exists. The on-screen Send button remains the
  discoverable phone affordance.
- **No user-facing setting or toggle.** The behaviour is a fixed function of
  pointer type, not an operator preference to expose. This mirrors ADR-0022's
  "no colour/type/radius/shadow redesign, no config surface" discipline for
  this class of change.
- **Tooltip text follows the new contract.** `NewRun.tsx`'s Start button
  `title` moves from "Start run (Cmd/Ctrl+Enter)" to "Start run (Enter)" — the
  fine-pointer case is now the common one on the surface where this tooltip
  is shown. The chat composer's Send button carries no shortcut hint before or
  after this change; nothing to update there.

## Status

Accepted. Partially supersedes ADR-0022 and ADR-0029: on fine-pointer devices
only, bare Enter now sends where both ADRs pinned it as an unconditional
newline; ADR-0022's rejected option "Bare Enter sends (with Shift+Enter for a
newline)" is overturned for that same fine-pointer case. Touch devices keep
the pre-existing return-as-newline behaviour untouched. Everything else in
ADR-0029 — the always-Send button that never reads `working`, no lab-managed
queue, and Interrupt living in the chat header next to Stop — remains in
force, unamended. A pure SPA diff: `web/src/lib/composerKeys.ts` (new),
`web/src/routes/RunChat.tsx`, `web/src/routes/NewRun.tsx`, and their test
files, plus this ADR and the annotations to ADR-0022/ADR-0029. **No**
Go, API, SSE, message-schema, provider, or migration change — a future
Codex/Gemini provider inherits the new gate for free, same as every prior
composer-behaviour ADR.

## Considered options

- **Snapshot the pointer-type check once at mount.** Rejected: a hybrid
  device that docks or undocks a physical keyboard/trackpad mid-session would
  keep whatever gate value was true at first render, silently wrong until a
  full remount. Evaluating `matchMedia(...).matches` fresh inside the keydown
  handler costs nothing and stays correct across the session.
- **`enterkeyhint="send"` to signal the new behaviour on-screen.** Rejected
  (see pinned decisions above): the platform offers one action-key label, and
  claiming it for "send" silently removes the newline affordance from every
  phone keyboard, which is exactly the regression ADR-0005/0022 were written
  to avoid.
- **A user-facing setting to choose Enter-sends vs. Enter-newlines.** Rejected:
  the fine-pointer/touch split already encodes the right default per device
  without asking the operator to configure anything; a toggle would be a
  second, redundant lever for the same decision the media query already makes
  correctly.
- **Gate on `navigator.maxTouchPoints` or user-agent sniffing instead of
  `matchMedia`.** Rejected: `(hover: hover) and (pointer: fine)` is the
  standards-track signal for "the primary input can hover and points
  precisely" and updates live as the CSS media environment changes (e.g. a
  detachable keyboard docking); UA sniffing is brittle and touch-point counts
  don't distinguish a trackpad-and-touchscreen laptop from a phone.
- **Skip the IME guard, reasoning CJK users would just avoid bare Enter to
  commit.** Rejected: composition-commit-via-Enter is the default, expected
  keystroke for CJK input methods across every major OS/IME; shipping bare
  Enter as a send without the guard would misfire a send on essentially every
  CJK reply.
- **Duplicate the gate logic in each composer instead of a shared helper.**
  Rejected: `NewRun.tsx`'s keydown comment had to assert "matches the chat
  composer" by hand — a mirror maintained by convention is exactly how two
  hand-rolled handlers drift. A shared `isComposerSend` in `composerKeys.ts`
  makes the sameness structural, and is what issue #70 explicitly asked to
  consider.

## Consequences

- `web/src/lib/composerKeys.ts` is new: a single pure predicate,
  `isComposerSend(e: KeyboardEvent): boolean`, imported by both
  `RunChat.tsx` and `NewRun.tsx`'s keydown handlers. Any future third composer
  (e.g. a dialog free-text reply) should call the same helper rather than
  re-deriving the rule.
- `RunChat.tsx`'s `onKeyDown` gains the `isComposerSend` call in place of the
  old literal `e.metaKey || e.ctrlKey` check, with the popover-acceptance
  branch unchanged ahead of it.
- `NewRun.tsx`'s `onKeyDown` gains the same call, plus its own
  `text().trim() === ''` guard restricted to the un-modified case, preserving
  Cmd/Ctrl+Enter's existing explicit-empty-spawn behaviour untouched. Its
  Start button tooltip changes from "Start run (Cmd/Ctrl+Enter)" to
  "Start run (Enter)".
- `RunChat.test.tsx`'s "sends on Cmd/Ctrl+Enter but never on bare Enter" test
  is rewritten for the new contract: bare Enter sends under a mocked
  fine-pointer `matchMedia`, does not send under a mocked coarse/no-hover
  profile, Shift+Enter never sends, an `isComposing: true` Enter never sends,
  and Cmd/Ctrl+Enter keeps sending unconditionally including while `working`
  (the existing ADR-0029 coverage). `NewRun.tsx` gains equivalent keyboard
  coverage it did not have before.
- The backend `Reply` path (tmux bracketed-paste + `Enter`,
  `internal/chat/chat.go`) is untouched — this ADR only changes which
  browser keystroke triggers the existing `replyRun`/`startInstance` calls,
  never what happens once a send is triggered.
