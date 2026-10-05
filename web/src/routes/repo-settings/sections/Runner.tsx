// Runner section (issues #205, #55, #61): where this repo's instances run —
// the Runner pick, the dev image and the three container limits — as a thin
// renderer over the form store. Every control edits a draft; the page's save
// bar sends the changed ones (form.tsx, fields.ts).
//
// repos.runner is nullable: '' on the picker is "inherit the global runner
// default" (null on the wire), and its row names what that currently is. The
// EFFECTIVE runner is the drafted pick, else the global default — the host
// warning follows it, pinned or inherited. The dev image and the limits stay
// editable on the host runner: staging a value before switching is legal.
//
// The lines that read the global settings (the inherit row's "currently …",
// what a blank dev image inherits, the limits' placeholders) wait for the
// settings to load rather than guess; the fields themselves never wait.

import { Show } from 'solid-js';
import Banner from '../../../components/Banner';
import {
  DEV_IMAGE_PLACEHOLDER,
  HOST_RUNNER_HINT,
  RUNNER_OPTIONS,
  runnerName,
} from '../../../lib/runner';
import { FieldGroup, SelectField, TextField } from '../Field';
import { useRepoSettingsForm } from '../form';

/** The placeholder of a limit whose global default is not known (yet). */
const GLOBAL_DEFAULT = 'global default';

export default function RunnerSection() {
  const form = useRepoSettingsForm();
  const settings = form.catalog.settings;
  const runner = form.field('runner');

  const effectiveRunner = () =>
    runner.value() === '' ? settings()?.runner_default : runner.value();
  const inheritLabel = () => {
    const current = runnerName(settings()?.runner_default);
    return current === null
      ? 'Inherit global default'
      : `Inherit global default — currently ${current}`;
  };
  // What a blank dev image inherits: the global default dev image, else the
  // server's --container-image flag.
  const inheritedImage = () =>
    settings()?.dev_image_default || settings()?.dev_image_fallback || '';
  // A blank limit inherits the global one, shown as the placeholder.
  const limitDefault = (value: string | number | undefined): string =>
    value === undefined || value === '' ? GLOBAL_DEFAULT : String(value);

  return (
    <div class="card settings-card">
      <SelectField name="runner" options={RUNNER_OPTIONS} inheritLabel={inheritLabel()} />
      <Show when={effectiveRunner() === 'host'}>
        <Banner message={HOST_RUNNER_HINT} variant="notice" />
      </Show>
      <TextField
        name="image_ref"
        mono
        placeholder={DEV_IMAGE_PLACEHOLDER}
        hint={
          <>
            For container runs. Resolved and pinned to a digest on save.
            <Show when={settings()}>
              {' '}
              <Show
                when={inheritedImage() !== ''}
                fallback="No dev image is configured — container spawns are refused until one is set here, in global Settings, or on the server."
              >
                Blank inherits <code>{inheritedImage()}</code>.
              </Show>
            </Show>
          </>
        }
      />
      <FieldGroup title="Container limits">
        <p class="settings-note">
          Limits apply to container runs only. A blank limit inherits the global default.
        </p>
        <div class="settings-grid3 even">
          <TextField
            name="container_memory"
            placeholder={limitDefault(settings()?.container_memory)}
          />
          <TextField
            name="container_pids"
            type="number"
            min={1}
            placeholder={limitDefault(settings()?.container_pids)}
          />
          <TextField
            name="container_nofile"
            type="number"
            min={1}
            placeholder={limitDefault(settings()?.container_nofile)}
          />
        </div>
      </FieldGroup>
    </div>
  );
}
