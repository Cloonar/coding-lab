// Branches section suite (issues #198, #61): the section's fields on the
// one-page settings — a dirty-only PATCH through the page's one Save, the
// branch checks the browser runs first — plus the stale-draft regression
// cases, which centered on default_branch (the verified clone-detection
// scenario) and so live here with their field.
//
// The seed/resync contract under test: a field's draft is the operator's
// edit, else the live repo. When an SSE repo.changed refetch lands while the
// page stays mounted (e.g. clone completes and default-branch detection
// rewrites default_branch), a save of an UNRELATED field must not send stale
// values back. Untouched fields follow the server; dirty ones keep the
// operator's edit.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  container,
  emitRepoChanged,
  fieldError,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  save,
  saveBar,
  saveBarSections,
  saveBarTitle,
  settle,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountBranches = () => mountSettings(`/repos/${REPO_ID}/settings/branches`);

describe('repo-settings Branches section', () => {
  it('editing the AFK branch pattern PATCHes exactly that field, trimmed', async () => {
    await mountBranches();
    const pattern = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_branch_pattern"]'),
      'branches fields',
    );
    expect(pattern.value).toBe('afk/<N>');

    typeInto(pattern, ' issue-<N> ');
    await save();

    expect(h.patchBodies).toEqual([{ afk_branch_pattern: 'issue-<N>' }]);
    expect(h.repoOnServer.afk_branch_pattern).toBe('issue-<N>');
    // Saved: the field shows what the server holds now.
    expect(pattern.value).toBe('issue-<N>');
  });

  it('a pattern without <N> sends no request, shows the message under the field and focuses it', async () => {
    await mountBranches();
    const pattern = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_branch_pattern"]'),
      'branches fields',
    );

    typeInto(pattern, 'afk/');
    await save();

    expect(h.patchBodies).toHaveLength(0);
    expect(fieldError('afk_branch_pattern')).toBe(
      'The pattern needs <N> exactly once. It stands for the issue number.',
    );
    expect(pattern.getAttribute('aria-invalid')).toBe('true');
    expect(document.activeElement).toBe(pattern);
    expect(saveBarTitle()).toBe('1 problem to fix');
    expect(saveBarSections()).toEqual(['Branches']);
    // Save scrolled to the field.
    expect(h.scrolls.at(-1)?.target).toBe('afk_branch_pattern');

    // <N> twice is just as wrong; once is right.
    typeInto(pattern, 'afk/<N>/<N>');
    await settle();
    expect(fieldError('afk_branch_pattern')).not.toBeNull();
    typeInto(pattern, 'issue-<N>');
    await settle();
    expect(fieldError('afk_branch_pattern')).toBeNull();

    await save();
    expect(h.patchBodies).toEqual([{ afk_branch_pattern: 'issue-<N>' }]);
  });

  it('an empty default branch or manual prefix is refused in the browser', async () => {
    await mountBranches();
    const branch = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="default_branch"]'),
      'branches fields',
    );

    typeInto(branch, '  ');
    typeInto(input('manual_branch_prefix'), '');
    await save();

    expect(h.patchBodies).toHaveLength(0);
    expect(fieldError('default_branch')).toBe('Enter the default branch.');
    expect(fieldError('manual_branch_prefix')).toBe('Enter a prefix, for example lab/.');
    expect(saveBarTitle()).toBe('2 problems to fix');
    // The first problem in page order takes the focus.
    expect(document.activeElement).toBe(branch);
  });
});

describe('RepoSettings stale-draft handling', () => {
  it('saving an unrelated edit after a server-side default_branch change PATCHes only that edit', async () => {
    await mountBranches();
    const branch = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="default_branch"]'),
      'settings fields',
    );
    expect(branch.value).toBe('main');

    // Clone completes: detection rewrites default_branch server-side and the
    // frame refetches on repo.changed while the page stays mounted.
    h.repoOnServer = { ...h.repoOnServer, default_branch: 'master', clone_status: 'ready' };
    emitRepoChanged();
    await settle();

    // The untouched field follows the server...
    expect(branch.value).toBe('master');

    // ...and saving an edit to ONLY the manual prefix must not revert it.
    typeInto(input('manual_branch_prefix'), 'wip/');
    await save();

    expect(h.patchBodies).toEqual([{ manual_branch_prefix: 'wip/' }]);
    expect(h.repoOnServer.default_branch).toBe('master');

    // Saved: nothing is pending, so there is nothing to send a second time.
    expect(saveBar()).toBeNull();
    expect(h.patchBodies).toHaveLength(1);
  });

  it('keeps a dirty draft across a refetch and PATCHes only that field', async () => {
    await mountBranches();
    const branch = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="default_branch"]'),
      'settings fields',
    );
    typeInto(branch, 'trunk'); // operator edits default_branch first

    // Server-side changes land while the operator is mid-edit.
    h.repoOnServer = {
      ...h.repoOnServer,
      default_branch: 'master',
      afk_branch_pattern: 'task/<N>',
      clone_status: 'ready',
    };
    emitRepoChanged();
    await settle();

    expect(branch.value).toBe('trunk'); // dirty draft survives the refetch
    expect(input('afk_branch_pattern').value).toBe('task/<N>'); // untouched field follows

    await save();

    // Only the operator's edit is PATCHed — the server-side pattern change is
    // not clobbered back to a stale value.
    expect(h.patchBodies).toEqual([{ default_branch: 'trunk' }]);
    expect(h.repoOnServer.afk_branch_pattern).toBe('task/<N>');
  });

  it('a draft the server caught up with is clean again and follows the server from then on', async () => {
    await mountBranches();
    const branch = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="default_branch"]'),
      'settings fields',
    );
    typeInto(branch, 'master');
    await settle();
    expect(saveBarTitle()).toBe('1 unsaved change');

    // Detection lands on the very value the operator typed.
    h.repoOnServer = { ...h.repoOnServer, default_branch: 'master' };
    emitRepoChanged();
    await settle();
    expect(saveBar()).toBeNull();

    // A later server-side change is followed, not shown as a pending edit.
    h.repoOnServer = { ...h.repoOnServer, default_branch: 'trunk' };
    emitRepoChanged();
    await settle();
    expect(branch.value).toBe('trunk');
    expect(saveBar()).toBeNull();
  });
});
