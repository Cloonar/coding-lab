// deviceHash contract (issue #160): the device key is the SHA-256 (lowercase
// hex) of the push-subscription endpoint, and currentDeviceHash resolves it for
// the current browser — degrading to null (never throwing) whenever there is
// nothing to suppress (no service worker, no subscription) or a lookup fails.

import { describe, expect, it } from 'vitest';
import { currentDeviceHash, sha256Hex } from './deviceHash';

// A navigator whose serviceWorker.ready → registration.pushManager returns the
// given subscription (or null). Shaped structurally to the injected slice.
function fakeNav(subscription: { endpoint: string } | null) {
  return {
    serviceWorker: {
      ready: Promise.resolve({
        pushManager: {
          getSubscription: () => Promise.resolve(subscription),
        },
      }),
    },
  };
}

describe('sha256Hex', () => {
  it('matches the NIST "abc" vector, lowercase hex', async () => {
    expect(await sha256Hex('abc')).toBe(
      'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
    );
  });

  it('hashes the empty string', async () => {
    expect(await sha256Hex('')).toBe(
      'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    );
  });
});

describe('currentDeviceHash', () => {
  it('hashes the endpoint of the browser subscription', async () => {
    const nav = fakeNav({ endpoint: 'https://push.example/abc' });
    expect(await currentDeviceHash(nav)).toBe(await sha256Hex('https://push.example/abc'));
  });

  it('is null when the environment has no service worker (nothing to suppress)', async () => {
    expect(await currentDeviceHash({})).toBeNull();
  });

  it('is null when there is no push subscription (no push_subscriptions row)', async () => {
    expect(await currentDeviceHash(fakeNav(null))).toBeNull();
  });

  it('degrades to null (never throws) when the lookup rejects', async () => {
    const nav = {
      serviceWorker: {
        ready: Promise.resolve({
          pushManager: {
            getSubscription: () => Promise.reject(new Error('push service unreachable')),
          },
        }),
      },
    };
    await expect(currentDeviceHash(nav)).resolves.toBeNull();
  });
});
