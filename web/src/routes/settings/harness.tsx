// Shared test harness for the global settings suites (issues #198, #85), the
// repo-settings harness (routes/repo-settings/harness.tsx) applied to the one
// global page: each suite mounts the real /settings/:section? route under App
// (MemoryRouter) — so the page renders with its form store, save bar, leave
// dialog, host-switch dialog and toast — and pokes the mutable `h` state
// object to shape server responses (ESM importers can mutate `h.x` but never
// rebind an imported `let`).
//
// PATCH /settings behaves like handleSettingsPatch where a test can tell: it
// refuses a read-only key (afk_prompt_default, dev_image_fallback) with the
// plain shape, digest-pins a non-blank dev image (so a test sees the response
// applied, not the draft), answers `{settings: …}` like GET, and can be made
// to refuse with or without a `field` (h.patchRefusal), to fail like a
// dropped connection (h.patchOffline) or to hold (h.patchHold).
//
// The page's layout seam (components/settings/scrolling.ts `viewport`) is
// replaced by a fake for every test: jsdom has no layout, so `h.tops` says
// where each section sits and `h.scrolls` records where the page scrolled to.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import type { MemoryHistory } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, vi } from 'vitest';
import type { OneCLIHealth, Provider, PushDevice, Repo, Runner, WarpgateHealth } from '../../api';
import App from '../../App';
import { viewport, type Viewport } from '../../components/settings/scrolling';
import { baseRepo } from '../repo-settings/harness';
import SettingsRoute from './index';

/** A repo whose runner is `runner` (null = inherits the global default) — the
 *  rows the Runner section's inheriting-repo count is computed from. */
export function repoWithRunner(id: string, runner: Runner | null): Repo {
  return { ...baseRepo(), id, name: id, runner };
}

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

/** A second provider with its own catalogs and no remote knob. */
export const CODEX: Provider = {
  id: 'codex',
  display_name: 'Codex',
  supports_remote: false,
  auth: { kind: 'api-key' },
  models: [{ value: 'gpt-5-codex', label: 'GPT-5 Codex', efforts: [] }],
  efforts: [{ value: 'medium', label: 'medium' }],
  options: [],
};

/** The settings a seeded server holds (what store.SeedDefaults writes, roughly). */
export function baseSettings(): Record<string, unknown> {
  return {
    provider_default: 'claude-code',
    spawn_model_default: 'opus[1m]',
    spawn_effort_default: 'high',
    spawn_remote_default: false,
    spawn_provider_default_afk: '',
    spawn_model_default_afk: '',
    spawn_effort_default_afk: '',
    spawn_remote_default_afk: null,
    spawn_model_default_lander: '',
    spawn_effort_default_lander: '',
    afk_prompt: '',
    afk_prompt_default: 'Work on issue <N> on branch <BRANCH>.',
    max_instances: 4,
    afk_budget_minutes: 60,
    afk_tick_seconds: 30,
    afk_schedule_seconds: 60,
    sweep_interval_minutes: 10,
    git_author_name: '',
    git_author_email: '',
    transcript_retention_days: 30,
    merge_delete_head: true,
    runner_default: 'host',
    dev_image_default: '',
    dev_image_fallback: '',
    container_memory: '8g',
    container_pids: 4096,
    container_nofile: 16384,
  };
}

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

/** The digest the fake server pins a dev image ref to. */
export const PINNED = '@sha256:feedface';

export interface SettingsHarnessState {
  settingsOnServer: Record<string, unknown>;
  providersOnServer: Provider[];
  /** Every PATCH /settings body, in order (refused ones included). */
  patchBodies: Record<string, unknown>[];
  /** Makes the next PATCH refuse with this answer (400 unless `status` says
   *  otherwise); `field` names the settings key the way the server does. */
  patchRefusal: { error: string; field?: string; status?: number } | null;
  /** Makes every PATCH fail like a dropped connection (fetch rejects). */
  patchOffline: boolean;
  /** When set, a PATCH answers only once this resolves. */
  patchHold: Promise<void> | null;
  /** Makes GET /settings answer 500 with this message. */
  settingsError: string | null;
  /** Number of GET /settings requests so far (the app shell reads it too). */
  settingsGets: number;
  /** Makes GET /providers answer 500 with this message. */
  providersError: string | null;
  /** GET /repos (issue #55): the repos the Runner section counts. */
  reposOnServer: Repo[];
  /** Makes GET /repos answer 500 — the count must degrade, not block. */
  reposError: boolean;
  /** The fake layout: each element's top edge by id (absent = far below). */
  tops: Record<string, number>;
  /** The fake layout's "the page is scrolled to its end". */
  pageAtEnd: boolean;
  /** Every scroll the page asked for: target (a section id or a field key),
   *  offset, and whether animated. */
  scrolls: { target: string; offset: number; smooth: boolean }[];
  // Web Push (issue #98) server state, exercised by the notifications suite.
  pushKeyValue: string;
  subsOnServer: PushDevice[];
  createdSubBodies: Record<string, unknown>[];
  deletedSubIDs: string[];
  testedSubIDs: string[];
  /** Credential-gateway health (issue #23): General's status card. */
  oneCLIHealthOnServer: OneCLIHealth;
  /** SSH-bastion health (issue #39): General's BastionStatus card. */
  warpgateHealthOnServer: WarpgateHealth;
}
export const h = {} as SettingsHarnessState;

/** What the app shell itself reads on mount (the More tab's logged-out dot). */
export const SHELL_SETTINGS_GETS = 1;

let dispose: (() => void) | undefined;
export let container: HTMLDivElement = document.createElement('div');
export let history: MemoryHistory;

const READ_ONLY = ['dev_image_fallback', 'afk_prompt_default'];

export function stubApi(): void {
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: unknown, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      if (url === '/api/v1/auth/state' && method === 'GET') {
        return jsonResponse(200, {
          setup_required: false,
          authenticated: true,
          username: 'dominik',
        });
      }
      if (url === '/api/v1/providers' && method === 'GET') {
        if (h.providersError !== null) return jsonResponse(500, { error: h.providersError });
        return jsonResponse(200, { providers: h.providersOnServer });
      }
      if (url === '/api/v1/settings' && method === 'GET') {
        h.settingsGets += 1;
        if (h.settingsError !== null) return jsonResponse(500, { error: h.settingsError });
        return jsonResponse(200, { settings: { ...h.settingsOnServer } });
      }
      if (url === '/api/v1/settings' && method === 'PATCH') {
        const patch = JSON.parse(String(init?.body)) as Record<string, unknown>;
        h.patchBodies.push(patch);
        if (h.patchHold !== null) await h.patchHold;
        if (h.patchOffline) throw new TypeError('Failed to fetch');
        const refusal = h.patchRefusal;
        if (refusal !== null) {
          h.patchRefusal = null;
          const { status, ...body } = refusal;
          return jsonResponse(status ?? 400, body);
        }
        for (const key of READ_ONLY) {
          if (key in patch) return jsonResponse(400, { error: `unknown setting "${key}"` });
        }
        const next = { ...h.settingsOnServer, ...patch };
        const ref = patch.dev_image_default;
        if (typeof ref === 'string' && ref.trim() !== '') next.dev_image_default = ref + PINNED;
        h.settingsOnServer = next;
        return jsonResponse(200, { settings: { ...next } });
      }
      if (url === '/api/v1/repos' && method === 'GET') {
        if (h.reposError) return jsonResponse(500, { error: 'repos unavailable' });
        return jsonResponse(200, { repos: h.reposOnServer });
      }
      // AppShell's side rail.
      if (url === '/api/v1/instances' && method === 'GET') {
        return jsonResponse(200, { instances: [] });
      }
      // Web Push (issue #98).
      if (url === '/api/v1/push/key' && method === 'GET') {
        return jsonResponse(200, { public_key: h.pushKeyValue });
      }
      if (url === '/api/v1/push/subscriptions' && method === 'GET') {
        return jsonResponse(200, { subscriptions: h.subsOnServer });
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
        return jsonResponse(201, device);
      }
      if (
        url.startsWith('/api/v1/push/subscriptions/') &&
        url.endsWith('/test') &&
        method === 'POST'
      ) {
        h.testedSubIDs.push(url.slice('/api/v1/push/subscriptions/'.length, -'/test'.length));
        return jsonResponse(202);
      }
      if (url.startsWith('/api/v1/push/subscriptions/') && method === 'DELETE') {
        const id = url.slice('/api/v1/push/subscriptions/'.length);
        h.deletedSubIDs.push(id);
        h.subsOnServer = h.subsOnServer.filter((s) => s.id !== id);
        return jsonResponse(204);
      }
      if (url === '/api/v1/onecli/health' && method === 'GET') {
        return jsonResponse(200, h.oneCLIHealthOnServer);
      }
      if (url === '/api/v1/warpgate/health' && method === 'GET') {
        return jsonResponse(200, h.warpgateHealthOnServer);
      }
      throw new Error(`unexpected fetch: ${method} ${url}`);
    }),
  );
}

// --- media queries --------------------------------------------------------------

/** The desktop breakpoint — byte-equal to the page's (DESKTOP_QUERY). */
export const DESKTOP_QUERY = '(min-width: 1024px)';

let mediaStub: { set: (query: string, matches: boolean) => void } | undefined;

/** A matchMedia fake whose queries can flip live (all start false: mobile). */
export function stubMatchMedia(): { set: (query: string, matches: boolean) => void } {
  if (mediaStub !== undefined) return mediaStub;
  type Entry = { mql: { matches: boolean } & Record<string, unknown>; listeners: Set<() => void> };
  const entries = new Map<string, Entry>();
  const entry = (query: string): Entry => {
    let e = entries.get(query);
    if (e === undefined) {
      const listeners = new Set<() => void>();
      e = {
        listeners,
        mql: {
          matches: false,
          media: query,
          addEventListener: (type: string, listener: () => void) => {
            if (type === 'change') listeners.add(listener);
          },
          removeEventListener: (_type: string, listener: () => void) => {
            listeners.delete(listener);
          },
          addListener: () => {},
          removeListener: () => {},
          onchange: null,
          dispatchEvent: () => false,
        },
      };
      entries.set(query, e);
    }
    return e;
  };
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => entry(query).mql),
  );
  mediaStub = {
    set: (query, matches) => {
      const e = entry(query);
      e.mql.matches = matches;
      for (const listener of e.listeners) listener();
    },
  };
  return mediaStub;
}

/** Flips the desktop breakpoint (installs the matchMedia stub on first use). */
export function setDesktop(matches: boolean): void {
  stubMatchMedia().set(DESKTOP_QUERY, matches);
}

// --- timing ---------------------------------------------------------------------

export const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

/** Lets queued fetches resolve and Solid propagate the results. */
export async function settle(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await flush();
}

export async function waitFor<T>(get: () => T | null, what: string): Promise<T> {
  for (let i = 0; i < 50; i += 1) {
    const value = get();
    if (value !== null) return value;
    await flush();
  }
  throw new Error(`timed out waiting for ${what}`);
}

// --- mounting -------------------------------------------------------------------

/** Mounts the settings route (plus a neighbour to leave to) at `path`. */
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

/** Mounts the page and waits until its sections are there. */
export async function mountPage(path = '/settings'): Promise<void> {
  await mountAt(path);
  await waitFor(() => container.querySelector('#settings-notifications'), 'the settings page');
  await settle();
}

/** Tear down the current mount — for tests that remount within one `it`. */
export function unmount(): void {
  dispose?.();
  dispose = undefined;
  container.remove();
}

// --- controls -------------------------------------------------------------------

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

export function typeInto(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

/** Types into the text field of a settings key. */
export async function typeField(key: string, value: string): Promise<void> {
  typeInto(input(key), value);
  await settle();
}

/** The Select trigger button (field skin) for a settings key. */
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

/** The labels of the named Select's options, in order (opens and closes it). */
export async function optionLabels(name: string): Promise<string[]> {
  selectTrigger(name).click();
  await settle();
  const labels = optionRows().map(
    (row) => row.querySelector('.select-option-label')?.textContent ?? '',
  );
  // The trigger toggles the panel shut again.
  selectTrigger(name).click();
  await settle();
  return labels;
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

/** A switch (role="switch") by its form name. */
export function switchButton(name: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(`button[role="switch"][name="${name}"]`);
  if (!el) throw new Error(`missing switch button[name="${name}"]`);
  return el;
}

export function switchOn(name: string): boolean {
  return switchButton(name).getAttribute('aria-checked') === 'true';
}

/** One segment (role="radio") of a segmented control, by form name and value. */
export function segment(name: string, value: string): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>(
    `button[role="radio"][name="${name}"][value="${value}"]`,
  );
  if (!el) throw new Error(`missing segment button[name="${name}"][value="${value}"]`);
  return el;
}

/** The checked value of a segmented control. */
export function segmentValue(name: string): string | null {
  return (
    container
      .querySelector<HTMLButtonElement>(`button[role="radio"][name="${name}"][aria-checked="true"]`)
      ?.getAttribute('value') ?? null
  );
}

/** The labels of a segmented control's segments, in order. */
export function segmentLabels(name: string): string[] {
  return Array.from(
    container.querySelectorAll<HTMLButtonElement>(`button[role="radio"][name="${name}"]`),
  ).map((b) => b.textContent ?? '');
}

/** Clicks one segment and lets the page follow. */
export async function pick(name: string, value: string): Promise<void> {
  segment(name, value).click();
  await settle();
}

// --- the one page ---------------------------------------------------------------

/** The page-level <section> of a settings section, by slug. */
export function pageSection(slug: string): HTMLElement {
  const el = container.querySelector<HTMLElement>(`section#settings-${slug}`);
  if (!el) throw new Error(`missing settings section "${slug}"`);
  return el;
}

/** A field's wrapper (label, control, problem, hint), by its settings key. */
export function fieldWrapper(key: string): HTMLElement {
  const el = container.querySelector<HTMLElement>(`[data-field="${key}"]`);
  if (!el) throw new Error(`missing field "${key}"`);
  return el;
}

/** The label a field shows (its changed mark's words left out). */
export function fieldLabel(key: string): string {
  const label = fieldWrapper(key).querySelector('.sfield-label label, .sfield-label span[id]');
  const switchLabel = fieldWrapper(key).querySelector('.switch-label');
  const el = label ?? switchLabel;
  return (el?.textContent ?? '').replace(' (unsaved change)', '').trim();
}

/** Whether a field carries the changed mark: the dot's class AND its words. */
export function fieldChanged(key: string): boolean {
  const wrapper = fieldWrapper(key);
  const marked = wrapper.classList.contains('changed');
  const worded = wrapper.textContent?.includes('(unsaved change)') === true;
  if (marked !== worded) throw new Error(`field "${key}": changed dot and words disagree`);
  return marked;
}

/** The problem shown under a field, or null. */
export function fieldError(key: string): string | null {
  return fieldWrapper(key).querySelector('.sfield-error')?.textContent ?? null;
}

/** The hint under a field ('' when it has none). */
export function fieldHint(key: string): string {
  return fieldWrapper(key).querySelector('.sfield-hint')?.textContent ?? '';
}

/** An overridable field's state: 'inherited', 'set here', or null. */
export function fieldState(key: string): string | null {
  return fieldWrapper(key).querySelector('.sfield-state')?.textContent ?? null;
}

/** The "Default: …" text under a field set here (null when it shows none). */
export function fieldDefault(key: string): string | null {
  return fieldWrapper(key).querySelector('.sfield-default small')?.textContent ?? null;
}

/** A field's Reset action, or null while the field is inherited. */
export function resetButton(key: string): HTMLButtonElement | null {
  return fieldWrapper(key).querySelector<HTMLButtonElement>('.sfield-default button');
}

/** The save bar, or null while nothing is pending. */
export function saveBar(): HTMLElement | null {
  return container.querySelector<HTMLElement>('.settings-savebar');
}

function saveBarButton(text: string): HTMLButtonElement {
  const bar = saveBar();
  if (!bar) throw new Error(`no save bar: nothing is pending (wanted its "${text}" button)`);
  const el = Array.from(bar.querySelectorAll('button')).find((b) => b.textContent?.trim() === text);
  if (!el) throw new Error(`missing save bar button ${JSON.stringify(text)}`);
  return el;
}

/** The save bar's headline ("3 unsaved changes" / "1 problem to fix"). */
export function saveBarTitle(): string {
  return saveBar()?.querySelector('strong')?.textContent ?? '';
}

/** The section names the save bar links to, in order. */
export function saveBarSections(): string[] {
  return Array.from(saveBar()?.querySelectorAll('a.settings-savebar-link') ?? []).map(
    (a) => a.textContent ?? '',
  );
}

/** The save bar's error line ('' when it shows none). */
export function saveBarError(): string {
  return saveBar()?.querySelector('.settings-savebar-error')?.textContent ?? '';
}

/** Clicks the save bar's Save and lets the request land. */
export async function save(): Promise<void> {
  saveBarButton('Save').click();
  await settle();
}

/** Clicks the save bar's Discard. */
export async function discard(): Promise<void> {
  saveBarButton('Discard').click();
  await settle();
}

/** The toast text ('' when none shows). */
export function toastText(): string {
  return container.querySelector('.toast')?.textContent ?? '';
}

/** The in-page dialog that is open, or null. */
export function openDialog(): HTMLElement | null {
  return container.querySelector<HTMLElement>(
    '.dialog[role="dialog"], .dialog[role="alertdialog"]',
  );
}

/** A button of the open dialog, by its text. */
export function dialogButton(text: string): HTMLButtonElement {
  const dialog = openDialog();
  if (!dialog) throw new Error(`no dialog open (wanted its "${text}" button)`);
  const el = Array.from(dialog.querySelectorAll('button')).find(
    (b) => b.textContent?.trim() === text,
  );
  if (!el) throw new Error(`missing dialog button ${JSON.stringify(text)}`);
  return el;
}

/** Follows an in-app link the way a click does (the router intercepts it). */
export async function followLink(link: Element | null | undefined): Promise<void> {
  if (!(link instanceof HTMLElement)) throw new Error('missing link to follow');
  link.click();
  await settle();
}

/** Navigates in-app to `/other` through a real link (the leave guard's case). */
export async function leaveToOther(): Promise<void> {
  const link = document.createElement('a');
  link.setAttribute('href', '/other');
  container.appendChild(link);
  await followLink(link);
  link.remove();
}

/** Scrolls the fake page: sets where the sections sit, then fires `scroll`. */
export async function scrollPage(tops: Record<string, number>, atEnd = false): Promise<void> {
  h.tops = tops;
  h.pageAtEnd = atEnd;
  window.dispatchEvent(new Event('scroll'));
  await settle();
}

// --- hooks ----------------------------------------------------------------------

/** The real layout seam, put back after every test. */
const realViewport: Viewport = { ...viewport };

/**
 * Registers the shared beforeEach/afterEach for a settings suite: reset the
 * server state, stub the API and the layout, then tear the mount down and
 * unstub globals.
 */
export function installSettingsHooks(): void {
  beforeEach(() => {
    h.settingsOnServer = baseSettings();
    h.providersOnServer = baseProviders();
    h.patchBodies = [];
    h.patchRefusal = null;
    h.patchOffline = false;
    h.patchHold = null;
    h.settingsError = null;
    h.settingsGets = 0;
    h.providersError = null;
    h.reposOnServer = [];
    h.reposError = false;
    h.tops = {};
    h.pageAtEnd = false;
    h.scrolls = [];
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
    h.oneCLIHealthOnServer = {
      state: 'off',
      api: { configured: false, reachable: false },
      gateway: { configured: false, reachable: false },
    };
    h.warpgateHealthOnServer = {
      state: 'off',
      api: { configured: false, reachable: false },
      ssh: { configured: false, reachable: false },
    };
    stubApi();
    // jsdom has no layout: the page's seam answers from `h` instead.
    Object.assign(viewport, {
      topOf: (element) => h.tops[element.id] ?? 100_000,
      heightOf: () => 0,
      scrollTo: (element, offset, smooth) => {
        const target = element.getAttribute('data-field') ?? element.id;
        h.scrolls.push({ target, offset, smooth });
      },
      atEnd: () => h.pageAtEnd,
    } satisfies Viewport);
    // The router scrolls to the top after a navigation; jsdom only logs
    // "not implemented" for it.
    vi.stubGlobal('scrollTo', vi.fn());
    // A frame is the next macrotask here, so settle() lets once-per-frame
    // work run.
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) =>
      setTimeout(() => callback(performance.now()), 0),
    );
    vi.stubGlobal('cancelAnimationFrame', (handle: number) => clearTimeout(handle));
  });

  afterEach(() => {
    dispose?.();
    dispose = undefined;
    container.remove();
    vi.unstubAllGlobals();
    mediaStub = undefined;
    vi.restoreAllMocks();
    Object.assign(viewport, realViewport);
  });
}
