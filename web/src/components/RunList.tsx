// The grouped live-run list (issue #76): ONE component with two homes — the
// phone's Runs page at `/` (variant 'page', 56px rows) and the desktop rail's
// run list (variant 'rail', the same rows compacted for 260px). Live instances
// group into Needs you / Working / Idle in the rail's attention-first order
// (lib/runGroups → lib/railOrder); each group's label carries its count, and
// the Needs you count — the attention count the tab badge and `(N) lab` show —
// is accent-styled.
//
// A row is a single <A> to the chat: the state dot (colour-only, so the link's
// accessible name carries the state word, exactly as the old rail row did),
// the title plus the "N behind" chip (issue #149), a second line `repo · state
// phrase · AFK budget`, and the age on the right (since started_at — the API
// has no last-activity field, see lib/runGroups). The rail variant follows the
// mockup's rail and drops the state phrase and the age: the dot and the
// accessible name carry the state, and 260px has no room for more. Rows carry
// NO destructive action — Stop lives in the chat header and on the Repos page.
//
// The instances arrive as a prop; the caller reads them from the shell's one
// listInstances resource (lib/shellInstances), never a second fetch.

import { A } from '@solidjs/router';
import { For, Show, createSignal, onCleanup } from 'solid-js';
import type { ConversationState, Instance } from '../api';
import { budgetRemaining, parseAFKLabel } from '../lib/afk';
import { stateBadge } from '../lib/conversation';
import { runDisplayTitle, sessionLabel } from '../lib/instanceLabel';
import { railGroup } from '../lib/railOrder';
import { groupLive, liveAge, statePhrase } from '../lib/runGroups';

export type RunListVariant = 'page' | 'rail';

export default function RunList(props: { instances: Instance[]; variant: RunListVariant }) {
  const groups = () => groupLive(props.instances);

  // Display-only tick for the AFK budget countdown and the row ages (the
  // spec's one sanctioned interval, inherited from the old rail rows).
  const [now, setNow] = createSignal(Date.now());
  const ticker = setInterval(() => setNow(Date.now()), 30_000);
  onCleanup(() => clearInterval(ticker));

  return (
    <Show when={groups().length > 0} fallback={<p class="runlist-empty">No live runs.</p>}>
      <div classList={{ runlist: true, [`runlist-${props.variant}`]: true }}>
        <For each={groups()}>
          {(group) => (
            <section class="runlist-group">
              <p class="runlist-label">
                {group.label}
                <span classList={{ 'runlist-count': true, attn: group.key === 'needs-you' }}>
                  {group.instances.length}
                </span>
              </p>
              <ul class="runlist-rows">
                <For each={group.instances}>
                  {(instance) => <RunRow instance={instance} variant={props.variant} now={now()} />}
                </For>
              </ul>
            </section>
          )}
        </For>
      </div>
    </Show>
  );
}

/** The colour-only state dot, shared with the desktop Runs table. */
export function StateDot(props: { state: ConversationState }) {
  return (
    <span
      class="runlist-dot"
      classList={{
        working: props.state === 'working',
        'needs-input': props.state === 'needs_input',
        question: props.state === 'question',
      }}
      title={stateBadge(props.state)?.title}
    />
  );
}

/** The AFK budget countdown for an AFK run, null for every other run. */
export function afkBudget(instance: Instance, nowMs: number): string | null {
  const afk = parseAFKLabel(sessionLabel(instance.session_name));
  return afk === null ? null : budgetRemaining(instance.budget_deadline, nowMs);
}

/**
 * The row link's accessible name: title — repo — state word — N behind. The
 * dot is colour-only, so the state word a screen reader would otherwise miss
 * rides here (same shape as the pre-#76 rail row).
 */
export function runAriaLabel(instance: Instance): string {
  const base = `${runDisplayTitle(instance)} — ${instance.repo_name}`;
  const badge = stateBadge(instance.state);
  const withBadge = badge === null ? base : `${base} — ${badge.label}`;
  const behind = instance.commits_behind ?? 0;
  return behind > 0 ? `${withBadge} — ${behind} behind` : withBadge;
}

function RunRow(props: { instance: Instance; variant: RunListVariant; now: number }) {
  const state = () => props.instance.state;
  // A user-set title wins over the parsed label (issue #111 rename overlay).
  const title = () => runDisplayTitle(props.instance);
  const budget = () => afkBudget(props.instance, props.now);
  // Absent/0 hides the chip — same convention as the chat header's chip.
  const behind = () => props.instance.commits_behind ?? 0;
  const page = () => props.variant === 'page';
  return (
    <li>
      <A
        href={`/runs/${props.instance.id}`}
        end
        class="runlist-row"
        aria-label={runAriaLabel(props.instance)}
      >
        <StateDot state={state()} />
        <span class="runlist-body">
          <span class="runlist-top">
            <span class="runlist-title">{title()}</span>
            <Show when={behind() > 0}>
              <span
                class="chip runlist-behind"
                title={`${behind()} commit${behind() === 1 ? '' : 's'} behind the base branch`}
              >
                {behind()} behind
              </span>
            </Show>
          </span>
          <span class="runlist-sub">
            {props.instance.repo_name}
            <Show when={page()}>
              {' · '}
              <span classList={{ 'runlist-state': true, attn: railGroup(state()) === 0 }}>
                {statePhrase(state()).toLowerCase()}
              </span>
            </Show>
            <Show when={budget()}>
              {(b) => (
                <span
                  classList={{ 'runlist-budget': true, over: b() === 'over budget' }}
                  title="Time left on this AFK run's budget"
                >
                  {' · '}
                  {b()}
                </span>
              )}
            </Show>
          </span>
        </span>
        <Show when={page()}>
          <span class="runlist-age">{liveAge(props.instance, props.now)}</span>
        </Show>
      </A>
    </li>
  );
}
