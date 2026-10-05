// On/off switch (issue #61): a button with role="switch" and aria-checked, so
// Space and Enter toggle it natively and assistive tech announces "on"/"off".
// The thumb sits left when off and right when on, so the state never rests on
// the track colour alone. The hit area is at least 44px tall; the visible
// track stays compact inside it.
//
// Naming: pass `label` for a visible label (rendered beside the switch and
// tied to it with <label for>, so tapping the text toggles too), optionally
// with a muted `description` under it (wired as aria-describedby); or
// `labelledBy` to point at a label rendered elsewhere; or `aria-label` when
// there is no visible text. A host that shows more about the switch — a
// hint, a problem, an "unsaved change" mark — passes `describedBy` (ids
// added to the control's description), `invalid`, and `labelExtra` (content
// rendered inside the label, so it joins the accessible name).
//
// Default export is named ToggleSwitch so files that also use solid-js's
// <Switch>/<Match> can import it without a clash.

import { Show, createUniqueId, type JSX } from 'solid-js';

export interface SwitchProps {
  checked: boolean;
  /** Receives the requested next state; the parent owns `checked`. */
  onChange: (next: boolean) => void;
  /** Visible label text beside the switch. */
  label?: string;
  /** Muted secondary text under the label (needs `label`). */
  description?: string;
  /** More content inside the label, after its text (needs `label`). */
  labelExtra?: JSX.Element;
  /** Ids of further text that describes the switch (a hint, a problem). */
  describedBy?: string;
  /** Marks the switch as holding a refused value (pair it with describedBy). */
  invalid?: boolean;
  /** Accessible name when no visible label exists. */
  'aria-label'?: string;
  /** Id of an element elsewhere that labels the switch. */
  labelledBy?: string;
  disabled?: boolean;
  /** Form-semantics name, exposed on the button (tests and forms query it). */
  name?: string;
  id?: string;
  /** Extra classes on the outer element. */
  class?: string;
}

export default function ToggleSwitch(props: SwitchProps) {
  const uid = createUniqueId();
  const controlId = () => props.id ?? `switch-${uid}`;
  const descId = `switch-${uid}-desc`;
  // The description first, then whatever the host adds.
  const describedBy = (): string | undefined => {
    const ids = [
      props.label !== undefined && props.description ? descId : null,
      props.describedBy ?? null,
    ].filter(Boolean);
    return ids.length > 0 ? ids.join(' ') : undefined;
  };

  const control = () => (
    <button
      type="button"
      role="switch"
      id={controlId()}
      name={props.name}
      class={
        props.label === undefined && props.class !== undefined ? `switch ${props.class}` : 'switch'
      }
      aria-checked={props.checked}
      aria-label={props['aria-label']}
      aria-labelledby={props.labelledBy}
      aria-describedby={describedBy()}
      aria-invalid={props.invalid === true ? 'true' : undefined}
      disabled={props.disabled}
      onClick={() => props.onChange(!props.checked)}
    >
      <span class="switch-track" aria-hidden="true">
        <span class="switch-thumb" />
      </span>
    </button>
  );

  return (
    <Show when={props.label !== undefined} fallback={control()}>
      <div
        classList={{
          'switch-row': true,
          disabled: props.disabled === true,
          ...(props.class !== undefined ? { [props.class]: true } : {}),
        }}
      >
        <span class="switch-text">
          <label class="switch-label" for={controlId()}>
            {props.label}
            {props.labelExtra}
          </label>
          <Show when={props.description}>
            <small class="switch-desc" id={descId}>
              {props.description}
            </small>
          </Show>
        </span>
        {control()}
      </div>
    </Show>
  );
}
