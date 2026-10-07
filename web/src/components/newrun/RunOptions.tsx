// The New run composer's run options (issue #66): the chips in the field's
// bottom bar and the pickers they open, plus the attached issue action.
//
// - ChoiceChip: the Model and Effort chips. A tap opens the option picker
//   straight away (a sheet on the phone, a popover on the chip from 1024px);
//   a tap on an option picks it and closes — two taps, never three. The list
//   marks the inherited default "default" and checks the current value, and
//   one hint line says where the default comes from and that a pick applies
//   to this run only.
// - MoreOptions: the ⋯ chip and its More options picker — the agent
//   (a segmented control, only with a real choice of two or more providers,
//   ADR-0030), the per-spawn remote-control switch (disabled, with
//   "<Agent> ignores this", for a provider with no remote knob, ADR-0045),
//   the optional run label, and one sentence naming the resolved Runner with
//   a link to the repo's Runner settings (the one navigation this page offers
//   besides a blocker's remedy).
// - AttachmentChip: the attached issue action ("Triage #47 · <title>") with
//   its remove button.
//
// These are presentational: the page owns the resolution chain (per-spawn
// pick → repo override → global default, mirroring the server) and passes
// resolved values in, together with whether each one is a per-spawn pick that
// differs from what would be inherited (`changed` → the accent outline).
//
// A chip toggles its picker on click: the Picker leaves a mousedown on its
// anchor to the host, so the chip's own click is what closes it again.

import { A } from '@solidjs/router';
import { For, Show, createEffect, createSignal, type JSX } from 'solid-js';
import Icon from '../Icon';
import Picker, { PickerHint, PickerList, PickerOption } from '../Picker';
import Segmented from '../Segmented';
import ToggleSwitch from '../Switch';

/** The longest run label the server accepts. */
export const RUN_LABEL_MAX = 32;

export interface ChoiceOption {
  value: string;
  label: string;
  description?: string;
}

/** Model / Effort: a chip that opens its picker immediately (2 taps: chip → option). */
export function ChoiceChip(props: {
  /** "Model" | "Effort": the picker title and the chip's aria-label prefix. */
  name: string;
  /** The resolved value. */
  value: string;
  /** The inherited default (pick-less resolution), marked "default" in the list. */
  defaultValue: string;
  options: ChoiceOption[];
  /** The current value is a per-spawn pick that differs from the inherited
   *  default → accent outline. */
  changed: boolean;
  /** Where the default comes from: e.g. "coding-lab's settings" or "global Settings". */
  defaultSource: string;
  onPick: (value: string) => void;
  disabled?: boolean;
}): JSX.Element {
  const [open, setOpen] = createSignal(false);
  let chip: HTMLButtonElement | undefined;
  // A chip disabled while its picker is open (a blocker appeared) closes it.
  createEffect(() => {
    if (props.disabled === true) setOpen(false);
  });

  const valueLabel = () =>
    props.options.find((option) => option.value === props.value)?.label ?? props.value;

  return (
    <>
      <button
        ref={chip}
        type="button"
        class="composer-chip run-chip"
        classList={{ changed: props.changed }}
        aria-label={`${props.name}: ${valueLabel()}`}
        aria-haspopup="dialog"
        aria-expanded={open()}
        disabled={props.disabled === true}
        onClick={() => setOpen((v) => !v)}
      >
        <span class="composer-chip-label">{valueLabel()}</span>
        <Icon name="chevron-down" size={14} class="composer-chip-caret" />
      </button>
      <Picker
        open={open()}
        onClose={() => setOpen(false)}
        title={props.name}
        size="narrow"
        anchor={() => chip}
      >
        <PickerList label={props.name}>
          <For each={props.options}>
            {(option) => (
              <PickerOption
                selected={option.value === props.value}
                isDefault={option.value === props.defaultValue}
                title={option.label}
                description={option.description}
                onSelect={() => {
                  props.onPick(option.value);
                  setOpen(false);
                }}
              />
            )}
          </For>
        </PickerList>
        <PickerHint>
          The default comes from {props.defaultSource}. A pick here applies to this run only.
        </PickerHint>
      </Picker>
    </>
  );
}

/** The ⋯ chip + More options picker. */
export function MoreOptions(props: {
  /** The agent segmented control shows only when length >= 2 (ADR-0030). */
  providers: { id: string; label: string }[];
  /** The effective provider id. */
  providerId: string;
  onProvider: (id: string) => void;
  /** The resolved remote-control value. */
  remote: boolean;
  /** True when a per-spawn pick differs from the inherited value. */
  remoteSetHere: boolean;
  /** The provider's display name when it has no remote knob → the switch is
   *  disabled with "<Agent> ignores this". */
  remoteBlocker: string | null;
  onRemote: (value: boolean) => void;
  /** The optional run label (≤32 chars). */
  label: string;
  onLabel: (value: string) => void;
  runner: 'host' | 'container' | null;
  runnerInherited: boolean;
  /** The repo's Runner settings. */
  runnerHref: string;
  /** Any of agent/remote/label set → the ⋯ chip gets the accent outline. */
  changed: boolean;
  disabled?: boolean;
}): JSX.Element {
  const [open, setOpen] = createSignal(false);
  let chip: HTMLButtonElement | undefined;
  // A chip disabled while its picker is open (a blocker appeared) closes it.
  createEffect(() => {
    if (props.disabled === true) setOpen(false);
  });

  const remoteState = (): string => {
    if (props.remoteBlocker !== null) return `${props.remoteBlocker} ignores this`;
    if (props.remoteSetHere) return 'set here';
    return `inherited · ${props.remote ? 'on' : 'off'}`;
  };

  return (
    <>
      <button
        ref={chip}
        type="button"
        class="composer-chip run-chip run-chip-more"
        classList={{ changed: props.changed }}
        aria-label="More options"
        title="More options"
        aria-haspopup="dialog"
        aria-expanded={open()}
        disabled={props.disabled === true}
        onClick={() => setOpen((v) => !v)}
      >
        <Icon name="more-horizontal" size={16} />
      </button>
      <Picker
        open={open()}
        onClose={() => setOpen(false)}
        title="More options"
        size="narrow"
        anchor={() => chip}
        class="run-more"
      >
        <Show when={props.providers.length >= 2}>
          <Segmented
            label="Agent"
            name="provider"
            fill
            value={props.providerId}
            options={props.providers.map((p) => ({ value: p.id, label: p.label }))}
            onChange={(id) => props.onProvider(id)}
          />
        </Show>
        <ToggleSwitch
          label="Remote control"
          description={remoteState()}
          name="remote"
          checked={props.remote}
          disabled={props.remoteBlocker !== null}
          onChange={(next) => props.onRemote(next)}
        />
        <label class="field run-more-label">
          <span>
            Label <small class="run-more-optional">optional</small>
          </span>
          <input
            name="label"
            maxlength={RUN_LABEL_MAX}
            placeholder="debug"
            autocomplete="off"
            value={props.label}
            onInput={(e) => props.onLabel(e.currentTarget.value.slice(0, RUN_LABEL_MAX))}
          />
        </label>
        <Show when={props.runner}>
          {(runner) => (
            <p class="run-more-runner">
              {runner() === 'container'
                ? 'Runs in a container'
                : 'Runs directly on the host, unsandboxed'}
              {props.runnerInherited ? ' (inherited)' : ''}
              {' · '}
              <A href={props.runnerHref}>Runner settings</A>
            </p>
          )}
        </Show>
      </Picker>
    </>
  );
}

/** The attached issue action: a removable chip above the textarea ("Triage #47 · <title>"). */
export function AttachmentChip(props: { text: string; onRemove: () => void }): JSX.Element {
  return (
    <div class="composer-attach">
      <span class="composer-attach-text" title={props.text}>
        {props.text}
      </span>
      <button
        type="button"
        class="icon-btn composer-attach-remove"
        aria-label={`Remove ${props.text}`}
        title="Remove"
        onClick={() => props.onRemove()}
      >
        <Icon name="x" size={16} />
      </button>
    </div>
  );
}
