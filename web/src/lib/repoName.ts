// Client-side PREVIEW of the repo-name derivation the server performs when
// `name` is omitted from POST /repos: URL basename, sanitized with the v0
// scanner rules ("/" → "-", anything outside [A-Za-z0-9_-] → "_"). The server
// remains the authority — this exists only so the add-repo form can show the
// name that will be used before submit. (One knowing divergence: the Go
// sanitizer walks bytes, so a multi-byte rune becomes several underscores;
// here it becomes one. ASCII inputs — the practical case — agree exactly.)

import type { ForgeKind } from '../api';

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

/**
 * Builds the forge's web URL from a repo's clone remote ("open on forge"
 * links). Forgejo and GitHub both serve `https://host/owner/repo` for a
 * repo's web page, so there's no per-kind branching beyond the `'none'`
 * guard below — callers should hide the link when this returns `null`
 * (no forge, or a remote that doesn't parse into a host plus path).
 * Never throws.
 */
export function forgeWebUrl(remoteUrl: string, forgeKind: ForgeKind): string | null {
  if (forgeKind === 'none') return null;
  const url = remoteUrl.trim();
  if (url === '') return null;

  let host: string;
  let scheme: string;
  let port: string | undefined;
  let rawPath: string;

  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(url)) {
    // scheme://[user@]host[:port]/path — keep an http(s) scheme and its port
    // (a genuine web port); anything else (ssh, git, …) becomes plain https
    // with the port dropped, since ssh ports aren't web ports.
    const m = /^([a-z][a-z0-9+.-]*):\/\/(?:[^/@]*@)?([^/:]+)(?::(\d+))?\/(.+)$/i.exec(url);
    if (m === null) return null;
    const isHttp = /^https?$/i.test(m[1]!);
    host = m[2]!;
    scheme = isHttp ? m[1]!.toLowerCase() : 'https';
    port = isHttp ? m[3] : undefined;
    rawPath = m[4]!;
  } else {
    // scp-like: [user@]host:path
    const m = /^(?:[^@/]+@)?([^:/]+):(.+)$/.exec(url);
    if (m === null) return null;
    host = m[1]!;
    scheme = 'https';
    rawPath = m[2]!;
  }

  let path = rawPath.replace(/\/+$/, '');
  if (path.toLowerCase().endsWith('.git')) path = path.slice(0, -4);
  path = path.replace(/\/+$/, '');
  if (path === '') return null;

  return `${scheme}://${host}${port !== undefined ? `:${port}` : ''}/${path}`;
}
