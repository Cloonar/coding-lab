// Global settings › General (issue #198): the git author identity used for
// commits unless a repo overrides it. Ported field-for-field from the old
// Settings monolith's "Git author" card — same name attrs, labels and hints —
// onto the shared useSettingsForm primitive: drafts seed from the mounted
// snapshot, buildPatch diffs against that seed and sends only dirty fields
// (trimmed), and the unsaved-changes guard is armed off the same buildPatch.

import { Show, createSignal } from 'solid-js';
import { updateSettings, type Settings, type TextSettingKey } from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';
import SectionCard from '../../../components/SectionCard';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';

const TEXT_KEYS: TextSettingKey[] = ['git_author_name', 'git_author_email'];

/** String draft of one settings value ('' for an absent/null key). */
function seedDraft(initial: Settings, key: TextSettingKey): string {
  const value = initial[key];
  return value === undefined || value === null ? '' : String(value);
}

export default function General(props: { initial: Settings; onSaved: () => void }) {
  // Drafts seed from the settings snapshot this section mounted with; buildPatch
  // diffs against that snapshot so only edited fields enter the PATCH. A save →
  // refetch remounts this component (index.tsx keys the section on the settings
  // object), so the seed is always the freshly-saved state.
  const initial = props.initial;
  const [drafts, setDrafts] = createSignal<Record<string, string>>({
    git_author_name: seedDraft(initial, 'git_author_name'),
    git_author_email: seedDraft(initial, 'git_author_email'),
  });
  const draft = (key: string) => drafts()[key] ?? '';
  const setDraft = (key: string, value: string) => setDrafts({ ...drafts(), [key]: value });

  const textDirty = (key: TextSettingKey) => draft(key).trim() !== seedDraft(initial, key).trim();

  const buildPatch = (): Settings => {
    const patch: Settings = {};
    for (const key of TEXT_KEYS) {
      if (textDirty(key)) patch[key] = draft(key).trim();
    }
    return patch;
  };

  // One source of truth for the leave guard: dirty is derived straight from
  // buildPatch, never a parallel bookkeeping signal.
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

  return (
    <form onSubmit={(e) => void form.save(e)} class="stack">
      <ErrorBanner message={form.error()} onDismiss={() => form.setError(null)} />
      <Show when={form.note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{form.note()}</span>
        </div>
      </Show>

      <SectionCard title="Git author">
        <label class="field">
          <span>Author name</span>
          <input
            type="text"
            name="git_author_name"
            autocomplete="off"
            value={draft('git_author_name')}
            onInput={(e) => setDraft('git_author_name', e.currentTarget.value)}
          />
          <small class="hint">Used for commits unless a repo overrides it.</small>
        </label>
        <label class="field">
          <span>Author email</span>
          <input
            type="text"
            name="git_author_email"
            autocomplete="off"
            spellcheck={false}
            value={draft('git_author_email')}
            onInput={(e) => setDraft('git_author_email', e.currentTarget.value)}
          />
        </label>
      </SectionCard>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save settings'}
      </button>
    </form>
  );
}
