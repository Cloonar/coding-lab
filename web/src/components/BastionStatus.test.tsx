// BastionStatus (issue #39 / ADR-0068): GET /warpgate/health always answers
// 200, so every state — including `off` (not configured) — must render
// WITHOUT the error/muted-unknown treatment reserved for an actual fetch
// failure. Chip vocabulary mirrors CredentialGatewayStatus: bare .chip for
// off, .chip.in-use for ok, .chip.status-warn for degraded,
// .chip.status-error for unreachable, and the "unknown" muted treatment —
// never a crash — when the request itself fails.
//
// The host-key mismatch flow is this card's own addition over the OneCLI
// precedent: it is the only place in this component that makes a second,
// mutating call, so it gets its own confirm → POST → refetch coverage.

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

/**
 * Stubs fetch for both routes this card calls: GET /warpgate/health answers
 * from `opts.health()` (a function, so a test can change what the NEXT call
 * returns — e.g. the refetch after a successful accept), and POST
 * /warpgate/host-key/accept answers from `opts.accept`, recording every body
 * it was called with in `opts.acceptBodies`.
 */
function stubApi(opts: {
  health: () => ReturnType<typeof jsonResponse> | Promise<never>;
  accept?: (fingerprint: string) => ReturnType<typeof jsonResponse> | Promise<never>;
  acceptBodies?: { fingerprint: string }[];
}): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === '/api/v1/warpgate/health' && method === 'GET') {
        const res = opts.health();
        return res instanceof Promise ? res : Promise.resolve(res);
      }
      if (url === '/api/v1/warpgate/host-key/accept' && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as { fingerprint: string };
        opts.acceptBodies?.push(body);
        if (!opts.accept) throw new Error('unexpected accept call');
        const res = opts.accept(body.fingerprint);
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
    stubApi({ health: () => jsonResponse(200, OFF) });
    await mount();

    expect(container.querySelector('h2')?.textContent).toBe('SSH bastion');
  });

  it('off: a muted (bare) chip reading Off, with a "not configured" hint — never an error', async () => {
    stubApi({ health: () => jsonResponse(200, OFF) });
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip');
    expect(badge.textContent).toBe('Off');
    expect(container.textContent).toContain('Not configured — the SSH bastion integration is off.');
    expect(container.querySelector('.chip.status-error')).toBeNull();
  });

  it('ok: chip in-use reading Reachable, with no detail line', async () => {
    stubApi({
      health: () =>
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
          hostKey: { state: 'pinned', pinned: ['SHA256:abc'], observed: [] },
        } satisfies WarpgateHealth),
    });
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip in-use');
    expect(badge.textContent).toBe('Reachable');
    expect(container.textContent).not.toContain('unreachable');
    expect(container.textContent).not.toContain('Host key not pinned');
  });

  it('degraded: a warning chip naming the failing component and its dial error', async () => {
    stubApi({
      health: () =>
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
    });
    await mount();

    const badge = chip();
    expect(badge.className).toBe('chip status-warn');
    expect(badge.textContent).toBe('Degraded');
    expect(container.textContent).toContain('SSH listener unreachable');
    expect(container.textContent).toContain('dial tcp 10.88.0.1:2222: connect: connection refused');
    expect(container.textContent).not.toContain('Warpgate API unreachable');
  });

  it('unreachable: chip status-error reading Unreachable, with both dial errors surfaced', async () => {
    stubApi({
      health: () =>
        jsonResponse(200, {
          state: 'unreachable',
          api: { configured: true, reachable: false, error: 'dial tcp 127.0.0.1:8888: refused' },
          ssh: { configured: true, reachable: false, error: 'dial tcp 10.88.0.1:2222: refused' },
        } satisfies WarpgateHealth),
    });
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
    stubApi({
      health: () =>
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
    });
    await mount();

    expect(container.textContent).toContain('admin token rejected');
    expect(container.textContent).not.toContain('Warpgate API unreachable');
  });

  it('unpinned host key: a muted line, distinct from the mismatch UI', async () => {
    stubApi({
      health: () =>
        jsonResponse(200, {
          state: 'ok',
          api: { configured: true, reachable: true, authenticated: true },
          ssh: { configured: true, reachable: true, addr: '10.88.0.1:2222' },
          hostKey: { state: 'unpinned', pinned: [], observed: [] },
        } satisfies WarpgateHealth),
    });
    await mount();

    expect(container.textContent).toContain('Host key not pinned yet.');
    expect(container.querySelector('button[name^="accept-host-key-"]')).toBeNull();
  });

  it('a failed fetch renders the same muted "unknown" treatment, never a crash', async () => {
    stubApi({ health: () => Promise.reject(new Error('network down')) });

    await expect(mount()).resolves.toBeUndefined();

    const badge = chip();
    expect(badge.className).toBe('muted');
    expect(badge.textContent).toBe('unknown');
    expect(container.querySelector('.chip')).toBeNull();
  });

  describe('host-key mismatch', () => {
    const MISMATCH: WarpgateHealth = {
      state: 'degraded',
      api: { configured: true, reachable: true, authenticated: true },
      ssh: { configured: true, reachable: true, addr: '10.88.0.1:2222' },
      hostKey: { state: 'mismatch', pinned: ['SHA256:old'], observed: ['SHA256:new'] },
    };
    const PINNED: WarpgateHealth = {
      state: 'ok',
      api: { configured: true, reachable: true, authenticated: true },
      ssh: { configured: true, reachable: true, addr: '10.88.0.1:2222' },
      hostKey: { state: 'pinned', pinned: ['SHA256:new'], observed: [] },
    };

    it('shows pinned vs observed fingerprints and explains the block', async () => {
      stubApi({ health: () => jsonResponse(200, MISMATCH) });
      await mount();

      expect(container.textContent).toContain('SSH host key changed');
      expect(container.textContent).toContain('Target-bearing spawns are blocked');
      expect(container.textContent).toContain('verify');
      const pinnedCode = container.querySelector('code');
      expect(pinnedCode?.textContent).toBe('SHA256:old');
      expect(container.textContent).toContain('SHA256:new');
    });

    it('confirm → POST the fingerprint → refetch, chip and mismatch UI update', async () => {
      const acceptBodies: { fingerprint: string }[] = [];
      let healthCall = 0;
      const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);
      stubApi({
        health: () => {
          healthCall += 1;
          return jsonResponse(200, healthCall === 1 ? MISMATCH : PINNED);
        },
        accept: (fp) => jsonResponse(200, { state: 'pinned', pinned: [fp], observed: [] }),
        acceptBodies,
      });
      await mount();

      const acceptButton = container.querySelector<HTMLButtonElement>(
        'button[name="accept-host-key-SHA256:new"]',
      );
      if (!acceptButton) throw new Error('missing accept button');
      acceptButton.click();
      await settle();

      expect(confirmSpy).toHaveBeenCalledTimes(1);
      expect(acceptBodies).toEqual([{ fingerprint: 'SHA256:new' }]);
      // The refetch landed: the chip now reads the new, healthy state and the
      // mismatch UI (with its accept button) is gone.
      expect(chip().textContent).toBe('Reachable');
      expect(container.querySelector('button[name^="accept-host-key-"]')).toBeNull();
      expect(container.textContent).not.toContain('SSH host key changed');
    });

    it('does not call accept when the confirm is cancelled', async () => {
      const acceptBodies: { fingerprint: string }[] = [];
      vi.spyOn(window, 'confirm').mockReturnValue(false);
      stubApi({
        health: () => jsonResponse(200, MISMATCH),
        accept: (fp) => jsonResponse(200, { state: 'pinned', pinned: [fp], observed: [] }),
        acceptBodies,
      });
      await mount();

      const acceptButton = container.querySelector<HTMLButtonElement>(
        'button[name="accept-host-key-SHA256:new"]',
      );
      if (!acceptButton) throw new Error('missing accept button');
      acceptButton.click();
      await settle();

      expect(acceptBodies).toEqual([]);
      expect(container.textContent).toContain('SSH host key changed');
    });

    it('a failed accept shows the error inline and leaves the mismatch UI in place', async () => {
      vi.spyOn(window, 'confirm').mockReturnValue(true);
      stubApi({
        health: () => jsonResponse(200, MISMATCH),
        accept: () =>
          jsonResponse(409, { error: 'fingerprint not among the currently observed keys' }),
      });
      await mount();

      const acceptButton = container.querySelector<HTMLButtonElement>(
        'button[name="accept-host-key-SHA256:new"]',
      );
      if (!acceptButton) throw new Error('missing accept button');
      acceptButton.click();
      await settle();

      expect(container.textContent).toContain('fingerprint not among the currently observed keys');
      // Still degraded/mismatch — the failed call never refetched.
      expect(chip().textContent).toBe('Degraded');
      expect(
        container.querySelector<HTMLButtonElement>('button[name="accept-host-key-SHA256:new"]'),
      ).not.toBeNull();
    });
  });
});
