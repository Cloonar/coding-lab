// Global settings › Notifications coverage (issue #198), ported from the old
// Settings.test.tsx: the Web Push block (issue #98) — its hard support gate,
// device list, enable/remove/test flows — and the PWA install re-entry row
// (issue #142). Mounted at /settings/notifications; behavior and strings are
// byte-identical to the monolith.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { PushDevice } from '../../../api';
import { button, container, h, installSettingsHooks, mountAt, settle, waitFor } from '../harness';

installSettingsHooks();

const mountNotifications = () => mountAt('/settings/notifications');

// Notifications block (Web Push, issue #98). The support gate is hard: without
// a secure context, a service worker and the push/notification APIs the block
// only renders a requirements note. When supported+registered it lists the
// account's devices and enabling registers THIS browser's subscription.
describe('Settings notifications (issue #98)', () => {
  let localSubscription: {
    endpoint: string;
    toJSON: () => { endpoint: string; keys: { p256dh: string; auth: string } };
    unsubscribe: () => Promise<boolean>;
  } | null;
  let subscribeCalls: { userVisibleOnly?: boolean; applicationServerKey?: unknown }[];
  let requestPermissionCalls: number;
  let requestPermissionResult: NotificationPermission;
  let unsubscribeCalls: number;

  function makeSub(endpoint: string, p256dh: string, auth: string) {
    return {
      endpoint,
      toJSON: () => ({ endpoint, keys: { p256dh, auth } }),
      unsubscribe: () => {
        unsubscribeCalls += 1;
        return Promise.resolve(true);
      },
    };
  }

  /** Stubs a supported, service-worker-registered browser. */
  function installPushSupport(): void {
    vi.stubGlobal('isSecureContext', true);
    vi.stubGlobal('PushManager', class {});
    vi.stubGlobal('Notification', {
      permission: 'default',
      requestPermission: () => {
        requestPermissionCalls += 1;
        return Promise.resolve(requestPermissionResult);
      },
    });
    const pushManager = {
      getSubscription: () => Promise.resolve(localSubscription),
      subscribe: (opts: { userVisibleOnly?: boolean; applicationServerKey?: unknown }) => {
        subscribeCalls.push(opts);
        localSubscription = makeSub('https://push.example/new', 'p256dh-new', 'auth-new');
        return Promise.resolve(localSubscription);
      },
    };
    Object.defineProperty(navigator, 'serviceWorker', {
      configurable: true,
      value: { getRegistration: () => Promise.resolve({ pushManager }) },
    });
  }

  beforeEach(() => {
    localSubscription = null;
    subscribeCalls = [];
    requestPermissionCalls = 0;
    requestPermissionResult = 'granted';
    unsubscribeCalls = 0;
  });

  afterEach(() => {
    // stubGlobal is reverted by the top-level afterEach; the defineProperty is
    // not, so drop it here to leave navigator clean for the next test.
    if (Object.getOwnPropertyDescriptor(navigator, 'serviceWorker')) {
      delete (navigator as { serviceWorker?: unknown }).serviceWorker;
    }
  });

  function device(id: string, label: string, endpoint: string): PushDevice {
    return { id, label, endpoint, created_at: '2026-07-01T00:00:00.000Z' };
  }

  it('renders the requirements note in an unsupported environment', async () => {
    // No push support installed: jsdom has no secure context / SW / PushManager.
    await mountNotifications();
    await waitFor(
      () => (container.textContent?.includes('Notifications') ? true : null),
      'Notifications block',
    );

    expect(container.textContent).toContain('need HTTPS');
    expect(container.textContent).toContain('Home Screen');
    // The enable button + device list only exist when supported.
    expect(container.textContent).not.toContain('Enable notifications on this device');
  });

  it('lists the account devices and marks the current one "this device"', async () => {
    installPushSupport();
    h.subsOnServer = [
      device('sub_1', 'Chrome on macOS', 'https://push.example/one'),
      device('sub_2', 'Firefox on Linux', 'https://push.example/two'),
    ];
    localSubscription = makeSub('https://push.example/one', 'p', 'a');
    await mountNotifications();
    await waitFor(
      () => (container.textContent?.includes('Chrome on macOS') ? true : null),
      'device list',
    );

    expect(container.textContent).toContain('Chrome on macOS');
    expect(container.textContent).toContain('Firefox on Linux');
    expect(container.textContent).toContain('this device');
    expect(button('Enable notifications on this device')).toBeTruthy();
  });

  it('Remove calls deletePushDevice', async () => {
    installPushSupport();
    h.subsOnServer = [device('sub_1', 'Chrome on macOS', 'https://push.example/one')];
    await mountNotifications();
    await waitFor(
      () => (container.textContent?.includes('Chrome on macOS') ? true : null),
      'device list',
    );

    button('Remove').click();
    await settle();

    expect(h.deletedSubIDs).toEqual(['sub_1']);
  });

  it('Remove also unsubscribes the local browser for this device', async () => {
    installPushSupport();
    h.subsOnServer = [device('sub_1', 'This laptop', 'https://push.example/one')];
    localSubscription = makeSub('https://push.example/one', 'p', 'a');
    await mountNotifications();
    await waitFor(
      () => (container.textContent?.includes('This laptop') ? true : null),
      'device list',
    );

    button('Remove').click();
    await settle();

    expect(h.deletedSubIDs).toEqual(['sub_1']);
    expect(unsubscribeCalls).toBe(1);
  });

  it('Send test calls testPushDevice and confirms', async () => {
    installPushSupport();
    h.subsOnServer = [device('sub_1', 'Chrome on macOS', 'https://push.example/one')];
    await mountNotifications();
    await waitFor(
      () => (container.textContent?.includes('Chrome on macOS') ? true : null),
      'device list',
    );

    button('Send test').click();
    await settle();

    expect(h.testedSubIDs).toEqual(['sub_1']);
    expect(container.textContent).toContain('Test notification sent');
  });

  it('Enable requests permission, subscribes, then registers the device', async () => {
    installPushSupport();
    h.subsOnServer = [];
    await mountNotifications();
    await waitFor(
      () => (container.textContent?.includes('Enable notifications on this device') ? true : null),
      'enable button',
    );

    button('Enable notifications on this device').click();
    await settle();

    // Permission is requested first, then the subscription, then the register.
    expect(requestPermissionCalls).toBe(1);
    expect(subscribeCalls).toHaveLength(1);
    expect(subscribeCalls[0]?.userVisibleOnly).toBe(true);
    expect(subscribeCalls[0]?.applicationServerKey).toBeInstanceOf(Uint8Array);
    expect(h.createdSubBodies).toEqual([
      {
        endpoint: 'https://push.example/new',
        keys: { p256dh: 'p256dh-new', auth: 'auth-new' },
      },
    ]);
  });

  it('a denied permission surfaces an error and never subscribes', async () => {
    installPushSupport();
    requestPermissionResult = 'denied';
    await mountNotifications();
    await waitFor(
      () => (container.textContent?.includes('Enable notifications on this device') ? true : null),
      'enable button',
    );

    button('Enable notifications on this device').click();
    await settle();

    expect(requestPermissionCalls).toBe(1);
    expect(subscribeCalls).toHaveLength(0);
    expect(h.createdSubBodies).toEqual([]);
    expect(container.textContent).toContain('permission was denied');
  });
});

// --- Install app row (issue #142) -------------------------------------------
//
// Drives the REAL module-scope install singleton through the wiring loop:
// AppShell mounts the sheet, the Notifications section owns the re-entry row,
// both read the same signals. The singleton's stashed-event state flows forward
// across tests, so this describe stays LAST in the file and relies on
// declaration order: the hidden-by-default assertion must precede the dispatch.
// (The eligibility truth table itself is unit-tested in lib/install.test.ts.)
describe('Settings install app row (issue #142)', () => {
  function installCard(): HTMLElement | null {
    const headings = Array.from(container.querySelectorAll('section.card > h2'));
    const h2 = headings.find((el) => el.textContent === 'Install app');
    return (h2?.closest('section.card') as HTMLElement | undefined) ?? null;
  }

  it('is hidden without a captured install event (jsdom: non-iOS UA, no event)', async () => {
    await mountNotifications();
    expect(installCard()).toBeNull();
  });

  it('appears when beforeinstallprompt lands; the row opens the sheet; Not now persists', async () => {
    await mountNotifications();

    window.dispatchEvent(new Event('beforeinstallprompt', { cancelable: true }));
    await settle();
    // Row is in hand, but no auto-show: jsdom reports no coarse pointer.
    expect(installCard()).not.toBeNull();
    expect(container.querySelector('[role="dialog"]')).toBeNull();

    button('Install app').click();
    await settle();
    const dialog = container.querySelector('[role="dialog"]');
    expect(dialog?.textContent).toContain('Install lab');

    button('Not now').click();
    await settle();
    expect(container.querySelector('[role="dialog"]')).toBeNull();
    expect(localStorage.getItem('lab.install-dismissed')).toBe('1');
  });
});
