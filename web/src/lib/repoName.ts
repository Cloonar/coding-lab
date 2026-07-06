// Client-side PREVIEW of the repo-name derivation the server performs when
// `name` is omitted from POST /repos: URL basename, sanitized with the v0
// scanner rules ("/" → "-", anything outside [A-Za-z0-9_-] → "_"). The server
// remains the authority — this exists only so the add-repo form can show the
// name that will be used before submit. (One knowing divergence: the Go
// sanitizer walks bytes, so a multi-byte rune becomes several underscores;
// here it becomes one. ASCII inputs — the practical case — agree exactly.)

/** v0 scanner sanitize: "/" → "-", then anything outside [A-Za-z0-9_-] → "_". */
export function sanitizeName(name: string): string {
  let out = '';
  for (const ch of name.replaceAll('/', '-')) {
    out += /[A-Za-z0-9_-]/.test(ch) ? ch : '_';
  }
  return out;
}

/**
 * Derives the repo name preview from a remote URL: basename (handles both
 * scheme URLs and scp-like `git@host:owner/repo.git`), minus a `.git`
 * suffix, sanitized. Empty input → empty preview.
 */
export function deriveRepoName(remoteUrl: string): string {
  let s = remoteUrl.trim();
  if (s === '') return '';
  s = s.replace(/\/+$/, '');
  // Basename: after the last "/" — or the last ":" for scp-like URLs, where
  // the colon separates host from path and no slash may follow it.
  const cut = Math.max(s.lastIndexOf('/'), s.lastIndexOf(':'));
  if (cut >= 0) s = s.slice(cut + 1);
  if (s.toLowerCase().endsWith('.git')) s = s.slice(0, -4);
  return sanitizeName(s);
}

/** Extracts the remote host for display on repo cards ("git.cloonar.com"). */
export function remoteHost(remoteUrl: string): string {
  const url = remoteUrl.trim();
  // scheme://[user@]host[:port]/…
  const scheme = /^[a-z][a-z0-9+.-]*:\/\/(?:[^/@]*@)?([^/:]+)/i.exec(url);
  const schemeHost = scheme?.[1];
  if (schemeHost !== undefined) return schemeHost;
  // scp-like: [user@]host:path
  const scp = /^(?:[^@/]+@)?([^:/]+):/.exec(url);
  const scpHost = scp?.[1];
  if (scpHost !== undefined) return scpHost;
  return url;
}
