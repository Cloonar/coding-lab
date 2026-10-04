// The in-stream dialog surfaces (ADR-0020, issue #56): the pending dialog's
// interactive stream card for plan reviews, approvals and non-answerable
// dialogs, plus the compact answered Q→A summaries that stay in history
// (issue #56 decision 3). Answerable QUESTION dialogs — flat single-select,
// flat multi-select and the multi-question form (issue #51 decision 3) — no
// longer answer here: they dock above the composer (issue #58 §3,
// QuestionDock.tsx), and the stream only marks their position.

import { For, Match, Show, Switch, createEffect, createMemo, createSignal, on } from 'solid-js';
import {
  answerRun,
  errorMessage,
  type Dialog,
  type DialogOption,
  type QuestionResult,
} from '../../api';
import { Markdown } from './Markdown';

// The pending dialog as a full-width stream card (issue #56 decision 1): the
// interactive DialogPanel wrapped in house card chrome, rendered inside
// .chat-stream — at its matching transcript message's position when one
// exists, else appended after the last item. cardRef hands the element up for
// the arrival scroll (decision 5). Since issue #58 only plan reviews,
// approvals and non-answerable dialogs reach it (docksQuestion() routes the
// rest to the composer dock).
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

// Enter-to-submit for a card's free-text "Other" input (issue #165): Enter is
// exactly equivalent to clicking the adjacent Send button — same enabled
// guard, same action — never a shortcut around it. isComposing is guarded so
// committing IME (CJK) composition never fires an early submit. (The docked
// question panel applies the same rule to its own answer box, issue #58.)
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
  const [otherText, setOtherText] = createSignal('');

  // The typed text is keyed to the dialog's identity: if the pending dialog
  // changes while the card stays mounted, a half-typed answer must not carry
  // over to the new dialog. The identity is MEMOIZED because every refetch
  // delivers a fresh pending_dialog object for the same dialog, and `on` alone
  // re-runs on any upstream write — without the equality-gating memo, each SSE
  // tick wiped the operator's half-typed text.
  const dialogIdentity = createMemo(() => props.dialog.tool_id);
  createEffect(on(dialogIdentity, () => setOtherText(''), { defer: true }));

  const options = () => props.dialog.options ?? [];
  const answer = async (payload: { index: number; other_text?: string }) => {
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
        {/* Flat single-select: one button per option answers on tap; the
            free-text row (is_other — a plan's "reject with feedback") takes
            text plus its own Send. Plan reviews and the generic 'approval'
            kind land here; answerable QUESTION dialogs never do — they dock
            above the composer (issue #58 §3, QuestionDock.tsx). */}
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
                          () => void answer({ index: i(), other_text: otherText().trim() }),
                        )}
                      />
                      <button
                        type="button"
                        class="chat-send"
                        disabled={busy() || otherText().trim() === ''}
                        onClick={() => void answer({ index: i(), other_text: otherText().trim() })}
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
      </Show>
    </div>
  );
}

/**
 * One option's face — bold label over the ALWAYS-visible muted description
 * (issue #56 decision 7), so plan approve/reject rows and approval options
 * read like the docked question rows (issue #58).
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
