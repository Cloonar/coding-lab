// CredentialGatewayStatus (issue #23): GET /onecli/health always answers 200,
// so every state — including `off` (not configured) — must render WITHOUT the
// error/muted-unknown treatment reserved for an actual fetch failure. Chip
// vocabulary mirrors ProviderAuthCard: bare .chip for off, .chip.in-use for
// ok, .chip.status-warn for degraded (naming the failing component + its raw
// dial error), .chip.status-error for unreachable, and the "unknown" muted
// treatment — never a crash — when the request itself fails.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { OneCLIHealth } from '../api';
import CredentialGatewayStatus from './CredentialGatewayStatus';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function jsonResponse(status: number, body?: unknown) {
  const text = body === undefined ? '' : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () =>
      text === ''
        ? Promise.reject(new SyntaxError('empty body'))
        : Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

function stubHealth(response: () => ReturnType<typeof jsonResponse> | Promise<never>): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown) => {
      expect(String(input)).toBe('/api/v1/onecli/health');
      const res = response();
      return res instanceof Promise ? res : Promise.resolve(res);
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function mount(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(() => <CredentialGatewayStatus />, container);
  await settle();
}

function chip(): HTMLElement {
  const el = container.querySelector('.card-head .chip, .card-head .muted');
  if (!el) throw new Error('missing status chip/badge in the card head');
  return el as HTMLElement;
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('CredentialGatewayStatus', () => {
  it('renders the title from CONTEXT.md glossary term "credential gateway"', async () => {
    stubHealth(() =>
      jsonResponse(200, {
        state: 'off',
        api: { configured: false, reachable: false },
        gateway: { configured: false, reachable: false },
      } satisfies OneCLIHealth),
    );
    await mount();

    expect(container.querySelector('h2')?.textContent).toBe('Credential gateway');
  });

  it('off: a muted (bare) chip reading Off, with a "not configured" hint — never an error', async () => {
    stubHealth(() =>
      jsonResponse(200, {
        state: 'off',
        api: { configured: false, reachable: false },
        gateway: { configured: false, reachable: false },
      } satisfies OneCLIHealth),
    );
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip');
    expect(badge.textContent).toBe('Off');
    expect(container.textContent).toContain('Not configured');
    expect(container.querySelector('.chip.status-error')).toBeNull();
  });

  it('ok: chip in-use reading Reachable', async () => {
    stubHealth(() =>
      jsonResponse(200, {
        state: 'ok',
        api: { configured: true, reachable: true, url: 'http://127.0.0.1:10254' },
        gateway: { configured: true, reachable: true, url: 'http://10.88.0.1:10255' },
      } satisfies OneCLIHealth),
    );
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip in-use');
    expect(badge.textContent).toBe('Reachable');
  });

  it('degraded: a warning chip naming the failing component and its dial error', async () => {
    stubHealth(() =>
      jsonResponse(200, {
        state: 'degraded',
        api: { configured: true, reachable: true, url: 'http://127.0.0.1:10254' },
        gateway: {
          configured: true,
          reachable: false,
          url: 'http://10.88.0.1:10255',
          error: 'dial tcp 10.88.0.1:10255: connect: connection refused',
        },
      } satisfies OneCLIHealth),
    );
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip status-warn');
    expect(badge.textContent).toBe('Partly reachable');
    expect(container.textContent).toContain('credential gateway unreachable');
    expect(container.textContent).toContain(
      'dial tcp 10.88.0.1:10255: connect: connection refused',
    );
    // The still-reachable component is not named as failing.
    expect(container.textContent).not.toContain('OneCLI API unreachable');
  });

  it('unreachable: chip status-error reading Unreachable, with the dial error surfaced', async () => {
    stubHealth(() =>
      jsonResponse(200, {
        state: 'unreachable',
        api: { configured: true, reachable: false, error: 'dial tcp 127.0.0.1:10254: refused' },
        gateway: { configured: true, reachable: false, error: 'dial tcp 10.88.0.1:10255: refused' },
      } satisfies OneCLIHealth),
    );
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip status-error');
    expect(badge.textContent).toBe('Unreachable');
    expect(container.textContent).toContain('OneCLI API unreachable: dial tcp 127.0.0.1:10254');
    expect(container.textContent).toContain(
      'credential gateway unreachable: dial tcp 10.88.0.1:10255',
    );
  });

  it('a failed fetch renders the same muted "unknown" treatment, never a crash', async () => {
    stubHealth(() => Promise.reject(new Error('network down')));

    await expect(mount()).resolves.toBeUndefined();

    const badge = chip();
    expect(badge.className).toBe('muted');
    expect(badge.textContent).toBe('unknown');
    expect(container.querySelector('.chip')).toBeNull();
  });
});
