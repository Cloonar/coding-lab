// Repo settings area (issue #198): the category-list → detail-subpage IA over
// the monolith's cards, mounted at /repos/:id/settings/:section?. Every
// PATCHable field per the M2 contract stays grouped, saved as a per-section
// dirty-fields-only PATCH; 400 {"error"} surfaces in the section's banner.
// Danger zone deletes the repo (confirm; force checkbox appears after a 409).
// It is the repo home's Settings tab (issue #61): the frame at /repos/:id owns
// the page, RequireAuth, the repo header and the ONE live repo resource, read
// here through useRepoHome(); this area owns the catalogs it needs and
// SettingsLayout owns the mobile-index / desktop-master-detail chrome. The
// schedule editor URLs (settings/schedules/new, settings/schedules/:scheduleId)
// render the Schedules section for now.

import { useMatch, useParams } from '@solidjs/router';
import { Match, Show, Switch, createResource } from 'solid-js';
import { getSettings, listCredentials, listProviders } from '../../api';
import SettingsLayout from '../../components/settings/SettingsLayout';
import { useRepoHome } from '../repo-home/context';
import { REPO_SETTINGS_CATEGORIES } from './categories';
import AgentsSection from './sections/Agents';
import AutolandSection from './sections/Autoland';
import BranchesSection from './sections/Branches';
import DangerZone from './sections/Danger';
import GeneralSection from './sections/General';
import ImportsSection from './sections/Imports';
import IntegrationsSection from './sections/Integrations';
import SchedulesSection from './sections/Schedules';
import RepoSecretsSection from './sections/Secrets';
import RunnerSection from './sections/Runner';

export default function RepoSettings() {
  const params = useParams<{ id: string; section?: string; scheduleId?: string }>();
  const home = useRepoHome();
  const refetch = () => void home.refetch();
  const [credentials] = createResource(() => listCredentials());
  const [providers] = createResource(() => listProviders());
  // Global settings feed the effective-provider chains (provider_default /
  // spawn_provider_default_afk) the Agents/Autoland catalogs resolve against.
  const [settings] = createResource(() => getSettings());

  // The schedule editor's URLs carry no :section; they belong to Schedules.
  const scheduleNew = useMatch(() => `/repos/${params.id}/settings/schedules/new`);
  const section = (): string | undefined =>
    params.section ??
    (params.scheduleId !== undefined || scheduleNew() !== undefined ? 'schedules' : undefined);

  return (
    <SettingsLayout
      base={`/repos/${params.id}/settings`}
      categories={REPO_SETTINGS_CATEGORIES}
      section={section()}
    >
      <Show when={home.repo()}>
        {(r) => (
          <Switch>
            <Match when={section() === 'general'}>
              <GeneralSection repo={r} onSaved={refetch} />
            </Match>
            <Match when={section() === 'integrations'}>
              <IntegrationsSection repo={r} credentials={credentials() ?? []} onSaved={refetch} />
            </Match>
            <Match when={section() === 'branches'}>
              <BranchesSection repo={r} onSaved={refetch} />
            </Match>
            <Match when={section() === 'agents'}>
              <AgentsSection
                repo={r}
                providers={providers() ?? []}
                settings={settings() ?? {}}
                onSaved={refetch}
              />
            </Match>
            <Match when={section() === 'runner'}>
              {/* Gated on the settings fetch (issue #55): the inherit row's
                  "currently …" and the dev image hint read the global
                  defaults, and an empty stand-in would briefly claim "no dev
                  image is configured". */}
              <Show when={settings()}>
                {(s) => <RunnerSection repo={r} settings={s()} onSaved={refetch} />}
              </Show>
            </Match>
            <Match when={section() === 'autoland'}>
              <AutolandSection
                repo={r}
                providers={providers() ?? []}
                settings={settings() ?? {}}
                onSaved={refetch}
              />
            </Match>
            <Match when={section() === 'secrets'}>
              <RepoSecretsSection repoId={r().id} />
            </Match>
            <Match when={section() === 'schedules'}>
              <SchedulesSection
                repo={r}
                providers={providers() ?? []}
                settings={settings() ?? {}}
                onSaved={refetch}
              />
            </Match>
            <Match when={section() === 'imports'}>
              <ImportsSection repoId={r().id} />
            </Match>
            <Match when={section() === 'danger'}>
              <DangerZone repo={r} />
            </Match>
          </Switch>
        )}
      </Show>
    </SettingsLayout>
  );
}
