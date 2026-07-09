// Unified searchable select (issue #66): one hand-rolled ARIA-listbox control
// behind every pick surface — the composer's repo/model/effort chips (skin
// "chip") and the settings pages' form fields (skin "field", the former
// CatalogSelect). The trigger stays a button; the panel holds an optional
// filter input above a role="listbox" list.
//
// CatalogSelect semantics are preserved exactly: a stored value missing from
// the catalog stays selectable as-is (marked "(not in catalog)"), so an
// untouched field never silently changes on save; with `inheritLabel` set, an
// inherit entry (value "") is prepended and selecting it emits ''.

import {
  For,
  Show,
  createEffect,
  createMemo,
  createSignal,
  createUniqueId,
  onCleanup,
  type JSX,
} from 'solid-js';
import Icon from './Icon';

export type SelectOption = {
  value: string;
  label: string;
  /** Renders the row but keeps it unselectable (e.g. a still-cloning repo). */
  disabled?: boolean;
  /** Secondary muted row text (e.g. a repo's clone status). */
  status?: string;
};

/** Panel height cap when space allows — the former `max-height: 15rem` clamp. */
export const PANEL_MAX_HEIGHT = 240;
/** Gap between the trigger and the panel — the former `calc(100% + 6px)`. */
export const PANEL_MARGIN = 6;

/**
 * Pure placement math (unit-testable under jsdom, which has no layout).
 * Coordinates are relative to the visual viewport. Below is preferred; flip
 * above only when below cannot fit a full panel AND above offers more room.
 * Each side's free space keeps `margin` px to the trigger and the same margin
 * to the viewport edge, and the returned maxHeight clamps to it so the whole
 * panel (with internal scroll) always fits on-screen.
 */
export function panelPlacement(
  triggerTop: number,
  triggerBottom: number,
  viewportHeight: number,
  margin: number = PANEL_MARGIN,
): { openAbove: boolean; maxHeight: number } {
  const below = viewportHeight - triggerBottom - margin * 2;
  const above = triggerTop - margin * 2;
  const openAbove = below < PANEL_MAX_HEIGHT && above > below;
  const free = Math.max(0, openAbove ? above : below);
  return { openAbove, maxHeight: Math.min(free, PANEL_MAX_HEIGHT) };
}

export default function Select(props: {
  value: string;
  options: SelectOption[];
  onChange: (value: string) => void;
  /** chip = composer pill trigger; field = full-width control in a .field block. */
  skin: 'chip' | 'field';
  /** field skin: the visible label; chip skin: the trigger's aria-label. */
  label: string;
  /** field skin: form-semantics name, exposed on the trigger button. */
  name?: string;
  /** chip skin: leading icon inside the pill (e.g. the repo folder). */
  icon?: JSX.Element;
  /** Prepends an inherit entry (value "") with this label; picking it emits ''. */
  inheritLabel?: string;
  /** Forces the filter input on/off; default: on from 8 options up. */
  search?: boolean;
  disabled?: boolean;
  /** Fires as the panel opens, so a host can close sibling popovers. */
  onOpen?: () => void;
}) {
  const id = createUniqueId();
  const labelId = `select-${id}-label`;
  const listboxId = `select-${id}-listbox`;
  const optionId = (index: number): string => `select-${id}-option-${index}`;

  let rootEl: HTMLDivElement | undefined;
  let triggerEl: HTMLButtonElement | undefined;
  let searchEl: HTMLInputElement | undefined;
  let listEl: HTMLDivElement | undefined;

  const [open, setOpen] = createSignal(false);
  const [query, setQuery] = createSignal('');
  const [active, setActive] = createSignal(-1);
  const [placement, setPlacement] = createSignal({
    openAbove: false,
    maxHeight: PANEL_MAX_HEIGHT,
  });

  // The full row list: inherit entry, then the not-in-catalog marker (an empty
  // value is representable when an inherit entry is offered, so it is not
  // "unknown" then), then the catalog.
  const entries = createMemo<SelectOption[]>(() => {
    const list: SelectOption[] = [];
    if (props.inheritLabel !== undefined) list.push({ value: '', label: props.inheritLabel });
    const known =
      props.value === ''
        ? props.inheritLabel !== undefined
        : props.options.some((option) => option.value === props.value);
    if (!known) {
      list.push({
        value: props.value,
        label: props.value === '' ? '(unset)' : `${props.value} (not in catalog)`,
      });
    }
    return [...list, ...props.options];
  });

  const searchOn = () => props.search ?? props.options.length >= 8;

  // Case-insensitive substring filter over label AND value.
  const visible = createMemo<SelectOption[]>(() => {
    const needle = query().trim().toLowerCase();
    const all = entries();
    if (!searchOn() || needle === '') return all;
    return all.filter(
      (entry) =>
        entry.label.toLowerCase().includes(needle) || entry.value.toLowerCase().includes(needle),
    );
  });

  const triggerLabel = () =>
    entries().find((entry) => entry.value === props.value)?.label ?? props.value;

  const firstEnabled = (list: SelectOption[]): number =>
    list.findIndex((entry) => entry.disabled !== true);
  const lastEnabled = (list: SelectOption[]): number => {
    for (let i = list.length - 1; i >= 0; i -= 1) {
      if (list[i]?.disabled !== true) return i;
    }
    return -1;
  };

  // Measure the trigger against the visual viewport (accounts for the
  // on-screen keyboard; falls back to the layout viewport) and let the pure
  // math pick a side + height. Re-run on visualViewport resize while open.
  const measure = (): void => {
    const el = triggerEl;
    if (el === undefined) return;
    const rect = el.getBoundingClientRect();
    const viewport = window.visualViewport;
    const height = viewport?.height ?? window.innerHeight;
    const offsetTop = viewport?.offsetTop ?? 0;
    setPlacement(panelPlacement(rect.top - offsetTop, rect.bottom - offsetTop, height));
  };

  const openPanel = (): void => {
    if (open()) return;
    props.onOpen?.();
    setQuery('');
    setOpen(true);
    measure();
    const list = visible();
    const current = list.findIndex(
      (entry) => entry.value === props.value && entry.disabled !== true,
    );
    setActive(current !== -1 ? current : firstEnabled(list));
    // Focus lands in the filter input when present, else on the listbox.
    queueMicrotask(() => (searchEl ?? listEl)?.focus());
  };

  const close = (refocus: boolean): void => {
    setOpen(false);
    if (refocus) triggerEl?.focus();
  };

  const commit = (entry: SelectOption): void => {
    if (entry.disabled === true) return;
    props.onChange(entry.value);
    close(true);
  };

  // Outside mousedown closes: a document listener mounted only while open.
  // The trigger itself is inside the root, so its own click stays a toggle.
  createEffect(() => {
    if (!open()) return;
    const onDocMouseDown = (e: MouseEvent): void => {
      const target = e.target as Node | null;
      if (target === null || rootEl === undefined || !rootEl.contains(target)) close(false);
    };
    document.addEventListener('mousedown', onDocMouseDown);
    onCleanup(() => document.removeEventListener('mousedown', onDocMouseDown));
    const viewport = window.visualViewport;
    if (viewport !== null && viewport !== undefined) {
      viewport.addEventListener('resize', measure);
      onCleanup(() => viewport.removeEventListener('resize', measure));
    }
  });

  // Keep the keyboard-active row in view inside the scrolling listbox
  // (scrollIntoView is absent under jsdom — the optional call is a no-op).
  createEffect(() => {
    if (!open() || active() < 0) return;
    document.getElementById(optionId(active()))?.scrollIntoView?.({ block: 'nearest' });
  });

  const moveActive = (delta: 1 | -1): void => {
    const list = visible();
    let index = active();
    for (;;) {
      index += delta;
      if (index < 0 || index >= list.length) return;
      if (list[index]?.disabled !== true) break;
    }
    setActive(index);
  };

  const onTriggerClick = (): void => {
    if (open()) close(false);
    else openPanel();
  };

  const onTriggerKeyDown = (e: KeyboardEvent): void => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      openPanel();
    }
  };

  // One handler on the panel serves both the filter input and the listbox.
  // Home/End stay with the input's text cursor while it has focus (the
  // WAI-ARIA combobox convention); on the listbox they jump rows.
  const onPanelKeyDown = (e: KeyboardEvent): void => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      moveActive(1);
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      moveActive(-1);
    } else if (e.key === 'Home' && e.target !== searchEl) {
      e.preventDefault();
      setActive(firstEnabled(visible()));
    } else if (e.key === 'End' && e.target !== searchEl) {
      e.preventDefault();
      setActive(lastEnabled(visible()));
    } else if (e.key === 'Enter') {
      e.preventDefault(); // never submit an enclosing form
      const entry = visible()[active()];
      if (entry !== undefined) commit(entry);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      close(true);
    }
  };

  const activeId = () => (active() >= 0 ? optionId(active()) : undefined);

  // Placement is driven inline (top/bottom + max-height) from panelPlacement;
  // the stylesheet only carries the look.
  const panelStyle = (): JSX.CSSProperties => ({
    'max-height': `${placement().maxHeight}px`,
    top: placement().openAbove ? 'auto' : `calc(100% + ${PANEL_MARGIN}px)`,
    bottom: placement().openAbove ? `calc(100% + ${PANEL_MARGIN}px)` : 'auto',
  });

  const chip = () => props.skin === 'chip';

  // `trigger` and `panel` are created once and mounted by exactly one branch
  // of the skin <Show> below — `skin` is fixed per call site.
  const trigger = (
    <button
      ref={triggerEl}
      type="button"
      class={chip() ? 'composer-chip select-chip' : 'select-field-trigger'}
      name={props.name}
      aria-label={chip() ? props.label : undefined}
      aria-labelledby={chip() ? undefined : labelId}
      aria-haspopup="listbox"
      aria-expanded={open()}
      disabled={props.disabled === true}
      onClick={onTriggerClick}
      onKeyDown={onTriggerKeyDown}
    >
      <Show when={chip()}>{props.icon}</Show>
      <span class={chip() ? 'composer-chip-label' : 'select-field-label'}>{triggerLabel()}</span>
      <Icon
        name="chevron-down"
        size={chip() ? 14 : 16}
        class={chip() ? 'composer-chip-caret' : 'select-field-caret'}
      />
    </button>
  );

  const panel = (
    <Show when={open()}>
      <div class="composer-pop-panel select-panel" style={panelStyle()} onKeyDown={onPanelKeyDown}>
        <Show when={searchOn()}>
          <input
            ref={searchEl}
            class="select-search"
            type="text"
            placeholder="Search…"
            aria-label="Search"
            autocomplete="off"
            aria-controls={listboxId}
            aria-activedescendant={activeId()}
            value={query()}
            onInput={(e) => {
              setQuery(e.currentTarget.value);
              setActive(firstEnabled(visible()));
            }}
          />
        </Show>
        <div
          ref={listEl}
          id={listboxId}
          class="select-listbox"
          role="listbox"
          tabindex="-1"
          aria-label={props.label}
          aria-activedescendant={activeId()}
        >
          <For each={visible()}>
            {(entry, index) => (
              <button
                type="button"
                id={optionId(index())}
                role="option"
                classList={{
                  'select-option': true,
                  active: index() === active(),
                  selected: entry.value === props.value,
                }}
                aria-selected={entry.value === props.value}
                aria-disabled={entry.disabled === true ? 'true' : undefined}
                disabled={entry.disabled === true}
                onMouseEnter={() => {
                  if (entry.disabled !== true) setActive(index());
                }}
                onClick={() => commit(entry)}
              >
                <span class="select-option-label">{entry.label}</span>
                <Show when={entry.status !== undefined && entry.status !== ''}>
                  <span class="select-option-status">{entry.status}</span>
                </Show>
              </button>
            )}
          </For>
        </div>
      </div>
    </Show>
  );

  return (
    <Show
      when={!chip()}
      fallback={
        <div ref={rootEl} class="select-pop">
          {trigger}
          {panel}
        </div>
      }
    >
      <div class="field">
        <span id={labelId}>{props.label}</span>
        <div ref={rootEl} class="select-pop select-pop-field">
          {trigger}
          {panel}
        </div>
      </div>
    </Show>
  );
}
