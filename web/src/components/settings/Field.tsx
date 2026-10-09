// The settings field vocabulary (issue #61, issue #85): `Field` draws what
// every field of a one-page settings form shares — the label with its changed
// mark, the control, the problem under it, the hint — and the small wrappers
// below bind the app's controls to one entry of the page's field table
// (fields.ts) through the page's form store (form.tsx, read through
// SettingsFormContext). A section is then a list of
// `<TextField name="git_author_name" …/>` lines.
//
// The components are written once, against the loose contract every form
// meets; fieldComponents<Shape>() hands a page the same components typed for
// its own table, so a `name` that is no field of that page (or whose draft is
// not the control's value type) does not compile.
//
// What `Field` guarantees:
//   - the label comes from the field table unless a section overrides it;
//   - an OVERRIDABLE field (fields.ts) says at its label whether it is
//     "inherited" or "set here" — in words, and as part of the control's
//     description. Set here, it also shows "Default: <what it would inherit>"
//     and a Reset action that returns it to inherited (a change like any
//     other: it waits for Save and is saved as an inherit — null on the repo
//     page, "" for a global text override). Inherited,
//     it shows what it resolves to — as the first pick of a select or
//     segmented control ("Inherited · <value>"), as the placeholder of a text
//     or number field. The value is the store's `inheritedText`; while that
//     is not known the state shows without a value;
//   - a changed field is marked AT ITS LABEL by a dot plus the words
//     "unsaved change" inside the label (so it is part of the control's
//     accessible name) — never by colour alone. A switch draws its own
//     label, and gets the words through `control.changedText`;
//   - a problem (a browser check or a server refusal) renders under the
//     control as an alert, and the control is aria-invalid and described by it;
//   - the hint describes the control too (aria-describedby);
//   - the wrapper carries `data-field="<patch key>"` and the control the id
//     `rs-<patch key>` (the prefix both settings pages share), which is how
//     `?field=` and Save find a field to scroll to and focus
//     (sectionScroll.ts).

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
import Segmented, { type SegmentedOption } from '../Segmented';
import Select, { type SelectOption } from '../Select';
import ToggleSwitch from '../Switch';
import type { DraftOf, FieldKey, FormShape } from './fields';
import { useSettingsFormContext, type FieldBinding, type LooseShape } from './form';

/** The fields whose draft is free text (text, number and select fields). */
export type TextFieldKey<T extends FormShape> = {
  [K in FieldKey<T>]: string extends DraftOf<T, K> ? K : never;
}[FieldKey<T>];

/** The fields whose draft is a boolean (the switches). */
export type FlagFieldKey<T extends FormShape> = {
  [K in FieldKey<T>]: DraftOf<T, K> extends boolean ? K : never;
}[FieldKey<T>];

/** The DOM id of a field's control. */
export function fieldControlId(key: string): string {
  return `rs-${key}`;
}

/**
 * Moves focus to a field's control without scrolling; false when there is
 * none to take it. `wrapper` is the field's `[data-field]` element.
 */
export function focusFieldControl(wrapper: HTMLElement, key: string): boolean {
  const control =
    document.getElementById(fieldControlId(key)) ??
    // A group of controls (a segmented pick, an option bag) has no single
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
export interface FieldControl<T extends FormShape, K extends FieldKey<T>> {
  binding: FieldBinding<T, K>;
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

export interface FieldProps<T extends FormShape, K extends FieldKey<T>> {
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
  children: (control: FieldControl<T, K>) => JSX.Element;
}

type Loose = LooseShape;

function Field(props: FieldProps<Loose, string>) {
  const form = useSettingsFormContext();
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

export interface TextFieldProps<K extends string> {
  name: K;
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
}

/**
 * A one-line text or number field. A number field is a TEXT input with the
 * numeric keypad: a browser's number input reports anything it cannot parse
 * as "" — which here means "inherit" — so a typo would be saved as a reset
 * and the field table's whole-number rule would never see it.
 */
function TextField(props: TextFieldProps<string>) {
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
          value={control.binding.value() as string}
          onInput={(event) => control.binding.set(event.currentTarget.value)}
        />
      )}
    </Field>
  );
}

export interface SelectFieldProps<K extends string> {
  name: K;
  label?: string;
  hint?: JSX.Element;
  options: SelectOption[];
  disabled?: boolean;
  class?: string;
}

/**
 * A pick from a catalog, through the app's searchable Select. For an
 * overridable field the first entry is "Inherited · <what it resolves to>".
 */
function SelectField(props: SelectFieldProps<string>) {
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
          value={control.binding.value() as string}
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

export interface NativeSelectFieldProps<K extends string> {
  name: K;
  label?: string;
  hint?: JSX.Element;
  options: { value: string; label: string }[];
  disabled?: boolean;
  class?: string;
}

/** A pick from a short list (credentials), as a native select. */
function NativeSelectField(props: NativeSelectFieldProps<string>) {
  return (
    <Field name={props.name} label={props.label} hint={props.hint} class={props.class}>
      {(control) => (
        <select
          id={control.id}
          name={control.binding.key}
          disabled={props.disabled}
          aria-invalid={control.invalid() ? 'true' : undefined}
          aria-describedby={control.describedBy()}
          value={control.binding.value() as string}
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

export interface SegmentedFieldProps<K extends string, V extends string> {
  name: K;
  label?: string;
  hint?: JSX.Element;
  options: (SegmentedOption & { value: V })[];
  disabled?: boolean;
  class?: string;
}

/**
 * One pick out of a few short options shown side by side. For an overridable
 * field the first segment is "Inherited · <what it resolves to>" (the '' draft),
 * so inherit, and each explicit value, is one tap — a three-way control over
 * a tri-state field, where "off" is a value and never a blank.
 */
function SegmentedField(props: SegmentedFieldProps<string, string>) {
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
          onChange={(value) => control.binding.set(value)}
        />
      )}
    </Field>
  );
}

export interface SwitchFieldProps<K extends string> {
  name: K;
  label?: string;
  /** The muted line under the label. */
  description?: string;
  /** Further help under the row. */
  hint?: JSX.Element;
  disabled?: boolean;
  class?: string;
}

/** An on/off field: the label and its one-line description beside a switch. */
function SwitchField(props: SwitchFieldProps<string>) {
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
          checked={control.binding.value() as boolean}
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

/** The field components, typed for one page's field table. */
export interface FieldComponents<T extends FormShape> {
  Field: <K extends FieldKey<T>>(props: FieldProps<T, K>) => JSX.Element;
  TextField: (props: TextFieldProps<TextFieldKey<T>>) => JSX.Element;
  SelectField: (props: SelectFieldProps<TextFieldKey<T>>) => JSX.Element;
  NativeSelectField: (props: NativeSelectFieldProps<TextFieldKey<T>>) => JSX.Element;
  SegmentedField: <K extends FieldKey<T>>(
    props: SegmentedFieldProps<K, DraftOf<T, K> & string>,
  ) => JSX.Element;
  SwitchField: (props: SwitchFieldProps<FlagFieldKey<T>>) => JSX.Element;
}

/**
 * The one implementation of the field components, typed for the form shape
 * `T`. The page's form must be the one SettingsFormContext provides.
 */
export function fieldComponents<T extends FormShape>(): FieldComponents<T> {
  return {
    Field,
    TextField,
    SelectField,
    NativeSelectField,
    SegmentedField,
    SwitchField,
  } as unknown as FieldComponents<T>;
}
