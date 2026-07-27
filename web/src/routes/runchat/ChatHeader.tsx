// The compact chat header (issue #7 / ADR-0016): click-to-edit title (issue
// #111) with project and forge links (issue #132), the state/exposure/model/
// behind chips (issues #68/#108/#149), the open affordance (ADR-0017), the
// one-tap turn Interrupt (ADR-0029) beside the two-step Stop, and the <640px
// ••• overflow menu carrying what the one-line row can't hold.

import { A } from '@solidjs/router';
import { Portal } from 'solid-js/web';
import { Match, Show, Switch, createSignal, onCleanup } from 'solid-js';
import {
  errorMessage,
  stopInstance,
  updateRun,
  type ContextUsage,
  type ConversationState,
  type Provider,
  type Repo,
  type Run,
} from '../../api';
import Icon from '../../components/Icon';
import OpenAffordance from '../../components/OpenAffordance';
import { stateBadge } from '../../lib/conversation';
import {
  openState,
  providerOpen,
  providerSupportsRemote,
  type OpenState,
} from '../../lib/deepLink';
import { runDisplayTitle, sessionRepo } from '../../lib/instanceLabel';
import { forgeWebUrl } from '../../lib/repoName';
import { createInterrupt } from './Composer';

export function ChatHeader(props: {
  run: Run | undefined;
  /** The run's repo (issue #132), for the forge web link; undefined until loaded or on a failed fetch. */
  repo: Repo | undefined;
  providers: Provider[] | undefined;
  state: ConversationState;
  /** The latest messages response's context-occupancy meter (issue #243 /
   *  ADR-0061), fed from createMessageFeed; null/undefined hides the meter. */
  contextUsage: ContextUsage | null | undefined;
  onError: (message: string) => void;
  onChanged: () => void;
  /** One-tap turn Interrupt fired from the header/menu (ADR-0029) — refetch on done. */
  onInterrupted: () => void;
  hidden: boolean;
  headerRef: (el: HTMLElement) => void;
}) {
  const [confirming, setConfirming] = createSignal(false);
  const [stopping, setStopping] = createSignal(false);
  // Inline rename (issue #111): the title is click-to-edit. A user-set title
  // is a pure display overlay — identity (session/branch/worktree/tmux) never
  // changes — always available, live or finished.
  const [titleMode, setTitleMode] = createSignal<'view' | 'edit'>('view');
  const [titleDraft, setTitleDraft] = createSignal('');
  const [titleSaving, setTitleSaving] = createSignal(false);
  // The edit-mode placeholder — what the title would be without an override.
  // Force title: null through the shared runDisplayTitle fallback chain
  // (label → branch) rather than re-deriving it here.
  const generatedTitle = () => {
    const r = props.run;
    if (r === undefined) return 'Chat';
    return runDisplayTitle({ ...r, title: null });
  };
  const title = () => (props.run === undefined ? 'Chat' : runDisplayTitle(props.run));
  // The project name beside the title, titled or not — the session name is
  // `<repo>~<label>`, and the label/generated title alone is ambiguous across
  // repos. "" for legacy no-`~` sessions, which the view hides entirely.
  const project = () => sessionRepo(props.run?.session_name ?? '');
  // The repo's hosted forge web URL for the git-icon link (issue #132), or null
  // when the repo isn't loaded yet, has no forge, or its remote doesn't parse —
  // the icon is hidden entirely in every null case, never greyed.
  const forgeHref = (): string | null => {
    const r = props.repo;
    return r === undefined ? null : forgeWebUrl(r.remote_url, r.forge_kind);
  };
  const startRename = () => {
    const r = props.run;
    if (r === undefined) return;
    setTitleDraft(r.title ?? '');
    setTitleMode('edit');
  };
  const saveTitle = (event: SubmitEvent) => {
    event.preventDefault();
    const r = props.run;
    if (r === undefined) return;
    // Empty-after-trim clears the override — reverting to the generated title
    // IS the reset path; the server stores null.
    const value = titleDraft().trim();
    setTitleSaving(true);
    void (async () => {
      try {
        await updateRun(r.id, { title: value === '' ? null : value });
        setTitleMode('view');
        props.onChanged();
      } catch (err) {
        // Errors ride the page banner via onError, like Stop/Interrupt — the
        // single-row header has no room for inline error text.
        props.onError(errorMessage(err));
      } finally {
        setTitleSaving(false);
      }
    })();
  };
  const badge = () => stateBadge(props.state);
  // The exposure warning badge (issue #108): props.run.exposed_secrets names
  // this run's secrets whose value has surfaced in its own transcript — a
  // sticky flag cleared only by rotating the secret in repo settings. Shares
  // the state badge's dot(<640px)/chip(>=640px) rendering pattern below, but
  // is not itself a StateBadge (it isn't conversational state), so it gets
  // its own small label/title mapping rather than reusing stateBadge.
  const exposedNames = () => props.run?.exposed_secrets ?? [];
  const exposedBadge = (): { label: string; title: string } | null => {
    const names = exposedNames();
    if (names.length === 0) return null;
    if (names.length === 1) {
      return {
        label: `${names[0]} exposed`,
        title: `${names[0]} appeared in this run's transcript — rotate it in repo settings to clear`,
      };
    }
    return {
      label: `${names.length} secrets exposed`,
      title: `Exposed in this run's transcript: ${names.join(', ')} — rotate them in repo settings to clear`,
    };
  };
  const live = () => props.run !== undefined && props.run.outcome === 'active';
  // The "N behind" chip (issue #149): commits on origin/<base> not yet in this
  // run's branch. Absent/0 hides the chip entirely — a countless dot would be
  // useless, so unlike the state/exposed badges above this rides at ALL
  // widths, not just >=640px (chat-behind-chip skips their display:none gate).
  const behindCount = () => props.run?.commits_behind ?? 0;
  // The run's spawn-time model chip (issue #68): catalog pretty labels with the
  // raw id as fallback, hidden entirely for legacy rows with no model. A
  // mid-session /model switch is knowingly not reflected — spawn-time truth only.
  // The context-pressure meter (issue #243) rides this same chip as a nested
  // `· 62%` span (contextMeter below), so it inherits the chip's fate — hidden
  // with it, both by the sub-640px display gate and for a model-less run.
  const modelInfo = (): string | null => {
    const r = props.run;
    if (r === undefined || r.model === '') return null;
    const p = props.providers?.find((x) => x.id === r.provider);
    const model = p?.models.find((o) => o.value === r.model)?.label ?? r.model;
    if (r.effort === '') return model;
    const effort = p?.efforts.find((o) => o.value === r.effort)?.label ?? r.effort;
    return `${model} · ${effort}`;
  };
  // fmtTokens renders a raw token count for the meter's title (issue #243):
  // 127432 → "127k"; a count below 1000 stays verbatim so a tiny value never
  // rounds down to "0k".
  const fmtTokens = (n: number): string => (n < 1000 ? String(n) : `${Math.round(n / 1000)}k`);
  // The context-pressure meter (issue #243 / ADR-0061): the fraction of the
  // model's RAW context window the next request roughly carries — a rounded
  // percentage, a tint band, and an `N of M tokens` title. null (→ no meter)
  // when the adapter sent no usage or a limit it can't divide by (limit <= 0);
  // the percentage tints amber at >=80% occupancy and red at >=95% (red wins).
  const contextMeter = (): { pct: number; tint: 'warn' | 'danger' | ''; title: string } | null => {
    const cu = props.contextUsage;
    if (cu === null || cu === undefined || cu.limit <= 0) return null;
    const ratio = cu.used / cu.limit;
    return {
      pct: Math.round(ratio * 100),
      tint: ratio >= 0.95 ? 'danger' : ratio >= 0.8 ? 'warn' : '',
      title: `${fmtTokens(cu.used)} of ${fmtTokens(cu.limit)} tokens`,
    };
  };
  // The open affordance (ADR-0017): the exact deep link when captured, else the
  // provider's generic web fallback, else a tmux-attach for a link-less
  // provider — same source of truth as the dashboard rows, never hardcoded.
  // Nothing at all for a remote-capable provider's run that spawned with remote
  // control off (issue #163): openState resolves that gate and answers null,
  // which the <Show>s on open() already render as nothing.
  const open = () => {
    const r = props.run;
    if (r === undefined) return null;
    return openState(
      {
        connecting: false,
        deep_link_url: r.deep_link_url,
        session_name: r.session_name,
        remote: r.remote,
      },
      providerOpen(props.providers, r.provider),
      providerSupportsRemote(props.providers, r.provider),
    );
  };

  const stop = async () => {
    const r = props.run;
    if (r === undefined) return;
    setStopping(true);
    try {
      await stopInstance(r.session_name);
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setStopping(false);
      setConfirming(false);
      props.onChanged();
    }
  };

  // The header's one-tap turn Interrupt (ADR-0029), shared by the inline
  // desktop button and its `•••` menu twin.
  const interrupt = createInterrupt(
    () => props.run?.id ?? '',
    (m) => props.onError(m),
    () => props.onInterrupted(),
  );

  return (
    <header
      ref={props.headerRef}
      classList={{ 'chat-header': true, 'chat-header--hidden': props.hidden }}
    >
      <A href="/" class="crumb chat-back icon-btn" aria-label="Back to home" title="Back to home">
        <Icon name="arrow-left" />
      </A>
      {/* The title area (issue #132): the click-to-edit title button (or rename
          form) with the project name and forge git-icon as SIBLINGS beside it,
          NOT inside the button — a link nested in a button is invalid
          interactive HTML. The .chat-titlebar wrapper carries the header's flex
          sizing (mobile grower / desktop cap) the button used to hold, so the
          title still dominates and the project shrinks first. */}
      <div class="chat-titlebar">
        {/* View: a click-to-edit button (issue #111) — the display title (custom
            or generated, issue #120) always wins the space. The full session
            name — the branch/worktree/tmux identity — rides the tooltip, not the
            row. The pencil fades in on hover/focus as the edit affordance. */}
        <Switch>
          <Match when={titleMode() === 'view'}>
            <button
              type="button"
              class="chat-title"
              title={props.run === undefined ? 'Rename this instance' : props.run.session_name}
              aria-label={`Rename: ${title()}`}
              onClick={startRename}
            >
              <span class="chat-title-text">{title()}</span>
              <Icon name="pencil" size={13} class="chat-title-pencil" />
            </button>
          </Match>
          <Match when={titleMode() === 'edit'}>
            <form class="chat-title chat-title-form" onSubmit={saveTitle}>
              <input
                type="text"
                class="chat-title-input"
                value={titleDraft()}
                placeholder={generatedTitle()}
                maxlength={120}
                aria-label="Instance title"
                onInput={(e) => setTitleDraft(e.currentTarget.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Escape') {
                    // Consume it: the tool panel's window Esc-close (issue
                    // #145) skips defaultPrevented events, so canceling the
                    // rename never also closes an open panel.
                    e.preventDefault();
                    setTitleMode('view');
                  }
                }}
                // Refs run before insertion; defer the focus until mounted.
                ref={(el) => setTimeout(() => el.focus())}
              />
              <button
                type="submit"
                class="icon-btn accent chat-title-save"
                aria-label="Save title"
                title="Save title (empty reverts to the generated title)"
                disabled={titleSaving()}
              >
                <Icon name="check" />
              </button>
              <button
                type="button"
                class="icon-btn chat-title-cancel"
                aria-label="Cancel rename"
                title="Cancel"
                disabled={titleSaving()}
                onClick={() => setTitleMode('view')}
              >
                <Icon name="x" />
              </button>
            </form>
          </Match>
        </Switch>
        {/* The project name as muted secondary text beside the title (view mode
            only — the edit form owns the row). A link to the repo's issues page
            (the de-facto repo landing) only when both the project name and
            repo_id are present; a legacy no-`~` session hides it entirely, and a
            repo_id-less run keeps it as inert text. */}
        <Show when={titleMode() === 'view' && project()}>
          {(p) => (
            <Show
              when={props.run?.repo_id}
              fallback={<span class="chat-title-project">{p()}</span>}
            >
              {(repoId) => (
                <A href={`/repos/${repoId()}/issues`} class="chat-title-project">
                  {p()}
                </A>
              )}
            </Show>
          )}
        </Show>
        {/* The git-icon link to the repo's hosted forge page (issue #132), new
            tab — beside the project name (so it rides the same view-mode +
            project-present gate), hidden entirely when there's no forge URL. The
            git-branch glyph is a deliberate choice over external-link. */}
        <Show when={titleMode() === 'view' && project() && forgeHref()}>
          {(href) => (
            <a
              class="chat-title-forge"
              href={href()}
              target="_blank"
              rel="noreferrer"
              aria-label="Open on forge"
              title="Open on forge"
            >
              <Icon name="git-branch" size={15} />
            </a>
          )}
        </Show>
      </div>

      {/* <640px: the convo chip distilled to a colored dot beside the title. A
          labeled graphic (role=img), not a live region — an aria-label swap
          wouldn't announce, and the full chip is display:none here so the dot is
          the only state cue a screen reader can reach on mobile. */}
      <Show when={badge()}>
        {(b) => (
          <span
            classList={{ 'chat-state-dot': true, [b().cls]: true }}
            title={b().title}
            aria-label={b().title}
            role="img"
          />
        )}
      </Show>
      {/* >=640px: the full text chip (same markup as before, now display-gated). */}
      <Show when={badge()}>
        {(b) => (
          <span
            classList={{ chip: true, convo: true, 'chat-state-chip': true, [b().cls]: true }}
            title={b().title}
          >
            {b().label}
          </span>
        )}
      </Show>
      {/* The exposure warning badge (issue #108) — same dot(<640px)/chip(>=640px)
          split as the state badge above, styled as a destructive chip. */}
      <Show when={exposedBadge()}>
        {(b) => (
          <span
            classList={{ 'chat-state-dot': true, exposed: true }}
            title={b().title}
            aria-label={b().title}
            role="img"
          />
        )}
      </Show>
      <Show when={exposedBadge()}>
        {(b) => (
          <span
            classList={{ chip: true, 'chat-state-chip': true, exposed: true }}
            title={b().title}
          >
            {b().label}
          </span>
        )}
      </Show>
      {/* >=640px: the run's spawn-time model, read-only (issue #68), now
          carrying the context-pressure meter (issue #243 / ADR-0061) as a
          nested `· 62%` span INSIDE the chip — so the meter inherits the chip's
          sub-640px hiding and goes with it. <640px the model string rides the
          ••• menu's info row instead; the meter is chat-header-only for v1, so
          that row stays model·effort. */}
      <Show when={modelInfo()}>
        {(info) => (
          <span class="chip mono chat-model-chip" title="Model · effort (set at spawn)">
            {info()}
            {/* The meter shows only when the chip text renders (this Show) AND
                the adapter composed a usable occupancy (contextMeter gates
                limit > 0). A codex run with an empty stored model could carry
                usage without a chip — nesting the meter here hides it with the
                chip by construction. Its own title carries the token counts;
                the chip keeps its outer model·effort title. */}
            <Show when={contextMeter()}>
              {(meter) => (
                <span
                  classList={{
                    'chat-context-meter': true,
                    warn: meter().tint === 'warn',
                    danger: meter().tint === 'danger',
                  }}
                  title={meter().title}
                >
                  {` · ${meter().pct}%`}
                </span>
              )}
            </Show>
          </span>
        )}
      </Show>
      {/* ALL widths (issue #149): commits on origin/<base> not yet in this
          run's branch, from /pull-base. Hidden entirely at 0/absent. */}
      <Show when={behindCount() > 0}>
        <span
          class="chip chat-behind-chip"
          title={`${behindCount()} commit${behindCount() === 1 ? '' : 's'} behind the base branch`}
        >
          {behindCount()} behind
        </span>
      </Show>

      <span class="spacer" />

      {/* >=640px: the original inline controls, verbatim. display:contents at
          >=640px makes this wrapper transparent (identical flex row to before);
          <640px the wrapper is display:none and the controls live in the menu. */}
      <div class="chat-desktop-actions">
        <Show when={open()}>{(s) => <OpenAffordance state={s()} />}</Show>
        {/* One-tap turn Interrupt (ADR-0029) — gated on live(), not the derived
            (possibly false) `working` state; accent + `pause`, deliberately
            distinct from the adjacent danger two-step `square` Stop. */}
        <Show when={live()}>
          <button
            type="button"
            class="icon-btn accent chat-turn-interrupt"
            classList={{ busy: interrupt.busy() }}
            aria-label="Interrupt"
            title="Interrupt the current turn (keeps the session)"
            onClick={() => void interrupt.run()}
          >
            <Show when={interrupt.busy()} fallback={<Icon name="pause" />}>
              <span class="chat-interrupt-busy" aria-hidden="true">
                …
              </span>
            </Show>
          </button>
        </Show>
        <Show when={live()}>
          <Switch>
            <Match when={!confirming()}>
              <button
                type="button"
                class="icon-btn danger chat-stop"
                aria-label="Stop the instance"
                title="Stop the instance"
                onClick={() => setConfirming(true)}
              >
                <Icon name="square" />
              </button>
            </Match>
            <Match when={confirming()}>
              <button
                type="button"
                class="danger chat-stop-confirm"
                disabled={stopping()}
                onClick={() => void stop()}
              >
                {stopping() ? 'Stopping…' : 'Confirm stop'}
              </button>
              <button type="button" class="seg" onClick={() => setConfirming(false)}>
                Cancel
              </button>
            </Match>
          </Switch>
        </Show>
      </div>

      {/* <640px: everything the one-line row can't hold moves into `•••`. */}
      <ChatMenu
        modelInfo={modelInfo()}
        openState={open()}
        live={live()}
        confirming={confirming()}
        stopping={stopping()}
        onInterrupt={() => void interrupt.run()}
        onRequestStop={() => setConfirming(true)}
        onCancelStop={() => setConfirming(false)}
        onStop={() => void stop()}
      />
    </header>
  );
}

// The mobile (<640px) header overflow menu (§1): an anchored dropdown mirroring
// TopBar's nav-menu/nav-scrim pattern — outside-tap + Escape + tap-to-close —
// carrying the controls the one-line header can't hold inline: the model info
// row (non-interactive, top, issue #68), the open affordance (when available),
// the one-tap turn Interrupt and the two-step Stop (both live only). `•••` is
// always present; the items are contextual. CSS hides the whole wrapper at
// >=640px, where the inline header controls render instead.
function ChatMenu(props: {
  /** The run's spawn-time model · effort string, or null to hide the row (issue #68). */
  modelInfo: string | null;
  openState: OpenState | null;
  live: boolean;
  confirming: boolean;
  stopping: boolean;
  onInterrupt: () => void;
  onRequestStop: () => void;
  onCancelStop: () => void;
  onStop: () => void;
}) {
  const [menuOpen, setMenuOpen] = createSignal(false);
  // Closing always abandons a half-armed Stop, so a confirm can't linger
  // invisibly and fire on the next open.
  const close = () => {
    setMenuOpen(false);
    props.onCancelStop();
  };

  // Close on Escape while open (mirrors TopBar).
  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Escape' && menuOpen()) close();
  };
  window.addEventListener('keydown', onKeyDown);
  onCleanup(() => window.removeEventListener('keydown', onKeyDown));

  return (
    <div class="chat-menu">
      <button
        type="button"
        class="icon-btn chat-menu-toggle"
        aria-label="More actions"
        title="More actions"
        aria-haspopup="menu"
        aria-expanded={menuOpen()}
        aria-controls={menuOpen() ? 'chat-menu-panel' : undefined}
        onClick={() => (menuOpen() ? close() : setMenuOpen(true))}
      >
        <Icon name="more-horizontal" />
      </button>

      <Show when={menuOpen()}>
        <div class="chat-menu-panel" id="chat-menu-panel" role="menu">
          {/* Non-interactive info row (issue #68): the run's spawn-time model, pinned
              at the top. role=none keeps it out of the menu's item semantics — it is
              not focusable and arrow/tab navigation never lands on it. */}
          <Show when={props.modelInfo}>
            {(info) => (
              <div class="chat-menu-info" role="none">
                {info()}
              </div>
            )}
          </Show>

          {/* Presentational wrapper (role=none): the OpenAffordance renders its
              own focusable <a>/<button>, so nesting it inside a role=menuitem
              would be invalid ARIA. Closing on click still works via bubbling. */}
          <Show when={props.openState}>
            {(s) => (
              <div class="chat-menu-item chat-menu-open" role="none" onClick={close}>
                <OpenAffordance state={s()} />
              </div>
            )}
          </Show>

          {/* One-tap turn Interrupt (ADR-0029): no confirm step, unlike the
              two-step Stop below — it keeps the session, just ends the turn. */}
          <Show when={props.live}>
            <button
              type="button"
              class="chat-menu-item accent"
              role="menuitem"
              title="Interrupt the current turn (keeps the session)"
              onClick={() => {
                props.onInterrupt();
                close();
              }}
            >
              <Icon name="pause" class="chat-menu-icon" />
              <span>Interrupt</span>
            </button>
          </Show>

          <Show when={props.live}>
            <Switch>
              <Match when={!props.confirming}>
                <button
                  type="button"
                  class="chat-menu-item danger"
                  role="menuitem"
                  onClick={() => props.onRequestStop()}
                >
                  <Icon name="square" class="chat-menu-icon" />
                  <span>Stop run…</span>
                </button>
              </Match>
              <Match when={props.confirming}>
                <button
                  type="button"
                  class="chat-menu-item danger"
                  role="menuitem"
                  disabled={props.stopping}
                  onClick={() => props.onStop()}
                >
                  <Icon name="square" class="chat-menu-icon" />
                  <span>{props.stopping ? 'Stopping…' : 'Confirm stop'}</span>
                </button>
                <button
                  type="button"
                  class="chat-menu-item"
                  role="menuitem"
                  onClick={() => props.onCancelStop()}
                >
                  <span class="chat-menu-icon" aria-hidden="true" />
                  <span>Cancel</span>
                </button>
              </Match>
            </Switch>
          </Show>
        </div>
      </Show>

      {/* Portal the scrim to document.body: the mobile header is its own
          stacking context (position:absolute; z-index:2), so a scrim nested
          inside it would dim the header's own back/title. At body level it sits
          below the header and dims only the page behind — like TopBar's scrim. */}
      <Show when={menuOpen()}>
        <Portal>
          <div class="chat-menu-scrim" aria-hidden="true" onClick={close} />
        </Portal>
      </Show>
    </div>
  );
}
