// ChatHeader behavioral contract (issue #7), header-area slice of the
// RunChat contract split (issue #194):
// - one-tap interrupt (POST /interrupt, no confirm) lives in the live-gated
//   header — inline on desktop and a `•••` menu item above Stop, a `pause`
//   glyph distinct from the two-step danger `square` Stop — plus the two
//   locked-state escape hatches;
// - the run's spawn-time model · effort rides a read-only chip beside the
//   state chip on desktop, and a non-interactive info row (role=none, not a
//   menuitem) pinned atop the `•••` panel on mobile — catalog pretty labels
//   with the raw id as fallback, hidden entirely for a legacy row with no
//   model (issue #68);

import { describe, expect, it } from 'vitest';
import {
  baseRepo,
  baseRun,
  container,
  h,
  installChatHooks,
  menuItem,
  mountChat,
  moreButton,
  settle,
  buttonByLabel,
} from './harness';

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

  it('opens an anchored dropdown with the model info row, open affordance, Interrupt and Stop run for a live run', async () => {
    await mountChat(); // default fixture: live, needs_input

    expect(container.querySelector('.chat-menu-panel')).toBeNull();
    moreButton()!.click();
    await settle();

    const panel = container.querySelector('.chat-menu-panel');
    expect(panel).not.toBeNull();
    expect(container.querySelector('.chat-menu-open')).not.toBeNull(); // the open affordance
    expect(menuItem('Interrupt')).toBeDefined(); // live turn Interrupt (ADR-0029)
    expect(menuItem('Stop run…')).toBeDefined();
    expect(menuItem('Show thinking')).toBeUndefined();
    expect(menuItem('New conversation')).toBeUndefined();

    // The spawn-time model info row (issue #68): a plain, non-focusable div —
    // NOT a menuitem — pinned as the panel's FIRST child, above every item.
    const info = panel!.firstElementChild as HTMLElement;
    expect(info.classList.contains('chat-menu-info')).toBe(true);
    expect(info.tagName).toBe('DIV');
    expect(info.getAttribute('role')).toBe('none');
    expect(info.textContent).toBe('opus[1m] · max');

    // The model string never leaks into an actual menu item.
    const menuItemTexts = Array.from(document.querySelectorAll('[role=menuitem]')).map(
      (el) => el.textContent,
    );
    expect(menuItemTexts.some((t) => t?.includes('opus[1m]'))).toBe(false);
  });

  it('omits Stop run from the dropdown when the run has ended', async () => {
    h.runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    h.messagesOnServer = { ...h.messagesOnServer, state: 'ended' };
    await mountChat();

    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).not.toBeNull();
    // The model info row shows regardless of liveness (it's spawn-time, not run state).
    expect(container.querySelector('.chat-menu-info')?.textContent).toBe('opus[1m] · max');
    // Both the turn Interrupt and Stop are live-gated — gone on an ended run.
    expect(menuItem('Interrupt')).toBeUndefined();
    expect(menuItem('Stop run…')).toBeUndefined();
  });

  it('closes the dropdown on Escape', async () => {
    await mountChat();
    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-panel')).not.toBeNull();

    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    expect(container.querySelector('.chat-menu-panel')).toBeNull();
  });

  it('renders a state dot carrying the convo class token for the live state', async () => {
    await mountChat(); // needs_input fixture
    // The mobile dot and the full chip both live in the DOM (CSS gates which
    // shows); the dot carries stateBadge('needs_input').cls, which the CSS maps
    // to the convo color (jsdom applies no stylesheet, so only the class is
    // assertable here — the color mapping is exercised by conversation.test.ts).
    expect(container.querySelector('.chat-state-dot.needs-input')).not.toBeNull();
  });

  it('omits the exposure badge for a run that has exposed nothing', async () => {
    await mountChat(); // baseRun() carries no exposed_secrets
    expect(container.querySelector('.chat-state-dot.exposed')).toBeNull();
    expect(container.querySelector('.chip.exposed')).toBeNull();
  });

  it('renders a singular exposure badge naming the one exposed secret', async () => {
    h.runOnServer = { ...baseRun(), exposed_secrets: ['API_KEY'] };
    await mountChat();
    expect(container.querySelector('.chat-state-dot.exposed')).not.toBeNull();
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

  it('renders a one-tap desktop header Interrupt that posts /interrupt, live-gated', async () => {
    await mountChat(); // default fixture: live (outcome active)

    const interrupt = container.querySelector<HTMLButtonElement>(
      '.chat-desktop-actions button[aria-label="Interrupt"]',
    );
    expect(interrupt).not.toBeNull();
    expect(interrupt!.classList.contains('chat-turn-interrupt')).toBe(true);
    expect(interrupt!.title).toBe('Interrupt the current turn (keeps the session)');
    // The `pause` glyph (two rects) reads distinct from the danger two-step
    // `square` Stop (one rect) that renders immediately after it.
    expect(interrupt!.querySelectorAll('svg rect')).toHaveLength(2);
    const stop = container.querySelector<HTMLButtonElement>('.chat-desktop-actions .chat-stop');
    expect(stop!.querySelectorAll('svg rect')).toHaveLength(1);

    // One click fires interrupt with no confirm step.
    interrupt!.click();
    await settle();
    expect(h.interruptPosts).toBe(1);
  });

  it('omits the header Interrupt for an ended run (live-gated)', async () => {
    h.runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    h.messagesOnServer = { ...h.messagesOnServer, state: 'ended' };
    await mountChat();

    expect(
      container.querySelector('.chat-desktop-actions button[aria-label="Interrupt"]'),
    ).toBeNull();
    // None anywhere: not live (no header/menu turn Interrupt) and the ended
    // composer is read-only (no escape hatch).
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
    expect(interrupt!.title).toBe('Interrupt the current turn (keeps the session)');
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

  it('renders the desktop model chip with raw ids when the catalog has no match', async () => {
    await mountChat(); // default mocks: h.providersOnServer[0].models/efforts are both []

    expect(container.querySelector('.chat-model-chip')?.textContent).toBe('opus[1m] · max');
  });

  it('renders the desktop model chip with catalog pretty labels when they match', async () => {
    h.providersOnServer[0]!.models = [{ value: 'opus[1m]', label: 'Opus 4.6 [1m]', efforts: [] }];
    h.providersOnServer[0]!.efforts = [{ value: 'max', label: 'Max' }];
    await mountChat();

    expect(container.querySelector('.chat-model-chip')?.textContent).toBe('Opus 4.6 [1m] · Max');
  });

  it('hides the model chip and the menu info row entirely for a legacy run with no model', async () => {
    h.runOnServer = { ...baseRun(), model: '' };
    await mountChat();

    expect(container.querySelector('.chat-model-chip')).toBeNull();
    moreButton()!.click();
    await settle();
    expect(container.querySelector('.chat-menu-info')).toBeNull();
  });

  it('renders the model alone, with no separator, when effort is empty', async () => {
    h.runOnServer = { ...baseRun(), effort: '' };
    await mountChat();

    expect(container.querySelector('.chat-model-chip')?.textContent).toBe('opus[1m]');
  });

  it('renders the "N behind" chip when commits_behind is positive', async () => {
    h.runOnServer = { ...baseRun(), commits_behind: 3 };
    await mountChat();

    const chip = container.querySelector('.chat-behind-chip');
    expect(chip?.textContent).toBe('3 behind');
    expect(chip?.getAttribute('title')).toBe('3 commits behind the base branch');
  });

  it('hides the "N behind" chip when commits_behind is 0', async () => {
    h.runOnServer = { ...baseRun(), commits_behind: 0 };
    await mountChat();

    expect(container.querySelector('.chat-behind-chip')).toBeNull();
  });

  it('hides the "N behind" chip when commits_behind is absent', async () => {
    await mountChat(); // baseRun() carries no commits_behind

    expect(container.querySelector('.chat-behind-chip')).toBeNull();
  });
});
