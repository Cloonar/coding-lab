// The chat header's Preview chip (issue #105): shown only while the run has at
// least one exposure (`labctl expose`), same responsive pattern as the
// exposed-secrets badge — a globe icon below 640px, a "Preview" text chip at
// and above (CSS-gated; both forms are in the DOM, the accessible name lives on
// the link/button so it is stable across them).
//
// - One exposure: the chip IS a link opening its URL in a new tab.
// - Several: the chip is a menu button; an anchored dropdown (the `•••` menu's
//   .chat-menu-dropdown / .chat-menu-item look) lists each by name and URL, each
//   entry a new-tab link. It closes on Escape (consumed), an outside press, and
//   on picking an entry.
// - An exposure that is not live (labctl reconnecting) is still openable — the
//   preview listener answers with a page saying so — but muted and titled.
//
// Pure view of `exposures`: ChatHeader passes the run resource's value, which
// run.changed already refetches, so no extra wiring is needed to follow the
// server.

import { For, Show, createEffect, createSignal, createUniqueId, onCleanup } from 'solid-js';
import type { RunExposure } from '../../api';
import Icon from '../../components/Icon';

const exposureTitle = (e: RunExposure): string =>
  e.live ? `Open ${e.name} preview` : `${e.name}: tunnel reconnecting — open for status`;

export function PreviewChip(props: { exposures: RunExposure[] }) {
  const [open, setOpen] = createSignal(false);
  const menuId = `chat-preview-menu-${createUniqueId()}`;
  let anchorEl: HTMLDivElement | undefined;
  let toggleEl: HTMLButtonElement | undefined;

  const allDown = () => props.exposures.every((e) => !e.live);

  // The menu only exists for several exposures; if the count drops to one (or
  // zero) while it is open, the open flag must not linger for the next growth.
  createEffect(() => {
    if (props.exposures.length < 2) setOpen(false);
  });

  // Outside press + Escape, only while open. Escape is consumed so the tool
  // panel's window Esc-close (issue #145) skips it, like ChatHeader's overlays.
  createEffect(() => {
    if (!open()) return;
    const onPointerDown = (e: Event) => {
      if (anchorEl !== undefined && e.target instanceof Node && !anchorEl.contains(e.target)) {
        setOpen(false);
      }
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      e.preventDefault();
      setOpen(false);
      toggleEl?.focus();
    };
    window.addEventListener('pointerdown', onPointerDown);
    window.addEventListener('keydown', onKeyDown);
    onCleanup(() => {
      window.removeEventListener('pointerdown', onPointerDown);
      window.removeEventListener('keydown', onKeyDown);
    });
  });

  const face = () => (
    <span class="chat-preview-pill">
      <Icon name="globe" class="chat-preview-icon" />
      <span class="chat-preview-label">Preview</span>
    </span>
  );

  return (
    <Show when={props.exposures.length > 0}>
      <Show
        when={props.exposures.length > 1}
        fallback={
          <Show when={props.exposures[0]}>
            {(e) => (
              <a
                classList={{ 'chat-preview': true, 'not-live': !e().live }}
                href={e().url}
                target="_blank"
                rel="noopener noreferrer"
                aria-label={`Open preview: ${e().name}${e().live ? '' : ' (reconnecting)'}`}
                title={exposureTitle(e())}
              >
                {face()}
              </a>
            )}
          </Show>
        }
      >
        <div class="chat-preview-anchor" ref={anchorEl}>
          <button
            type="button"
            ref={toggleEl}
            classList={{ 'chat-preview': true, 'not-live': allDown() }}
            aria-label={`Previews (${props.exposures.length})`}
            title={`${props.exposures.length} previews`}
            aria-haspopup="menu"
            aria-expanded={open()}
            aria-controls={open() ? menuId : undefined}
            onClick={() => setOpen(!open())}
          >
            {face()}
          </button>
          <Show when={open()}>
            <div
              class="chat-menu-panel chat-menu-dropdown chat-preview-menu"
              id={menuId}
              role="menu"
              aria-label="Previews"
            >
              <For each={props.exposures}>
                {(e) => (
                  <a
                    classList={{
                      'chat-menu-item': true,
                      'chat-preview-item': true,
                      'not-live': !e.live,
                    }}
                    role="menuitem"
                    href={e.url}
                    target="_blank"
                    rel="noopener noreferrer"
                    title={exposureTitle(e)}
                    onClick={() => setOpen(false)}
                  >
                    <span class="chat-preview-item-text">
                      <span class="chat-preview-item-name">{e.name}</span>
                      <small class="mono chat-preview-item-url">{e.url}</small>
                    </span>
                    <Show when={!e.live}>
                      <small class="chat-preview-item-state">reconnecting</small>
                    </Show>
                  </a>
                )}
              </For>
            </div>
          </Show>
        </div>
      </Show>
    </Show>
  );
}
