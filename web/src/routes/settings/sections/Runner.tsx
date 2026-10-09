// Global settings › Runner (issues #55, #205, #85): where runs execute for
// every repo that inherits the default — the Runner pick, the default dev
// image and the container limits — as a thin renderer over the form store.
// Every control edits a draft; the page's save bar sends the changed ones
// (form.tsx, fields.ts). None of these is an override: each is the bottom
// layer a repo's own value stands on.
//
// The Runner pick (Container / Host, the repo page's words) shows the
// unsandboxed warning while Host is drafted, and says how many repos inherit
// the default — a line left out while the repo list is loading or could not
// be loaded, never guessed. Switching the default TO host asks once, at Save,
// in an in-page dialog (HostSwitchDialog.tsx); nothing here asks.
//
// Nothing folds (ADR-0080): the dev image and the container limits feed every
// repo pinned to container even while the default is Host, so they show
// whatever the Runner is, and the note says whom they apply to. The dev
// image's hint names what a blank falls through to — the deployed image (the
// server's read-only dev_image_fallback, never a field of its own and never
// sent) or the plain fact that nothing is configured — and, with a ref typed,
// that the server pins it on save.

import { Show } from 'solid-js';
import Banner from '../../../components/Banner';
import { DEV_IMAGE_PLACEHOLDER, HOST_RUNNER_HINT } from '../../../lib/runner';
import { FieldGroup, SegmentedField, TextField } from '../Field';
import { useGlobalSettingsForm } from '../form';

/** The Runner picks: the repo page's, without its inherit segment. */
const RUNNER_PICKS = [
  { value: 'container', label: 'Container' },
  { value: 'host', label: 'Host' },
];

/** "3 repos inherit this default." — the line under the Runner pick. */
export function inheritLine(count: number): string {
  if (count === 0) return 'No repos inherit this default.';
  if (count === 1) return '1 repo inherits this default.';
  return `${count} repos inherit this default.`;
}

export default function RunnerSection() {
  const form = useGlobalSettingsForm();
  const runner = form.field('runner_default');
  const image = form.field('dev_image_default');

  const fallback = (): string => form.saved()?.dev_image_fallback ?? '';
  const inheritText = (): string | null => {
    const count = form.inheritingRepos();
    return count === null ? null : inheritLine(count);
  };

  return (
    <div class="card settings-card">
      <SegmentedField
        name="runner_default"
        options={RUNNER_PICKS}
        hint="Where sessions run for every repo that inherits the default. A repo can pin its own."
      />
      <Show when={runner.value() === 'host'}>
        <Banner message={HOST_RUNNER_HINT} variant="notice" />
      </Show>
      {/* 0 is a count too: only an unknown one (null) leaves the line out. */}
      <Show when={inheritText()}>{(text) => <p class="settings-note">{text()}</p>}</Show>

      <p class="settings-note">
        The dev image and the container limits apply to container runs — including repos pinned to
        container while the default is Host. A repo can override each one.
      </p>
      <TextField
        name="dev_image_default"
        mono
        placeholder={DEV_IMAGE_PLACEHOLDER}
        hint={
          <Show
            when={image.value().trim() === ''}
            fallback="Resolved and pinned to a digest on save."
          >
            <Show
              when={fallback() !== ''}
              fallback="No dev image is configured — container spawns are refused until one is set here, per repo, or on the server."
            >
              Blank uses the deployed image <code>{fallback()}</code>.
            </Show>
          </Show>
        }
      />
      <FieldGroup title="Container limits">
        <div class="settings-grid3 even">
          <TextField name="container_memory" hint="Podman memory limit, e.g. 512m or 8g." />
          <TextField name="container_pids" type="number" />
          <TextField name="container_nofile" type="number" />
        </div>
      </FieldGroup>
    </div>
  );
}
