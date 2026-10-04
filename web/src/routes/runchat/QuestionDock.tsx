// The docked question panel (issue #58 §3 + §4): an answerable dialog of kind
// question — flat single-select, flat multi-select, or multi-question — is
// answered ABOVE the composer instead of in an interactive stream card (issue
// #56's card stays for plan reviews and non-answerable dialogs). The stream
// marks the dialog's position with one lifecycle-style line ("{agent} is
// asking a question. Answer below."), and the dock owns everything else:
//
// - Panel: attention dot, "{agent} is asking", the question count, and a
//   chevron that folds the panel to its header line. Options are full-width
//   rows (radios for single-select, checkboxes for multi-select) and picking
//   never submits. An option labeled "… (Recommended)" is preselected (single-
//   select: the first one; multi-select: each) and wears a "Recommended" tag
//   instead of the suffix.
// - The synthesized free-text row (is_other) is never drawn as an option: the
//   composer's text box IS that row — single line, because free-text answers
//   are validated single-line server-side — and it is absent for a question
//   with no such row. Single-select: typing clears the pick and picking clears
//   the text. Multi-select: the text adds to the ticks and rides other_text.
// - Multi-question: one question at a time behind a chip stepper ("Next" /
//   "Review"), a per-question text box, and a Review step whose single "Send
//   N answers" posts the positional answers[] exactly like issue #51's form.
// - Chat about this (§4): one tap, no text. It is the provider picker's own
//   "Chat about this" choice on the question on screen: the server enters the
//   answers given to the EARLIER questions exactly as picked here, then
//   chooses "Chat about this" on this one ({tool_id, chat:true}, or
//   {tool_id, answers:[…earlier answers, {chat:true}]} — one action under the
//   answer endpoint's stale-dialog guard). The agent then asks what the
//   operator wants to know, and they reply in the ordinary composer. On a
//   multi-question dialog it needs every earlier question answered — that is
//   the path the provider's own form walks.
//
// Drafts are keyed to the dialog's IDENTITY (tool_id), memoized exactly like
// the old DialogPanel's dialogIdentity: a refetch hands in a fresh dialog
// object for the same pending dialog on every SSE tick, and only a genuinely
// new tool_id may drop the operator's picks, typed text, current question or
// fold.
//
// Answer encoding is the wire contract provider.go's DialogAnswer /
// QuestionAnswer define (compat §7): a single-select answer is `index` (the
// is_other row's index + `other_text` for typed text); a multi-select answer
// is `selected` — the REAL ticked indices ascending, never the is_other index
// (its text IS its toggle) — plus `other_text` when typed. A multi-question
// dialog sends one entry per question, positionally — or, for Chat about
// this, the entries of the questions before it and a closing `{chat: true}`.

import {
  Index,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createSignal,
  createUniqueId,
  on,
} from 'solid-js';
import {
  answerRun,
  errorMessage,
  type AnswerRequest,
  type Dialog,
  type DialogOption,
  type QuestionAnswer,
} from '../../api';
import Icon from '../../components/Icon';
import { capitalize } from './shared';

/**
 * True for the dialogs the dock answers: kind question AND answerable. Plan
 * reviews and non-answerable dialogs keep the in-stream DialogCard and the
 * composer's waiting note (issue #58: docking them is out of scope).
 */
export function docksQuestion(d: Dialog | null | undefined): d is Dialog {
  return d != null && d.dialog_kind === 'question' && d.answerable;
}

/** How many questions the dialog asks (a flat dialog asks one). */
export function questionCount(d: Dialog): number {
  const n = d.questions?.length ?? 0;
  return n > 0 ? n : 1;
}

/** The stream's marker line at the docked dialog's position. */
export function askingLine(agentName: string, d: Dialog): string {
  const n = questionCount(d);
  return `${capitalize(agentName)} is asking ${n === 1 ? 'a question' : `${n} questions`}. Answer below.`;
}

// "(Recommended)" at the very end of a label, case-insensitive, trailing
// whitespace tolerated — the provider's own convention for its suggestion.
const RECOMMENDED = /\s*\(recommended\)\s*$/i;

/** Whether an option is the provider's recommendation. */
export function isRecommended(label: string): boolean {
  return RECOMMENDED.test(label);
}

/** The label as shown: the "(Recommended)" suffix becomes a tag instead. */
export function displayLabel(label: string): string {
  return label.replace(RECOMMENDED, '');
}

/** One question as the dock renders it — a flat dialog is a one-question list. */
interface DockQuestion {
  header?: string;
  text: string;
  options: DialogOption[];
  multi: boolean;
}

function dockQuestions(d: Dialog): DockQuestion[] {
  const qs = d.questions ?? [];
  if (qs.length > 0) {
    return qs.map((q) => ({
      header: q.header,
      text: q.text,
      options: q.options,
      multi: q.multi_select === true,
    }));
  }
  return [{ text: d.prompt, options: d.options ?? [], multi: d.multi === true }];
}

/** The recommended preselection: the first recommended row, or every one on multi-select. */
function recommendedPicks(q: DockQuestion): number[] {
  const rec = q.options.flatMap((o, i) =>
    o.is_other !== true && isRecommended(o.label) ? [i] : [],
  );
  return q.multi ? rec : rec.slice(0, 1);
}

/** A question's chip / Review name: its header, else "Question N". */
const stepName = (q: DockQuestion, i: number) =>
  q.header !== undefined && q.header !== '' ? q.header : `Question ${i + 1}`;

export function QuestionDock(props: {
  runID: string;
  /** The pending dialog — always one docksQuestion() accepted. */
  dialog: Dialog;
  /** The provider's display name ('the agent' while metadata loads). */
  agentName: string;
  onError: (message: string) => void;
  /** After every answer / chat POST, successful or not: refetch the stream. */
  onAnswered: () => void;
}) {
  const uid = createUniqueId();
  const agent = () => capitalize(props.agentName);
  const questions = createMemo(() => dockQuestions(props.dialog));
  const multiQ = () => (props.dialog.questions?.length ?? 0) > 0;
  const total = () => questions().length;
  const plural = () => total() > 1;

  // --- Drafts (all keyed to the dialog identity) ---------------------------
  // `cur` is the question on screen; on a multi-question dialog cur === total
  // is the Review step. Picks and texts are per question index; a question
  // with no stored picks falls back to its recommended preselection, so the
  // default needs no initialization pass and resets for free.
  const [cur, setCur] = createSignal(0);
  const [picks, setPicks] = createSignal<ReadonlyMap<number, readonly number[]>>(new Map());
  const [texts, setTexts] = createSignal<ReadonlyMap<number, string>>(new Map());
  const [left, setLeft] = createSignal<ReadonlySet<number>>(new Set()); // questions navigated away from
  const [folded, setFolded] = createSignal(false);
  const [busy, setBusy] = createSignal(false);

  const identity = createMemo(() => props.dialog.tool_id);
  createEffect(
    on(
      identity,
      () => {
        setCur(0);
        setPicks(new Map());
        setTexts(new Map());
        setLeft(new Set<number>());
        setFolded(false);
      },
      { defer: true },
    ),
  );

  const question = (i: number): DockQuestion | undefined => questions()[i];
  const onReview = () => multiQ() && cur() >= total();
  const picked = (i: number): readonly number[] => {
    const stored = picks().get(i);
    if (stored !== undefined) return stored;
    const q = question(i);
    return q === undefined ? [] : recommendedPicks(q);
  };
  const text = (i: number) => texts().get(i) ?? '';
  const otherIdx = (i: number) => question(i)?.options.findIndex((o) => o.is_other === true) ?? -1;
  const hasOther = (i: number) => otherIdx(i) >= 0;
  const typed = (i: number) => (hasOther(i) ? text(i).trim() : '');
  const answered = (i: number) => picked(i).length > 0 || typed(i) !== '';
  const allAnswered = () => questions().every((_, i) => answered(i));
  // A chip ticks once its question has an answer AND has been left.
  const done = (i: number) => left().has(i) && answered(i);

  const pick = (qi: number, oi: number) => {
    const q = question(qi);
    if (q === undefined) return;
    if (q.multi) {
      const now = picked(qi);
      const next = now.includes(oi)
        ? now.filter((x) => x !== oi)
        : [...now, oi].sort((a, b) => a - b);
      setPicks((prev) => new Map(prev).set(qi, next));
      return;
    }
    // Single-select: picking replaces the pick AND clears the typed answer —
    // the two are alternative answers to the same question.
    setPicks((prev) => new Map(prev).set(qi, [oi]));
    setTexts((prev) => new Map(prev).set(qi, ''));
  };
  const type = (qi: number, value: string) => {
    setTexts((prev) => new Map(prev).set(qi, value));
    // Single-select: typed text replaces the pick (multi-select adds to it).
    if (question(qi)?.multi !== true && value.trim() !== '') {
      setPicks((prev) => new Map(prev).set(qi, []));
    }
  };

  // One question's wire answer (the flat payload for a single question, one
  // positional entry of answers[] otherwise).
  const answerFor = (qi: number): QuestionAnswer => {
    const t = typed(qi);
    if (question(qi)?.multi === true) {
      const a: QuestionAnswer = {
        selected: picked(qi)
          .filter((i) => i !== otherIdx(qi))
          .sort((x, y) => x - y),
      };
      if (t !== '') a.other_text = t;
      return a;
    }
    const p = picked(qi)[0];
    if (p !== undefined) return { index: p };
    return { index: otherIdx(qi), other_text: t };
  };

  const post = async (req: Omit<AnswerRequest, 'tool_id'>): Promise<boolean> => {
    if (busy()) return false;
    setBusy(true);
    try {
      await answerRun(props.runID, { tool_id: props.dialog.tool_id, ...req });
      return true;
    } catch (err) {
      // A stale tool_id (409) is just the banner: the refetch below brings
      // whatever is pending now.
      props.onError(errorMessage(err));
      return false;
    } finally {
      setBusy(false);
      props.onAnswered();
    }
  };

  // --- Navigation ----------------------------------------------------------
  let stepsEl: HTMLDivElement | undefined;
  const go = (i: number) => {
    const from = cur();
    if (from < total()) setLeft((prev) => new Set(prev).add(from));
    setCur(i);
  };
  // Keep the current chip in view when the row overflows (it scrolls
  // horizontally inside the panel — never the page).
  createEffect(() => {
    const i = cur();
    const strip = stepsEl;
    if (strip === undefined) return;
    const chip = strip.children[i] as HTMLElement | undefined;
    if (chip !== undefined) strip.scrollLeft = Math.max(0, chip.offsetLeft - strip.offsetLeft - 40);
  });

  const confirmLabel = () => (!multiQ() ? 'Answer' : cur() === total() - 1 ? 'Review' : 'Next');
  const canConfirm = () => !busy() && answered(cur());
  const confirm = () => {
    if (!canConfirm()) return;
    if (multiQ()) go(cur() + 1);
    else void post(answerFor(0));
  };
  const sendAll = () => {
    if (busy() || !allAnswered()) return;
    void post({ answers: questions().map((_, i) => answerFor(i)) });
  };

  // Enter in the single-line answer box is exactly the confirm button (the
  // issue #165 rule for free-text rows): same guard, same action — never a
  // shortcut around it. Composing IME text never confirms.
  const onAnswerKey = (e: KeyboardEvent) => {
    if (e.key !== 'Enter' || e.isComposing || e.keyCode === 229) return;
    e.preventDefault();
    confirm();
  };

  // --- Chat about this (§4) ------------------------------------------------
  // One tap, no text: the provider picker's own "Chat about this" choice on
  // the question on screen. The request is the operator's path through the
  // form EXACTLY as they walked it here — the answers to the questions before
  // this one, then the chat choice — because that is what the server replays
  // on the provider's picker, and how the agent learns which question the
  // chat is about ("second option on the first question, chat about the
  // second"). Answers drafted for LATER questions are not sent: choosing the
  // row ends the form. It therefore needs every earlier question answered.
  const earlierAnswered = () => questions().every((_, i) => i >= cur() || answered(i));
  const canChat = () => !busy() && earlierAnswered();
  const chatAbout = () => {
    if (!canChat()) return;
    if (!multiQ()) {
      void post({ chat: true });
      return;
    }
    const earlier = questions()
      .slice(0, cur())
      .map((_, i) => answerFor(i));
    void post({ answers: [...earlier, { chat: true }] });
  };

  const bodyId = `chat-qpanel-${uid}`;
  const qTextId = `chat-qtext-${uid}`;
  const chatHintId = `chat-qchat-hint-${uid}`;
  const current = () => question(cur());
  // The current question's selectable rows with their ORIGINAL indices (the
  // adapter drives picker rows by index); the is_other row is the text box.
  const rows = createMemo(() =>
    (current()?.options ?? [])
      .map((option, index) => ({ option, index }))
      .filter((r) => r.option.is_other !== true),
  );
  // Review: picked labels, then typed text in quotes — the answered-dialog
  // summary's wording for the same answer.
  const summary = (i: number) => {
    const q = question(i);
    const labels = picked(i).map((oi) => displayLabel(q?.options[oi]?.label ?? ''));
    const t = typed(i);
    if (t !== '') labels.push(`“${t}”`);
    return labels.join(', ');
  };

  return (
    <div class="chat-qdock">
      <section
        class="chat-qpanel"
        classList={{ folded: folded() }}
        aria-label={`${agent()} is asking`}
      >
        <div class="chat-qpanel-head">
          <span class="chat-qpanel-dot" aria-hidden="true" />
          <b class="chat-qpanel-title">{agent()} is asking</b>
          <span class="chat-qpanel-count">
            {total()} {plural() ? 'questions' : 'question'}
          </span>
          {/* The fold: the panel collapses to this header line so the
              stream can be read; it reopens on tap and resets with the
              dialog. */}
          <button
            type="button"
            class="icon-btn chat-qpanel-fold"
            aria-label={plural() ? 'Questions' : 'Question'}
            aria-expanded={!folded()}
            aria-controls={bodyId}
            onClick={() => setFolded((f) => !f)}
          >
            <Icon name="chevron-down" size={18} />
          </button>
        </div>
        <div class="chat-qpanel-body" id={bodyId} hidden={folded()}>
          {/* Multi-question stepper: one chip per question (its header, else
              "Question N"), then Review. Any chip is tappable in any order;
              the row scrolls sideways when it overflows. */}
          <Show when={multiQ()}>
            <div class="chat-qsteps" role="group" aria-label="Questions" ref={stepsEl}>
              <Index each={questions()}>
                {(q, i) => (
                  <button
                    type="button"
                    class="chat-qstep"
                    classList={{ current: cur() === i, done: done(i) }}
                    aria-current={cur() === i ? 'step' : undefined}
                    aria-label={`${stepName(q(), i)}${done(i) ? ', answered' : ''}`}
                    onClick={() => go(i)}
                  >
                    {/* The tick replaces the number — a shape change, not
                        only a colour one. */}
                    <span class="chat-qstep-mark" aria-hidden="true">
                      <Show when={done(i)} fallback={i + 1}>
                        <Icon name="check" size={12} />
                      </Show>
                    </span>
                    <span class="chat-qstep-name">{stepName(q(), i)}</span>
                  </button>
                )}
              </Index>
              <button
                type="button"
                class="chat-qstep review"
                classList={{ current: onReview() }}
                aria-current={onReview() ? 'step' : undefined}
                onClick={() => go(total())}
              >
                Review
              </button>
            </div>
          </Show>
          {/* The option area scrolls inside the panel (CSS caps it near half
              the viewport on phones) so the header and composer stay put. */}
          <div class="chat-qpanel-scroll">
            <Show
              when={!onReview()}
              fallback={
                <>
                  <p class="chat-qpanel-q">Check your answers</p>
                  <ul class="chat-qreview">
                    <Index each={questions()}>
                      {(q, i) => (
                        <li>
                          <button type="button" class="chat-qreview-row" onClick={() => go(i)}>
                            <span class="chat-qreview-head">{stepName(q(), i)}</span>
                            <span class="chat-qreview-answer" classList={{ missing: !answered(i) }}>
                              {answered(i) ? summary(i) : 'Not answered yet'}
                            </span>
                            <Icon name="pencil" size={14} class="chat-qreview-edit" />
                          </button>
                        </li>
                      )}
                    </Index>
                  </ul>
                </>
              }
            >
              <p class="chat-qpanel-q" id={qTextId}>
                {current()?.text}
                <Show when={current()?.multi}>
                  <small class="chat-qpanel-hint">Pick any that apply.</small>
                </Show>
              </p>
              <div
                class="chat-qopts"
                role={current()?.multi ? 'group' : 'radiogroup'}
                aria-labelledby={qTextId}
              >
                <Index each={rows()}>
                  {(row) => (
                    <OptionRow
                      option={row().option}
                      multi={current()?.multi === true}
                      checked={picked(cur()).includes(row().index)}
                      disabled={busy()}
                      onPick={() => pick(cur(), row().index)}
                    />
                  )}
                </Index>
              </div>
              {/* Chat about this: one tap chooses the provider picker's own
                  row on THIS question — no text; the agent asks what the
                  operator wants to know. Off until every earlier question of
                  a multi-question dialog has an answer (the server enters
                  those first), with the reason spelled out beneath. */}
              <button
                type="button"
                class="chat-qpanel-chat"
                classList={{ busy: busy() }}
                title={`Talk about this question first — ${agent()} asks what you want to know`}
                aria-describedby={earlierAnswered() ? undefined : chatHintId}
                disabled={!canChat()}
                onClick={chatAbout}
              >
                <Icon name="message-square" size={16} />
                <span>Chat about this</span>
              </button>
              <Show when={!earlierAnswered()}>
                <p class="chat-qpanel-chat-hint" id={chatHintId}>
                  Answer the earlier questions first.
                </p>
              </Show>
            </Show>
          </div>
        </div>
      </section>

      <Switch>
        {/* Review: the text box gives way to the one atomic send. */}
        <Match when={onReview()}>
          <button
            type="button"
            class="chat-answer-confirm chat-answer-all"
            classList={{ busy: busy() }}
            disabled={busy() || !allAnswered()}
            onClick={sendAll}
          >
            Send {total()} answers
          </button>
        </Match>
        {/* Answer mode: the text box is the question's free-text row (absent
            when it has none) and Send became a labeled confirm. */}
        <Match when={true}>
          <div class="chat-answer-row" classList={{ 'no-text': !hasOther(cur()) }}>
            <Show when={hasOther(cur())}>
              <input
                type="text"
                class="chat-answer-input"
                placeholder="Or type your own answer…"
                aria-label={
                  multiQ() ? `Your own answer to: ${current()?.text ?? ''}` : 'Your own answer'
                }
                autocomplete="off"
                value={text(cur())}
                onInput={(e) => type(cur(), e.currentTarget.value)}
                onKeyDown={onAnswerKey}
              />
            </Show>
            <button
              type="button"
              class="chat-answer-confirm"
              classList={{ busy: busy() }}
              disabled={!canConfirm()}
              onClick={confirm}
            >
              <span>{confirmLabel()}</span>
              <Icon name={multiQ() ? 'chevron-right' : 'send'} size={16} />
            </button>
          </div>
        </Match>
      </Switch>
    </div>
  );
}

/**
 * One option row: a full-width radio (single-select) or checkbox (multi-
 * select) carrying the label — with the "Recommended" tag in place of the
 * suffix — over its always-visible description. The mark changes SHAPE when
 * picked (a filled dot, a ticked box), so the state never rides colour alone.
 */
function OptionRow(props: {
  option: DialogOption;
  multi: boolean;
  checked: boolean;
  disabled: boolean;
  onPick: () => void;
}) {
  return (
    <button
      type="button"
      role={props.multi ? 'checkbox' : 'radio'}
      aria-checked={props.checked}
      class="chat-qopt"
      classList={{ picked: props.checked, multi: props.multi }}
      disabled={props.disabled}
      onClick={() => props.onPick()}
    >
      <span class="chat-qopt-mark" aria-hidden="true">
        <Show when={props.multi && props.checked}>
          <Icon name="check" size={12} />
        </Show>
      </span>
      <span class="chat-qopt-body">
        <span class="dialog-option-label">
          <span class="chat-qopt-label">{displayLabel(props.option.label)}</span>
          <Show when={isRecommended(props.option.label)}>
            {' '}
            <span class="chat-qopt-tag">Recommended</span>
          </Show>
        </span>
        <Show when={props.option.description}>
          <span class="dialog-option-desc">{props.option.description}</span>
        </Show>
      </span>
    </button>
  );
}
