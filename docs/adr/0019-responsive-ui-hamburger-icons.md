# Responsive UI overhaul: hamburger nav, spacing scale, vendored icon system

ADR-0005 pinned the SPA as phone-first — a 760px single column, 44px touch
targets, 16px inputs, OS-driven dark mode — but the app _chrome_ was still laid
out for a wide viewport. The topbar rendered the brand, five text nav links, the
SSE live dot, the username, and Log out inline at every width (a lone ad-hoc
560px rule hid only the username); spacing values were ad-hoc literals scattered
through the stylesheet; and a repo card's instance row crammed the title/branch,
up to three state chips, a Chat link, an Open deep link, and a Stop button onto
one wrapping flex line — squeezing the title to an unreadable sliver on a ~360px
phone. The only iconography was unicode glyphs (`←`, `↗`, `×`, `▸`). This ADR
records the overhaul that makes the operator UI genuinely usable on a phone. It
is a **pure frontend/CSS** change _within_ the existing design language — no
colour/type/radius/shadow redesign — and touches only the SPA: no message
schema, provider seam, API, SSE, or migration.

The decisions, pinned:

- **The nav collapses to a hamburger below 640px; the bottom tab bar and
  priority+ overflow were both rejected.** Under 640px the topbar shows only
  brand · live dot · hamburger; the five section links, the username, and Log
  out move into a full-width dropdown panel anchored to the bottom of the sticky
  topbar, over a viewport scrim. The panel closes on outside tap (scrim), `Esc`,
  and route navigation — including a tap on the _already-active_ link, which
  fires no navigation, so the menu closes on link activation directly rather
  than only on a pathname change. The trigger carries an accessible name and
  `aria-expanded`. At ≥640px the inline nav renders as before, with the username
  always visible; the old 560px hide-username rule is **deleted** (640
  supersedes it). A bottom tab bar was rejected because it collides with the
  chat's pinned composer; priority+ overflow was rejected as needless
  complexity for five items.

- **A tokenized `--space-*` scale (4px grid) joins the existing token block.**
  It is theme-agnostic — defined once in `:root`, never duplicated in the dark
  block — and card padding, card-list gaps, and section margins reference it in
  place of the old literals. Phone density is essentially unchanged; a single
  `@media (min-width: 640px)` block steps the semantic consumers up one rung for
  a slightly roomier desktop. The 44px touch-target and 16px-input rules
  survive untouched.

- **Instance rows stack into two lines below 640px.** Line one is the title +
  branch (+ AFK budget) at full width with its state chips; line two is the
  Chat / Open (or the connecting pulse) / Stop actions. At ≥640px the single
  line is unchanged. `min-width: 0` down the flex chain plus
  `overflow-wrap: anywhere` on the mono branch keep a long branch name wrapping,
  so the page never scrolls sideways at 360px — the load-bearing acceptance
  criterion.

- **One vendored Lucide icon system sits behind a single `Icon` component — no
  new dependency.** Fifteen glyphs are inline SVG paths (Lucide, ISC-licensed,
  attribution in the component header), drawn `fill="none"
  stroke="currentColor"` so dark mode needs no extra work; the size is a prop
  and the `<svg>` is always `aria-hidden` (the accessible name lives on the
  enclosing button/link). No icon font and no icon package (`lucide-solid` et
  al.) — the SPA dependency set stays `solid-js` + `@solidjs/router`,
  continuing the repo's no-heavy-deps ethos (ADR-0018). The two copy/check SVGs
  that ADR-0018 inlined ad hoc fold into this component, so `Icon` is the single
  icon entry point.

- **Icon-only for universal actions in tight spots; icon+text everywhere else.**
  Icon-only, each carrying **both** `aria-label` and `title`: the hamburger,
  back, close/dismiss, send, stop, external-link, and the instance-row actions
  on mobile. Icon+text: the nav menu entries (folder = Repos, key =
  Credentials, play = Runs, ticket = Tokens, gear = Settings), Log out, and
  form/action buttons. The chat composer's Send becomes an icon-only
  paper-plane; the chat header's Stop resting state is a 44px danger-coloured
  stop-square icon.

- **The two-phase stop confirmation stays verbal and unchanged.** The chat
  header's stop _icon_ only opens the confirmation; the destructive step keeps
  the textual "Confirm stop" + "Cancel" pair from ADR-0016. Icon-only never
  applies to a destructive confirmation.

## Status

Accepted. A pure SPA diff: `web/src/base.css`, `TopBar.tsx`, `InstanceList.tsx`,
`OpenAffordance.tsx`, `RunChat.tsx`, `ErrorBanner.tsx`, the new
`components/Icon.tsx`, and the tests that query the now-icon-only controls by
accessible name. Adds **no** runtime npm dependency. A future Codex/Gemini
provider inherits all of it for free — the chrome is provider-agnostic.

## Considered options

- **A bottom tab bar for mobile navigation.** Rejected: the chat view (`/runs/:id`)
  pins a composer to the bottom of the viewport, and a tab bar would fight it for
  that space. A top dropdown keeps navigation out of the composer's zone.
- **Priority+ / overflow-menu navigation** (show what fits, tuck the rest behind
  a "more"). Rejected: it is real machinery for a five-item, fixed nav where a
  plain dropdown is both simpler and fully predictable.
- **An icon font or an icon package** (`lucide-solid`, `@tabler/icons`, …).
  Rejected: it adds a dependency and bundle weight for what fifteen inline paths
  behind one component cover, and an icon font brings a11y and FOUT baggage.
  Vendoring the paths keeps the "no heavy deps" line the repo has held since
  ADR-0018.
- **Swapping icon vs. text by conditionally rendering** (JS `matchMedia`).
  Rejected in favour of CSS: the control renders both an icon and a label span
  and the breakpoint (or the chat-header context) decides which is _shown_,
  while the accessible name stays fixed on the control. This keeps the swap
  declarative, keeps the accessible name stable across breakpoints, and lets
  jsdom unit tests query by role/accessible name without evaluating CSS.
- **Leaving the desktop text actions on the mobile instance row.** Rejected —
  that crowding is exactly the unreadable-title squeeze this ADR removes.

## Consequences

- `components/Icon.tsx` is the single icon entry point: adding a glyph is one
  path plus one `IconName` union member; every control colours its icon through
  `currentColor`, so dark mode and `.danger` variants are free.
- Component tests select icon-only controls by accessible name (`aria-label`),
  not by visible text — the pattern to follow for any future icon-only control.
- A CSS ordering invariant now matters and is worth stating: the mobile-first
  "hide in the base rule, show in the `@media (min-width: 640px)` rule" pattern
  only works when the base hide-rule precedes the media show-rule in source. The
  hamburger is the exception — it also carries `.icon-btn`, whose
  `display: inline-flex` is declared _later_ in the file and ties on
  specificity — so its desktop hide-rule is scoped `.topbar .nav-toggle` to
  out-specify `.icon-btn`. jsdom applies no CSS, so this class of breakpoint bug
  is invisible to the unit suite and must be caught by rendering.
