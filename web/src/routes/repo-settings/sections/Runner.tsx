// Runner section (issue #205 / the 2026-07-22 container-isolation design):
// the schema/API/UI tracer-bullet slice's repo-settings card, modeled on
// Autoland.tsx — drafts seeded via createSeededDrafts, saved as a
// dirty-fields-only PATCH through useSettingsForm. The spawn path itself
// (podman argv, preflight, mounts) is a separate slice; this section only
// carries the repos.runner pick and its three container resource-limit
// overrides end to end.

import { Show } from 'solid-js';
import type { Accessor } from 'solid-js';
import { updateRepo, type Repo, type RepoPatch, type Runner, type Settings } from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';
import Select, { type SelectOption } from '../../../components/Select';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import { createSeededDrafts } from '../../../lib/seededDrafts';
import { normInt, normText } from '../shared';

// repos.runner is NOT NULL (no inherit state), so — unlike every Select in
// Agents/Autoland — this one carries no inheritLabel: the two rows are the
// whole catalog. "host" is deliberately spelled out as unsandboxed/break-glass
// here (issue #205's UI copy is pinned verbatim in the acceptance criteria).
const RUNNER_OPTIONS: SelectOption[] = [
  { value: 'container', label: 'Container — rootless podman' },
  { value: 'host', label: 'Host — unsandboxed, full host access' },
];

export default function RunnerSection(props: {
  repo: Accessor<Repo>;
  settings: Settings;
  onSaved: () => void;
}) {
  const drafts = createSeededDrafts(() => props.repo());
  const [runner, setRunner] = drafts.field((r) => r.runner);
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

  // The inherited global defaults (issue #205), read from GET /settings —
  // SeedDefaultSettings always seeds these three, so the fallback literals
  // below only cover a settings fetch that hasn't landed yet.
  const memoryDefault = () => props.settings.container_memory ?? '8g';
  const pidsDefault = () => props.settings.container_pids ?? 4096;
  const nofileDefault = () => props.settings.container_nofile ?? 16384;

  const buildPatch = (): RepoPatch | string => {
    // Diff against the seed the drafts came from — NOT the live props.repo().
    // Diffing against the live repo would mark a stale draft of a field the
    // operator never touched as "dirty" and PATCH the old value back.
    const current = drafts.seed();
    const patch: RepoPatch = {};

    if (runner() !== current.runner) patch.runner = runner();

    const memory = normText(containerMemory());
    if (memory !== current.container_memory) patch.container_memory = memory;

    const pids = normInt(containerPids());
    if (pids === undefined) return 'Container PID limit must be a whole number.';
    if (pids !== current.container_pids) patch.container_pids = pids;

    const nofile = normInt(containerNofile());
    if (nofile === undefined) return 'Container open-files limit must be a whole number.';
    if (nofile !== current.container_nofile) patch.container_nofile = nofile;

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
      <ErrorBanner message={form.error()} onDismiss={() => form.setError(null)} />
      <Show when={form.note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{form.note()}</span>
        </div>
      </Show>

      <section class="card">
        <h2>Runner</h2>
        <Select
          skin="field"
          label="Runner"
          name="runner"
          value={runner()}
          options={RUNNER_OPTIONS}
          onChange={(v) => setRunner(v as Runner)}
        />
        <Show when={runner() === 'host'}>
          <small class="hint hint-block">
            Host runs are unsandboxed — the agent has full host access to the server. Break-glass
            only; use the container runner once available.
          </small>
        </Show>
      </section>

      <section class="card">
        <h2>Container limits</h2>
        <small class="hint hint-block">Limits apply to container runs only.</small>
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
      </section>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save changes'}
      </button>
    </form>
  );
}
