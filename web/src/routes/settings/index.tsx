// Global settings, on one page (issue #85, ADR-0080): /settings renders all
// four sections together, in the order and groups of GLOBAL_SETTINGS_CATEGORIES
// — Runs (Agents, Runner), Setup (General), This device (Notifications) —
// under a sticky row of section chips (below 1024px) or beside a sticky
// outline (from 1024px). Breadcrumb-free, like every top-level page.
//
// One save rule: the fields of Agents, Runner and General edit drafts in the
// page's form store (form.tsx) and wait for the one save bar; Save sends ONE
// PATCH of exactly the changed keys and confirms in the page's toast.
// Notifications acts at once and says so in its heading. A Save that switches
// the runner default to host asks first, in an in-page dialog
// (HostSwitchDialog.tsx); leaving /settings with pending changes asks in the
// shared leave dialog. No browser confirm anywhere.
//
// URLs: /settings is the top of the page; /settings/:section — every slug of
// issue #198 — opens it scrolled to that section, and an unknown slug stays
// at the top. `?field=<settings key>` scrolls that field into view and
// focuses its control. A chip or outline entry scrolls to its section and
// REPLACES the URL with the section's — no history entry per section. Both
// URLs are one route (/settings/:section?), so the page stays mounted, and
// scrolled where it was, as the URL moves between sections.
//
// The one-page machinery — the section chips and outline, the section frame,
// the save bar, the leave dialog, the scroll, arrival and deep-link logic — is
// the shared settings core's (components/settings/), the repo page's too; this
// page renders the global sections into it. What could not be loaded is said
// above the sections with a way to try again: the settings themselves (then
// nothing else renders — every field seeds from them), the provider catalog
// (the agent, model and effort picks are incomplete without it).

import { useParams } from '@solidjs/router';
import { For, Show, type JSX } from 'solid-js';
import Banner from '../../components/Banner';
import RequireAuth from '../../components/RequireAuth';
import SectionHead from '../../components/SectionHead';
import LeaveGuard, { isInsidePath } from '../../components/settings/LeaveGuard';
import SaveBar from '../../components/settings/SaveBar';
import SectionNav from '../../components/settings/SectionNav';
import SettingsSection from '../../components/settings/SettingsSection';
import { DESKTOP_QUERY, createSectionScroll } from '../../components/settings/sectionScroll';
import { createToast } from '../../components/Toast';
import { createMediaQuery } from '../../lib/media';
import { GLOBAL_SETTINGS_CATEGORIES } from './categories';
import { isGlobalFieldKey } from './fields';
import {
  GlobalSettingsFormProvider,
  SETTINGS_BASE,
  createGlobalSettingsForm,
  useGlobalSettingsForm,
} from './form';
import HostSwitchDialog from './HostSwitchDialog';
import AgentsSection from './sections/Agents';
import GeneralSection from './sections/General';
import NotificationsSection from './sections/Notifications';
import RunnerSection from './sections/Runner';

/** Where focus goes after a Save takes the save bar away: the page heading. */
const HEADING = '.global-settings > .section-head h2';

export default function SettingsRoute() {
  return (
    <RequireAuth>
      <SettingsPage />
    </RequireAuth>
  );
}

function SettingsPage() {
  const toast = createToast();
  const form = createGlobalSettingsForm({
    notify: (message, options) => toast.show(message, options),
  });

  return (
    <main class="page page-wide global-settings">
      <SectionHead title="Settings" />
      <GlobalSettingsFormProvider form={form}>
        <SettingsBody />
        <SaveBar categories={GLOBAL_SETTINGS_CATEGORIES} base={SETTINGS_BASE} heading={HEADING} />
        <LeaveGuard inside={(url) => isInsidePath(url, SETTINGS_BASE)} subject="settings" />
        <HostSwitchDialog />
      </GlobalSettingsFormProvider>
      {/* After the save bar: the toast rides above it while both show. */}
      {toast.Toast()}
    </main>
  );
}

function SettingsBody() {
  const params = useParams<{ section?: string }>();
  const form = useGlobalSettingsForm();
  const desktop = createMediaQuery(DESKTOP_QUERY);

  // The page's scroll position, the section in view, and acting on the URL
  // (the shared settings core). The catalog landing adds a field (the AFK
  // option bag): a held deep link goes to its target again once it is there.
  const scroll = createSectionScroll({
    categories: GLOBAL_SETTINGS_CATEGORIES,
    base: () => SETTINGS_BASE,
    section: () => params.section,
    isField: isGlobalFieldKey,
    desktop,
    lateContent: () => form.catalog.providers(),
  });

  const body = (slug: string): JSX.Element => {
    switch (slug) {
      case 'agents':
        return <AgentsSection />;
      case 'runner':
        return <RunnerSection />;
      case 'general':
        return <GeneralSection />;
      case 'notifications':
        return <NotificationsSection />;
      default:
        return null;
    }
  };

  return (
    <>
      <Show when={form.loadError()}>
        {(message) => (
          <Banner
            message={`The settings could not be loaded. ${message()}`}
            action={
              <button type="button" onClick={() => form.retryLoad()}>
                Try again
              </button>
            }
          />
        )}
      </Show>
      <div class="settings-page" ref={(element) => scroll.page(element)}>
        {/* Nothing to navigate, and nothing to show, until the settings have
            loaded: every field seeds from them. */}
        <Show when={form.saved()}>
          <SectionNav
            categories={GLOBAL_SETTINGS_CATEGORIES}
            base={SETTINGS_BASE}
            desktop={desktop()}
            current={scroll.current()}
            onGo={scroll.go}
            chipsRef={scroll.chips}
          />
          <div class="settings-sections">
            <Show when={form.catalog.error()}>
              {(message) => (
                <Banner
                  message={`The agent catalog could not be loaded, so the agent, model and effort picks are incomplete. ${message()}`}
                  action={
                    <button type="button" onClick={() => form.catalog.retry()}>
                      Try again
                    </button>
                  }
                />
              )}
            </Show>
            <For each={GLOBAL_SETTINGS_CATEGORIES}>
              {(category) => (
                <SettingsSection category={category}>{body(category.slug)}</SettingsSection>
              )}
            </For>
          </div>
        </Show>
      </div>
    </>
  );
}
