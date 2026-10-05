// The repo home frame (issue #61) at /repos/:id: a back link to the list, the
// header (name, tracker binding chip, incogni, the clone state while not
// ready, the remote), the tabs, and the routed tab below them. Overview,
// Issues (with issue detail, New issue and Labels), CRs (builtin-bound repos
// only) and Settings are nested routes, so every tab and every deep link has
// its own URL and renders inside this frame.
//
// The frame owns RequireAuth, the page container, the ONE live repo resource
// (getRepo + repo.changed for this repo) and the one toast; tabs read both
// through useRepoHome() (./context.ts). It also mounts the repo settings form
// store with its save bar and leave guard (routes/repo-settings/form.tsx), so
// pending settings changes outlive the Settings tab. A failed repo load shows
// its banner in the header slot while the back link and the tabs stay usable,
// and the routed tab still renders — a loaded issue stays readable without
// its repo.
//
// The tabs are navigation links (a <nav> of <a href>, aria-current="page" on
// the active one), not ARIA tabs: each one is a real URL, and the router's
// link interception keeps them in-app. Active rules: Overview only on the
// bare /repos/:id; Issues on issues/* and labels; CRs on crs/*; Settings on
// settings/*. The active tab is underlined and in text colour, never colour
// alone.

import { A, useLocation, useParams, type RouteSectionProps } from '@solidjs/router';
import { Show, type JSX } from 'solid-js';
import { errorMessage, getRepo, type Repo } from '../../api';
import Banner from '../../components/Banner';
import Icon from '../../components/Icon';
import RequireAuth from '../../components/RequireAuth';
import { createToast } from '../../components/Toast';
import { createLiveResource } from '../../lib/liveResource';
import { remoteLabel } from '../../lib/repoName';
import { resourceValue } from '../../lib/resource';
import { useRouteNotice } from '../../lib/routeNotice';
import { RepoSettingsFormProvider } from '../repo-settings/form';
import LeaveGuard from '../repo-settings/LeaveGuard';
import SaveBar from '../repo-settings/SaveBar';
import { RepoHomeContext, type RepoHomeState } from './context';

export { useRepoHome } from './context';
export type { RepoHomeState } from './context';

/** The repo home's tabs, by the first path segment after /repos/:id. */
export type RepoTab = 'overview' | 'issues' | 'crs' | 'settings';

/**
 * Which tab a /repos/:id/… pathname belongs to: the bare repo path is
 * Overview; issues/* and labels are Issues; crs/* is CRs; settings/* is
 * Settings. Anything else under the repo matches no tab (null).
 */
export function activeRepoTab(pathname: string): RepoTab | null {
  // ['', 'repos', '<id>', '<segment>', …]
  const segment = pathname.split('/')[3] ?? '';
  switch (segment) {
    case '':
      return 'overview';
    case 'issues':
    case 'labels':
      return 'issues';
    case 'crs':
      return 'crs';
    case 'settings':
      return 'settings';
    default:
      return null;
  }
}

export default function RepoHome(props: RouteSectionProps) {
  return (
    <RequireAuth>
      <RepoHomeFrame>{props.children}</RepoHomeFrame>
    </RequireAuth>
  );
}

function RepoHomeFrame(props: { children?: JSX.Element }) {
  const params = useParams<{ id: string }>();
  const location = useLocation();

  const [resource, { refetch, mutate }] = createLiveResource(
    () => params.id,
    (id) => getRepo(id),
    [{ type: 'repo.changed', match: (event) => event.repoID === params.id }],
  );
  // Non-throwing, and never stale: the resource keeps the previous repo while
  // the next id loads, so a value for another id reads as "not loaded yet".
  const repo = (): Repo | undefined => {
    const value = resourceValue(resource);
    return value !== undefined && value.id === params.id ? value : undefined;
  };

  const toast = createToast();
  // A page that navigated here after an action (Add repository) hands over a
  // one-line confirmation through router state.
  useRouteNotice((message) => toast.show(message));

  const state: RepoHomeState = {
    id: () => params.id,
    repo,
    error: () => resource.error as unknown,
    loading: () => resource.loading,
    refetch: async () => {
      try {
        return (await refetch()) ?? undefined;
      } catch {
        return undefined;
      }
    },
    mutate: (next) => mutate(next),
    notify: (message, options) => toast.show(message, options),
  };

  const base = () => `/repos/${params.id}`;
  const tab = () => activeRepoTab(location.pathname);
  // CRs are the builtin tracker's counterpart to pull requests. The tab also
  // stays while a CR page is open, so the active tab never vanishes when the
  // repo itself failed to load.
  const showCRs = () => repo()?.tracker_binding === 'builtin' || tab() === 'crs';
  const openIssues = () => repo()?.summary?.open_issues ?? null;

  return (
    <RepoHomeContext.Provider value={state}>
      <main class="page page-wide repo-home">
        <A href="/repos" class="back-link">
          <Icon name="chevron-left" size={20} />
          Repositories
        </A>

        <header class="repo-head">
          <Show when={resource.error !== undefined}>
            <Banner message={errorMessage(resource.error)} />
          </Show>
          <Show when={repo()}>{(r) => <RepoHeader repo={r()} />}</Show>
        </header>

        {/* The repo settings form (issue #61) lives at the frame, around the
            tabs and the routed tab, so pending changes survive switching
            tabs inside this repo. Its save bar follows the tab body and
            stays visible on Overview and Issues while changes are pending;
            its leave guard asks in an in-page dialog before any in-app
            navigation that leaves /repos/:id (same-repo moves pass). */}
        <RepoSettingsFormProvider>
          <nav class="repo-tabs" aria-label="Repository">
            <RepoTabLink href={base()} active={tab() === 'overview'}>
              Overview
            </RepoTabLink>
            <RepoTabLink href={`${base()}/issues`} active={tab() === 'issues'}>
              Issues
              {/* 0 is a real count (a fresh repo); null = not known yet. */}
              <Show when={openIssues() !== null}>
                <span class="count" aria-hidden="true">
                  {openIssues()}
                </span>
                <span class="visually-hidden"> ({openIssues()} open)</span>
              </Show>
            </RepoTabLink>
            <Show when={showCRs()}>
              <RepoTabLink href={`${base()}/crs`} active={tab() === 'crs'}>
                CRs
              </RepoTabLink>
            </Show>
            <RepoTabLink href={`${base()}/settings`} active={tab() === 'settings'}>
              Settings
            </RepoTabLink>
          </nav>

          <div class="repo-home-body">{props.children}</div>
          <SaveBar />
          <LeaveGuard />
        </RepoSettingsFormProvider>
        {toast.Toast()}
      </main>
    </RepoHomeContext.Provider>
  );
}

function RepoHeader(props: { repo: Repo }) {
  return (
    <>
      <div class="repo-head-title">
        <h1>{props.repo.name}</h1>
        <span class="chip">
          {props.repo.tracker_binding === 'forge'
            ? `forge · ${props.repo.forge_kind}`
            : 'builtin tracker'}
        </span>
        <Show when={props.repo.incogni}>
          <span class="chip incogni">incogni</span>
        </Show>
        <Show when={props.repo.clone_status === 'cloning'}>
          <span class="chip status-cloning">cloning</span>
        </Show>
        <Show when={props.repo.clone_status === 'error'}>
          <span class="chip status-error">clone failed</span>
        </Show>
      </div>
      <p class="repo-head-remote mono">{remoteLabel(props.repo.remote_url)}</p>
    </>
  );
}

function RepoTabLink(props: { href: string; active: boolean; children: JSX.Element }) {
  // A plain anchor, not the router's <A>: <A> computes its own aria-current
  // (exact match only) after any we pass, and the Issues tab must also be
  // current on issues/* and labels. The router still intercepts the click.
  return (
    <a
      href={props.href}
      classList={{ 'repo-tab': true, active: props.active }}
      aria-current={props.active ? 'page' : undefined}
    >
      {props.children}
    </a>
  );
}
