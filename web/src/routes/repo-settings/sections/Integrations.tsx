// Integrations section (issue #61): the git credential, the tracker binding
// and the forge credential — a thin renderer over the form store. Every
// control edits a draft; the page's save bar sends the changed ones
// (form.tsx, fields.ts). Each credential pick offers only its own kinds.

import type { CredentialListItem } from '../../../api';
import { NativeSelectField, SegmentedField } from '../Field';

export default function IntegrationsSection(props: { credentials: CredentialListItem[] }) {
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
      <NativeSelectField
        name="forge_credential_id"
        options={forgeCredentials()}
        hint="Forge API token for issues and PRs — required for the forge binding, never given to runs."
      />
    </div>
  );
}
