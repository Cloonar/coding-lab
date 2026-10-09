// Repo settings, on one page (issue #61): the Settings tab of the repo home.
// All ten sections render together, in the order and groups of
// REPO_SETTINGS_CATEGORIES — Runs (Agents, Runner), Automation (Autoland,
// Schedules), Access (Secrets, Imports), Setup (General, Integrations,
// Branches), then Danger zone — under a sticky row of section chips (below
// 1024px) or beside a sticky outline (from 1024px).
//
// One save rule: the fields of the six form sections edit drafts in the form
// store the frame owns (form.tsx) and wait for the frame's save bar. The
// three sections whose rows are independent objects (Schedules, Secrets,
// Imports) act at once and say so in their heading.
//
// URLs: /repos/:id/settings is the top of the page;
// /repos/:id/settings/:section — every slug of issue #198 — opens it scrolled
// to that section, and an unknown slug stays at the top. `?field=<patch key>`
// (the readiness "Fix" links, a problem found by Save) scrolls that field
// into view and focuses its control. A chip or outline entry scrolls to its
// section and REPLACES the URL with the section's — no history entry per
// section.
//
// The schedule editor's URLs (settings/schedules/new,
// settings/schedules/:scheduleId) are this page too: one route, so the page
// stays mounted — and scrolled where it was — while the editor opens over it
// (sections/ScheduleEditor.tsx) and closes again. Opening a row pushes the
// editor's URL, so Browser Back closes it; closing from inside the editor
// goes back when the editor was opened from this page, else replaces the
// URL with the Schedules section's (a deep link has nowhere to go back to).
//
// The frame owns the page container, RequireAuth, the repo header and the one
// live repo resource; this page reads the repo through the form store, whose
// `saved()` survives a failed refresh. The one-page machinery — the section
// chips and outline, the section frame, the scroll, arrival and deep-link
// logic — is the shared settings core's (components/settings/); this page
// renders the repo's sections into it.

import { useNavigate, useParams } from '@solidjs/router';
import {
  For,
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createResource,
  createSignal,
  on,
  onCleanup,
  type Accessor,
  type JSX,
} from 'solid-js';
import {
  errorMessage,
  listCredentials,
  listRepoSchedules,
  listScheduleFlows,
  runScheduleNow,
  type Repo,
  type Schedule,
} from '../../api';
import Banner from '../../components/Banner';
import Icon from '../../components/Icon';
import SectionNav from '../../components/settings/SectionNav';
import SettingsSection from '../../components/settings/SettingsSection';
import { DESKTOP_QUERY, createSectionScroll } from '../../components/settings/sectionScroll';
import { createLiveResource } from '../../lib/liveResource';
import { createMediaQuery } from '../../lib/media';
import { createModal } from '../../lib/modalStack';
import { resourceValue } from '../../lib/resource';
import { createRowStore, rescueRowFocus, type OwnedRows } from '../../lib/rowStore';
import { useRepoHome } from '../repo-home/context';
import { REPO_SETTINGS_CATEGORIES } from './categories';
import { isRepoFieldKey } from './fields';
import { useRepoSettingsForm } from './form';
import AgentsSection from './sections/Agents';
import AutolandSection from './sections/Autoland';
import BranchesSection from './sections/Branches';
import DangerZone from './sections/Danger';
import GeneralSection from './sections/General';
import ImportsSection from './sections/Imports';
import IntegrationsSection from './sections/Integrations';
import RunnerSection from './sections/Runner';
import ScheduleEditor from './sections/ScheduleEditor';
import SchedulesSection from './sections/Schedules';
import RepoSecretsSection from './sections/Secrets';

export default function RepoSettings() {
  const params = useParams<{ id: string; section?: string; scheduleId?: string }>();
  const navigate = useNavigate();
  const home = useRepoHome();
  const form = useRepoSettingsForm();
  const desktop = createMediaQuery(DESKTOP_QUERY);

  // The provider catalog and the inherited values load with this tab only,
  // and the inherited values follow the repo only while it shows.
  form.catalog.load();
  onCleanup(() => form.catalog.idle());
  const [credentials, { refetch: refetchCredentials }] = createResource(() => listCredentials());

  const base = (): string => `/repos/${params.id}/settings`;
  const section = (): string | undefined => params.section;
  // The schedule editor's URLs: settings/schedules/<id>, or …/new. A second
  // segment under any other section is nothing.
  const editorId = (): string | undefined =>
    section() === 'schedules' ? params.scheduleId : undefined;
  const editorHref = (id: string): string => `${base()}/schedules/${id}`;

  // The page's scroll position, the section in view, and acting on the URL
  // (the shared settings core). The catalog and the inherited values landing
  // add fields (the option bag) and unfold or fold others, and the
  // credentials fill Integrations: a held deep link goes to its target again
  // once they are on the page.
  const scroll = createSectionScroll({
    categories: REPO_SETTINGS_CATEGORIES,
    base,
    section,
    isField: isRepoFieldKey,
    desktop,
    lateContent: () => [form.catalog.providers(), form.inherited(), resourceValue(credentials)],
  });
  let page: HTMLDivElement | undefined;

  // --- schedules and their editor -----------------------------------------------
  // The list is live: repo.changed (a pause, a re-enable, a count moved) and
  // run.changed (a run of a Schedule started or ended) both refetch it. Its
  // rows live in a store reconciled by id (lib/rowStore.ts): a refetch patches
  // a row in place, so the switch or link a keyboard user is on survives it —
  // and the store only ever answers for THIS repo, never with the list the
  // resource still holds for the one the operator just came from.
  const ofThisRepo = (event: { repoID?: string }): boolean =>
    event.repoID === undefined || event.repoID === home.id();
  const [schedulesFetched, { refetch: refetchSchedules }] = createLiveResource(
    () => home.id(),
    async (repoID): Promise<OwnedRows<Schedule>> => ({
      owner: repoID,
      rows: await listRepoSchedules(repoID),
    }),
    [
      { type: 'repo.changed', match: ofThisRepo },
      { type: 'run.changed', match: ofThisRepo },
    ],
  );
  const schedules = createRowStore(() => resourceValue(schedulesFetched), home.id);
  const schedulesError = (): string | null =>
    schedulesFetched.error !== undefined ? errorMessage(schedulesFetched.error) : null;
  const [flows] = createResource(() => listScheduleFlows());
  // A problem to report in the Schedules section once the editor is gone (a
  // Run now from the Saved toast that did not start).
  const [scheduleNotice, setScheduleNotice] = createSignal<string | null>(null);
  // The page can be gone while something it started is still answering (the
  // toast's Run now outlives the Settings tab): what it has to say then goes
  // through the frame.
  let showing = true;
  onCleanup(() => {
    showing = false;
  });

  // Opened from this page: a history entry was pushed, so closing goes back.
  // Arrived at directly (a deep link, a reload): closing replaces the URL
  // with the section's.
  let openedHere = false;
  const openEditor = (id: string): void => {
    openedHere = true;
    navigate(editorHref(id), { scroll: false });
  };
  // Closes the editor of ONE Schedule — and only while that is the editor
  // showing, so nothing that belongs to an editor long gone can close
  // another one, or move a page the operator has opened since.
  const closeEditor = (id: string): void => {
    if (!showing || editorId() !== id) return;
    if (openedHere) {
      openedHere = false;
      navigate(-1);
    } else {
      navigate(`${base()}/schedules`, { replace: true, scroll: false });
    }
  };
  // What to do about the focus once the editor is off the page (its row was
  // deleted, so the link it would return focus to is gone).
  let afterClose: (() => void) | undefined;
  createEffect(
    on(editorId, (id) => {
      if (id !== undefined) return;
      openedHere = false;
      const act = afterClose;
      afterClose = undefined;
      if (act !== undefined) queueMicrotask(act);
    }),
  );
  const scheduleLinks = (): HTMLElement[] =>
    Array.from(page?.querySelectorAll<HTMLElement>('.schedules-list a.schedule-row-main') ?? []);
  const newScheduleLink = (): HTMLElement | null =>
    page?.querySelector<HTMLElement>('.schedules-list a.schedule-new') ?? null;

  const notStarted = (name: string, reason: string): string =>
    `No run started from "${name}": ${reason}`;
  // Run now from the "Saved" toast. The toast outlives the Settings tab, so
  // the repo is the one the toast was shown for, and a refusal is reported
  // where the operator is: in the Schedules section while this page shows,
  // through the frame's toast otherwise — never into a page that is gone.
  const runNow = async (repoID: string, schedule: Schedule): Promise<void> => {
    try {
      await runScheduleNow(repoID, schedule.id);
      home.notify(`Started a run from "${schedule.name}"`);
      if (showing) void refetchSchedules();
    } catch (err) {
      const said = notStarted(schedule.name, errorMessage(err));
      if (showing && home.id() === repoID) setScheduleNotice(said);
      else home.notify(said);
    }
  };

  const body = (slug: string, repo: Accessor<Repo>): JSX.Element => {
    switch (slug) {
      case 'agents':
        return <AgentsSection />;
      case 'runner':
        return <RunnerSection />;
      case 'autoland':
        return <AutolandSection />;
      case 'schedules':
        return (
          <SchedulesSection
            repoId={repo().id}
            rows={schedules.rows}
            loaded={schedules.loaded()}
            error={schedulesError()}
            flows={resourceValue(flows) ?? []}
            editorHref={editorHref}
            onOpen={openEditor}
            // The server's answer first — the row shows it at once, and keeps
            // it should the refetch fail — then the list.
            onChanged={(row) => {
              schedules.patch(row);
              void refetchSchedules();
            }}
            notice={scheduleNotice()}
            onDismissNotice={() => setScheduleNotice(null)}
          />
        );
      case 'secrets':
        return <RepoSecretsSection repoId={repo().id} />;
      case 'imports':
        return <ImportsSection repoId={repo().id} />;
      case 'general':
        return <GeneralSection />;
      case 'integrations':
        return (
          <IntegrationsSection
            credentials={resourceValue(credentials)}
            credentialsError={
              credentials.error !== undefined ? errorMessage(credentials.error) : null
            }
            onRetryCredentials={() => void refetchCredentials()}
          />
        );
      case 'branches':
        return <BranchesSection />;
      case 'danger':
        return <DangerZone repo={repo} />;
      default:
        return null;
    }
  };

  return (
    <div
      class="settings-page"
      ref={(element) => {
        page = element;
        scroll.page(element);
      }}
    >
      {/* Nothing to navigate, and nothing to show, until the repo has loaded
          (the frame reports a failed load in its header). */}
      <Show when={form.saved()}>
        {(repo) => (
          <>
            <SectionNav
              categories={REPO_SETTINGS_CATEGORIES}
              base={base()}
              desktop={desktop()}
              current={scroll.current()}
              onGo={scroll.go}
              chipsRef={scroll.chips}
            />
            <div class="settings-sections">
              {/* What could not be loaded is said, with a way to try again —
                  never papered over with an empty pick or a guessed value. */}
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
              <Show when={form.inheritedError()}>
                {(message) => (
                  <Banner
                    variant="notice"
                    message={
                      form.inherited() === undefined
                        ? `The inherited values could not be loaded. ${message()}`
                        : `The inherited values could not be refreshed, so the ones shown may be out of date. ${message()}`
                    }
                    action={
                      <button type="button" onClick={() => form.retryInherited()}>
                        Try again
                      </button>
                    }
                  />
                )}
              </Show>
              <For each={REPO_SETTINGS_CATEGORIES}>
                {(category) => (
                  <SettingsSection category={category}>{body(category.slug, repo)}</SettingsSection>
                )}
              </For>
            </div>
            {/* The schedule editor, over the page, keyed on the URL: another
                id (or "new") is another editor. */}
            <Show when={editorId()} keyed>
              {(id) => {
                // The Schedule the editor opens on — latched once found, so a
                // refetch while it is open (a run of it ended) neither
                // remounts it nor, should the row have gone, takes it away.
                // A COPY of the row: the editor diffs its drafts against what
                // it opened on, and the row itself keeps changing under it.
                const seed = createMemo<Schedule | null | 'missing' | 'failed' | undefined>(
                  (previous) => {
                    if (previous !== undefined && previous !== 'missing' && previous !== 'failed') {
                      return previous;
                    }
                    if (id === 'new') return null;
                    if (!schedules.loaded()) {
                      return schedulesFetched.error !== undefined ? 'failed' : undefined;
                    }
                    const row = schedules.rows.find((candidate) => candidate.id === id);
                    return row === undefined
                      ? 'missing'
                      : (JSON.parse(JSON.stringify(row)) as Schedule);
                  },
                );
                const close = (): void => closeEditor(id);
                // The repo these answers are about: the toast they show can
                // outlive a move to another one.
                const repoID = repo().id;
                return (
                  <Switch>
                    <Match when={seed() === undefined}>
                      <EditorNotice title="Loading schedule…" onClose={close} />
                    </Match>
                    <Match when={seed() === 'missing'}>
                      <EditorNotice title="Schedule not found" onClose={close}>
                        This schedule no longer exists.
                      </EditorNotice>
                    </Match>
                    <Match when={seed() === 'failed'}>
                      <EditorNotice title="Schedule not loaded" onClose={close}>
                        The schedules could not be loaded. {schedulesError()}
                      </EditorNotice>
                    </Match>
                    <Match when={typeof seed() === 'object'}>
                      <ScheduleEditor
                        repoId={repoID}
                        schedule={seed() as Schedule | null}
                        providers={form.catalog.providers()}
                        // The layer under a Schedule's own agent pick: the
                        // SAVED repo's AFK agent, else the one the server
                        // says the saved repo inherits. Never the answer for
                        // the drafts — a Schedule applies at once, and an
                        // unsaved Agent pick is not the repo's agent yet.
                        afkProviderId={
                          repo().afk_provider_default ??
                          form.inheritedSaved()?.afk_provider_default ??
                          null
                        }
                        flows={resourceValue(flows) ?? []}
                        url={editorHref(id)}
                        onClose={close}
                        // What a request did: reported and applied, never
                        // navigated on — the editor closed itself, or was
                        // gone before the answer came.
                        onSaved={(saved, created) => {
                          if (showing && home.id() === repoID) {
                            if (!created) schedules.patch(saved);
                            void refetchSchedules();
                          }
                          home.notify(`Saved "${saved.name}"`, {
                            action: { label: 'Run now', run: () => void runNow(repoID, saved) },
                          });
                        }}
                        onDeleted={(deleted) => {
                          if (showing && home.id() === repoID) {
                            const at = schedules.rows.findIndex((row) => row.id === deleted.id);
                            // The row goes, and with it the link the editor
                            // hands the focus back to: the row that takes its
                            // place gets it, else the way to add one.
                            const rescue = (): void => {
                              schedules.remove(deleted.id);
                              rescueRowFocus(scheduleLinks(), at, newScheduleLink());
                            };
                            if (editorId() === id) afterClose = rescue;
                            else rescue();
                            void refetchSchedules();
                          }
                          home.notify(`Deleted "${deleted.name}"`);
                        }}
                        onRan={(ran) => {
                          if (showing && home.id() === repoID) void refetchSchedules();
                          home.notify(`Started a run from "${ran.name}"`);
                        }}
                        onLateFailure={(what, name, reason) => {
                          if (what === 'run') home.notify(notStarted(name, reason));
                          else if (what === 'save')
                            home.notify(`"${name}" was not saved: ${reason}`);
                          else home.notify(`"${name}" was not deleted: ${reason}`);
                        }}
                      />
                    </Match>
                  </Switch>
                );
              }}
            </Show>
          </>
        )}
      </Show>
    </div>
  );
}

/**
 * The editor's frame with a message instead of a form: while the Schedule a
 * deep link names is still loading, or when it is gone. As modal as the
 * editor it stands in for — focus moves in and stays in, Escape and the back
 * control close it (also while loading: a list that never answers must not
 * leave the operator behind a scrim), and the page behind does not scroll.
 */
function EditorNotice(props: { title: string; onClose: () => void; children?: JSX.Element }) {
  const desktop = createMediaQuery(DESKTOP_QUERY);
  let panel: HTMLElement | undefined;
  let heading: HTMLHeadingElement | undefined;
  createModal({
    panel: () => panel,
    fallback: () => heading,
    onEscape: (event) => {
      event.preventDefault();
      props.onClose();
    },
  });
  return (
    <>
      <Show when={desktop()}>
        <div class="schedule-editor-scrim" aria-hidden="true" onClick={() => props.onClose()} />
      </Show>
      <section
        ref={panel}
        class="schedule-editor"
        role="dialog"
        aria-modal="true"
        aria-label={props.title}
      >
        <header class="schedule-editor-head">
          <button
            type="button"
            class="icon-btn"
            aria-label="Back to schedules"
            onClick={() => props.onClose()}
          >
            <Icon name="chevron-left" />
          </button>
          <h2 ref={heading} tabIndex={-1}>
            {props.title}
          </h2>
        </header>
        <Show when={props.children}>
          <div class="schedule-editor-body">
            <p class="settings-note">{props.children}</p>
          </div>
        </Show>
      </section>
    </>
  );
}
