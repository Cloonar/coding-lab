import { request } from './core';

// --- Web Push devices (issue #98) ---

/**
 * One registered push subscription — a single browser on a single device.
 * `label` is derived server-side from the User-Agent at create time; `endpoint`
 * is the push service URL that also identifies the browser's local
 * subscription, so it is what the UI matches to mark "this device".
 */
export interface PushDevice {
  id: string;
  endpoint: string;
  label: string;
  created_at: string;
}

/**
 * The VAPID application server key: a base64url-encoded 65-byte uncompressed
 * P-256 point. Decoded to a Uint8Array and passed as pushManager.subscribe's
 * applicationServerKey.
 */
export async function pushKey(): Promise<string> {
  const res = await request<{ public_key: string }>('GET', '/push/key');
  return res.public_key;
}

/** Every registered push device — device-level trust, not scoped to a user. */
export async function listPushDevices(): Promise<PushDevice[]> {
  const res = await request<{ subscriptions: PushDevice[] }>('GET', '/push/subscriptions');
  return res.subscriptions;
}

/**
 * Registers the browser's PushSubscription — pass the fields off
 * subscription.toJSON(). The server derives the label from the User-Agent and
 * upserts by endpoint, so re-registering the same browser is idempotent.
 */
export function createPushDevice(sub: {
  endpoint: string;
  keys: { p256dh: string; auth: string };
}): Promise<PushDevice> {
  return request<PushDevice>('POST', '/push/subscriptions', sub);
}

export function deletePushDevice(id: string): Promise<void> {
  return request<void>('DELETE', `/push/subscriptions/${encodeURIComponent(id)}`);
}

/**
 * Presence report for push suppression (issue #160) — fetch path. Tells the
 * server whether this `conn`'s app is visible so broadcast Web Push to `device`
 * (the SHA-256 of the push-subscription endpoint) is suppressed while the tab
 * is in front of the operator. 204; an unknown conn is silently ignored. The
 * synchronous close path uses navigator.sendBeacon instead (no headers). */
export function reportPresence(conn: string, device: string, visible: boolean): Promise<void> {
  return request<void>('POST', '/presence', { conn, device, visible });
}

/**
 * Fires a real test notification through the sender (202). Push delivery is
 * otherwise silent server-side, so this is the self-hoster's way to confirm
 * HTTPS and egress actually reach the push service.
 */
export function testPushDevice(id: string): Promise<void> {
  return request<void>('POST', `/push/subscriptions/${encodeURIComponent(id)}/test`);
}
