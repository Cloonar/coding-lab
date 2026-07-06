import { describe, expect, it } from 'vitest';
import { deriveRepoName, remoteHost, sanitizeName } from './repoName';

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
