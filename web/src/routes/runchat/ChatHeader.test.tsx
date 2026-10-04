// ChatHeader behavioral contract (issue #7), header-area slice of the
// RunChat contract split (issue #194), reshaped by issue #58 §1:
// - a two-line title block: the click-to-edit title over a secondary line of
//   project link · forge link · the run's spawn-time model · effort — at every
//   width, catalog pretty labels with the raw id as fallback, hidden for a
//   legacy row with no model (issue #68);
// - the context meter (issue #243 / ADR-0061) is a ring + percentage button at
//   every width, in every conversational state, whenever usable usage exists
//   — no longer nested in the model text — tinting amber at >=80% and red at
//   >=95%; it opens Run details (a bottom sheet <1024px, an anchored popover
//   >=1024px: model, effort, tokens, branch, base, commits behind, and a Pull
//   base that sends `/pull-base` only while a live run is behind);
// - no conversational state badge and no "N behind" chip at any width;
// - the `•••` menu at every width (a bottom sheet <640px, an anchored dropdown
//   >=640px) always offers "Run details" and, whenever the run is live, the
//   one-tap turn Interrupt (POST /interrupt, no confirm, a `pause` glyph
//   distinct from the two-step danger `square` Stop); below 640px it also
//   carries the open affordance and the two-step Stop.

import { describe, expect, it } from 'vitest';
import type { ConversationState } from '../../api';
import {
  DESKTOP_QUERY,
  baseRepo,
  baseRun,
  container,
  h,
  installChatHooks,
  menuItem,
  mountChat,
  moreButton,
  settle,
  stubMatchMedia,
  buttonByLabel,
} from './harness';

// The header's own breakpoint: the `•••` menu is a dropdown from here up.
const MENU_DROPDOWN_QUERY = '(min-width: 640px)';

const header = () => container.querySelector('header.chat-header') as HTMLElement;
const meterButton = () => container.querySelector<HTMLButtonElement>('button.chat-context-meter');
const details = () => container.querySelector<HTMLElement>('section.chat-details');
/** A Run details fact row's <dd> text, by its <dt> label. */
const fact = (label: string): string | null | undefined =>
  Array.from(details()?.querySelectorAll('.chat-details-fact') ?? [])
    .find((row) => row.querySelector('dt')?.textContent === label)
    ?.querySelector('dd')?.textContent;
const withUsage = (used: number, limit: number) => {
  h.messagesOnServer = { ...h.messagesOnServer, context_usage: { used, limit } };
};

installChatHooks();

describe('ChatHeader', () => {
  it('titles the chat with the generated label and falls back to the provider web link', async () => {
    h.runOnServer = { ...baseRun(), deep_link_url: null };
    await mountChat();

    expect(container.querySelector('.chat-title-text')?.textContent).toBe('dom · 15:00');
    expect(container.querySelector('.chat-title-project')?.textContent).toBe('proj');
    // ADR-0017: the fallback URL + tooltip come from the providers API, not a
    // hardcoded constant. Scope to the OpenAffordance link (a.card-link) so the
    // header's forge git-icon link (issue #132) isn't mistaken for it.
    const link = container.querySelector<HTMLAnchorElement>('a.card-link');
    expect(link?.getAttribute('href')).toBe('https://claude.ai/code');
    expect(link?.getAttribute('title')).toContain('claude.ai session picker');
  });

  it('shows a set title verbatim with the project name as secondary text + session tooltip', async () => {
    h.runOnServer = { ...baseRun(), title: 'Fix the flaky login test' };
    await mountChat();

    const btn = container.querySelector('button.chat-title')!;
    expect(btn.querySelector('.chat-title-text')?.textContent).toBe('Fix the flaky login test');
    // The project name (not the old repeated label · session string) rides
    // beside the title — a SIBLING of the button now (issue #132), not inside
    // it; the full session name — the branch/worktree/tmux correlation — is the
    // button's tooltip.
    expect(btn.querySelector('.chat-title-project')).toBeNull();
    expect(container.querySelector('.chat-title-project')?.textContent).toBe('proj');
    expect(btn.getAttribute('title')).toBe('proj~dom-20260706-1500');
  });

  it('renames inline: click the title, submit → PATCH {title}, refetch shows the new name', async () => {
    await mountChat();
    // No title set: the project name still rides the generated title (issue #120).
    expect(container.querySelector('.chat-title-project')?.textContent).toBe('proj');

    (container.querySelector('button.chat-title') as HTMLButtonElement).click();
    await settle();
    const input = container.querySelector('.chat-title-input') as HTMLInputElement;
    expect(input).not.toBeNull();
    expect(input.value).toBe(''); // seeded with run.title ?? ''
    expect(input.placeholder).toBe('dom · 15:00'); // the generated title, repo-less

    input.value = '  Ship it  ';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    buttonByLabel('Save title')!.click();
    await settle();

    expect(h.titlePatches).toEqual([{ title: 'Ship it' }]); // trimmed
    // Back in view mode, the refetched run's title renders.
    expect(container.querySelector('.chat-title-input')).toBeNull();
    expect(container.querySelector('.chat-title-text')?.textContent).toBe('Ship it');
  });

  it('clears the override on empty submit, and Escape/Cancel exit without saving', async () => {
    h.runOnServer = { ...baseRun(), title: 'Old name' };
    await mountChat();

    // Escape exits edit mode without a PATCH.
    (container.querySelector('button.chat-title') as HTMLButtonElement).click();
    await settle();
    const input = container.querySelector('.chat-title-input') as HTMLInputElement;
    expect(input.value).toBe('Old name');
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await settle();
    expect(container.querySelector('.chat-title-input')).toBeNull();
    expect(h.titlePatches).toHaveLength(0);

    // Cancel exits without saving too.
    (container.querySelector('button.chat-title') as HTMLButtonElement).click();
    await settle();
    buttonByLabel('Cancel rename')!.click();
    await settle();
    expect(container.querySelector('.chat-title-input')).toBeNull();
    expect(h.titlePatches).toHaveLength(0);

    // Saving empty clears (PATCH {title: null}) — that IS the reset path —
    // and the header falls back to the generated title.
    (container.querySelector('button.chat-title') as HTMLButtonElement).click();
    await settle();
    const again = container.querySelector('.chat-title-input') as HTMLInputElement;
    again.value = '   ';
    again.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    buttonByLabel('Save title')!.click();
    await settle();
    expect(h.titlePatches).toEqual([{ title: null }]);
    expect(container.querySelector('.chat-title-text')?.textContent).toBe('dom · 15:00');
    expect(container.querySelector('.chat-title-project')?.textContent).toBe('proj');
  });

  it('falls back to the branch with no project span for a legacy no-~ session name', async () => {
    h.runOnServer = { ...baseRun(), session_name: 'legacy-session', title: null };
    await mountChat();

    const btn = container.querySelector('button.chat-title')!;
    expect(btn.querySelector('.chat-title-text')?.textContent).toBe(h.runOnServer.branch);
    // No `~` in the session name → no project name at all (issue #132): neither
    // the muted text/link nor the forge icon renders.
    expect(container.querySelector('.chat-title-project')).toBeNull();
    expect(container.querySelector('.chat-title-forge')).toBeNull();
    expect(btn.getAttribute('title')).toBe('legacy-session');
  });

  it('renders the project name as a link to the repo issues page', async () => {
    await mountChat();

    const link = container.querySelector<HTMLAnchorElement>('a.chat-title-project');
    expect(link).not.toBeNull();
    expect(link!.textContent).toBe('proj');
    // The issues page is the de-facto repo landing (no /repos/:id route).
    expect(link!.getAttribute('href')).toBe('/repos/repo_1/issues');
  });

  it('renders the git-icon forge link (new tab) for a forgejo repo with a parseable remote', async () => {
    await mountChat();

    const forge = container.querySelector<HTMLAnchorElement>('a.chat-title-forge');
    expect(forge).not.toBeNull();
    // forgeWebUrl strips the .git suffix off the clone URL's path.
    expect(forge!.getAttribute('href')).toBe('https://git.cloonar.com/Cloonar/proj');
    expect(forge!.getAttribute('target')).toBe('_blank');
    expect(forge!.getAttribute('rel')).toBe('noreferrer');
    expect(forge!.getAttribute('aria-label')).toBe('Open on forge');
    // The git-branch glyph (two circles), a deliberate choice over the
    // external-link icon (three paths, no circles).
    expect(forge!.querySelectorAll('svg circle')).toHaveLength(2);
  });

  it('hides the git-icon forge link when the repo has no forge (forge_kind none)', async () => {
    h.repoOnServer = { ...baseRepo(), forge_kind: 'none' };
    await mountChat();

    // The forge icon is hidden entirely — not greyed — while the project name
    // link still renders (it depends on repo_id, not the forge URL).
    expect(container.querySelector('.chat-title-forge')).toBeNull();
    expect(container.querySelector('a.chat-title-project')?.getAttribute('href')).toBe(
      '/repos/repo_1/issues',
    );
  });

  it('shows a copyable tmux-attach for a link-less provider (no web fallback)', async () => {
    // A provider with no remote-control knob is ALWAYS remote:false (the server
    // clamps it) — its attach affordance must survive the remote gate untouched
    // (issue #163), which a bare `if (!run.remote)` check would have killed.
    h.runOnServer = { ...baseRun(), provider: 'codex', remote: false, deep_link_url: null };
    h.providersOnServer = [
      {
        id: 'codex',
        display_name: 'Codex CLI',
        supports_remote: false,
        auth: { kind: 'external' },
        models: [],
        efforts: [],
        options: [],
      },
    ];
    await mountChat();

    // No OpenAffordance web link (a.card-link) for a link-less provider; the
    // header's forge git-icon link (issue #132) is a separate anchor and does
    // not count here.
    expect(container.querySelector('a.card-link')).toBeNull();
    const attach = container.querySelector('button.attach-copy');
    expect(attach?.textContent).toContain('Copy attach');
    expect(attach?.getAttribute('title')).toContain('tmux attach -t proj~dom-20260706-1500');
  });

  it('hides the Open affordance entirely for a remote-capable run spawned without remote control', async () => {
    // Remote control off = no session was registered with the provider's web
    // app, so the deep link AND its fallback picker link would both point at
    // nothing (issue #163): render nothing at all, not even the connecting pulse.
    h.runOnServer = { ...baseRun(), remote: false, deep_link_url: null };
    await mountChat();

    expect(container.querySelector('a.card-link')).toBeNull();
    expect(container.querySelector('button.attach-copy')).toBeNull();
    expect(container.querySelector('.chip.connecting')).toBeNull();
    // The rest of the header is unaffected — this is not an error state.
    expect(container.querySelector('.chat-title-text')?.textContent).toBe('dom · 15:00');
  });

  it('always renders the ••• menu trigger, even when the run is not live', async () => {
    h.runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    h.messagesOnServer = { ...h.messagesOnServer, state: 'ended' };
    await mountChat();

    expect(moreButton()).not.toBeNull();
    // Closed by default → no menu items leak into the DOM.
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
  });

  it('opens the ••• menu as a bottom sheet below 640px with Run details, the open affordance, Interrupt and Stop run', async () => {
    await mountChat(); // default fixture: live, needs_input; jsdom matches no media query

    expect(container.querySelector('.chat-menu-panel')).toBeNull();
    moreButton()!.click();
    await settle();

    const panel = container.querySelector('.chat-menu-panel');
    expect(panel).not.toBeNull();
    // A sheet over a dimming scrim, rendered OUTSIDE the header (its stacking
    // context and <640px overlay transform would trap a fixed sheet).
    expect(panel!.classList.contains('chat-sheet')).toBe(true);
    expect(panel!.closest('header')).toBeNull();
    expect(container.querySelector('.chat-sheet-scrim')).not.toBeNull();
    expect(panel!.getAttribute('role')).toBe('menu');
    expect(moreButton()!.getAttribute('aria-expanded')).toBe('true');

    expect(menuItem('Run details')).toBeDefined();
    expect(container.querySelector('.chat-menu-open')).not.toBeNull(); // the open affordance
    expect(menuItem('Interrupt')).toBeDefined(); // live turn Interrupt (ADR-0029)
    expect(menuItem('Stop run…')).toBeDefined();
    expect(menuItem('Show thinking')).toBeUndefined();
    expect(menuItem('New conversation')).toBeUndefined();

    // The model info row left the menu: model · effort rides the header's
    // secondary line at every width now (issue #58 §1).
    expect(container.querySelector('.chat-menu-info')).toBeNull();
    const menuItemTexts = Array.from(panel!.querySelectorAll('[role=menuitem]')).map(
      (el) => el.textContent,
    );
    expect(menuItemTexts.some((t) => t?.includes('opus[1m]'))).toBe(false);
  });

  it('opens an anchored dropdown at >=640px with Run details and Interrupt, the open affordance and Stop staying inline', async () => {
    stubMatchMedia().set(MENU_DROPDOWN_QUERY, true);
    await mountChat();

    // Inline at this width: the open affordance and the two-step Stop.
    expect(container.querySelector('.chat-desktop-actions a.card-link')).not.toBeNull();
    expect(container.querySelector('.chat-desktop-actions .chat-stop')).not.toBeNull();

    moreButton()!.click();
    await settle();
    const panel = container.querySelector('.chat-menu-panel');
    expect(panel?.classList.contains('chat-menu-dropdown')).toBe(true);
    expect(panel!.closest('header')).not.toBeNull(); // anchored inside the header
    expect(container.querySelector('.chat-sheet-scrim')).toBeNull();
    expect(container.querySelector('.chat-menu-scrim')).not.toBeNull(); // outside-tap catcher

    expect(menuItem('Run details')).toBeDefined();
    expect(menuItem('Interrupt')).toBeDefined();
    // What fits the row stays out of the dropdown.
    expect(menuItem('Stop run…')).toBeUndefined();
    expect(container.querySelector('.chat-menu-open')).toBeNull();

    // Outside tap closes it.
    (container.querySelector('.chat-menu-scrim') as HTMLElement).click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
  });

  it('omits Interrupt and Stop run from the menu when the run has ended, keeping Run details', async () => {
    h.runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    h.messagesOnServer = { ...h.messagesOnServer, state: 'ended' };
    await mountChat();

    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).not.toBeNull();
    // Run details is always reachable — it isn't run state.
    expect(menuItem('Run details')).toBeDefined();
    // Both the turn Interrupt and Stop are live-gated — gone on an ended run.
    expect(menuItem('Interrupt')).toBeUndefined();
    expect(menuItem('Stop run…')).toBeUndefined();
  });

  it('closes the menu on Escape, consuming the event', async () => {
    await mountChat();
    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).not.toBeNull();

    const esc = new KeyboardEvent('keydown', { key: 'Escape', cancelable: true });
    window.dispatchEvent(esc);
    await settle();
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
    // Consumed, so the tool panel's window Esc-close skips it (issue #145).
    expect(esc.defaultPrevented).toBe(true);
  });

  it('closes the sheet on a scrim tap and abandons a half-armed Stop', async () => {
    await mountChat();
    moreButton()!.click();
    await settle();
    menuItem('Stop run…')!.click();
    await settle();
    expect(menuItem('Confirm stop')).toBeDefined();

    (container.querySelector('.chat-sheet-scrim') as HTMLElement).click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).toBeNull();

    // Reopened: the confirm did not linger.
    moreButton()!.click();
    await settle();
    expect(menuItem('Confirm stop')).toBeUndefined();
    expect(menuItem('Stop run…')).toBeDefined();
  });

  it.each<ConversationState>(['working', 'needs_input', 'question', 'idle'])(
    'renders no conversational state badge in the header (state %s)',
    async (state) => {
      h.messagesOnServer = { ...h.messagesOnServer, state };
      await mountChat();

      // The dock's status line (issue #58 §2) names the state in words now;
      // neither the <640px dot nor the >=640px chip renders at any width.
      expect(header().querySelector('.chat-state-dot')).toBeNull();
      expect(header().querySelector('.chat-state-chip')).toBeNull();
      expect(header().querySelector('.chip.convo')).toBeNull();
    },
  );

  it('omits the exposure badge for a run that has exposed nothing', async () => {
    await mountChat(); // baseRun() carries no exposed_secrets
    expect(container.querySelector('.chat-exposed-dot')).toBeNull();
    expect(container.querySelector('.chip.exposed')).toBeNull();
  });

  it('renders a singular exposure badge naming the one exposed secret', async () => {
    h.runOnServer = { ...baseRun(), exposed_secrets: ['API_KEY'] };
    await mountChat();
    // The <640px dot is a labeled graphic; the >=640px chip carries the text.
    const dot = container.querySelector('.chat-exposed-dot');
    expect(dot?.getAttribute('role')).toBe('img');
    expect(dot?.getAttribute('aria-label')).toContain('API_KEY');
    const chip = container.querySelector('.chip.exposed');
    expect(chip?.textContent).toBe('API_KEY exposed');
    expect(chip?.getAttribute('title')).toContain('API_KEY');
  });

  it('renders a plural exposure badge with a tooltip listing every exposed secret', async () => {
    h.runOnServer = { ...baseRun(), exposed_secrets: ['ALPHA_KEY', 'ZEBRA_KEY'] };
    await mountChat();
    const chip = container.querySelector('.chip.exposed');
    expect(chip?.textContent).toBe('2 secrets exposed');
    expect(chip?.getAttribute('title')).toContain('ALPHA_KEY');
    expect(chip?.getAttribute('title')).toContain('ZEBRA_KEY');
  });

  it.each<ConversationState>(['working', 'idle', 'question'])(
    'puts Interrupt in the ••• menu, firing /interrupt on one tap whenever live (state %s)',
    async (state) => {
      // The derived state can be stale (issue #38), so the menu Interrupt is
      // gated on the live outcome alone — offered in every state.
      h.messagesOnServer = { ...h.messagesOnServer, state };
      await mountChat();

      // The inline header Interrupt is gone at every width.
      expect(header().querySelector('.chat-turn-interrupt')).toBeNull();
      expect(header().querySelector('button[aria-label="Interrupt"]')).toBeNull();

      moreButton()!.click();
      await settle();
      const interrupt = menuItem('Interrupt');
      expect(interrupt).toBeDefined();
      expect(interrupt!.title).toBe('Interrupt the current turn (keeps the session)');
      // The `pause` glyph (two rects) reads distinct from the danger two-step
      // `square` Stop (one rect) listed below it.
      expect(interrupt!.querySelectorAll('svg rect')).toHaveLength(2);
      expect(menuItem('Stop run…')!.querySelectorAll('svg rect')).toHaveLength(1);

      // One click fires interrupt with no confirm step.
      interrupt!.click();
      await settle();
      expect(h.interruptPosts).toBe(1);
    },
  );

  it('offers Interrupt in the >=640px dropdown too', async () => {
    stubMatchMedia().set(MENU_DROPDOWN_QUERY, true);
    h.messagesOnServer = { ...h.messagesOnServer, state: 'working' };
    await mountChat();

    expect(
      header().querySelector('.chat-desktop-actions button[aria-label="Interrupt"]'),
    ).toBeNull();
    moreButton()!.click();
    await settle();
    menuItem('Interrupt')!.click();
    await settle();
    expect(h.interruptPosts).toBe(1);
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
  });

  it('offers no Interrupt anywhere for an ended run (live-gated)', async () => {
    h.runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    h.messagesOnServer = { ...h.messagesOnServer, state: 'ended' };
    await mountChat();

    moreButton()!.click();
    await settle();
    expect(menuItem('Interrupt')).toBeUndefined();
    // None anywhere: not live (no menu turn Interrupt) and the ended composer
    // is read-only (no escape hatch).
    expect(buttonByLabel('Interrupt')).toBeNull();
  });

  it('offers a menu Interrupt above Stop that fires and closes the menu', async () => {
    await mountChat(); // default fixture: live
    moreButton()!.click();
    await settle();

    const panel = container.querySelector('.chat-menu-panel')!;
    const interrupt = menuItem('Interrupt');
    const stop = menuItem('Stop run…');
    expect(interrupt).toBeDefined();
    expect(stop).toBeDefined();
    // Listed ABOVE the danger Stop item among the panel's buttons.
    const buttons = Array.from(panel.querySelectorAll('button'));
    expect(buttons.indexOf(interrupt!)).toBeLessThan(buttons.indexOf(stop!));

    // Clicking fires interrupt AND closes the menu (one tap, no confirm).
    interrupt!.click();
    await settle();
    expect(h.interruptPosts).toBe(1);
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
  });

  it('renders the header visible (no --hidden class) by default', async () => {
    await mountChat();
    const header = container.querySelector('.chat-header') as HTMLElement;
    expect(header).not.toBeNull();
    expect(header.classList.contains('chat-header--hidden')).toBe(false); // headerVisible starts true
  });

  it('renders model · effort on the secondary line with raw ids when the catalog has no match', async () => {
    await mountChat(); // default mocks: h.providersOnServer[0].models/efforts are both []

    // The secondary line under the title, at every width (no CSS gate): the
    // project, the forge link, then the model · effort text.
    const sub = container.querySelector('.chat-titlebar .chat-title-sub')!;
    expect(sub.querySelector('.chat-title-model')?.textContent).toBe('opus[1m] · max');
    // First class only: the router's <A> appends its own active/inactive class.
    const order = Array.from(sub.children).map((el) => el.classList[0]);
    expect(order).toEqual([
      'chat-title-project',
      'chat-title-forge',
      'chat-title-sep',
      'chat-title-model',
    ]);
    expect(sub.textContent).toBe('proj·opus[1m] · max');
    // No model chip anymore.
    expect(container.querySelector('.chat-model-chip')).toBeNull();
  });

  it('renders model · effort with catalog pretty labels when they match', async () => {
    h.providersOnServer[0]!.models = [{ value: 'opus[1m]', label: 'Opus 4.6 [1m]', efforts: [] }];
    h.providersOnServer[0]!.efforts = [{ value: 'max', label: 'Max' }];
    await mountChat();

    expect(container.querySelector('.chat-title-model')?.textContent).toBe('Opus 4.6 [1m] · Max');
  });

  it('hides the model text for a legacy run with no model', async () => {
    h.runOnServer = { ...baseRun(), model: '' };
    await mountChat();

    expect(container.querySelector('.chat-title-model')).toBeNull();
    expect(container.querySelector('.chat-title-sep')).toBeNull();
    // The project still rides the secondary line.
    expect(container.querySelector('.chat-title-sub .chat-title-project')?.textContent).toBe(
      'proj',
    );
  });

  it('renders the model alone, with no separator, when effort is empty', async () => {
    h.runOnServer = { ...baseRun(), effort: '' };
    await mountChat();

    expect(container.querySelector('.chat-title-model')?.textContent).toBe('opus[1m]');
  });

  it('shows the model text with no leading separator for a legacy no-~ session', async () => {
    h.runOnServer = { ...baseRun(), session_name: 'legacy-session' };
    await mountChat();

    expect(container.querySelector('.chat-title-sub')?.textContent).toBe('opus[1m] · max');
    expect(container.querySelector('.chat-title-sep')).toBeNull();
  });

  // The context meter (issue #243 / ADR-0061, issue #58 §1): a ring + rounded
  // percentage button of its own — not nested in the model text — tinting
  // amber at >=80% occupancy and red at >=95%.
  it('renders the context meter as a ring + percentage button, outside the model text', async () => {
    withUsage(127432, 200000);
    await mountChat();

    const meter = meterButton();
    expect(meter).not.toBeNull();
    expect(meter!.closest('header')).not.toBeNull();
    // Its own control, not nested in (or gated by) the model text.
    expect(meter!.closest('.chat-title-model')).toBeNull();
    expect(container.querySelector('.chat-title-model')?.textContent).toBe('opus[1m] · max');
    // Percentage text always present (never colour-only), plus the ring glyph
    // filled to the percentage.
    expect(meter!.textContent).toBe('64%');
    const fill = meter!.querySelector('svg.chat-context-ring .chat-context-ring-fill');
    expect(fill?.getAttribute('stroke-dasharray')).toBe('64 100');
    expect(meter!.getAttribute('aria-label')).toBe('Context 64% used — run details');
    // The tooltip humanizes the token counts (127432 → 127k).
    expect(meter!.getAttribute('title')).toBe('127k of 200k tokens');
    // Occupancy 0.64 is below the amber threshold → no tint band.
    expect(meter!.classList.contains('warn')).toBe(false);
    expect(meter!.classList.contains('danger')).toBe(false);
  });

  it.each<ConversationState>(['working', 'needs_input', 'question', 'idle'])(
    'shows the meter in every conversational state (state %s)',
    async (state) => {
      withUsage(100000, 200000);
      h.messagesOnServer = { ...h.messagesOnServer, state };
      await mountChat();

      expect(meterButton()?.textContent).toBe('50%');
    },
  );

  it('tints the meter amber at exactly 80% occupancy (the warn threshold)', async () => {
    withUsage(160000, 200000);
    await mountChat();

    const meter = meterButton();
    expect(meter?.textContent).toBe('80%');
    expect(meter?.classList.contains('warn')).toBe(true);
    expect(meter?.classList.contains('danger')).toBe(false);
  });

  it('keeps the meter untinted just below the amber threshold (0.80)', async () => {
    // Occupancy 0.799995 rounds to 80% but is below the 0.80 tint boundary, so
    // the band stays off — the tint keys on the ratio, not the shown percent.
    withUsage(159999, 200000);
    await mountChat();

    const meter = meterButton();
    expect(meter?.classList.contains('warn')).toBe(false);
    expect(meter?.classList.contains('danger')).toBe(false);
  });

  it('tints the meter red at exactly 95% occupancy (red wins over amber)', async () => {
    withUsage(190000, 200000);
    await mountChat();

    const meter = meterButton();
    expect(meter?.textContent).toBe('95%');
    expect(meter?.classList.contains('danger')).toBe(true);
    // Red wins: past 95% the amber band is not also applied.
    expect(meter?.classList.contains('warn')).toBe(false);
  });

  it('hides the meter without usage, keeping Run details reachable from the menu', async () => {
    await mountChat(); // default fixture: no context_usage

    expect(container.querySelector('.chat-title-model')?.textContent).toBe('opus[1m] · max');
    expect(container.querySelector('.chat-context-meter')).toBeNull();

    moreButton()!.click();
    await settle();
    menuItem('Run details')!.click();
    await settle();
    // Opening Run details closes the menu.
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
    expect(details()).not.toBeNull();
    expect(fact('Context')).toBe('Not reported yet');
    expect(fact('Branch')).toBe('lab/x');
  });

  it('keeps the meter for a legacy model-less run when usage is present', async () => {
    // No longer nested in the model chip: a codex run with an empty stored
    // model that carries usage still shows its meter.
    h.runOnServer = { ...baseRun(), model: '' };
    withUsage(100000, 200000);
    await mountChat();

    expect(container.querySelector('.chat-title-model')).toBeNull();
    expect(meterButton()?.textContent).toBe('50%');
  });

  it('hides the meter when the limit is 0 (no denominator to divide by)', async () => {
    withUsage(5000, 0);
    await mountChat();

    expect(container.querySelector('.chat-title-model')?.textContent).toBe('opus[1m] · max');
    expect(container.querySelector('.chat-context-meter')).toBeNull();
    moreButton()!.click();
    await settle();
    expect(menuItem('Run details')).toBeDefined();
  });

  it('renders no "N behind" chip at any count — Run details carries it', async () => {
    h.runOnServer = { ...baseRun(), commits_behind: 3 };
    await mountChat();

    expect(container.querySelector('.chat-behind-chip')).toBeNull();
    expect(header().textContent).not.toContain('behind');
  });

  // Run details (issue #58 §1): what the meter opens.
  it('opens Run details from the meter as a bottom sheet below 1024px with model, effort, tokens, branch, base and behind count', async () => {
    h.providersOnServer[0]!.models = [{ value: 'opus[1m]', label: 'Opus 4.6 [1m]', efforts: [] }];
    h.providersOnServer[0]!.efforts = [{ value: 'max', label: 'Max' }];
    h.runOnServer = { ...baseRun(), commits_behind: 3 };
    withUsage(127432, 200000);
    await mountChat();

    expect(details()).toBeNull();
    meterButton()!.click();
    await settle();

    const sheet = details();
    expect(sheet).not.toBeNull();
    // A modal sheet over a scrim, outside the header.
    expect(sheet!.classList.contains('chat-sheet')).toBe(true);
    expect(sheet!.closest('header')).toBeNull();
    expect(sheet!.getAttribute('role')).toBe('dialog');
    expect(sheet!.getAttribute('aria-modal')).toBe('true');
    expect(container.querySelector('.chat-sheet-scrim')).not.toBeNull();
    const headingId = sheet!.getAttribute('aria-labelledby')!;
    expect(document.getElementById(headingId)?.textContent).toBe('Run details');
    expect(meterButton()!.getAttribute('aria-expanded')).toBe('true');

    expect(fact('Model')).toBe('Opus 4.6 [1m]Max effort · set at spawn');
    expect(fact('Context')).toBe('64%127k of 200k tokens');
    expect(sheet!.querySelector('.chat-details-bar > span')?.getAttribute('style')).toContain(
      'width: 64%',
    );
    expect(fact('Branch')).toBe('lab/x');
    expect(fact('Base')).toBe('main3 behindPull base');
    expect(sheet!.querySelector('.chat-details-behind')?.getAttribute('title')).toBe(
      '3 commits behind the base branch',
    );
    // Focus moves into the surface.
    expect(document.activeElement?.getAttribute('aria-label')).toBe('Close run details');
  });

  it('opens Run details as a popover anchored under the meter at >=1024px', async () => {
    stubMatchMedia().set(MENU_DROPDOWN_QUERY, true);
    stubMatchMedia().set(DESKTOP_QUERY, true);
    withUsage(50000, 200000);
    await mountChat();

    meterButton()!.click();
    await settle();
    const popover = details();
    expect(popover?.classList.contains('chat-popover')).toBe(true);
    expect(popover!.closest('.chat-meter-anchor')).not.toBeNull();
    // Non-modal: no aria-modal, no dimming scrim — a transparent outside-tap
    // catcher instead.
    expect(popover!.getAttribute('aria-modal')).toBeNull();
    expect(container.querySelector('.chat-sheet-scrim')).toBeNull();
    (container.querySelector('.chat-menu-scrim') as HTMLElement).click();
    await settle();
    expect(details()).toBeNull();

    // Opened from the menu, it anchors under the ••• trigger instead.
    moreButton()!.click();
    await settle();
    menuItem('Run details')!.click();
    await settle();
    expect(details()?.closest('.chat-menu')).not.toBeNull();
  });

  it('offers Pull base only while a live run is behind', async () => {
    withUsage(50000, 200000);
    await mountChat(); // baseRun() carries no commits_behind → up to date

    meterButton()!.click();
    await settle();
    expect(fact('Base')).toBe('mainUp to date');
    expect(details()!.querySelector('.chat-details-pull')).toBeNull();
  });

  it('offers no Pull base on an ended run', async () => {
    h.runOnServer = {
      ...baseRun(),
      outcome: 'stopped',
      ended_at: '2026-07-06T16:00:00.000Z',
      commits_behind: 3,
    };
    h.messagesOnServer = { ...h.messagesOnServer, state: 'ended' };
    await mountChat();

    moreButton()!.click();
    await settle();
    menuItem('Run details')!.click();
    await settle();
    expect(fact('Base')).toBe('main');
    expect(details()!.querySelector('.chat-details-pull')).toBeNull();
  });

  it('Pull base sends /pull-base down the reply path and surfaces the returned notice', async () => {
    h.runOnServer = { ...baseRun(), commits_behind: 2 };
    h.replyStatus = 200;
    h.replyNotice = 'already up to date with origin/main';
    withUsage(50000, 200000);
    await mountChat();

    meterButton()!.click();
    await settle();
    const pull = details()!.querySelector<HTMLButtonElement>('.chat-details-pull')!;
    expect(pull.textContent).toBe('Pull base');
    pull.click();
    await settle();

    expect(h.replyPosts).toEqual([{ text: '/pull-base' }]);
    // The surface closes so the reply's notice banner is in view.
    expect(details()).toBeNull();
    const notice = container.querySelector('.banner.notice');
    expect(notice?.textContent).toContain('already up to date with origin/main');
    expect(container.querySelector('.banner.error')).toBeNull();
  });

  it('closes Run details on Escape (consumed), returning focus to the meter', async () => {
    withUsage(50000, 200000);
    await mountChat();
    meterButton()!.click();
    await settle();
    expect(details()).not.toBeNull();

    const esc = new KeyboardEvent('keydown', { key: 'Escape', cancelable: true });
    window.dispatchEvent(esc);
    await settle();
    expect(details()).toBeNull();
    // Consumed, so the tool panel's window Esc-close skips it (issue #145).
    expect(esc.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(meterButton());
  });

  it('closes Run details on the close button and on a scrim tap', async () => {
    withUsage(50000, 200000);
    await mountChat();

    meterButton()!.click();
    await settle();
    buttonByLabel('Close run details')!.click();
    await settle();
    expect(details()).toBeNull();

    meterButton()!.click();
    await settle();
    (container.querySelector('.chat-sheet-scrim') as HTMLElement).click();
    await settle();
    expect(details()).toBeNull();
    expect(container.querySelector('.chat-sheet-scrim')).toBeNull();
  });
});
