// Branches section suite (issue #198): per-section dirty-only PATCH, plus the
// monolith's stale-draft regression cases — they centered on default_branch
// (the verified clone-detection scenario), so they moved here with their
// field; the formerly cross-card halves (budget / name) now use Branches
// fields.
//
// The seed/resync contract under test: drafts seed from a repo SNAPSHOT.
// When an SSE repo.changed refetch lands while the form stays mounted (e.g.
// clone completes and default-branch detection rewrites default_branch), a
// save of an UNRELATED field must not diff stale drafts against the refreshed
// repo and silently PATCH old values back. Untouched drafts follow the
// server; dirty drafts keep the operator's edit.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  container,
  emitRepoChanged,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  settle,
  submitForm,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountBranches = () => mountSettings(`/repos/${REPO_ID}/settings/branches`);

describe('repo-settings Branches section', () => {
  it('editing the AFK branch pattern PATCHes exactly that field', async () => {
    await mountBranches();
    const pattern = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="afk_branch_pattern"]'),
      'branches form',
    );
    expect(pattern.value).toBe('afk/<N>');

    typeInto(pattern, 'issue-<N>');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ afk_branch_pattern: 'issue-<N>' }]);
    expect(h.repoOnServer.afk_branch_pattern).toBe('issue-<N>');
  });
});

describe('RepoSettings stale-draft handling', () => {
  it('saving an unrelated edit after a server-side default_branch change PATCHes only that edit', async () => {
    await mountBranches();
    const branch = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="default_branch"]'),
      'settings form',
    );
    expect(branch.value).toBe('main');

    // Clone completes: detection rewrites default_branch server-side and the
    // page refetches on repo.changed while the form stays mounted.
    h.repoOnServer = { ...h.repoOnServer, default_branch: 'master', clone_status: 'ready' };
    emitRepoChanged();
    await settle();

    // The untouched draft follows the server...
    expect(branch.value).toBe('master');

    // ...and saving an edit to ONLY the manual prefix must not revert it.
    typeInto(input('manual_branch_prefix'), 'wip/');
    submitForm();
    await settle();

    expect(h.patchBodies).toEqual([{ manual_branch_prefix: 'wip/' }]);
    expect(h.repoOnServer.default_branch).toBe('master');

    // The seed advanced with the save: a second submit has nothing to send.
    submitForm();
    await settle();
    expect(h.patchBodies).toHaveLength(1);
  });

  it('keeps a dirty draft across a refetch and PATCHes only that field', async () => {
    await mountBranches();
    const branch = await waitFor(
      () => container.querySelector<HTMLInputElement>('input[name="default_branch"]'),
      'settings form',
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

    submitForm();
    await settle();

    // Only the operator's edit is PATCHed — the server-side pattern change is
    // not clobbered back to the stale draft value.
    expect(h.patchBodies).toEqual([{ default_branch: 'trunk' }]);
    expect(h.repoOnServer.afk_branch_pattern).toBe('task/<N>');
  });
});
