// Docked question panel contract (issue #58 §3 + §4), the successor of the
// in-stream question card tests (issue #56 / issue #51 decision 3) that lived
// in Dialogs.test.tsx:
// - an answerable QUESTION dialog (flat single-select, flat multi-select,
//   multi-question) docks above the composer, never as an in-stream card; the
//   stream marks its position with ONE "…is asking … Answer below." line (at
//   its transcript message, else appended); plan reviews and non-answerable
//   dialogs keep the card (Dialogs.test.tsx);
// - options are radios / checkboxes and picking never submits; the free-text
//   row is the composer's single-line box, never an option; single-select
//   typing and picking clear each other; multi-select text rides other_text
//   and the free-text index never enters `selected`; "(Recommended)" options
//   are preselected with a tag in place of the suffix;
// - a multi-question dialog steps one question at a time behind chips, keeps
//   typed text per question, reviews every answer, and sends ONE positional
//   answers[] POST, disabled until complete;
// - drafts survive refetches of the same dialog and reset on a new tool_id;
//   the panel folds; slash autocomplete is off;
// - "Chat about this" holds the question (sends nothing; Back restores every
//   draft) and its Send posts exactly {tool_id, chat_text}.

import { describe, expect, it, vi } from 'vitest';
import type { ChatMessage, Dialog } from '../../api';
import {
  RUN_ID,
  buttonByText,
  container,
  emitMessagesChangedSettled,
  h,
  installChatHooks,
  jsonResponse,
  mountChat,
  settle,
} from './harness';

installChatHooks();

const flatSingle = (toolID = 'toolu_single'): Dialog => ({
  tool_id: toolID,
  dialog_kind: 'question',
  prompt: 'Which fix?',
  answerable: true,
  options: [
    { label: 'Revert', description: 'Roll back the change' },
    { label: 'Patch forward' },
    { label: 'Other', is_other: true },
  ],
});

const flatMulti = (toolID = 'toolu_flat_multi'): Dialog => ({
  tool_id: toolID,
  dialog_kind: 'question',
  prompt: 'Which areas?',
  answerable: true,
  multi: true,
  options: [
    { label: 'Frontend', description: 'The SPA under web/' },
    { label: 'Backend', description: 'The Go API' },
    { label: 'Other', is_other: true },
  ],
});

const threeQuestions = (toolID = 'toolu_mq'): Dialog => ({
  tool_id: toolID,
  dialog_kind: 'question',
  prompt: '3 questions',
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
    {
      // No header: the chip and the Review row fall back to "Question 3".
      text: 'Anything else?',
      options: [{ label: 'Nope' }, { label: 'Other', is_other: true }],
    },
  ],
});

function withPending(dialog: Dialog): void {
  h.messagesOnServer = {
    messages: [{ seq: 1, kind: 'text', role: 'assistant', text: 'need input' }],
    state: 'question',
    cursor: 1,
    has_more: false,
    transcript: 'available',
    pending_dialog: dialog,
  };
}

const panel = () => container.querySelector<HTMLElement>('.chat-composer .chat-qpanel');
const questionText = () => container.querySelector('.chat-qpanel-q')?.textContent ?? '';
const answerInput = () => container.querySelector<HTMLInputElement>('.chat-answer-input');
const confirmBtn = () =>
  container.querySelector<HTMLButtonElement>('.chat-answer-row .chat-answer-confirm');

function option(label: string): HTMLButtonElement {
  const btn = Array.from(container.querySelectorAll<HTMLButtonElement>('.chat-qopt')).find(
    (b) => b.querySelector('.chat-qopt-label')?.textContent === label,
  );
  if (!btn) throw new Error(`missing option row "${label}"`);
  return btn;
}

function optionLabels(): string[] {
  return Array.from(container.querySelectorAll('.chat-qopt .chat-qopt-label')).map(
    (el) => el.textContent ?? '',
  );
}

function typeAnswer(value: string): void {
  const input = answerInput();
  if (!input) throw new Error('missing answer box');
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
}

function chips(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('.chat-qstep'));
}

function chip(name: string): HTMLButtonElement {
  const c = chips().find(
    (b) => (b.querySelector('.chat-qstep-name')?.textContent ?? b.textContent?.trim()) === name,
  );
  if (!c) throw new Error(`missing chip "${name}"`);
  return c;
}

function reviewRows(): { head: string; answer: string }[] {
  return Array.from(container.querySelectorAll('.chat-qreview-row')).map((row) => ({
    head: row.querySelector('.chat-qreview-head')?.textContent ?? '',
    answer: row.querySelector('.chat-qreview-answer')?.textContent ?? '',
  }));
}

describe('QuestionDock', () => {
  it('docks a pending flat single-select question instead of an in-stream card, marked by one asking line', async () => {
    withPending(flatSingle());
    await mountChat();

    // No interactive card in the stream: the dialog's position (appended —
    // the spool-served dialog has no transcript message) carries one marker.
    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    const stream = container.querySelector('.chat-stream')!;
    const marker = stream.lastElementChild;
    expect(marker?.classList.contains('chat-lifecycle')).toBe(true);
    expect(marker?.textContent).toBe('Claude Code is asking a question. Answer below.');
    expect(stream.querySelectorAll('.chat-dialog-asking')).toHaveLength(1);

    // The panel docks inside the composer: header, count, question, rows.
    const p = panel();
    expect(p).not.toBeNull();
    expect(p!.getAttribute('aria-label')).toBe('Claude Code is asking');
    expect(p!.querySelector('.chat-qpanel-title')?.textContent).toBe('Claude Code is asking');
    expect(p!.querySelector('.chat-qpanel-count')?.textContent).toBe('1 question');
    expect(questionText()).toBe('Which fix?');
    // Radios in a radiogroup; the free-text row is NOT an option.
    expect(p!.querySelector('[role="radiogroup"]')).not.toBeNull();
    expect(p!.querySelectorAll('[role="radio"]')).toHaveLength(2);
    expect(optionLabels()).toEqual(['Revert', 'Patch forward']);
    expect(option('Revert').querySelector('.dialog-option-desc')?.textContent).toBe(
      'Roll back the change',
    );

    // The normal composer, its status line and its slash affordances are gone;
    // the answer box is a SINGLE-LINE input (free text validates single-line).
    expect(container.querySelector('.chat-composer-row')).toBeNull();
    expect(container.querySelector('.chat-status')).toBeNull();
    expect(answerInput()?.tagName).toBe('INPUT');
    expect(answerInput()?.placeholder).toBe('Or type your own answer…');
    expect(confirmBtn()?.textContent?.trim()).toBe('Answer');
    expect(confirmBtn()!.disabled).toBe(true); // nothing picked or typed yet

    // Picking never submits — it only checks the row (radio semantics).
    option('Patch forward').click();
    await settle();
    expect(h.answerPosts).toHaveLength(0);
    expect(option('Patch forward').getAttribute('aria-checked')).toBe('true');
    expect(confirmBtn()!.disabled).toBe(false);
    option('Revert').click();
    await settle();
    expect(option('Revert').getAttribute('aria-checked')).toBe('true');
    expect(option('Patch forward').getAttribute('aria-checked')).toBe('false');
    expect(h.answerPosts).toHaveLength(0);

    confirmBtn()!.click();
    await settle();
    expect(h.answerPosts).toEqual([{ tool_id: 'toolu_single', index: 0 }]);
  });

  it('single-select: typed text answers as the free-text row, and typing and picking clear each other', async () => {
    withPending(flatSingle());
    await mountChat();

    option('Revert').click();
    await settle();
    typeAnswer('roll it back by hand');
    await settle();
    // Typing replaced the pick.
    expect(option('Revert').getAttribute('aria-checked')).toBe('false');
    expect(confirmBtn()!.disabled).toBe(false);

    // Picking replaces the typed text.
    option('Patch forward').click();
    await settle();
    expect(answerInput()!.value).toBe('');
    expect(option('Patch forward').getAttribute('aria-checked')).toBe('true');

    typeAnswer('roll it back by hand');
    await settle();
    expect(option('Patch forward').getAttribute('aria-checked')).toBe('false');
    confirmBtn()!.click();
    await settle();
    // The free-text row's index + the text, exactly today's flat shape.
    expect(h.answerPosts).toEqual([
      { tool_id: 'toolu_single', index: 2, other_text: 'roll it back by hand' },
    ]);
  });

  it('Enter in the answer box confirms exactly like Answer; empty or mid-IME Enter no-ops', async () => {
    withPending(flatSingle());
    await mountChat();

    answerInput()!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.answerPosts).toHaveLength(0); // nothing to answer with yet

    typeAnswer('still composing');
    await settle();
    answerInput()!.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', isComposing: true, bubbles: true }),
    );
    await settle();
    expect(h.answerPosts).toHaveLength(0);

    answerInput()!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(h.answerPosts).toEqual([
      { tool_id: 'toolu_single', index: 2, other_text: 'still composing' },
    ]);
  });

  it('preselects a "(Recommended)" option, tags it in place of the suffix, and Answer submits it as is', async () => {
    withPending({
      ...flatSingle('toolu_rec'),
      options: [
        { label: 'Add to PR #7' },
        { label: 'Separate PR (Recommended)', description: 'Merges today' },
        // Case-insensitive, trailing whitespace tolerated — but single-select
        // preselects only the FIRST recommendation.
        { label: 'Only write the plan (recommended)  ' },
        { label: 'Other', is_other: true },
      ],
    });
    await mountChat();

    expect(optionLabels()).toEqual(['Add to PR #7', 'Separate PR', 'Only write the plan']);
    const rec = option('Separate PR');
    expect(rec.getAttribute('aria-checked')).toBe('true');
    expect(rec.querySelector('.chat-qopt-tag')?.textContent).toBe('Recommended');
    expect(rec.textContent).not.toContain('(Recommended)');
    // The second recommendation is tagged but not picked.
    const second = option('Only write the plan');
    expect(second.querySelector('.chat-qopt-tag')?.textContent).toBe('Recommended');
    expect(second.getAttribute('aria-checked')).toBe('false');
    expect(option('Add to PR #7').querySelector('.chat-qopt-tag')).toBeNull();

    // No further input needed.
    expect(confirmBtn()!.disabled).toBe(false);
    confirmBtn()!.click();
    await settle();
    expect(h.answerPosts).toEqual([{ tool_id: 'toolu_rec', index: 1 }]);
  });

  it('preselects every recommended option of a multi-select question', async () => {
    withPending({
      ...flatMulti('toolu_rec_multi'),
      options: [
        { label: 'Docs (Recommended)' },
        { label: 'CI paths' },
        { label: 'Nix hash (RECOMMENDED)' },
        { label: 'Other', is_other: true },
      ],
    });
    await mountChat();

    expect(option('Docs').getAttribute('aria-checked')).toBe('true');
    expect(option('CI paths').getAttribute('aria-checked')).toBe('false');
    expect(option('Nix hash').getAttribute('aria-checked')).toBe('true');
    confirmBtn()!.click();
    await settle();
    expect(h.answerPosts).toEqual([{ tool_id: 'toolu_rec_multi', selected: [0, 2] }]);
  });

  it('omits the text box for a question with no free-text row', async () => {
    withPending({
      ...flatSingle('toolu_closed'),
      options: [{ label: 'Yes' }, { label: 'No' }],
    });
    await mountChat();

    expect(answerInput()).toBeNull();
    expect(container.querySelector('.chat-answer-row.no-text')).not.toBeNull();
    option('No').click();
    await settle();
    confirmBtn()!.click();
    await settle();
    expect(h.answerPosts).toEqual([{ tool_id: 'toolu_closed', index: 1 }]);
  });

  it('flat multi-select: checkboxes submit ticked indices ascending plus typed text, never the free-text index', async () => {
    withPending(flatMulti());
    await mountChat();

    const p = panel()!;
    expect(p.querySelector('[role="group"].chat-qopts')).not.toBeNull();
    expect(p.querySelectorAll('[role="checkbox"]')).toHaveLength(2);
    expect(optionLabels()).toEqual(['Frontend', 'Backend']); // no "Other" row
    expect(questionText()).toContain('Pick any that apply.');
    expect(option('Frontend').querySelector('.dialog-option-desc')?.textContent).toBe(
      'The SPA under web/',
    );
    expect(confirmBtn()!.disabled).toBe(true);

    // Ticks toggle (and untoggle) without submitting.
    option('Backend').click();
    option('Frontend').click();
    await settle();
    expect(option('Frontend').getAttribute('aria-checked')).toBe('true');
    option('Frontend').click();
    await settle();
    expect(option('Frontend').getAttribute('aria-checked')).toBe('false');
    option('Frontend').click();
    await settle();
    expect(h.answerPosts).toHaveLength(0);

    // Typed text ADDS to the ticks on multi-select.
    typeAnswer('the CI pipeline');
    await settle();
    expect(option('Backend').getAttribute('aria-checked')).toBe('true');

    confirmBtn()!.click();
    await settle();
    expect(h.answerPosts).toEqual([
      { tool_id: 'toolu_flat_multi', selected: [0, 1], other_text: 'the CI pipeline' },
    ]);
  });

  it('flat multi-select accepts a typed-only answer, and a ticks-only answer carries no other_text', async () => {
    withPending(flatMulti());
    await mountChat();

    typeAnswer('docs only');
    await settle();
    confirmBtn()!.click();
    await settle();
    expect(h.answerPosts[0]).toEqual({
      tool_id: 'toolu_flat_multi',
      selected: [],
      other_text: 'docs only',
    });

    typeAnswer('   '); // whitespace is no answer
    option('Backend').click();
    await settle();
    confirmBtn()!.click();
    await settle();
    expect(h.answerPosts[1]).toEqual({ tool_id: 'toolu_flat_multi', selected: [1] });
  });

  it('steps a three-question dialog one question at a time and sends one positional answers[] from Review', async () => {
    withPending(threeQuestions());
    await mountChat();

    expect(container.querySelector('.chat-dialog-asking')?.textContent).toBe(
      'Claude Code is asking 3 questions. Answer below.',
    );
    expect(panel()!.querySelector('.chat-qpanel-count')?.textContent).toBe('3 questions');
    // Chips: each question by header (fallback "Question N"), then Review.
    expect(
      chips().map((c) => c.querySelector('.chat-qstep-name')?.textContent ?? 'REVIEW'),
    ).toEqual(['Approach', 'Scope', 'Question 3', 'REVIEW']);
    expect(chips()[3]!.textContent?.trim()).toBe('Review');
    expect(chip('Approach').getAttribute('aria-current')).toBe('step');

    // One question on screen.
    expect(questionText()).toBe('Which approach?');
    expect(panel()!.textContent).not.toContain('Which areas?');
    expect(confirmBtn()!.textContent?.trim()).toBe('Next');
    expect(confirmBtn()!.disabled).toBe(true);

    // Q1 by typed text → Next.
    typeAnswer('ship a hotfix');
    await settle();
    confirmBtn()!.click();
    await settle();
    expect(questionText()).toContain('Which areas?');
    expect(answerInput()!.value).toBe(''); // the box belongs to the question on screen
    expect(chip('Scope').getAttribute('aria-current')).toBe('step');
    // Q1 answered AND left: its chip ticks (the number becomes a check).
    expect(chip('Approach').classList.contains('done')).toBe(true);
    expect(chip('Approach').getAttribute('aria-label')).toBe('Approach, answered');
    expect(chip('Approach').querySelector('.chat-qstep-mark svg')).not.toBeNull();
    expect(chip('Scope').classList.contains('done')).toBe(false);

    // Q2 (multi-select): a tick plus typed text.
    option('Frontend').click();
    typeAnswer('and the docs');
    await settle();
    expect(confirmBtn()!.textContent?.trim()).toBe('Next');
    confirmBtn()!.click();
    await settle();
    expect(questionText()).toBe('Anything else?');
    expect(confirmBtn()!.textContent?.trim()).toBe('Review'); // the last question

    // Chips are tappable in any order; typed text is restored per question.
    chip('Approach').click();
    await settle();
    expect(questionText()).toBe('Which approach?');
    expect(answerInput()!.value).toBe('ship a hotfix');

    // Review straight from a chip, with question 3 still open.
    chips()[3]!.click();
    await settle();
    expect(chips()[3]!.getAttribute('aria-current')).toBe('step');
    expect(reviewRows()).toEqual([
      { head: 'Approach', answer: '“ship a hotfix”' },
      { head: 'Scope', answer: 'Frontend, “and the docs”' },
      { head: 'Question 3', answer: 'Not answered yet' },
    ]);
    // The text box gives way to one full-width send, disabled while any
    // question lacks an answer; no "Chat about this" on Review.
    expect(answerInput()).toBeNull();
    const sendAll = () => buttonByText('Send 3 answers')!;
    expect(sendAll().disabled).toBe(true);
    expect(buttonByText('Chat about this')).toBeNull();

    // Tapping a Review row returns to that question.
    (
      container.querySelectorAll<HTMLButtonElement>('.chat-qreview-row')[2] as HTMLButtonElement
    ).click();
    await settle();
    expect(questionText()).toBe('Anything else?');
    option('Nope').click();
    await settle();
    confirmBtn()!.click(); // "Review"
    await settle();
    expect(reviewRows()[2]).toEqual({ head: 'Question 3', answer: 'Nope' });
    expect(sendAll().disabled).toBe(false);
    expect(h.answerPosts).toHaveLength(0); // nothing sent while stepping

    sendAll().click();
    await settle();
    // ONE POST, positionally aligned: single-select free text = the free-text
    // row's index + other_text; multi-select = selected (real indices) +
    // other_text; a picked single-select = index.
    expect(h.answerPosts).toEqual([
      {
        tool_id: 'toolu_mq',
        answers: [
          { index: 2, other_text: 'ship a hotfix' },
          { selected: [0], other_text: 'and the docs' },
          { index: 0 },
        ],
      },
    ]);
  });

  it('keeps the current question, picks and typed text across a refetch of the SAME dialog; a new tool_id resets them', async () => {
    withPending(threeQuestions('toolu_same'));
    await mountChat();

    option('Patch forward').click();
    await settle();
    confirmBtn()!.click(); // Next
    await settle();
    option('Backend').click();
    typeAnswer('half-typed');
    await settle();
    const input = answerInput()!;

    // A refetch delivers a FRESH dialog object with the same tool_id.
    h.messagesOnServer = { ...h.messagesOnServer, pending_dialog: threeQuestions('toolu_same') };
    await emitMessagesChangedSettled();

    expect(questionText()).toContain('Which areas?');
    expect(answerInput()).toBe(input); // same element — focus survives
    expect(answerInput()!.value).toBe('half-typed');
    expect(option('Backend').getAttribute('aria-checked')).toBe('true');
    expect(chip('Approach').classList.contains('done')).toBe(true);
    chip('Approach').click();
    await settle();
    expect(option('Patch forward').getAttribute('aria-checked')).toBe('true');

    // A NEW dialog identity drops every draft and starts at question 1.
    chip('Scope').click();
    await settle();
    h.messagesOnServer = { ...h.messagesOnServer, pending_dialog: threeQuestions('toolu_next') };
    await emitMessagesChangedSettled();

    expect(questionText()).toBe('Which approach?');
    expect(option('Patch forward').getAttribute('aria-checked')).toBe('false');
    expect(answerInput()!.value).toBe('');
    expect(chips().some((c) => c.classList.contains('done'))).toBe(false);
  });

  it('keeps a flat question’s pick and typed text across a refetch of the SAME dialog', async () => {
    withPending(flatMulti('toolu_flat_same'));
    await mountChat();

    option('Frontend').click();
    typeAnswer('half-typed answer');
    await settle();
    const input = answerInput()!;

    h.messagesOnServer = { ...h.messagesOnServer, pending_dialog: flatMulti('toolu_flat_same') };
    await emitMessagesChangedSettled();

    expect(answerInput()).toBe(input);
    expect(answerInput()!.value).toBe('half-typed answer');
    expect(option('Frontend').getAttribute('aria-checked')).toBe('true');
    expect(confirmBtn()!.disabled).toBe(false);
  });

  it('folds the panel to its header line and back; the fold resets with a new dialog', async () => {
    withPending(flatSingle('toolu_fold_a'));
    await mountChat();

    const fold = () => container.querySelector<HTMLButtonElement>('.chat-qpanel-fold')!;
    const body = () => container.querySelector<HTMLElement>('.chat-qpanel-body')!;
    expect(fold().getAttribute('aria-label')).toBe('Question');
    expect(fold().getAttribute('aria-expanded')).toBe('true');
    expect(fold().getAttribute('aria-controls')).toBe(body().id);
    expect(body().hidden).toBe(false);

    fold().click();
    await settle();
    expect(fold().getAttribute('aria-expanded')).toBe('false');
    expect(body().hidden).toBe(true);
    // The header line stays, and so does the answer row below the panel.
    expect(panel()!.querySelector('.chat-qpanel-title')?.textContent).toBe('Claude Code is asking');
    expect(confirmBtn()).not.toBeNull();

    fold().click();
    await settle();
    expect(body().hidden).toBe(false);

    fold().click();
    await settle();
    h.messagesOnServer = { ...h.messagesOnServer, pending_dialog: flatSingle('toolu_fold_b') };
    await emitMessagesChangedSettled();
    expect(fold().getAttribute('aria-expanded')).toBe('true');
    expect(body().hidden).toBe(false);
  });

  it('keeps slash autocomplete and the / button off while the panel is open', async () => {
    withPending(flatSingle());
    await mountChat(); // the default catalog has three commands

    expect(container.querySelector('.chat-slash')).toBeNull();
    typeAnswer('/');
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();
  });

  it('Enter in a stepper’s answer box is exactly Next / Review — a no-op while unanswered, never a send', async () => {
    withPending(threeQuestions());
    await mountChat();
    const enter = () =>
      answerInput()!.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));

    enter(); // question 1 unanswered → stays put
    await settle();
    expect(questionText()).toBe('Which approach?');

    typeAnswer('ship a hotfix');
    await settle();
    enter(); // = Next
    await settle();
    expect(questionText()).toContain('Which areas?');

    typeAnswer('the docs');
    await settle();
    enter(); // = Next
    await settle();
    typeAnswer('nothing else');
    await settle();
    enter(); // = Review on the last question — still no POST
    await settle();
    expect(container.querySelector('.chat-qreview')).not.toBeNull();
    expect(h.answerPosts).toHaveLength(0);
  });

  it('Chat about this holds the question, and Back restores the panel with every draft, sending nothing', async () => {
    withPending(threeQuestions());
    await mountChat();

    option('Patch forward').click();
    await settle();
    confirmBtn()!.click(); // Next → question 2
    await settle();
    typeAnswer('maybe both');
    await settle();

    buttonByText('Chat about this')!.click();
    await settle();

    // The panel folds to one "on hold" line with a Back button.
    expect(panel()!.querySelector('.chat-qpanel-title')?.textContent).toBe('3 questions on hold');
    expect(container.querySelector<HTMLElement>('.chat-qpanel-body')!.hidden).toBe(true);
    expect(container.querySelector('.chat-qpanel-back')?.textContent).toBe('Back to questions');
    // The composer is a normal multi-line reply box quoting the question that
    // was on screen, with the hold note beneath.
    const box = container.querySelector<HTMLTextAreaElement>('.chat-hold textarea');
    expect(box).not.toBeNull();
    expect(document.activeElement).toBe(box);
    expect(container.querySelector('.chat-hold-quote-text')?.textContent).toBe('Which areas?');
    expect(container.querySelector('.chat-hold-note')?.textContent).toBe(
      'Sending sets the questions aside. Claude Code asks again after it has replied.',
    );
    expect(answerInput()).toBeNull();
    // Slash autocomplete stays off in this mode too.
    box!.value = '/';
    box!.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(container.querySelector('.chat-cmd-pop')).toBeNull();

    // The quote's dismiss control is the same Back.
    const dismiss = container.querySelector<HTMLButtonElement>('.chat-hold-dismiss')!;
    expect(dismiss.getAttribute('aria-label')).toBe('Back to questions');
    dismiss.click();
    await settle();

    // Everything as it was: question 2 on screen, its text, question 1's pick.
    expect(container.querySelector('.chat-hold')).toBeNull();
    expect(questionText()).toContain('Which areas?');
    expect(answerInput()!.value).toBe('maybe both');
    expect(chip('Approach').classList.contains('done')).toBe(true);
    chip('Approach').click();
    await settle();
    expect(option('Patch forward').getAttribute('aria-checked')).toBe('true');
    expect(h.answerPosts).toHaveLength(0);
    expect(h.replyPosts).toHaveLength(0);
  });

  it('Chat about this then Send posts exactly {tool_id, chat_text} and leaves chat mode', async () => {
    withPending(flatSingle('toolu_chat'));
    await mountChat();

    option('Revert').click();
    await settle();
    buttonByText('Chat about this')!.click();
    await settle();

    expect(panel()!.querySelector('.chat-qpanel-title')?.textContent).toBe('Question on hold');
    expect(container.querySelector('.chat-qpanel-back')?.textContent).toBe('Back to question');
    expect(container.querySelector('.chat-hold-quote-text')?.textContent).toBe('Which fix?');
    expect(container.querySelector('.chat-hold-note')?.textContent).toBe(
      'Sending sets the question aside. Claude Code asks again after it has replied.',
    );

    const send = () => container.querySelector<HTMLButtonElement>('.chat-hold .chat-send')!;
    expect(send().getAttribute('aria-label')).toBe('Send');
    expect(send().disabled).toBe(true);
    const box = container.querySelector<HTMLTextAreaElement>('.chat-hold textarea')!;
    box.value = '  why not both?  ';
    box.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    expect(send().disabled).toBe(false);

    send().click();
    await settle();
    // Only the typed words, trimmed — never the quote, never the draft pick.
    expect(h.answerPosts).toEqual([{ tool_id: 'toolu_chat', chat_text: 'why not both?' }]);
    expect(h.replyPosts).toHaveLength(0);
    // Chat mode left (the next refetch resolves the dialog server-side).
    expect(container.querySelector('.chat-hold')).toBeNull();
  });

  it('Chat about this: a refused Send keeps the text and the hold, surfacing the banner', async () => {
    withPending(flatSingle('toolu_stale'));
    await mountChat();
    const base = globalThis.fetch;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown, init?: RequestInit) => {
        if (String(input) === `/api/v1/runs/${RUN_ID}/answer` && init?.method === 'POST') {
          h.answerPosts.push(JSON.parse(String(init.body)) as Record<string, unknown>);
          return Promise.resolve(jsonResponse(409, { error: 'dialog is no longer pending' }));
        }
        return (base as typeof fetch)(input as RequestInfo, init);
      }),
    );

    buttonByText('Chat about this')!.click();
    await settle();
    const box = container.querySelector<HTMLTextAreaElement>('.chat-hold textarea')!;
    box.value = 'explain the options';
    box.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    container.querySelector<HTMLButtonElement>('.chat-hold .chat-send')!.click();
    await settle();

    expect(h.answerPosts).toEqual([{ tool_id: 'toolu_stale', chat_text: 'explain the options' }]);
    expect(container.querySelector('.banner.error')?.textContent).toContain(
      'dialog is no longer pending',
    );
    expect(container.querySelector<HTMLTextAreaElement>('.chat-hold textarea')?.value).toBe(
      'explain the options',
    );
  });

  it('renders a transcript-flushed question once: the marker at its position, the dock fed by the field', async () => {
    h.messagesOnServer = {
      messages: [
        {
          seq: 1,
          kind: 'dialog',
          dialog: { ...flatSingle('toolu_dup'), options: [{ label: 'Revert' }] },
        },
        { seq: 2, kind: 'text', role: 'assistant', text: 'context after' },
      ],
      state: 'question',
      cursor: 2,
      has_more: false,
      transcript: 'available',
      pending_dialog: flatSingle('toolu_dup'),
    };
    await mountChat();

    const markers = container.querySelectorAll('.chat-stream .chat-dialog-asking');
    expect(markers).toHaveLength(1);
    expect(markers[0]!.nextElementSibling?.textContent).toContain('context after');
    expect(container.querySelector('.chat-dialog-inline')).toBeNull();
    expect(container.querySelector('.chat-dialog-card')).toBeNull();
    expect(container.querySelectorAll('.chat-qpanel')).toHaveLength(1);
    // The richer field copy (description, free-text row) feeds the dock.
    expect(option('Revert').querySelector('.dialog-option-desc')?.textContent).toBe(
      'Roll back the change',
    );
    expect(answerInput()).not.toBeNull();
  });

  it('docks a question found by the messages-scan fallback (state question, no field)', async () => {
    const msg: ChatMessage = { seq: 1, kind: 'dialog', dialog: flatSingle('toolu_scan') };
    h.messagesOnServer = {
      messages: [msg],
      state: 'question',
      cursor: 1,
      has_more: false,
      transcript: 'available',
    };
    await mountChat();

    expect(container.querySelector('.chat-dialog-asking')).not.toBeNull();
    option('Patch forward').click();
    await settle();
    confirmBtn()!.click();
    await settle();
    expect(h.answerPosts).toEqual([{ tool_id: 'toolu_scan', index: 1 }]);
  });

  it('brings the stream bottom (above the dock) into view when a question arrives, never scrollIntoView', async () => {
    const calls: unknown[] = [];
    const proto = Element.prototype as unknown as { scrollIntoView?: (arg?: unknown) => void };
    proto.scrollIntoView = vi.fn((arg?: unknown) => calls.push(arg));
    try {
      await mountChat();
      const stream = container.querySelector('.chat-stream') as HTMLElement;
      const writes: number[] = [];
      Object.defineProperty(stream, 'scrollHeight', { value: 1000, configurable: true });
      Object.defineProperty(stream, 'clientHeight', { value: 1000, configurable: true });
      Object.defineProperty(stream, 'scrollTop', {
        configurable: true,
        get: () => 0,
        set: (v: number) => writes.push(v),
      });

      h.messagesOnServer = {
        ...h.messagesOnServer,
        state: 'question',
        pending_dialog: flatSingle('toolu_arrive'),
      };
      await emitMessagesChangedSettled();

      expect(panel()).not.toBeNull();
      expect(calls).toHaveLength(0);
      expect(writes[writes.length - 1]).toBe(1000);
    } finally {
      delete proto.scrollIntoView;
    }
  });

  it('returns the status line and the normal composer — with its compose-ahead draft — once the question resolves', async () => {
    h.messagesOnServer = { ...h.messagesOnServer, state: 'working' };
    await mountChat();
    const draft = container.querySelector('.chat-composer-row .chat-input') as HTMLTextAreaElement;
    draft.value = 'draft thought';
    draft.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();

    h.messagesOnServer = {
      ...h.messagesOnServer,
      state: 'question',
      pending_dialog: flatSingle(),
    };
    await emitMessagesChangedSettled();
    expect(panel()).not.toBeNull();
    expect(container.querySelector('.chat-status')).toBeNull();

    h.messagesOnServer = { ...h.messagesOnServer, state: 'working', pending_dialog: null };
    await emitMessagesChangedSettled();
    expect(panel()).toBeNull();
    expect(container.querySelector('.chat-dialog-asking')).toBeNull();
    expect(container.querySelector('.chat-status')?.textContent).toContain('Working');
    expect(
      (container.querySelector('.chat-composer-row .chat-input') as HTMLTextAreaElement).value,
    ).toBe('draft thought');
  });

  it('names the agent from the provider display name everywhere in the dock', async () => {
    h.providersOnServer[0]!.display_name = 'Agent Zed';
    withPending(flatSingle());
    await mountChat();

    expect(container.querySelector('.chat-dialog-asking')?.textContent).toBe(
      'Agent Zed is asking a question. Answer below.',
    );
    expect(panel()!.getAttribute('aria-label')).toBe('Agent Zed is asking');
    buttonByText('Chat about this')!.click();
    await settle();
    expect(container.querySelector('.chat-hold-note')?.textContent).toContain(
      'Agent Zed asks again after it has replied.',
    );
    expect(container.textContent).not.toContain('Claude');
  });
});
