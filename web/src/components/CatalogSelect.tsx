// Model/effort select over a provider catalog. A stored value missing from
// the catalog stays selectable as-is (marked), so an untouched field never
// silently changes on save. With `inheritLabel` set, an inherit entry (value
// "") is prepended — selecting it yields "" to mean "inherit the default".

import { For, Show, createEffect, on } from 'solid-js';
import type { ProviderOption } from '../api';

export default function CatalogSelect(props: {
  label: string;
  name: string;
  value: string;
  options: ProviderOption[];
  onChange: (value: string) => void;
  /** When set, prepends an inherit entry (value "") with this label. */
  inheritLabel?: string;
}) {
  // An empty value is representable when an inherit entry is offered, so it is
  // not "unknown" then — otherwise a duplicate empty option would render.
  const known = () =>
    props.value === ''
      ? props.inheritLabel !== undefined
      : props.options.some((option) => option.value === props.value);

  let el!: HTMLSelectElement;
  // Re-assert the selection whenever the catalog changes: when a stored value's
  // option is swapped from the "(not in catalog)" fallback to the real catalog
  // option (providers loading after the form mounted), the browser drops the
  // selection on that node replacement and Solid won't re-apply an unchanged
  // value. This runs after <For> has reconciled the options into the DOM.
  createEffect(
    on([() => props.options, () => props.value, () => props.inheritLabel], () => {
      el.value = props.value;
    }),
  );

  return (
    <label class="field">
      <span>{props.label}</span>
      <select ref={el} name={props.name} onChange={(e) => props.onChange(e.currentTarget.value)}>
        <Show when={props.inheritLabel !== undefined}>
          <option value="">{props.inheritLabel}</option>
        </Show>
        <Show when={!known()}>
          <option value={props.value}>
            {props.value === '' ? '(unset)' : `${props.value} (not in catalog)`}
          </option>
        </Show>
        <For each={props.options}>
          {(option) => <option value={option.value}>{option.label}</option>}
        </For>
      </select>
    </label>
  );
}
