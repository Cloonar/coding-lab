// BastionStatus (issue #39 / ADR-0068): GET /warpgate/health always answers
// 200, so every state — including `off` (not configured) — must render
// WITHOUT the error/muted-unknown treatment reserved for an actual fetch
// failure. Chip vocabulary mirrors CredentialGatewayStatus: bare .chip for
// off, .chip.in-use for ok, .chip.status-warn for degraded,
// .chip.status-error for unreachable, and the "unknown" muted treatment —
// never a crash — when the request itself fails.
//
// The host-key detail is this card's own addition over the OneCLI precedent:
// read-only, since which key runs trust is a server setting
// (--warpgate-ssh-host-key), it names the setting on a mismatch and lists the
// observed fingerprints when nothing is pinned.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { WarpgateHealth } from '../api';
import BastionStatus from './BastionStatus';

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

/** Stubs fetch for the one route this card calls: GET /warpgate/health
 *  answers from `health()`. Anything else is a test failure. */
function stubApi(health: () => ReturnType<typeof jsonResponse> | Promise<never>): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === '/api/v1/warpgate/health' && method === 'GET') {
        const res = health();
        return res instanceof Promise ? res : Promise.resolve(res);
      }
      throw new Error(`unexpected fetch: ${method} ${url}`);
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
  dispose = render(() => <BastionStatus />, container);
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

const OFF: WarpgateHealth = {
  state: 'off',
  api: { configured: false, reachable: false },
  ssh: { configured: false, reachable: false },
};

describe('BastionStatus', () => {
  it('renders the "SSH bastion" title', async () => {
    stubApi(() => jsonResponse(200, OFF));
    await mount();

    expect(container.querySelector('h2')?.textContent).toBe('SSH bastion');
  });

  it('off: a muted (bare) chip reading Off, with a "not configured" hint — never an error', async () => {
    stubApi(() => jsonResponse(200, OFF));
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip');
    expect(badge.textContent).toBe('Off');
    expect(container.textContent).toContain('Not configured — the SSH bastion integration is off.');
    expect(container.querySelector('.chip.status-error')).toBeNull();
  });

  it('ok with a pinned host key: chip in-use reading Reachable, with no detail line', async () => {
    stubApi(() =>
      jsonResponse(200, {
        state: 'ok',
        api: {
          configured: true,
          reachable: true,
          url: 'https://localhost:8888',
          version: '0.29.1',
          authenticated: true,
        },
        ssh: { configured: true, reachable: true, addr: '10.88.0.1:2222' },
        hostKey: { state: 'pinned', pinned: ['SHA256:abc'], observed: ['SHA256:abc'] },
      } satisfies WarpgateHealth),
    );
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip in-use');
    expect(badge.textContent).toBe('Reachable');
    expect(container.textContent).not.toContain('unreachable');
    expect(container.textContent).not.toContain('trusted host key');
    expect(container.textContent).not.toContain('Observed');
  });

  it('degraded: a warning chip naming the failing component and its dial error', async () => {
    stubApi(() =>
      jsonResponse(200, {
        state: 'degraded',
        api: {
          configured: true,
          reachable: true,
          url: 'https://localhost:8888',
          version: '0.29.1',
          authenticated: true,
        },
        ssh: {
          configured: true,
          reachable: false,
          addr: '10.88.0.1:2222',
          error: 'dial tcp 10.88.0.1:2222: connect: connection refused',
        },
        hostKey: { state: 'unreachable', pinned: [], observed: [] },
      } satisfies WarpgateHealth),
    );
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip status-warn');
    expect(badge.textContent).toBe('Degraded');
    expect(container.textContent).toContain('SSH listener unreachable');
    expect(container.textContent).toContain('dial tcp 10.88.0.1:2222: connect: connection refused');
    expect(container.textContent).not.toContain('Warpgate API unreachable');
  });

  it('unreachable: chip status-error reading Unreachable, with both dial errors surfaced', async () => {
    stubApi(() =>
      jsonResponse(200, {
        state: 'unreachable',
        api: { configured: true, reachable: false, error: 'dial tcp 127.0.0.1:8888: refused' },
        ssh: { configured: true, reachable: false, error: 'dial tcp 10.88.0.1:2222: refused' },
      } satisfies WarpgateHealth),
    );
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip status-error');
    expect(badge.textContent).toBe('Unreachable');
    expect(container.textContent).toContain(
      'Warpgate API unreachable: dial tcp 127.0.0.1:8888: refused',
    );
    expect(container.textContent).toContain(
      'SSH listener unreachable: dial tcp 10.88.0.1:2222: refused',
    );
  });

  it('degraded: names a rejected admin token distinctly from a dial error', async () => {
    stubApi(() =>
      jsonResponse(200, {
        state: 'degraded',
        api: {
          configured: true,
          reachable: true,
          url: 'https://localhost:8888',
          authenticated: false,
        },
        ssh: { configured: true, reachable: true, addr: '10.88.0.1:2222' },
      } satisfies WarpgateHealth),
    );
    await mount();

    expect(container.textContent).toContain('admin token rejected');
    expect(container.textContent).not.toContain('Warpgate API unreachable');
  });

  it('unpinned: ok, with the setting to pin and the observed fingerprints listed', async () => {
    stubApi(() =>
      jsonResponse(200, {
        state: 'ok',
        api: { configured: true, reachable: true, authenticated: true },
        ssh: { configured: true, reachable: true, addr: '10.88.0.1:2222' },
        hostKey: { state: 'unpinned', pinned: [], observed: ['SHA256:one', 'SHA256:two'] },
      } satisfies WarpgateHealth),
    );
    await mount();

    expect(chip().textContent).toBe('Reachable');
    expect(container.textContent).toContain('No trusted host key is configured');
    expect(container.textContent).toContain('--warpgate-ssh-host-key');
    const codes = Array.from(container.querySelectorAll('code')).map((c) => c.textContent);
    expect(codes).toEqual(['--warpgate-ssh-host-key', 'SHA256:one', 'SHA256:two']);
    expect(container.textContent).not.toContain('Trusted:');
  });

  it('mismatch: degraded, showing trusted vs observed fingerprints and naming the setting', async () => {
    stubApi(() =>
      jsonResponse(200, {
        state: 'degraded',
        api: { configured: true, reachable: true, authenticated: true },
        ssh: { configured: true, reachable: true, addr: '10.88.0.1:2222' },
        hostKey: { state: 'mismatch', pinned: ['SHA256:old'], observed: ['SHA256:new'] },
      } satisfies WarpgateHealth),
    );
    await mount();

    expect(chip().textContent).toBe('Degraded');
    expect(container.textContent).toContain('does not match the trusted key');
    expect(container.textContent).toContain('Target-bearing spawns are blocked');
    expect(container.textContent).toContain('--warpgate-ssh-host-key');
    expect(container.textContent).toContain('Trusted: SHA256:old');
    expect(container.textContent).toContain('Observed: SHA256:new');
    expect(container.querySelector('button')).toBeNull();
  });

  it('a failed fetch renders the same muted "unknown" treatment, never a crash', async () => {
    stubApi(() => Promise.reject(new Error('network down')));

    await expect(mount()).resolves.toBeUndefined();

    const badge = chip();
    expect(badge.className).toBe('muted');
    expect(badge.textContent).toBe('unknown');
    expect(container.querySelector('.chip')).toBeNull();
  });
});
