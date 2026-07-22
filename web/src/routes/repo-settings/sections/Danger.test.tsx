// Danger zone suite (issue #198, new coverage): the delete flow on
// /repos/:id/settings/danger — confirm gate, navigation away on success, and
// the 409 → force-checkbox escalation with ?force=true on the retry.

import { describe, expect, it, vi } from 'vitest';
import {
  REPO_ID,
  button,
  container,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  routerHistory,
  settle,
  toggleCheckbox,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const DANGER_PATH = `/repos/${REPO_ID}/settings/danger`;
const mountDanger = () => mountSettings(DANGER_PATH);

describe('repo-settings Danger zone', () => {
  it('declining the confirm leaves the repo in place', async () => {
    await mountDanger();
    await waitFor(() => container.querySelector('.danger-zone'), 'danger zone');

    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false);
    button('Delete repository').click();
    await settle();

    expect(confirmSpy).toHaveBeenCalledWith(
      'Delete repository "coding-lab"? This removes lab\'s clone.',
    );
    expect(h.deleteRequests).toHaveLength(0);
    expect(routerHistory.get()).toBe(DANGER_PATH);
  });

  it('confirming DELETEs the repo and navigates away', async () => {
    await mountDanger();
    await waitFor(() => container.querySelector('.danger-zone'), 'danger zone');

    vi.spyOn(window, 'confirm').mockReturnValue(true);
    button('Delete repository').click();
    await settle();

    expect(h.deleteRequests).toEqual([`/api/v1/repos/${REPO_ID}`]);
    expect(routerHistory.get()).toBe('/');
  });

  it('a 409 reveals the force checkbox; the force retry sends ?force=true', async () => {
    h.deleteStatus = 409;
    await mountDanger();
    await waitFor(() => container.querySelector('.danger-zone'), 'danger zone');

    vi.spyOn(window, 'confirm').mockReturnValue(true);
    // No force checkbox before the conflict.
    expect(container.querySelector('input[name="force"]')).toBeNull();

    button('Delete repository').click();
    await settle();

    // The 409 surfaces its error and reveals the force escalation; the first
    // request went out WITHOUT force.
    expect(h.deleteRequests).toEqual([`/api/v1/repos/${REPO_ID}`]);
    expect(container.textContent).toContain('clone in progress — retry with force');
    const force = input('force');
    expect(routerHistory.get()).toBe(DANGER_PATH); // still here

    // Escalate deliberately: check force, retry, and the query string rides.
    toggleCheckbox(force, true);
    h.deleteStatus = 204;
    button('Delete repository').click();
    await settle();

    expect(h.deleteRequests).toEqual([
      `/api/v1/repos/${REPO_ID}`,
      `/api/v1/repos/${REPO_ID}?force=true`,
    ]);
    expect(routerHistory.get()).toBe('/');
  });
});
