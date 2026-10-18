import { request } from './core';

// --- issue #106: preview TLS front door ---
//
// What Settings → Previews shows: whether the preview listener is on, the
// domain and port it serves `https://<name>.<domain>` on, which certificate it
// presents (lab's own CA, or the operator's pair) and when that leaf expires.
// A read-only status like onecli.ts / warpgate.ts; nothing here is a setting
// the save bar could commit.

/** Where the served certificate comes from: lab's own minted CA + leaf, or
 *  the operator's `--preview-cert-file` / `--preview-key-file` pair. */
export type PreviewCertSource = 'lab-ca' | 'operator';

/** The leaf the preview listener currently serves. */
export interface PreviewCert {
  source: PreviewCertSource;
  /** RFC 3339 expiry of the leaf. */
  not_after: string;
  /** The leaf's SANs, e.g. `["*.lab.internal", "lab.internal"]`. */
  dns_names: string[];
  /** The last failed reload or renewal; omitted or "" when all is well. */
  error?: string;
}

/** `enabled: false` is the normal answer when previews are not configured. */
export type PreviewInfo =
  | { enabled: false }
  | {
      enabled: true;
      domain: string;
      /** The preview listener's port; 443 is left out of printed URLs. */
      port: number;
      /** Whether a plain-HTTP listener redirects to HTTPS. */
      redirect: boolean;
      cert: PreviewCert;
      /** True only for source `lab-ca`: the CA can be downloaded. */
      ca_download: boolean;
    };

/** GET /preview: always 200 for an authenticated caller, enabled or not. */
export function getPreview(): Promise<PreviewInfo> {
  return request<PreviewInfo>('GET', '/preview');
}

/**
 * GET /preview/ca serves lab's CA certificate as an attachment
 * (`application/x-x509-ca-cert`) behind the session cookie; 404 for an
 * operator certificate or with previews off. It is a plain URL, not a
 * request() call: the browser itself must navigate to it so a phone offers
 * the certificate for install. Mirrors core.ts's BASE ('/api/v1' — the SPA is
 * served from the origin root, there is no base path).
 */
export const PREVIEW_CA_URL = '/api/v1/preview/ca';
