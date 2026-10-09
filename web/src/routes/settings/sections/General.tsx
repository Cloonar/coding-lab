// Global settings › General (issue #198): the git author identity used for
// commits unless a repo overrides it. Ported field-for-field from the old
// Settings monolith's "Git author" card — same name attrs, labels and hints —
// onto the shared useSettingsForm primitive: drafts seed from the mounted
// snapshot, buildPatch diffs against that seed and sends only dirty fields
// (trimmed), and the unsaved-changes guard is armed off the same buildPatch.
// The Transcripts card (issue #81) carries the one global retention knob,
// transcript_retention_days: a whole number 0..365 validated per field
// client-side exactly like Agents' int fields (an invalid dirty value blocks
// the save with 'Fix the highlighted fields first.'), the server's 400
// {"error"} landing in the banner otherwise.

import { Show, createSignal } from 'solid-js';
import {
  TRANSCRIPT_RETENTION_MAX_DAYS,
  updateSettings,
  type IntSettingKey,
  type Settings,
  type TextSettingKey,
} from '../../../api';
import Banner from '../../../components/Banner';
import BastionStatus from '../../../components/BastionStatus';
import CredentialGatewayStatus from '../../../components/CredentialGatewayStatus';
import SectionCard from '../../../components/SectionCard';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';

const TEXT_KEYS: TextSettingKey[] = ['git_author_name', 'git_author_email'];

/** The retention knob (issue #81) — this section's only int key. */
const RETENTION_KEY: IntSettingKey = 'transcript_retention_days';

/** String draft of one settings value ('' for an absent/null key). */
function seedDraft(initial: Settings, key: TextSettingKey | IntSettingKey): string {
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
    [RETENTION_KEY]: seedDraft(initial, RETENTION_KEY),
  });
  const draft = (key: string) => drafts()[key] ?? '';
  const setDraft = (key: string, value: string) => setDrafts({ ...drafts(), [key]: value });

  const textDirty = (key: TextSettingKey) => draft(key).trim() !== seedDraft(initial, key).trim();

  const retentionDirty = () => draft(RETENTION_KEY).trim() !== seedDraft(initial, RETENTION_KEY);

  /** Only a dirty value can be in error: 0 is valid (retain nothing), the
   *  server's cap is the ceiling — the same 0..365 the PATCH enforces. */
  const retentionError = (): string | null => {
    if (!retentionDirty()) return null;
    const trimmed = draft(RETENTION_KEY).trim();
    if (!/^\d+$/.test(trimmed)) return 'Enter a whole number.';
    if (Number(trimmed) > TRANSCRIPT_RETENTION_MAX_DAYS)
      return `Must be at most ${TRANSCRIPT_RETENTION_MAX_DAYS}.`;
    return null;
  };

  const buildPatch = (): Settings | string => {
    const patch: Settings = {};
    for (const key of TEXT_KEYS) {
      if (textDirty(key)) patch[key] = draft(key).trim();
    }
    if (retentionDirty()) {
      // A string result is the banner useSettingsForm surfaces (and still
      // counts as dirty for the leave guard), as in Agents.
      if (retentionError() !== null) return 'Fix the highlighted fields first.';
      patch[RETENTION_KEY] = Number(draft(RETENTION_KEY).trim());
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
    <>
      <form onSubmit={(e) => void form.save(e)} class="stack">
        <Banner message={form.error()} onDismiss={() => form.setError(null)} />
        <Banner message={form.note()} variant="success" />

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

        <SectionCard title="Transcripts">
          <label class="field">
            <span>Transcript retention (days)</span>
            <input
              type="text"
              inputmode="numeric"
              name="transcript_retention_days"
              autocomplete="off"
              value={draft(RETENTION_KEY)}
              onInput={(e) => setDraft(RETENTION_KEY, e.currentTarget.value)}
              aria-invalid={retentionError() !== null}
            />
            <Show
              when={retentionError()}
              fallback={
                <small class="hint">
                  Ended runs keep their transcript for this many days; 0 keeps none (max{' '}
                  {TRANSCRIPT_RETENTION_MAX_DAYS}).
                </small>
              }
            >
              <small class="field-error" role="alert">
                {retentionError()}
              </small>
            </Show>
          </label>
        </SectionCard>

        <button type="submit" class="primary wide" disabled={form.busy()}>
          {form.busy() ? 'Saving…' : 'Save settings'}
        </button>
      </form>

      {/* Read-only — not part of the settings PATCH, so it lives outside the
          form (issue #23): confirms the OneCLI sidecar is reachable before a
          run depends on it. */}
      <CredentialGatewayStatus />
      {/* Read-only, same reasons as above (issue #39): confirms the Warpgate
          SSH bastion sidecar is reachable, and surfaces a host-key mismatch
          that blocks target-bearing spawns until accepted. */}
      <BastionStatus />
    </>
  );
}
