// A throwaway git remote for the smoke (issue #61): a bare repository with one
// commit on `main`, made with the real git CLI in a fresh temp dir, so Add
// repository clones something real without touching the network.
//
// web/ has no @types/node — tsc checks e2e/ together with the SPA, under its
// DOM-only types — so the few Node APIs used here are loaded through a
// specifier TypeScript does not resolve, and typed by hand, narrowly, below.

interface NodeChildProcess {
  execFileSync(
    file: string,
    args: readonly string[],
    options: { cwd: string; stdio: 'pipe' },
  ): unknown;
}

interface NodeFs {
  mkdtempSync(prefix: string): string;
  rmSync(path: string, options: { recursive: true; force: true }): void;
}

interface NodeOs {
  tmpdir(): string;
}

/** A Node built-in module by name. The non-literal specifier keeps tsc out of it. */
async function nodeModule<T>(name: string): Promise<T> {
  const specifier: string = `node:${name}`;
  return (await import(specifier)) as T;
}

export interface GitRemote {
  /** The bare repository's absolute path — what Add repository is given. */
  url: string;
  /** Removes the remote and its temp dir. */
  dispose: () => void;
}

/**
 * Creates `<tmp>/lab-e2e-remote.XXXXXX/<name>.git`, a bare repository whose
 * `main` holds one empty commit. The identity and signing settings are passed
 * per command, so the host's git config cannot make the commit fail.
 */
export async function createGitRemote(name: string): Promise<GitRemote> {
  const [{ execFileSync }, fs, os] = await Promise.all([
    nodeModule<NodeChildProcess>('child_process'),
    nodeModule<NodeFs>('fs'),
    nodeModule<NodeOs>('os'),
  ]);
  const root = fs.mkdtempSync(`${os.tmpdir()}/lab-e2e-remote.`);
  const work = `${root}/work`;
  const url = `${root}/${name}.git`;
  const git = (cwd: string, ...args: string[]) =>
    execFileSync(
      'git',
      [
        '-c',
        'user.name=lab smoke',
        '-c',
        'user.email=smoke@lab.invalid',
        '-c',
        'commit.gpgsign=false',
        ...args,
      ],
      { cwd, stdio: 'pipe' },
    );
  try {
    git(root, 'init', '--quiet', '--initial-branch=main', work);
    git(work, 'commit', '--quiet', '--allow-empty', '--message', 'Initial commit');
    git(root, 'clone', '--quiet', '--bare', work, url);
  } catch (err) {
    fs.rmSync(root, { recursive: true, force: true });
    throw err;
  }
  return { url, dispose: () => fs.rmSync(root, { recursive: true, force: true }) };
}
