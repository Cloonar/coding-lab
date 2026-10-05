// SSH-bastion target picker suite (issue #39 / ADR-0068) on
// /repos/:id/settings/secrets, where the picker renders directly below the
// credential-gateway grant picker (SecretGrants.test.tsx's precedent, applied
// to Warpgate SSH targets). Device-local/immediate: no form to submit, no
// Save button, no leave guard — every assertion is either on the exact
// request a toggle issued or on what the section renders after it.
//
// The properties worth pinning here, mirroring SecretGrants.test.tsx: the
// picker is not optimistic (a failed assign leaves the row exactly as the
// server last described it), the "not configured" / error / empty / list
// states are visibly distinct, and a toggle round-trips through the real
// PUT/DELETE endpoints rather than flipping local state.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  container,
  h,
  installRepoSettingsHooks,
  mountSettings,
  settle,
  sshTargetsSection,
  unmount,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountSecrets = () => mountSettings(`/repos/${REPO_ID}/settings/secrets`);

/** A target toggle button by target id (its form name). */
const toggle = (id: string): HTMLButtonElement => {
  const el = container.querySelector<HTMLButtonElement>(`button[name="ssh-target-${id}"]`);
  if (!el) throw new Error(`missing toggle button[name="ssh-target-${id}"]`);
  return el;
};

/** Every target toggle in the picker, in render order. */
const toggles = (): HTMLButtonElement[] =>
  Array.from(sshTargetsSection().querySelectorAll<HTMLButtonElement>('button[aria-pressed]'));

const waitForPicker = () =>
  waitFor(() => (toggles().length > 0 ? true : null), 'SSH targets picker rows');

describe('RepoSettings SSH-targets picker', () => {
  it('renders the title, hint and both targets with name (mono), description and state', async () => {
    await mountSecrets();
    await waitForPicker();

    const section = sshTargetsSection();
    expect(section.querySelector('h2')?.textContent).toBe('SSH targets');
    expect(section.textContent).toContain('ssh <name>');
    expect(section.textContent).toContain('never enters the run');
    expect(section.textContent).toContain('staging');
    expect(section.querySelector('.mono')?.textContent).toBe('staging');
    expect(section.textContent).toContain('Staging box');
    expect(toggle('tgt_1').getAttribute('aria-pressed')).toBe('true');
    expect(toggle('tgt_1').classList.contains('on')).toBe(true);
    expect(toggle('tgt_2').getAttribute('aria-pressed')).toBe('false');
    expect(toggle('tgt_2').classList.contains('on')).toBe(false);
  });

  it('toggling an unassigned target PUTs the assignment and the row then reads as assigned', async () => {
    await mountSecrets();
    await waitForPicker();
    expect(toggle('tgt_2').getAttribute('aria-pressed')).toBe('false');

    toggle('tgt_2').click();
    await settle();

    expect(h.sshTargetRequests).toEqual([`PUT /api/v1/repos/${REPO_ID}/warpgate/targets/tgt_2`]);
    expect(h.sshTargetsOnServer.find((t) => t.id === 'tgt_2')?.assigned).toBe(true);
    // Rendered from the refetched list, not from a local flip.
    expect(toggle('tgt_2').getAttribute('aria-pressed')).toBe('true');
    expect(toggle('tgt_2').classList.contains('on')).toBe(true);
    // The other target is untouched.
    expect(toggle('tgt_1').getAttribute('aria-pressed')).toBe('true');
  });

  it('toggling an assigned target DELETEs the assignment and the row then reads as unassigned', async () => {
    await mountSecrets();
    await waitForPicker();
    expect(toggle('tgt_1').getAttribute('aria-pressed')).toBe('true');

    toggle('tgt_1').click();
    await settle();

    expect(h.sshTargetRequests).toEqual([`DELETE /api/v1/repos/${REPO_ID}/warpgate/targets/tgt_1`]);
    expect(h.sshTargetsOnServer.find((t) => t.id === 'tgt_1')?.assigned).toBe(false);
    expect(toggle('tgt_1').getAttribute('aria-pressed')).toBe('false');
    expect(toggle('tgt_1').classList.contains('on')).toBe(false);
  });

  it('a failed assign surfaces the server message and leaves the row unchanged', async () => {
    h.sshTargetWriteError = 'warpgate: assign refused — the target was deleted';
    await mountSecrets();
    await waitForPicker();

    toggle('tgt_2').click();
    await settle();

    // The call went out, the server refused, and NOTHING flipped: this is the
    // proof the picker renders server truth rather than an optimistic guess.
    expect(h.sshTargetRequests).toEqual([`PUT /api/v1/repos/${REPO_ID}/warpgate/targets/tgt_2`]);
    expect(sshTargetsSection().textContent).toContain(
      'warpgate: assign refused — the target was deleted',
    );
    expect(toggle('tgt_2').getAttribute('aria-pressed')).toBe('false');
    expect(h.sshTargetsOnServer.find((t) => t.id === 'tgt_2')?.assigned).toBe(false);
  });

  it('an unconfigured integration names the setup flags and offers no toggles', async () => {
    h.sshTargetsConfigured = false;
    h.sshTargetsOnServer = [];
    await mountSecrets();
    await waitFor(
      () => (sshTargetsSection().textContent?.includes('Not configured') ? true : null),
      'unconfigured copy',
    );

    const section = sshTargetsSection();
    expect(section.textContent).toContain(
      'Not configured — the SSH bastion integration is off in this lab',
    );
    expect(section.textContent).toContain('--warpgate-url');
    expect(section.textContent).toContain('--warpgate-admin-token-file');
    // Normal and healthy, never an error — and nothing to toggle.
    expect(section.querySelector('.banner.error')).toBeNull();
    expect(toggles()).toHaveLength(0);
  });

  it('a reachable but empty target list points at Warpgate’s own admin UI', async () => {
    h.sshTargetsOnServer = [];
    await mountSecrets();
    await waitFor(() => sshTargetsSection().querySelector('.empty'), 'empty-targets state');

    expect(sshTargetsSection().querySelector('.empty')?.textContent).toContain(
      "defined by the operator in Warpgate's own admin UI",
    );
    expect(sshTargetsSection().querySelector('.banner.error')).toBeNull();
    expect(toggles()).toHaveLength(0);
  });

  it('a failed read shows an error banner with a Retry that refetches and succeeds', async () => {
    h.sshTargetsReadError = 'warpgate: list targets: dial tcp 127.0.0.1:8888: connection refused';
    await mountSecrets();
    await waitFor(
      () => sshTargetsSection().querySelector('.banner.error'),
      'ssh targets read error banner',
    );

    expect(sshTargetsSection().textContent).toContain(
      'warpgate: list targets: dial tcp 127.0.0.1:8888: connection refused',
    );
    expect(sshTargetsSection().textContent).not.toContain('Loading SSH targets');
    expect(toggles()).toHaveLength(0);

    const retry = Array.from(sshTargetsSection().querySelectorAll('button')).find(
      (b) => b.textContent?.trim() === 'Retry',
    );
    expect(retry).toBeDefined();

    h.sshTargetsReadError = null;
    retry?.click();
    await settle();

    expect(sshTargetsSection().querySelector('.banner.error')).toBeNull();
    expect(toggles()).toHaveLength(2);
    expect(sshTargetsSection().textContent).toContain('staging');
  });

  it('reads the target list back from the server on a remount — nothing is cached lab-side', async () => {
    await mountSecrets();
    await waitForPicker();

    toggle('tgt_2').click();
    await settle();
    expect(toggle('tgt_2').getAttribute('aria-pressed')).toBe('true');

    unmount();
    await mountSecrets();
    await waitForPicker();

    expect(toggle('tgt_2').getAttribute('aria-pressed')).toBe('true');
    expect(toggle('tgt_1').getAttribute('aria-pressed')).toBe('true');
    expect(h.sshTargetRequests).toEqual([`PUT /api/v1/repos/${REPO_ID}/warpgate/targets/tgt_2`]);
  });

  it('renders above the credential-gateway grant picker but below nothing else on the page', async () => {
    await mountSecrets();
    await waitForPicker();

    // The cards of the page's Secrets section, in order (the section's own
    // "Secrets" heading above them is not a card heading).
    const headings = Array.from(
      container.querySelectorAll('section#settings-secrets section.card h2'),
    ).map((h2) => h2.textContent);
    expect(headings).toHaveLength(3);
    const grantsIdx = headings.indexOf('Credential gateway');
    const sshIdx = headings.indexOf('SSH targets');
    const secretsIdx = headings.indexOf('Secrets');
    expect(grantsIdx).toBeGreaterThanOrEqual(0);
    expect(sshIdx).toBeGreaterThan(grantsIdx);
    expect(secretsIdx).toBeGreaterThan(sshIdx);
  });
});
