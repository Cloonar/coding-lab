// Picker contract (issue #66): closed renders nothing; open renders a scrim
// and a role="dialog" panel in a Portal on <body> (never inside the host, so a
// horizontally scrolling chip row cannot clip it), labelled by its title, with
// the pinned header and the scrolling body; Escape, the scrim, the close
// button and an outside mousedown call onClose (a mousedown inside the panel,
// on the scrim or on the anchor does not); focus moves to initialFocus, else
// the selected option, and returns to the opener on close.
//
// Building blocks: PickerOption carries role="option" + aria-selected, a
// check and a "default" tag, and a disabled row never fires; PickerSearch's
// Enter calls onEnter; PickerFilters are aria-pressed toggles with counts.
//
// pickerPlacement (pure): below preferred, flip above when below lacks room
// and above has more, max-height clamped, left clamped into the viewport.

import { Show, createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import Picker, {
  PICKER_EDGE,
  PICKER_GAP,
  PICKER_MAX_HEIGHT,
  PickerFilters,
  PickerGroup,
  PickerHint,
  PickerList,
  PickerOption,
  PickerSearch,
  pickerPlacement,
  type PickerProps,
} from './Picker';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  document.body.style.overflow = '';
});

const panel = () => document.querySelector<HTMLElement>('.picker');
const scrim = () => document.querySelector<HTMLElement>('.picker-scrim');
const key = (k: string) => {
  const event = new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true });
  (document.activeElement ?? document.body).dispatchEvent(event);
};
const mousedown = (el: Element) =>
  el.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));

function mount(
  props: Partial<PickerProps> = {},
  shape: { anchored?: boolean; options?: boolean } = {},
): {
  onClose: ReturnType<typeof vi.fn>;
  open: () => void;
  opener: () => HTMLButtonElement;
} {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onClose = vi.fn();
  dispose = render(() => {
    const [open, setOpen] = createSignal(false);
    const [model, setModel] = createSignal('sonnet');
    let chip: HTMLButtonElement | undefined;
    return (
      <>
        <div class="chips" style={{ 'overflow-x': 'auto' }}>
          <button type="button" class="opener" ref={chip} onClick={() => setOpen((v) => !v)}>
            Model
          </button>
        </div>
        <button type="button" class="elsewhere">
          Elsewhere
        </button>
        <Picker
          open={open()}
          onClose={() => {
            onClose();
            setOpen(false);
          }}
          title="Model"
          anchor={shape.anchored === false ? undefined : () => chip}
          header={<p class="pinned">pinned header</p>}
          {...props}
        >
          <Show when={shape.options !== false}>
            <PickerList label="Models">
              <PickerOption
                selected={model() === 'opus'}
                onSelect={() => setModel('opus')}
                title="Opus"
                class="opt-opus"
              />
              <PickerOption
                selected={model() === 'sonnet'}
                onSelect={() => setModel('sonnet')}
                title="Sonnet"
                isDefault
                class="opt-sonnet"
              />
            </PickerList>
          </Show>
          <PickerHint>A pick here applies to this run only.</PickerHint>
        </Picker>
      </>
    );
  }, container);
  const opener = () => container.querySelector<HTMLButtonElement>('.opener')!;
  return {
    onClose,
    opener,
    open: () => {
      opener().focus();
      opener().click();
    },
  };
}

describe('Picker', () => {
  it('renders nothing while closed', () => {
    mount();
    expect(panel()).toBeNull();
    expect(scrim()).toBeNull();
  });

  it('renders title, pinned header and body in a portal outside the host', () => {
    mount();
    container.querySelector<HTMLButtonElement>('.opener')!.click();
    const p = panel()!;
    expect(p).not.toBeNull();
    expect(container.contains(p)).toBe(false);
    expect(document.body.contains(p)).toBe(true);
    expect(p.getAttribute('role')).toBe('dialog');
    expect(p.getAttribute('aria-modal')).toBe('true');
    const title = document.getElementById(p.getAttribute('aria-labelledby')!)!;
    expect(title.textContent).toBe('Model');
    expect(p.querySelector('.picker-header .pinned')?.textContent).toBe('pinned header');
    expect(p.querySelector('.picker-body .picker-hint')?.textContent).toContain('this run only');
    expect(p.querySelector('.picker-header .picker-option')).toBeNull();
  });

  it('closes on Escape, the scrim and the close button', () => {
    const { onClose, open } = mount();
    open();
    key('Escape');
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(panel()).toBeNull();

    open();
    scrim()!.click();
    expect(onClose).toHaveBeenCalledTimes(2);

    open();
    panel()!.querySelector<HTMLButtonElement>('button[aria-label="Close"]')!.click();
    expect(onClose).toHaveBeenCalledTimes(3);
  });

  it('closes on an outside mousedown, but not inside the panel, on the scrim or the anchor', () => {
    const { onClose, open, opener } = mount();
    open();
    mousedown(panel()!.querySelector('.opt-opus')!);
    mousedown(scrim()!);
    mousedown(opener());
    expect(onClose).not.toHaveBeenCalled();
    mousedown(container.querySelector('.elsewhere')!);
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(panel()).toBeNull();
  });

  it('moves focus to the selected option by default and back to the opener on close', () => {
    const { open, opener } = mount();
    open();
    expect(document.activeElement).toBe(panel()!.querySelector('.opt-sonnet'));
    key('Escape');
    expect(document.activeElement).toBe(opener());
  });

  it('moves focus to initialFocus when given', () => {
    mount({
      header: <input class="q" aria-label="Filter" />,
      initialFocus: () => document.querySelector<HTMLElement>('.picker .q') ?? undefined,
    });
    container.querySelector<HTMLButtonElement>('.opener')!.click();
    expect(document.activeElement).toBe(panel()!.querySelector('.q'));
  });

  it('falls back to the title when nothing is selected', () => {
    mount({}, { options: false });
    container.querySelector<HTMLButtonElement>('.opener')!.click();
    expect(document.activeElement?.classList.contains('picker-title')).toBe(true);
  });

  it('is a centered dialog on desktop when it has no anchor', () => {
    mount({}, { anchored: false });
    container.querySelector<HTMLButtonElement>('.opener')!.click();
    expect(panel()!.classList.contains('picker-centered')).toBe(true);
    expect(scrim()!.classList.contains('picker-centered')).toBe(true);
  });

  it('carries its anchored placement as custom properties when anchored', () => {
    mount({ size: 'wide' });
    container.querySelector<HTMLButtonElement>('.opener')!.click();
    const p = panel()!;
    expect(p.classList.contains('picker-centered')).toBe(false);
    expect(p.style.getPropertyValue('--picker-left')).toMatch(/px$/);
    expect(p.style.getPropertyValue('--picker-max-height')).toMatch(/px$/);
  });

  it('locks page scroll while open', () => {
    const { open } = mount();
    open();
    expect(document.body.style.overflow).toBe('hidden');
    key('Escape');
    expect(document.body.style.overflow).toBe('');
  });
});

describe('PickerOption', () => {
  function mountOption(props: { selected: boolean; disabled?: boolean; isDefault?: boolean }) {
    container = document.createElement('div');
    document.body.appendChild(container);
    const onSelect = vi.fn();
    dispose = render(
      () => (
        <PickerList label="Repositories">
          <PickerOption
            {...props}
            onSelect={onSelect}
            title="coding-lab"
            description="github.com"
            status={props.disabled === true ? 'cloning 42%' : undefined}
          />
        </PickerList>
      ),
      container,
    );
    return { onSelect, row: container.querySelector<HTMLButtonElement>('[role="option"]')! };
  }

  it('is an option inside a labelled listbox, with a check and a default tag when selected', () => {
    const { row, onSelect } = mountOption({ selected: true, isDefault: true });
    expect(container.querySelector('[role="listbox"]')?.getAttribute('aria-label')).toBe(
      'Repositories',
    );
    expect(row.getAttribute('aria-selected')).toBe('true');
    expect(row.querySelector('.picker-option-check')).not.toBeNull();
    expect(row.querySelector('.picker-option-default')?.textContent).toBe('default');
    expect(row.querySelector('.picker-option-desc')?.textContent).toBe('· github.com');
    row.click();
    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  it('shows no check when unselected', () => {
    const { row } = mountOption({ selected: false });
    expect(row.getAttribute('aria-selected')).toBe('false');
    expect(row.querySelector('.picker-option-check')).toBeNull();
    expect(row.querySelector('.picker-option-default')).toBeNull();
  });

  it('keeps a disabled row visible with its status and never fires', () => {
    const { row, onSelect } = mountOption({ selected: false, disabled: true });
    expect(row.disabled).toBe(true);
    expect(row.getAttribute('aria-disabled')).toBe('true');
    expect(row.querySelector('.picker-option-status')?.textContent).toBe('cloning 42%');
    row.click();
    expect(onSelect).not.toHaveBeenCalled();
  });
});

describe('PickerSearch, PickerFilters, PickerGroup', () => {
  it('reuses select-search, reports input and calls onEnter on Enter', () => {
    container = document.createElement('div');
    document.body.appendChild(container);
    const onEnter = vi.fn();
    const onInput = vi.fn();
    let el: HTMLInputElement | undefined;
    dispose = render(
      () => (
        <form onSubmit={() => onInput('submitted')}>
          <PickerSearch
            ref={(e) => (el = e)}
            value=""
            onInput={onInput}
            onEnter={onEnter}
            placeholder="Type a repository name"
            aria-label="Filter repositories"
          />
        </form>
      ),
      container,
    );
    const input = container.querySelector<HTMLInputElement>('input')!;
    expect(el).toBe(input);
    expect(input.classList.contains('select-search')).toBe(true);
    expect(input.getAttribute('aria-label')).toBe('Filter repositories');
    expect(container.querySelector('.picker-search svg')).not.toBeNull();
    input.value = 'lab';
    input.dispatchEvent(new InputEvent('input', { bubbles: true }));
    expect(onInput).toHaveBeenLastCalledWith('lab');
    const enter = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    input.dispatchEvent(enter);
    expect(onEnter).toHaveBeenCalledTimes(1);
    expect(enter.defaultPrevented).toBe(true);
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'a', bubbles: true }));
    expect(onEnter).toHaveBeenCalledTimes(1);
  });

  it('renders aria-pressed filter chips with counts and reports a change', () => {
    container = document.createElement('div');
    document.body.appendChild(container);
    const onChange = vi.fn();
    dispose = render(
      () => (
        <>
          <PickerGroup id="g-all">All repositories · 9</PickerGroup>
          <PickerFilters
            aria-label="Issue state"
            value="all"
            onChange={onChange}
            items={[
              { value: 'all', label: 'All', count: 12 },
              { value: 'needs-triage', label: 'needs-triage', count: 3 },
            ]}
          />
        </>
      ),
      container,
    );
    expect(container.querySelector('.picker-group')?.textContent).toBe('All repositories · 9');
    const buttons = container.querySelectorAll<HTMLButtonElement>('.picker-filters button');
    expect(buttons.length).toBe(2);
    expect(buttons[0]!.getAttribute('aria-pressed')).toBe('true');
    expect(buttons[1]!.getAttribute('aria-pressed')).toBe('false');
    expect(buttons[1]!.querySelector('.picker-filter-count')?.textContent).toBe('3');
    buttons[1]!.click();
    expect(onChange).toHaveBeenCalledWith('needs-triage');
  });
});

describe('pickerPlacement', () => {
  const vp = { width: 1280, height: 800 };

  it('opens below, aligned to the anchor, with the full cap when there is room', () => {
    expect(pickerPlacement({ top: 100, bottom: 140, left: 400 }, vp, 320)).toEqual({
      openAbove: false,
      top: 140 + PICKER_GAP,
      bottom: null,
      left: 400,
      width: 320,
      maxHeight: Math.min(800 - 140 - PICKER_GAP - PICKER_EDGE, PICKER_MAX_HEIGHT),
    });
  });

  it('clamps max-height to the space below while it stays below', () => {
    // 800 - 400 - 6 - 8 = 386 free below (>= 320): stays below, clamped.
    const p = pickerPlacement({ top: 360, bottom: 400, left: 400 }, vp, 320);
    expect(p.openAbove).toBe(false);
    expect(p.maxHeight).toBe(386);
  });

  it('flips above when below lacks room and above has more', () => {
    // composer low on the page: 800 - 700 - 14 = 86 below, 650 - 14 = 636 above.
    const p = pickerPlacement({ top: 650, bottom: 700, left: 400 }, vp, 320);
    expect(p.openAbove).toBe(true);
    expect(p.top).toBeNull();
    expect(p.bottom).toBe(800 - 650 + PICKER_GAP);
    expect(p.maxHeight).toBe(PICKER_MAX_HEIGHT);
  });

  it('stays below when both sides are cramped and below is the larger', () => {
    const p = pickerPlacement({ top: 60, bottom: 100, left: 0 }, { width: 1280, height: 300 }, 320);
    expect(p.openAbove).toBe(false);
    expect(p.maxHeight).toBe(300 - 100 - PICKER_GAP - PICKER_EDGE);
  });

  it('clamps the left edge so the popover stays inside the viewport', () => {
    // ⋯ chip near the right edge: 1280 - 8 - 560 = 712.
    expect(pickerPlacement({ top: 100, bottom: 140, left: 1200 }, vp, 560).left).toBe(712);
    // an anchor left of the edge inset
    expect(pickerPlacement({ top: 100, bottom: 140, left: 2 }, vp, 320).left).toBe(PICKER_EDGE);
  });

  it('caps the width to the viewport minus both edges', () => {
    const p = pickerPlacement(
      { top: 100, bottom: 140, left: 50 },
      { width: 400, height: 800 },
      560,
    );
    expect(p.width).toBe(400 - PICKER_EDGE * 2);
    expect(p.left).toBe(PICKER_EDGE);
  });
});
