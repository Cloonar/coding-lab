// The dialog surfaces (ADR-0020, issue #56): the pending dialog's interactive
// stream card and its answer panel — single/multi-select option cards, plan
// review, the multi-question form (issue #51 decision 3) — plus the compact
// answered Q→A summaries that stay in history (issue #56 decision 3).

import { For, Match, Show, Switch, createEffect, createMemo, createSignal, on } from 'solid-js';
import {
  answerRun,
  errorMessage,
  type Dialog,
  type DialogOption,
  type Question,
  type QuestionAnswer,
  type QuestionResult,
} from '../../api';
import Icon from '../../components/Icon';
import { Markdown } from './Markdown';

// The pending dialog as a full-width stream card (issue #56 decision 1): the
// interactive DialogPanel wrapped in house card chrome, rendered inside
// .chat-stream — at its matching transcript message's position when one
// exists, else appended after the last item. cardRef hands the element up for
// the arrival scroll (decision 5).
export function DialogCard(props: {
  runID: string;
  dialog: Dialog;
  openHint: string;
  onError: (message: string) => void;
  onAnswered: () => void;
  cardRef: (el: HTMLDivElement) => void;
}) {
  return (
    <div class="chat-dialog-card" ref={(el) => props.cardRef(el)}>
      <DialogPanel
        runID={props.runID}
        dialog={props.dialog}
        openHint={props.openHint}
        onError={props.onError}
        onAnswered={props.onAnswered}
      />
    </div>
  );
}

/**
 * An answered dialog message in history (issue #56 decision 3): a compact,
 * inert Q→A summary — visibly quieter than the interactive card, no buttons,
 * no unchosen options. The caller guards on outcome PRESENCE (the answered
 * signal); this component never re-derives it from inner fields.
 */
export function AnsweredDialog(props: { dialog: Dialog }) {
  const outcome = () => props.dialog.outcome ?? {};
  const results = () => outcome().results ?? [];
  // Question texts for the renders that have no per-question answers to show
  // (dismissed, or an empty outcome): the outcome's own texts when recorded,
  // else the dialog's questions, else the prompt.
  const questionTexts = () => {
    if (results().length > 0) return results().map((r) => r.question);
    const questions = props.dialog.questions ?? [];
    if (questions.length > 0) return questions.map((q) => q.text);
    return [props.dialog.prompt];
  };
  const planMarker = () =>
    outcome().dismissed === true
      ? 'Plan dismissed'
      : outcome().approved === true
        ? 'Plan approved'
        : 'Plan rejected';
  return (
    <div class="chat-dialog-answered">
      <Switch>
        {/* Plan review: the FULL plan markdown stays readable in history
            (same .chat-dialog-plan render as the live card), followed by a
            one-line resolution marker; typed rejection feedback reads as an
            operator quote. */}
        <Match when={props.dialog.dialog_kind === 'plan'}>
          <div class="chat-dialog-plan">
            <Markdown source={props.dialog.prompt} />
          </div>
          <p class="chat-dialog-outcome">{planMarker()}</p>
          <Show when={outcome().dismissed !== true && outcome().approved !== true}>
            <Show when={outcome().feedback}>
              {(f) => (
                <p class="dialog-qa-answer">
                  <span class="dialog-qa-other">“{f()}”</span>
                </p>
              )}
            </Show>
          </Show>
        </Match>
        {/* Dismissed question/approval: the question text(s) with ONE
            dismissed marker — there is no answer to show. */}
        <Match when={outcome().dismissed === true}>
          <For each={questionTexts()}>{(text) => <p class="dialog-qa-question">{text}</p>}</For>
          <p class="chat-dialog-outcome">Dismissed</p>
        </Match>
        {/* Question kind (and the reserved approval kind): one Q→A pair per
            recorded result, dialog order. */}
        <Match when={results().length > 0}>
          <For each={results()}>
            {(r) => (
              <div class="dialog-qa">
                <p class="dialog-qa-question">{r.question}</p>
                <AnswerLine result={r} />
              </div>
            )}
          </For>
        </Match>
        {/* Answered but nothing recorded (an empty, non-dismissed outcome on
            a question) — mark the questions unanswered rather than vanish. */}
        <Match when={true}>
          <For each={questionTexts()}>
            {(text) => (
              <div class="dialog-qa">
                <p class="dialog-qa-question">{text}</p>
                <p class="dialog-qa-answer">
                  <span class="dialog-qa-none">No answer recorded</span>
                </p>
              </div>
            )}
          </For>
        </Match>
      </Switch>
    </div>
  );
}

/**
 * One recorded answer line: the chosen labels joined ", " (recorded toggle
 * order), then any typed Other text as a quoted span — visually distinct
 * from a listed label; a multi-select result can carry both. Neither means
 * that question got no recorded answer → the unanswered marker.
 */
function AnswerLine(props: { result: QuestionResult }) {
  const chosen = () => props.result.chosen ?? [];
  const other = () => props.result.other_text ?? '';
  return (
    <p class="dialog-qa-answer">
      <Show when={chosen().length > 0}>
        <span class="dialog-qa-chosen">{chosen().join(', ')}</span>
      </Show>
      <Show when={other() !== ''}>
        <span class="dialog-qa-other">“{other()}”</span>
      </Show>
      <Show when={chosen().length === 0 && other() === ''}>
        <span class="dialog-qa-none">No answer recorded</span>
      </Show>
    </p>
  );
}

// Enter-to-submit for a dialog's free-text "Other" input (issue #165): one
// rule shared by all three Other inputs (single-select, flat multi-select,
// multi-question form) so Enter is exactly equivalent to clicking the
// adjacent Send/Submit button — same enabled guard, same action — never a
// shortcut around it. isComposing is guarded so committing IME (CJK)
// composition never fires an early submit.
function submitOnEnter(canSubmit: () => boolean, submit: () => void) {
  return (e: KeyboardEvent) => {
    if (e.key !== 'Enter' || e.isComposing) return;
    if (!canSubmit()) return;
    e.preventDefault();
    submit();
  };
}

function DialogPanel(props: {
  runID: string;
  dialog: Dialog;
  /** "open it at <host>" / "open the session" — for the degraded note. */
  openHint: string;
  onError: (message: string) => void;
  onAnswered: () => void;
}) {
  const [busy, setBusy] = createSignal(false);
  const [selected, setSelected] = createSignal<number[]>([]);
  const [otherText, setOtherText] = createSignal('');

  // Selection state is keyed to the dialog's identity: if the pending dialog
  // changes while the panel is mounted, stale picks must not carry over and
  // answer the new dialog. The identity is MEMOIZED because every refetch
  // delivers a fresh pending_dialog object for the same dialog, and `on` alone
  // re-runs on any upstream write — without the equality-gating memo, each SSE
  // tick wiped the operator's in-progress picks and half-typed Other text.
  const dialogIdentity = createMemo(() => props.dialog.tool_id);
  createEffect(
    on(
      dialogIdentity,
      () => {
        setSelected([]);
        setOtherText('');
      },
      { defer: true },
    ),
  );

  const options = () => props.dialog.options ?? [];
  const questions = () => props.dialog.questions ?? [];
  const answer = async (payload: {
    index?: number;
    selected?: number[];
    other_text?: string;
    answers?: QuestionAnswer[];
  }) => {
    setBusy(true);
    try {
      await answerRun(props.runID, { tool_id: props.dialog.tool_id, ...payload });
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
      props.onAnswered();
    }
  };
  const toggle = (i: number) =>
    setSelected((prev) => (prev.includes(i) ? prev.filter((x) => x !== i) : [...prev, i]));

  // Flat multi-select payload: real toggles in `selected` (ascending — the
  // adapter walks the picker downward), the Other row's text in `other_text`.
  // The Other INDEX stays out of `selected` (the adapter rejects it — the
  // free-text row's toggle IS its text, compat §7).
  const otherIdx = () => options().findIndex((opt) => opt.is_other === true);
  const otherOn = () => selected().includes(otherIdx()) && otherIdx() >= 0;
  const realSelected = () =>
    selected()
      .filter((i) => i !== otherIdx())
      .sort((a, b) => a - b);
  const multiAnswerReady = () => (otherOn() ? otherText().trim() !== '' : selected().length > 0);
  const multiPayload = () => {
    const payload: { selected: number[]; other_text?: string } = { selected: realSelected() };
    if (otherOn()) payload.other_text = otherText().trim();
    return payload;
  };

  return (
    <div class="chat-dialog">
      {/* A plan review's prompt IS the plan body — rendered as markdown in
          full (issue #56 decision 4): the chat pane is the only scrollbar, so
          approve/reject sit after the whole plan. Every other kind keeps the
          plain one-line prompt. */}
      <Show
        when={props.dialog.dialog_kind === 'plan'}
        fallback={<p class="chat-dialog-prompt">{props.dialog.prompt}</p>}
      >
        <div class="chat-dialog-plan">
          <Markdown source={props.dialog.prompt} />
        </div>
      </Show>
      <Show
        when={props.dialog.answerable}
        fallback={
          <p class="chat-composer-note">
            This dialog can't be answered here — {props.openHint} to respond.
          </p>
        }
      >
        <Switch>
          {/* Multi-question form (issue #51 decision 3): one atomic submit. */}
          <Match when={questions().length > 0}>
            <MultiQuestionForm
              questions={questions()}
              resetKey={props.dialog.tool_id}
              busy={busy()}
              onSubmit={(answers) => void answer({ answers })}
            />
          </Match>
          {/* Multi-select: toggle cards (issue #56 decision 7 — the former
              bare checkbox rows dropped the option descriptions) + a single
              confirm. The synthesized Other row toggles like the rest and
              opens a free-text input; its INDEX never enters the payload —
              the text IS its toggle, riding other_text (the adapter fills
              the TUI's "Type something" row with it, compat §7). */}
          <Match when={props.dialog.multi}>
            <ul class="dialog-options">
              <For each={options()}>
                {(opt, i) => (
                  <li>
                    <OptionToggle
                      option={opt}
                      pressed={selected().includes(i())}
                      disabled={busy()}
                      onToggle={() => toggle(i())}
                    />
                    <Show when={opt.is_other && selected().includes(i())}>
                      {/* eslint-disable solid/reactivity -- submitOnEnter's returned closure
                          only calls these thunks at keydown time, same as the plain onClick
                          handler below; the indirection through the helper just isn't visible
                          to the static check (issue #165). */}
                      <input
                        class="chat-input dialog-other-input"
                        placeholder="Type your answer…"
                        aria-label="Other — type your answer"
                        value={otherText()}
                        onInput={(e) => setOtherText(e.currentTarget.value)}
                        onKeyDown={submitOnEnter(
                          () => !busy() && multiAnswerReady(),
                          () => void answer(multiPayload()),
                        )}
                      />
                      {/* eslint-enable solid/reactivity */}
                    </Show>
                  </li>
                )}
              </For>
            </ul>
            <button
              type="button"
              class="chat-send"
              disabled={busy() || !multiAnswerReady()}
              onClick={() => void answer(multiPayload())}
            >
              Submit
            </button>
          </Match>
          {/* Single-select: one button per option; the Other row takes text.
              Plan approval and the generic 'approval' kind land here too —
              their pinned options answer as a flat single-select index. */}
          <Match when={true}>
            <ul class="dialog-options">
              <For each={options()}>
                {(opt, i) => (
                  <li>
                    <Show
                      when={!opt.is_other}
                      fallback={
                        <div class="dialog-other">
                          <input
                            class="chat-input"
                            placeholder="Other…"
                            aria-label="Other — type your answer"
                            value={otherText()}
                            onInput={(e) => setOtherText(e.currentTarget.value)}
                            onKeyDown={submitOnEnter(
                              () => !busy() && otherText().trim() !== '',
                              () => void answer({ index: i(), other_text: otherText() }),
                            )}
                          />
                          <button
                            type="button"
                            class="chat-send"
                            disabled={busy() || otherText().trim() === ''}
                            onClick={() => void answer({ index: i(), other_text: otherText() })}
                          >
                            Send
                          </button>
                        </div>
                      }
                    >
                      <button
                        type="button"
                        class="dialog-option"
                        disabled={busy()}
                        onClick={() => void answer({ index: i() })}
                      >
                        <OptionContent option={opt} />
                      </button>
                    </Show>
                  </li>
                )}
              </For>
            </ul>
          </Match>
        </Switch>
      </Show>
    </div>
  );
}

/**
 * One option's face — bold label over the ALWAYS-visible muted description
 * (issue #56 decision 7) — shared by every option-card shape so single-select
 * rows, multi-select toggles and plan approve/reject rows read alike. The
 * body wrapper takes the card's flexible column so a check indicator can ride
 * the row's trailing edge.
 */
function OptionContent(props: { option: DialogOption }) {
  return (
    <span class="dialog-option-body">
      <span class="dialog-option-label">{props.option.label}</span>
      <Show when={props.option.description}>
        <span class="dialog-option-desc">{props.option.description}</span>
      </Show>
    </span>
  );
}

/**
 * A toggleable option card (issue #56 decision 7): the shared card face plus
 * a pressed state — accent border, subtle tint and a check indicator —
 * announced via aria-pressed. Used by the flat multi-select path and the
 * multi-question form; tapping only toggles, submission stays with the
 * form's own Submit.
 */
function OptionToggle(props: {
  option: DialogOption;
  pressed: boolean;
  disabled: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      classList={{ 'dialog-option': true, selected: props.pressed }}
      aria-pressed={props.pressed}
      disabled={props.disabled}
      onClick={() => props.onToggle()}
    >
      <OptionContent option={props.option} />
      <Show when={props.pressed}>
        <span class="dialog-option-check" aria-hidden="true">
          <Icon name="check" size={16} />
        </span>
      </Show>
    </button>
  );
}

/**
 * The multi-question form (issue #51 decision 3), rendered the way the
 * provider's own app renders question forms: stacked questions in order, each
 * a small header chip + the question text + option buttons; multi_select
 * questions toggle; the synthesized "Other" row (is_other) opens a free-text
 * input when picked — for single-select AND multi-select questions. ONE
 * submit answers all questions atomically via answers[] — disabled until
 * every question is answered (a picked Other requires non-empty text).
 *
 * Answer encoding (provider.go QuestionAnswer is authoritative): association
 * is POSITIONAL — answers[i] answers questions[i]. Each entry mirrors the
 * flat single-question shape: a single-select question sends `index` = the
 * chosen OPTION index (+ `other_text` when that row is the is_other row); a
 * multi_select question sends `selected` = the toggled REAL option indices
 * ascending (`index` absent) + `other_text` when the Other row is toggled —
 * never the Other row's index (its text IS its toggle; the adapter fills the
 * TUI's free-text row from other_text, compat §7). The adapter drives real
 * picker rows from these fields, so this shape is a wire contract, not a UI
 * convenience.
 */
function MultiQuestionForm(props: {
  questions: Question[];
  /** The dialog identity (tool_id): selections reset when it changes. */
  resetKey: string;
  busy: boolean;
  onSubmit: (answers: QuestionAnswer[]) => void;
}) {
  const [picks, setPicks] = createSignal<ReadonlyMap<number, number[]>>(new Map());
  const [others, setOthers] = createSignal<ReadonlyMap<number, string>>(new Map());

  // Memoized like DialogPanel's dialogIdentity: resetKey re-evaluates on every
  // refetch (a fresh dialog object each response), and only a REAL identity
  // change may drop the operator's in-progress answers.
  const resetKey = createMemo(() => props.resetKey);
  createEffect(
    on(
      resetKey,
      () => {
        setPicks(new Map());
        setOthers(new Map());
      },
      { defer: true },
    ),
  );

  // Every option of a question with its ORIGINAL index (the adapter
  // navigates picker rows by index). The synthesized Other row renders for
  // multi-select questions too — toggling it opens the free-text input, and
  // its text rides other_text (the adapter fills the TUI's "Type something"
  // row, which pastes-and-checks in one move — compat §7, live 2026-07-09).
  const allOptions = (q: Question): { opt: DialogOption; idx: number }[] =>
    q.options.map((opt, idx) => ({ opt, idx }));

  const picked = (qi: number) => picks().get(qi) ?? [];
  const otherText = (qi: number) => others().get(qi) ?? '';
  const isPicked = (qi: number, oi: number) => picked(qi).includes(oi);
  const togglePick = (qi: number, oi: number, multi: boolean) =>
    setPicks((prev) => {
      const next = new Map(prev);
      const current = prev.get(qi) ?? [];
      if (multi) {
        next.set(
          qi,
          current.includes(oi)
            ? current.filter((x) => x !== oi)
            : [...current, oi].sort((a, b) => a - b),
        );
      } else {
        // Single select: picking replaces; re-picking clears.
        next.set(qi, current.includes(oi) ? [] : [oi]);
      }
      return next;
    });
  const setOther = (qi: number, value: string) => setOthers((prev) => new Map(prev).set(qi, value));

  const otherPicked = (qi: number, q: Question) =>
    picked(qi).some((oi) => q.options[oi]?.is_other === true);
  const questionAnswered = (qi: number, q: Question) =>
    picked(qi).length > 0 && (!otherPicked(qi, q) || otherText(qi).trim() !== '');
  const complete = () => props.questions.every((q, qi) => questionAnswered(qi, q));

  const submit = () => {
    if (!complete()) return;
    props.onSubmit(
      props.questions.map((q, qi): QuestionAnswer => {
        if (q.multi_select === true) {
          // Real toggles ride `selected` (ascending by construction); the
          // Other row's INDEX never does — its text IS its toggle
          // (other_text, compat §7).
          const a: QuestionAnswer = {
            selected: picked(qi).filter((oi) => q.options[oi]?.is_other !== true),
          };
          if (otherPicked(qi, q)) a.other_text = otherText(qi).trim();
          return a;
        }
        const idx = picked(qi)[0] ?? 0; // complete() guarantees exactly one pick
        const a: QuestionAnswer = { index: idx };
        if (q.options[idx]?.is_other === true) a.other_text = otherText(qi).trim();
        return a;
      }),
    );
  };

  return (
    <>
      <div class="dialog-questions">
        <For each={props.questions}>
          {(q, qi) => (
            <div class="dialog-question" role="group" aria-label={q.text}>
              <Show when={q.header}>
                <span class="chip dialog-question-header">{q.header}</span>
              </Show>
              <p class="chat-dialog-prompt">{q.text}</p>
              <ul class="dialog-options">
                <For each={allOptions(q)}>
                  {(entry) => (
                    <li>
                      <OptionToggle
                        option={entry.opt}
                        pressed={isPicked(qi(), entry.idx)}
                        disabled={props.busy}
                        onToggle={() => togglePick(qi(), entry.idx, q.multi_select === true)}
                      />
                      <Show when={entry.opt.is_other && isPicked(qi(), entry.idx)}>
                        {/* eslint-disable solid/reactivity -- submitOnEnter's returned closure
                            only calls these thunks at keydown time, same as the plain onClick
                            Submit handler below; the indirection through the helper just isn't
                            visible to the static check (issue #165). */}
                        <input
                          class="chat-input dialog-other-input"
                          placeholder="Type your answer…"
                          aria-label={`Other answer for: ${q.text}`}
                          value={otherText(qi())}
                          onInput={(e) => setOther(qi(), e.currentTarget.value)}
                          onKeyDown={submitOnEnter(() => !props.busy && complete(), submit)}
                        />
                        {/* eslint-enable solid/reactivity */}
                      </Show>
                    </li>
                  )}
                </For>
              </ul>
            </div>
          )}
        </For>
      </div>
      {/* ONE atomic submit for every question (issue #51 decision 3). */}
      <button type="button" class="chat-send" disabled={props.busy || !complete()} onClick={submit}>
        Submit
      </button>
    </>
  );
}
