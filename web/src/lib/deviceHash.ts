// The push "device" key for presence-based push suppression (issue #160).
//
// The server suppresses broadcast Web Push to a device identified by the
// SHA-256 (lowercase hex) of its push-subscription endpoint. This resolves that
// same key for the current browser, so presence reports name exactly the device
// whose pushes the server should hold while the app is visible.
//
// Everything here degrades to "notify": a browser with no service worker or no
// push subscription has no push_subscriptions row and thus nothing to suppress
// (→ null), and any failure resolving the subscription is caught to null rather
// than thrown — a hash that can't be computed must never break the app or, by
// omission, silence a notification the operator should still get.

/** SHA-256 of `input`, lowercase hex — the on-the-wire device-key form. */
export async function sha256Hex(input: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(input));
  return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, '0')).join('');
}

/** The slice of `navigator` we read — injectable so tests can fake it. */
interface DeviceNavigator {
  serviceWorker?: {
    ready: Promise<{
      pushManager: { getSubscription(): Promise<{ endpoint: string } | null> };
    }>;
  };
}

/**
 * This browser's push-subscription device key, or null when there is nothing to
 * suppress (no service worker → no push at all; no subscription → no
 * push_subscriptions row). Never throws: any failure resolves to null so a hash
 * hiccup degrades to "notify" rather than breaking presence.
 */
export async function currentDeviceHash(nav: DeviceNavigator = navigator): Promise<string | null> {
  try {
    const sw = nav.serviceWorker;
    if (!sw) return null;
    const registration = await sw.ready;
    const subscription = await registration.pushManager.getSubscription();
    if (!subscription) return null;
    return await sha256Hex(subscription.endpoint);
  } catch {
    // A failed subscription lookup or digest must degrade to "notify", never throw.
    return null;
  }
}
