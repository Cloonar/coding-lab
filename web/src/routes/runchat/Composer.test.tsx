// Composer behavioral contract (issue #7), composer-area slice of the
// RunChat contract split (issue #194):
// - the composer replies (POST /reply) and clears; Cmd/Ctrl+Enter sends, bare
//   Enter does not; Send is ALWAYS present in the unlocked states and enabled
//   with text even while working (ADR-0029, issue #61), POSTing /reply
//   immediately — no morph, no queue copy, no working hint; Cmd/Ctrl+Enter
//   sends while working too;

import { describe, expect, it, vi } from 'vitest';
import {
  baseRun,
  buttonByLabel,
  buttonByText,
  container,
  emitMessagesChangedSettled,
  finePointer,
  h,
  installChatHooks,
  menuItem,
  mountChat,
  moreButton,
  settle,
  withAssistantText,
} from './harness';

installChatHooks();

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
    // The composer carries no Interrupt of its own now (the header holds the
    // one-tap turn Interrupt); scope so the live header button isn't counted.
    expect(container.querySelector('.chat-composer button[aria-label="Interrupt"]')).toBeNull();
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
});
