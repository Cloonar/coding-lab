// The chat header's context meter and Run details surface (issue #58 §1,
// superseding ADR-0061's sub-640px hiding of the meter).
//
// - contextMeter / fmtTokens: the raw `ContextUsage` counts → the rounded
//   percentage, the amber (>=80%) / red (>=95%) tint band and the `127k of
//   200k tokens` string. The UI does the division (issue #243): the adapter
//   ships raw counts only.
// - ContextRing: the meter's glyph — an inline SVG ring filled to the
//   percentage, coloured by currentColor so the tint band reaches it. The
//   percentage text always rides beside it, so the state is never colour-only.
// - RunDetails: what the meter (or the `•••` menu's "Run details" entry)
//   opens — model and effort, context as `N of M tokens` with a bar, the run's
//   branch, and the base branch with its commits-behind count and a Pull base
//   action shown only while a live run is behind. ChatHeader picks the
//   container: a bottom sheet below 1024px, an anchored popover at and above.

import { Show, createUniqueId, type JSX } from 'solid-js';
import type { ContextUsage, Provider, Repo, Run } from '../../api';
import Icon from '../../components/Icon';

export interface ContextMeter {
  /** Rounded occupancy percentage — may exceed 100 when usage overshoots the limit. */
  pct: number;
  /** The tint band, keyed on the raw ratio (not the rounded percentage). */
  tint: 'warn' | 'danger' | '';
  /** `127k of 200k tokens`. */
  tokens: string;
}

/** fmtTokens renders a raw token count compactly (issue #243): 127432 →
 *  "127k"; a count below 1000 stays verbatim so a tiny value never rounds down
 *  to "0k". */
export function fmtTokens(n: number): string {
  return n < 1000 ? String(n) : `${Math.round(n / 1000)}k`;
}

/** The context meter for a messages response's usage, or null (→ no meter)
 *  when the adapter sent none or a limit it can't divide by (limit <= 0). */
export function contextMeter(cu: ContextUsage | null | undefined): ContextMeter | null {
  if (cu === null || cu === undefined || cu.limit <= 0) return null;
  const ratio = cu.used / cu.limit;
  return {
    pct: Math.round(ratio * 100),
    tint: ratio >= 0.95 ? 'danger' : ratio >= 0.8 ? 'warn' : '',
    tokens: `${fmtTokens(cu.used)} of ${fmtTokens(cu.limit)} tokens`,
  };
}

/** The run's spawn-time model and effort as catalog labels (raw id fallback). */
export interface ModelLabels {
  model: string;
  /** '' when the run carries no effort. */
  effort: string;
}

/** A run's spawn-time model and effort (issue #68) as the provider catalog's
 *  labels, the raw id as fallback; null for a legacy row with no model. A
 *  mid-session /model switch is knowingly not reflected — spawn-time truth.
 *  Shared by the chat header and the desktop Runs table (issue #76). */
export function runModelLabels(
  run: Pick<Run, 'provider' | 'model' | 'effort'>,
  providers: Provider[] | undefined,
): ModelLabels | null {
  if (run.model === '') return null;
  const p = providers?.find((x) => x.id === run.provider);
  const model = p?.models.find((o) => o.value === run.model)?.label ?? run.model;
  if (run.effort === '') return { model, effort: '' };
  const effort = p?.efforts.find((o) => o.value === run.effort)?.label ?? run.effort;
  return { model, effort };
}

/** `Model · Effort`, or the model alone when the run carries no effort. */
export function modelLabelText(labels: ModelLabels): string {
  return labels.effort === '' ? labels.model : `${labels.model} · ${labels.effort}`;
}

/** The meter's ring glyph: a track plus an arc filled to `pct` (clamped to
 *  0..100). Decorative — the enclosing control carries the accessible name and
 *  the percentage text. */
export function ContextRing(props: { pct: number; size?: number }): JSX.Element {
  const fill = () => Math.max(0, Math.min(100, props.pct));
  return (
    <svg
      class="chat-context-ring"
      viewBox="0 0 20 20"
      width={props.size ?? 16}
      height={props.size ?? 16}
      aria-hidden="true"
    >
      <circle class="chat-context-ring-track" cx="10" cy="10" r="7" fill="none" stroke-width="3" />
      <circle
        class="chat-context-ring-fill"
        cx="10"
        cy="10"
        r="7"
        fill="none"
        stroke-width="3"
        pathLength="100"
        stroke-dasharray={`${fill()} 100`}
        transform="rotate(-90 10 10)"
      />
    </svg>
  );
}

export function RunDetails(props: {
  run: Run;
  repo: Repo | undefined;
  model: ModelLabels | null;
  meter: ContextMeter | null;
  live: boolean;
  /** Below 1024px: a modal bottom sheet with its own scrim. At and above: a
   *  non-modal popover anchored by the caller (whose scrim it also owns). */
  variant: 'sheet' | 'popover';
  pullBusy: boolean;
  onPullBase: () => void;
  onClose: () => void;
}): JSX.Element {
  const headingId = `run-details-${createUniqueId()}-heading`;
  // Commits on origin/<base> not yet in the branch (issue #149). The server
  // omits the count when there is nothing to report — up to date, ended, or
  // not computable — so absence reads as "up to date" only while live.
  const behind = () => props.run.commits_behind ?? 0;

  const card = (): JSX.Element => (
    // The card stops touchstart like ToolPanel's sheet: window-level touch
    // listeners behind a modal must not see touches that land on it.
    <section
      classList={{
        'chat-details': true,
        'chat-sheet': props.variant === 'sheet',
        'chat-popover': props.variant === 'popover',
      }}
      role="dialog"
      aria-modal={props.variant === 'sheet' ? 'true' : undefined}
      aria-labelledby={headingId}
      onTouchStart={(e) => e.stopPropagation()}
    >
      <div class="chat-details-head">
        <h2 id={headingId} class="chat-details-title">
          Run details
        </h2>
        <button
          type="button"
          class="icon-btn chat-details-close"
          aria-label="Close run details"
          title="Close"
          onClick={() => props.onClose()}
          // Move focus into the surface on open (deferred until mounted, the
          // rename input's idiom) so a keyboard user lands inside it.
          ref={(el) => setTimeout(() => el.focus())}
        >
          <Icon name="x" />
        </button>
      </div>
      <dl class="chat-details-facts">
        <Show when={props.model}>
          {(m) => (
            <div class="chat-details-fact">
              <dt>Model</dt>
              <dd class="chat-details-model">
                <span>{m().model}</span>
                <small>
                  {m().effort === '' ? 'set at spawn' : `${m().effort} effort · set at spawn`}
                </small>
              </dd>
            </div>
          )}
        </Show>
        <div class="chat-details-fact">
          <dt>Context</dt>
          <dd class="chat-details-context">
            <Show when={props.meter} fallback={<small>Not reported yet</small>}>
              {(meter) => (
                <>
                  <span
                    classList={{
                      'chat-details-bar': true,
                      warn: meter().tint === 'warn',
                      danger: meter().tint === 'danger',
                    }}
                    aria-hidden="true"
                  >
                    <span style={{ width: `${Math.max(0, Math.min(100, meter().pct))}%` }} />
                  </span>
                  <b>{meter().pct}%</b>
                  <small>{meter().tokens}</small>
                </>
              )}
            </Show>
          </dd>
        </div>
        <div class="chat-details-fact">
          <dt>Branch</dt>
          <dd>
            <span class="mono chat-details-branch">{props.run.branch}</span>
          </dd>
        </div>
        <div class="chat-details-fact">
          <dt>Base</dt>
          <dd class="chat-details-base">
            <span class="mono">{props.repo?.default_branch ?? 'base branch'}</span>
            <Show when={props.live}>
              <Show when={behind() > 0} fallback={<small>Up to date</small>}>
                <small
                  class="chat-details-behind"
                  title={`${behind()} commit${behind() === 1 ? '' : 's'} behind the base branch`}
                >
                  {behind()} behind
                </small>
                {/* Pull base (issue #58): the `/pull-base` lab command down the
                    ordinary reply path — offered only while a live run is
                    behind, so it never reads as a no-op. */}
                <button
                  type="button"
                  class="chat-details-pull"
                  disabled={props.pullBusy}
                  onClick={() => props.onPullBase()}
                >
                  {props.pullBusy ? 'Pulling…' : 'Pull base'}
                </button>
              </Show>
            </Show>
          </dd>
        </div>
      </dl>
      <Show when={props.meter}>
        <p class="chat-details-note">The ring turns amber at 80% and red at 95%.</p>
      </Show>
    </section>
  );

  return (
    <Show when={props.variant === 'sheet'} fallback={card()}>
      {/* Scrim and card are siblings, not nested (the InstallSheet idiom): the
          dismiss handler lives on the scrim alone, so a tap in the card never
          bubbles into a dismiss. */}
      <div
        class="chat-sheet-scrim"
        aria-hidden="true"
        onClick={() => props.onClose()}
        onTouchStart={(e) => e.stopPropagation()}
      />
      {card()}
    </Show>
  );
}
