// Global settings › Notifications (issue #198): device-local surfaces that
// live OUTSIDE the settings PATCH — the PWA install re-entry row (issue #142)
// and Web Push registration (issue #98). Guard-free by construction: no
// useSettingsForm, nothing to "save", so no unsaved-changes prompt. Moved
// verbatim from the old Settings monolith.

import { For, Match, Show, Switch, createResource, createSignal } from 'solid-js';
import {
  createPushDevice,
  deletePushDevice,
  errorMessage,
  listPushDevices,
  pushKey,
  testPushDevice,
  type PushDevice,
} from '../../../api';
import EmptyState from '../../../components/EmptyState';
import Banner from '../../../components/Banner';
import SectionCard from '../../../components/SectionCard';
import { install } from '../../../lib/install';

export default function Notifications() {
  return (
    <>
      {/* PWA install re-entry (issue #142) — device-local like notifications,
          so it lives outside the settings PATCH. The sole way back after a
          permanent "Not now": reopens the sheet AppShell mounts and clears the
          dismissed flag. Hidden when running standalone; on iOS always shown
          otherwise; on Android only while a captured beforeinstallprompt event
          is in hand. Placed above Notifications because on iOS installing is
          the prerequisite for enabling push at all. */}
      <Show when={install.settingsRowVisible()}>
        <SectionCard title="Install app" hint="Add lab to your Home Screen for a full-screen app.">
          <button type="button" class="primary" onClick={() => install.openFromSettings()}>
            Install app
          </button>
        </SectionCard>
      </Show>
      {/* Device-local, not part of the saved settings PATCH — its own card. */}
      <NotificationsSection />
    </>
  );
}

// --- Notifications (Web Push, issue #98) ---
//
// Device-local, so it lives outside the settings PATCH: enabling registers
// THIS browser's push subscription with the server and lists every device the
// account has enabled. Support is gated hard — Web Push needs a secure context
// and an installed service worker, neither of which the dev server provides.

type PushEnv =
  | { kind: 'unsupported' }
  | { kind: 'no-sw' }
  | { kind: 'ready'; registration: ServiceWorkerRegistration };

/** What this browser can do with Web Push, resolved once on mount. */
async function detectPushEnv(): Promise<PushEnv> {
  // Synchronous feature checks first: a non-secure context or a browser without
  // the push/notification APIs can never subscribe. (On iOS the whole surface
  // only exists once the PWA is installed to the Home Screen.)
  if (
    !window.isSecureContext ||
    !('serviceWorker' in navigator) ||
    !('PushManager' in window) ||
    !('Notification' in window)
  ) {
    return { kind: 'unsupported' };
  }
  // NEVER await navigator.serviceWorker.ready — it hangs forever when no worker
  // ever registers (the dev server serves no /sw.js). getRegistration() instead
  // resolves to undefined, which is exactly the "not the installed app" signal.
  const registration = await navigator.serviceWorker.getRegistration();
  if (!registration) return { kind: 'no-sw' };
  return { kind: 'ready', registration };
}

/** base64url (the VAPID key form) → the Uint8Array applicationServerKey wants.
 *  Backed by a concrete ArrayBuffer so it satisfies BufferSource. */
function urlBase64ToUint8Array(base64url: string): Uint8Array<ArrayBuffer> {
  const padding = '='.repeat((4 - (base64url.length % 4)) % 4);
  const base64 = (base64url + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64);
  const bytes = new Uint8Array(new ArrayBuffer(raw.length));
  for (let i = 0; i < raw.length; i += 1) bytes[i] = raw.charCodeAt(i);
  return bytes;
}

/** Locale date, tolerant of an unparseable timestamp (mirrors Tokens). */
function onDate(timestamp: string): string {
  const date = new Date(timestamp);
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleDateString();
}

function NotificationsSection() {
  const [env] = createResource(detectPushEnv);
  return (
    <SectionCard title="Notifications">
      <Switch fallback={<small class="hint hint-block">Checking push support…</small>}>
        <Match when={env()?.kind === 'unsupported'}>
          <small class="hint hint-block">
            Push notifications need HTTPS. On iOS 16.4+ you must add lab to the Home Screen and open
            it from there before notifications can be enabled.
          </small>
        </Match>
        <Match when={env()?.kind === 'no-sw'}>
          <small class="hint hint-block">
            Notifications need the installed (production) app — the dev server registers no service
            worker, so there is nothing to subscribe.
          </small>
        </Match>
        <Match when={env()?.kind === 'ready'}>
          <NotificationsReady
            registration={(env() as Extract<PushEnv, { kind: 'ready' }>).registration}
          />
        </Match>
      </Switch>
    </SectionCard>
  );
}

function NotificationsReady(props: { registration: ServiceWorkerRegistration }) {
  const [devices, { refetch }] = createResource(() => listPushDevices());
  // The browser's current local subscription — its endpoint is what marks a
  // listed device as "this device" and what Remove unsubscribes.
  const [localSub, { refetch: refetchLocal }] = createResource(() =>
    props.registration.pushManager.getSubscription(),
  );
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [note, setNote] = createSignal<string | null>(null);

  const localEndpoint = () => localSub()?.endpoint ?? null;

  const enable = async () => {
    setError(null);
    setNote(null);
    // The click is the user gesture: request permission FIRST, before any
    // network await — iOS only shows the prompt when requestPermission() rides
    // the gesture, and any prior await forfeits it.
    let permission: NotificationPermission;
    try {
      permission = await Notification.requestPermission();
    } catch (err) {
      setError(errorMessage(err));
      return;
    }
    if (permission !== 'granted') {
      setError(
        'Notification permission was denied — allow notifications in your browser settings, then try again.',
      );
      return;
    }
    setBusy(true);
    try {
      const sub = await props.registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array(await pushKey()),
      });
      const json = sub.toJSON() as {
        endpoint?: string;
        keys?: { p256dh?: string; auth?: string };
      };
      await createPushDevice({
        endpoint: json.endpoint ?? sub.endpoint,
        keys: { p256dh: json.keys?.p256dh ?? '', auth: json.keys?.auth ?? '' },
      });
      setNote('Notifications enabled on this device.');
      void refetchLocal();
      void refetch();
    } catch (err) {
      // Surfaced verbatim, never swallowed: a failed subscribe (bad VAPID key,
      // blocked push service, HTTPS misconfig) is the self-hoster's only clue.
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (device: PushDevice) => {
    setError(null);
    setNote(null);
    try {
      await deletePushDevice(device.id);
      // When it is THIS browser, drop the local subscription too so the browser
      // side matches the server — a stale local sub keeps receiving pushes.
      if (device.endpoint === localEndpoint()) {
        const sub = await props.registration.pushManager.getSubscription();
        await sub?.unsubscribe();
        void refetchLocal();
      }
      void refetch();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  const sendTest = async (device: PushDevice) => {
    setError(null);
    setNote(null);
    try {
      await testPushDevice(device.id);
      setNote(`Test notification sent to ${device.label}.`);
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <>
      <small class="hint hint-block">
        Get a push when an AFK run needs you or finishes. Enable it once per browser — every device
        you enable is listed below.
      </small>
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Banner message={note()} variant="success" />
      {/* Always offered — even when already granted it re-syncs this browser's
          subscription (the server upsert is idempotent). */}
      <button type="button" class="primary" disabled={busy()} onClick={() => void enable()}>
        {busy() ? 'Enabling…' : 'Enable notifications on this device'}
      </button>
      <Switch>
        <Match when={devices.error !== undefined}>
          <small class="hint hint-block">{errorMessage(devices.error)}</small>
        </Match>
        <Match when={devices()?.length === 0}>
          <EmptyState>No devices yet — enable notifications above.</EmptyState>
        </Match>
        <Match when={devices()}>
          <div class="card-list">
            <For each={devices()}>
              {(device) => (
                <article class="card">
                  <div class="card-head">
                    <span class="card-title">
                      {device.label}
                      <Show when={device.endpoint === localEndpoint()}>
                        <span class="muted"> · this device</span>
                      </Show>
                    </span>
                    <span class="spacer" />
                    <button type="button" class="small" onClick={() => void sendTest(device)}>
                      Send test
                    </button>
                    <button type="button" class="danger small" onClick={() => void remove(device)}>
                      Remove
                    </button>
                  </div>
                  <p class="muted card-sub">Added {onDate(device.created_at)}</p>
                </article>
              )}
            </For>
          </div>
        </Match>
      </Switch>
    </>
  );
}
