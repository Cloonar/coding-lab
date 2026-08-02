// Imports section suite (issue #261): declared imports — other registered
// lab repos this repo's instances may read as read-only snapshots — on
// /repos/:id/settings/imports. Modeled on Secrets.test.tsx: the section is
// device-local/immediate (no useSettingsForm, no leave guard).

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  button,
  chooseFromSelect,
  container,
  h,
  importsSection,
  installRepoSettingsHooks,
  mountSettings,
  optionRows,
  selectTrigger,
  settle,
  submitFormWithin,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountImports = () => mountSettings(`/repos/${REPO_ID}/settings/imports`);

describe('RepoSettings imports section', () => {
  it('renders declared imports by name', async () => {
    h.importsOnServer = [
      { id: 'repo_2', name: 'other-repo' },
      { id: 'repo_3', name: 'third-repo' },
    ];
    await mountImports();
    await waitFor(
      () => container.querySelector('section h2') && importsSection(),
      'imports section',
    );

    const section = importsSection();
    expect(section.textContent).toContain('other-repo');
    expect(section.textContent).toContain('third-repo');
  });

  it('renders the empty state when the repo has no imports', async () => {
    await mountImports();
    await waitFor(
      () => (importsSection().textContent?.includes('No imports declared') ? true : null),
      'empty state',
    );
    expect(importsSection().textContent).toContain('No imports declared');
  });

  it('the add-import dropdown excludes this repo and already-imported repos', async () => {
    // repo_1 is this repo (excluded as self); repo_2 is already imported
    // (excluded so re-adding can't even be attempted); repo_3 is the only
    // repo left to offer — still cloning, and offered anyway (not the guard).
    h.importsOnServer = [{ id: 'repo_2', name: 'other-repo' }];
    await mountImports();
    await waitFor(() => importsSection(), 'imports section');

    button('+ Add import').click();
    await settle();

    selectTrigger('target_repo_id').click();
    await settle();

    const labels = optionRows().map(
      (row) => row.querySelector('.select-option-label')?.textContent,
    );
    expect(labels).not.toContain('coding-lab');
    expect(labels).not.toContain('other-repo');
    expect(labels).toContain('third-repo');
  });

  it('add-import form submits target_repo_id and refreshes the list', async () => {
    await mountImports();
    await waitFor(() => importsSection(), 'imports section');

    button('+ Add import').click();
    await settle();

    await chooseFromSelect('target_repo_id', 'other-repo');
    submitFormWithin(importsSection());
    await settle();

    expect(h.importPostBodies).toEqual([{ target_repo_id: 'repo_2' }]);
    // The add form closes and the list reflects the newly declared import.
    expect(container.querySelector('button[name="target_repo_id"]')).toBeNull();
    expect(importsSection().textContent).toContain('other-repo');
  });

  it('remove asks for confirmation before removing the import', async () => {
    h.importsOnServer = [{ id: 'repo_2', name: 'other-repo' }];
    await mountImports();
    await waitFor(() => importsSection().querySelector('.card-title'), 'import row');

    // Declining the confirm leaves the import in place.
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValueOnce(false);
    button('Remove').click();
    await settle();
    expect(confirmSpy).toHaveBeenCalledWith('Remove import "other-repo"?');
    expect(importsSection().textContent).toContain('other-repo');

    // Confirming removes it.
    confirmSpy.mockReturnValueOnce(true);
    button('Remove').click();
    await settle();
    expect(h.importDeleteRequests).toEqual([`/api/v1/repos/${REPO_ID}/imports/repo_2`]);
    expect(h.importsOnServer).toHaveLength(0);
    expect(importsSection().textContent).toContain('No imports declared');
  });

  it('a 400 from the add-import POST surfaces in the ErrorBanner', async () => {
    // The picker already excludes self and already-imported targets, so the
    // real self-import/unknown-target 400s can't be reached by driving the
    // UI normally — h.importPostError forces the response the server would
    // give on a race (e.g. the target got deleted between page load and
    // submit) to prove the section surfaces it verbatim.
    h.importPostError = 'imports: a repository cannot import itself';
    await mountImports();
    await waitFor(() => importsSection(), 'imports section');

    button('+ Add import').click();
    await settle();
    await chooseFromSelect('target_repo_id', 'other-repo');
    submitFormWithin(importsSection());
    await settle();

    expect(importsSection().textContent).toContain('imports: a repository cannot import itself');
    // Nothing was added and the form stayed open.
    expect(h.importsOnServer).toHaveLength(0);
    expect(container.querySelector('button[name="target_repo_id"]')).not.toBeNull();
  });
});
