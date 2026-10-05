// Segmented control (issue #61): one pick out of a few short options shown
// side by side — the three-way remote control ("Global · on | On | Off"), the
// Runner, the tracker binding, a schedule's cadence mode.
//
// Semantics are a radio group: role="radiogroup" named by a visible label
// (`label`, or `labelledBy` pointing at one rendered elsewhere) or by
// `aria-label`; each segment is role="radio" with aria-checked. Keyboard, as
// in the ARIA radio group pattern: one tab stop (roving tabindex on the
// checked segment, else the first enabled one); the arrow keys and Home/End
// move to an enabled segment AND select it, skipping disabled ones, so what a
// screen reader announces while arrowing is what the form holds; Space or
// Enter selects the focused one, as does a click. The parent owns `value` and
// gets the pick through onChange — choosing the already-checked segment fires
// nothing.
//
// The checked segment is raised as a bordered pill in a heavier weight, so it
// reads without colour. Segments are 44px tall below 1024px and shrink and
// wrap their text rather than overflow a narrow container; `fill` stretches
// the group to its container with equal-width segments.

import { For, Show, createSignal, createUniqueId } from 'solid-js';

export interface SegmentedOption {
  value: string;
  label: string;
  disabled?: boolean;
}

export interface SegmentedProps {
  value: string;
  options: SegmentedOption[];
  onChange: (value: string) => void;
  /** Visible label rendered above the group. */
  label?: string;
  /** Accessible name when no visible label exists. */
  'aria-label'?: string;
  /** Id of an element elsewhere that labels the group. */
  labelledBy?: string;
  /** Id(s) of the hint and error text that describe the group. */
  describedBy?: string;
  /** Marks the group as holding a refused value (pair it with describedBy). */
  invalid?: boolean;
  /** Form-semantics name, set on every segment button (with its value). */
  name?: string;
  /** Disables the whole group. */
  disabled?: boolean;
  /** Stretch to the container's width with equal segments. */
  fill?: boolean;
  /** Extra classes on the group element. */
  class?: string;
}

export default function Segmented(props: SegmentedProps) {
  const labelId = `segmented-${createUniqueId()}-label`;
  const buttons: HTMLButtonElement[] = [];
  // The segment keyboard focus sits on while it moves through the group; null
  // = the tab stop rests on the checked segment.
  const [focused, setFocused] = createSignal<number | null>(null);

  const isDisabled = (option: SegmentedOption): boolean =>
    props.disabled === true || option.disabled === true;
  const enabledIndexes = (): number[] =>
    props.options.flatMap((option, index) => (isDisabled(option) ? [] : [index]));
  const checkedIndex = (): number => props.options.findIndex((o) => o.value === props.value);
  const tabStop = (): number => {
    const f = focused();
    if (f !== null && enabledIndexes().includes(f)) return f;
    const checked = checkedIndex();
    if (checked >= 0 && !isDisabled(props.options[checked]!)) return checked;
    return enabledIndexes()[0] ?? -1;
  };

  const select = (option: SegmentedOption): void => {
    if (isDisabled(option) || option.value === props.value) return;
    props.onChange(option.value);
  };

  const moveFocus = (to: number): void => {
    setFocused(to);
    buttons[to]?.focus();
  };

  const onKeyDown = (event: KeyboardEvent, index: number): void => {
    const enabled = enabledIndexes();
    if (enabled.length === 0) return;
    const at = enabled.indexOf(index);
    let next: number | undefined;
    switch (event.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        next = enabled[(at + 1) % enabled.length];
        break;
      case 'ArrowLeft':
      case 'ArrowUp':
        next = enabled[(at - 1 + enabled.length) % enabled.length];
        break;
      case 'Home':
        next = enabled[0];
        break;
      case 'End':
        next = enabled[enabled.length - 1];
        break;
      default:
        return; // Space/Enter fall through to the native button click
    }
    event.preventDefault();
    if (next === undefined) return;
    moveFocus(next);
    const option = props.options[next];
    if (option !== undefined) select(option);
  };

  return (
    <div classList={{ 'segmented-field': true, fill: props.fill === true }}>
      <Show when={props.label}>
        <span class="segmented-label" id={labelId}>
          {props.label}
        </span>
      </Show>
      <div
        role="radiogroup"
        classList={{
          segmented: true,
          fill: props.fill === true,
          ...(props.class !== undefined ? { [props.class]: true } : {}),
        }}
        aria-label={props.label === undefined ? props['aria-label'] : undefined}
        aria-labelledby={props.label !== undefined ? labelId : props.labelledBy}
        aria-describedby={props.describedBy}
        aria-invalid={props.invalid === true ? 'true' : undefined}
        aria-disabled={props.disabled === true ? 'true' : undefined}
        onFocusOut={(event) => {
          const next = event.relatedTarget as Node | null;
          if (next === null || !event.currentTarget.contains(next)) setFocused(null);
        }}
      >
        <For each={props.options}>
          {(option, index) => (
            <button
              type="button"
              role="radio"
              class="segment"
              ref={(el) => (buttons[index()] = el)}
              name={props.name}
              value={option.value}
              aria-checked={option.value === props.value}
              tabIndex={index() === tabStop() ? 0 : -1}
              disabled={isDisabled(option)}
              onClick={() => select(option)}
              onKeyDown={(event) => onKeyDown(event, index())}
              onFocus={() => setFocused(index())}
            >
              {option.label}
            </button>
          )}
        </For>
      </div>
    </div>
  );
}
