// The repo settings field vocabulary (issue #61): `Field` draws what every
// field of the one-page settings shares — the label with its changed mark,
// the control, the problem under it, the hint — and the small wrappers below
// bind the app's controls to one entry of the field table (fields.ts) through
// the form store (form.tsx). A section is then a list of
// `<TextField name="git_author_name" …/>` lines.
//
// What `Field` guarantees:
//   - the label comes from the field table unless a section overrides it;
//   - a changed field is marked AT ITS LABEL by a dot plus the words
//     "unsaved change" inside the label (so it is part of the control's
//     accessible name) — never by colour alone;
//   - a problem (a browser check or a server refusal) renders under the
//     control as an alert, and the control is aria-invalid and described by it;
//   - the hint describes the control too (aria-describedby);
//   - the wrapper carries `data-field="<patch key>"` and the control the id
//     `rs-<patch key>`, which is how `?field=` and Save find a field to scroll
//     to and focus (see index.tsx).

import { For, Show, children, createUniqueId, type Accessor, type JSX } from 'solid-js';
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

  const hint = children(() => props.hint);
  const hasHint = (): boolean => hint.toArray().length > 0;
  const invalid = (): boolean => binding.error() !== null;
  const describedBy = (): string | undefined => {
    const ids = [invalid() ? errorId : null, hasHint() ? hintId : null].filter(Boolean);
    return ids.length > 0 ? ids.join(' ') : undefined;
  };
  const label = (): string => props.label ?? binding.spec.label;
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
    >
      <Show when={props.labelMode !== 'none'} fallback={<ChangedText />}>
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
        </div>
      </Show>
      {props.children({ binding, id, labelId, describedBy, invalid })}
      <Show when={binding.error()}>
        {(message) => (
          <p class="sfield-error" id={errorId} role="alert">
            {message()}
          </p>
        )}
      </Show>
      <Show when={hasHint()}>
        <small class="sfield-hint" id={hintId}>
          {hint()}
        </small>
      </Show>
    </div>
  );
}

/** A one-line text or number field. */
export function TextField(props: {
  name: TextFieldKey;
  label?: string;
  hint?: JSX.Element;
  type?: 'text' | 'number';
  /** Monospace value (branch names, image references). */
  mono?: boolean;
  placeholder?: string;
  /** Lowest value a number field's stepper offers; the field table validates. */
  min?: number;
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
          type={props.type ?? 'text'}
          classList={{ mono: props.mono === true }}
          inputmode={props.type === 'number' ? 'numeric' : undefined}
          min={props.min}
          step={props.type === 'number' ? 1 : undefined}
          autocomplete="off"
          spellcheck={props.spellcheck ?? false}
          placeholder={props.placeholder}
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

/** A pick from a catalog, through the app's searchable Select. */
export function SelectField(props: {
  name: TextFieldKey;
  label?: string;
  hint?: JSX.Element;
  options: SelectOption[];
  /** Prepends the inherit entry (value '') with this label. */
  inheritLabel?: string;
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
          inheritLabel={props.inheritLabel}
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
  class?: string;
}) {
  return (
    <Field name={props.name} label={props.label} hint={props.hint} class={props.class}>
      {(control) => (
        <select
          id={control.id}
          name={control.binding.key}
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

/** One pick out of a few short options shown side by side. */
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
          options={props.options}
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
    <div class="settings-group" role="group" aria-labelledby={id}>
      <h3 class="settings-sub" id={id}>
        {props.title}
      </h3>
      {props.children}
    </div>
  );
}
