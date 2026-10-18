// Global settings › Previews (issue #106): a read-only account of the preview
// listener's TLS front door — whether previews are on, the domain and URL
// shape they are served on, which certificate is presented (lab's own CA or
// the operator's pair), when the leaf expires, the last failed reload or
// renewal, and whether plain HTTP redirects. It has no field: nothing here
// waits for the save bar (the server owns all of it through flags and the
// NixOS module), so the section is a status card like the Credential gateway
// and SSH bastion cards at the end of General.
//
// With lab's own CA the section also carries the one thing an operator does
// here: download the CA certificate and trust it once per device. The link is
// a real browser navigation — `rel="external"` makes @solidjs/router's click
// handler skip it (it skips anchors with a `target`, a `download` attribute
// or `rel="external"`), so the server's Content-Disposition decides. No
// `download` attribute: iOS Safari would then save the file to Files instead
// of offering to install it as a profile.
//
// A failed load shows like the rest of the page's load errors: a banner with
// Try again.

import { Match, Show, Switch, createResource } from 'solid-js';
import {
  PREVIEW_CA_URL,
  errorMessage,
  getPreview,
  type PreviewCert,
  type PreviewInfo,
} from '../../../api';
import Banner from '../../../components/Banner';

/** The answer when previews are on. */
type EnabledPreview = Extract<PreviewInfo, { enabled: true }>;

/** A leaf with fewer days than this left is flagged. The server re-mints a
 *  lab-CA leaf at the same threshold; an operator pair is on its own. */
const EXPIRY_WARN_DAYS = 30;
const DAY_MS = 24 * 60 * 60 * 1000;

/** The URL a preview is served on, e.g. `https://<name>.lab.internal:8443`;
 *  the port is only printed when it is not the https default. */
export function previewUrlShape(domain: string, port: number): string {
  return `https://<name>.${domain}${port === 443 ? '' : `:${port}`}`;
}

/** Whole days from `now` to `notAfter` (negative once past); null when the
 *  timestamp does not parse. */
function daysLeft(notAfter: string, now: number): number | null {
  const at = Date.parse(notAfter);
  return Number.isNaN(at) ? null : Math.floor((at - now) / DAY_MS);
}

function sourceLabel(cert: PreviewCert): string {
  return cert.source === 'lab-ca' ? "Lab's own CA" : 'Operator certificate';
}

export default function PreviewsSection() {
  const [info, { refetch }] = createResource(() => getPreview());
  /** The answer when previews are on; undefined while loading or when off. */
  const on = () => {
    const current = info();
    return current?.enabled ? current : undefined;
  };

  return (
    <>
      <Show when={info.error !== undefined}>
        <Banner
          message={`The preview settings could not be loaded. ${errorMessage(info.error)}`}
          action={
            <button type="button" onClick={() => void refetch()}>
              Try again
            </button>
          }
        />
      </Show>
      <Show when={info.error === undefined}>
        <div class="card settings-card preview-status">
          <Switch>
            <Match when={on()}>{(p) => <PreviewOn info={p()} />}</Match>
            <Match when={info() !== undefined}>
              <p class="preview-off">
                Previews are off. Turn them on with <code class="mono">--preview-addr</code>, or{' '}
                <code class="mono">services.lab.preview.enable</code> on NixOS.
              </p>
            </Match>
            <Match when={true}>
              <p class="muted card-sub" role="status">
                checking…
              </p>
            </Match>
          </Switch>
        </div>
      </Show>
    </>
  );
}

function PreviewOn(props: { info: EnabledPreview }) {
  return (
    <>
      <dl class="preview-facts">
        <div class="preview-fact">
          <dt>Domain</dt>
          <dd class="mono">{props.info.domain}</dd>
        </div>
        <div class="preview-fact">
          <dt>Preview URLs</dt>
          <dd class="mono">{previewUrlShape(props.info.domain, props.info.port)}</dd>
        </div>
        <div class="preview-fact">
          <dt>Certificate</dt>
          <dd>{sourceLabel(props.info.cert)}</dd>
        </div>
        <div class="preview-fact">
          <dt>Expires</dt>
          <dd>
            <Expiry notAfter={props.info.cert.not_after} />
          </dd>
        </div>
        <div class="preview-fact">
          <dt>HTTP redirect</dt>
          <dd>{props.info.redirect ? 'On' : 'Off'}</dd>
        </div>
      </dl>
      <Show when={(props.info.cert.error ?? '') !== ''}>
        <p class="preview-cert-error" role="alert">
          Certificate problem: {props.info.cert.error}
        </p>
      </Show>
      <Show when={props.info.ca_download}>
        <TrustCa />
      </Show>
    </>
  );
}

/** The leaf's expiry as a localized date, flagged when it is close or past. */
function Expiry(props: { notAfter: string }) {
  const left = () => daysLeft(props.notAfter, Date.now());
  /** Days left when the leaf is close enough to flag, boxed so 0 stays truthy. */
  const flagged = () => {
    const days = left();
    return days !== null && days < EXPIRY_WARN_DAYS ? { days } : undefined;
  };
  const date = () => {
    const at = new Date(props.notAfter);
    return Number.isNaN(at.getTime()) ? props.notAfter : at.toLocaleDateString();
  };
  return (
    <>
      <span>{date()}</span>
      {/* Not <Show when={left()}>: 0 days left ("Expires today") is falsy. */}
      <Show when={flagged()}>
        {(f) => (
          <span class={f().days < 0 ? 'chip status-error' : 'chip status-warn'}>
            {f().days < 0 ? 'Expired' : f().days === 0 ? 'Expires today' : `${f().days} days left`}
          </span>
        )}
      </Show>
    </>
  );
}

/** The CA download and the per-device trust steps. */
function TrustCa() {
  return (
    <div class="settings-group preview-trust">
      <h3 class="settings-sub">Trust lab's CA on your devices</h3>
      <p class="muted card-sub">
        Install the CA certificate once per device and every preview opens without a warning.
      </p>
      <div class="card-actions">
        {/* rel="external": a real browser navigation, not the router's
            (see the file head). No `download` attribute either. */}
        <a class="card-link" href={PREVIEW_CA_URL} rel="external">
          Download CA certificate
        </a>
      </div>
      <ul class="preview-trust-list">
        <li>
          <strong>iPhone / iPad:</strong> open this page in Safari and download the certificate,
          install the profile in Settings › General › VPN &amp; Device Management, then enable full
          trust under Settings › General › About › Certificate Trust Settings.
        </li>
        <li>
          <strong>macOS:</strong> open the file, add it to the login keychain in Keychain Access,
          and set &ldquo;When using this certificate&rdquo; to Always Trust.
        </li>
        <li>
          <strong>Linux:</strong> copy it to{' '}
          <code class="mono">/usr/local/share/ca-certificates/</code> (Debian/Ubuntu: run{' '}
          <code class="mono">update-ca-certificates</code>) or, on NixOS, add it to{' '}
          <code class="mono">security.pki.certificateFiles</code>. Firefox has its own certificate
          store and needs the CA imported there.
        </li>
      </ul>
    </div>
  );
}
