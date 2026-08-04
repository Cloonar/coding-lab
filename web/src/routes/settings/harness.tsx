// Shared test harness for the global settings area (issue #198). Models the
// old Settings.test.tsx fetch/EventSource stubbing and the runchat/harness.tsx
// precedent: mutable server state hangs off the exported `h` object (ESM
// importers can mutate `h.x` but can never rebind an imported `let`), the
// mount helper registers the NEW /settings/:section? route under App, and the
// query helpers close over the live `container` binding.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, vi } from 'vitest';
import type { MemoryHistory } from '@solidjs/router';
import type { Provider, PushDevice } from '../../api';
import App from '../../App';
import SettingsRoute from './index';

/** claude-code catalog with the ultracode bool option. */
export function baseProviders(): Provider[] {
  return [
    {
      id: 'claude-code',
      display_name: 'Claude Code',
      supports_remote: true,
      auth: { kind: 'oauth-code' },
      models: [
        { value: 'opus[1m]', label: 'Opus (1M)', efforts: [] },
        { value: 'sonnet', label: 'Sonnet', efforts: [] },
      ],
      // The settings pickers consume the provider-level UNION (issue #156) —
      // the per-model efforts above stay irrelevant here.
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
export const CODEX: Provider = {
  id: 'codex',
  display_name: 'Codex',
  supports_remote: false,
  auth: { kind: 'api-key' },
  models: [{ value: 'gpt-5-codex', label: 'GPT-5 Codex', efforts: [] }],
  efforts: [{ value: 'medium', label: 'medium' }],
  options: [],
};

/** Stand-in for EventSource so the authenticated App shell can mount. */
export class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  addEventListener(): void {}
  close(): void {}
}

export function jsonResponse(status: number, body?: unknown) {
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
 * Mutable server/response state the test files poke directly (e.g.
 * `h.settingsOnServer = {...}`). Routed through fields on a single exported
 * object because ESM importers get a read-only view of an imported `let`
 * binding and can never reassign one — but they can freely mutate `h.x`.
 */
export interface SettingsHarnessState {
  settingsOnServer: Record<string, unknown>;
  providersOnServer: Provider[];
  patchBodies: Record<string, unknown>[];
  // Web Push (issue #98) server state, exercised by the notifications suite.
  pushKeyValue: string;
  subsOnServer: PushDevice[];
  createdSubBodies: Record<string, unknown>[];
  deletedSubIDs: string[];
  testedSubIDs: string[];
}
export const h = {} as SettingsHarnessState;

let dispose: (() => void) | undefined;
export let container: HTMLDivElement;
export let history: MemoryHistory;

export function stubApi(): void {
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
        return Promise.resolve(jsonResponse(200, { providers: h.providersOnServer }));
      }
      if (url === '/api/v1/settings' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { ...h.settingsOnServer }));
      }
      if (url === '/api/v1/settings' && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.patchBodies.push(patch);
        h.settingsOnServer = { ...h.settingsOnServer, ...patch };
        return Promise.resolve(jsonResponse(200, { ...h.settingsOnServer }));
      }
      // AppShell mounts the side rail once authenticated; it fetches the
      // instance list for the ACTIVE rail + attention badge.
      if (url === '/api/v1/instances' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { instances: [] }));
      }
      // Web Push (issue #98).
      if (url === '/api/v1/push/key' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { public_key: h.pushKeyValue }));
      }
      if (url === '/api/v1/push/subscriptions' && method === 'GET') {
        return Promise.resolve(jsonResponse(200, { subscriptions: h.subsOnServer }));
      }
      if (url === '/api/v1/push/subscriptions' && method === 'POST') {
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.createdSubBodies.push(body);
        const device: PushDevice = {
          id: 'sub_new',
          endpoint: String(body.endpoint),
          label: 'This browser',
          created_at: '2026-07-10T00:00:00.000Z',
        };
        h.subsOnServer = [...h.subsOnServer, device];
        return Promise.resolve(jsonResponse(201, device));
      }
      if (
        url.startsWith('/api/v1/push/subscriptions/') &&
        url.endsWith('/test') &&
        method === 'POST'
      ) {
        const id = url.slice('/api/v1/push/subscriptions/'.length, -'/test'.length);
        h.testedSubIDs.push(id);
        return Promise.resolve(jsonResponse(202));
      }
      if (url.startsWith('/api/v1/push/subscriptions/') && method === 'DELETE') {
        const id = url.slice('/api/v1/push/subscriptions/'.length);
        h.deletedSubIDs.push(id);
        h.subsOnServer = h.subsOnServer.filter((s) => s.id !== id);
        return Promise.resolve(jsonResponse(204));
      }
      return Promise.reject(new Error(`unexpected fetch: ${method} ${url}`));
    }),
  );
}

/**
 * matchMedia fake for the desktop master-detail tests: a static `matches` for
 * the single (min-width) query SettingsLayout probes. vi.stubGlobal ties its
 * lifetime to vi.unstubAllGlobals() in the shared afterEach; jsdom ships no
 * matchMedia otherwise, which createMediaQuery reads as "mobile" (no match).
 */
export function stubDesktop(matches: boolean): void {
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      onchange: null,
      dispatchEvent: () => false,
    })),
  );
}

export const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

export async function settle(): Promise<void> {
  for (let i = 0; i < 5; i += 1) await flush();
}

export async function waitFor<T>(get: () => T | null, what: string): Promise<T> {
  for (let i = 0; i < 50; i += 1) {
    const value = get();
    if (value !== null) return value;
    await flush();
  }
  throw new Error(`timed out waiting for ${what}`);
}

/** Mount the NEW settings route (plus a neighbor for navigation-guard tests)
 *  under App at the given history path. */
export async function mountAt(path: string): Promise<void> {
  container = document.createElement('div');
  document.body.appendChild(container);
  history = createMemoryHistory();
  history.set({ value: path });
  dispose = render(
    () => (
      <MemoryRouter history={history} root={App}>
        <Route path="/settings/:section?" component={SettingsRoute} />
        <Route path="/other" component={() => <p class="other-page">other</p>} />
        <Route path="*" component={() => null} />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
}

/** Tear down the current mount — for tests that remount within one `it`. */
export function unmount(): void {
  dispose?.();
  dispose = undefined;
  container.remove();
}

export function input(name: string): HTMLInputElement {
  const el = container.querySelector<HTMLInputElement>(`input[name="${name}"]`);
  if (!el) throw new Error(`missing input[name="${name}"]`);
  return el;
}

export function textarea(name: string): HTMLTextAreaElement {
  const el = container.querySelector<HTMLTextAreaElement>(`textarea[name="${name}"]`);
  if (!el) throw new Error(`missing textarea[name="${name}"]`);
  return el;
}

export function button(text: string): HTMLButtonElement {
  const buttons = Array.from(container.querySelectorAll('button'));
  const el = buttons.find((b) => b.textContent?.trim() === text);
  if (!el) throw new Error(`missing button ${JSON.stringify(text)}`);
  return el;
}

/** The `section.card` whose `h2` matches, so a field's card placement
 *  (issue #124: Spawn defaults vs. Capacity & AFK) can be asserted. */
export function cardByHeading(heading: string): HTMLElement {
  const card = cardByHeadingOrNull(heading);
  if (!card) throw new Error(`missing card ${JSON.stringify(heading)}`);
  return card;
}

/** Like cardByHeading but returns null instead of throwing (parity tests). */
export function cardByHeadingOrNull(heading: string): HTMLElement | null {
  // Descendant, not direct child: SectionCard (issue #275) nests the title
  // `<h2>` inside the `.card-head.section-card-head` row.
  const headings = Array.from(container.querySelectorAll('section.card h2'));
  const h2 = headings.find((el) => el.textContent === heading);
  return (h2?.closest('section.card') as HTMLElement | undefined) ?? null;
}

export function typeInto(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/** The unified Select trigger button (field skin) for a form field name. */
export function selectTrigger(name: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(`button[name="${name}"]`);
  if (!el) throw new Error(`missing select trigger button[name="${name}"]`);
  return el;
}

/** The label the named Select currently shows on its trigger. */
export function selectedLabel(name: string): string {
  return selectTrigger(name).querySelector('.select-field-label')?.textContent ?? '';
}

export function optionRows(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('[role="option"]'));
}

/** Opens the named Select and clicks the option with the given label. */
export async function chooseFromSelect(name: string, optionLabel: string): Promise<void> {
  selectTrigger(name).click();
  await settle();
  const row = optionRows().find(
    (r) => r.querySelector('.select-option-label')?.textContent === optionLabel,
  );
  if (!row) throw new Error(`missing option ${JSON.stringify(optionLabel)} in ${name}`);
  row.click();
  await settle();
}

export function toggleCheckbox(el: HTMLInputElement, checked: boolean): void {
  el.checked = checked;
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

export function submitForm(): void {
  const form = container.querySelector('form');
  if (!form) throw new Error('missing settings form');
  form.dispatchEvent(new SubmitEvent('submit', { bubbles: true, cancelable: true }));
}

/**
 * Registers the shared beforeEach/afterEach for a settings suite: reset the
 * server state, stub the API, then tear the mount down and unstub globals.
 * (Called from each test file, mirroring runchat's installChatHooks.)
 */
export function installSettingsHooks(): void {
  beforeEach(() => {
    h.settingsOnServer = {};
    h.providersOnServer = baseProviders();
    h.patchBodies = [];
    // A valid base64url VAPID key (65 zero-ish bytes) so urlBase64ToUint8Array
    // round-trips without throwing.
    const keyBytes = new Uint8Array(65);
    keyBytes[0] = 4; // uncompressed-point prefix
    let bin = '';
    for (const b of keyBytes) bin += String.fromCharCode(b);
    h.pushKeyValue = btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    h.subsOnServer = [];
    h.createdSubBodies = [];
    h.deletedSubIDs = [];
    h.testedSubIDs = [];
    stubApi();
  });

  afterEach(() => {
    dispose?.();
    dispose = undefined;
    container.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });
}
