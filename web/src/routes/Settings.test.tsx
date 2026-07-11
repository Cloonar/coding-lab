// Global settings AFK-defaults section (issue #19):
// - the AFK model/effort selects offer an inherit entry (value "") that maps
//   back to an empty string in the PATCH — empty is allowed for the AFK keys;
// - the provider's bool options render as checkboxes whose toggled state is
//   PATCHed as the full declared spawn_options_afk bag ("true"/"false").

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Provider, PushDevice } from '../api';
import App from '../App';
import Settings from './Settings';

/** claude-code catalog with the ultracode bool option. */
function baseProviders(): Provider[] {
  return [
    {
      id: 'claude-code',
      display_name: 'Claude Code',
      auth: { kind: 'oauth-code' },
      models: [
        { value: 'opus[1m]', label: 'Opus (1M)' },
        { value: 'sonnet', label: 'Sonnet' },
      ],
      efforts: [
        { value: 'high', label: 'high' },
        { value: 'max', label: 'max' },
      ],
      options: [
        {
          key: 'ultracode',
          label: 'Ultracode (multi-agent workflows)',
          type: 'bool',
          default: 'false',
        },
      ],
    },
  ];
}

/** A second provider with its own catalogs (agent-selection tests). */
const CODEX: Provider = {
  id: 'codex',
  display_name: 'Codex',
  auth: { kind: 'api-key' },
  models: [{ value: 'gpt-5-codex', label: 'GPT-5 Codex' }],
  efforts: [{ value: 'medium', label: 'medium' }],
  options: [],
};

/** Stand-in for EventSource so the authenticated App shell can mount. */
class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

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

let settingsOnServer: Record<string, unknown>;
let providersOnServer: Provider[];
let patchBodies: Record<string, unknown>[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

// Web Push (issue #98) server state, exercised by the notifications suite.
let pushKeyValue: string;
let subsOnServer: PushDevice[];
let createdSubBodies: Record<string, unknown>[];
let deletedSubIDs: string[];
let testedSubIDs: string[];

function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === '/api/v1/auth/state' && method === 'GET') {
        return Promise.resolve(
          jsonResponse(200, { setup_required: false, authenticated: true, username: 'dominik' }),
        );
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { providers: providersOnServer }));
      }
      if (url === '/api/v1/settings' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...settingsOnServer }));
      }
      if (url === '/api/v1/settings' && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as Record<string, unknown>;
        patchBodies.push(patch);
        settingsOnServer = { ...settingsOnServer, ...patch };
        return Promise.resolve(jsonResponse(200, { ...settingsOnServer }));
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      // Web Push (issue #98).
      if (url === '/api/v1/push/key' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { public_key: pushKeyValue }));
      }
      if (url === '/api/v1/push/subscriptions' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { subscriptions: subsOnServer }));
      }
      if (url === '/api/v1/push/subscriptions' && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        createdSubBodies.push(body);
        const device: PushDevice = {
          id: 'sub_new',
          endpoint: String(body.endpoint),
          label: 'This browser',
          created_at: '2026-07-10T00:00:00.000Z',
        };
        subsOnServer = [...subsOnServer, device];
        return Promise.resolve(jsonResponse(201, device));
      }
      if (
        url.startsWith('/api/v1/push/subscriptions/') &&
        url.endsWith('/test') &&
        method === 'POST'
      ) {
        const id = url.slice('/api/v1/push/subscriptions/'.length, -'/test'.length);
        testedSubIDs.push(id);
        return Promise.resolve(jsonResponse(202));
      }
      if (url.startsWith('/api/v1/push/subscriptions/') && method === 'DELETE') {
        const id = url.slice('/api/v1/push/subscriptions/'.length);
        deletedSubIDs.push(id);
        subsOnServer = subsOnServer.filter((s) => s.id !== id);
        return Promise.resolve(jsonResponse(204));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

async function waitFor<T>(get: () => T | null, what: string): Promise<T> {
  for (let i = 0; i < 50; i += 1) {
    const value = get();
    if (value !== null) return value;
    await flush();
  }
  throw new Error(`timed out waiting for ${what}`);
}

async function mountSettings(): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/settings' });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/settings" component={Settings} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

function input(name: string): HTMLInputElement {
  const el = container.querySelector<HTMLInputElement>(`input[name="${name}"]`);
  if (!el) throw new Error(`missing input[name="${name}"]`);
  return el;
}

function textarea(name: string): HTMLTextAreaElement {
  const el = container.querySelector<HTMLTextAreaElement>(`textarea[name="${name}"]`);
  if (!el) throw new Error(`missing textarea[name="${name}"]`);
  return el;
}

function button(text: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'));
  const el = buttons.find((b) => b.textContent?.trim() === text);
  if (!el) throw new Error(`missing button ${JSON.stringify(text)}`);
  return el;
}

/** The `section.card` whose `h2` matches, so a field's card placement
 *  (issue #124: Spawn defaults vs. Capacity & AFK) can be asserted. */
function cardByHeading(heading: string): HTMLElement {
  const headings = Array.from(container.querySelectorAll('section.card > h2'));
  const h = headings.find((h) => h.textContent === heading);
  const card = h?.closest('section.card');
  if (!card) throw new Error(`missing card ${JSON.stringify(heading)}`);
  return card as HTMLElement;
}

function typeInto(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/** The unified Select trigger button (field skin) for a form field name. */
function selectTrigger(name: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(`button[name="${name}"]`);
  if (!el) throw new Error(`missing select trigger button[name="${name}"]`);
  return el;
}

/** The label the named Select currently shows on its trigger. */
function selectedLabel(name: string): string {
  return selectTrigger(name).querySelector('.select-field-label')?.textContent ?? '';
}

function optionRows(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('[role="option"]'));
}

/** Opens the named Select and clicks the option with the given label. */
async function chooseFromSelect(name: string, optionLabel: string): Promise<void> {
  selectTrigger(name).click();
  await settle();
  const row = optionRows().find(
    (r) => r.querySelector('.select-option-label')?.textContent === optionLabel,
  );
  if (!row) throw new Error(`missing option ${JSON.stringify(optionLabel)} in ${name}`);
  row.click();
  await settle();
}

function toggleCheckbox(el: HTMLInputElement, checked: boolean): void {
  el.checked = checked;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

function submitForm(): void {
  const form = container.querySelector('form');
  if (!form) throw new Error('missing settings form');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
}

beforeEach(() => {
  settingsOnServer = {};
  providersOnServer = baseProviders();
  patchBodies = [];
  // A valid base64url VAPID key (65 zero-ish bytes) so urlBase64ToUint8Array
  // round-trips without throwing.
  const keyBytes = new Uint8Array(65);
  keyBytes[0] = 4; // uncompressed-point prefix
  let bin = '';
  for (const b of keyBytes) bin += String.fromCharCode(b);
  pushKeyValue = btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  subsOnServer = [];
  createdSubBodies = [];
  deletedSubIDs = [];
  testedSubIDs = [];
  stubApi();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
  vi.unstubAllGlobals();
});

describe('Settings AFK defaults', () => {
  it('offers an inherit entry that seeds selected and the ultracode checkbox', async () => {
    await mountSettings();
    const model = await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    // Unset seeds to the inherit entry (value ''), which titles the trigger…
    expect(selectedLabel('spawn_model_default_afk')).toBe('Same as default');
    // …and sits first (and selected) in the open panel.
    model.click();
    await settle();
    const rows = optionRows();
    expect(rows[0]?.textContent).toBe('Same as default');
    expect(rows[0]?.getAttribute('aria-selected')).toBe('true');
    model.click(); // toggle shut again
    await settle();

    const ultracode = input('spawn_options_afk.ultracode');
    expect(ultracode.type).toBe('checkbox');
    expect(ultracode.checked).toBe(false);
  });

  it('seeds the AFK selects and checkbox from the stored payload', async () => {
    settingsOnServer = {
      spawn_model_default_afk: 'sonnet',
      spawn_options_afk: '{"ultracode":"true"}', // server returns a JSON string
    };
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    expect(selectedLabel('spawn_model_default_afk')).toBe('Sonnet');
    expect(input('spawn_options_afk.ultracode').checked).toBe(true);
  });

  it('PATCHes an AFK model and the full declared option bag', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    await chooseFromSelect('spawn_model_default_afk', 'Sonnet');
    toggleCheckbox(input('spawn_options_afk.ultracode'), true);
    submitForm();
    await settle();

    expect(patchBodies).toEqual([
      { spawn_model_default_afk: 'sonnet', spawn_options_afk: { ultracode: 'true' } },
    ]);
  });

  it('selecting inherit clears a stored AFK model back to an empty string', async () => {
    settingsOnServer = { spawn_model_default_afk: 'opus[1m]' };
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLButtonElement>('button[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );
    expect(selectedLabel('spawn_model_default_afk')).toBe('Opus (1M)');

    await chooseFromSelect('spawn_model_default_afk', 'Same as default'); // the inherit entry
    submitForm();
    await settle();

    // Empty is allowed for the AFK key — it clears back to the base default.
    expect(patchBodies).toEqual([{ spawn_model_default_afk: '' }]);
  });
});

describe('Settings AFK seed prompt (issue #52)', () => {
  const DEFAULT_PROMPT = 'Resolve issue #<N> on branch <BRANCH>, then open a PR.';

  it('renders empty with the built-in default as the placeholder', async () => {
    settingsOnServer = { afk_prompt_default: DEFAULT_PROMPT };
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    const field = textarea('afk_prompt');
    expect(field.value).toBe('');
    expect(field.placeholder).toBe(DEFAULT_PROMPT);
  });

  it('Customize copies the effective default into the textarea for editing', async () => {
    settingsOnServer = { afk_prompt_default: DEFAULT_PROMPT };
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    button('Customize').click();

    expect(textarea('afk_prompt').value).toBe(DEFAULT_PROMPT);
  });

  it('editing the prompt and saving PATCHes afk_prompt', async () => {
    settingsOnServer = { afk_prompt_default: DEFAULT_PROMPT };
    await mountSettings();
    await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );

    typeInto(textarea('afk_prompt'), 'Always branch from main and open a PR when finished.');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([
      { afk_prompt: 'Always branch from main and open a PR when finished.' },
    ]);
  });

  it('clearing a stored prompt back to empty PATCHes afk_prompt as ""', async () => {
    settingsOnServer = { afk_prompt: 'A previously customized prompt.' };
    await mountSettings();
    const field = await waitFor(
      () => container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]'),
      'seed prompt textarea',
    );
    expect(field.value).toBe('A previously customized prompt.');

    typeInto(field, '');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ afk_prompt: '' }]);
  });
});

// Global agent defaults (issue #66 / ADR-0030): provider_default is the ROOT
// of the chain (no inherit entry); spawn_provider_default_afk inherits it
// ("" = same as default); the model/effort catalogs re-resolve live against
// the DRAFTED providers before anything is saved.
describe('Settings agent defaults (issue #66)', () => {
  beforeEach(() => {
    providersOnServer = [...baseProviders(), CODEX];
  });

  it('choosing a base agent PATCHes provider_default', async () => {
    settingsOnServer = { provider_default: 'claude-code' };
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="provider_default"]'), 'agent select');
    expect(selectedLabel('provider_default')).toBe('Claude Code');

    await chooseFromSelect('provider_default', 'Codex');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ provider_default: 'codex' }]);
  });

  it('shows the effective first provider when the store is unseeded — no inherit entry', async () => {
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="provider_default"]'), 'agent select');

    // The root of the chain has nothing to inherit from: the trigger resolves
    // to the first registered provider and the panel offers no inherit row.
    expect(selectedLabel('provider_default')).toBe('Claude Code');
    selectTrigger('provider_default').click();
    await settle();
    expect(optionRows().map((r) => r.textContent)).toEqual(['Claude Code', 'Codex']);
  });

  it('choosing an AFK agent PATCHes spawn_provider_default_afk, "" on inherit', async () => {
    settingsOnServer = { spawn_provider_default_afk: 'codex' };
    await mountSettings();
    await waitFor(
      () => container.querySelector('button[name="spawn_provider_default_afk"]'),
      'AFK agent select',
    );
    expect(selectedLabel('spawn_provider_default_afk')).toBe('Codex');

    await chooseFromSelect('spawn_provider_default_afk', 'Same as default');
    submitForm();
    await settle();

    // Empty is the inherit value for the AFK key — it clears back to the base.
    expect(patchBodies).toEqual([{ spawn_provider_default_afk: '' }]);
  });

  it('re-catalogs the base model select against the drafted provider_default', async () => {
    await mountSettings();
    await waitFor(() => container.querySelector('button[name="provider_default"]'), 'agent select');

    // Before the flip: the claude-code catalog.
    selectTrigger('spawn_model_default').click();
    await settle();
    let labels = optionRows().map((r) => r.textContent);
    expect(labels).toContain('Sonnet');
    expect(labels).not.toContain('GPT-5 Codex');
    selectTrigger('spawn_model_default').click(); // toggle shut
    await settle();

    await chooseFromSelect('provider_default', 'Codex');

    // After the flip (still unsaved): the codex catalog.
    selectTrigger('spawn_model_default').click();
    await settle();
    labels = optionRows().map((r) => r.textContent);
    expect(labels).toContain('GPT-5 Codex');
    expect(labels).not.toContain('Sonnet');
  });
});

// Dialog auto-dismiss timeout (issue #124): a typed-int setting that is NOT
// seeded server-side (absent from GET = never set = blank input, not 0) and
// renders in the Spawn defaults card rather than Capacity & AFK, where every
// other int field lives. 0 ("never") must be an accepted, saveable value.
describe('Settings dialog timeout (issue #124)', () => {
  it('renders in the Spawn defaults card, blank when unset', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector('input[name="dialog_timeout_minutes"]'),
      'dialog timeout field',
    );

    const field = input('dialog_timeout_minutes');
    expect(field.value).toBe('');
    expect(cardByHeading('Spawn defaults').contains(field)).toBe(true);
    expect(cardByHeading('Capacity & AFK').contains(field)).toBe(false);
    expect(container.textContent).toContain('Dialog auto-dismiss (minutes)');
  });

  it('typing 0 and saving PATCHes dialog_timeout_minutes as 0 — 0 is not rejected', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector('input[name="dialog_timeout_minutes"]'),
      'dialog timeout field',
    );

    typeInto(input('dialog_timeout_minutes'), '0');
    submitForm();
    await settle();

    expect(patchBodies).toEqual([{ dialog_timeout_minutes: 0 }]);
  });

  it('a non-numeric value shows the validation error and blocks the save', async () => {
    await mountSettings();
    await waitFor(
      () => container.querySelector('input[name="dialog_timeout_minutes"]'),
      'dialog timeout field',
    );

    typeInto(input('dialog_timeout_minutes'), 'abc');
    submitForm();
    await settle();

    expect(container.textContent).toContain('Enter a whole number.');
    expect(patchBodies).toEqual([]);
  });
});

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
    await mountSettings();
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
    subsOnServer = [
      device('sub_1', 'Chrome on macOS', 'https://push.example/one'),
      device('sub_2', 'Firefox on Linux', 'https://push.example/two'),
    ];
    localSubscription = makeSub('https://push.example/one', 'p', 'a');
    await mountSettings();
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
    subsOnServer = [device('sub_1', 'Chrome on macOS', 'https://push.example/one')];
    await mountSettings();
    await waitFor(
      () => (container.textContent?.includes('Chrome on macOS') ? true : null),
      'device list',
    );

    button('Remove').click();
    await settle();

    expect(deletedSubIDs).toEqual(['sub_1']);
  });

  it('Remove also unsubscribes the local browser for this device', async () => {
    installPushSupport();
    subsOnServer = [device('sub_1', 'This laptop', 'https://push.example/one')];
    localSubscription = makeSub('https://push.example/one', 'p', 'a');
    await mountSettings();
    await waitFor(
      () => (container.textContent?.includes('This laptop') ? true : null),
      'device list',
    );

    button('Remove').click();
    await settle();

    expect(deletedSubIDs).toEqual(['sub_1']);
    expect(unsubscribeCalls).toBe(1);
  });

  it('Send test calls testPushDevice and confirms', async () => {
    installPushSupport();
    subsOnServer = [device('sub_1', 'Chrome on macOS', 'https://push.example/one')];
    await mountSettings();
    await waitFor(
      () => (container.textContent?.includes('Chrome on macOS') ? true : null),
      'device list',
    );

    button('Send test').click();
    await settle();

    expect(testedSubIDs).toEqual(['sub_1']);
    expect(container.textContent).toContain('Test notification sent');
  });

  it('Enable requests permission, subscribes, then registers the device', async () => {
    installPushSupport();
    subsOnServer = [];
    await mountSettings();
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
    expect(createdSubBodies).toEqual([
      {
        endpoint: 'https://push.example/new',
        keys: { p256dh: 'p256dh-new', auth: 'auth-new' },
      },
    ]);
  });

  it('a denied permission surfaces an error and never subscribes', async () => {
    installPushSupport();
    requestPermissionResult = 'denied';
    await mountSettings();
    await waitFor(
      () => (container.textContent?.includes('Enable notifications on this device') ? true : null),
      'enable button',
    );

    button('Enable notifications on this device').click();
    await settle();

    expect(requestPermissionCalls).toBe(1);
    expect(subscribeCalls).toHaveLength(0);
    expect(createdSubBodies).toEqual([]);
    expect(container.textContent).toContain('permission was denied');
  });
});
