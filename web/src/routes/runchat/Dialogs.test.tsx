// Dialog behavioral contract (issue #7), dialog-area slice of the
// RunChat contract split (issue #194):
// - a pending dialog renders as an interactive card INSIDE the chat stream
//   (issue #56) — deduped by tool_id against a transcript dialog message, never
//   twice — with native option buttons that POST /answer with the option index;
//   the composer collapses to a waiting note + Interrupt (no textarea) until it
//   resolves; the messages-scan fallback is gated on state==='question' and
//   selections reset when the dialog identity (tool_id) changes; a newly
//   arriving card scrolls its top into view only while following the tail;
// - an ANSWERED dialog message (outcome present — issue #56 decision 3)
//   renders as a compact inert Q→A summary: no buttons, no interactive card,
//   no raw tool chip; outcome PRESENCE alone is the answered signal;
// - dialog options are house-style cards (issue #56 decision 7): full-width,
//   descriptions always visible — the flat multi-select path is toggle-card
//   buttons (aria-pressed) instead of checkboxes, same Submit payload;

import { describe, expect, it, vi } from 'vitest';
import type { ChatMessage, Dialog } from '../../api';
import {
  baseRun,
  buttonByLabel,
  buttonByText,
  container,
  emitMessagesChangedSettled,
  h,
  installChatHooks,
  mountChat,
  settle,
} from './harness';

installChatHooks();

describe('Dialogs', () => {
  it('locks the composer and answers a pending dialog by option index', async () => {
    h.messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'dialog',
          dialog: {
            tool_id: 'toolu_1',
            dialog_kind: 'question',
            prompt: 'Which fix?',
            answerable: true,
            options: [
              { label: 'Revert' },
              { label: 'Patch forward' },
              { label: 'Other', is_other: true },
            ],
          },
        },
      ],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    // The free-text reply composer collapses while the dialog is pending.
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    // The interactive card renders in the STREAM at the dialog message's
    // position (issue #56) — the message does not double as an inert prompt.
    const card = container.querySelector('.chat-stream .chat-dialog-card');
    expect(card).not.toBeNull();
    expect(card?.textContent).toContain('Which fix?');
    expect(container.querySelector('.chat-dialog-inline')).toBeNull();

    buttonByText('Patch forward')!.click();
    await settle();
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toMatchObject({ tool_id: 'toolu_1', index: 1 });
  });

  it('renders and answers a pending dialog from the pending_dialog field (spool)', async () => {
    // The transcript carries no dialog message (Claude Code never flushes a
    // pending tool_use); the dialog arrives only via the top-level field.
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_field',
        dialog_kind: 'question',
        prompt: 'Pick a flavor?',
        answerable: true,
        options: [{ label: 'Option A' }, { label: 'Option B' }, { label: 'Other', is_other: true }],
      },
    };
    await mountChat();

    // Composer is locked; the card renders the field's prompt + options,
    // APPENDED as the last stream item (nothing in the transcript anchors it).
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    const card = container.querySelector('.chat-stream .chat-dialog-card');
    expect(card).not.toBeNull();
    expect(card?.textContent).toContain('Pick a flavor?');
    expect(container.querySelector('.chat-stream')!.lastElementChild).toBe(card);

    buttonByText('Option B')!.click();
    await settle();
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toMatchObject({ tool_id: 'toolu_field', index: 1 });
  });

  const singleSelectDialog = (toolID = 'toolu_card') => ({
    tool_id: toolID,
    dialog_kind: 'question' as const,
    prompt: 'Pick a flavor?',
    answerable: true,
    options: [{ label: 'Option A' }, { label: 'Option B' }],
  });

  it('collapses the composer to a bare waiting note, with no composer Interrupt, while the card is pending', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectDialog(),
    };
    await mountChat();

    // The interactive single-select renders inside the scrollable stream.
    const card = container.querySelector('.chat-stream .chat-dialog-card');
    expect(card).not.toBeNull();
    expect(card?.querySelector('.chat-dialog-prompt')?.textContent).toBe('Pick a flavor?');

    // The composer: one-line waiting note pointing up at the card — no
    // textarea, no Send (decision 2).
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    expect(container.querySelector('.chat-input')).toBeNull();
    expect(buttonByLabel('Send')).toBeNull();
    expect(container.querySelector('.chat-composer .chat-composer-note')?.textContent).toBe(
      'Claude Code is waiting on your answer — see the question above.',
    );
    // No composer Interrupt in this branch anymore (issue #165 item 3): an
    // accent square in Send's slot, right next to the live interactive card
    // above, drew muscle-memory "send" taps that declined the focused picker.
    // Neither the composer nor the card carries a `.chat-interrupt`.
    expect(container.querySelectorAll('.chat-interrupt')).toHaveLength(0);
    // The escape hatch survives elsewhere: the sticky header's turn Interrupt
    // (class `chat-turn-interrupt`) is gated on `live()`, which is true while
    // a dialog pends on this live run.
    expect(container.querySelector('.chat-turn-interrupt')).not.toBeNull();
  });

  it('renders the dialog exactly once when a stream message and pending_dialog share a tool_id', async () => {
    // The transcript flushed a dialog message AND the spool serves the same
    // tool_id: ONE interactive card, at the stream message's position, fed by
    // the richer field data.
    h.messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'dialog',
          dialog: {
            tool_id: 'toolu_dup',
            dialog_kind: 'question',
            prompt: 'Which fix?',
            answerable: true,
            options: [{ label: 'Revert' }, { label: 'Patch forward' }],
          },
        },
        { seq: 2, kind: 'text', role: 'assistant', text: 'context after' },
      ],
      state: 'question',
      cursor: 2,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_dup',
        dialog_kind: 'question',
        prompt: 'Which fix?',
        answerable: true,
        options: [
          { label: 'Revert', description: 'Roll back the change' },
          { label: 'Patch forward' },
        ],
      },
    };
    await mountChat();

    // Exactly one card, the interactive options render once, and no inert
    // duplicate of the same prompt remains.
    expect(container.querySelectorAll('.chat-dialog-card')).toHaveLength(1);
    expect(
      Array.from(container.querySelectorAll('button.dialog-option')).filter(
        (b) => b.querySelector('.dialog-option-label')?.textContent === 'Revert',
      ),
    ).toHaveLength(1);
    expect(container.querySelector('.chat-dialog-inline')).toBeNull();

    // At the stream MESSAGE's position — the later text message follows it —
    // and carrying the spool field's data (the transcript copy lacks the
    // description).
    const card = container.querySelector('.chat-stream .chat-dialog-card')!;
    expect(card.nextElementSibling?.textContent).toContain('context after');
    expect(card.querySelector('.dialog-option-desc')?.textContent).toBe('Roll back the change');

    buttonByText('Patch forward')!.click();
    await settle();
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toMatchObject({ tool_id: 'toolu_dup', index: 1 });
  });

  it('removes the card and returns the textarea once the dialog resolves', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectDialog(),
    };
    await mountChat();
    expect(container.querySelector('.chat-dialog-card')).not.toBeNull();
    expect(container.querySelector('.chat-input')).toBeNull();

    // The dialog resolves (answered on another surface) and the agent works on.
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'working',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: null,
    };
    await emitMessagesChangedSettled();

    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    expect(container.querySelector('.chat-composer-row')).not.toBeNull();
    expect(container.querySelector('.chat-input')).not.toBeNull();
  });

  const singleSelectOtherDialog = (toolID = 'toolu_solo_other') => ({
    tool_id: toolID,
    dialog_kind: 'question' as const,
    prompt: 'Which fix?',
    answerable: true,
    options: [{ label: 'Revert' }, { label: 'Other', is_other: true }],
  });

  it('single-select Other: Enter submits exactly like clicking Send', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectOtherDialog(),
    };
    await mountChat();

    const other = container.querySelector('.dialog-other input') as HTMLInputElement;
    other.value = 'roll it back manually';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Send')!.disabled).toBe(false);

    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_solo_other',
      index: 1,
      other_text: 'roll it back manually',
    });
  });

  it('single-select Other: Enter no-ops on empty or whitespace-only text, matching disabled Send', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectOtherDialog(),
    };
    await mountChat();

    const other = container.querySelector('.dialog-other input') as HTMLInputElement;
    expect(buttonByText('Send')!.disabled).toBe(true);
    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.answerPosts).toHaveLength(0);

    other.value = '   ';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Send')!.disabled).toBe(true);
    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.answerPosts).toHaveLength(0);
  });

  it('single-select Other: ignores Enter fired mid-IME-composition even with valid text', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'hmm' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: singleSelectOtherDialog(),
    };
    await mountChat();

    const other = container.querySelector('.dialog-other input') as HTMLInputElement;
    other.value = 'still composing';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Send')!.disabled).toBe(false);

    other.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'Enter',
        isComposing: true,
        bubbles: true,
        cancelable: true,
      }),
    );
    await settle();
    expect(h.answerPosts).toHaveLength(0);
  });

  function stubScrollIntoView(): {
    calls: { target: Element; arg: unknown }[];
    restore: () => void;
  } {
    const calls: { target: Element; arg: unknown }[] = [];
    const proto = Element.prototype as unknown as { scrollIntoView?: (arg?: unknown) => void };
    proto.scrollIntoView = vi.fn(function (this: Element, arg?: unknown) {
      calls.push({ target: this, arg });
    });
    return {
      calls,
      restore: () => {
        delete proto.scrollIntoView;
      },
    };
  }

  it('scrolls the arriving dialog card top into view while following the tail', async () => {
    const scrolls = stubScrollIntoView();
    try {
      await mountChat(); // no dialog yet; jsdom geometry reads as at-bottom
      expect(scrolls.calls).toHaveLength(0);

      // A refetch brings a NEW pending dialog: follow is on, so the CARD's
      // top comes into view — not the stream bottom.
      h.messagesOnServer = {
        ...h.messagesOnServer,
        state: 'question',
        pending_dialog: singleSelectDialog('toolu_scroll'),
      };
      await emitMessagesChangedSettled();

      const card = container.querySelector('.chat-dialog-card');
      expect(card).not.toBeNull();
      expect(scrolls.calls).toHaveLength(1);
      expect(scrolls.calls[0]?.target).toBe(card);
      expect(scrolls.calls[0]?.arg).toEqual({ block: 'start' });

      // The SAME dialog on the next tick is not new — no re-yank.
      await emitMessagesChangedSettled();
      expect(scrolls.calls).toHaveLength(1);
    } finally {
      scrolls.restore();
    }
  });

  it('leaves the viewport alone when a dialog arrives while scrolled up', async () => {
    const scrolls = stubScrollIntoView();
    try {
      await mountChat(); // no dialog yet
      // Fake a scrolled-up viewport (the jump-pill test's geometry recipe):
      // far from the bottom, so follow is off.
      const stream = container.querySelector('.chat-stream') as HTMLElement;
      Object.defineProperty(stream, 'scrollHeight', { value: 1000, configurable: true });
      Object.defineProperty(stream, 'clientHeight', { value: 100, configurable: true });
      stream.scrollTop = 0;

      h.messagesOnServer = {
        ...h.messagesOnServer,
        state: 'question',
        pending_dialog: singleSelectDialog('toolu_up'),
      };
      await emitMessagesChangedSettled();

      // The card rendered, but the position was preserved — the jump pill is
      // the scrolled-up reader's affordance (untouched by issue #56).
      expect(container.querySelector('.chat-dialog-card')).not.toBeNull();
      expect(scrolls.calls).toHaveLength(0);
      expect(stream.scrollTop).toBe(0); // no bottom-follow either
    } finally {
      scrolls.restore();
    }
  });

  it('surfaces a needs-input status line in the stream and keeps the composer usable', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'done' }],
      state: 'needs_input',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: null,
    };
    await mountChat();

    // needs_input keeps the composer usable (just the input row) and surfaces
    // the waiting status as the last line of the stream, not a composer note (§3).
    expect(container.querySelector('.chat-input')).not.toBeNull();
    expect(container.textContent).toContain('Claude Code is waiting for your reply.');
    expect(container.querySelector('.chat-composer-note')).toBeNull(); // moved into the stream
  });

  it('offers a one-tap Interrupt escape hatch in the locked question state', async () => {
    // state 'question' with no structured dialog: the composer is locked, but a
    // one-tap Interrupt escape hatch remains (decision 5) — no confirm step.
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.chat-composer-row')).toBeNull(); // locked
    // Scope to the composer: on this live run the header also renders a turn
    // Interrupt (class `chat-turn-interrupt`), which buttonByLabel would hit
    // first in DOM order.
    const hatch = container.querySelector<HTMLButtonElement>('.chat-composer .chat-interrupt');
    expect(hatch).not.toBeNull();
    // The hatch wears the `pause` glyph now (two rects) — the morph's pulse cue
    // is gone; two rects also read distinct from the danger `square` Stop's one.
    expect(hatch!.querySelectorAll('svg rect')).toHaveLength(2);
    hatch!.click();
    await settle();
    expect(h.interruptPosts).toBe(1); // one tap, no confirm gate
  });

  it('keeps the composer unlocked when a dialog message exists but state is not question', async () => {
    h.messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'dialog',
          dialog: {
            tool_id: 'toolu_1',
            dialog_kind: 'question',
            prompt: 'Which fix?',
            answerable: true,
            options: [{ label: 'Revert' }],
          },
        },
        { seq: 2, kind: 'text', role: 'assistant', text: 'answered elsewhere' },
      ],
      state: 'needs_input', // answered externally — the tailer moved on
      cursor: 2,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.chat-dialog')).toBeNull();
    expect(container.querySelector('.chat-composer-row')).not.toBeNull();
  });

  it('resets dialog selections when the pending dialog identity changes', async () => {
    const dialogMessage = (seq: number, toolID: string, prompt: string): ChatMessage => ({
      seq,
      kind: 'dialog',
      dialog: {
        tool_id: toolID,
        dialog_kind: 'question',
        prompt,
        answerable: true,
        multi: true,
        options: [{ label: 'One' }, { label: 'Two' }],
      },
    });
    h.messagesOnServer = {
      messages: [dialogMessage(1, 'toolu_a', 'First question?')],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    // Flat multi-select options are toggle cards (issue #56 decision 7).
    buttonByText('One')!.click();
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    // The pending dialog changes identity while the panel stays mounted.
    h.messagesOnServer = {
      ...h.messagesOnServer,
      messages: [
        dialogMessage(1, 'toolu_a', 'First question?'),
        dialogMessage(2, 'toolu_b', 'Second question?'),
      ],
      cursor: 2,
    };
    await emitMessagesChangedSettled();

    expect(container.textContent).toContain('Second question?');
    expect(buttonByText('Submit')!.disabled).toBe(true); // stale picks dropped
  });

  it('keeps in-progress picks, Other text and the input element across a refetch of the SAME dialog', async () => {
    // Every response is a fresh JSON parse — the same pending dialog arrives
    // as a new object each refetch. Neither the operator's picks, nor the
    // half-typed Other text, nor the input ELEMENT (focus!) may churn on an
    // SSE tick that changed nothing.
    h.messagesOnServer = {
      messages: [],
      state: 'question',
      cursor: 0,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_same',
        dialog_kind: 'question',
        prompt: 'Pick or type?',
        answerable: true,
        multi: true,
        options: [{ label: 'One' }, { label: 'Two' }, { label: 'Other', is_other: true }],
      },
    };
    await mountChat();

    buttonByText('One')!.click();
    buttonByText('Other')!.click();
    await settle();
    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'half-typed answer';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    await emitMessagesChangedSettled();

    const after = container.querySelector('.dialog-other-input') as HTMLInputElement;
    expect(after).toBe(other); // same element — focus survives
    expect(after.value).toBe('half-typed answer');
    expect(buttonByText('One')!.getAttribute('aria-pressed')).toBe('true');
    expect(buttonByText('Submit')!.disabled).toBe(false);
  });

  it('keeps multi-question form answers across a refetch of the SAME dialog', async () => {
    h.messagesOnServer = {
      messages: [],
      state: 'question',
      cursor: 0,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_form',
        dialog_kind: 'question',
        prompt: '2 questions',
        answerable: true,
        questions: [
          {
            text: 'Pick a flavor?',
            header: 'Flavor',
            options: [{ label: 'Sweet' }, { label: 'Sour' }, { label: 'Other', is_other: true }],
          },
          {
            text: 'Pick a size?',
            header: 'Size',
            options: [{ label: 'Small' }, { label: 'Large' }],
          },
        ],
      },
    };
    await mountChat();

    buttonByText('Other')!.click();
    await settle();
    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'umami';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    buttonByText('Large')!.click();
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    await emitMessagesChangedSettled();

    const after = container.querySelector('.dialog-other-input') as HTMLInputElement;
    expect(after).toBe(other);
    expect(after.value).toBe('umami');
    expect(buttonByText('Large')!.getAttribute('aria-pressed')).toBe('true');
    expect(buttonByText('Submit')!.disabled).toBe(false);
  });

  it('derives the answer-elsewhere hint from fallback_open in the locked question state', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    // fallback_open.url is https://claude.ai/code → the hint names its host.
    expect(container.querySelector('.chat-composer-note')?.textContent).toBe(
      'Claude Code needs input — open it at claude.ai to respond.',
    );
  });

  it('does not name the web host in the locked-question hint for a remote-off run', async () => {
    // Issue #163: with remote control off no claude.ai session was ever
    // registered, so naming that host sends the operator to a page that cannot
    // exist — the same reason the Open affordance is hidden. The hint must fall
    // back to the generic wording, and this branch is exactly where a non-remote
    // run lands (claude without --remote-control flushes its pending tool_use,
    // so the dialog arrives by transcript scan rather than the spool).
    h.runOnServer = { ...baseRun(), remote: false, deep_link_url: null };
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    const note = container.querySelector('.chat-composer-note')?.textContent;
    expect(note).toBe('Claude Code needs input — open the session to respond.');
    expect(note).not.toContain('claude.ai');
  });

  const multiQuestionDialog = () => ({
    tool_id: 'toolu_mq',
    dialog_kind: 'question' as const,
    prompt: '2 questions',
    answerable: true,
    questions: [
      {
        header: 'Approach',
        text: 'Which approach?',
        options: [
          { label: 'Revert', description: 'Roll back the change' },
          { label: 'Patch forward' },
          { label: 'Other', is_other: true },
        ],
      },
      {
        header: 'Scope',
        text: 'Which areas?',
        multi_select: true,
        options: [{ label: 'Frontend' }, { label: 'Backend' }, { label: 'Other', is_other: true }],
      },
    ],
  });

  function questionEl(i: number): Element {
    const el = container.querySelectorAll('.dialog-question')[i];
    if (!el) throw new Error(`missing .dialog-question[${i}]`);
    return el;
  }

  function questionOption(qi: number, label: string): HTMLButtonElement {
    const btn = Array.from(
      questionEl(qi).querySelectorAll<HTMLButtonElement>('button.dialog-option'),
    ).find((b) => b.querySelector('.dialog-option-label')?.textContent === label);
    if (!btn) throw new Error(`missing option "${label}" in question ${qi}`);
    return btn;
  }

  it('renders a multi-question form and submits all answers atomically via answers[]', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: multiQuestionDialog(),
    };
    await mountChat();

    // Composer locked; the form lives in the stream card with NO inner
    // scrollbox — nothing in the card carries an inline max-height (the CSS
    // caps are gone; jsdom applies no stylesheet, so inline style is the
    // assertable surface). The chat pane stays the only scrollbar.
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    const mqCard = container.querySelector('.chat-stream .chat-dialog-card');
    expect(mqCard).not.toBeNull();
    expect(mqCard?.querySelector('.dialog-questions')).not.toBeNull();
    expect(
      Array.from(mqCard!.querySelectorAll<HTMLElement>('*')).filter(
        (el) => el.style.maxHeight !== '',
      ),
    ).toEqual([]);

    // The two questions render stacked, in order, each with its header chip +
    // question text; options show label + description.
    const headers = Array.from(container.querySelectorAll('.dialog-question-header')).map(
      (el) => el.textContent,
    );
    expect(headers).toEqual(['Approach', 'Scope']);
    expect(questionEl(0).textContent).toContain('Which approach?');
    expect(questionEl(1).textContent).toContain('Which areas?');
    expect(questionOption(0, 'Revert').querySelector('.dialog-option-desc')?.textContent).toBe(
      'Roll back the change',
    );

    // BOTH questions keep the synthesized Other row — the multi-select one
    // included (the adapter fills the TUI's free-text row from other_text;
    // compat §7, captured live 2026-07-09).
    const optionLabels = (qi: number) =>
      Array.from(questionEl(qi).querySelectorAll('.dialog-option-label')).map(
        (el) => el.textContent,
      );
    expect(optionLabels(0)).toEqual(['Revert', 'Patch forward', 'Other']);
    expect(optionLabels(1)).toEqual(['Frontend', 'Backend', 'Other']);

    // ONE submit for the whole form, disabled until every question is answered.
    const submit = () => buttonByText('Submit')!;
    expect(submit().disabled).toBe(true);

    // Answer question 0 (single select) — still incomplete.
    questionOption(0, 'Patch forward').click();
    await settle();
    expect(questionOption(0, 'Patch forward').getAttribute('aria-pressed')).toBe('true');
    expect(submit().disabled).toBe(true);

    // Question 1 is multi-select: toggle two options.
    questionOption(1, 'Frontend').click();
    questionOption(1, 'Backend').click();
    await settle();
    expect(submit().disabled).toBe(false);

    // Multi-select toggles OFF again too.
    questionOption(1, 'Backend').click();
    await settle();
    expect(questionOption(1, 'Backend').getAttribute('aria-pressed')).toBe('false');
    questionOption(1, 'Backend').click();
    await settle();

    submit().click();
    await settle();

    // One atomic POST, positionally aligned with the questions (no question
    // index on the wire): a single-select answer is `index` = the chosen
    // OPTION index (never `selected`); a multi-select answer is `selected` =
    // the toggled option indices ascending (no `index`). No flat fields.
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_mq',
      answers: [{ index: 1 }, { selected: [0, 1] }],
    });
  });

  it('requires non-empty Other text for a single-select Other pick too', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: multiQuestionDialog(),
    };
    await mountChat();

    // Pick Other in question 0 and answer question 1 fully.
    questionOption(0, 'Other').click();
    questionOption(1, 'Frontend').click();
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(true); // Other text missing

    const other = questionEl(0).querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'ship a hotfix';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    buttonByText('Submit')!.click();
    await settle();
    // Single-select Other = index of the is_other row + the free text; the
    // multi-select answer stays selected-only.
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_mq',
      answers: [{ index: 2, other_text: 'ship a hotfix' }, { selected: [0] }],
    });
  });

  it('multi-select Other in a form: toggling it gates on text and rides other_text, never selected', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: multiQuestionDialog(),
    };
    await mountChat();

    questionOption(0, 'Revert').click();
    questionOption(1, 'Backend').click();
    questionOption(1, 'Other').click();
    await settle();
    // Other toggled but empty → the form is incomplete.
    expect(questionOption(1, 'Other').getAttribute('aria-pressed')).toBe('true');
    expect(buttonByText('Submit')!.disabled).toBe(true);

    const other = questionEl(1).querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'the build scripts';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    buttonByText('Submit')!.click();
    await settle();
    // The Other row's INDEX (2) stays out of selected — its text IS its
    // toggle (the adapter pastes it onto the TUI's free-text row, compat §7).
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_mq',
      answers: [{ index: 0 }, { selected: [1], other_text: 'the build scripts' }],
    });
  });

  it('multi-question form Other: Enter no-ops while incomplete, then fires the atomic submit once complete (issue #165)', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: multiQuestionDialog(),
    };
    await mountChat();

    questionOption(0, 'Other').click();
    await settle();
    const other = questionEl(0).querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'ship a hotfix';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    // Question 1 (Scope) is still unanswered — the form is incomplete, so
    // Enter in question 0's Other input must no-op exactly like the disabled
    // Submit button.
    expect(buttonByText('Submit')!.disabled).toBe(true);
    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.answerPosts).toHaveLength(0);

    questionOption(1, 'Frontend').click();
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    // Same atomic payload clicking Submit would send.
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_mq',
      answers: [{ index: 2, other_text: 'ship a hotfix' }, { selected: [0] }],
    });
  });

  it('renders a plan dialog as markdown with real approve/reject buttons answering flat', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'planned' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_plan',
        dialog_kind: 'plan',
        prompt: '# The plan\n\nDo **bold** things',
        answerable: true,
        options: [
          { label: 'Approve', description: 'Start implementing' },
          { label: 'Keep planning', description: 'Send feedback instead' },
        ],
      },
    };
    await mountChat();

    // The prompt IS the plan body — rendered markdown, not raw markup, inside
    // the stream card (issue #56 decision 4: full height, no inner scrollbox,
    // approve/reject after the whole plan).
    const plan = container.querySelector('.chat-stream .chat-dialog-card .chat-dialog-plan');
    expect(plan).not.toBeNull();
    expect(plan?.querySelector('.md-h')?.textContent).toBe('The plan');
    expect(plan?.querySelector('strong')?.textContent).toBe('bold');
    expect(plan?.textContent).not.toContain('# The plan');
    expect((plan as HTMLElement).style.maxHeight).toBe('');

    // Real option buttons with label + description; answering stays the flat
    // single-select shape (no answers[]).
    const approve = Array.from(
      container.querySelectorAll<HTMLButtonElement>('button.dialog-option'),
    ).find((b) => b.querySelector('.dialog-option-label')?.textContent === 'Approve');
    expect(approve?.querySelector('.dialog-option-desc')?.textContent).toBe('Start implementing');
    approve!.click();
    await settle();
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toEqual({ tool_id: 'toolu_plan', index: 0 });
  });

  it('renders the approval kind like a single-question dialog', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'may I?' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_ok',
        dialog_kind: 'approval',
        prompt: 'Allow the tool call?',
        answerable: true,
        options: [{ label: 'Allow' }, { label: 'Deny' }],
      },
    };
    await mountChat();

    expect(container.textContent).toContain('Allow the tool call?');
    buttonByText('Deny')!.click();
    await settle();
    expect(h.answerPosts[0]).toEqual({ tool_id: 'toolu_ok', index: 1 });
  });

  it('keeps the degraded note for a not-answerable dialog, hinting the provider surface', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'stuck' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_odd',
        dialog_kind: 'question',
        prompt: 'A shape lab cannot drive',
        answerable: false,
      },
    };
    await mountChat();

    // The degraded note renders inside the stream card (the composer shows its
    // own waiting note alongside — hence the scoped query).
    expect(container.querySelector('.chat-dialog-card .chat-composer-note')?.textContent).toBe(
      "This dialog can't be answered here — open it at claude.ai to respond.",
    );
    expect(container.querySelector('button.dialog-option')).toBeNull();
  });

  it('does not name the web host in the unanswerable-dialog note for a remote-off run', async () => {
    // The other half of the same gate (issue #163): an unanswerable shape on a
    // remote-off run must not point at a claude.ai session that was never
    // registered — the operator's real recourse is Interrupt.
    h.runOnServer = { ...baseRun(), remote: false, deep_link_url: null };
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'thinking…' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: {
        tool_id: 'toolu_odd',
        dialog_kind: 'question',
        prompt: 'A shape lab cannot drive',
        answerable: false,
      },
    };
    await mountChat();

    const note = container.querySelector('.chat-dialog-card .chat-composer-note')?.textContent;
    expect(note).toBe("This dialog can't be answered here — open the session to respond.");
    expect(note).not.toContain('claude.ai');
  });

  function withAnsweredDialog(dialog: Dialog): void {
    h.messagesOnServer = {
      messages: [
        { seq: 1, kind: 'dialog', dialog },
        { seq: 2, kind: 'text', role: 'assistant', text: 'moving on' },
      ],
      state: 'working',
      cursor: 2,
      has_more: false,
      transcript: 'available',
      pending_dialog: null,
    };
  }

  it('renders an answered single-select dialog as an inert Q→A summary', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_done',
      dialog_kind: 'question',
      prompt: 'Which fix?',
      answerable: true,
      options: [{ label: 'Revert' }, { label: 'Patch forward' }],
      outcome: { results: [{ question: 'Which fix?', chosen: ['Patch forward'] }] },
    });
    await mountChat();

    const summary = container.querySelector('.chat-stream .chat-dialog-answered');
    expect(summary).not.toBeNull();
    // The question (muted line) above the chosen label.
    expect(summary?.querySelector('.dialog-qa-question')?.textContent).toBe('Which fix?');
    expect(summary?.querySelector('.dialog-qa-chosen')?.textContent).toBe('Patch forward');
    // Compact history: the unchosen option is NOT rendered.
    expect(summary?.textContent).not.toContain('Revert');
    // Inert: no buttons, no interactive card, no raw tool chip.
    expect(summary?.querySelector('button')).toBeNull();
    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    expect(container.querySelector('.chat-tool')).toBeNull();
  });

  it('renders one Q→A pair per outcome result, in dialog order', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_mq_done',
      dialog_kind: 'question',
      prompt: '2 questions',
      answerable: true,
      outcome: {
        results: [
          { question: 'Which approach?', chosen: ['Patch forward'] },
          { question: 'Which areas?', chosen: ['Frontend', 'Backend'] },
        ],
      },
    });
    await mountChat();

    const pairs = Array.from(container.querySelectorAll('.dialog-qa'));
    expect(pairs).toHaveLength(2);
    expect(pairs[0]?.querySelector('.dialog-qa-question')?.textContent).toBe('Which approach?');
    expect(pairs[0]?.querySelector('.dialog-qa-chosen')?.textContent).toBe('Patch forward');
    expect(pairs[1]?.querySelector('.dialog-qa-question')?.textContent).toBe('Which areas?');
    // Multi-select labels join with ", " in recorded toggle order.
    expect(pairs[1]?.querySelector('.dialog-qa-chosen')?.textContent).toBe('Frontend, Backend');
  });

  it('renders an other-text answer as the operator’s quoted words, distinct from labels', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_other',
      dialog_kind: 'question',
      prompt: 'Which fix?',
      answerable: true,
      outcome: {
        // A multi-select result can carry BOTH chosen labels and typed text.
        results: [
          { question: 'Which fix?', chosen: ['Patch forward'], other_text: 'and add a test' },
        ],
      },
    });
    await mountChat();

    const answer = container.querySelector('.dialog-qa-answer');
    expect(answer?.querySelector('.dialog-qa-chosen')?.textContent).toBe('Patch forward');
    // The typed text renders quoted — a different text node than a label.
    expect(answer?.querySelector('.dialog-qa-other')?.textContent).toBe('“and add a test”');
  });

  it('marks a result with neither chosen nor other_text as unanswered', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_blank',
      dialog_kind: 'question',
      prompt: 'Which fix?',
      answerable: true,
      outcome: { results: [{ question: 'Which fix?' }] },
    });
    await mountChat();

    expect(container.querySelector('.dialog-qa-question')?.textContent).toBe('Which fix?');
    expect(container.querySelector('.dialog-qa-none')?.textContent).toBe('No answer recorded');
  });

  it('renders a dismissed dialog as the question text plus one dismissed marker', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_dismissed',
      dialog_kind: 'question',
      prompt: 'Which fix?',
      answerable: true,
      options: [{ label: 'Revert' }],
      outcome: { dismissed: true, results: [{ question: 'Which fix?' }] },
    });
    await mountChat();

    const summary = container.querySelector('.chat-dialog-answered')!;
    expect(summary.querySelector('.dialog-qa-question')?.textContent).toBe('Which fix?');
    const markers = summary.querySelectorAll('.chat-dialog-outcome');
    expect(markers).toHaveLength(1);
    expect(markers[0]?.textContent).toBe('Dismissed');
    expect(summary.querySelector('button')).toBeNull();
    expect(container.querySelector('.chat-dialog-card')).toBeNull();
  });

  it('renders an approved plan as the full markdown plus the approved marker, inert', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_plan_ok',
      dialog_kind: 'plan',
      prompt: '# The plan\n\nDo **bold** things',
      answerable: true,
      options: [{ label: 'Approve' }, { label: 'Keep planning' }],
      outcome: { approved: true },
    });
    await mountChat();

    const summary = container.querySelector('.chat-dialog-answered')!;
    // The FULL plan, rendered markdown (same .chat-dialog-plan face as live).
    expect(summary.querySelector('.chat-dialog-plan .md-h')?.textContent).toBe('The plan');
    expect(summary.querySelector('.chat-dialog-plan strong')?.textContent).toBe('bold');
    expect(summary.querySelector('.chat-dialog-outcome')?.textContent).toBe('Plan approved');
    // No approve/reject buttons — this is history.
    expect(summary.querySelector('button')).toBeNull();
    expect(buttonByText('Approve')).toBeNull();
    expect(container.querySelector('.chat-dialog-card')).toBeNull();
  });

  it('renders a rejected plan with the typed feedback as an operator quote', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_plan_no',
      dialog_kind: 'plan',
      prompt: '# The plan\n\nSteps',
      answerable: true,
      outcome: { feedback: 'tighten the tests first' },
    });
    await mountChat();

    const summary = container.querySelector('.chat-dialog-answered')!;
    expect(summary.querySelector('.chat-dialog-outcome')?.textContent).toBe('Plan rejected');
    expect(summary.querySelector('.dialog-qa-other')?.textContent).toBe(
      '“tighten the tests first”',
    );
  });

  it('treats an EMPTY outcome object as answered (rejection without feedback)', async () => {
    // The critical wire semantics: all outcome fields are omitempty, so a plan
    // rejected without typed feedback arrives as {} — still answered.
    withAnsweredDialog({
      tool_id: 'toolu_plan_bare',
      dialog_kind: 'plan',
      prompt: '# The plan\n\nSteps',
      answerable: true,
      outcome: {},
    });
    await mountChat();

    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    expect(container.querySelector('.chat-dialog-answered .chat-dialog-outcome')?.textContent).toBe(
      'Plan rejected',
    );
    expect(container.querySelector('.chat-dialog-answered button')).toBeNull();
  });

  it('renders a plan dismissal marker', async () => {
    withAnsweredDialog({
      tool_id: 'toolu_plan_gone',
      dialog_kind: 'plan',
      prompt: '# The plan\n\nSteps',
      answerable: true,
      outcome: { dismissed: true },
    });
    await mountChat();

    expect(container.querySelector('.chat-dialog-answered .chat-dialog-outcome')?.textContent).toBe(
      'Plan dismissed',
    );
  });

  function optionCard(label: string): HTMLButtonElement {
    const btn = Array.from(
      container.querySelectorAll<HTMLButtonElement>('button.dialog-option'),
    ).find((b) => b.querySelector('.dialog-option-label')?.textContent === label);
    if (!btn) throw new Error(`missing option card "${label}"`);
    return btn;
  }

  const flatMultiDialog = () => ({
    tool_id: 'toolu_flat_multi',
    dialog_kind: 'question' as const,
    prompt: 'Which areas?',
    answerable: true,
    multi: true,
    options: [
      { label: 'Frontend', description: 'The SPA under web/' },
      { label: 'Backend', description: 'The Go API' },
      { label: 'Other', is_other: true },
    ],
  });

  it('renders flat multi-select options as toggle cards with visible descriptions', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'pick some' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatMultiDialog(),
    };
    await mountChat();

    // Toggle-card buttons, not checkboxes — and the descriptions show now.
    expect(container.querySelector('.dialog-check')).toBeNull();
    expect(container.querySelector('input[type="checkbox"]')).toBeNull();
    expect(optionCard('Frontend').querySelector('.dialog-option-desc')?.textContent).toBe(
      'The SPA under web/',
    );
    // Card, not pill: the seg class is gone from dialog options.
    expect(optionCard('Frontend').classList.contains('seg')).toBe(false);
    // Completeness gating: nothing selected yet → Submit disabled.
    expect(buttonByText('Submit')!.disabled).toBe(true);

    // Toggling carries the selected state on the card itself.
    expect(optionCard('Frontend').getAttribute('aria-pressed')).toBe('false');
    optionCard('Frontend').click();
    await settle();
    expect(optionCard('Frontend').getAttribute('aria-pressed')).toBe('true');
    expect(optionCard('Frontend').classList.contains('selected')).toBe(true);
    expect(optionCard('Frontend').querySelector('.dialog-option-check')).not.toBeNull();

    // Toggling OFF works too, then re-select both for the submit.
    optionCard('Frontend').click();
    await settle();
    expect(optionCard('Frontend').getAttribute('aria-pressed')).toBe('false');
    expect(optionCard('Frontend').querySelector('.dialog-option-check')).toBeNull();
    optionCard('Frontend').click();
    optionCard('Backend').click();
    await settle();

    // The Submit flow and payload are byte-for-byte the pre-card contract
    // (an untouched Other row adds nothing to the wire).
    buttonByText('Submit')!.click();
    await settle();
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toEqual({ tool_id: 'toolu_flat_multi', selected: [0, 1] });
  });

  it('flat multi-select Other: toggling opens the input, gates Submit on text, rides other_text', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'pick some' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatMultiDialog(),
    };
    await mountChat();

    // No stray input while Other is untoggled.
    expect(container.querySelector('.dialog-other-input')).toBeNull();

    optionCard('Backend').click();
    optionCard('Other').click();
    await settle();
    // Other toggled but empty → Submit stays disabled even with a real pick.
    expect(optionCard('Other').getAttribute('aria-pressed')).toBe('true');
    expect(buttonByText('Submit')!.disabled).toBe(true);

    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;
    expect(other).not.toBeNull();
    other.value = 'the CI pipeline';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    buttonByText('Submit')!.click();
    await settle();
    // The Other row's INDEX (2) never enters selected — its text IS its
    // toggle (the adapter pastes it onto the TUI's "Type something" row,
    // which fills AND checks it — compat §7, live 2026-07-09).
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_flat_multi',
      selected: [1],
      other_text: 'the CI pipeline',
    });
  });

  it('flat multi-select accepts an Other-only answer (nothing else toggled)', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'pick some' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatMultiDialog(),
    };
    await mountChat();

    optionCard('Other').click();
    await settle();
    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;
    other.value = 'docs only';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    buttonByText('Submit')!.click();
    await settle();
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_flat_multi',
      selected: [],
      other_text: 'docs only',
    });
  });

  it('flat multi-select Other: Enter no-ops until ready, then submits exactly like clicking Submit (issue #165)', async () => {
    h.messagesOnServer = {
      messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'pick some' }],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatMultiDialog(),
    };
    await mountChat();

    optionCard('Backend').click();
    optionCard('Other').click();
    await settle();
    const other = container.querySelector('.dialog-other-input') as HTMLInputElement;

    // Other toggled but still empty → not ready, Enter no-ops like the
    // disabled Submit.
    expect(buttonByText('Submit')!.disabled).toBe(true);
    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.answerPosts).toHaveLength(0);

    other.value = 'the CI pipeline';
    other.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(buttonByText('Submit')!.disabled).toBe(false);

    other.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.answerPosts).toHaveLength(1);
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_flat_multi',
      selected: [1],
      other_text: 'the CI pipeline',
    });
  });
});
