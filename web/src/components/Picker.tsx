// The one pick surface of the New run page (issue #66): a bottom sheet below
// 1024px and a popover anchored to its trigger at and above it — the Model
// and Effort pickers (on their composer chip), More options (the ⋯ chip), the
// repository picker (the "All N" pill), the issue picker (a wide popover) and
// the issue action sheet. Given no anchor, the desktop shape is a centered
// dialog instead (the mockup's openPicker falls back to openSheet the same way).
//
// The phone/desktop switch is CSS alone (styles/picker.css, the Dialog
// precedent): the panel always carries its measured desktop placement as
// custom properties, and only the >=1024px rules read them. The placement
// math is the pure pickerPlacement() below (jsdom has no layout); the panel
// re-measures on resize and on any scroll outside itself, so it stays on its
// anchor even when the content column (not the body) scrolls.
//
// Being modal is lib/modalStack.ts's createModal(): focus moves in (to
// `initialFocus()`, else the selected option, else the title), Tab is trapped,
// Escape closes, focus returns to the opener on close, and the body does not
// scroll — on the phone sheet that is the point; on the desktop popover it is
// kept too, since the panel is fixed-positioned against a one-time measure of
// its anchor and a page that scrolled under it would leave it floating.
//
// Dismissal: on the phone a scrim tap; on desktop there is no dimming (the
// scrim is hidden) and a mousedown anywhere outside the panel closes, so one
// click on another control both closes the picker and lands on that control.
// A mousedown on the anchor itself is left to the host's click handler, which
// should toggle — otherwise the click would reopen what the mousedown closed.
//
// The panel renders in a Portal on document.body: the composer's chip row
// scrolls horizontally (overflow-x: auto), which clips any absolutely
// positioned descendant, so the picker must never live inside it.
//
// Also here: the presentational rows every picker shares (PickerList,
// PickerOption, PickerGroup, PickerSearch, PickerFilters, PickerHint), so the
// five pickers read as one family. PickerSearch reuses the unified Select's
// filter input (`select-search`).

import { For, Show, createSignal, createUniqueId, onCleanup, onMount, type JSX } from 'solid-js';
import { Portal } from 'solid-js/web';
import { createModal } from '../lib/modalStack';
import Icon from './Icon';

/** Desktop popover widths, px, by `size`. */
export const PICKER_WIDTHS = { narrow: 320, regular: 400, wide: 560 } as const;
/** Gap between the anchor and the popover. */
export const PICKER_GAP = 6;
/** Minimum distance from the popover to the viewport edge. */
export const PICKER_EDGE = 8;
/** Popover height cap when space allows. */
export const PICKER_MAX_HEIGHT = 560;
/** Below is kept while it offers at least this much height (the mockup's 320). */
export const PICKER_FLIP_MIN = 320;

export interface PickerPlacement {
  /** True when the popover opens above its anchor. */
  openAbove: boolean;
  /** Viewport px from the top edge (opening below), else null. */
  top: number | null;
  /** Viewport px from the bottom edge (opening above), else null. */
  bottom: number | null;
  left: number;
  width: number;
  maxHeight: number;
}

/**
 * Pure placement math for the desktop popover (unit-testable under jsdom).
 * Coordinates are viewport px (getBoundingClientRect, position: fixed).
 *
 * Horizontal: the popover's left edge aligns with the anchor's, clamped so the
 * whole width (itself capped to the viewport minus both edges) stays on-screen.
 * Vertical: below is preferred; it flips above only when below offers less
 * than PICKER_FLIP_MIN AND above offers more. The side's free space keeps
 * `gap` to the anchor and `edge` to the viewport, and maxHeight clamps to it
 * (and to PICKER_MAX_HEIGHT), so the panel always fits with internal scroll.
 */
export function pickerPlacement(
  anchor: { top: number; bottom: number; left: number },
  viewport: { width: number; height: number },
  width: number,
  gap: number = PICKER_GAP,
  edge: number = PICKER_EDGE,
): PickerPlacement {
  const fitted = Math.max(0, Math.min(width, viewport.width - edge * 2));
  const left = Math.max(edge, Math.min(anchor.left, viewport.width - edge - fitted));
  const below = viewport.height - anchor.bottom - gap - edge;
  const above = anchor.top - gap - edge;
  const openAbove = below < PICKER_FLIP_MIN && above > below;
  const free = Math.max(0, openAbove ? above : below);
  return {
    openAbove,
    top: openAbove ? null : anchor.bottom + gap,
    bottom: openAbove ? viewport.height - anchor.top + gap : null,
    left,
    width: fitted,
    maxHeight: Math.min(free, PICKER_MAX_HEIGHT),
  };
}

export interface PickerProps {
  open: boolean;
  /** Scrim tap, Escape, the close button, an outside mousedown (desktop). */
  onClose: () => void;
  /** The heading; also the panel's accessible name. */
  title: JSX.Element;
  /** Desktop popover width: narrow ~320 (model/effort/more), regular ~400
   *  (repo), wide ~560 (issues). Default narrow. */
  size?: 'narrow' | 'regular' | 'wide';
  /** The element the desktop popover anchors to (the chip / pill / row);
   *  none → a centered dialog on desktop. */
  anchor?: () => HTMLElement | undefined;
  /** The element to focus on open (e.g. the search input); default: the
   *  selected option, else the title. */
  initialFocus?: () => HTMLElement | undefined;
  /** Pinned area above the scrolling body (search field, filter chips). */
  header?: JSX.Element;
  /** The scrolling body. */
  children: JSX.Element;
  /** Extra classes on the panel. */
  class?: string;
}

export default function Picker(props: PickerProps): JSX.Element {
  return (
    <Show when={props.open}>
      <Portal>
        <PickerPanel {...props} />
      </Portal>
    </Show>
  );
}

function PickerPanel(props: PickerProps) {
  const titleId = `picker-${createUniqueId()}-title`;
  let panel: HTMLDivElement | undefined;
  let heading: HTMLHeadingElement | undefined;
  let scrim: HTMLDivElement | undefined;

  const [place, setPlace] = createSignal<PickerPlacement | null>(null);
  const width = () => PICKER_WIDTHS[props.size ?? 'narrow'];

  const measure = (): void => {
    const el = props.anchor?.();
    // An anchor that left the document mid-open keeps the last placement.
    if (el === undefined || !el.isConnected) return;
    const root = document.documentElement;
    setPlace(
      pickerPlacement(
        el.getBoundingClientRect(),
        {
          width: root.clientWidth || window.innerWidth,
          height: root.clientHeight || window.innerHeight,
        },
        width(),
      ),
    );
  };

  // Registered before createModal so it runs after createModal's cleanup
  // (Solid runs cleanups last-registered first): when the opener could not
  // take focus back — Safari does not focus a clicked button — the anchor does.
  onCleanup(() => {
    const active = document.activeElement;
    if (active !== null && active !== document.body) return;
    const el = props.anchor?.();
    if (el?.isConnected === true) el.focus();
  });

  createModal({
    panel: () => panel,
    fallback: () => heading,
    initialFocus: () =>
      props.initialFocus?.() ??
      panel?.querySelector<HTMLElement>('[role="option"][aria-selected="true"]:not(:disabled)') ??
      undefined,
    onEscape: (event) => {
      event.preventDefault();
      event.stopPropagation();
      props.onClose();
    },
  });

  // Measured once mounted (still before the first paint), then on resize
  // and on any scroll outside the panel.
  onMount(() => {
    measure();
    const onMouseDown = (event: MouseEvent): void => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      // The scrim's own click closes (phone): closing on its mousedown would
      // hand the click that follows to whatever sits under it.
      if (target === scrim || panel?.contains(target) === true) return;
      if (props.anchor?.()?.contains(target) === true) return;
      props.onClose();
    };
    const onScroll = (event: Event): void => {
      if (event.target instanceof Node && panel?.contains(event.target) === true) return;
      measure();
    };
    document.addEventListener('mousedown', onMouseDown);
    window.addEventListener('resize', measure);
    window.addEventListener('scroll', onScroll, true);
    onCleanup(() => {
      document.removeEventListener('mousedown', onMouseDown);
      window.removeEventListener('resize', measure);
      window.removeEventListener('scroll', onScroll, true);
    });
  });

  // Custom properties only the >=1024px rules read; the phone sheet ignores them.
  const style = (): JSX.CSSProperties => {
    const p = place();
    if (p === null) return { '--picker-width': `${width()}px` };
    return {
      '--picker-top': p.top === null ? 'auto' : `${p.top}px`,
      '--picker-bottom': p.bottom === null ? 'auto' : `${p.bottom}px`,
      '--picker-left': `${p.left}px`,
      '--picker-width': `${p.width}px`,
      '--picker-max-height': `${p.maxHeight}px`,
    };
  };

  const centered = () => place() === null;
  const panelClass = () =>
    [
      'picker',
      centered() ? 'picker-centered' : '',
      place()?.openAbove === true ? 'picker-above' : '',
      props.class ?? '',
    ]
      .filter((name) => name !== '')
      .join(' ');

  // Scrim and panel are siblings (the Dialog/InstallSheet idiom): the close
  // handler lives on the scrim alone. Both stop touchstart, like RunDetails'
  // sheet, so window-level touch listeners behind the picker never see its touches.
  return (
    <>
      <div
        ref={scrim}
        classList={{ 'picker-scrim': true, 'picker-centered': centered() }}
        aria-hidden="true"
        onClick={() => props.onClose()}
        onTouchStart={(e) => e.stopPropagation()}
      />
      <div
        ref={panel}
        class={panelClass()}
        style={style()}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        onTouchStart={(e) => e.stopPropagation()}
      >
        <span class="picker-grab" aria-hidden="true" />
        <div class="picker-head">
          <h2 class="picker-title" id={titleId} ref={heading} tabIndex={-1}>
            {props.title}
          </h2>
          <button
            type="button"
            class="icon-btn picker-close"
            aria-label="Close"
            onClick={() => props.onClose()}
          >
            <Icon name="x" />
          </button>
        </div>
        <Show when={props.header}>
          <div class="picker-header">{props.header}</div>
        </Show>
        <div class="picker-body">{props.children}</div>
      </div>
    </>
  );
}

/** The option list: a listbox wrapper around PickerOption rows. */
export function PickerList(props: {
  /** The list's accessible name (e.g. "Models"), or… */
  label?: string;
  /** …the id of a visible label (a PickerGroup's `id`). */
  labelledBy?: string;
  children: JSX.Element;
  class?: string;
}): JSX.Element {
  return (
    <div
      class={props.class === undefined ? 'picker-list' : `picker-list ${props.class}`}
      role="listbox"
      aria-label={props.label}
      aria-labelledby={props.labelledBy}
    >
      {props.children}
    </div>
  );
}

/** One full-width pick row (>=44px): title, optional muted description,
 *  optional trailing status, a "default" tag, a check when selected. A
 *  disabled row stays visible (with its status, e.g. "cloning 42%") and never
 *  fires onSelect. */
export function PickerOption(props: {
  selected: boolean;
  onSelect: () => void;
  title: JSX.Element;
  /** Muted text after the title on the same line (e.g. the repo's host). */
  description?: JSX.Element;
  /** Trailing muted status text (e.g. "clone failed", "last used"). */
  status?: JSX.Element;
  /** Marks the inherited default with a trailing "default" tag. */
  isDefault?: boolean;
  disabled?: boolean;
  /** Leading decoration (e.g. a readiness dot). */
  leading?: JSX.Element;
  id?: string;
  class?: string;
}): JSX.Element {
  return (
    <button
      type="button"
      id={props.id}
      class={props.class === undefined ? 'picker-option' : `picker-option ${props.class}`}
      role="option"
      aria-selected={props.selected}
      aria-disabled={props.disabled === true ? 'true' : undefined}
      disabled={props.disabled === true}
      onClick={() => {
        if (props.disabled !== true) props.onSelect();
      }}
    >
      <Show when={props.leading}>{props.leading}</Show>
      <span class="picker-option-text">
        <span class="picker-option-title">{props.title}</span>
        <Show when={props.description}>
          {' '}
          <span class="picker-option-desc">· {props.description}</span>
        </Show>
      </span>
      <Show when={props.status}>
        <span class="picker-option-status">{props.status}</span>
      </Show>
      <Show when={props.isDefault === true}>
        <span class="picker-option-default">default</span>
      </Show>
      <Show when={props.selected}>
        <Icon name="check" size={16} class="picker-option-check" />
      </Show>
    </button>
  );
}

/** A small uppercase group label ("Recent", "All repositories · 9"). Give it
 *  an `id` and pass that to the following PickerList's `labelledBy`. */
export function PickerGroup(props: { children: JSX.Element; id?: string }): JSX.Element {
  return (
    <p class="picker-group" id={props.id}>
      {props.children}
    </p>
  );
}

/** The filter input: the unified Select's `select-search` with a leading
 *  search glyph. Enter calls onEnter (the host picks its first enabled match)
 *  and never submits an enclosing form. */
export function PickerSearch(props: {
  value: string;
  onInput: (value: string) => void;
  placeholder?: string;
  'aria-label': string;
  onEnter?: () => void;
  ref?: HTMLInputElement | ((el: HTMLInputElement) => void);
  id?: string;
  /** The id of the list it filters. */
  'aria-controls'?: string;
}): JSX.Element {
  return (
    <div class="picker-search">
      <Icon name="search" size={16} class="picker-search-icon" />
      <input
        ref={props.ref}
        id={props.id}
        class="select-search"
        type="text"
        enterkeyhint="go"
        autocomplete="off"
        spellcheck={false}
        placeholder={props.placeholder}
        aria-label={props['aria-label']}
        aria-controls={props['aria-controls']}
        value={props.value}
        onInput={(e) => props.onInput(e.currentTarget.value)}
        onKeyDown={(e) => {
          if (e.key !== 'Enter' || e.isComposing) return;
          e.preventDefault();
          props.onEnter?.();
        }}
      />
    </div>
  );
}

export interface PickerFilter {
  value: string;
  label: string;
  /** Shown muted after the label; omitted → no count. */
  count?: number;
}

/** A row of toggle chips (aria-pressed) with counts — the issue picker's
 *  state filter. The host decides which chips to offer (e.g. hides zero
 *  counts); exactly one is pressed, the one whose value is `value`. */
export function PickerFilters(props: {
  items: PickerFilter[];
  value: string;
  onChange: (value: string) => void;
  'aria-label': string;
}): JSX.Element {
  return (
    <div class="picker-filters" role="group" aria-label={props['aria-label']}>
      <For each={props.items}>
        {(item) => (
          <button
            type="button"
            aria-pressed={item.value === props.value}
            onClick={() => props.onChange(item.value)}
          >
            {item.label}
            <Show when={item.count !== undefined}>
              {' '}
              <span class="picker-filter-count">{item.count}</span>
            </Show>
          </button>
        )}
      </For>
    </div>
  );
}

/** The one-line muted hint under a list ("The default comes from …"). */
export function PickerHint(props: { children: JSX.Element; id?: string }): JSX.Element {
  return (
    <p class="picker-hint" id={props.id}>
      {props.children}
    </p>
  );
}
