// Global settings AFK-defaults section (issue #19):
// - the AFK model/effort selects offer an inherit entry (value "") that maps
//   back to an empty string in the PATCH — empty is allowed for the AFK keys;
// - the provider's bool options render as checkboxes whose toggled state is
//   PATCHed as the full declared spawn_options_afk bag ("true"/"false").

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Provider } from '../api';
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

/** Stand-in for EventSource so the authenticated App shell can mount. */
class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

function jsonResponse(status: number, body: unknown) {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(JSON.parse(text) as unknown),
    text: () => Promise.resolve(text),
  };
}

let settingsOnServer: Record<string, unknown>;
let providersOnServer: Provider[];
let patchBodies: Record<string, unknown>[];
let dispose: (() => void) | undefined;
let container: HTMLDivElement;

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

function chooseOption(el: HTMLSelectElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('change', { bubbles: true }));
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
      () => container.querySelector<HTMLSelectElement>('select[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    expect(model.options[0]?.value).toBe('');
    expect(model.options[0]?.textContent).toBe('Same as default');
    expect(model.value).toBe('');

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
    const model = await waitFor(
      () => container.querySelector<HTMLSelectElement>('select[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    expect(model.value).toBe('sonnet');
    expect(input('spawn_options_afk.ultracode').checked).toBe(true);
  });

  it('PATCHes an AFK model and the full declared option bag', async () => {
    await mountSettings();
    const model = await waitFor(
      () => container.querySelector<HTMLSelectElement>('select[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );

    chooseOption(model, 'sonnet');
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
    const model = await waitFor(
      () => container.querySelector<HTMLSelectElement>('select[name="spawn_model_default_afk"]'),
      'AFK defaults section',
    );
    expect(model.value).toBe('opus[1m]');

    chooseOption(model, ''); // the inherit entry
    submitForm();
    await settle();

    // Empty is allowed for the AFK key — it clears back to the base default.
    expect(patchBodies).toEqual([{ spawn_model_default_afk: '' }]);
  });
});
