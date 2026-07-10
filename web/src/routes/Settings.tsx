// Global settings (/settings): spawn defaults (model/effort from the
// provider catalog), the global instance cap, AFK budget minutes, reaper and
// scheduler ticks, sweep interval and git author identity. Saved as a
// dirty-fields-only PATCH; int fields validate per field client-side and the
// server's 400 {"error"} lands in the banner. Runtime loops re-read settings
// each tick, so saves apply without a restart.

import { For, Match, Show, Switch, createResource, createSignal } from 'solid-js';
import {
  createPushDevice,
  deletePushDevice,
  errorMessage,
  getSettings,
  listProviders,
  listPushDevices,
  pushKey,
  testPushDevice,
  updateSettings,
  type IntSettingKey,
  type Provider,
  type PushDevice,
  type Settings as SettingsPayload,
  type TextSettingKey,
} from '../api';
import Select, { type SelectOption } from '../components/Select';
import ErrorBanner from '../components/ErrorBanner';
import RequireAuth from '../components/RequireAuth';
import { providerFor } from '../lib/spawn';

interface IntField {
  key: IntSettingKey;
  label: string;
  hint: string;
}

const INT_FIELDS: IntField[] = [
  {
    key: 'max_instances',
    label: 'Max instances',
    hint: 'Global cap on live sessions across all repos (login session exempt).',
  },
  {
    key: 'afk_budget_minutes',
    label: 'AFK budget (minutes)',
    hint: 'Wall-clock budget per AFK run before the reaper times it out.',
  },
  {
    key: 'afk_tick_seconds',
    label: 'Reaper tick (seconds)',
    hint: 'How often AFK runs are classified (success / death / timeout).',
  },
  {
    key: 'afk_schedule_seconds',
    label: 'Scheduler tick (seconds)',
    hint: 'How often auto-enabled repos are considered for a new AFK run.',
  },
  {
    key: 'sweep_interval_minutes',
    label: 'Sweep interval (minutes)',
    hint: 'Throttle for the merged-worktree/branch GC sweep.',
  },
];

export default function Settings() {
  return (
    <RequireAuth>
      <SettingsView />
    </RequireAuth>
  );
}

function SettingsView() {
  const [settings, { refetch }] = createResource(() => getSettings());
  const [providers] = createResource(() => listProviders());

  return (
    <main class="page">
      <div class="section-head">
        <h2>Settings</h2>
      </div>
      <Switch>
        <Match when={settings.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(settings.error)}</span>
          </div>
        </Match>
        <Match when={settings()}>
          {(current) => (
            <SettingsForm
              initial={current()}
              // Global spawn defaults have no repo context; the catalogs
              // resolve against the DRAFTED provider_default / AFK provider
              // (skip-layer over this list), never a hardcoded provider id
              // (issue #51).
              providers={providers() ?? []}
              onSaved={() => void refetch()}
            />
          )}
        </Match>
      </Switch>
      {/* Device-local, not part of the saved settings PATCH — its own card. */}
      <NotificationsSection />
    </main>
  );
}

/** String draft of one settings value ('' for an absent key). */
function seedDraft(initial: SettingsPayload, key: IntSettingKey | TextSettingKey): string {
  const value = initial[key];
  return value === undefined ? '' : String(value);
}

function SettingsForm(props: {
  initial: SettingsPayload;
  providers: Provider[];
  onSaved: () => void;
}) {
  // Drafts seed from the settings snapshot the form mounted with; buildPatch
  // diffs against that snapshot so only edited fields enter the PATCH
  // (settings have no SSE event, so no resync is needed).
  const initial = props.initial;
  const [drafts, setDrafts] = createSignal<Record<string, string>>({
    provider_default: seedDraft(initial, 'provider_default'),
    spawn_model_default: seedDraft(initial, 'spawn_model_default'),
    spawn_effort_default: seedDraft(initial, 'spawn_effort_default'),
    spawn_provider_default_afk: seedDraft(initial, 'spawn_provider_default_afk'),
    spawn_model_default_afk: seedDraft(initial, 'spawn_model_default_afk'),
    spawn_effort_default_afk: seedDraft(initial, 'spawn_effort_default_afk'),
    git_author_name: seedDraft(initial, 'git_author_name'),
    git_author_email: seedDraft(initial, 'git_author_email'),
    afk_prompt: seedDraft(initial, 'afk_prompt'),
    ...Object.fromEntries(INT_FIELDS.map((f) => [f.key, seedDraft(initial, f.key)])),
  });
  const draft = (key: string) => drafts()[key] ?? '';
  const setDraft = (key: string, value: string) => setDrafts({ ...drafts(), [key]: value });

  // Effective providers, resolved LIVE against the DRAFTS (skip-layer,
  // ADR-0030): the base catalogs follow the drafted provider_default, the AFK
  // catalogs the drafted AFK provider (falling through to the base draft) —
  // both re-catalog as the operator flips the agent selects, before save.
  const providerOptions = (): SelectOption[] =>
    props.providers.map((p) => ({ value: p.id, label: p.display_name }));
  const baseProvider = () => providerFor(props.providers, draft('provider_default'));
  const afkProvider = () =>
    providerFor(props.providers, draft('spawn_provider_default_afk'), draft('provider_default'));

  // AFK spawn-option checkboxes (bool provider options). The signal holds only
  // operator overrides; the seed reads the mounted snapshot, so a provider
  // catalog that loads after mount still resolves the right seeded state.
  const [optionDrafts, setOptionDrafts] = createSignal<Record<string, boolean>>({});
  const seedChecked = (key: string) => initial.spawn_options_afk?.[key] === 'true';
  const optionChecked = (key: string) => optionDrafts()[key] ?? seedChecked(key);
  const setOptionChecked = (key: string, value: boolean) =>
    setOptionDrafts({ ...optionDrafts(), [key]: value });
  const boolOptions = () => (afkProvider()?.options ?? []).filter((o) => o.type === 'bool');
  const optionsDirty = () => boolOptions().some((o) => optionChecked(o.key) !== seedChecked(o.key));

  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [note, setNote] = createSignal<string | null>(null);

  const intDirty = (key: IntSettingKey) => draft(key).trim() !== seedDraft(initial, key);

  /** Per-field validation: only dirty int fields can be in error. */
  const intError = (key: IntSettingKey): string | null => {
    if (!intDirty(key)) return null;
    const trimmed = draft(key).trim();
    if (!/^\d+$/.test(trimmed)) return 'Enter a whole number.';
    if (Number(trimmed) < 1) return 'Must be at least 1.';
    return null;
  };

  const textDirty = (key: TextSettingKey) => draft(key).trim() !== seedDraft(initial, key).trim();

  const buildPatch = (): SettingsPayload | null => {
    const patch: SettingsPayload = {};
    for (const key of [
      'provider_default',
      'spawn_model_default',
      'spawn_effort_default',
      'spawn_provider_default_afk',
      'spawn_model_default_afk',
      'spawn_effort_default_afk',
      'git_author_name',
      'git_author_email',
      'afk_prompt',
    ] as TextSettingKey[]) {
      // afk_prompt_default is deliberately absent from this list: it is a
      // read-only, server-injected key (issue #52) that must never be PATCHed.
      if (textDirty(key)) patch[key] = draft(key).trim();
    }
    for (const field of INT_FIELDS) {
      if (!intDirty(field.key)) continue;
      if (intError(field.key) !== null) return null; // blocked by a field error
      patch[field.key] = Number(draft(field.key).trim());
    }
    // Send the full declared bag ("true"/"false" per key) once any option
    // differs from its seed — a partial patch would be ambiguous server-side.
    if (optionsDirty()) {
      patch.spawn_options_afk = Object.fromEntries(
        boolOptions().map((o) => [o.key, optionChecked(o.key) ? 'true' : 'false']),
      );
    }
    return patch;
  };

  const save = async (event: SubmitEvent) => {
    event.preventDefault();
    setError(null);
    setNote(null);
    const patch = buildPatch();
    if (patch === null) {
      setError('Fix the highlighted fields first.');
      return;
    }
    if (Object.keys(patch).length === 0) {
      setNote('Nothing to save.');
      return;
    }
    setBusy(true);
    try {
      await updateSettings(patch);
      setNote('Saved.');
      props.onSaved();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={(e) => void save(e)} class="stack">
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{note()}</span>
        </div>
      </Show>

      <section class="card">
        <h2>Spawn defaults</h2>
        <Select
          skin="field"
          label="Agent"
          name="provider_default"
          // The ROOT of the provider chain — there is nothing to inherit from,
          // so no inherit entry. An unseeded store shows the effective
          // fallback (the first registered provider).
          value={
            draft('provider_default') !== ''
              ? draft('provider_default')
              : (baseProvider()?.id ?? '')
          }
          options={providerOptions()}
          onChange={(value) => setDraft('provider_default', value)}
        />
        <Select
          skin="field"
          label="Model"
          name="spawn_model_default"
          value={draft('spawn_model_default')}
          options={baseProvider()?.models ?? []}
          onChange={(value) => setDraft('spawn_model_default', value)}
        />
        <Select
          skin="field"
          label="Effort"
          name="spawn_effort_default"
          value={draft('spawn_effort_default')}
          options={baseProvider()?.efforts ?? []}
          onChange={(value) => setDraft('spawn_effort_default', value)}
        />
        <Show when={baseProvider() === null}>
          <small class="hint hint-block">
            Provider catalog unavailable — only the stored values are offered.
          </small>
        </Show>
      </section>

      <section class="card">
        <h2>AFK defaults</h2>
        <small class="hint hint-block">
          Used for unattended AFK runs. Leave a field on “Same as default” to inherit the spawn
          default above.
        </small>
        <Select
          skin="field"
          label="AFK agent"
          name="spawn_provider_default_afk"
          value={draft('spawn_provider_default_afk')}
          options={providerOptions()}
          inheritLabel="Same as default"
          onChange={(value) => setDraft('spawn_provider_default_afk', value)}
        />
        <Select
          skin="field"
          label="Model"
          name="spawn_model_default_afk"
          value={draft('spawn_model_default_afk')}
          options={afkProvider()?.models ?? []}
          inheritLabel="Same as default"
          onChange={(value) => setDraft('spawn_model_default_afk', value)}
        />
        <Select
          skin="field"
          label="Effort"
          name="spawn_effort_default_afk"
          value={draft('spawn_effort_default_afk')}
          options={afkProvider()?.efforts ?? []}
          inheritLabel="Same as default"
          onChange={(value) => setDraft('spawn_effort_default_afk', value)}
        />
        <For each={boolOptions()}>
          {(option) => (
            <label class="check">
              <input
                type="checkbox"
                name={`spawn_options_afk.${option.key}`}
                checked={optionChecked(option.key)}
                onChange={(e) => setOptionChecked(option.key, e.currentTarget.checked)}
              />
              <span>{option.label}</span>
            </label>
          )}
        </For>
        <label class="field">
          <span>Seed prompt</span>
          <textarea
            name="afk_prompt"
            rows="10"
            value={draft('afk_prompt')}
            onInput={(e) => setDraft('afk_prompt', e.currentTarget.value)}
            placeholder={initial.afk_prompt_default ?? ''}
          />
        </label>
        <Show when={draft('afk_prompt') === ''}>
          <button
            type="button"
            class="small"
            onClick={() => setDraft('afk_prompt', initial.afk_prompt_default ?? '')}
          >
            Customize
          </button>
        </Show>
        <small class="hint hint-block">
          The run is detected as done only by an open PR on its branch — a prompt that never opens a
          PR burns its budget, counts as a failure, and three failures auto-pause the repo's AFK.
        </small>
      </section>

      <section class="card">
        <h2>Capacity & AFK</h2>
        <For each={INT_FIELDS}>
          {(field) => (
            <label class="field">
              <span>{field.label}</span>
              <input
                type="text"
                inputmode="numeric"
                name={field.key}
                autocomplete="off"
                value={draft(field.key)}
                onInput={(e) => setDraft(field.key, e.currentTarget.value)}
                aria-invalid={intError(field.key) !== null}
              />
              <Show when={intError(field.key)} fallback={<small class="hint">{field.hint}</small>}>
                <small class="field-error" role="alert">
                  {intError(field.key)}
                </small>
              </Show>
            </label>
          )}
        </For>
      </section>

      <section class="card">
        <h2>Git author</h2>
        <label class="field">
          <span>Author name</span>
          <input
            type="text"
            name="git_author_name"
            autocomplete="off"
            value={draft('git_author_name')}
            onInput={(e) => setDraft('git_author_name', e.currentTarget.value)}
          />
          <small class="hint">Used for commits unless a repo overrides it.</small>
        </label>
        <label class="field">
          <span>Author email</span>
          <input
            type="text"
            name="git_author_email"
            autocomplete="off"
            spellcheck={false}
            value={draft('git_author_email')}
            onInput={(e) => setDraft('git_author_email', e.currentTarget.value)}
          />
        </label>
      </section>

      <button type="submit" class="primary wide" disabled={busy()}>
        {busy() ? 'Saving…' : 'Save settings'}
      </button>
    </form>
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
    <section class="card">
      <h2>Notifications</h2>
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
    </section>
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
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{note()}</span>
        </div>
      </Show>
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
          <p class="empty">No devices yet — enable notifications above.</p>
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
