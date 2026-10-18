// Global settings › Previews coverage (issue #106): the read-only account of
// the preview listener's TLS front door. Off says how to turn previews on; on
// shows the domain, the URL shape (with the port only when it is not 443), the
// certificate source, the leaf's expiry (flagged when under 30 days or past),
// the last renewal error and the HTTP redirect; with lab's own CA it offers
// the CA download and the per-device trust steps, with an operator certificate
// it does not. The section has no field and never shows the save bar. The
// download link must be a real browser navigation, not the router's.

import { describe, expect, it } from 'vitest';
import type { PreviewInfo } from '../../../api';
import {
  container,
  h,
  history,
  installSettingsHooks,
  mountPage,
  pageSection,
  saveBar,
  settle,
  waitFor,
} from '../harness';

installSettingsHooks();

const DAY_MS = 24 * 60 * 60 * 1000;

/** An enabled answer whose leaf expires `days` from now. */
function previewOn(
  overrides: {
    port?: number;
    redirect?: boolean;
    source?: 'lab-ca' | 'operator';
    days?: number;
    error?: string;
  } = {},
): PreviewInfo {
  const source = overrides.source ?? 'lab-ca';
  return {
    enabled: true,
    domain: 'lab.internal',
    port: overrides.port ?? 443,
    redirect: overrides.redirect ?? true,
    cert: {
      source,
      not_after: new Date(Date.now() + (overrides.days ?? 300) * DAY_MS).toISOString(),
      dns_names: ['*.lab.internal', 'lab.internal'],
      ...(overrides.error !== undefined ? { error: overrides.error } : {}),
    },
    ca_download: source === 'lab-ca',
  };
}

const section = () => pageSection('previews');
const text = () => section().textContent ?? '';

/** The value cell next to a fact label. */
function fact(label: string): HTMLElement {
  const row = Array.from(section().querySelectorAll('.preview-fact')).find(
    (el) => el.querySelector('dt')?.textContent === label,
  );
  if (!row) throw new Error(`missing preview fact "${label}"`);
  return row.querySelector('dd') as HTMLElement;
}

const downloadLink = () =>
  Array.from(section().querySelectorAll<HTMLAnchorElement>('a')).find(
    (a) => a.textContent === 'Download CA certificate',
  );

describe('Settings › Previews', () => {
  it('is a read-only section in Setup: no field, no save bar', async () => {
    h.previewOnServer = previewOn();
    await mountPage('/settings/previews');
    await settle();

    expect(section().querySelector('.settings-section-head h2')?.textContent).toBe('Previews');
    expect(section().querySelector('[data-field]')).toBeNull();
    expect(section().querySelector('input, textarea, select')).toBeNull();
    expect(saveBar()).toBeNull();
  });

  it('previews off: one line saying how to turn them on', async () => {
    await mountPage();
    await settle();

    expect(section().querySelector('.preview-off')?.textContent).toBe(
      'Previews are off. Turn them on with --preview-addr, or services.lab.preview.enable on NixOS.',
    );
    expect(section().querySelector('.preview-facts')).toBeNull();
    expect(downloadLink()).toBeUndefined();
  });

  it('lab-ca: domain, URL shape, source, expiry, redirect, the download link and the device lines', async () => {
    h.previewOnServer = previewOn({ days: 300 });
    await mountPage();
    await settle();

    expect(fact('Domain').textContent).toBe('lab.internal');
    expect(fact('Preview URLs').textContent).toBe('https://<name>.lab.internal');
    expect(fact('Certificate').textContent).toBe("Lab's own CA");
    expect(fact('HTTP redirect').textContent).toBe('On');
    expect(fact('Expires').textContent).toBe(
      new Date(Date.now() + 300 * DAY_MS).toLocaleDateString(),
    );
    // Far from expiry: no flag, no error line.
    expect(fact('Expires').querySelector('.chip')).toBeNull();
    expect(section().querySelector('.preview-cert-error')).toBeNull();

    const link = downloadLink();
    expect(link?.getAttribute('href')).toBe('/api/v1/preview/ca');
    expect(link?.hasAttribute('download')).toBe(false);
    expect(link?.getAttribute('rel')).toBe('external');

    const steps = Array.from(section().querySelectorAll('.preview-trust-list li')).map(
      (li) => li.textContent ?? '',
    );
    expect(steps).toHaveLength(3);
    expect(steps[0]).toContain('iPhone / iPad');
    expect(steps[0]).toContain('Safari');
    expect(steps[0]).toContain('Settings › General › VPN & Device Management');
    expect(steps[0]).toContain('Settings › General › About › Certificate Trust Settings');
    expect(steps[1]).toContain('macOS');
    expect(steps[1]).toContain('Keychain Access');
    expect(steps[1]).toContain('Always Trust');
    expect(steps[2]).toContain('Linux');
    expect(steps[2]).toContain('/usr/local/share/ca-certificates/');
    expect(steps[2]).toContain('update-ca-certificates');
    expect(steps[2]).toContain('security.pki.certificateFiles');
    expect(steps[2]).toContain('Firefox has its own');
  });

  it("the download link is a real browser navigation, not the router's", async () => {
    h.previewOnServer = previewOn();
    await mountPage();
    await settle();
    const before = history.get();

    // Registered after the router's delegated handler, so it sees what the
    // router decided; it also stops jsdom from "navigating".
    let prevented: boolean | null = null;
    const probe = (event: Event) => {
      prevented = event.defaultPrevented;
      event.preventDefault();
    };
    document.addEventListener('click', probe);
    try {
      downloadLink()?.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
    } finally {
      document.removeEventListener('click', probe);
    }

    expect(prevented).toBe(false);
    expect(history.get()).toBe(before);
  });

  it('operator certificate: the source is named and there is no download or device lines', async () => {
    h.previewOnServer = previewOn({ source: 'operator' });
    await mountPage();
    await settle();

    expect(fact('Certificate').textContent).toBe('Operator certificate');
    expect(downloadLink()).toBeUndefined();
    expect(section().querySelector('.preview-trust')).toBeNull();
    expect(text()).not.toContain('Keychain');
  });

  it('shows the port in the URL shape for 8443', async () => {
    h.previewOnServer = previewOn({ port: 8443 });
    await mountPage();
    await settle();
    expect(fact('Preview URLs').textContent).toBe('https://<name>.lab.internal:8443');
  });

  it('leaves the port out of the URL shape on 443', async () => {
    h.previewOnServer = previewOn({ port: 443 });
    await mountPage();
    await settle();
    expect(fact('Preview URLs').textContent).toBe('https://<name>.lab.internal');
    expect(fact('Preview URLs').textContent).not.toContain(':443');
  });

  it('says when the HTTP redirect is off', async () => {
    h.previewOnServer = previewOn({ redirect: false });
    await mountPage();
    await settle();
    expect(fact('HTTP redirect').textContent).toBe('Off');
  });

  it('flags a leaf with under 30 days left', async () => {
    h.previewOnServer = previewOn({ days: 12.5 });
    await mountPage();
    await settle();

    const chip = fact('Expires').querySelector('.chip');
    expect(chip?.textContent).toBe('12 days left');
    expect(chip?.classList.contains('status-warn')).toBe(true);
  });

  it('flags an expired leaf', async () => {
    h.previewOnServer = previewOn({ days: -3 });
    await mountPage();
    await settle();

    const chip = fact('Expires').querySelector('.chip');
    expect(chip?.textContent).toBe('Expired');
    expect(chip?.classList.contains('status-error')).toBe(true);
  });

  it('flags a leaf that expires today (0 whole days left)', async () => {
    h.previewOnServer = previewOn({ days: 0.5 });
    await mountPage();
    await settle();

    const chip = fact('Expires').querySelector('.chip');
    expect(chip?.textContent).toBe('Expires today');
    expect(chip?.classList.contains('status-warn')).toBe(true);
  });

  it('does not flag a leaf with exactly 30 days left', async () => {
    h.previewOnServer = previewOn({ days: 30.5 });
    await mountPage();
    await settle();
    expect(fact('Expires').querySelector('.chip')).toBeNull();
  });

  it('shows the last failed renewal as an alert line', async () => {
    h.previewOnServer = previewOn({ error: 'reload cert.pem: bad key pair' });
    await mountPage();
    await settle();

    const line = section().querySelector('.preview-cert-error');
    expect(line?.textContent).toBe('Certificate problem: reload cert.pem: bad key pair');
    expect(line?.getAttribute('role')).toBe('alert');
  });

  it('says when the preview settings could not be loaded, and loads them on Try again', async () => {
    h.previewOnServer = previewOn({ source: 'operator' });
    h.previewError = 'state dir unreadable';
    await mountPage();
    await settle();

    const banner = await waitFor(
      () => section().querySelector('.banner'),
      'the preview load error',
    );
    expect(banner.textContent).toContain(
      'The preview settings could not be loaded. state dir unreadable',
    );
    expect(section().querySelector('.preview-status')).toBeNull();
    // The rest of the page is unaffected.
    expect(container.querySelector('#settings-agents')).not.toBeNull();

    Array.from(banner.querySelectorAll('button'))
      .find((b) => b.textContent === 'Try again')
      ?.click();
    await settle();

    expect(section().querySelector('.banner')).toBeNull();
    expect(fact('Certificate').textContent).toBe('Operator certificate');
    expect(h.previewGets).toBe(2);
  });
});
