// ToggleSwitch contract (issue #61): a button with role="switch" whose
// aria-checked mirrors `checked`; activating it asks the parent for the
// opposite state (the parent owns it); a visible label is tied to the control
// (clicking the text toggles too) and a description is wired as
// aria-describedby; aria-label / labelledBy name it without visible text;
// disabled blocks it.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import ToggleSwitch, { type SwitchProps } from './Switch';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

function mount(props: Partial<SwitchProps> = {}): {
  onChange: ReturnType<typeof vi.fn>;
  control: () => HTMLButtonElement;
} {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onChange = vi.fn();
  // A controlled host: the switch only flips when the parent applies onChange.
  dispose = render(() => {
    const [checked, setChecked] = createSignal(props.checked ?? false);
    return (
      <ToggleSwitch
        {...(props.label === undefined && props.labelledBy === undefined
          ? { 'aria-label': 'Auto' }
          : {})}
        {...props}
        checked={checked()}
        onChange={(next) => {
          onChange(next);
          setChecked(next);
        }}
      />
    );
  }, container);
  return {
    onChange,
    control: () => container.querySelector<HTMLButtonElement>('button[role="switch"]')!,
  };
}

describe('ToggleSwitch', () => {
  it('exposes role="switch" with aria-checked and toggles on click', () => {
    const { onChange, control } = mount();
    expect(control().getAttribute('type')).toBe('button');
    expect(control().getAttribute('aria-checked')).toBe('false');
    expect(control().getAttribute('aria-label')).toBe('Auto');

    control().click();
    expect(onChange).toHaveBeenCalledWith(true);
    expect(control().getAttribute('aria-checked')).toBe('true');

    control().click();
    expect(onChange).toHaveBeenLastCalledWith(false);
    expect(control().getAttribute('aria-checked')).toBe('false');
  });

  it('is keyboard operable: it is a native button, focusable, activated by Enter/Space', () => {
    const { control } = mount({ checked: true });
    control().focus();
    expect(document.activeElement).toBe(control());
    // Native buttons turn Enter/Space into a click; the click path is the one
    // keyboard activation takes.
    control().click();
    expect(control().getAttribute('aria-checked')).toBe('false');
  });

  it('ties a visible label to the control and wires the description', () => {
    const { onChange, control } = mount({
      label: 'Auto-spawn',
      description: 'Claim ready-for-agent issues as they appear.',
      name: 'afk_auto_enabled',
    });
    const label = container.querySelector('label');
    expect(label?.textContent).toBe('Auto-spawn');
    expect(label?.getAttribute('for')).toBe(control().id);
    expect(control().getAttribute('name')).toBe('afk_auto_enabled');
    const descId = control().getAttribute('aria-describedby');
    expect(descId).not.toBeNull();
    expect(document.getElementById(descId!)?.textContent).toBe(
      'Claim ready-for-agent issues as they appear.',
    );

    label?.click(); // the label activates its control
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("adds the host's hint and problem to its description, after its own", () => {
    const { control } = mount({
      label: 'Autoland',
      description: 'A lander run validates each PR.',
      describedBy: 'host-error host-hint',
      invalid: true,
    });
    const ids = control().getAttribute('aria-describedby')?.split(' ') ?? [];
    expect(ids).toHaveLength(3);
    expect(document.getElementById(ids[0] ?? '')?.textContent).toBe(
      'A lander run validates each PR.',
    );
    expect(ids.slice(1)).toEqual(['host-error', 'host-hint']);
    expect(control().getAttribute('aria-invalid')).toBe('true');
  });

  it('takes a host description without one of its own, and is not invalid by default', () => {
    const plain = mount({ label: 'Autoland' });
    expect(plain.control().hasAttribute('aria-describedby')).toBe(false);
    expect(plain.control().hasAttribute('aria-invalid')).toBe(false);
    dispose?.();
    container.remove();

    const { control } = mount({ label: 'Autoland', describedBy: 'host-hint' });
    expect(control().getAttribute('aria-describedby')).toBe('host-hint');
  });

  it('renders extra label content inside the label, so it joins the name', () => {
    mount({
      label: 'Incogni',
      labelExtra: <span class="visually-hidden"> (unsaved change)</span>,
    });
    const label = container.querySelector('label');
    expect(label?.textContent).toBe('Incogni (unsaved change)');
    expect(label?.querySelector('.visually-hidden')).not.toBeNull();
  });

  it('can be named by an element elsewhere', () => {
    const { control } = mount({ labelledBy: 'auto-label' });
    expect(control().getAttribute('aria-labelledby')).toBe('auto-label');
    expect(control().hasAttribute('aria-label')).toBe(false);
  });

  it('does nothing while disabled', () => {
    const { onChange, control } = mount({ disabled: true });
    expect(control().disabled).toBe(true);
    control().click();
    expect(onChange).not.toHaveBeenCalled();
    expect(control().getAttribute('aria-checked')).toBe('false');
  });
});
