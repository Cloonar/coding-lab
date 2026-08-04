// Global settings (/settings/:section?) — the area root (issue #198). It owns
// the page chrome and the two area-level resources (settings + provider
// catalog), fetched ONCE here and threaded into the sections by props; the
// category subpages (General, Agents, Notifications) live under ./sections and
// are switched on the active slug. GLOBAL_SETTINGS_CATEGORIES is the single
// source driving the routes, the mobile index rows and the desktop nav.
// Global settings is deliberately breadcrumb-free (no Crumbs) — spec.

import { Match, Show, Switch, createResource } from 'solid-js';
import type { JSX } from 'solid-js';
import { useParams } from '@solidjs/router';
import { errorMessage, getSettings, listProviders, type Settings } from '../../api';
import Banner from '../../components/Banner';
import RequireAuth from '../../components/RequireAuth';
import SettingsLayout from '../../components/settings/SettingsLayout';
import { GLOBAL_SETTINGS_CATEGORIES } from './categories';
import General from './sections/General';
import Agents from './sections/Agents';
import Notifications from './sections/Notifications';

export default function SettingsRoute() {
  return (
    <RequireAuth>
      <SettingsView />
    </RequireAuth>
  );
}

function SettingsView() {
  const params = useParams<{ section?: string }>();
  // Area-level resources: fetched once here, shared by the sections via props.
  // Settings have no SSE event, so the only refetch is our own onSaved.
  const [settings, { refetch }] = createResource(() => getSettings());
  const [providers] = createResource(() => listProviders());

  // The form sections (general, agents) render inside a KEYED Show on the
  // settings object: a successful save → refetch → NEW settings object →
  // the section REMOUNTS with a fresh seed and reads clean. The monolith never
  // remounted, so after a save its drafts stayed nominally "dirty" against the
  // mount snapshot — harmless then, but now the leave guard would fire
  // spuriously. The keyed remount is the fix. A remount can never clobber
  // in-flight edits in OTHER fields because save() just sent every dirty field
  // of the section, and (settings having no SSE event) our onSaved is the only
  // thing that ever swaps the object.
  const formGate = (render: (current: Settings) => JSX.Element): JSX.Element => (
    <Switch>
      <Match when={settings.error !== undefined}>
        <Banner message={errorMessage(settings.error)} />
      </Match>
      <Match when={settings()}>
        <Show when={settings()} keyed>
          {(current) => render(current)}
        </Show>
      </Match>
    </Switch>
  );

  return (
    <main class="page">
      <SettingsLayout
        base="/settings"
        categories={GLOBAL_SETTINGS_CATEGORIES}
        section={params.section}
        indexTitle={
          <div class="section-head">
            <h2>Settings</h2>
          </div>
        }
      >
        <Switch>
          {/* Device-local: no settings payload, so no keyed gate. */}
          <Match when={params.section === 'notifications'}>
            <Notifications />
          </Match>
          <Match when={params.section === 'general'}>
            {formGate((current) => (
              <General initial={current} onSaved={() => void refetch()} />
            ))}
          </Match>
          <Match when={params.section === 'agents'}>
            {formGate((current) => (
              <Agents
                initial={current}
                // Global spawn defaults have no repo context; the catalogs
                // resolve against the DRAFTED provider_default / AFK provider
                // (skip-layer over this list), never a hardcoded provider id
                // (issue #51).
                providers={providers() ?? []}
                onSaved={() => void refetch()}
              />
            ))}
          </Match>
        </Switch>
      </SettingsLayout>
    </main>
  );
}
