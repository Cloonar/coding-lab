// Reading a git remote URL in the browser (issue #61): what the Add
// repository form may say about a remote before it is sent, and the host the
// delete dialog names when it says the remote is not touched.
//
// Deliberately permissive — the server (and git) stay the authority. The form
// only refuses what can never be cloned: an empty field, text with spaces in
// it, or something that reads as none of the three remote shapes git accepts:
//
//   scheme://[user@]host[:port]/path    ssh, https, git, file:///path, …
//   [user@]host:path                    scp-like (the first ':' ends the host)
//   /path, ./path, ../path, ~/path      a path on lab's own host
//
// Nothing here guesses the forge: the host is the only thing a URL states.

/** A remote as far as its text goes; `host` is null for a local path. */
export interface ParsedRemote {
  kind: 'url' | 'scp' | 'path';
  host: string | null;
}

/** Whether a remote path names something past its slashes ("/" alone does not). */
function hasName(path: string): boolean {
  return path.replace(/\.git$/i, '').replace(/[/\\]+/g, '') !== '';
}

/**
 * Parses a remote URL into its shape and host, or null when it reads as none
 * of the shapes above (including empty input). Never throws.
 */
export function parseRemote(remoteUrl: string): ParsedRemote | null {
  const url = remoteUrl.trim();
  if (url === '' || /\s/.test(url)) return null;

  const scheme = /^([a-z][a-z0-9+.-]*):\/\/(.*)$/i.exec(url);
  if (scheme !== null) {
    const rest = scheme[2] ?? '';
    if (scheme[1]?.toLowerCase() === 'file') {
      // file:///srv/git/repo.git (an empty host) or file://host/path.
      const path = /^[^/]*(\/.*)$/.exec(rest)?.[1] ?? '';
      return hasName(path) ? { kind: 'path', host: null } : null;
    }
    // [user[:password]@]host[:port]/path — the path must name something.
    const m = /^(?:[^/@]*@)?(\[[^\]/]+\]|[^/:@[\]]+)(?::\d*)?(\/.*)$/.exec(rest);
    if (m === null || !hasName(m[2] ?? '')) return null;
    return { kind: 'url', host: m[1] ?? null };
  }

  if (/^(?:\/|\.\.?\/|~\/)/.test(url)) {
    return hasName(url) ? { kind: 'path', host: null } : null;
  }

  // scp-like: the first ':' separates the host, which holds no '/'.
  const scp = /^(?:[^@/:]+@)?([^/:@]+):(.+)$/.exec(url);
  if (scp !== null && hasName(scp[2] ?? '')) return { kind: 'scp', host: scp[1] ?? null };
  return null;
}

/** The example the Add repository form quotes in its URL messages. */
export const REMOTE_EXAMPLE = 'git@github.com:owner/repo.git';

/**
 * Why a remote URL cannot be sent as it stands, or null when it can. The
 * message is shown under the Remote URL field.
 */
export function remoteUrlProblem(remoteUrl: string): string | null {
  if (remoteUrl.trim() === '') return `Paste the remote URL, for example ${REMOTE_EXAMPLE}`;
  if (parseRemote(remoteUrl) === null) {
    return `This is not a remote lab can clone. Use an SSH, HTTPS or file URL, for example ${REMOTE_EXAMPLE}`;
  }
  return null;
}
