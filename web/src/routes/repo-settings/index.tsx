// Repo settings area (issue #198): the category-list → detail-subpage IA over
// the monolith's cards, mounted at /repos/:id/settings/:section?. Every
// PATCHable field per the M2 contract stays grouped, saved as a per-section
// dirty-fields-only PATCH; 400 {"error"} surfaces in the section's banner.
// Danger zone deletes the repo (confirm; force checkbox appears after a 409).
// The area root owns the data (live repo + catalogs), Crumbs and RequireAuth;
// SettingsLayout owns the mobile-index / desktop-master-detail chrome.

import { useParams } from '@solidjs/router';
import { Match, Show, Switch, createResource } from 'solid-js';
import { errorMessage, getRepo, getSettings, listCredentials, listProviders } from '../../api';
import Crumbs, { type Crumb } from '../../components/Crumbs';
import RequireAuth from '../../components/RequireAuth';
import SettingsLayout from '../../components/settings/SettingsLayout';
import { createLiveResource } from '../../lib/liveResource';
import { remoteHost } from '../../lib/repoName';
import { resourceValue } from '../../lib/resource';
import { REPO_SETTINGS_CATEGORIES } from './categories';
import AgentsSection from './sections/Agents';
import AutolandSection from './sections/Autoland';
import BranchesSection from './sections/Branches';
import DangerZone from './sections/Danger';
import GeneralSection from './sections/General';
import IntegrationsSection from './sections/Integrations';
import SchedulesSection from './sections/Schedules';
import RepoSecretsSection from './sections/Secrets';
import RunnerSection from './sections/Runner';

export default function RepoSettings() {
  return (
    <RequireAuth>
      <RepoSettingsView />
    </RequireAuth>
  );
}

function RepoSettingsView() {
  const params = useParams<{ id: string; section?: string }>();
  const [repo, { refetch }] = createLiveResource(
    () => getRepo(params.id),
    [{ type: 'repo.changed', match: (event) => event.repoID === params.id }],
  );
  const [credentials] = createResource(() => listCredentials());
  const [providers] = createResource(() => listProviders());
  // Global settings feed the effective-provider chains (provider_default /
  // spawn_provider_default_afk) the Agents/Autoland catalogs resolve against.
  const [settings] = createResource(() => getSettings());

  const active = () => REPO_SETTINGS_CATEGORIES.find((c) => c.slug === params.section);

  // Non-throwing read: the crumb renders above the error <Match>, so a failed
  // getRepo must degrade to the placeholder name, not re-throw and blank it.
  const crumbs = (): Crumb[] => {
    const trail: Crumb[] = [
      { label: 'Repos', href: '/repos' },
      { label: resourceValue(repo)?.name ?? 'Repository', href: `/repos/${params.id}/issues` },
    ];
    const category = active();
    // Bare index (or an unknown slug mid-redirect): Settings is the inert
    // leaf. On a section: Settings links back to the index and the section
    // title is the inert leaf (issue #198 acceptance).
    if (category === undefined) return [...trail, { label: 'Settings' }];
    return [
      ...trail,
      { label: 'Settings', href: `/repos/${params.id}/settings` },
      { label: category.title },
    ];
  };

  return (
    <main class="page">
      <Crumbs segments={crumbs()} />
      <Switch>
        <Match when={repo.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(repo.error)}</span>
          </div>
        </Match>
        <Match when={repo.error === undefined}>
          <SettingsLayout
            base={`/repos/${params.id}/settings`}
            categories={REPO_SETTINGS_CATEGORIES}
            section={params.section}
            indexTitle={
              <Show when={resourceValue(repo)}>
                {(r) => (
                  <>
                    <div class="section-head">
                      <h2>{r().name}</h2>
                      <Show when={r().clone_status !== 'ready'}>
                        <span
                          classList={{
                            chip: true,
                            'status-cloning': r().clone_status === 'cloning',
                            'status-error': r().clone_status === 'error',
                          }}
                        >
                          {r().clone_status === 'cloning' ? 'cloning' : 'clone failed'}
                        </span>
                      </Show>
                    </div>
                    <p class="muted card-sub mono">{remoteHost(r().remote_url)}</p>
                  </>
                )}
              </Show>
            }
          >
            <Show when={repo()}>
              {(r) => (
                <Switch>
                  <Match when={params.section === 'general'}>
                    <GeneralSection repo={r} onSaved={refetch} />
                  </Match>
                  <Match when={params.section === 'integrations'}>
                    <IntegrationsSection
                      repo={r}
                      credentials={credentials() ?? []}
                      onSaved={refetch}
                    />
                  </Match>
                  <Match when={params.section === 'branches'}>
                    <BranchesSection repo={r} onSaved={refetch} />
                  </Match>
                  <Match when={params.section === 'agents'}>
                    <AgentsSection
                      repo={r}
                      providers={providers() ?? []}
                      settings={settings() ?? {}}
                      onSaved={refetch}
                    />
                  </Match>
                  <Match when={params.section === 'runner'}>
                    <RunnerSection repo={r} settings={settings() ?? {}} onSaved={refetch} />
                  </Match>
                  <Match when={params.section === 'autoland'}>
                    <AutolandSection
                      repo={r}
                      providers={providers() ?? []}
                      settings={settings() ?? {}}
                      onSaved={refetch}
                    />
                  </Match>
                  <Match when={params.section === 'secrets'}>
                    <RepoSecretsSection repoId={r().id} />
                  </Match>
                  <Match when={params.section === 'schedules'}>
                    <SchedulesSection
                      repo={r}
                      providers={providers() ?? []}
                      settings={settings() ?? {}}
                      onSaved={refetch}
                    />
                  </Match>
                  <Match when={params.section === 'danger'}>
                    <DangerZone repo={r} />
                  </Match>
                </Switch>
              )}
            </Show>
          </SettingsLayout>
        </Match>
      </Switch>
    </main>
  );
}
