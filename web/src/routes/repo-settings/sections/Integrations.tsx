// Integrations section (issue #61): the git credential, the tracker binding
// and the forge credential — a thin renderer over the form store. Every
// control edits a draft; the page's save bar sends the changed ones
// (form.tsx, fields.ts). Each credential pick offers only its own kinds.
//
// Only what applies (issue #61 §7): the builtin tracker binding reads no
// forge credential, so with that binding DRAFTED the field is replaced by a
// note. It shows again while it has a pending change or a problem, and when a
// link or a Save points at it; folding never clears or changes its value.
//
// A value is never misrepresented: a stored credential always shows as an
// entry of its pick — also while the credential list is loading, when it
// could not be loaded (the picks are then disabled, and the section says so
// with a way to try again), and when the list no longer carries it.

import { Show, createComputed, createSignal } from 'solid-js';
import type { CredentialListItem } from '../../../api';
import Banner from '../../../components/Banner';
import { NativeSelectField, SegmentedField } from '../Field';
import { useRepoSettingsForm } from '../form';

type Option = { value: string; label: string };

export default function IntegrationsSection(props: {
  /** The credential list; undefined while it loads and after a failed load. */
  credentials: CredentialListItem[] | undefined;
  /** Why the list could not be loaded, or null. */
  credentialsError: string | null;
  onRetryCredentials: () => void;
}) {
  const form = useRepoSettingsForm();
  const forgeCredential = form.field('forge_credential_id');
  const known = (): boolean => props.credentials !== undefined;
  // Nothing to pick from, and no telling what a pick would mean.
  const unusable = (): boolean => props.credentialsError !== null;

  // The picked credential is always an entry, whatever the list holds: a
  // pick without its entry would read as the first one — "None".
  const withPicked = (options: Option[], picked: string): Option[] => {
    if (picked === '' || options.some((option) => option.value === picked)) return options;
    return [
      ...options,
      {
        value: picked,
        label: known()
          ? `Unknown credential (${picked})`
          : `Credential ${picked} (list not loaded)`,
      },
    ];
  };
  const gitCredentials = (): Option[] =>
    withPicked(
      [
        { value: '', label: 'None (public remote)' },
        ...(props.credentials ?? [])
          .filter((c) => c.kind === 'ssh_key' || c.kind === 'https_token')
          .map((c) => ({
            value: c.id,
            label: `${c.name} (${c.kind === 'ssh_key' ? 'SSH key' : 'HTTPS token'})`,
          })),
      ],
      form.field('credential_id').value(),
    );
  const forgeCredentials = (): Option[] =>
    withPicked(
      [
        { value: '', label: 'None' },
        ...(props.credentials ?? [])
          .filter((c) => c.kind === 'forge_token')
          .map((c) => ({ value: c.id, label: c.name })),
      ],
      forgeCredential.value(),
    );

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
      <Banner
        message={
          props.credentialsError !== null
            ? `The credentials could not be loaded, so they cannot be changed here. ${props.credentialsError}`
            : null
        }
        action={
          <button type="button" onClick={() => props.onRetryCredentials()}>
            Try again
          </button>
        }
      />
      <NativeSelectField name="credential_id" options={gitCredentials()} disabled={unusable()} />
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
          disabled={unusable()}
          hint="Forge API token for issues and PRs — required for the forge binding, never given to runs."
        />
      </Show>
    </div>
  );
}
