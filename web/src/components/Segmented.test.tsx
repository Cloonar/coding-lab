// Segmented contract (issue #61): a radiogroup named by its label (or
// aria-label / labelledBy) of role="radio" segments with aria-checked; one tab
// stop (roving tabindex on the checked segment); arrows and Home/End move focus
// over enabled segments with wrap-around and select the segment they land on
// (the ARIA radio group pattern); Space/Enter (the native click) and a click
// select; picking the checked segment fires nothing; disabled options and a
// disabled group are skipped and inert.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import Segmented, { type SegmentedOption, type SegmentedProps } from './Segmented';

const OPTIONS: SegmentedOption[] = [
  { value: '', label: 'Global · on' },
  { value: 'on', label: 'On' },
  { value: 'off', label: 'Off' },
];

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

function mount(props: Partial<SegmentedProps> = {}): { onChange: ReturnType<typeof vi.fn> } {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onChange = vi.fn();
  dispose = render(() => {
    const [value, setValue] = createSignal(props.value ?? '');
    return (
      <Segmented
        options={OPTIONS}
        {...(props['aria-label'] === undefined && props.labelledBy === undefined
          ? { label: 'Remote control' }
          : {})}
        {...props}
        value={value()}
        onChange={(next) => {
          onChange(next);
          setValue(next);
        }}
      />
    );
  }, container);
  return { onChange };
}

const group = () => container.querySelector<HTMLElement>('[role="radiogroup"]')!;
const radios = () => Array.from(container.querySelectorAll<HTMLButtonElement>('[role="radio"]'));
const radio = (label: string) => radios().find((r) => r.textContent === label)!;
const key = (el: HTMLElement, k: string) =>
  el.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true }));

describe('Segmented', () => {
  it('is a radiogroup named by its visible label, with checked state per segment', () => {
    mount({ value: 'on', name: 'remote_default' });
    const labelId = group().getAttribute('aria-labelledby');
    expect(document.getElementById(labelId!)?.textContent).toBe('Remote control');
    expect(radios().map((r) => r.getAttribute('aria-checked'))).toEqual(['false', 'true', 'false']);
    expect(radios().every((r) => r.getAttribute('name') === 'remote_default')).toBe(true);
    expect(radios().map((r) => r.value)).toEqual(['', 'on', 'off']);
  });

  it('takes aria-label or labelledBy when there is no visible label', () => {
    mount({ 'aria-label': 'Runner' });
    expect(group().getAttribute('aria-label')).toBe('Runner');
    dispose?.();
    container.remove();
    mount({ labelledBy: 'runner-label' });
    expect(group().getAttribute('aria-labelledby')).toBe('runner-label');
  });

  it('has one tab stop, on the checked segment', () => {
    mount({ value: 'off' });
    expect(radios().map((r) => r.tabIndex)).toEqual([-1, -1, 0]);
  });

  it('selects on click and ignores the already-checked segment', () => {
    const { onChange } = mount({ value: '' });
    radio('Off').click();
    expect(onChange).toHaveBeenCalledWith('off');
    expect(radio('Off').getAttribute('aria-checked')).toBe('true');
    radio('Off').click();
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it('moves focus and selection with the arrows and Home/End, wrapping', () => {
    const { onChange } = mount({ value: '' });
    const checked = () => radios().find((r) => r.getAttribute('aria-checked') === 'true');
    radios()[0]!.focus();

    // The ARIA radio group pattern: an arrow checks the segment it moves to,
    // so a screen reader user who arrows to an option and tabs on keeps it.
    key(radios()[0]!, 'ArrowRight');
    expect(document.activeElement).toBe(radio('On'));
    expect(checked()).toBe(radio('On'));
    expect(onChange).toHaveBeenLastCalledWith('on');
    expect(radio('On').tabIndex).toBe(0); // the tab stop follows
    key(radio('On'), 'ArrowDown');
    expect(document.activeElement).toBe(radio('Off'));
    expect(checked()).toBe(radio('Off'));
    key(radio('Off'), 'ArrowRight'); // wraps
    expect(document.activeElement).toBe(radio('Global · on'));
    expect(checked()).toBe(radio('Global · on'));
    key(radio('Global · on'), 'ArrowLeft'); // wraps back
    expect(document.activeElement).toBe(radio('Off'));
    expect(checked()).toBe(radio('Off'));
    key(radio('Off'), 'Home');
    expect(document.activeElement).toBe(radio('Global · on'));
    expect(checked()).toBe(radio('Global · on'));
    key(radio('Global · on'), 'End');
    expect(document.activeElement).toBe(radio('Off'));
    expect(checked()).toBe(radio('Off'));
    key(radio('Off'), 'ArrowUp');
    expect(document.activeElement).toBe(radio('On'));
    expect(checked()).toBe(radio('On'));
    expect(onChange.mock.calls.map((call) => call[0])).toEqual([
      'on',
      'off',
      '',
      'off',
      '',
      'off',
      'on',
    ]);
    expect(radios().map((r) => r.tabIndex)).toEqual([-1, 0, -1]);

    // Space/Enter on a native button is its click; the checked one fires nothing.
    radio('On').click();
    expect(onChange).toHaveBeenCalledTimes(7);
  });

  it('fires nothing when an arrow lands on the segment already checked', () => {
    const { onChange } = mount({
      value: 'off',
      options: [
        { ...OPTIONS[0]!, disabled: true },
        { ...OPTIONS[1]!, disabled: true },
        OPTIONS[2]!,
      ],
    });
    radio('Off').focus();
    key(radio('Off'), 'ArrowRight'); // the only enabled segment: stays put
    expect(document.activeElement).toBe(radio('Off'));
    expect(onChange).not.toHaveBeenCalled();
  });

  it('skips disabled options and never selects them', () => {
    const { onChange } = mount({
      value: '',
      options: [OPTIONS[0]!, { ...OPTIONS[1]!, disabled: true }, OPTIONS[2]!],
    });
    expect(radio('On').disabled).toBe(true);
    radios()[0]!.focus();
    key(radios()[0]!, 'ArrowRight');
    expect(document.activeElement).toBe(radio('Off'));
    expect(onChange).toHaveBeenCalledWith('off'); // the arrow skipped On and checked Off
    expect(radio('On').getAttribute('aria-checked')).toBe('false');
    radio('On').click();
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it('disables the whole group', () => {
    const { onChange } = mount({ value: 'on', disabled: true });
    expect(group().getAttribute('aria-disabled')).toBe('true');
    expect(radios().every((r) => r.disabled)).toBe(true);
    radio('Off').click();
    expect(onChange).not.toHaveBeenCalled();
  });

  it('carries its hint and error to the group: describedBy and invalid', () => {
    mount({ describedBy: 'binding-error binding-hint', invalid: true });
    const group = container.querySelector('[role="radiogroup"]')!;
    expect(group.getAttribute('aria-describedby')).toBe('binding-error binding-hint');
    expect(group.getAttribute('aria-invalid')).toBe('true');
  });

  it('is neither described nor invalid by default', () => {
    mount();
    const group = container.querySelector('[role="radiogroup"]')!;
    expect(group.hasAttribute('aria-describedby')).toBe(false);
    expect(group.hasAttribute('aria-invalid')).toBe(false);
  });

  it('stretches with `fill`', () => {
    mount({ fill: true });
    expect(group().classList.contains('fill')).toBe(true);
  });
});
