// Composer behavioral contract (issue #7), composer-area slice of the
// RunChat contract split (issue #194):
// - the composer replies (POST /reply) and clears; Cmd/Ctrl+Enter sends, bare
//   Enter does not; Send is ALWAYS present in the unlocked states and enabled
//   with text in every state without a pending dialog — working, idle,
//   needs_input, '' — (ADR-0029, issue #61, issue #58 §2), POSTing /reply
//   immediately — no morph, no queue copy, no working hint; Cmd/Ctrl+Enter
//   sends while working too;
// - the slash-command popover opens on a leading `/` (issue #51 decision 5,
//   tiered per issue #122) and from the `/` button left of the box (issue #58
//   §6), which shows only with a non-empty catalog while the box is empty or
//   already a slash command, lists the FULL catalog, focuses the box, and
//   whose picks behave exactly like the typed popover's;
// - the reply box is a per-run draft in localStorage (issue #92): restored on
//   open, isolated per run (also across a :id change without a remount),
//   deleted by a successful send / an emptied box / an ended run, and inert
//   when storage throws.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Composer } from './Composer';
import {
  baseRun,
  buttonByLabel,
  buttonByText,
  container,
  emitMessagesChangedSettled,
  finePointer,
  h,
  installChatHooks,
  jsonResponse,
  menuItem,
  mountChat,
  moreButton,
  settle,
  withAssistantText,
} from './harness';

installChatHooks();
// Drafts persist in localStorage (issue #92): without this a test that types
// and never sends would restore its text into the next test's run_1 composer.
afterEach(() => localStorage.clear());

describe('Composer', () => {
  it('replies through the composer and clears the input', async () => {
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'keep going';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    buttonByLabel('Send')!.click();
    await settle();

    expect(h.replyPosts).toHaveLength(1);
    expect(h.replyPosts[0]?.text).toBe('keep going');
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
  });

  it('shows a 200 reply notice as an informational banner, never the error banner (issue #149)', async () => {
    h.replyStatus = 200;
    h.replyNotice = 'already up to date with origin/main';
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ping';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    buttonByLabel('Send')!.click();
    await settle();

    const notice = container.querySelector('.banner.notice');
    expect(notice?.textContent).toContain('already up to date with origin/main');
    expect(notice?.getAttribute('role')).toBe('status');
    expect(container.querySelector('.banner.error')).toBeNull();
    // The composer still clears on a 200, same as a 204.
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
  });

  it('keeps a reply error on the error banner, never the notice banner', async () => {
    h.replyStatus = 409;
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ping';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    buttonByLabel('Send')!.click();
    await settle();

    expect(container.querySelector('.banner.error')?.textContent).toContain(
      'run is not accepting replies',
    );
    expect(container.querySelector('.banner.notice')).toBeNull();
  });

  it('bare Enter sends on fine-pointer; Shift+Enter stays a newline; Cmd/Ctrl+Enter always sends', async () => {
    finePointer(true);
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ship it';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    // Bare Enter sends on a fine-pointer (mouse/trackpad) setup and clears the box.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.replyPosts).toHaveLength(1);
    expect(h.replyPosts[0]?.text).toBe('ship it');
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');

    // Shift+Enter never sends — the browser-default newline is left alone
    // (the handler must not preventDefault it).
    input.value = 'more text';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    const shiftEnter = new KeyboardEvent('keydown', {
      key: 'Enter',
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    input.dispatchEvent(shiftEnter);
    await settle();
    expect(h.replyPosts).toHaveLength(1); // unchanged
    expect(shiftEnter.defaultPrevented).toBe(false);

    // Cmd/Ctrl+Enter still sends and clears.
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(h.replyPosts).toHaveLength(2);
    expect(h.replyPosts[1]?.text).toBe('more text');
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
  });

  it('bare Enter never sends without a fine pointer (no matchMedia, or a touch profile)', async () => {
    // Default jsdom: no window.matchMedia at all — reads as "not fine-pointer".
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'no matchMedia here';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.replyPosts).toHaveLength(0);

    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(h.replyPosts).toHaveLength(1);
    expect(h.replyPosts[0]?.text).toBe('no matchMedia here');
  });

  it('bare Enter never sends on a touch profile (matchMedia present but not fine-pointer)', async () => {
    finePointer(false);
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'tap city';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.replyPosts).toHaveLength(0);

    // Cmd/Ctrl+Enter sends regardless of pointer type.
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(h.replyPosts).toHaveLength(1);
    expect(h.replyPosts[0]?.text).toBe('tap city');
  });

  it('ignores Enter fired mid-IME-composition even on a fine-pointer setup', async () => {
    finePointer(true);
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'still composing';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    input.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'Enter',
        isComposing: true,
        bubbles: true,
        cancelable: true,
      }),
    );
    await settle();
    expect(h.replyPosts).toHaveLength(0);
  });

  it('does not send bare Enter on an empty box, and preventDefaults it (no stray newline)', async () => {
    finePointer(true);
    await mountChat(); // default needs_input, empty box
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    const evt = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    input.dispatchEvent(evt);
    await settle();
    expect(h.replyPosts).toHaveLength(0);
    expect(evt.defaultPrevented).toBe(true);
  });

  it('keeps Send available and sending while the agent is working', async () => {
    // ADR-0029 (issue #61): Send no longer morphs — it stays in the composer
    // through `working`, enabled once the box has text, and POSTs /reply
    // immediately (a genuinely mid-turn reply is queued by the agent's own TUI,
    // with no queue UI here). No working hint, no "tap to interrupt" copy.
    h.messagesOnServer = { ...h.messagesOnServer, state: 'working' };
    await mountChat();

    const row = container.querySelector('.chat-composer-row');
    expect(row).not.toBeNull();
    const send = row!.querySelector<HTMLButtonElement>('button[aria-label="Send"]');
    expect(send).not.toBeNull();
    // Disabled while the box is empty, even though the agent is working.
    expect(send!.disabled).toBe(true);
    // The composer's input row carries no Interrupt of its own: the one-tap
    // turn Interrupt rides the status line ABOVE it (issue #58 §2), and Send
    // keeps its slot and its job.
    expect(row!.querySelector('.chat-interrupt, .chat-status-interrupt')).toBeNull();
    expect(container.querySelector('.chat-composer .chat-status-interrupt')).not.toBeNull();
    // The deleted working hint / "tap to interrupt" / queue copy are all gone.
    expect(container.querySelector('.chat-composer-hint')).toBeNull();
    expect(container.textContent).not.toContain('tap to interrupt');
    expect(container.textContent).not.toContain('queued');

    // Typing enables Send; the textarea is editable throughout.
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    expect(input.disabled).toBe(false);
    input.value = 'mid-turn thought';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(send!.disabled).toBe(false);

    // Clicking POSTs /reply immediately with the typed text.
    send!.click();
    await settle();
    expect(h.replyPosts).toHaveLength(1);
    expect(h.replyPosts[0]?.text).toBe('mid-turn thought');
  });

  it('disables Send while the composer is empty', async () => {
    await mountChat(); // default needs_input, empty box
    const send = buttonByLabel('Send');
    expect(send).not.toBeNull();
    expect(send!.classList.contains('chat-send')).toBe(true); // accent-square hook
    expect(send!.disabled).toBe(true);

    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'x';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByLabel('Send')!.disabled).toBe(false);

    // Whitespace-only is still empty.
    input.value = '   ';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByLabel('Send')!.disabled).toBe(true);
  });

  it('preserves a compose-ahead draft across a working→idle state flip', async () => {
    h.messagesOnServer = { ...h.messagesOnServer, state: 'working' };
    await mountChat();

    // Type a draft while working — Send is already present and enabled (ADR-0029).
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'draft thought';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByLabel('Send')!.disabled).toBe(false);

    // The agent returns to needs_input: the draft survives the state flip and
    // sending it posts the retained text.
    h.messagesOnServer = { ...h.messagesOnServer, state: 'needs_input' };
    await emitMessagesChangedSettled();

    const preserved = container.querySelector('.chat-input') as HTMLTextAreaElement;
    expect(preserved.value).toBe('draft thought');
    const send = buttonByLabel('Send');
    expect(send).not.toBeNull();
    expect(send!.disabled).toBe(false);

    send!.click();
    await settle();
    expect(h.replyPosts).toHaveLength(1);
    expect(h.replyPosts[0]?.text).toBe('draft thought');
  });

  it('sends on Cmd/Ctrl+Enter even while the agent is working (ADR-0029)', async () => {
    h.messagesOnServer = { ...h.messagesOnServer, state: 'working' };
    await mountChat();

    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = 'ship it now';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    // The shortcut no longer gates on `working` — it sends in every unlocked state.
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, bubbles: true }),
    );
    await settle();
    expect(h.replyPosts).toHaveLength(1);
    expect(h.replyPosts[0]?.text).toBe('ship it now');

    // Bare Enter on a fine-pointer setup sends too, mid-turn (issue #70): the
    // always-send contract extends bare Enter the same way it already covers
    // Cmd/Ctrl+Enter.
    finePointer(true);
    input.value = 'another mid-turn thought';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.replyPosts).toHaveLength(2);
    expect(h.replyPosts[1]?.text).toBe('another mid-turn thought');
  });

  it('is read-only for an ended run', async () => {
    h.runOnServer = { ...baseRun(), outcome: 'stopped', ended_at: '2026-07-06T16:00:00.000Z' };
    h.messagesOnServer = { ...h.messagesOnServer, state: 'ended' };
    await mountChat();

    expect(container.querySelector('.chat-input')).toBeNull();
    expect(container.querySelector('.chat-composer-note')?.textContent).toContain('read-only');
  });

  it('mounts the jump pill hidden and reveals it (emphasized on needs_input) when scrolled up', async () => {
    withAssistantText('done'); // needs_input fixture
    await mountChat();

    const pillBtn = container.querySelector('.chat-jump') as HTMLButtonElement;
    expect(pillBtn).not.toBeNull(); // always mounted so it can fade OUT
    // At/near the bottom (jsdom metrics are 0) → hidden + inert.
    expect(pillBtn.classList.contains('hidden')).toBe(true);
    expect(pillBtn.getAttribute('aria-hidden')).toBe('true');
    expect(pillBtn.tabIndex).toBe(-1);

    // Fake a scrolled-up viewport and fire a user scroll → the pill reveals.
    const stream = container.querySelector('.chat-stream') as HTMLElement;
    Object.defineProperty(stream, 'scrollHeight', { value: 1000, configurable: true });
    Object.defineProperty(stream, 'clientHeight', { value: 100, configurable: true });
    stream.scrollTop = 0;
    stream.dispatchEvent(new Event('scroll'));
    await settle();

    expect(pillBtn.classList.contains('hidden')).toBe(false);
    // needs_input content below the fold → emphasized pill with the needs-you copy.
    expect(pillBtn.classList.contains('emphasis')).toBe(true);
    expect(pillBtn.getAttribute('aria-label')).toBe('Claude Code is waiting — jump to latest');
    expect(pillBtn.textContent).toContain('Claude Code needs you');

    // Tapping smooth-scrolls to the latest (jsdom's scrollTo is a no-op stub, so
    // spy it to prove onJump → jumpToLatest is wired).
    const scrollToSpy = vi.fn();
    stream.scrollTo = scrollToSpy as unknown as typeof stream.scrollTo;
    pillBtn.click();
    await settle();
    expect(scrollToSpy).toHaveBeenCalledWith(expect.objectContaining({ behavior: 'smooth' }));
  });

  it('shows the non-emphasized pill copy when the content below is not a needs-you signal', async () => {
    h.messagesOnServer = { ...h.messagesOnServer, state: 'working' }; // not needs_input
    await mountChat();

    const pillBtn = container.querySelector('.chat-jump') as HTMLButtonElement;
    const stream = container.querySelector('.chat-stream') as HTMLElement;
    Object.defineProperty(stream, 'scrollHeight', { value: 1000, configurable: true });
    Object.defineProperty(stream, 'clientHeight', { value: 100, configurable: true });
    stream.scrollTop = 0;
    stream.dispatchEvent(new Event('scroll'));
    await settle();

    expect(pillBtn.classList.contains('hidden')).toBe(false);
    expect(pillBtn.classList.contains('emphasis')).toBe(false);
    expect(pillBtn.getAttribute('aria-label')).toBe('Jump to latest');
    expect(pillBtn.textContent).toContain('Latest');
  });

  function setComposerText(value: string): void {
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.value = value;
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }

  function composerKey(key: string, init: KeyboardEventInit = {}): void {
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    input.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, ...init }));
  }

  function popRows(): HTMLButtonElement[] {
    return Array.from(container.querySelectorAll<HTMLButtonElement>('.chat-cmd-row'));
  }

  it('opens the command popover only for a leading slash and filters across name/desc/hint', async () => {
    await mountChat();

    // A mid-message slash never triggers it (prefix-only).
    setComposerText('deploy a/b please');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();

    // A leading slash lists the whole catalog with name, hint, description
    // and source badge.
    setComposerText('/');
    await settle();
    const rows = popRows();
    expect(rows).toHaveLength(3);
    expect(rows[0]?.querySelector('.chat-cmd-name')?.textContent).toBe('/clear');
    expect(rows[1]?.querySelector('.chat-cmd-hint')?.textContent).toBe('instructions');
    expect(rows[2]?.querySelector('.chat-cmd-desc')?.textContent).toBe('Ship it');
    expect(rows[2]?.querySelector('.chat-cmd-source')?.textContent).toBe('project');
    // The description clamps to one line in CSS; the row's tooltip carries it in full.
    expect(rows[2]?.title).toBe('Ship it');

    // Filtering matches the name…
    setComposerText('/cle');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/clear',
    ]);

    // …the description, and the arg hint.
    setComposerText('/ship');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/deploy',
    ]);
    setComposerText('/env');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/deploy',
    ]);

    // No match → closed.
    setComposerText('/nosuch');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('cycles with arrows, completes with Tab (inserting "/name "), closes on Escape', async () => {
    await mountChat();
    setComposerText('/');
    await settle();

    // Down moves the active row; the listbox exposes it via aria-selected.
    composerKey('ArrowDown');
    await settle();
    expect(popRows()[1]?.getAttribute('aria-selected')).toBe('true');

    // Tab completes the active command — no reply is sent (issue #122: Tab is
    // the ONLY completion gesture; Enter never accepts the highlight).
    composerKey('Tab');
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/compact ');
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
    expect(h.replyPosts).toHaveLength(0);

    // Typing revives the popover; Escape dismisses it until the next keystroke.
    setComposerText('/cl');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).not.toBeNull();
    composerKey('Escape');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();

    // Tab also completes a single filtered match.
    setComposerText('/dep');
    await settle();
    composerKey('Tab');
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/deploy ');
  });

  it('ranks name matches above description matches (issue #122 tiered ranking)', async () => {
    // The original bug: 'setup-matt-pocock-skills' sits before 'triage' in
    // catalog order and its DESCRIPTION mentions triage, so the flat filter
    // listed it first and the highlight (index 0) picked the wrong skill.
    // Name tiers (exact, prefix, substring) now beat the description/arg-hint
    // tier at every query length; discovery via description still works, it
    // just never outranks a name match.
    h.commandsOnServer = [
      {
        name: 'setup-matt-pocock-skills',
        description: 'Vendor skills for planning and triage',
        arg_hint: '',
        source: 'project',
        chat_safe: true,
      },
      {
        name: 'triage',
        description: 'Triage issues',
        arg_hint: '',
        source: 'project',
        chat_safe: true,
      },
    ];
    await mountChat();

    // Exact-name tier wins over the earlier catalog entry's description match.
    setComposerText('/triage');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/triage',
      '/setup-matt-pocock-skills',
    ]);
    expect(popRows()[0]?.getAttribute('aria-selected')).toBe('true');

    // Name-prefix tier wins the same way mid-typing.
    setComposerText('/tri');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/triage',
      '/setup-matt-pocock-skills',
    ]);
  });

  it('sends the raw input on Enter while the popover is open — never the highlight (issue #122)', async () => {
    // Reverses the issue #70 popover-precedence rule (ADR-0041): Enter no
    // longer accepts the highlighted row; it falls through to the ordinary
    // fine-pointer send gate and posts the box exactly as typed — partial
    // text included (Tab first to complete is the user's responsibility).
    finePointer(true);
    await mountChat();
    setComposerText('/cle');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).not.toBeNull();

    composerKey('Enter');
    await settle();
    expect(h.replyPosts).toEqual([{ text: '/cle' }]);
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('sends a Tab-completed command as typed on Enter (fine-pointer)', async () => {
    finePointer(true);
    await mountChat();
    setComposerText('/cle');
    await settle();
    composerKey('Tab');
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/clear ');

    composerKey('Enter');
    await settle();
    expect(h.replyPosts).toEqual([{ text: '/clear' }]); // trimmed by the send path
  });

  it('bare Enter with the popover open neither sends nor completes without a fine pointer', async () => {
    // jsdom has no matchMedia → not fine-pointer, so bare Enter stays the
    // browser-default newline; with Enter no longer captured by the popover
    // there is nothing else it may do.
    await mountChat();
    setComposerText('/cle');
    await settle();

    composerKey('Enter');
    await settle();
    expect(h.replyPosts).toHaveLength(0);
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/cle');
  });

  it('Cmd/Ctrl+Enter still sends the raw text over an open popover', async () => {
    await mountChat();
    setComposerText('/cle');
    await settle();

    composerKey('Enter', { ctrlKey: true });
    await settle();
    expect(h.replyPosts).toEqual([{ text: '/cle' }]);
  });

  it('sends a no-argument command immediately on click (issue #122)', async () => {
    await mountChat();
    setComposerText('/cle');
    await settle();

    // /clear declares no arg_hint → the click IS the send: the ordinary reply
    // POST fires, the box clears, and the popover goes with it.
    popRows()[0]!.click();
    await settle();
    expect(h.replyPosts).toEqual([{ text: '/clear' }]);
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('');
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('completes a hinted command on click instead of sending (issue #122)', async () => {
    await mountChat();
    setComposerText('/comp');
    await settle();

    // /compact declares arg_hint 'instructions' → the click completes to
    // "/name " and waits for the argument; nothing is posted.
    popRows()[0]!.click();
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/compact ');
    expect(h.replyPosts).toHaveLength(0);
  });

  it('has no New conversation button or menu item; the clear command still autocompletes', async () => {
    await mountChat(); // default: active run, catalog has a role=clear command

    expect(buttonByText('New conversation')).toBeNull();
    moreButton()!.click();
    await settle();
    expect(menuItem('New conversation')).toBeUndefined();
    expect(buttonByText('Confirm clear')).toBeNull();

    // Composer autocomplete for /clear is untouched — only the dedicated
    // button + two-step confirm are gone.
    setComposerText('/');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toContain(
      '/clear',
    );
  });

  it.each(['working', 'idle', 'needs_input', ''] as const)(
    'enables Send whenever the box is non-empty without a pending dialog (state %j)',
    async (state) => {
      h.messagesOnServer = { ...h.messagesOnServer, state };
      await mountChat();
      const send = () =>
        container.querySelector<HTMLButtonElement>('.chat-composer-row button[aria-label="Send"]')!;
      expect(send().disabled).toBe(true);
      setComposerText('a reply');
      await settle();
      expect(send().disabled).toBe(false);
      send().click();
      await settle();
      expect(h.replyPosts).toEqual([{ text: 'a reply' }]);
    },
  );

  const slashButton = () => container.querySelector<HTMLButtonElement>('.chat-slash');

  it('shows the / button left of an empty box, and hides it once prose is typed', async () => {
    await mountChat();
    const btn = slashButton();
    expect(btn).not.toBeNull();
    expect(btn!.getAttribute('aria-label')).toBe('Slash commands');
    expect(btn!.getAttribute('aria-expanded')).toBe('false');
    // Left of the text box, inside the merged field.
    expect(btn!.nextElementSibling?.classList.contains('chat-input')).toBe(true);
    expect(container.querySelector('.chat-composer-row')!.classList.contains('has-slash')).toBe(
      true,
    );

    setComposerText('hello');
    await settle();
    expect(slashButton()).toBeNull();
    expect(container.querySelector('.chat-composer-row')!.classList.contains('has-slash')).toBe(
      false,
    );

    // A box that already starts with "/" keeps it.
    setComposerText('/cle');
    await settle();
    expect(slashButton()).not.toBeNull();
  });

  it('hides the / button when the catalog is empty', async () => {
    h.commandsOnServer = [];
    await mountChat();
    expect(slashButton()).toBeNull();
  });

  it('opens the FULL catalog from the / button, focuses the box, and a pick sends a no-argument command', async () => {
    await mountChat();
    const input = container.querySelector('.chat-input') as HTMLTextAreaElement;
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();

    slashButton()!.click();
    await settle();
    expect(document.activeElement).toBe(input);
    expect(slashButton()!.getAttribute('aria-expanded')).toBe('true');
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/clear',
      '/compact',
      '/deploy',
    ]);
    expect(input.value).toBe(''); // the box is untouched

    // Exactly a click in the typed popover: /clear has no argument → sent.
    popRows()[0]!.click();
    await settle();
    expect(h.replyPosts).toEqual([{ text: '/clear' }]);
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('a / button pick with an argument hint completes "/name " instead of sending', async () => {
    await mountChat();
    slashButton()!.click();
    await settle();

    popRows()[2]!.click(); // /deploy — arg hint "env"
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/deploy ');
    expect(h.replyPosts).toHaveLength(0);
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('the / button lists the full catalog even over a typed filter, and toggles it closed', async () => {
    await mountChat();
    setComposerText('/cle');
    await settle();
    expect(popRows()).toHaveLength(1); // the typed filter

    slashButton()!.click();
    await settle();
    expect(popRows()).toHaveLength(3); // the whole catalog

    slashButton()!.click();
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();

    // The next keystroke goes back to the typed filter.
    setComposerText('/dep');
    await settle();
    expect(popRows().map((r) => r.querySelector('.chat-cmd-name')?.textContent)).toEqual([
      '/deploy',
    ]);
  });

  it('closes the / button list on Escape, keeping keyboard cycling over the full catalog', async () => {
    await mountChat();
    slashButton()!.click();
    await settle();
    composerKey('ArrowDown');
    await settle();
    expect(popRows()[1]?.getAttribute('aria-selected')).toBe('true');
    composerKey('Tab');
    await settle();
    expect((container.querySelector('.chat-input') as HTMLTextAreaElement).value).toBe('/compact ');

    setComposerText('');
    await settle();
    slashButton()!.click();
    await settle();
    composerKey('Escape');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });
});

// The per-run draft (issue #92). Rendered straight (no router, no stream) so a
// test can remount a run's composer and drive `runID` as a signal — the router
// reuses the Composer when only `:id` changes.
describe('Composer draft persistence (issue #92)', () => {
  const draftKey = (id: string) => `lab.draft.chat.${id}`;
  const stored = (id: string): string | null => localStorage.getItem(draftKey(id));
  const storedValue = (id: string): unknown => {
    const raw = stored(id);
    return raw === null ? null : (JSON.parse(raw) as { v: unknown }).v;
  };

  let host: HTMLDivElement | undefined;
  let disposeHost: (() => void) | undefined;
  const unmount = () => {
    disposeHost?.();
    disposeHost = undefined;
    host?.remove();
    host = undefined;
  };
  afterEach(() => {
    unmount();
    vi.restoreAllMocks();
  });

  // Reply POSTs by run id; `replyStatus` is the knob for a failing send.
  let replyStatus = 204;
  const replied: { url: string; text: string }[] = [];
  function stubReply(): void {
    replyStatus = 204;
    replied.length = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown, init?: RequestInit) => {
        replied.push({
          url: String(input),
          text: (JSON.parse(String(init?.body)) as { text: string }).text,
        });
        if (replyStatus >= 400) {
          return Promise.resolve(
            jsonResponse(replyStatus, { error: 'run is not accepting replies' }),
          );
        }
        return Promise.resolve(jsonResponse(replyStatus, ''));
      }),
    );
  }

  // Mounts a Composer for `runID` and returns the setter that re-points it.
  function mountComposer(runID: string, extra: { ended?: boolean } = {}) {
    stubReply();
    host = document.createElement('div');
    document.body.appendChild(host);
    const [id, setID] = createSignal(runID);
    const [ended, setEnded] = createSignal(extra.ended ?? false);
    disposeHost = render(
      () => (
        <Composer
          runID={id()}
          state="idle"
          stateDetail=""
          ended={ended()}
          transcript="locating"
          dialog={null}
          messages={[]}
          commitsBehind={0}
          commands={[
            { name: 'clear', description: '', arg_hint: '', source: 'builtin', chat_safe: true },
          ]}
          agentName="Claude Code"
          openHint="open the session"
          jumpVisible={false}
          jumpEmphasis={false}
          onJump={() => {}}
          onError={() => {}}
          onNotice={() => {}}
          onSent={() => {}}
          onPulled={() => {}}
        />
      ),
      host,
    );
    return { setID, setEnded };
  }

  const box = () => host!.querySelector('.chat-input') as HTMLTextAreaElement;
  function typeInto(value: string): void {
    box().value = value;
    box().dispatchEvent(new Event('input', { bubbles: true }));
  }
  const send = () => host!.querySelector<HTMLButtonElement>('button[aria-label="Send"]')!;

  it("restores the same run's draft into the box on a fresh mount", async () => {
    mountComposer('run_a');
    typeInto('half a thought\nsecond line');
    await settle();
    expect(storedValue('run_a')).toBe('half a thought\nsecond line');

    unmount();
    mountComposer('run_a');
    await settle();
    expect(box().value).toBe('half a thought\nsecond line');
    expect(send().disabled).toBe(false);
  });

  it('keeps drafts per run: another run starts empty, and a :id change swaps the box without remounting', async () => {
    mountComposer('run_a');
    typeInto('only for A');
    await settle();

    // Run B has no draft: its box is empty even though A holds one.
    unmount();
    mountComposer('run_b');
    await settle();
    expect(box().value).toBe('');
    unmount();

    // Same component instance, runID A → B → A (the router reuses it on :id).
    const swap = mountComposer('run_a');
    await settle();
    const el = box();
    expect(el.value).toBe('only for A');

    swap.setID('run_b');
    await settle();
    expect(box()).toBe(el); // not remounted
    expect(box().value).toBe('');
    expect(stored('run_b')).toBeNull(); // A's text never lands under B's key
    expect(storedValue('run_a')).toBe('only for A'); // and A's draft survives

    typeInto('only for B');
    await settle();
    expect(storedValue('run_b')).toBe('only for B');
    expect(storedValue('run_a')).toBe('only for A');

    swap.setID('run_a');
    await settle();
    expect(box().value).toBe('only for A');
    expect(storedValue('run_b')).toBe('only for B');
  });

  it('deletes the entry on a successful send, via the button and via a popover click', async () => {
    mountComposer('run_a');
    typeInto('ship it');
    await settle();
    expect(stored('run_a')).not.toBeNull();

    send().click();
    await settle();
    expect(replied.map((r) => r.text)).toEqual(['ship it']);
    expect(box().value).toBe('');
    expect(stored('run_a')).toBeNull();

    // The popover's click-to-send path goes through the same send().
    typeInto('/cle');
    await settle();
    expect(stored('run_a')).not.toBeNull();
    host!.querySelector<HTMLButtonElement>('.chat-cmd-row')!.click();
    await settle();
    expect(replied.map((r) => r.text)).toEqual(['ship it', '/clear']);
    expect(stored('run_a')).toBeNull();
  });

  it('keeps the entry (and the box) when the send fails', async () => {
    mountComposer('run_a');
    typeInto('please retry');
    await settle();
    replyStatus = 500;

    send().click();
    await settle();
    expect(replied).toHaveLength(1);
    expect(box().value).toBe('please retry');
    expect(storedValue('run_a')).toBe('please retry');
  });

  it('deletes the entry when the box is emptied or left whitespace-only', async () => {
    mountComposer('run_a');
    typeInto('something');
    await settle();
    expect(stored('run_a')).not.toBeNull();

    typeInto('');
    await settle();
    expect(stored('run_a')).toBeNull();

    typeInto('again');
    await settle();
    typeInto('   ');
    await settle();
    expect(stored('run_a')).toBeNull();
  });

  it('saves a Tab completion', async () => {
    mountComposer('run_a');
    typeInto('/cle');
    await settle();
    box().dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true }));
    await settle();
    expect(box().value).toBe('/clear ');
    expect(storedValue('run_a')).toBe('/clear ');
  });

  it('deletes the entry once the run has ended', async () => {
    const m = mountComposer('run_a');
    typeInto('too late');
    await settle();
    expect(stored('run_a')).not.toBeNull();

    m.setEnded(true);
    await settle();
    expect(host!.querySelector('.chat-input')).toBeNull();
    expect(stored('run_a')).toBeNull();
  });

  it('works exactly as before when localStorage throws on every access', async () => {
    const boom = () => {
      throw new Error('storage disabled');
    };
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(boom);
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(boom);
    vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(boom);
    vi.spyOn(Storage.prototype, 'key').mockImplementation(boom);
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});

    const m = mountComposer('run_a');
    typeInto('no persistence');
    await settle();
    expect(box().value).toBe('no persistence');
    expect(send().disabled).toBe(false);

    send().click();
    await settle();
    expect(replied.map((r) => r.text)).toEqual(['no persistence']);
    expect(box().value).toBe('');

    m.setID('run_b'); // a swap reads a throwing storage as "no draft"
    await settle();
    expect(box().value).toBe('');
    expect(errors).not.toHaveBeenCalled();
  });
});
