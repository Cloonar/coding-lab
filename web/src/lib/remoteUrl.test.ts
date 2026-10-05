import { describe, expect, it } from 'vitest';
import { parseRemote, remoteUrlProblem } from './remoteUrl';

describe('parseRemote', () => {
  const rows: Array<[url: string, want: ReturnType<typeof parseRemote>]> = [
    ['git@github.com:Cloonar/coding-lab.git', { kind: 'scp', host: 'github.com' }],
    ['github.com:owner/repo', { kind: 'scp', host: 'github.com' }],
    ['https://git.cloonar.com/Cloonar/coding-lab.git', { kind: 'url', host: 'git.cloonar.com' }],
    ['https://user:pw@host.example:8443/o/r', { kind: 'url', host: 'host.example' }],
    [
      'ssh://git@git.cloonar.com:2222/Cloonar/coding-lab.git',
      { kind: 'url', host: 'git.cloonar.com' },
    ],
    ['git://host/o/r.git', { kind: 'url', host: 'host' }],
    ['ssh://git@[::1]:22/o/r.git', { kind: 'url', host: '[::1]' }],
    ['file:///srv/git/repo.git', { kind: 'path', host: null }],
    ['/srv/git/repo.git', { kind: 'path', host: null }],
    ['./repo', { kind: 'path', host: null }],
    ['../repo', { kind: 'path', host: null }],
    ['~/src/repo', { kind: 'path', host: null }],
    ['  git@h:o/r.git  ', { kind: 'scp', host: 'h' }],
    // Unparsable: nothing git could clone.
    ['', null],
    ['   ', null],
    ['coding-lab', null],
    ['github.com/owner/repo', null],
    ['https://github.com', null],
    ['https://github.com/', null],
    ['git@github.com:', null],
    ['git@github.com:/', null],
    ['file://', null],
    ['/', null],
    ['https://host/o/my repo.git', null],
  ];

  for (const [url, want] of rows) {
    it(`${JSON.stringify(url)} → ${JSON.stringify(want)}`, () => {
      expect(parseRemote(url)).toEqual(want);
    });
  }
});

describe('remoteUrlProblem', () => {
  it('asks for a URL when the field is empty', () => {
    expect(remoteUrlProblem('  ')).toBe(
      'Paste the remote URL, for example git@github.com:owner/repo.git',
    );
  });

  it('names the accepted shapes for an unparsable URL', () => {
    expect(remoteUrlProblem('coding-lab')).toMatch(/^This is not a remote lab can clone\./);
  });

  it('accepts every remote shape git clones', () => {
    expect(remoteUrlProblem('git@h:o/r.git')).toBeNull();
    expect(remoteUrlProblem('https://h/o/r')).toBeNull();
    expect(remoteUrlProblem('/srv/git/r.git')).toBeNull();
  });
});
