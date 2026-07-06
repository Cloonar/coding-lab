// A label chip. When a color is known (builtin repos expose label colors via
// GET /labels) the chip is painted with a readable contrast; forge repos only
// carry label names, so those render as the neutral default chip.

import { Show } from 'solid-js';
import { labelChipStyle } from '../lib/labels';

export default function LabelChip(props: { name: string; color?: string }) {
  const style = () => (props.color !== undefined ? labelChipStyle(props.color) : null);
  return (
    <Show when={style()} fallback={<span class="chip label-chip">{props.name}</span>}>
      {(s) => (
        <span class="chip label-chip" style={{ background: s().background, color: s().color }}>
          {props.name}
        </span>
      )}
    </Show>
  );
}
