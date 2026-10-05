// Integrations section (issue #61): the git credential, the tracker binding
// and the forge credential — a thin renderer over the form store. Every
// control edits a draft; the page's save bar sends the changed ones
// (form.tsx, fields.ts). Each credential pick offers only its own kinds.
//
// Only what applies (issue #61 §7): the builtin tracker binding reads no
// forge credential, so with that binding DRAFTED the field is replaced by a
// note. It shows again while it has a pending change or a problem, and when a
// link or a Save points at it; folding never clears or changes its value.

import { Show, createComputed, createSignal } from 'solid-js';
import type { CredentialListItem } from '../../../api';
import { NativeSelectField, SegmentedField } from '../Field';
import { useRepoSettingsForm } from '../form';

export default function IntegrationsSection(props: { credentials: CredentialListItem[] }) {
  const form = useRepoSettingsForm();
  const forgeCredential = form.field('forge_credential_id');

  const gitCredentials = () => [
    { value: '', label: 'None (public remote)' },
    ...props.credentials
      .filter((c) => c.kind === 'ssh_key' || c.kind === 'https_token')
      .map((c) => ({
        value: c.id,
        label: `${c.name} (${c.kind === 'ssh_key' ? 'SSH key' : 'HTTPS token'})`,
      })),
  ];
  const forgeCredentials = () => [
    { value: '', label: 'None' },
    ...props.credentials
      .filter((c) => c.kind === 'forge_token')
      .map((c) => ({ value: c.id, label: c.name })),
  ];

  const builtin = (): boolean => form.field('tracker_binding').value() === 'builtin';
  // Once something pointed at the field it stays shown.
  const [pointed, setPointed] = createSignal(false);
  createComputed(() => {
    if (form.pointedAt() === 'forge_credential_id') setPointed(true);
  });
  const folded = (): boolean =>
    builtin() && !pointed() && !forgeCredential.changed() && forgeCredential.error() === null;

  return (
    <div class="card settings-card">
      <NativeSelectField name="credential_id" options={gitCredentials()} />
      <SegmentedField
        name="tracker_binding"
        options={[
          { value: 'forge', label: 'Forge' },
          { value: 'builtin', label: 'Built-in' },
        ]}
        hint="Forge keeps issues and pull requests on the forge. Built-in keeps issues and change requests inside lab."
      />
      <Show
        when={!folded()}
        fallback={
          <p class="settings-na">The built-in tracker binding needs no forge credential.</p>
        }
      >
        <NativeSelectField
          name="forge_credential_id"
          options={forgeCredentials()}
          hint="Forge API token for issues and PRs — required for the forge binding, never given to runs."
        />
      </Show>
    </div>
  );
}
