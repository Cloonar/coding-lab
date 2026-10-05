// The repo settings field vocabulary (issue #61): `Field` draws what every
// field of the one-page settings shares — the label with its changed mark,
// the control, the problem under it, the hint — and the small wrappers below
// bind the app's controls to one entry of the field table (fields.ts) through
// the form store (form.tsx). A section is then a list of
// `<TextField name="git_author_name" …/>` lines.
//
// What `Field` guarantees:
//   - the label comes from the field table unless a section overrides it;
//   - an OVERRIDABLE field (fields.ts) says at its label whether it is
//     "inherited" or "set here" — in words, and as part of the control's
//     description. Set here, it also shows "Default: <what it would inherit>"
//     and a Reset action that returns it to inherited (a change like any
//     other: it waits for Save and is saved as a null override). Inherited,
//     it shows what it resolves to — as the first pick of a select or
//     segmented control ("Inherited · <value>"), as the placeholder of a text
//     or number field. The value is the server's answer (form.tsx); while
//     that is not known the state shows without a value;
//   - a changed field is marked AT ITS LABEL by a dot plus the words
//     "unsaved change" inside the label (so it is part of the control's
//     accessible name) — never by colour alone. A switch draws its own
//     label, and gets the words through `control.changedText`;
//   - a problem (a browser check or a server refusal) renders under the
//     control as an alert, and the control is aria-invalid and described by it;
//   - the hint describes the control too (aria-describedby);
//   - the wrapper carries `data-field="<patch key>"` and the control the id
//     `rs-<patch key>`, which is how `?field=` and Save find a field to scroll
//     to and focus (see index.tsx).

import {
  For,
  Show,
  children,
  createContext,
  createUniqueId,
  useContext,
  type Accessor,
  type JSX,
} from 'solid-js';
import Segmented, { type SegmentedOption } from '../../components/Segmented';
import Select, { type SelectOption } from '../../components/Select';
import ToggleSwitch from '../../components/Switch';
import type { RepoDrafts, RepoFieldKey } from './fields';
import { useRepoSettingsForm, type FieldBinding } from './form';

/** The fields whose draft is free text (text, number and select fields). */
export type TextFieldKey = {
  [K in RepoFieldKey]: string extends RepoDrafts[K] ? K : never;
}[RepoFieldKey];

/** The fields whose draft is a boolean (the switches). */
export type FlagFieldKey = {
  [K in RepoFieldKey]: RepoDrafts[K] extends boolean ? K : never;
}[RepoFieldKey];

/** The DOM id of a field's control. */
export function fieldControlId(key: RepoFieldKey): string {
  return `rs-${key}`;
}

/**
 * Moves focus to a field's control without scrolling; false when there is
 * none to take it. `wrapper` is the field's `[data-field]` element.
 */
export function focusFieldControl(wrapper: HTMLElement, key: RepoFieldKey): boolean {
  const control =
    document.getElementById(fieldControlId(key)) ??
    // A group of controls (a segmented pick, the option bag) has no single
    // id: its tab stop is the target.
    wrapper.querySelector<HTMLElement>(
      'input, select, textarea, button[role="radio"][tabindex="0"], button:not([tabindex="-1"])',
    );
  // A disabled control cannot take focus: report it, so a deep link that is
  // still being held tries again once the control is usable.
  if (control === null || control.matches(':disabled')) return false;
  control.focus({ preventScroll: true });
  return true;
}

/** "Inherited · on" — the pick that leaves an overridable field inherited. */
export function inheritPickLabel(text: string | null): string {
  return text === null ? 'Inherited' : `Inherited · ${text}`;
}

/** The title of the FieldGroup a field sits in, so Reset can name it. */
const GroupContext = createContext<string>();

/** What `Field` hands the control it wraps. */
export interface FieldControl<K extends RepoFieldKey> {
  binding: FieldBinding<K>;
  /** The id the control must carry (the label points at it; focus lands on it). */
  id: string;
  /** The label's id, for a control named by aria-labelledby (Select, Segmented, a group). */
  labelId: string;
  /** The ids of the problem and the hint, as far as each is shown right now. */
  describedBy: Accessor<string | undefined>;
  /** True while a problem is shown under the control. */
  invalid: Accessor<boolean>;
  /**
   * The words behind the changed mark, for a control that draws its own
   * label (a switch row): render it INSIDE that label, so it joins the
   * control's accessible name.
   */
  changedText: () => JSX.Element;
}

export function Field<K extends RepoFieldKey>(props: {
  /** The field's PATCH key. */
  name: K;
  /** Overrides the field table's label. */
  label?: string;
  /** One line of help under the control. */
  hint?: JSX.Element;
  /**
   * How the label names the control: 'for' (default) is a <label for> — native
   * inputs; 'id' is plain text the control points at with aria-labelledby;
   * 'none' draws no label row at all (a switch row brings its own).
   */
  labelMode?: 'for' | 'id' | 'none';
  /** Extra classes on the wrapper. */
  class?: string;
  children: (control: FieldControl<K>) => JSX.Element;
}) {
  const form = useRepoSettingsForm();
  // A section binds one fixed field per <Field>, so `name` is read once.
  // eslint-disable-next-line solid/reactivity -- the field a Field shows never changes
  const key = props.name;
  const binding = form.field(key);
  const id = fieldControlId(key);
  const labelId = `${id}-label`;
  const errorId = `${id}-error`;
  const hintId = `${id}-hint`;
  const stateId = `${id}-state`;
  const group = useContext(GroupContext);
  let wrapper: HTMLDivElement | undefined;

  const hint = children(() => props.hint);
  const hasHint = (): boolean => hint.toArray().length > 0;
  const invalid = (): boolean => binding.error() !== null;
  const setHere = (): boolean => binding.overridable && !binding.inherits();
  const describedBy = (): string | undefined => {
    const ids = [
      binding.overridable ? stateId : null,
      invalid() ? errorId : null,
      hasHint() ? hintId : null,
    ].filter(Boolean);
    return ids.length > 0 ? ids.join(' ') : undefined;
  };
  const label = (): string => props.label ?? binding.spec.label;
  const reset = (): void => {
    binding.reset();
    // Reset is gone with the click (the field is inherited again): hand the
    // focus to the field's control instead of dropping it on the page.
    queueMicrotask(() => {
      if (wrapper !== undefined) focusFieldControl(wrapper, key);
    });
  };
  // The words behind the dot. Inside the label, so the control's accessible
  // name carries them ("Model (unsaved change)").
  const ChangedText = () => (
    <Show when={binding.changed()}>
      <span class="visually-hidden"> (unsaved change)</span>
    </Show>
  );

  return (
    <div
      classList={{
        sfield: true,
        changed: binding.changed(),
        invalid: invalid(),
        ...(props.class !== undefined ? { [props.class]: true } : {}),
      }}
      data-field={key}
      ref={wrapper}
    >
      <Show when={props.labelMode !== 'none'}>
        <div class="sfield-label">
          <Show
            when={props.labelMode === 'id'}
            fallback={
              <label id={labelId} for={id}>
                {label()}
                <ChangedText />
              </label>
            }
          >
            <span id={labelId}>
              {label()}
              <ChangedText />
            </span>
          </Show>
          {/* The state in words — a dashed chip while inherited, a filled one
              once set here — and part of the control's description. */}
          <Show when={binding.overridable}>
            <span classList={{ 'sfield-state': true, set: setHere() }} id={stateId}>
              {setHere() ? 'set here' : 'inherited'}
            </span>
          </Show>
        </div>
      </Show>
      {props.children({
        binding,
        id,
        labelId,
        describedBy,
        invalid,
        changedText: () => <ChangedText />,
      })}
      <Show when={binding.error()}>
        {(message) => (
          <p class="sfield-error" id={errorId} role="alert">
            {message()}
          </p>
        )}
      </Show>
      <Show when={setHere()}>
        <div class="sfield-default">
          <Show when={binding.inheritedText()}>{(text) => <small>Default: {text()}</small>}</Show>
          <button
            type="button"
            class="settings-link-action"
            aria-label={`Reset ${label()}${group !== undefined ? ` (${group})` : ''} to inherited`}
            onClick={reset}
          >
            Reset
          </button>
        </div>
      </Show>
      <Show when={hasHint()}>
        <small class="sfield-hint" id={hintId}>
          {hint()}
        </small>
      </Show>
    </div>
  );
}

/**
 * A one-line text or number field. A number field is a TEXT input with the
 * numeric keypad: a browser's number input reports anything it cannot parse
 * as "" — which here means "inherit" — so a typo would be saved as a reset
 * and the field table's whole-number rule would never see it.
 */
export function TextField(props: {
  name: TextFieldKey;
  label?: string;
  hint?: JSX.Element;
  type?: 'text' | 'number';
  /** Monospace value (branch names, image references). */
  mono?: boolean;
  /** Default: what an overridable field inherits (nothing for any other field). */
  placeholder?: string;
  /** The field may not be left empty (announced; the field table enforces it). */
  required?: boolean;
  spellcheck?: boolean;
  class?: string;
}) {
  return (
    <Field name={props.name} label={props.label} hint={props.hint} class={props.class}>
      {(control) => (
        <input
          id={control.id}
          name={control.binding.key}
          type="text"
          classList={{ mono: props.mono === true }}
          inputmode={props.type === 'number' ? 'numeric' : undefined}
          pattern={props.type === 'number' ? '[0-9]*' : undefined}
          autocomplete="off"
          spellcheck={props.spellcheck ?? false}
          placeholder={props.placeholder ?? control.binding.inheritedText() ?? undefined}
          aria-required={props.required === true ? 'true' : undefined}
          aria-invalid={control.invalid() ? 'true' : undefined}
          aria-describedby={control.describedBy()}
          value={control.binding.value()}
          onInput={(event) => control.binding.set(event.currentTarget.value)}
        />
      )}
    </Field>
  );
}

/**
 * A pick from a catalog, through the app's searchable Select. For an
 * overridable field the first entry is "Inherited · <what it resolves to>".
 */
export function SelectField(props: {
  name: TextFieldKey;
  label?: string;
  hint?: JSX.Element;
  options: SelectOption[];
  disabled?: boolean;
  class?: string;
}) {
  return (
    <Field
      name={props.name}
      label={props.label}
      hint={props.hint}
      labelMode="id"
      class={props.class}
    >
      {(control) => (
        <Select
          skin="field"
          label={props.label ?? control.binding.spec.label}
          labelledBy={control.labelId}
          id={control.id}
          name={control.binding.key}
          value={control.binding.value()}
          options={props.options}
          // An overridable pick starts with the entry that leaves it
          // inherited, naming what that resolves to.
          inheritLabel={
            control.binding.overridable
              ? inheritPickLabel(control.binding.inheritedText())
              : undefined
          }
          disabled={props.disabled}
          describedBy={control.describedBy()}
          invalid={control.invalid()}
          onChange={(value) => control.binding.set(value)}
        />
      )}
    </Field>
  );
}

/** A pick from a short list of credentials, as a native select. */
export function NativeSelectField(props: {
  name: TextFieldKey;
  label?: string;
  hint?: JSX.Element;
  options: { value: string; label: string }[];
  disabled?: boolean;
  class?: string;
}) {
  return (
    <Field name={props.name} label={props.label} hint={props.hint} class={props.class}>
      {(control) => (
        <select
          id={control.id}
          name={control.binding.key}
          disabled={props.disabled}
          aria-invalid={control.invalid() ? 'true' : undefined}
          aria-describedby={control.describedBy()}
          value={control.binding.value()}
          onChange={(event) => control.binding.set(event.currentTarget.value)}
        >
          <For each={props.options}>
            {(option) => (
              <option value={option.value} selected={option.value === control.binding.value()}>
                {option.label}
              </option>
            )}
          </For>
        </select>
      )}
    </Field>
  );
}

/**
 * One pick out of a few short options shown side by side. For an overridable
 * field the first segment is "Inherited · <what it resolves to>" (the '' draft),
 * so inherit, and each explicit value, is one tap — a three-way control over
 * a tri-state field, where "off" is a value and never a blank.
 */
export function SegmentedField<K extends RepoFieldKey>(props: {
  name: K;
  label?: string;
  hint?: JSX.Element;
  options: (SegmentedOption & { value: RepoDrafts[K] & string })[];
  disabled?: boolean;
  class?: string;
}) {
  return (
    <Field
      name={props.name}
      label={props.label}
      hint={props.hint}
      labelMode="id"
      class={props.class}
    >
      {(control) => (
        <Segmented
          labelledBy={control.labelId}
          name={control.binding.key}
          value={String(control.binding.value())}
          options={
            control.binding.overridable
              ? [
                  { value: '', label: inheritPickLabel(control.binding.inheritedText()) },
                  ...props.options,
                ]
              : props.options
          }
          disabled={props.disabled}
          describedBy={control.describedBy()}
          invalid={control.invalid()}
          onChange={(value) => control.binding.set(value as RepoDrafts[K])}
        />
      )}
    </Field>
  );
}

/** An on/off field: the label and its one-line description beside a switch. */
export function SwitchField(props: {
  name: FlagFieldKey;
  label?: string;
  /** The muted line under the label. */
  description?: string;
  /** Further help under the row. */
  hint?: JSX.Element;
  disabled?: boolean;
  class?: string;
}) {
  return (
    <Field name={props.name} hint={props.hint} labelMode="none" class={props.class}>
      {(control) => (
        <ToggleSwitch
          id={control.id}
          name={control.binding.key}
          label={props.label ?? control.binding.spec.label}
          description={props.description}
          labelExtra={control.changedText()}
          describedBy={control.describedBy()}
          invalid={control.invalid()}
          checked={control.binding.value()}
          disabled={props.disabled}
          onChange={(next) => control.binding.set(next)}
        />
      )}
    </Field>
  );
}

/** A labelled run of fields inside a section's card ("Runs you start", "Lander"). */
export function FieldGroup(props: { title: string; children: JSX.Element }) {
  const id = `rs-group-${createUniqueId()}`;
  return (
    // eslint-disable-next-line solid/reactivity -- a group's title never changes
    <GroupContext.Provider value={props.title}>
      <div class="settings-group" role="group" aria-labelledby={id}>
        <h3 class="settings-sub" id={id}>
          {props.title}
        </h3>
        {props.children}
      </div>
    </GroupContext.Provider>
  );
}
