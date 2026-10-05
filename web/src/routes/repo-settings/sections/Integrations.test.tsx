// Integrations section suite (issues #198, #61): the section's fields on the
// one-page settings — the tracker binding pick and the two credential picks
// each put exactly the edited field in the page's one PATCH, and each
// credential pick offers only its own kinds. With the builtin tracker binding
// drafted, the forge credential — which that binding does not read — is
// replaced by a note; its value stays as it is.

import { describe, expect, it } from 'vitest';
import type { CredentialListItem } from '../../../api';
import {
  REPO_ID,
  baseRepo,
  chooseNative,
  container,
  fieldChanged,
  fieldState,
  h,
  installRepoSettingsHooks,
  mountSettings,
  nativeSelect,
  pageSection,
  save,
  saveBarTitle,
  segment,
  segmentValue,
  settle,
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
    await waitFor(
      () => container.querySelector('button[role="radio"][name="tracker_binding"]'),
      'integrations fields',
    );
    expect(segmentValue('tracker_binding')).toBe('forge');
    // The pick is named by the field's label.
    const group = segment('tracker_binding', 'forge').closest('[role="radiogroup"]');
    expect(group?.getAttribute('aria-labelledby')).toBe('rs-tracker_binding-label');

    segment('tracker_binding', 'builtin').click();
    await settle();
    expect(segmentValue('tracker_binding')).toBe('builtin');
    expect(fieldChanged('tracker_binding')).toBe(true);
    await save();

    expect(h.patchBodies).toEqual([{ tracker_binding: 'builtin' }]);
    expect(h.repoOnServer.tracker_binding).toBe('builtin');
  });

  it('picking a git credential PATCHes exactly credential_id, offering only git kinds', async () => {
    h.credentialsOnServer = [GIT_CRED, FORGE_CRED];
    await mountIntegrations();
    await waitFor(
      () => container.querySelector('select[name="credential_id"] option[value="cred_git"]'),
      'credential options',
    );

    // Each select filters to its own credential kinds.
    const gitOptions = Array.from(nativeSelect('credential_id').options).map((o) => o.value);
    expect(gitOptions).toEqual(['', 'cred_git']);
    const forgeOptions = Array.from(nativeSelect('forge_credential_id').options).map(
      (o) => o.value,
    );
    expect(forgeOptions).toEqual(['', 'cred_forge']);

    chooseNative('credential_id', 'cred_git');
    await save();

    expect(h.patchBodies).toEqual([{ credential_id: 'cred_git' }]);
    expect(h.repoOnServer.credential_id).toBe('cred_git');
  });

  it('picking a forge credential PATCHes exactly forge_credential_id', async () => {
    h.credentialsOnServer = [GIT_CRED, FORGE_CRED];
    await mountIntegrations();
    await waitFor(
      () =>
        container.querySelector('select[name="forge_credential_id"] option[value="cred_forge"]'),
      'credential options',
    );

    chooseNative('forge_credential_id', 'cred_forge');
    await save();

    expect(h.patchBodies).toEqual([{ forge_credential_id: 'cred_forge' }]);
    expect(h.repoOnServer.forge_credential_id).toBe('cred_forge');
  });

  it('shows a stored credential as picked once the credential list has loaded', async () => {
    h.credentialsOnServer = [GIT_CRED, FORGE_CRED];
    h.repoOnServer = { ...h.repoOnServer, credential_id: 'cred_git' };
    await mountIntegrations();
    // Before the list is there the pick already is the stored credential…
    await waitFor(() => container.querySelector('select[name="credential_id"]'), 'the pick');
    expect(nativeSelect('credential_id').value).toBe('cred_git');
    // …and with the list it reads by its name.
    await waitFor(
      () =>
        nativeSelect('credential_id').selectedOptions[0]?.textContent === 'deploy-key (SSH key)'
          ? true
          : null,
      'credential options',
    );

    expect(nativeSelect('credential_id').value).toBe('cred_git');

    // Clearing it PATCHes null.
    chooseNative('credential_id', '');
    await save();
    expect(h.patchBodies).toEqual([{ credential_id: null }]);
  });
});

describe('repo-settings Integrations: a credential is never shown as something it is not', () => {
  const LOAD_ERROR = 'The credentials could not be loaded, so they cannot be changed here.';
  const banner = () => pageSection('integrations').querySelector('.banner.error');
  const picked = (name: string): string => {
    const select = nativeSelect(name);
    return select.selectedOptions[0]?.textContent ?? '';
  };

  it('a failed credential load says so, keeps the stored picks as they are, and offers a retry', async () => {
    h.credentialsOnServer = [GIT_CRED, FORGE_CRED];
    h.repoOnServer = {
      ...h.repoOnServer,
      credential_id: 'cred_git',
      forge_credential_id: 'cred_forge',
    };
    h.credentialsError = 'credentials: store unavailable';
    await mountIntegrations();
    await waitFor(banner, 'the load error');

    expect(banner()?.textContent).toContain(`${LOAD_ERROR} credentials: store unavailable`);
    // Not "None (public remote)": the repo HAS a credential, and the pick says so.
    expect(nativeSelect('credential_id').value).toBe('cred_git');
    expect(picked('credential_id')).toBe('Credential cred_git (list not loaded)');
    expect(picked('forge_credential_id')).toBe('Credential cred_forge (list not loaded)');
    // Nothing to pick from: the picks are held, and nothing is pending.
    expect(nativeSelect('credential_id').disabled).toBe(true);
    expect(nativeSelect('forge_credential_id').disabled).toBe(true);
    expect(saveBarTitle()).toBe('');

    h.credentialsError = null;
    Array.from(banner()?.querySelectorAll('button') ?? [])
      .find((b) => b.textContent === 'Try again')
      ?.click();
    await waitFor(() => (banner() === null ? true : null), 'the retry');

    expect(picked('credential_id')).toBe('deploy-key (SSH key)');
    expect(picked('forge_credential_id')).toBe('forge-token');
    expect(nativeSelect('credential_id').disabled).toBe(false);
  });

  it('a repo without a credential still reads None while the list cannot be loaded', async () => {
    h.credentialsError = 'credentials: store unavailable';
    await mountIntegrations();
    await waitFor(banner, 'the load error');

    expect(picked('credential_id')).toBe('None (public remote)');
    expect(nativeSelect('credential_id').value).toBe('');
  });

  it('a stored credential the list no longer carries is named as unknown, not as None', async () => {
    h.credentialsOnServer = [GIT_CRED];
    h.repoOnServer = { ...h.repoOnServer, credential_id: 'cred_gone' };
    await mountIntegrations();
    await waitFor(
      () => container.querySelector('select[name="credential_id"] option[value="cred_git"]'),
      'credential options',
    );

    expect(nativeSelect('credential_id').value).toBe('cred_gone');
    expect(picked('credential_id')).toBe('Unknown credential (cred_gone)');
    expect(banner()).toBeNull();
    // It can be replaced by one that exists.
    chooseNative('credential_id', 'cred_git');
    await save();
    expect(h.patchBodies).toEqual([{ credential_id: 'cred_git' }]);
  });
});

describe('repo-settings Integrations: the builtin tracker binding needs no forge credential', () => {
  const NOTE = 'The built-in tracker binding needs no forge credential.';
  const forgeField = () => container.querySelector('select[name="forge_credential_id"]');
  const text = () => pageSection('integrations').textContent ?? '';
  const waitForBinding = () =>
    waitFor(
      () => container.querySelector('button[role="radio"][name="tracker_binding"]'),
      'integrations fields',
    );

  it('replaces the forge credential with a note on a builtin-bound repo', async () => {
    h.repoOnServer = { ...baseRepo(), tracker_binding: 'builtin', forge_kind: 'none' };
    await mountIntegrations();
    await waitForBinding();

    expect(forgeField()).toBeNull();
    expect(text()).toContain(NOTE);
    // The git credential is read by both bindings and stays.
    expect(container.querySelector('select[name="credential_id"]')).not.toBeNull();
  });

  it('follows the DRAFT, and never clears the credential it hides', async () => {
    h.credentialsOnServer = [GIT_CRED, FORGE_CRED];
    h.repoOnServer = { ...baseRepo(), forge_credential_id: 'cred_forge' };
    await mountIntegrations();
    await waitFor(
      () =>
        container.querySelector('select[name="forge_credential_id"] option[value="cred_forge"]'),
      'credential options',
    );
    expect(text()).not.toContain(NOTE);

    segment('tracker_binding', 'builtin').click();
    await settle();
    expect(forgeField()).toBeNull();
    expect(text()).toContain(NOTE);
    expect(saveBarTitle()).toBe('1 unsaved change');
    await save();

    // Only the binding: the folded credential was neither sent nor cleared.
    expect(h.patchBodies).toEqual([{ tracker_binding: 'builtin' }]);
    expect(h.repoOnServer.forge_credential_id).toBe('cred_forge');

    // Back to forge: the credential is where it was.
    segment('tracker_binding', 'forge').click();
    await settle();
    expect(nativeSelect('forge_credential_id').value).toBe('cred_forge');
  });

  it('keeps a forge credential with a pending change on the page', async () => {
    h.credentialsOnServer = [GIT_CRED, FORGE_CRED];
    await mountIntegrations();
    await waitFor(
      () =>
        container.querySelector('select[name="forge_credential_id"] option[value="cred_forge"]'),
      'credential options',
    );

    chooseNative('forge_credential_id', 'cred_forge');
    segment('tracker_binding', 'builtin').click();
    await settle();

    expect(forgeField()).not.toBeNull();
    expect(fieldChanged('forge_credential_id')).toBe(true);
    expect(saveBarTitle()).toBe('2 unsaved changes');
    await save();
    expect(h.patchBodies).toEqual([
      { tracker_binding: 'builtin', forge_credential_id: 'cred_forge' },
    ]);
  });

  it('?field=forge_credential_id unfolds it on a builtin-bound repo', async () => {
    h.repoOnServer = { ...baseRepo(), tracker_binding: 'builtin', forge_kind: 'none' };
    await mountSettings(`/repos/${REPO_ID}/settings/integrations?field=forge_credential_id`);
    await waitFor(forgeField, 'the forge credential');
    await settle();

    expect(document.activeElement).toBe(forgeField());
    expect(text()).not.toContain(NOTE);
  });

  it('none of these fields is overridable: no inherited state at their labels', async () => {
    await mountIntegrations();
    await waitForBinding();

    for (const key of ['credential_id', 'tracker_binding', 'forge_credential_id']) {
      expect(fieldState(key)).toBeNull();
    }
  });
});
