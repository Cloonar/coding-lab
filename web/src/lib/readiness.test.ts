// Readiness wording and fix navigation (issue #61): check titles, the order
// (failing, pending, passing; canonical order within a group), the block's
// headline for each roll-up, and where a fix lives for each scope.

import { describe, expect, it } from 'vitest';
import type { Readiness, ReadinessCheck, ReadinessState } from '../api';
import {
  checkState,
  checksOf,
  checkTitle,
  fixHref,
  fixLabel,
  normalizeReadiness,
  orderChecks,
  plural,
  readinessHeadline,
  readinessState,
} from './readiness';

const check = (over: Partial<ReadinessCheck> & Pick<ReadinessCheck, 'id'>): ReadinessCheck => ({
  state: 'passing',
  detail: `${over.id} detail`,
  ...over,
});

describe('checkTitle', () => {
  it('names the six checks', () => {
    expect(
      (['clone', 'git_credential', 'tracker', 'agent_login', 'dev_image', 'imports'] as const).map(
        checkTitle,
      ),
    ).toEqual(['Clone', 'Git credential', 'Tracker', 'Agent', 'Dev image', 'Imports']);
  });

  it('humanizes an id it does not know', () => {
    expect(checkTitle('disk_space')).toBe('Disk space');
    expect(checkTitle('')).toBe('Check');
  });
});

describe('orderChecks', () => {
  it('puts failing first, then pending, then passing, keeping canonical order within each', () => {
    const checks = [
      check({ id: 'clone', state: 'pending' }),
      check({ id: 'git_credential' }),
      check({ id: 'tracker', state: 'failing' }),
      check({ id: 'agent_login' }),
      check({ id: 'dev_image', state: 'failing' }),
      check({ id: 'imports', state: 'pending' }),
    ];
    expect(orderChecks(checks).map((c) => c.id)).toEqual([
      'tracker',
      'dev_image',
      'clone',
      'imports',
      'git_credential',
      'agent_login',
    ]);
    // Pure: the input keeps its order.
    expect(checks[0]?.id).toBe('clone');
  });
});

describe('a report from an older or newer server', () => {
  const newer = 'degraded' as ReadinessState;

  it('reads an unknown state as pending, never passing', () => {
    expect(checkState('passing')).toBe('passing');
    expect(checkState('failing')).toBe('failing');
    expect(checkState(newer)).toBe('pending');
    expect(checkState(undefined)).toBe('pending');
    expect(
      readinessState({ state: 'passing', checks: [check({ id: 'tracker', state: newer })] }),
    ).toBe('pending');
    expect(readinessState({ state: newer, checks: [] })).toBe('pending');
  });

  it('orders an unknown state with the pending checks', () => {
    const checks = [
      check({ id: 'clone' }),
      check({ id: 'tracker', state: newer }),
      check({ id: 'dev_image', state: 'failing' }),
    ];
    expect(orderChecks(checks).map((c) => c.id)).toEqual(['dev_image', 'tracker', 'clone']);
  });

  it('treats a missing report or check list as no checks', () => {
    expect(checksOf(undefined)).toEqual([]);
    expect(checksOf(null)).toEqual([]);
    expect(checksOf({ state: 'passing' } as unknown as Readiness)).toEqual([]);
    expect(normalizeReadiness(undefined)).toBeUndefined();
    const bare = normalizeReadiness({ state: 'passing' } as unknown as Readiness);
    expect(bare).toEqual({ state: 'passing', checks: [] });
    expect(readinessHeadline(bare!).title).toBe('Ready to run');
    expect(readinessState({ checks: [] } as unknown as Readiness)).toBe('passing');
  });

  it('normalizes every state, passing known checks through untouched', () => {
    const known = check({ id: 'clone' });
    const report = normalizeReadiness({
      state: newer,
      checks: [known, check({ id: 'tracker', state: newer })],
    });
    expect(report?.state).toBe('pending');
    expect(report?.checks[0]).toBe(known);
    expect(report?.checks[1]?.state).toBe('pending');
    expect(readinessHeadline(report!).title).toBe('Getting ready');
  });
});

describe('readinessState', () => {
  it('rolls up failing over pending over passing, the server state included', () => {
    expect(readinessState({ state: 'passing', checks: [check({ id: 'clone' })] })).toBe('passing');
    expect(
      readinessState({ state: 'passing', checks: [check({ id: 'clone', state: 'pending' })] }),
    ).toBe('pending');
    expect(
      readinessState({
        state: 'pending',
        checks: [
          check({ id: 'clone', state: 'pending' }),
          check({ id: 'tracker', state: 'failing' }),
        ],
      }),
    ).toBe('failing');
    expect(readinessState({ state: 'failing', checks: [] })).toBe('failing');
  });
});

describe('readinessHeadline', () => {
  const passing = (n: number): Readiness => ({
    state: 'passing',
    checks: Array.from({ length: n }, () => check({ id: 'clone' })),
  });

  it('reads "Ready to run" with the number of passing checks', () => {
    expect(readinessHeadline(passing(6))).toEqual({
      state: 'passing',
      title: 'Ready to run',
      detail: 'All 6 checks pass.',
    });
    expect(readinessHeadline(passing(1)).detail).toBe('The one check passes.');
  });

  it('still reads "Ready to run" with no checks at all', () => {
    expect(readinessHeadline(passing(0))).toEqual({
      state: 'passing',
      title: 'Ready to run',
      detail: 'Nothing blocks a new run.',
    });
  });

  it('counts the problems of a failing report', () => {
    const one: Readiness = {
      state: 'failing',
      checks: [check({ id: 'tracker', state: 'failing' }), check({ id: 'clone' })],
    };
    expect(readinessHeadline(one)).toEqual({
      state: 'failing',
      title: 'Not ready, 1 problem',
      detail: 'New runs are refused until this is fixed.',
    });
    const two: Readiness = {
      state: 'failing',
      checks: [
        check({ id: 'tracker', state: 'failing' }),
        check({ id: 'imports', state: 'failing' }),
      ],
    };
    expect(readinessHeadline(two).title).toBe('Not ready, 2 problems');
    expect(readinessHeadline(two).detail).toBe('New runs are refused until these are fixed.');
  });

  it('says runs can start when the clone has finished while it runs', () => {
    const cloning: Readiness = {
      state: 'pending',
      checks: [check({ id: 'clone', state: 'pending' }), check({ id: 'tracker' })],
    };
    expect(readinessHeadline(cloning)).toEqual({
      state: 'pending',
      title: 'Getting ready',
      detail: 'Runs can start when the clone has finished.',
    });
    // A cloning repo whose report has not caught up yet still waits for it.
    expect(readinessHeadline(passing(0), true).detail).toBe(
      'Runs can start when the clone has finished.',
    );
    expect(
      readinessHeadline({ state: 'pending', checks: [check({ id: 'imports', state: 'pending' })] })
        .detail,
    ).toBe('Runs can start when the pending checks have finished.');
  });
});

describe('fixHref', () => {
  it('opens the repo settings field: section in the path, field in the query', () => {
    expect(
      fixHref('repo_1', { scope: 'repo', section: 'integrations', field: 'forge_credential_id' }),
    ).toBe('/repos/repo_1/settings/integrations?field=forge_credential_id');
    expect(fixHref('repo_1', { scope: 'repo', section: 'imports' })).toBe(
      '/repos/repo_1/settings/imports',
    );
    expect(fixHref('repo_1', { scope: 'repo' })).toBe('/repos/repo_1/settings');
  });

  it('opens a global Settings section', () => {
    expect(fixHref('repo_1', { scope: 'global', section: 'runner' })).toBe('/settings/runner');
    expect(fixHref('repo_1', { scope: 'global' })).toBe('/settings');
  });

  it('opens Credentials for an agent login', () => {
    expect(fixHref('repo_1', { scope: 'credentials' })).toBe('/credentials');
  });
});

describe('fixLabel', () => {
  it('names the target', () => {
    expect(fixLabel({ scope: 'repo', section: 'integrations', field: 'credential_id' })).toBe(
      'Change credential',
    );
    expect(fixLabel({ scope: 'repo', section: 'integrations', field: 'forge_credential_id' })).toBe(
      'Change credential',
    );
    expect(fixLabel({ scope: 'repo', section: 'runner', field: 'image_ref' })).toBe(
      'Change dev image',
    );
    expect(fixLabel({ scope: 'repo', section: 'imports' })).toBe('Change imports');
    expect(fixLabel({ scope: 'credentials' })).toBe('Open Credentials');
  });

  it('falls back to a generic label', () => {
    expect(fixLabel({ scope: 'repo', section: 'general', field: 'name' })).toBe('Open settings');
    expect(fixLabel({ scope: 'global', section: 'runner', field: 'dev_image_default' })).toBe(
      'Open global settings',
    );
  });
});

describe('plural', () => {
  it('adds the s from two on', () => {
    expect(plural(1, 'run')).toBe('1 run');
    expect(plural(2, 'run')).toBe('2 runs');
    expect(plural(0, 'run')).toBe('0 runs');
  });
});
