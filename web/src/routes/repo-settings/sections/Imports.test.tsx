// Imports section suite (issue #61 §9, issue #261): declared imports — other
// registered lab repos this repo's instances may read as read-only snapshots
// — on /repos/:id/settings/imports. Every action is immediate: Remove at
// once with Undo in the toast, Add from a pick and a button.

import { describe, expect, it } from 'vitest';
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
  toastText,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountImports = () => mountSettings(`/repos/${REPO_ID}/settings/imports`);
const waitForList = () =>
  waitFor(() => importsSection().querySelector('button[name="target_repo_id"]'), 'imports list');

describe('RepoSettings imports section', () => {
  it('renders declared imports by name, each with Remove, and no duplicate title', async () => {
    h.importsOnServer = [
      { id: 'repo_2', name: 'other-repo' },
      { id: 'repo_3', name: 'third-repo' },
    ];
    await mountImports();
    await waitForList();

    const section = importsSection();
    expect(section.textContent).toContain('other-repo');
    expect(section.textContent).toContain('third-repo');
    expect(section.querySelectorAll('li.import-row')).toHaveLength(2);
    expect(button('Remove the import of other-repo').textContent).toBe('Remove');
    expect(section.querySelector('h2')).toBeNull();
  });

  it('renders the empty state when the repo has no imports', async () => {
    await mountImports();
    await waitForList();
    expect(importsSection().textContent).toContain('No imports. Runs only see this repository.');
  });

  it('the pick excludes this repo and already-imported repos', async () => {
    // repo_1 is this repo (excluded as self); repo_2 is already imported
    // (excluded so re-adding can't even be attempted); repo_3 is the only
    // repo left to offer — still cloning, and offered anyway (not the guard).
    h.importsOnServer = [{ id: 'repo_2', name: 'other-repo' }];
    await mountImports();
    await waitForList();

    selectTrigger('target_repo_id').click();
    await settle();

    const labels = optionRows().map(
      (row) => row.querySelector('.select-option-label')?.textContent,
    );
    expect(labels).not.toContain('coding-lab');
    expect(labels).not.toContain('other-repo');
    expect(labels).toContain('third-repo');
  });

  it('Add with nothing chosen asks for a pick; a pick adds and says so', async () => {
    await mountImports();
    await waitForList();

    button('Add').click();
    await settle();

    expect(h.importPostBodies).toHaveLength(0);
    expect(importsSection().querySelector('.sfield-error')?.textContent).toBe(
      'Choose a repository first.',
    );
    expect(selectTrigger('target_repo_id').getAttribute('aria-invalid')).toBe('true');

    await chooseFromSelect('target_repo_id', 'other-repo');
    expect(importsSection().querySelector('.sfield-error')).toBeNull();
    button('Add').click();
    await settle();

    expect(h.importPostBodies).toEqual([{ target_repo_id: 'repo_2' }]);
    expect(toastText()).toBe('coding-lab can now read other-repo');
    expect(importsSection().textContent).toContain('other-repo');
    // The pick is back to its placeholder, ready for the next one.
    expect(
      container.querySelector('button[name="target_repo_id"] .select-field-label')?.textContent,
    ).toBe('Choose a repository to import');
  });

  it('removes at once, and the toast offers Undo', async () => {
    h.importsOnServer = [{ id: 'repo_2', name: 'other-repo' }];
    await mountImports();
    await waitForList();

    button('Remove the import of other-repo').click();
    await settle();

    expect(h.importDeleteRequests).toEqual([`/api/v1/repos/${REPO_ID}/imports/repo_2`]);
    expect(h.importsOnServer).toHaveLength(0);
    expect(importsSection().textContent).toContain('No imports.');
    expect(toastText()).toContain('Removed the import of other-repo');

    const undo = container.querySelector<HTMLButtonElement>('.toast button');
    expect(undo?.textContent).toBe('Undo');
    undo?.click();
    await settle();

    expect(h.importPostBodies).toEqual([{ target_repo_id: 'repo_2' }]);
    expect(importsSection().textContent).toContain('other-repo');
  });

  it('says so when the Undo cannot declare the import again', async () => {
    h.importsOnServer = [{ id: 'repo_2', name: 'other-repo' }];
    await mountImports();
    await waitForList();

    button('Remove the import of other-repo').click();
    await settle();
    h.importPostError = 'imports: target repository not found';
    container.querySelector<HTMLButtonElement>('.toast button')?.click();
    await settle();

    expect(importsSection().querySelector('.banner.error')?.textContent).toContain(
      'Could not import other-repo again: imports: target repository not found',
    );
  });

  it('a 400 from the add POST surfaces in the banner', async () => {
    // The picker already excludes self and already-imported targets, so the
    // real self-import/unknown-target 400s can't be reached by driving the
    // UI normally — h.importPostError forces the response the server would
    // give on a race (e.g. the target got deleted between page load and
    // submit) to prove the section surfaces it verbatim.
    h.importPostError = 'imports: a repository cannot import itself';
    await mountImports();
    await waitForList();

    await chooseFromSelect('target_repo_id', 'other-repo');
    button('Add').click();
    await settle();

    expect(importsSection().textContent).toContain('imports: a repository cannot import itself');
    // Nothing was added and the pick stays.
    expect(h.importsOnServer).toHaveLength(0);
    expect(
      container.querySelector('button[name="target_repo_id"] .select-field-label')?.textContent,
    ).toBe('other-repo');
  });
});
