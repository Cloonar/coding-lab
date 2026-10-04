// Runner section (issue #205 / the 2026-07-22 container-isolation design):
// the schema/API/UI tracer-bullet slice's repo-settings card, modeled on
// Autoland.tsx — drafts seeded via createSeededDrafts, saved as a
// dirty-fields-only PATCH through useSettingsForm. The spawn path itself
// (podman argv, preflight, mounts) is a separate slice; this section carries
// the repos.runner pick, its three container resource-limit overrides and the
// dev image override end to end.
//
// repos.runner is nullable (issue #55): null = inherit the global runner
// default, so the picker's first row is Select's inherit entry (value '' ↔
// null on the wire), worded like the limit hints below ("Inherit global
// default — currently …"). The EFFECTIVE runner is the pick, else
// settings.runner_default — the host hint follows it, pinned or inherited.

import { Show } from 'solid-js';
import type { Accessor } from 'solid-js';
import { updateRepo, type Repo, type RepoPatch, type Runner, type Settings } from '../../../api';
import Banner from '../../../components/Banner';
import SectionCard from '../../../components/SectionCard';
import Select from '../../../components/Select';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import {
  DEV_IMAGE_PLACEHOLDER,
  HOST_RUNNER_HINT,
  RUNNER_OPTIONS,
  runnerName,
} from '../../../lib/runner';
import { createSeededDrafts } from '../../../lib/seededDrafts';
import { normInt, normText } from '../shared';

export default function RunnerSection(props: {
  repo: Accessor<Repo>;
  settings: Settings;
  onSaved: () => void;
}) {
  const drafts = createSeededDrafts(() => props.repo());
  // '' = inherit (null on the wire), exactly like every other nullable pick.
  const [runner, setRunner] = drafts.field<string>((r) => r.runner ?? '');
  // The three container resource-limit overrides (issue #205): nullable, same
  // '' ↔ null shape as every other nullable text/int repo field. They stay
  // meaningful only while runner is "container", but are always PATCHable —
  // an operator may stage a limit before flipping the runner over.
  const [containerMemory, setContainerMemory] = drafts.field((r) => r.container_memory ?? '');
  const [containerPids, setContainerPids] = drafts.field((r) =>
    r.container_pids === null ? '' : String(r.container_pids),
  );
  const [containerNofile, setContainerNofile] = drafts.field((r) =>
    r.container_nofile === null ? '' : String(r.container_nofile),
  );
  // The per-repo dev image override (issue #207): nullable, same '' ↔ null
  // shape as the limits above. The server resolves and digest-pins the ref on
  // save (https registries only) — a bad ref surfaces as a 400 in the
  // section's Banner via useSettingsForm, not a client-side check here.
  const [imageRef, setImageRef] = drafts.field((r) => r.image_ref ?? '');

  // The inherited global defaults (issue #205), read from GET /settings —
  // SeedDefaultSettings always seeds these three, so the fallback literals
  // below only cover a settings fetch that hasn't landed yet.
  const memoryDefault = () => props.settings.container_memory ?? '8g';
  const pidsDefault = () => props.settings.container_pids ?? 4096;
  const nofileDefault = () => props.settings.container_nofile ?? 16384;

  // The effective runner (issue #55): the drafted pick, else the global runner
  // default — so the host hint and the inherit row's "currently …" both track
  // the live draft and the live setting.
  const effectiveRunner = () => (runner() === '' ? props.settings.runner_default : runner());
  const inheritLabel = () => {
    const current = runnerName(props.settings.runner_default);
    return current === null
      ? 'Inherit global default'
      : `Inherit global default — currently ${current}`;
  };

  // The image a blank dev image field would inherit (issue #55): the global
  // default dev image setting, else the server's --container-image flag.
  const inheritedImage = () =>
    props.settings.dev_image_default || props.settings.dev_image_fallback || '';

  const buildPatch = (): RepoPatch | string => {
    // Diff against the seed the drafts came from — NOT the live props.repo().
    // Diffing against the live repo would mark a stale draft of a field the
    // operator never touched as "dirty" and PATCH the old value back.
    const current = drafts.seed();
    const patch: RepoPatch = {};

    if (runner() !== (current.runner ?? '')) {
      patch.runner = runner() === '' ? null : (runner() as Runner);
    }

    const memory = normText(containerMemory());
    if (memory !== current.container_memory) patch.container_memory = memory;

    const pids = normInt(containerPids());
    if (pids === undefined) return 'Container PID limit must be a whole number.';
    if (pids !== current.container_pids) patch.container_pids = pids;

    const nofile = normInt(containerNofile());
    if (nofile === undefined) return 'Container open-files limit must be a whole number.';
    if (nofile !== current.container_nofile) patch.container_nofile = nofile;

    const image = normText(imageRef());
    if (image !== current.image_ref) patch.image_ref = image;

    return patch;
  };

  const dirty = () => {
    const p = buildPatch();
    return typeof p === 'string' || Object.keys(p).length > 0;
  };

  const form = useSettingsForm<RepoPatch>({
    dirty,
    buildPatch,
    submit: (patch) => updateRepo(props.repo().id, patch),
    onSaved: () => props.onSaved(),
  });

  return (
    <form onSubmit={(e) => void form.save(e)} class="stack">
      <Banner message={form.error()} onDismiss={() => form.setError(null)} />
      <Banner message={form.note()} variant="success" />

      <SectionCard title="Runner">
        <Select
          skin="field"
          label="Runner"
          name="runner"
          value={runner()}
          options={RUNNER_OPTIONS}
          inheritLabel={inheritLabel()}
          onChange={setRunner}
        />
        <Show when={effectiveRunner() === 'host'}>
          <small class="hint hint-block">{HOST_RUNNER_HINT}</small>
        </Show>
      </SectionCard>

      <SectionCard title="Dev image" hint="Applies to container runs only.">
        <label class="field">
          <span>Image reference</span>
          <input
            type="text"
            name="image_ref"
            autocomplete="off"
            placeholder={DEV_IMAGE_PLACEHOLDER}
            value={imageRef()}
            onInput={(e) => setImageRef(e.currentTarget.value)}
          />
          <small class="hint">
            Resolved and pinned to a digest on save.{' '}
            <Show
              when={inheritedImage() !== ''}
              fallback="No dev image is configured — container spawns are refused until one is set here, in global Settings, or on the server."
            >
              Blank inherits <code>{inheritedImage()}</code>.
            </Show>
          </small>
        </label>
      </SectionCard>

      <SectionCard title="Container limits" hint="Limits apply to container runs only.">
        <label class="field">
          <span>Memory</span>
          <input
            type="text"
            name="container_memory"
            autocomplete="off"
            placeholder="global default"
            value={containerMemory()}
            onInput={(e) => setContainerMemory(e.currentTarget.value)}
          />
          <small class="hint">Inherit global default — currently {memoryDefault()}</small>
        </label>
        <label class="field">
          <span>Process limit (pids)</span>
          <input
            type="number"
            name="container_pids"
            min="1"
            step="1"
            autocomplete="off"
            placeholder="global default"
            value={containerPids()}
            onInput={(e) => setContainerPids(e.currentTarget.value)}
          />
          <small class="hint">Inherit global default — currently {pidsDefault()}</small>
        </label>
        <label class="field">
          <span>Open files (nofile)</span>
          <input
            type="number"
            name="container_nofile"
            min="1"
            step="1"
            autocomplete="off"
            placeholder="global default"
            value={containerNofile()}
            onInput={(e) => setContainerNofile(e.currentTarget.value)}
          />
          <small class="hint">Inherit global default — currently {nofileDefault()}</small>
        </label>
      </SectionCard>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save changes'}
      </button>
    </form>
  );
}
