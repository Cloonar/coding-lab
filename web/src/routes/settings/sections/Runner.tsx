// Global settings › Runner (issue #55): the operator's runtime-editable
// defaults for WHERE sessions run — the global runner default, the global
// default dev image, and the global container resource limits — mirroring the
// repo-settings Runner section (three cards, one form, one Save). Saved as a
// dirty-fields-only PATCH through useSettingsForm like General/Agents: drafts
// seed from the mounted snapshot, buildPatch diffs against it, and the
// server's 400 {"error"} (a dev image ref that cannot be resolved, a bad memory
// grammar, a limit below its floor) lands in the section banner. Runtime reads
// settings per spawn, so a save applies without a restart.
//
// Two deliberate details:
//  - Switching the runner default TO host asks for one window.confirm naming
//    how many repos inherit it (inheritance is live, so the flip reaches every
//    one of them at its next spawn). The confirm wraps the form's submit, NOT
//    the useSettingsForm submit callback: a declined confirm must save nothing
//    and leave every draft in place, whereas a no-op submit would still note
//    "Saved." and refetch (remounting the section and dropping the drafts).
//    Switching to container never confirms — the spawn refusal is where an
//    unready host is reported, not this form.
//  - dev_image_fallback (the server's --container-image flag) is read-only. It
//    is only ever displayed — in the hint under a blank dev image field — and
//    never enters a PATCH: buildPatch below builds from its own explicit key
//    lists, which keeps it out (the server 400s a PATCH that carries it).

import { Show, createResource, createSignal } from 'solid-js';
import {
  listRepos,
  updateSettings,
  type IntSettingKey,
  type Settings,
  type TextSettingKey,
} from '../../../api';
import Banner from '../../../components/Banner';
import SectionCard from '../../../components/SectionCard';
import Select from '../../../components/Select';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import { DEV_IMAGE_PLACEHOLDER, HOST_RUNNER_HINT, RUNNER_OPTIONS } from '../../../lib/runner';
import { resourceValue } from '../../../lib/resource';

/** The writable text keys of this section. Deliberately excludes the
 *  read-only dev_image_fallback (see the header comment). */
const TEXT_KEYS: TextSettingKey[] = ['runner_default', 'dev_image_default', 'container_memory'];

/** The two global limit ints, with the operator-facing name used in the
 *  client-side "whole number" error (same wording as the repo page's). */
const LIMIT_INT_FIELDS: { key: IntSettingKey; error: string }[] = [
  { key: 'container_pids', error: 'Container PID limit must be a whole number.' },
  { key: 'container_nofile', error: 'Container open-files limit must be a whole number.' },
];

/** String draft of one settings value ('' for an absent/null key). */
function seedDraft(initial: Settings, key: IntSettingKey | TextSettingKey): string {
  const value = initial[key];
  return value === undefined || value === null ? '' : String(value);
}

/** "3 repos inherit this default." — the line under the Runner picker. */
function inheritLine(count: number): string {
  if (count === 0) return 'No repos inherit this default.';
  if (count === 1) return '1 repo inherits this default.';
  return `${count} repos inherit this default.`;
}

/** The one confirmation a switch TO host asks for, naming the inheriting repos. */
function hostConfirmMessage(count: number | null): string {
  const head = 'Switch the global runner default to Host?';
  if (count === null) {
    return `${head} Repos that inherit it will run their next sessions unsandboxed with full host access.`;
  }
  if (count === 0) {
    return `${head} No repos inherit it now, but new repos will, and their sessions will run unsandboxed with full host access.`;
  }
  if (count === 1) {
    return `${head} 1 repo inherits it and its next session will run unsandboxed with full host access.`;
  }
  return `${head} ${count} repos inherit it and their next sessions will run unsandboxed with full host access.`;
}

export default function Runner(props: { initial: Settings; onSaved: () => void }) {
  // Drafts seed from the settings snapshot this section mounted with; a save →
  // refetch remounts it (index.tsx keys the section on the settings object), so
  // the seed is always the freshly-saved state (e.g. the digest-pinned image).
  const initial = props.initial;
  const [drafts, setDrafts] = createSignal<Record<string, string>>(
    Object.fromEntries(
      [...TEXT_KEYS, ...LIMIT_INT_FIELDS.map((f) => f.key)].map((key) => [
        key,
        seedDraft(initial, key),
      ]),
    ),
  );
  const draft = (key: string) => drafts()[key] ?? '';
  const setDraft = (key: string, value: string) => setDrafts({ ...drafts(), [key]: value });

  // How many repos currently inherit the runner default (repos.runner NULL).
  // Never blocks the form: while loading, or if the fetch fails, the count is
  // unknown (null) — the line is omitted and the confirm falls back to
  // wording that names no number.
  const [repos] = createResource(() => listRepos());
  const inheritingCount = (): number | null => {
    const list = resourceValue(repos);
    return list === undefined ? null : list.filter((repo) => repo.runner === null).length;
  };
  const inheritText = (): string | null => {
    const count = inheritingCount();
    return count === null ? null : inheritLine(count);
  };

  const dirtyKey = (key: IntSettingKey | TextSettingKey) =>
    draft(key).trim() !== seedDraft(initial, key).trim();

  const buildPatch = (): Settings | string => {
    const patch: Settings = {};
    for (const key of TEXT_KEYS) {
      if (!dirtyKey(key)) continue;
      const value = draft(key).trim();
      // These are concrete values, not overrides: there is no inherit state to
      // clear to, so a blank memory limit is an error rather than a clear. The
      // grammar itself (and the int floors) stay the server's call.
      if (key === 'container_memory' && value === '') {
        return 'Container memory limit must not be blank.';
      }
      patch[key] = value;
    }
    for (const { key, error } of LIMIT_INT_FIELDS) {
      if (!dirtyKey(key)) continue;
      const value = draft(key).trim();
      if (!/^\d+$/.test(value)) return error;
      patch[key] = Number(value);
    }
    return patch;
  };

  // One source of truth for the leave guard: dirty is derived straight from
  // buildPatch (a string result means "dirty but blocked by a field error").
  const dirty = () => {
    const patch = buildPatch();
    return typeof patch === 'string' || Object.keys(patch).length > 0;
  };

  const form = useSettingsForm<Settings>({
    dirty,
    buildPatch,
    submit: (patch) => updateSettings(patch),
    onSaved: () => props.onSaved(),
  });

  // A switch TO host (draft host, seed anything else — runner_default is only in
  // the patch when dirty) asks once; declining stops the whole submit, so no
  // PATCH goes out, not even for other dirty fields of this section.
  const onSubmit = (event: SubmitEvent) => {
    const patch = buildPatch();
    if (
      typeof patch !== 'string' &&
      patch.runner_default === 'host' &&
      !window.confirm(hostConfirmMessage(inheritingCount()))
    ) {
      event.preventDefault();
      return;
    }
    void form.save(event);
  };

  const devImageBlank = () => draft('dev_image_default').trim() === '';
  const fallback = () => initial.dev_image_fallback ?? '';

  return (
    <form onSubmit={onSubmit} class="stack">
      <Banner message={form.error()} onDismiss={() => form.setError(null)} />
      <Banner message={form.note()} variant="success" />

      <SectionCard
        title="Runner"
        hint="Where sessions run for every repo that inherits the default. A repo can pin its own."
      >
        <Select
          skin="field"
          label="Runner"
          name="runner_default"
          value={draft('runner_default')}
          options={RUNNER_OPTIONS}
          onChange={(value) => setDraft('runner_default', value)}
        />
        <Show when={draft('runner_default') === 'host'}>
          <small class="hint hint-block">{HOST_RUNNER_HINT}</small>
        </Show>
        <Show when={inheritText()}>
          {(text) => <small class="hint hint-block">{text()}</small>}
        </Show>
      </SectionCard>

      <SectionCard
        title="Dev image"
        hint="Applies to container runs only. A repo's own image overrides it."
      >
        <label class="field">
          <span>Image reference</span>
          <input
            type="text"
            name="dev_image_default"
            autocomplete="off"
            spellcheck={false}
            placeholder={DEV_IMAGE_PLACEHOLDER}
            value={draft('dev_image_default')}
            onInput={(e) => setDraft('dev_image_default', e.currentTarget.value)}
          />
          {/* While the field is blank the hint names what a blank falls through
              to — the deployed image (the server's --container-image flag), or
              the plain fact that nothing is configured. With a value typed it
              explains the pinning instead; the fallback is never its own
              field or row. */}
          <small class="hint">
            <Show when={devImageBlank()} fallback="Resolved and pinned to a digest on save.">
              <Show
                when={fallback() !== ''}
                fallback="No dev image is configured — container spawns are refused until one is set here, per repo, or on the server."
              >
                Blank uses the deployed image <code>{fallback()}</code>.
              </Show>
            </Show>
          </small>
        </label>
      </SectionCard>

      <SectionCard
        title="Container limits"
        hint="Limits apply to container runs only. A repo can override each one."
      >
        <label class="field">
          <span>Memory</span>
          <input
            type="text"
            name="container_memory"
            autocomplete="off"
            spellcheck={false}
            value={draft('container_memory')}
            onInput={(e) => setDraft('container_memory', e.currentTarget.value)}
          />
          <small class="hint">Podman memory limit, e.g. 512m or 8g.</small>
        </label>
        <label class="field">
          <span>Process limit (pids)</span>
          <input
            type="number"
            name="container_pids"
            min="1"
            step="1"
            autocomplete="off"
            value={draft('container_pids')}
            onInput={(e) => setDraft('container_pids', e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Open files (nofile)</span>
          <input
            type="number"
            name="container_nofile"
            min="1"
            step="1"
            autocomplete="off"
            value={draft('container_nofile')}
            onInput={(e) => setDraft('container_nofile', e.currentTarget.value)}
          />
        </label>
      </SectionCard>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save settings'}
      </button>
    </form>
  );
}
