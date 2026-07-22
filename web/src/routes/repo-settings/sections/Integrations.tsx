// Integrations section (issue #198): the monolith's Credentials & tracker
// card as one per-section form — drafts seeded via createSeededDrafts, saved
// as a dirty-fields-only PATCH through useSettingsForm.

import { For, Show } from 'solid-js';
import type { Accessor } from 'solid-js';
import {
  updateRepo,
  type CredentialListItem,
  type Repo,
  type RepoPatch,
  type TrackerBinding,
} from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import { createSeededDrafts } from '../../../lib/seededDrafts';

export default function IntegrationsSection(props: {
  repo: Accessor<Repo>;
  credentials: CredentialListItem[];
  onSaved: () => void;
}) {
  const drafts = createSeededDrafts(() => props.repo());
  const [credentialId, setCredentialId] = drafts.field((r) => r.credential_id ?? '');
  const [binding, setBinding] = drafts.field<TrackerBinding>((r) => r.tracker_binding);
  const [forgeCredentialId, setForgeCredentialId] = drafts.field(
    (r) => r.forge_credential_id ?? '',
  );

  const gitCredentials = () =>
    props.credentials.filter((c) => c.kind === 'ssh_key' || c.kind === 'https_token');
  const forgeCredentials = () => props.credentials.filter((c) => c.kind === 'forge_token');

  const buildPatch = (): RepoPatch | string => {
    // Diff against the seed the drafts came from — NOT the live props.repo().
    // Diffing against the live repo would mark a stale draft of a field the
    // operator never touched as "dirty" and PATCH the old value back.
    const current = drafts.seed();
    const patch: RepoPatch = {};

    const cred = credentialId() === '' ? null : credentialId();
    if (cred !== current.credential_id) patch.credential_id = cred;
    const forgeCred = forgeCredentialId() === '' ? null : forgeCredentialId();
    if (forgeCred !== current.forge_credential_id) patch.forge_credential_id = forgeCred;
    if (binding() !== current.tracker_binding) patch.tracker_binding = binding();

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
        <h2>Credentials & tracker</h2>
        <label class="field">
          <span>Git credential</span>
          <select
            name="credential_id"
            value={credentialId()}
            onChange={(e) => setCredentialId(e.currentTarget.value)}
          >
            <option value="">None (public remote)</option>
            <For each={gitCredentials()}>
              {(c) => (
                <option value={c.id}>
                  {c.name} ({c.kind === 'ssh_key' ? 'SSH key' : 'HTTPS token'})
                </option>
              )}
            </For>
          </select>
        </label>
        <label class="field">
          <span>Tracker binding</span>
          <select
            name="tracker_binding"
            value={binding()}
            onChange={(e) => setBinding(e.currentTarget.value as TrackerBinding)}
          >
            <option value="forge">Forge (Forgejo / GitHub issues + PRs)</option>
            <option value="builtin">Builtin (lab's own issues + CRs)</option>
          </select>
        </label>
        <label class="field">
          <span>Forge credential</span>
          <select
            name="forge_credential_id"
            value={forgeCredentialId()}
            onChange={(e) => setForgeCredentialId(e.currentTarget.value)}
          >
            <option value="">None</option>
            <For each={forgeCredentials()}>{(c) => <option value={c.id}>{c.name}</option>}</For>
          </select>
          <small class="hint">
            Forge API token for issues and PRs — required for the forge binding, never given to
            runs.
          </small>
        </label>
      </section>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save changes'}
      </button>
    </form>
  );
}
