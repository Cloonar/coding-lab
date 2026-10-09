// TabBar (issue #76): the four tabs and their roots, the lit tab by path
// (sub-pages keep their section lit), the Runs attention badge (hidden at
// zero) and the More logged-out dot with their accessible names, and the
// re-tap rule — the lit tab from a sub-page returns to the section root and
// scrolls to the top; on the root it only scrolls.

import { MemoryRouter, Route, createMemoryHistory, useLocation } from '@solidjs/router';
import { render } from 'solid-js/web';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TabBar from './TabBar';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;
let history: ReturnType<typeof createMemoryHistory>;
let scrollTo: ReturnType<typeof vi.fn>;

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

function mount(path: string, o: { attention?: number; moreAlert?: boolean } = {}): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  history = createMemoryHistory();
  history.set({ value: path });
  dispose = render(
    () => (
      <MemoryRouter history={history}>
        <Route
          path="*"
          component={() => {
            const location = useLocation();
            return (
              <TabBar
                pathname={location.pathname}
                attention={o.attention ?? 0}
                moreAlert={o.moreAlert ?? false}
              />
            );
          }}
        />
      </MemoryRouter>
    ),
    container,
  );
}

const tab = (href: string) =>
  container.querySelector<HTMLAnchorElement>(`nav[aria-label="Tabs"] a[href="${href}"]`)!;
const current = () =>
  Array.from(container.querySelectorAll('a[aria-current="page"]')).map((a) =>
    a.getAttribute('href'),
  );

beforeEach(() => {
  scrollTo = vi.fn();
  vi.stubGlobal('scrollTo', scrollTo);
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container?.remove();
  vi.unstubAllGlobals();
});

describe('TabBar', () => {
  it('renders Runs · New · Repos · More as links to their roots', () => {
    mount('/');
    const nav = container.querySelector('nav[aria-label="Tabs"]')!;
    const links = Array.from(nav.querySelectorAll('a'));
    expect(links.map((a) => a.getAttribute('href'))).toEqual(['/', '/new', '/repos', '/more']);
    expect(links.map((a) => a.querySelector('.tab-label')?.textContent)).toEqual([
      'Runs',
      'New',
      'Repos',
      'More',
    ]);
    // The New tab is drawn as the accent circle.
    expect(tab('/new').querySelector('.tab-plus svg')).toBeTruthy();
  });

  it.each([
    ['/', '/'],
    ['/history', '/'],
    ['/new', '/new'],
    ['/repos', '/repos'],
    ['/repos/x/issues', '/repos'],
    ['/settings/runner', '/more'],
    ['/credentials', '/more'],
    ['/tokens', '/more'],
    ['/more', '/more'],
  ])('on %s the lit tab is %s', (path, href) => {
    mount(path);
    expect(current()).toEqual([href]);
    expect(tab(href).classList.contains('on')).toBe(true);
  });

  it('lights nothing on a path outside the four sections', () => {
    mount('/somewhere');
    expect(current()).toEqual([]);
  });

  it('shows the attention count on Runs, with an accessible name', () => {
    mount('/', { attention: 2 });
    expect(tab('/').querySelector('.tab-badge')?.textContent).toBe('2');
    expect(tab('/').getAttribute('aria-label')).toBe('Runs, 2 need you');
  });

  it('says "needs" for a single run', () => {
    mount('/', { attention: 1 });
    expect(tab('/').getAttribute('aria-label')).toBe('Runs, 1 needs you');
  });

  it('hides the badge at zero', () => {
    mount('/', { attention: 0 });
    expect(tab('/').querySelector('.tab-badge')).toBeNull();
    expect(tab('/').getAttribute('aria-label')).toBe('Runs');
  });

  it('shows the More dot only when the provider is logged out', () => {
    mount('/', { moreAlert: true });
    expect(tab('/more').querySelector('.tab-dot')).toBeTruthy();
    expect(tab('/more').getAttribute('aria-label')).toBe('More, provider logged out');
    dispose?.();
    container.remove();

    mount('/', { moreAlert: false });
    expect(tab('/more').querySelector('.tab-dot')).toBeNull();
    expect(tab('/more').getAttribute('aria-label')).toBe('More');
  });

  it('re-tapping the lit tab on a sub-page goes to the section root and scrolls up', async () => {
    mount('/repos/x/issues');
    tab('/repos').click();
    await flush();
    expect(history.get()).toBe('/repos');
    expect(scrollTo).toHaveBeenCalledWith({ top: 0 });
    expect(current()).toEqual(['/repos']);
  });

  it('re-tapping Runs on /history goes to / (the Runs root)', async () => {
    mount('/history');
    tab('/').click();
    await flush();
    expect(history.get()).toBe('/');
    expect(scrollTo).toHaveBeenCalledWith({ top: 0 });
  });

  it('re-tapping the lit tab on its root only scrolls', async () => {
    mount('/repos');
    const before = history.get();
    tab('/repos').click();
    await flush();
    expect(history.get()).toBe(before);
    expect(scrollTo).toHaveBeenCalledWith({ top: 0 });
  });

  it('tapping another tab is an ordinary navigation', async () => {
    mount('/repos/x');
    tab('/more').click();
    await flush();
    expect(history.get()).toBe('/more');
    expect(current()).toEqual(['/more']);
  });
});
