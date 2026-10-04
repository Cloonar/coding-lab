// The compact chat header (issue #7 / ADR-0016), reshaped by issue #58 §1:
// - a two-line title block — the click-to-edit title (issue #111) over a
//   secondary line carrying the project link, the forge link (issue #132) and
//   the run's spawn-time model · effort (issue #68), at EVERY width;
// - the exposed-secrets badge (issue #108);
// - the context meter (issue #243 / ADR-0061) as a ring + percentage button at
//   every width and in every conversational state, opening Run details;
// - the open affordance (ADR-0017) and the two-step Stop, inline at >=640px;
// - the `•••` menu at every width — an anchored dropdown >=640px, a bottom
//   sheet <640px — always carrying "Run details" and, while the run is live,
//   the one-tap turn Interrupt (ADR-0029), plus on phones what the one-line
//   row can't hold (open affordance, two-step Stop).
// The conversational state badge and the "N behind" chip left the header: the
// dock's status line (issue #58 §2) and Run details replace them.

import { A } from '@solidjs/router';
import { Match, Show, Switch, createSignal, onCleanup, type JSX } from 'solid-js';
import {
  errorMessage,
  stopInstance,
  updateRun,
  type ContextUsage,
  type Provider,
  type Repo,
  type Run,
} from '../../api';
import Icon from '../../components/Icon';
import OpenAffordance from '../../components/OpenAffordance';
import {
  openState,
  providerOpen,
  providerSupportsRemote,
  type OpenState,
} from '../../lib/deepLink';
import { runDisplayTitle, sessionRepo } from '../../lib/instanceLabel';
import { createMediaQuery } from '../../lib/media';
import { forgeWebUrl } from '../../lib/repoName';
import { createInterrupt } from './Composer';
import { createPullBase } from './pullBase';
import { ContextRing, RunDetails, contextMeter, type ModelLabels } from './RunDetails';

// The `•••` menu is an anchored dropdown from the header's own >=640px layout
// up, a bottom sheet below it (where the row is a single line, issue #35 §1).
const MENU_DROPDOWN_QUERY = '(min-width: 640px)';
// Run details is an anchored popover from the desktop breakpoint up, a bottom
// sheet below it — RunChat's DESKTOP_MIN_PX (1024, the shell rail width), the
// same flip the tool panel makes, so the page never mixes phone and desktop
// chrome.
const DETAILS_POPOVER_QUERY = '(min-width: 1024px)';

export function ChatHeader(props: {
  run: Run | undefined;
  /** The run's repo (issue #132), for the forge web link and Run details' base
   *  branch; undefined until loaded or on a failed fetch. */
  repo: Repo | undefined;
  providers: Provider[] | undefined;
  /** The latest messages response's context-occupancy meter (issue #243 /
   *  ADR-0061), fed from createMessageFeed; null/undefined hides the meter. */
  contextUsage: ContextUsage | null | undefined;
  onError: (message: string) => void;
  /** A reply's informational notice (issue #149) — Pull base's "already up to
   *  date…" rides the page's notice banner, never the error one. */
  onNotice: (message: string) => void;
  onChanged: () => void;
  /** One-tap turn Interrupt fired from the `•••` menu (ADR-0029) — refetch on done. */
  onInterrupted: () => void;
  /** Pull base from Run details finished (either way) — refetch on done. */
  onPullBase: () => void;
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
  // The project name under the title, titled or not — the session name is
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
        // compact header has no room for inline error text.
        props.onError(errorMessage(err));
      } finally {
        setTitleSaving(false);
      }
    })();
  };
  // The exposure warning badge (issue #108): props.run.exposed_secrets names
  // this run's secrets whose value has surfaced in its own transcript — a
  // sticky flag cleared only by rotating the secret in repo settings. A dot
  // (<640px) and a text chip (>=640px), CSS-gated.
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
  // The run's spawn-time model and effort (issue #68): catalog pretty labels
  // with the raw id as fallback, null for a legacy row with no model. A
  // mid-session /model switch is knowingly not reflected — spawn-time truth.
  const modelLabels = (): ModelLabels | null => {
    const r = props.run;
    if (r === undefined || r.model === '') return null;
    const p = props.providers?.find((x) => x.id === r.provider);
    const model = p?.models.find((o) => o.value === r.model)?.label ?? r.model;
    if (r.effort === '') return { model, effort: '' };
    const effort = p?.efforts.find((o) => o.value === r.effort)?.label ?? r.effort;
    return { model, effort };
  };
  const modelInfo = (): string | null => {
    const m = modelLabels();
    if (m === null) return null;
    return m.effort === '' ? m.model : `${m.model} · ${m.effort}`;
  };
  // The context meter (issue #243 / ADR-0061, issue #58 §1): null — no meter —
  // when the adapter sent no usage or a limit it can't divide by. Its own
  // gate, no longer nested in (or hidden with) the model text.
  const meter = () => contextMeter(props.contextUsage);
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

  // The `•••` menu's one-tap turn Interrupt (ADR-0029) — the single shared
  // contract in Composer, never reimplemented here.
  const interrupt = createInterrupt(
    () => props.run?.id ?? '',
    (m) => props.onError(m),
    () => props.onInterrupted(),
  );
  // Run details' Pull base: the `/pull-base` lab command down the ordinary
  // reply path, its notice on the page's notice banner.
  const pullBase = createPullBase(
    () => props.run?.id ?? '',
    (m) => props.onError(m),
    (m) => props.onNotice(m),
    () => props.onPullBase(),
  );

  // --- the two overlays: the `•••` menu and Run details ----------------------
  // At most one is open: opening either closes the other. Each picks its
  // container from a live media query (a pure render switch — crossing the
  // breakpoint while open keeps it open). The dropdown and the popover anchor
  // INSIDE the header; the bottom sheets render AFTER it, outside the header's
  // stacking context (z-index 2) and its <640px overlay transform, so a
  // position:fixed sheet is placed against the viewport and stacks over the
  // whole page.
  const menuDropdown = createMediaQuery(MENU_DROPDOWN_QUERY);
  const detailsPopover = createMediaQuery(DETAILS_POPOVER_QUERY);
  const [menuOpen, setMenuOpen] = createSignal(false);
  // Which trigger opened Run details — the popover anchors under it — or null
  // while closed. The meter can vanish under an open popover (usage reset), so
  // the anchor falls back to the `•••` menu, which is always present.
  const [detailsFrom, setDetailsFrom] = createSignal<'meter' | 'menu' | null>(null);
  const detailsOpen = () => detailsFrom() !== null && props.run !== undefined;
  const detailsAnchor = (): 'meter' | 'menu' =>
    detailsFrom() === 'meter' && meter() !== null ? 'meter' : 'menu';
  let menuToggleEl: HTMLButtonElement | undefined;
  let meterEl: HTMLButtonElement | undefined;

  // Closing the menu always abandons a half-armed Stop, so a confirm can't
  // linger invisibly and fire on the next open.
  const closeMenu = () => {
    setMenuOpen(false);
    setConfirming(false);
  };
  const toggleMenu = () => {
    if (menuOpen()) {
      closeMenu();
      return;
    }
    setDetailsFrom(null);
    setMenuOpen(true);
  };
  const openDetails = (from: 'meter' | 'menu') => {
    closeMenu();
    setDetailsFrom(from);
  };
  // Focus returns to whichever trigger opened the surface (the menu entry is
  // gone with the closed menu, so its trigger is the `•••` button).
  const closeDetails = () => {
    const from = detailsFrom();
    setDetailsFrom(null);
    const back = from === 'meter' && meterEl?.isConnected ? meterEl : menuToggleEl;
    back?.focus();
  };

  // Escape closes whichever overlay is open (mirrors TopBar) and CONSUMES the
  // event: the tool panel's window Esc-close (issue #145) skips
  // defaultPrevented events, so closing these never also closes the panel —
  // and the rename input, which consumes its own Escape, never closes them.
  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key !== 'Escape' || e.defaultPrevented) return;
    if (detailsOpen()) {
      e.preventDefault();
      closeDetails();
    } else if (menuOpen()) {
      e.preventDefault();
      closeMenu();
    }
  };
  window.addEventListener('keydown', onKeyDown);
  onCleanup(() => window.removeEventListener('keydown', onKeyDown));

  const runDetails = (variant: 'sheet' | 'popover') => (
    <Show when={props.run}>
      {(r) => (
        <RunDetails
          run={r()}
          repo={props.repo}
          model={modelLabels()}
          meter={meter()}
          live={live()}
          variant={variant}
          pullBusy={pullBase.busy()}
          onPullBase={() => {
            // Close first so the reply's notice (or error) banner is in view.
            closeDetails();
            void pullBase.run();
          }}
          onClose={closeDetails}
        />
      )}
    </Show>
  );

  const menuItems = (sheet: boolean) => (
    <ChatMenuItems
      sheet={sheet}
      meterPct={meter()?.pct ?? 0}
      openState={open()}
      live={live()}
      confirming={confirming()}
      stopping={stopping()}
      onClose={closeMenu}
      onDetails={() => openDetails('menu')}
      onInterrupt={() => void interrupt.run()}
      onRequestStop={() => setConfirming(true)}
      onCancelStop={() => setConfirming(false)}
      onStop={() => void stop()}
    />
  );

  return (
    <>
      <header
        ref={props.headerRef}
        classList={{ 'chat-header': true, 'chat-header--hidden': props.hidden }}
      >
        <A href="/" class="crumb chat-back icon-btn" aria-label="Back to home" title="Back to home">
          <Icon name="arrow-left" />
        </A>
        {/* The two-line title block (issue #58 §1): line 1 the click-to-edit
            title button (or rename form); line 2 the secondary line — project
            link, forge git-icon link, and the spawn-time model · effort. The
            links are SIBLINGS of the title button, never inside it: a link
            nested in a button is invalid interactive HTML (issue #132). */}
        <div class="chat-titlebar">
          {/* View: a click-to-edit button (issue #111) — the display title
              (custom or generated, issue #120). The full session name — the
              branch/worktree/tmux identity — rides the tooltip. The pencil fades
              in on hover/focus as the edit affordance. */}
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
          {/* The secondary line (view mode only — the edit form owns the
              block). Hidden entirely when it would be empty: a legacy no-`~`
              session with no model. */}
          <Show when={titleMode() === 'view' && (project() !== '' || modelInfo() !== null)}>
            <div class="chat-title-sub">
              {/* The project name as muted text, linking to the repo's issues
                  page (the de-facto repo landing) when repo_id is present; a
                  repo_id-less run keeps it as inert text. */}
              <Show when={project()}>
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
              {/* The git-icon link to the repo's hosted forge page (issue #132),
                  new tab — beside the project name (same project-present gate),
                  hidden entirely when there's no forge URL. */}
              <Show when={project() && forgeHref()}>
                {(href) => (
                  <a
                    class="chat-title-forge"
                    href={href()}
                    target="_blank"
                    rel="noreferrer"
                    aria-label="Open on forge"
                    title="Open on forge"
                  >
                    <Icon name="git-branch" size={14} />
                  </a>
                )}
              </Show>
              {/* The run's spawn-time model · effort (issue #68), read-only
                  metadata at EVERY width (issue #58 §1 — it used to hide below
                  640px). It ellipsizes after the project has given way. */}
              <Show when={modelInfo()}>
                {(info) => (
                  <>
                    <Show when={project()}>
                      <span class="chat-title-sep" aria-hidden="true">
                        ·
                      </span>
                    </Show>
                    <span class="chat-title-model" title="Model · effort (set at spawn)">
                      {info()}
                    </span>
                  </>
                )}
              </Show>
            </div>
          </Show>
        </div>

        {/* The exposure warning badge (issue #108): a labeled dot (role=img)
            below 640px, a destructive text chip at >=640px — CSS-gated. */}
        <Show when={exposedBadge()}>
          {(b) => (
            <span class="chat-exposed-dot" title={b().title} aria-label={b().title} role="img" />
          )}
        </Show>
        <Show when={exposedBadge()}>
          {(b) => (
            <span class="chip exposed chat-exposed-chip" title={b().title}>
              {b().label}
            </span>
          )}
        </Show>

        <span class="spacer" />

        {/* The context meter (issue #58 §1): ring + rounded percentage at every
            width, in every conversational state, whenever usable usage exists
            — a button opening Run details. The percentage text is always there,
            so the amber/red tint is never the only cue. */}
        <Show when={meter()}>
          {(m) => (
            <div class="chat-meter-anchor">
              <button
                type="button"
                ref={meterEl}
                classList={{
                  'chat-context-meter': true,
                  warn: m().tint === 'warn',
                  danger: m().tint === 'danger',
                }}
                aria-label={`Context ${m().pct}% used — run details`}
                aria-haspopup="dialog"
                aria-expanded={detailsOpen() && detailsAnchor() === 'meter'}
                title={m().tokens}
                onClick={() => (detailsOpen() ? closeDetails() : openDetails('meter'))}
              >
                <span class="chat-context-pill">
                  <ContextRing pct={m().pct} />
                  <span class="chat-context-pct">{m().pct}%</span>
                </span>
              </button>
              <Show when={detailsOpen() && detailsPopover() && detailsAnchor() === 'meter'}>
                {runDetails('popover')}
              </Show>
            </div>
          )}
        </Show>

        {/* >=640px: the open affordance and the two-step Stop inline.
            display:contents at >=640px makes this wrapper transparent; <640px
            it is display:none and both live in the `•••` sheet instead. */}
        <div class="chat-desktop-actions">
          <Show when={open()}>{(s) => <OpenAffordance state={s()} />}</Show>
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

        {/* The `•••` menu (every width): the trigger, plus the anchored
            dropdown at >=640px and, when opened from here, the Run details
            popover at >=1024px. */}
        <div class="chat-menu">
          <button
            type="button"
            ref={menuToggleEl}
            class="icon-btn chat-menu-toggle"
            aria-label="More actions"
            title="More actions"
            aria-haspopup="menu"
            aria-expanded={menuOpen()}
            aria-controls={menuOpen() ? 'chat-menu-panel' : undefined}
            onClick={toggleMenu}
          >
            <Icon name="more-horizontal" />
          </button>
          <Show when={menuOpen() && menuDropdown()}>
            <div
              class="chat-menu-panel chat-menu-dropdown"
              id="chat-menu-panel"
              role="menu"
              aria-label="Run actions"
            >
              {menuItems(false)}
            </div>
          </Show>
          <Show when={detailsOpen() && detailsPopover() && detailsAnchor() === 'menu'}>
            {runDetails('popover')}
          </Show>
        </div>
      </header>

      {/* The dropdown's and popover's outside-tap catcher: a transparent scrim
          BELOW the header (z-index 1 vs 2), so the page behind closes them
          while the header's own controls stay live. Outside the header, which
          is its own stacking context. */}
      <Show when={menuOpen() && menuDropdown()}>
        <div class="chat-menu-scrim" aria-hidden="true" onClick={closeMenu} />
      </Show>
      <Show when={detailsOpen() && detailsPopover()}>
        <div class="chat-menu-scrim" aria-hidden="true" onClick={closeDetails} />
      </Show>

      {/* <640px: the `•••` menu as a bottom sheet over a dimming scrim. The
          wrapper keeps the `.chat-menu` scope (display:contents) so the sheet
          reads as the same menu as the dropdown. */}
      <Show when={menuOpen() && !menuDropdown()}>
        <div class="chat-menu chat-menu--sheet">
          <div
            class="chat-sheet-scrim"
            aria-hidden="true"
            onClick={closeMenu}
            onTouchStart={(e) => e.stopPropagation()}
          />
          <div
            class="chat-menu-panel chat-sheet"
            id="chat-menu-panel"
            role="menu"
            aria-label="Run actions"
            onTouchStart={(e) => e.stopPropagation()}
          >
            {menuItems(true)}
          </div>
        </div>
      </Show>

      {/* <1024px: Run details as a bottom sheet (it renders its own scrim). */}
      <Show when={detailsOpen() && !detailsPopover()}>{runDetails('sheet')}</Show>
    </>
  );
}

// The `•••` menu's items, shared by the >=640px dropdown and the <640px
// sheet: "Run details" always (the meter only exists while usage does, so this
// keeps the surface reachable), the open affordance (sheet only — inline at
// >=640px), the one-tap turn Interrupt whenever the run is live — whatever the
// derived conversational state says, since it can be stale (issue #58 §2) —
// and the two-step Stop (sheet only, live only).
function ChatMenuItems(props: {
  /** The <640px sheet carries what the one-line phone row can't hold. */
  sheet: boolean;
  /** The ring glyph's fill for the Run details entry (0 without usage). */
  meterPct: number;
  openState: OpenState | null;
  live: boolean;
  confirming: boolean;
  stopping: boolean;
  onClose: () => void;
  onDetails: () => void;
  onInterrupt: () => void;
  onRequestStop: () => void;
  onCancelStop: () => void;
  onStop: () => void;
}): JSX.Element {
  return (
    <>
      <button
        type="button"
        class="chat-menu-item"
        role="menuitem"
        title="Model, context, branch and base"
        onClick={() => props.onDetails()}
      >
        <span class="chat-menu-icon chat-menu-ring">
          <ContextRing pct={props.meterPct} size={18} />
        </span>
        <span>Run details</span>
      </button>

      {/* Presentational wrapper (role=none): the OpenAffordance renders its
          own focusable <a>/<button>, so nesting it inside a role=menuitem
          would be invalid ARIA. Closing on click still works via bubbling. */}
      <Show when={props.sheet && props.openState}>
        {(s) => (
          <div class="chat-menu-item chat-menu-open" role="none" onClick={() => props.onClose()}>
            <OpenAffordance state={s()} />
          </div>
        )}
      </Show>

      {/* One-tap turn Interrupt (ADR-0029): no confirm step, unlike the
          two-step Stop below — it keeps the session, just ends the turn. */}
      <Show when={props.live}>
        <button
          type="button"
          class="chat-menu-item accent chat-menu-interrupt"
          role="menuitem"
          title="Interrupt the current turn (keeps the session)"
          onClick={() => {
            props.onInterrupt();
            props.onClose();
          }}
        >
          <Icon name="pause" class="chat-menu-icon" />
          <span>Interrupt</span>
        </button>
      </Show>

      <Show when={props.sheet && props.live}>
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
    </>
  );
}
