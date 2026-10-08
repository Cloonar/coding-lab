// The New run page's pure rules (issue #66): the recent repositories list and
// its storage key (old and new format), the picker's rows and filter, the
// triage state/filter/counts/age of the Issues card, the three first_message
// shapes and the default label, the AFK line, and the composer's blockers
// (a failing tracker warns but leaves the field enabled).

import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ReadinessCheck, Repo } from '../api';
import { baseRepo } from '../routes/repo-home/harness';
import {
  afkLine,
  attachmentText,
  composeFirstMessage,
  composerBlockers,
  composerPlaceholder,
  defaultRunLabel,
  fieldDisabled,
  filterIssues,
  filterRepos,
  HOST_RUNNER_WARNING,
  isStartable,
  ISSUE_ACTIONS,
  issueAge,
  issueFilterCounts,
  newestIssues,
  parseRecentRepos,
  preselectedRepo,
  pushRecentRepo,
  readinessDot,
  readRecentRepos,
  recentRepos,
  RECENT_REPOS_KEY,
  RECENT_REPOS_STORED_MAX,
  repoHost,
  repoRowDisabled,
  repoRowStatus,
  runLabelFor,
  sendLabel,
  suggestedAction,
  triageState,
  triageTint,
  writeRecentRepos,
} from './newRun';

const repo = (id: string, over: Partial<Repo> = {}): Repo =>
  baseRepo({ id, name: id, remote_url: `git@github.com:example/${id}.git`, ...over });

const withChecks = (r: Repo, checks: ReadinessCheck[]): Repo => ({
  ...r,
  summary: {
    claimable: 1,
    open_issues: 1,
    readiness: {
      state: checks.some((c) => c.state === 'failing')
        ? 'failing'
        : checks.some((c) => c.state === 'pending')
          ? 'pending'
          : 'passing',
      checks,
    },
  },
});

const trackerFailing: ReadinessCheck = {
  id: 'tracker',
  state: 'failing',
  detail: 'The forge token was rejected, so issues cannot be read.',
  fix: { scope: 'repo', section: 'tracker', field: 'forge_credential_id' },
};

describe('parseRecentRepos', () => {
  it('reads the new format, a JSON array of ids', () => {
    expect(parseRecentRepos('["b","a","c"]')).toEqual(['b', 'a', 'c']);
  });

  it('reads the old format, one bare id, as a list of one', () => {
    expect(parseRecentRepos('repo-123')).toEqual(['repo-123']);
    expect(parseRecentRepos('12345')).toEqual(['12345']);
    expect(parseRecentRepos('"quoted"')).toEqual(['quoted']);
  });

  it('reads nothing from null, empty, garbage and non-array JSON', () => {
    expect(parseRecentRepos(null)).toEqual([]);
    expect(parseRecentRepos('')).toEqual([]);
    expect(parseRecentRepos('   ')).toEqual([]);
    expect(parseRecentRepos('[not json')).toEqual([]);
    expect(parseRecentRepos('{"a":1}')).toEqual([]);
    expect(parseRecentRepos('null')).toEqual([]);
    expect(parseRecentRepos('true')).toEqual([]);
  });

  it('drops non-string and empty entries and de-duplicates, keeping the first', () => {
    expect(parseRecentRepos('["a",1,null,"","b","a",{"x":1},"c","b"]')).toEqual(['a', 'b', 'c']);
  });
});

describe('recent repos storage', () => {
  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('round-trips through the lab.last-repo key', () => {
    expect(RECENT_REPOS_KEY).toBe('lab.last-repo');
    expect(readRecentRepos()).toEqual([]);
    writeRecentRepos(['b', 'a']);
    expect(localStorage.getItem('lab.last-repo')).toBe('["b","a"]');
    expect(readRecentRepos()).toEqual(['b', 'a']);
  });

  it("reads what today's page stored (a bare id)", () => {
    localStorage.setItem('lab.last-repo', 'old-id');
    expect(readRecentRepos()).toEqual(['old-id']);
  });

  it('survives a localStorage that throws (private mode)', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied');
    });
    expect(readRecentRepos()).toEqual([]);
    expect(() => writeRecentRepos(['a'])).not.toThrow();
  });
});

describe('pushRecentRepo', () => {
  it('adds a new id to the front', () => {
    expect(pushRecentRepo(['a', 'b'], 'c')).toEqual(['c', 'a', 'b']);
  });

  it('moves an existing id to the front without a duplicate', () => {
    expect(pushRecentRepo(['a', 'b', 'c'], 'c')).toEqual(['c', 'a', 'b']);
  });

  it('caps the list at the stored maximum, dropping the oldest', () => {
    const ids = Array.from({ length: RECENT_REPOS_STORED_MAX }, (_, i) => `r${i}`);
    const next = pushRecentRepo(ids, 'new');
    expect(next).toHaveLength(RECENT_REPOS_STORED_MAX);
    expect(next[0]).toBe('new');
    expect(next).not.toContain(`r${RECENT_REPOS_STORED_MAX - 1}`);
  });

  it('does not mutate its input', () => {
    const ids = ['a', 'b'];
    pushRecentRepo(ids, 'b');
    expect(ids).toEqual(['a', 'b']);
  });
});

describe('recentRepos / preselectedRepo / isStartable', () => {
  const repos = [
    repo('a'),
    repo('b'),
    repo('c', { clone_status: 'cloning' }),
    repo('d', { clone_status: 'error' }),
    repo('e'),
    repo('f'),
    repo('g'),
  ];

  it('lists the stored repos in stored order', () => {
    expect(recentRepos(repos, ['e', 'a', 'b']).map((r) => r.id)).toEqual(['e', 'a', 'b']);
  });

  it('skips ids that are gone and repos that are not startable', () => {
    expect(recentRepos(repos, ['gone', 'c', 'd', 'b', 'a']).map((r) => r.id)).toEqual(['b', 'a']);
  });

  it('caps at four by default and at max when given', () => {
    const ids = ['a', 'b', 'e', 'f', 'g'];
    expect(recentRepos(repos, ids).map((r) => r.id)).toEqual(['a', 'b', 'e', 'f']);
    expect(recentRepos(repos, ids, 2).map((r) => r.id)).toEqual(['a', 'b']);
  });

  it('falls back to the first startable repos in list order on a first visit', () => {
    expect(recentRepos(repos, []).map((r) => r.id)).toEqual(['a', 'b', 'e', 'f']);
  });

  it('falls back when nothing stored is usable, and does not pad a partly usable list', () => {
    expect(recentRepos(repos, ['gone', 'c']).map((r) => r.id)).toEqual(['a', 'b', 'e', 'f']);
    expect(recentRepos(repos, ['e']).map((r) => r.id)).toEqual(['e']);
  });

  it('is empty when no repo is startable', () => {
    expect(recentRepos([repo('c', { clone_status: 'cloning' })], ['c'])).toEqual([]);
    expect(recentRepos([], ['a'])).toEqual([]);
  });

  it('does not repeat a repo that is stored twice', () => {
    expect(recentRepos(repos, ['a', 'a', 'b']).map((r) => r.id)).toEqual(['a', 'b']);
  });

  it('preselects the most recent usable repo', () => {
    expect(preselectedRepo(repos, ['b', 'a'])?.id).toBe('b');
  });

  it('preselects past a stored id that is gone or not ready, else the first startable', () => {
    expect(preselectedRepo(repos, ['gone', 'c', 'e'])?.id).toBe('e');
    expect(preselectedRepo(repos, ['gone'])?.id).toBe('a');
    expect(preselectedRepo(repos, [])?.id).toBe('a');
  });

  it('still selects a repo when none is startable, so its blocker can show', () => {
    const cloning = repo('c', { clone_status: 'cloning' });
    const failed = repo('f', { clone_status: 'error' });
    expect(preselectedRepo([cloning], [])).toBe(cloning);
    expect(preselectedRepo([cloning, failed], ['f'])).toBe(failed);
    expect(preselectedRepo([cloning, failed], ['gone'])).toBe(cloning);
    expect(preselectedRepo([], ['a'])).toBeNull();
  });

  it('prefers any startable repo over a more recent non-startable one', () => {
    const ready = repo('r');
    expect(preselectedRepo([repo('c', { clone_status: 'cloning' }), ready], ['c'])).toBe(ready);
  });

  it('calls only a finished clone startable', () => {
    expect(isStartable(repo('a'))).toBe(true);
    expect(isStartable(repo('a', { clone_status: 'cloning' }))).toBe(false);
    expect(isStartable(repo('a', { clone_status: 'error' }))).toBe(false);
  });
});

describe('repo rows', () => {
  it('shows the remote as host and path, and nothing for an unparseable remote', () => {
    expect(repoHost(repo('a', { remote_url: 'git@github.com:Cloonar/coding-lab.git' }))).toBe(
      'github.com/Cloonar/coding-lab',
    );
    expect(repoHost(repo('a', { remote_url: 'https://git.example.com/acme/auth.git' }))).toBe(
      'git.example.com/acme/auth',
    );
    expect(repoHost(repo('a', { remote_url: 'not a remote' }))).toBe('');
    expect(repoHost(repo('a', { remote_url: '' }))).toBe('');
  });

  it('maps the readiness roll-up to the dot', () => {
    const pass = withChecks(repo('a'), [{ id: 'clone', state: 'passing', detail: 'ok' }]);
    const fail = withChecks(repo('a'), [trackerFailing]);
    const pend = withChecks(repo('a'), [{ id: 'clone', state: 'pending', detail: '…' }]);
    expect(readinessDot(pass)).toBe('ok');
    expect(readinessDot(fail)).toBe('err');
    expect(readinessDot(pend)).toBe('pending');
  });

  it('draws a hollow dot for a repo whose clone has not finished', () => {
    const cloning = withChecks(repo('a', { clone_status: 'error' }), [
      { id: 'clone', state: 'failing', detail: 'x' },
    ]);
    expect(readinessDot(cloning)).toBe('pending');
  });

  it('draws a hollow dot when the repo has no readiness report', () => {
    const bare = { ...repo('a'), summary: undefined } as unknown as Repo;
    expect(readinessDot(bare)).toBe('pending');
  });

  it('words the status of a non-ready row, with the live percent', () => {
    expect(repoRowStatus(repo('a', { clone_status: 'error' }), null)).toBe('clone failed');
    expect(repoRowStatus(repo('a', { clone_status: 'cloning' }), null)).toBe('cloning…');
    expect(
      repoRowStatus(repo('a', { clone_status: 'cloning' }), {
        phase: 'Receiving objects',
        percent: 62,
        line: '',
      }),
    ).toBe('cloning 62%');
    expect(
      repoRowStatus(repo('a', { clone_status: 'cloning' }), {
        phase: 'Resolving',
        percent: null,
        line: '',
      }),
    ).toBe('cloning…');
  });

  it('says "tracker failing" on a ready repo whose tracker check fails, nothing otherwise', () => {
    expect(repoRowStatus(withChecks(repo('a'), [trackerFailing]), null)).toBe('tracker failing');
    expect(repoRowStatus(repo('a'), null)).toBeUndefined();
    const otherFailing = withChecks(repo('a'), [
      { id: 'dev_image', state: 'failing', detail: 'x' },
    ]);
    expect(repoRowStatus(otherFailing, null)).toBeUndefined();
  });

  it('disables cloning and failed rows, but keeps a tracker-failing repo pickable', () => {
    expect(repoRowDisabled(repo('a'))).toBe(false);
    expect(repoRowDisabled(repo('a', { clone_status: 'cloning' }))).toBe(true);
    expect(repoRowDisabled(repo('a', { clone_status: 'error' }))).toBe(true);
    expect(repoRowDisabled(withChecks(repo('a'), [trackerFailing]))).toBe(false);
  });
});

describe('filterRepos', () => {
  const repos = [
    repo('coding-lab', { remote_url: 'git@github.com:Cloonar/coding-lab.git' }),
    repo('cloonar-nixos', { remote_url: 'https://git.example.com/cloonar/nixos.git' }),
    repo('website', { remote_url: 'ssh://git@github.com:22/example/website.git' }),
  ];

  it('keeps every repo for an empty or blank query, as a copy', () => {
    const all = filterRepos(repos, '');
    expect(all).toEqual(repos);
    expect(all).not.toBe(repos);
    expect(filterRepos(repos, '   ')).toHaveLength(3);
  });

  it('matches the name, case-insensitively', () => {
    expect(filterRepos(repos, 'CODING').map((r) => r.id)).toEqual(['coding-lab']);
  });

  it('matches the host', () => {
    expect(filterRepos(repos, 'git.example.com').map((r) => r.id)).toEqual(['cloonar-nixos']);
    expect(filterRepos(repos, 'github.com').map((r) => r.id)).toEqual(['coding-lab', 'website']);
  });

  it('matches nothing for an unknown query', () => {
    expect(filterRepos(repos, 'zzz')).toEqual([]);
  });
});

describe('triage state', () => {
  it('follows the precedence ready-for-agent, needs-triage, needs-info, unlabeled', () => {
    expect(triageState(['needs-triage', 'ready-for-agent'])).toBe('ready-for-agent');
    expect(triageState(['needs-info', 'needs-triage'])).toBe('needs-triage');
    expect(triageState(['bug', 'needs-info'])).toBe('needs-info');
    expect(triageState(['bug', 'enhancement'])).toBe('unlabeled');
    expect(triageState([])).toBe('unlabeled');
  });

  it('tints ready green, needs-triage amber, needs-info idle and unlabeled nothing', () => {
    expect(triageTint('ready-for-agent')).toBe('run');
    expect(triageTint('needs-triage')).toBe('notice');
    expect(triageTint('needs-info')).toBe('idle');
    expect(triageTint('unlabeled')).toBeNull();
  });
});

describe('filterIssues / issueFilterCounts', () => {
  const issues = [
    { number: 47, title: 'Warpgate: dashboard exposure', labels: ['needs-triage', 'enhancement'] },
    { number: 56, title: 'Record the Runner a run was spawned with', labels: ['ready-for-agent'] },
    { number: 470, title: 'Chat header context meter', labels: ['needs-info'] },
    { number: 4, title: 'Fix the OG image for docs pages', labels: ['bug'] },
    { number: 12, title: 'Export the rollup as parquet', labels: [] },
  ];
  const nums = (list: { number: number }[]) => list.map((i) => i.number);

  it('keeps every issue for an empty query and the All chip', () => {
    expect(nums(filterIssues(issues, '', 'all'))).toEqual([47, 56, 470, 4, 12]);
    expect(nums(filterIssues(issues, '  ', 'all'))).toEqual([47, 56, 470, 4, 12]);
  });

  it('matches a number as a prefix, with or without #', () => {
    expect(nums(filterIssues(issues, '47', 'all'))).toEqual([47, 470]);
    expect(nums(filterIssues(issues, '#47', 'all'))).toEqual([47, 470]);
    expect(nums(filterIssues(issues, '4', 'all'))).toEqual([47, 470, 4]);
    expect(nums(filterIssues(issues, '7', 'all'))).toEqual([]);
  });

  it('matches words from the title, case-insensitively and in any order', () => {
    expect(nums(filterIssues(issues, 'WARPGATE', 'all'))).toEqual([47]);
    expect(nums(filterIssues(issues, 'runner spawned', 'all'))).toEqual([56]);
    expect(nums(filterIssues(issues, 'spawned runner', 'all'))).toEqual([56]);
    expect(nums(filterIssues(issues, 'the', 'all'))).toEqual([56, 4, 12]);
    expect(nums(filterIssues(issues, 'runner nope', 'all'))).toEqual([]);
  });

  it('does not look for a digit-only query in the titles', () => {
    expect(
      nums(filterIssues([{ number: 9, title: 'OAuth 2 support', labels: [] }], '2', 'all')),
    ).toEqual([]);
  });

  it('narrows by state', () => {
    expect(nums(filterIssues(issues, '', 'needs-triage'))).toEqual([47]);
    expect(nums(filterIssues(issues, '', 'ready-for-agent'))).toEqual([56]);
    expect(nums(filterIssues(issues, '', 'needs-info'))).toEqual([470]);
    expect(nums(filterIssues(issues, '', 'unlabeled'))).toEqual([4, 12]);
  });

  it('combines the query with the state', () => {
    expect(nums(filterIssues(issues, '47', 'needs-info'))).toEqual([470]);
    expect(nums(filterIssues(issues, 'the', 'unlabeled'))).toEqual([4, 12]);
    expect(nums(filterIssues(issues, 'warpgate', 'needs-info'))).toEqual([]);
  });

  it('does not mutate its input', () => {
    const copy = [...issues];
    filterIssues(issues, '47', 'all');
    expect(issues).toEqual(copy);
  });

  it('counts each chip over the whole list', () => {
    expect(issueFilterCounts(issues)).toEqual({
      all: 5,
      'needs-triage': 1,
      'ready-for-agent': 1,
      'needs-info': 1,
      unlabeled: 2,
    });
    expect(issueFilterCounts([])).toEqual({
      all: 0,
      'needs-triage': 0,
      'ready-for-agent': 0,
      'needs-info': 0,
      unlabeled: 0,
    });
  });

  it('counts only the matches of a query when one is given', () => {
    expect(issueFilterCounts(issues, '47')).toEqual({
      all: 2,
      'needs-triage': 1,
      'ready-for-agent': 0,
      'needs-info': 1,
      unlabeled: 0,
    });
  });
});

describe('newestIssues', () => {
  const issue = (number: number, created_at: string) => ({ number, created_at });

  it('lists the newest first, at most four by default', () => {
    const issues = [
      issue(1, '2026-01-01T00:00:00Z'),
      issue(5, '2026-05-01T00:00:00Z'),
      issue(3, '2026-03-01T00:00:00Z'),
      issue(2, '2026-02-01T00:00:00Z'),
      issue(4, '2026-04-01T00:00:00Z'),
    ];
    expect(newestIssues(issues).map((i) => i.number)).toEqual([5, 4, 3, 2]);
    expect(newestIssues(issues, 2).map((i) => i.number)).toEqual([5, 4]);
    expect(issues[0]?.number).toBe(1); // pure
  });

  it('breaks a tie by the higher number and sorts an unparseable time last', () => {
    const issues = [
      issue(7, '2026-05-01T00:00:00Z'),
      issue(9, '2026-05-01T00:00:00Z'),
      issue(99, 'garbage'),
    ];
    expect(newestIssues(issues).map((i) => i.number)).toEqual([9, 7, 99]);
  });

  it('returns fewer than max when there are fewer', () => {
    expect(newestIssues([issue(1, '2026-01-01T00:00:00Z')])).toHaveLength(1);
    expect(newestIssues([])).toEqual([]);
  });
});

describe('issueAge', () => {
  const now = Date.parse('2026-10-07T12:00:00Z');
  const ago = (ms: number) => new Date(now - ms).toISOString();
  const MIN = 60_000;
  const HOUR = 60 * MIN;
  const DAY = 24 * HOUR;

  it('reads minutes and hours for a very recent issue', () => {
    expect(issueAge(ago(10_000), now)).toBe('now');
    expect(issueAge(ago(5 * MIN), now)).toBe('5 min');
    expect(issueAge(ago(3 * HOUR), now)).toBe('3 h');
  });

  it('reads days up to two weeks, then weeks, months and years', () => {
    expect(issueAge(ago(25 * HOUR), now)).toBe('1 d');
    expect(issueAge(ago(3 * DAY), now)).toBe('3 d');
    expect(issueAge(ago(8 * DAY), now)).toBe('8 d');
    expect(issueAge(ago(13 * DAY), now)).toBe('13 d');
    expect(issueAge(ago(14 * DAY), now)).toBe('2 wk');
    expect(issueAge(ago(49 * DAY), now)).toBe('7 wk');
    expect(issueAge(ago(60 * DAY), now)).toBe('2 mo');
    expect(issueAge(ago(364 * DAY), now)).toBe('12 mo');
    expect(issueAge(ago(365 * DAY), now)).toBe('1 y');
    expect(issueAge(ago(800 * DAY), now)).toBe('2 y');
  });

  it('reads now for a time ahead of the clock and nothing for garbage', () => {
    expect(issueAge(new Date(now + 10 * MIN).toISOString(), now)).toBe('now');
    expect(issueAge('garbage', now)).toBe('');
    expect(issueAge('', now)).toBe('');
  });
});

describe('issue actions', () => {
  it('lists Triage, Implement, Discuss with the mockup copy', () => {
    expect(ISSUE_ACTIONS.map((a) => [a.id, a.label])).toEqual([
      ['triage', 'Triage'],
      ['implement', 'Implement'],
      ['discuss', 'Discuss'],
    ]);
    const [triage, implement, discuss] = ISSUE_ACTIONS;
    expect(triage?.describe(47)).toBe(
      'Runs /triage #47: the agent reads it, asks you what is missing and sets the label.',
    );
    expect(implement?.describe(47)).toBe('A run with #47 as its brief, on a branch of its own.');
    expect(discuss?.describe(47)).toBe(
      'Opens a chat with #47 as context. Nothing happens until you type.',
    );
  });

  it('suggests triage, implement or discuss by the label', () => {
    expect(suggestedAction(['needs-triage', 'bug'])).toBe('triage');
    expect(suggestedAction(['ready-for-agent'])).toBe('implement');
    expect(suggestedAction(['ready-for-agent', 'needs-triage'])).toBe('implement');
    expect(suggestedAction(['needs-info'])).toBe('discuss');
    expect(suggestedAction(['bug'])).toBe('discuss');
    expect(suggestedAction([])).toBe('discuss');
  });
});

describe('composeFirstMessage', () => {
  const issue = { number: 47, title: 'Warpgate: dashboard exposure' };

  it('triage: /triage #n, then the typed text on its own line', () => {
    expect(composeFirstMessage('triage', issue, '')).toBe('/triage #47');
    expect(composeFirstMessage('triage', issue, 'it is about the ssh bastion')).toBe(
      '/triage #47\nit is about the ssh bastion',
    );
  });

  it('implement: the brief sentence, then the typed text', () => {
    const head =
      'Implement issue #47 "Warpgate: dashboard exposure". Read it with `labctl issue view 47` first; it is your brief.';
    expect(composeFirstMessage('implement', issue, '')).toBe(head);
    expect(composeFirstMessage('implement', issue, 'keep it small')).toBe(`${head}\nkeep it small`);
  });

  it('discuss: the discussion sentence, then the typed text', () => {
    const head =
      'Let\'s discuss issue #47 "Warpgate: dashboard exposure". Read it with `labctl issue view 47`, then wait for my questions.';
    expect(composeFirstMessage('discuss', issue, '')).toBe(head);
    expect(composeFirstMessage('discuss', issue, 'why two ports?')).toBe(`${head}\nwhy two ports?`);
  });

  it('trims the typed text and appends nothing for whitespace', () => {
    expect(composeFirstMessage('triage', issue, '  \n ')).toBe('/triage #47');
    expect(composeFirstMessage('triage', issue, '\n  hello  \n')).toBe('/triage #47\nhello');
  });

  it('keeps line breaks inside the typed text', () => {
    expect(composeFirstMessage('triage', issue, 'a\nb')).toBe('/triage #47\na\nb');
  });
});

describe('labels and copy of an attached action', () => {
  const triage = { action: 'triage', number: 47 } as const;
  const discuss = { action: 'discuss', number: 47 } as const;
  const implement = { action: 'implement', number: 56 } as const;

  it('defaults the run label to <action>-<number>', () => {
    expect(defaultRunLabel('triage', 47)).toBe('triage-47');
    expect(defaultRunLabel('implement', 56)).toBe('implement-56');
    expect(defaultRunLabel('discuss', 3)).toBe('discuss-3');
  });

  it('lets a typed label win over the default, and defaults only when attached', () => {
    expect(runLabelFor('', triage)).toBe('triage-47');
    expect(runLabelFor('   ', triage)).toBe('triage-47');
    expect(runLabelFor('  mine ', triage)).toBe('mine');
    expect(runLabelFor('mine', null)).toBe('mine');
    expect(runLabelFor('', null)).toBe('');
  });

  it('words the attachment chip', () => {
    expect(attachmentText('triage', { number: 47, title: 'Warpgate: dashboard' })).toBe(
      'Triage #47 · Warpgate: dashboard',
    );
    expect(attachmentText('implement', { number: 56, title: 'Record the Runner' })).toBe(
      'Implement #56 · Record the Runner',
    );
    expect(attachmentText('discuss', { number: 1, title: 'x' })).toBe('Discuss #1 · x');
  });

  it('relabels Send', () => {
    expect(sendLabel(null)).toBe('Start run');
    expect(sendLabel(triage)).toBe('Start: Triage #47');
    expect(sendLabel(implement)).toBe('Start: Implement #56');
  });

  it('words the placeholder', () => {
    expect(composerPlaceholder(null, 'coding-lab')).toBe('Describe a task for coding-lab…');
    expect(composerPlaceholder(triage, 'coding-lab')).toBe(
      'Anything the agent should know? (optional)',
    );
    expect(composerPlaceholder(implement, 'coding-lab')).toBe(
      'Anything the agent should know? (optional)',
    );
    expect(composerPlaceholder(discuss, 'coding-lab')).toBe(
      'Say what you want to discuss about #47…',
    );
  });
});

describe('afkLine', () => {
  it('Auto on: ready, live runs and the next-claim note, no button', () => {
    expect(afkLine({ auto: true, paused: false, ready: 3, liveAFK: 2 })).toEqual({
      text: 'Auto on · 3 ready · 2 AFK runs live · next claim when a slot frees',
      action: null,
    });
  });

  it('says "1 AFK run live" in the singular and leaves the segment out at 0', () => {
    expect(afkLine({ auto: true, paused: false, ready: 3, liveAFK: 1 }).text).toBe(
      'Auto on · 3 ready · 1 AFK run live · next claim when a slot frees',
    );
    expect(afkLine({ auto: true, paused: false, ready: 3, liveAFK: 0 }).text).toBe(
      'Auto on · 3 ready · next claim when a slot frees',
    );
  });

  it('leaves out the count when the ready count is unknown', () => {
    expect(afkLine({ auto: true, paused: false, ready: null, liveAFK: 0 }).text).toBe(
      'Auto on · next claim when a slot frees',
    );
    expect(afkLine({ auto: false, paused: false, ready: null, liveAFK: 0 }).text).toBe('Auto off');
  });

  it('Auto off: no next-claim note, and a Run one button', () => {
    expect(afkLine({ auto: false, paused: false, ready: 4, liveAFK: 0 })).toEqual({
      text: 'Auto off · 4 ready',
      action: 'run-one',
    });
    expect(afkLine({ auto: false, paused: false, ready: 4, liveAFK: 2 })).toEqual({
      text: 'Auto off · 4 ready · 2 AFK runs live',
      action: 'run-one',
    });
  });

  it('paused: names the threshold, wins over Auto, and offers Reset', () => {
    expect(afkLine({ auto: true, paused: true, ready: 5, liveAFK: 1 })).toEqual({
      text: 'AFK paused after 3 failed runs · 5 ready',
      action: 'reset',
    });
    expect(afkLine({ auto: false, paused: true, ready: null, liveAFK: 0 })).toEqual({
      text: 'AFK paused after 3 failed runs',
      action: 'reset',
    });
  });

  it('keeps zero ready visible', () => {
    expect(afkLine({ auto: false, paused: false, ready: 0, liveAFK: 0 }).text).toBe(
      'Auto off · 0 ready',
    );
  });
});

describe('composerBlockers', () => {
  const none = { progress: null, loggedOut: false, providerName: 'Claude' };

  it('shows nothing for a repo that can run, or for no repo', () => {
    expect(composerBlockers({ ...none, repo: repo('a') })).toEqual([]);
    expect(composerBlockers({ ...none, repo: null })).toEqual([]);
    const passing = withChecks(repo('a'), [{ id: 'tracker', state: 'passing', detail: 'ok' }]);
    expect(composerBlockers({ ...none, repo: passing })).toEqual([]);
  });

  it('cloning: a notice with the live percent that disables the field', () => {
    const blockers = composerBlockers({
      ...none,
      repo: repo('a', { clone_status: 'cloning' }),
      progress: { phase: 'Receiving objects', percent: 62, line: '' },
    });
    expect(blockers).toEqual([
      {
        kind: 'cloning',
        variant: 'notice',
        message: 'Cloning 62%. Runs can start when the clone finishes.',
        disablesField: true,
      },
    ]);
    expect(fieldDisabled(blockers)).toBe(true);
  });

  it('cloning without a percent reads "Cloning…"', () => {
    const [blocker] = composerBlockers({ ...none, repo: repo('a', { clone_status: 'cloning' }) });
    expect(blocker?.message).toBe('Cloning… Runs can start when the clone finishes.');
  });

  it('clone failed: an error with the clone error and a retry, field disabled', () => {
    const blockers = composerBlockers({
      ...none,
      repo: repo('a', { clone_status: 'error', clone_error: 'remote rejected the credential' }),
    });
    expect(blockers).toEqual([
      {
        kind: 'clone-failed',
        variant: 'error',
        message: 'remote rejected the credential',
        disablesField: true,
        retryClone: true,
      },
    ]);
    expect(fieldDisabled(blockers)).toBe(true);
  });

  it('clone failed without an error text falls back to a plain sentence', () => {
    const [blocker] = composerBlockers({
      ...none,
      repo: repo('a', { clone_status: 'error', clone_error: null }),
    });
    expect(blocker?.message).toBe('The clone failed.');
    expect(blocker?.retryClone).toBe(true);
  });

  it('logged out: an error naming the agent with Reconnect → /credentials, field disabled', () => {
    const blockers = composerBlockers({
      ...none,
      repo: repo('a'),
      loggedOut: true,
      providerName: 'Codex',
    });
    expect(blockers).toEqual([
      {
        kind: 'logged-out',
        variant: 'error',
        message: 'Codex is logged out.',
        disablesField: true,
        fixHref: '/credentials',
        fixLabel: 'Reconnect',
      },
    ]);
    expect(fieldDisabled(blockers)).toBe(true);
  });

  it('logged out still applies before a repo is selected', () => {
    const blockers = composerBlockers({ ...none, repo: null, loggedOut: true });
    expect(blockers.map((b) => b.kind)).toEqual(['logged-out']);
  });

  it('tracker failing: a warning with the detail and the Fix link, field stays enabled', () => {
    const blockers = composerBlockers({
      ...none,
      repo: withChecks(repo('r1'), [trackerFailing]),
    });
    expect(blockers).toEqual([
      {
        kind: 'tracker',
        variant: 'warning',
        message: 'The forge token was rejected, so issues cannot be read. A run can still start.',
        disablesField: false,
        fixHref: '/repos/r1/settings/tracker?field=forge_credential_id',
        fixLabel: 'Fix',
      },
    ]);
    expect(fieldDisabled(blockers)).toBe(false);
  });

  it('tracker failing without a named fix offers no Fix link', () => {
    const [blocker] = composerBlockers({
      ...none,
      repo: withChecks(repo('a'), [{ id: 'tracker', state: 'failing', detail: 'unreachable' }]),
    });
    expect(blocker?.kind).toBe('tracker');
    expect(blocker?.fixHref).toBeUndefined();
    expect(blocker?.fixLabel).toBeUndefined();
  });

  it('a pending tracker check shows nothing', () => {
    const pending = withChecks(repo('a'), [{ id: 'tracker', state: 'pending', detail: '…' }]);
    expect(composerBlockers({ ...none, repo: pending })).toEqual([]);
  });

  it('a failing check other than the tracker shows nothing here', () => {
    const other = withChecks(repo('a'), [{ id: 'dev_image', state: 'failing', detail: 'x' }]);
    expect(composerBlockers({ ...none, repo: other })).toEqual([]);
  });

  it('orders the most severe first: errors, then the warning, then notices', () => {
    const blockers = composerBlockers({
      ...none,
      repo: withChecks(repo('a'), [trackerFailing]),
      loggedOut: true,
    });
    expect(blockers.map((b) => b.kind)).toEqual(['logged-out', 'tracker']);
    expect(fieldDisabled(blockers)).toBe(true);

    const cloning = composerBlockers({
      ...none,
      repo: repo('a', { clone_status: 'cloning' }),
      loggedOut: true,
    });
    expect(cloning.map((b) => b.kind)).toEqual(['logged-out', 'cloning']);

    const failed = composerBlockers({
      ...none,
      repo: repo('a', { clone_status: 'error' }),
      loggedOut: true,
    });
    expect(failed.map((b) => b.kind)).toEqual(['clone-failed', 'logged-out']);
  });

  it('fieldDisabled is false for no blockers', () => {
    expect(fieldDisabled([])).toBe(false);
  });

  it('exports the host-Runner warning', () => {
    expect(HOST_RUNNER_WARNING).toBe('Runs on the host, unsandboxed, with full host access.');
  });
});
