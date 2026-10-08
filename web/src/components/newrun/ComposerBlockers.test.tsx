// ComposerBlockers contract (issue #66): nothing at all without blockers on a
// container Runner; one banner per blocker in the order given — an error
// alerts (role="alert"), the warning and notices announce politely
// (role="status", the amber notice palette); each carries its remedy: a
// logged-out agent's Reconnect links to /credentials, a failing tracker's Fix
// links to the settings field it names, a failed clone's Retry calls
// onRetryClone and reads "Retrying…" (ignoring taps) while retrying, and the
// cloning notice has no action. The host-Runner warning follows the blockers
// whenever the Runner is host — also with no blockers at all.

import { MemoryRouter, Route, createMemoryHistory } from '@solidjs/router';
import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { HOST_RUNNER_WARNING, type Blocker } from '../../lib/newRun';
import ComposerBlockers from './ComposerBlockers';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
async function settle(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await flush();
}

const CLONE_FAILED: Blocker = {
  kind: 'clone-failed',
  variant: 'error',
  message: 'fatal: repository not found',
  disablesField: true,
  retryClone: true,
};
const LOGGED_OUT: Blocker = {
  kind: 'logged-out',
  variant: 'error',
  message: 'Claude Code is logged out.',
  disablesField: true,
  fixHref: '/credentials',
  fixLabel: 'Reconnect',
};
const TRACKER: Blocker = {
  kind: 'tracker',
  variant: 'warning',
  message: 'The forge token was refused. A run can still start.',
  disablesField: false,
  fixHref: '/repos/r_lab/settings#forge_credential_id',
  fixLabel: 'Fix',
};
const CLONING: Blocker = {
  kind: 'cloning',
  variant: 'notice',
  message: 'Cloning 42%. Runs can start when the clone finishes.',
  disablesField: true,
};

async function mount(
  blockers: Blocker[],
  hostRunner = false,
): Promise<{ onRetry: ReturnType<typeof vi.fn>; setRetrying: (v: boolean) => void }> {
  container = document.createElement('div');
  document.body.appendChild(container);
  const history = createMemoryHistory();
  history.set({ value: '/' });
  const onRetry = vi.fn();
  const [retrying, setRetrying] = createSignal(false);
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route
          path="/"
          component={() => (
            <ComposerBlockers
              blockers={blockers}
              hostRunner={hostRunner}
              onRetryClone={onRetry}
              retrying={retrying()}
            />
          )}
        />
      </MemoryRouter>
    ),
    container,
  );
  await settle();
  return { onRetry, setRetrying };
}

const banners = () => Array.from(container.querySelectorAll<HTMLElement>('.banner'));
const bannerOf = (kind: string) =>
  container.querySelector<HTMLElement>(`.composer-blocker-${kind}`);
const text = (el: Element | null) => el?.querySelector('.banner-text')?.textContent;

describe('ComposerBlockers', () => {
  it('renders nothing without blockers on a container Runner', async () => {
    await mount([]);
    expect(container.querySelector('.composer-blockers')).toBeNull();
    expect(container.textContent).toBe('');
  });

  it('renders one banner per blocker in the given order', async () => {
    await mount([CLONE_FAILED, LOGGED_OUT, TRACKER, CLONING]);
    expect(banners().map((b) => text(b))).toEqual([
      CLONE_FAILED.message,
      LOGGED_OUT.message,
      TRACKER.message,
      CLONING.message,
    ]);
  });

  it('a failed clone: an error alert whose Retry calls onRetryClone', async () => {
    const { onRetry, setRetrying } = await mount([CLONE_FAILED]);
    const banner = bannerOf('clone-failed')!;
    expect(banner.className).toBe('banner error composer-blocker composer-blocker-clone-failed');
    expect(banner.getAttribute('role')).toBe('alert');
    const retry = banner.querySelector<HTMLButtonElement>('button.composer-blocker-action')!;
    expect(retry.textContent).toBe('Retry');
    retry.click();
    expect(onRetry).toHaveBeenCalledOnce();

    setRetrying(true);
    expect(retry.textContent).toBe('Retrying…');
    expect(retry.getAttribute('aria-disabled')).toBe('true');
    retry.click();
    expect(onRetry).toHaveBeenCalledOnce();
  });

  it('a logged-out agent: an error alert with Reconnect → /credentials', async () => {
    await mount([LOGGED_OUT]);
    const banner = bannerOf('logged-out')!;
    expect(banner.classList.contains('error')).toBe(true);
    expect(banner.getAttribute('role')).toBe('alert');
    const link = banner.querySelector('a.composer-blocker-action')!;
    expect(link.textContent).toBe('Reconnect');
    expect(link.getAttribute('href')).toBe('/credentials');
  });

  it('a failing tracker: an amber status banner with Fix → the named field', async () => {
    await mount([TRACKER]);
    const banner = bannerOf('tracker')!;
    expect(banner.classList.contains('notice')).toBe(true);
    expect(banner.getAttribute('role')).toBe('status');
    const link = banner.querySelector('a.composer-blocker-action')!;
    expect(link.textContent).toBe('Fix');
    expect(link.getAttribute('href')).toBe('/repos/r_lab/settings#forge_credential_id');
  });

  it('cloning: a notice with no action', async () => {
    await mount([CLONING]);
    const banner = bannerOf('cloning')!;
    expect(banner.classList.contains('notice')).toBe(true);
    expect(banner.getAttribute('role')).toBe('status');
    expect(banner.querySelector('.composer-blocker-action')).toBeNull();
  });

  it('a tracker warning without a named fix has no action', async () => {
    await mount([{ ...TRACKER, fixHref: undefined, fixLabel: undefined }]);
    expect(bannerOf('tracker')!.querySelector('.composer-blocker-action')).toBeNull();
  });

  it('the host-Runner warning shows on its own', async () => {
    await mount([], true);
    const banner = bannerOf('host')!;
    expect(text(banner)).toBe(HOST_RUNNER_WARNING);
    expect(banner.classList.contains('notice')).toBe(true);
    expect(banners()).toHaveLength(1);
  });

  it('the host-Runner warning follows the blockers', async () => {
    await mount([LOGGED_OUT], true);
    expect(banners().map((b) => text(b))).toEqual([LOGGED_OUT.message, HOST_RUNNER_WARNING]);
  });
});
