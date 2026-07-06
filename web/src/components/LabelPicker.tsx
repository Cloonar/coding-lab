// Multi-select label picker (builtin repos only — forge labels are managed on
// the forge): one toggle chip per repo label, painted with the label's color.
// The selection algebra (toggleLabel / sameLabelSet) lives in lib/labels; the
// caller owns the selected set and passes toggles back down.

import { For } from 'solid-js';
import type { Label } from '../api';
import { labelChipStyle } from '../lib/labels';

export default function LabelPicker(props: {
  labels: Label[];
  selected: string[];
  onToggle: (name: string) => void;
}) {
  const isOn = (name: string) => props.selected.includes(name);
  return (
    <div class="label-picker" role="group" aria-label="Labels">
      <For each={props.labels}>
        {(label) => {
          const style = labelChipStyle(label.color);
          return (
            <button
              type="button"
              classList={{ 'chip-toggle': true, on: isOn(label.name) }}
              style={{
                background: style.background,
                color: style.color,
                'border-color': 'transparent',
              }}
              aria-pressed={isOn(label.name)}
              onClick={() => props.onToggle(label.name)}
            >
              {isOn(label.name) ? '✓ ' : ''}
              {label.name}
            </button>
          );
        }}
      </For>
    </div>
  );
}
