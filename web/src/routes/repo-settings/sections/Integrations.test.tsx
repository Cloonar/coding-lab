// Integrations section suite (issue #198, new coverage): per-section
// dirty-only PATCH on the Credentials & tracker card — the native selects put
// exactly the edited field on the wire, and each credential select offers
// only its own kinds.

import { describe, expect, it } from 'vitest';
import type { CredentialListItem } from '../../../api';
import {
  REPO_ID,
  chooseNative,
  container,
  h,
  installRepoSettingsHooks,
  mountSettings,
  nativeSelect,
  settle,
  submitForm,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountIntegrations = () => mountSettings(`/repos/${REPO_ID}/settings/integrations`);

const GIT_CRED: CredentialListItem = {
  id: 'cred_git',
  name: 'deploy-key',
  kind: 'ssh_key',
  created_at: '2026-07-01T00:00:00.000Z',
  updated_at: '2026-07-01T00:00:00.000Z',
  referenced: false,
};

const FORGE_CRED: CredentialListItem = {
  id: 'cred_forge',
  name: 'forge-token',
  kind: 'forge_token',
  created_at: '2026-07-01T00:00:00.000Z',
  updated_at: '2026-07-01T00:00:00.000Z',
  referenced: false,
};

describe('repo-settings Integrations section', () => {
  it('flipping the tracker binding PATCHes exactly that field', async () => {
    await mountIntegrations();
    const binding = await waitFor(
      () => container.querySelector<HTMLSelectElement>('select[name="tracker_binding"]'),
      'integrations form',
    );
    expect(binding.value).toBe('forge');

    chooseNative('tracker_binding', 'builtin');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ tracker_binding: 'builtin' }]);
    expect(h.repoOnServer.tracker_binding).toBe('builtin');
  });

  it('picking a git credential PATCHes exactly credential_id, offering only git kinds', async () => {
    h.credentialsOnServer = [GIT_CRED, FORGE_CRED];
    await mountIntegrations();
    await waitFor(
      () => container.querySelector<HTMLSelectElement>('select[name="credential_id"]'),
      'integrations form',
    );

    // Each select filters to its own credential kinds.
    const gitOptions = Array.from(nativeSelect('credential_id').options).map((o) => o.value);
    expect(gitOptions).toEqual(['', 'cred_git']);
    const forgeOptions = Array.from(nativeSelect('forge_credential_id').options).map(
      (o) => o.value,
    );
    expect(forgeOptions).toEqual(['', 'cred_forge']);

    chooseNative('credential_id', 'cred_git');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ credential_id: 'cred_git' }]);
    expect(h.repoOnServer.credential_id).toBe('cred_git');
  });

  it('picking a forge credential PATCHes exactly forge_credential_id', async () => {
    h.credentialsOnServer = [GIT_CRED, FORGE_CRED];
    await mountIntegrations();
    await waitFor(
      () => container.querySelector<HTMLSelectElement>('select[name="forge_credential_id"]'),
      'integrations form',
    );

    chooseNative('forge_credential_id', 'cred_forge');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ forge_credential_id: 'cred_forge' }]);
    expect(h.repoOnServer.forge_credential_id).toBe('cred_forge');
  });
});
