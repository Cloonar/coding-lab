// The repositories list's pure rules (issue #61): the order (latest run
// first, never-run repos after, newest first), the filter (name or remote,
// case-insensitive), run counts, AFK state, the relative "Last run" time and
// the Needs you entries (one per problem, a failed clone exactly once).

import { describe, expect, it } from 'vitest';
import type { Instance, ReadinessCheck, ReadinessState, Repo } from '../api';
import { baseInstance, baseRepo } from '../routes/repo-home/harness';
import {
  afkState,
  filterRepos,
  isFilterActive,
  isNotReady,
  needsYou,
  orderRepos,
  relativeTime,
  runCounts,
  summaryRefreshSpecs,
  SUMMARY_REFRESH_MS,
} from './repoList';

const repo = (id: string, over: Partial<Repo> = {}): Repo =>
  baseRepo({ id, name: id, remote_url: `git@github.com:example/${id}.git`, ...over });

const withChecks = (r: Repo, checks: ReadinessCheck[], claimable: number | null = 2): Repo => ({
  ...r,
  summary: {
    claimable,
    open_issues: 3,
    readiness: {
      state: checks.some((c) => c.state === 'failing') ? 'failing' : 'passing',
      checks,
    },
  },
});

describe('orderRepos', () => {
  it('puts the latest run first, then never-run repos newest first', () => {
    const repos = [
      repo('old-run', { last_opened_at: '2026-07-01T00:00:00Z' }),
      repo('never-old', { created_at: '2026-01-01T00:00:00Z' }),
      repo('new-run', { last_opened_at: '2026-07-05T00:00:00Z' }),
      repo('never-new', { created_at: '2026-06-01T00:00:00Z' }),
      repo('mid-run', { last_opened_at: '2026-07-03T00:00:00Z' }),
    ];
    expect(orderRepos(repos).map((r) => r.id)).toEqual([
      'new-run',
      'mid-run',
      'old-run',
      'never-new',
      'never-old',
    ]);
    expect(repos[0]?.id).toBe('old-run'); // pure
  });

  it('keeps the input order on ties and treats an unparseable time as never', () => {
    const repos = [
      repo('a', { last_opened_at: '2026-07-01T00:00:00Z' }),
      repo('b', { last_opened_at: '2026-07-01T00:00:00Z' }),
      repo('c', { last_opened_at: 'garbage', created_at: '2026-07-09T00:00:00Z' }),
    ];
    expect(orderRepos(repos).map((r) => r.id)).toEqual(['a', 'b', 'c']);
  });
});

describe('filterRepos', () => {
  const repos = [
    repo('coding-lab', { remote_url: 'git@github.com:Cloonar/coding-lab.git' }),
    repo('cloonar-nixos', { remote_url: 'https://git.example.com/cloonar/nixos.git' }),
    repo('website', { remote_url: 'ssh://git@github.com:22/example/website.git' }),
  ];

  it('matches the name, case-insensitively', () => {
    expect(filterRepos(repos, 'CODING').map((r) => r.id)).toEqual(['coding-lab']);
  });

  it('matches the remote host and path', () => {
    expect(filterRepos(repos, 'git.example.com').map((r) => r.id)).toEqual(['cloonar-nixos']);
    expect(filterRepos(repos, 'example/website').map((r) => r.id)).toEqual(['website']);
    expect(filterRepos(repos, 'github.com').map((r) => r.id)).toEqual(['coding-lab', 'website']);
  });

  it('keeps everything for an empty or blank query, and nothing for a miss', () => {
    expect(filterRepos(repos, '   ')).toHaveLength(3);
    expect(filterRepos(repos, 'zzz')).toEqual([]);
    expect(isFilterActive(' ')).toBe(false);
    expect(isFilterActive('a')).toBe(true);
  });
});

describe('runCounts', () => {
  it('counts the repo’s live instances and those waiting for the operator', () => {
    const instances: Instance[] = [
      baseInstance({ id: 'a', state: 'working' }),
      baseInstance({ id: 'b', state: 'needs_input' }),
      baseInstance({ id: 'c', state: 'question' }),
      baseInstance({ id: 'd', state: 'needs_input', live: false }),
      baseInstance({ id: 'e', state: 'needs_input', repo_id: 'other' }),
    ];
    expect(runCounts(instances, 'repo_1')).toEqual({ live: 3, waiting: 2 });
    expect(runCounts(instances, 'none')).toEqual({ live: 0, waiting: 0 });
  });
});

describe('afkState and isNotReady', () => {
  it('reads paused at three strikes, else the auto flag', () => {
    expect(afkState(repo('a', { consecutive_failures: 3, afk_auto_enabled: true }))).toBe('paused');
    expect(afkState(repo('a', { consecutive_failures: 2, afk_auto_enabled: true }))).toBe('on');
    expect(afkState(repo('a', { afk_auto_enabled: false }))).toBe('off');
  });

  it('is not ready only with a failing check', () => {
    expect(isNotReady(repo('a'))).toBe(false);
    expect(
      isNotReady(withChecks(repo('a'), [{ id: 'tracker', state: 'failing', detail: 'x' }])),
    ).toBe(true);
    expect(
      isNotReady(withChecks(repo('a'), [{ id: 'clone', state: 'pending', detail: 'x' }])),
    ).toBe(false);
  });

  it('tolerates a missing summary or report and a state from a newer server', () => {
    expect(isNotReady({ ...repo('a'), summary: undefined } as unknown as Repo)).toBe(false);
    expect(
      isNotReady({
        ...repo('a'),
        summary: { claimable: 1, open_issues: 1 },
      } as unknown as Repo),
    ).toBe(false);
    expect(
      isNotReady({
        ...repo('a'),
        summary: { claimable: 1, open_issues: 1, readiness: { state: 'passing' } },
      } as unknown as Repo),
    ).toBe(false);
    const newer = withChecks(repo('a'), [
      { id: 'tracker', state: 'degraded' as ReadinessState, detail: 'x' },
    ]);
    expect(isNotReady(newer)).toBe(false); // unknown = pending, not failing
  });
});

describe('relativeTime', () => {
  const now = Date.parse('2026-07-10T12:00:00Z');
  const ago = (ms: number) => new Date(now - ms).toISOString();
  const MIN = 60_000;
  const HOUR = 60 * MIN;
  const DAY = 24 * HOUR;

  const rows: Array<[string, number, string]> = [
    ['30 seconds', 30_000, 'just now'],
    ['4 minutes', 4 * MIN, '4 min ago'],
    ['59 minutes', 59 * MIN, '59 min ago'],
    ['1 hour', HOUR, '1 h ago'],
    ['23 hours', 23 * HOUR, '23 h ago'],
    ['30 hours', 30 * HOUR, 'yesterday'],
    ['2 days', 2 * DAY, '2 days ago'],
    ['45 days', 45 * DAY, '45 days ago'],
    ['90 days', 90 * DAY, '3 months ago'],
    ['400 days', 400 * DAY, '1 year ago'],
    ['800 days', 800 * DAY, '2 years ago'],
  ];
  for (const [what, ms, want] of rows) {
    it(`${what} ago → ${want}`, () => {
      expect(relativeTime(ago(ms), now)).toBe(want);
    });
  }

  it('shows no time for a repo that never had a run, or a bad time', () => {
    expect(relativeTime(null, now)).toBeNull();
    expect(relativeTime('', now)).toBeNull();
    expect(relativeTime('not a time', now)).toBeNull();
  });

  it('reads a time in the future (clock skew) as just now', () => {
    expect(relativeTime(new Date(now + 5 * MIN).toISOString(), now)).toBe('just now');
  });

  it('reads a time further in the future as its plain date, not a wrong relative time', () => {
    const ahead = new Date(now + 2 * HOUR);
    expect(relativeTime(ahead.toISOString(), now)).toBe(ahead.toLocaleDateString());
    const far = new Date(now + 40 * DAY);
    expect(relativeTime(far.toISOString(), now)).toBe(far.toLocaleDateString());
  });
});

describe('needsYou', () => {
  it('lists nothing for healthy repos', () => {
    expect(needsYou([repo('a'), repo('b', { clone_status: 'cloning' })])).toEqual([]);
  });

  it('lists a failed clone ONCE, with the first line of its error', () => {
    const failed = withChecks(
      repo('infra-docs', {
        clone_status: 'error',
        clone_error: 'the remote rejected the git credential.\nfatal: more detail',
      }),
      [
        { id: 'clone', state: 'failing', detail: 'The last clone failed.', action: 'retry_clone' },
        {
          id: 'git_credential',
          state: 'failing',
          detail: 'The git credential was rejected.',
          fix: { scope: 'repo', section: 'integrations', field: 'credential_id' },
        },
      ],
    );
    const entries = needsYou([failed]);
    expect(entries.map((e) => [e.kind, e.message])).toEqual([
      ['clone', 'Clone failed: the remote rejected the git credential.'],
      ['readiness', 'The git credential was rejected.'],
    ]);
  });

  it('words a failed clone without an error message', () => {
    expect(needsYou([repo('a', { clone_status: 'error', clone_error: null })])[0]?.message).toBe(
      'Clone failed.',
    );
  });

  it('lists a three-strikes pause with the waiting count when it is known', () => {
    const paused = withChecks(repo('website', { consecutive_failures: 3 }), [], 5);
    expect(needsYou([paused])).toEqual([
      {
        key: 'paused:website',
        kind: 'paused',
        repo: paused,
        message: 'AFK paused after 3 failed runs. 5 issues waiting.',
      },
    ]);
    const one = withChecks(repo('website', { consecutive_failures: 4 }), [], 1);
    expect(needsYou([one])[0]?.message).toBe('AFK paused after 3 failed runs. 1 issue waiting.');
    const unknown = withChecks(repo('website', { consecutive_failures: 3 }), [], null);
    expect(needsYou([unknown])[0]?.message).toBe('AFK paused after 3 failed runs.');
  });

  it('lists each failing readiness check with the server’s sentence, in list order', () => {
    const tracker: ReadinessCheck = {
      id: 'tracker',
      state: 'failing',
      detail: 'The forge token was rejected. AFK runs cannot read issues.',
      fix: { scope: 'repo', section: 'integrations', field: 'forge_credential_id' },
    };
    const a = withChecks(repo('auth-service'), [
      tracker,
      { id: 'clone', state: 'passing', detail: '' },
    ]);
    const b = withChecks(repo('website', { consecutive_failures: 3 }), [], 0);
    const entries = needsYou([b, a]);
    expect(entries.map((e) => [e.repo.id, e.kind, e.message])).toEqual([
      ['website', 'paused', 'AFK paused after 3 failed runs. No issues waiting.'],
      ['auth-service', 'readiness', 'The forge token was rejected. AFK runs cannot read issues.'],
    ]);
    const entry = entries[1];
    expect(entry?.kind === 'readiness' ? entry.check : undefined).toBe(tracker);
  });

  it('keys each entry by its problem, stable across refetches', () => {
    const make = () => [
      withChecks(repo('infra-docs', { clone_status: 'error' }), []),
      withChecks(repo('auth-service', { consecutive_failures: 3 }), [
        { id: 'tracker', state: 'failing', detail: 'x' },
        { id: 'git_credential', state: 'failing', detail: 'y' },
      ]),
    ];
    const keys = needsYou(make()).map((e) => e.key);
    expect(keys).toEqual([
      'clone:infra-docs',
      'readiness:auth-service:tracker',
      'readiness:auth-service:git_credential',
      'paused:auth-service',
    ]);
    expect(needsYou(make()).map((e) => e.key)).toEqual(keys);
  });

  it('tolerates a repo without a summary or report, and an unknown check state', () => {
    const bare = { ...repo('a', { consecutive_failures: 3 }), summary: undefined };
    expect(needsYou([bare as unknown as Repo]).map((e) => e.message)).toEqual([
      'AFK paused after 3 failed runs.',
    ]);
    const noReport = { ...repo('b'), summary: { claimable: 1, open_issues: 1 } };
    expect(needsYou([noReport as unknown as Repo])).toEqual([]);
    const newer = withChecks(repo('c'), [
      { id: 'tracker', state: 'degraded' as ReadinessState, detail: 'x' },
    ]);
    expect(needsYou([newer])).toEqual([]);
  });
});

describe('summaryRefreshSpecs', () => {
  const ev = (type: string, repoID?: string) =>
    ({ type, ...(repoID === undefined ? {} : { repoID }) }) as Parameters<
      NonNullable<ReturnType<typeof summaryRefreshSpecs>[number]['match']>
    >[0];

  it('re-reads on issue, run, parked and agent login events, debounced', () => {
    const specs = summaryRefreshSpecs();
    expect(specs.map((s) => s.type)).toEqual([
      'issue.changed',
      'run.changed',
      'parked.changed',
      'provider.auth.changed',
    ]);
    expect(specs.every((s) => s.debounceMs === SUMMARY_REFRESH_MS)).toBe(true);
    // The list: every repo's events count.
    expect(specs.every((s) => s.match === undefined || s.match(ev(s.type, 'other')))).toBe(true);
  });

  it('scopes the repo-tagged events to one repo when asked', () => {
    const specs = summaryRefreshSpecs(() => 'mine');
    for (const spec of specs.slice(0, 3)) {
      expect(spec.match?.(ev(spec.type, 'mine'))).toBe(true);
      expect(spec.match?.(ev(spec.type, 'other'))).toBe(false);
      expect(spec.match?.(ev(spec.type))).toBe(true); // no repoID: still counts
    }
    // Agent logins are not per repo.
    expect(specs[3]?.match).toBeUndefined();
  });
});
