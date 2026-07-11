import { describe, expect, it } from 'vitest';
import type { ForgeKind } from '../api';
import { deriveRepoName, forgeWebUrl, remoteHost, sanitizeName } from './repoName';

describe('deriveRepoName (preview of the server derivation)', () => {
  const rows: Array<[url: string, want: string]> = [
    ['https://git.cloonar.com/Cloonar/coding-lab.git', 'coding-lab'],
    ['https://git.cloonar.com/Cloonar/coding-lab', 'coding-lab'],
    ['git@git.cloonar.com:Cloonar/coding-lab.git', 'coding-lab'],
    ['ssh://git@git.cloonar.com:2222/Cloonar/coding-lab.git', 'coding-lab'],
    ['https://github.com/foo/My.Repo.git', 'My_Repo'], // "." is unsafe (tmux)
    ['https://host/owner/repo/', 'repo'], // trailing slash
    ['https://host/owner/repo.GIT', 'repo'], // case-insensitive suffix
    ['git@host:repo.git', 'repo'], // scp-like without owner
    ['  https://host/o/spaced repo.git  ', 'spaced_repo'],
    ['', ''],
    ['   ', ''],
  ];

  for (const [url, want] of rows) {
    it(`${JSON.stringify(url)} → ${JSON.stringify(want)}`, () => {
      expect(deriveRepoName(url)).toBe(want);
    });
  }
});

describe('sanitizeName (v0 scanner rules)', () => {
  it('maps "/" to "-" and anything outside [A-Za-z0-9_-] to "_"', () => {
    expect(sanitizeName('a/b')).toBe('a-b');
    expect(sanitizeName('foo.bar')).toBe('foo_bar');
    expect(sanitizeName('Ok_Name-123')).toBe('Ok_Name-123');
    expect(sanitizeName('sp ace!')).toBe('sp_ace_');
  });
});

describe('remoteHost', () => {
  const rows: Array<[url: string, want: string]> = [
    ['https://git.cloonar.com/Cloonar/coding-lab.git', 'git.cloonar.com'],
    ['https://user:pass@github.com/foo/bar.git', 'github.com'],
    ['http://localhost:3000/o/r.git', 'localhost'],
    ['git@git.cloonar.com:Cloonar/coding-lab.git', 'git.cloonar.com'],
    ['ssh://git@git.cloonar.com:2222/Cloonar/coding-lab.git', 'git.cloonar.com'],
    ['not-a-url', 'not-a-url'], // fallback: shown verbatim
  ];

  for (const [url, want] of rows) {
    it(`${JSON.stringify(url)} → ${JSON.stringify(want)}`, () => {
      expect(remoteHost(url)).toBe(want);
    });
  }
});

describe('forgeWebUrl', () => {
  const rows: Array<[url: string, forgeKind: ForgeKind, want: string | null]> = [
    [
      'git@git.cloonar.com:Cloonar/coding-lab.git',
      'forgejo',
      'https://git.cloonar.com/Cloonar/coding-lab',
    ],
    ['host:owner/repo', 'forgejo', 'https://host/owner/repo'], // scp-like, no user
    [
      'ssh://git@git.cloonar.com:2222/Cloonar/coding-lab.git',
      'forgejo',
      'https://git.cloonar.com/Cloonar/coding-lab', // ssh port dropped
    ],
    [
      'https://git.cloonar.com/Cloonar/coding-lab.git',
      'forgejo',
      'https://git.cloonar.com/Cloonar/coding-lab',
    ],
    ['https://github.com/foo/bar', 'github', 'https://github.com/foo/bar'],
    ['http://localhost:3000/o/r.git', 'github', 'http://localhost:3000/o/r'], // http scheme+port preserved
    ['https://git.cloonar.com/Cloonar/coding-lab.git', 'none', null],
    ['not a url', 'forgejo', null],
    ['', 'forgejo', null],
    ['host-only', 'forgejo', null],
  ];

  for (const [url, forgeKind, want] of rows) {
    it(`${JSON.stringify(url)}, ${forgeKind} → ${JSON.stringify(want)}`, () => {
      expect(forgeWebUrl(url, forgeKind)).toBe(want);
    });
  }
});
