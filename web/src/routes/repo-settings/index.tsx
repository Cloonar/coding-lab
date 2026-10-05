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
// `saved()` survives a failed refresh.

import { useIsRouting, useLocation, useNavigate, useParams } from '@solidjs/router';
import {
  ErrorBoundary,
  For,
  Match,
  Show,
  Switch,
  createComputed,
  createEffect,
  createMemo,
  createResource,
  createSignal,
  on,
  onCleanup,
  onMount,
  untrack,
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
import { createLiveResource } from '../../lib/liveResource';
import { createMediaQuery } from '../../lib/media';
import { resourceValue } from '../../lib/resource';
import { sectionInView } from '../../lib/sectionSpy';
import { useRepoHome } from '../repo-home/context';
import {
  REPO_SETTINGS_CATEGORIES,
  repoSettingsCategory,
  sectionElementId,
  type RepoSettingsCategory,
} from './categories';
import { focusFieldControl } from './Field';
import { isRepoFieldKey, repoField, type RepoFieldKey } from './fields';
import { useRepoSettingsForm, type RevealTarget } from './form';
import { viewport } from './scrolling';
import SectionNav from './SectionNav';
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

/** The app shell's layout breakpoint (AppShell DESKTOP_MIN_PX). */
const DESKTOP_QUERY = '(min-width: 1024px)';
/** Air between whatever is stuck to the top of the viewport and a scroll target. */
const TARGET_GAP = 12;
/** Extra room above a field, so its label is not flush under the chips. */
const FIELD_HEADROOM = 36;
/** The outline does not cover the page, so desktop targets only clear the page padding. */
const DESKTOP_OFFSET = 24;
/** How long a deep link keeps its target in place while late content lands above it. */
const HOLD_MS = 3000;
/** How long the outline around a revealed field stays. */
const FLASH_MS = 1800;

/** What the operator does to scroll or to take over the page. */
const SCROLL_INPUTS = ['wheel', 'touchstart', 'pointerdown', 'keydown'] as const;

const FIRST_SECTION = REPO_SETTINGS_CATEGORIES[0]!.slug;

export default function RepoSettings() {
  const params = useParams<{ id: string; section?: string; scheduleId?: string }>();
  const location = useLocation();
  const navigate = useNavigate();
  const isRouting = useIsRouting();
  const home = useRepoHome();
  const form = useRepoSettingsForm();
  const desktop = createMediaQuery(DESKTOP_QUERY);

  // The provider catalog and the inherited values load with this tab only,
  // and the inherited values follow the repo only while it shows.
  form.catalog.load();
  onCleanup(() => form.catalog.idle());
  const [credentials] = createResource(() => listCredentials());

  const base = (): string => `/repos/${params.id}/settings`;
  const section = (): string | undefined => params.section;
  // The schedule editor's URLs: settings/schedules/<id>, or …/new. A second
  // segment under any other section is nothing.
  const editorId = (): string | undefined =>
    section() === 'schedules' ? params.scheduleId : undefined;
  const editorHref = (id: string): string => `${base()}/schedules/${id}`;

  const fieldParam = (): RepoFieldKey | undefined => {
    const field = location.query.field;
    return typeof field === 'string' && isRepoFieldKey(field) ? field : undefined;
  };
  // Where the URL points: a field (its own section wins over a mismatched
  // :section), else a known section, else nowhere — the top of the page.
  const urlTarget = (): RevealTarget | undefined => {
    const field = fieldParam();
    if (field !== undefined) return { section: repoField(field).section, field };
    const slug = repoSettingsCategory(section())?.slug;
    return slug !== undefined ? { section: slug } : undefined;
  };
  // What a URL means to the page's scroll position: its section and field.
  // The editor's segment is left out on purpose — opening or closing the
  // editor is not an arrival, and must not move the page under it.
  const arrivalKey = (slug: string | undefined, field: string | undefined): string =>
    `${repoSettingsCategory(slug)?.slug ?? ''}|${field ?? ''}`;

  // A section that folds fields away unfolds the one the URL points at.
  // A computed, so it has happened before the page looks for the field.
  createComputed(() => form.pointAt(urlTarget()?.field));

  let page: HTMLDivElement | undefined;
  let chips: HTMLElement | undefined;
  const sectionElement = (slug: string): HTMLElement | null =>
    document.getElementById(sectionElementId(slug));

  // --- what sticks to the top ---------------------------------------------------
  // Below 1024px the app shell's top strip is stuck to the viewport and the
  // chips stick right under it; a scroll target has to clear both.
  const [stripHeight, setStripHeight] = createSignal(0);
  const measureStrip = (): void => {
    const strip = document.querySelector('.shell-topstrip');
    setStripHeight(strip !== null ? viewport.heightOf(strip) : 0);
  };
  const stuckOffset = (): number =>
    desktop()
      ? DESKTOP_OFFSET
      : stripHeight() + (chips !== undefined ? viewport.heightOf(chips) : 0) + TARGET_GAP;

  // --- the section in view ------------------------------------------------------
  const [current, setCurrent] = createSignal(FIRST_SECTION);
  // The section the page was last SENT to (a chip, a deep link, a problem
  // found by Save). It stays marked until the operator scrolls on their own:
  // a section near the end of the page cannot always climb to the line, and
  // the entry just tapped must not hand its mark to a neighbour.
  let sentTo: string | undefined;
  const updateCurrent = (): void => {
    if (sentTo !== undefined) {
      setCurrent(sentTo);
      return;
    }
    const tops = REPO_SETTINGS_CATEGORIES.flatMap((category) => {
      const element = sectionElement(category.slug);
      return element !== null ? [{ id: category.slug, top: viewport.topOf(element) }] : [];
    });
    setCurrent(sectionInView(tops, stuckOffset() + 8, viewport.atEnd()) ?? FIRST_SECTION);
  };

  // --- going to a section or a field --------------------------------------------
  // While a deep link is being held, this re-applies it.
  let held: ((smooth: boolean) => void) | undefined;
  let stopHold: (() => void) | undefined;
  let flashTimer: ReturnType<typeof setTimeout> | undefined;
  const flash = (wrapper: HTMLElement): void => {
    clearTimeout(flashTimer);
    page?.querySelectorAll('.sfield.flash').forEach((el) => el.classList.remove('flash'));
    wrapper.classList.add('flash');
    flashTimer = setTimeout(() => wrapper.classList.remove('flash'), FLASH_MS);
  };

  /**
   * Scrolls to the target and, for a field, focuses its control. `hold` keeps
   * the target in place for a moment while the page is still filling in: the
   * rows of Schedules, Secrets and Imports arrive after the page and push
   * everything below them down, and a field that waits for the provider
   * catalog is not even there yet. A deep link must end up AT its target, not
   * where the target was before the rest landed. Any input from the operator
   * ends the hold at once.
   */
  const goTo = (target: RevealTarget, options: { smooth: boolean; hold: boolean }): void => {
    stopHold?.();
    sentTo = target.section;
    let focused = false;
    const apply = (smooth: boolean): void => {
      const sectionNode = sectionElement(target.section);
      if (sectionNode === null) return;
      const fieldNode =
        target.field !== undefined
          ? (page?.querySelector<HTMLElement>(`[data-field="${target.field}"]`) ?? null)
          : null;
      if (fieldNode !== null && target.field !== undefined) {
        viewport.scrollTo(fieldNode, stuckOffset() + FIELD_HEADROOM, smooth);
        if (!focused) {
          focused = focusFieldControl(fieldNode, target.field);
          flash(fieldNode);
        }
      } else {
        // A touch above the offset, so the section reads as "in view".
        viewport.scrollTo(sectionNode, stuckOffset() - 4, smooth);
      }
      updateCurrent();
    };
    apply(options.smooth);
    if (!options.hold) return;

    held = apply;
    const observer =
      page !== undefined && typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(() => held?.(false))
        : undefined;
    if (page !== undefined) observer?.observe(page);
    const release = (): void => stopHold?.();
    for (const type of SCROLL_INPUTS) window.addEventListener(type, release, { passive: true });
    const timer = setTimeout(release, HOLD_MS);
    stopHold = () => {
      held = undefined;
      stopHold = undefined;
      observer?.disconnect();
      clearTimeout(timer);
      for (const type of SCROLL_INPUTS) window.removeEventListener(type, release);
    };
  };
  // The catalog and the inherited values landing add fields (the option bag)
  // and unfold or fold others: a held deep link goes to its target again once
  // they are on the page.
  createEffect(
    on(
      () => [form.catalog.providers(), form.inherited(), resourceValue(credentials)],
      () => held?.(false),
      { defer: true },
    ),
  );

  // A chip or outline entry: scroll there, and put the section's URL in the
  // address bar without a history entry. `byNav` tells the arrival effect
  // below that this URL change has already been acted on.
  let byNav: string | undefined;
  const onNavGo = (slug: string): void => {
    const key = arrivalKey(slug, undefined);
    if (arrivalKey(section(), fieldParam()) !== key) byNav = key;
    goTo({ section: slug }, { smooth: true, hold: false });
    navigate(`${base()}/${slug}`, { replace: true, scroll: false });
  };

  // Arrival: act on the URL once the sections exist and the router is done
  // with the navigation (it scrolls to the top as its last step — after that,
  // the page's own scroll stands). Runs again for every later URL change and
  // every form.reveal() — a Save that found a problem, a save bar link.
  let seenKey: string | undefined;
  let seenTick = 0;
  createEffect(() => {
    const ready = form.saved() !== undefined;
    const routing = isRouting();
    const key = arrivalKey(section(), fieldParam());
    const tick = form.revealTick();
    if (!ready || routing) return;
    untrack(() => {
      const first = seenKey === undefined;
      if (!first && key === seenKey && tick === seenTick) return;
      const handled = key === byNav;
      seenKey = key;
      seenTick = tick;
      byNav = undefined;
      if (handled) return;
      const target = urlTarget();
      if (target === undefined) return;
      // The first arrival is a deep link: no animation, and held in place.
      goTo(target, first ? { smooth: false, hold: true } : { smooth: true, hold: false });
    });
  });

  onMount(() => {
    measureStrip();
    updateCurrent();
    const onResize = (): void => {
      measureStrip();
      updateCurrent();
    };
    // The operator takes over the scroll position: from here on the section
    // in view is whatever the page shows. (A scroll event alone cannot tell —
    // the page's own animated scroll fires them too.)
    const onOwnScroll = (): void => {
      sentTo = undefined;
    };
    for (const type of SCROLL_INPUTS) {
      window.addEventListener(type, onOwnScroll, { passive: true });
    }
    window.addEventListener('scroll', updateCurrent, { passive: true });
    window.addEventListener('resize', onResize);
    // Late content moves every section below it.
    const observer =
      page !== undefined && typeof ResizeObserver !== 'undefined'
        ? new ResizeObserver(updateCurrent)
        : undefined;
    if (page !== undefined) observer?.observe(page);
    onCleanup(() => {
      for (const type of SCROLL_INPUTS) window.removeEventListener(type, onOwnScroll);
      window.removeEventListener('scroll', updateCurrent);
      window.removeEventListener('resize', onResize);
      observer?.disconnect();
    });
  });
  onCleanup(() => {
    stopHold?.();
    clearTimeout(flashTimer);
  });

  // --- schedules and their editor -----------------------------------------------
  // The list is live: repo.changed (a pause, a re-enable, a count moved) and
  // run.changed (a run of a Schedule started or ended) both refetch it.
  const ofThisRepo = (event: { repoID?: string }): boolean =>
    event.repoID === undefined || event.repoID === home.id();
  const [schedules, { refetch: refetchSchedules }] = createLiveResource(
    () => home.id(),
    (repoID) => listRepoSchedules(repoID),
    [
      { type: 'repo.changed', match: ofThisRepo },
      { type: 'run.changed', match: ofThisRepo },
    ],
  );
  const [flows] = createResource(() => listScheduleFlows());
  // A problem to report in the Schedules section once the editor is gone (a
  // Run now from the Saved toast that the server refused).
  const [scheduleNotice, setScheduleNotice] = createSignal<string | null>(null);

  // Opened from this page: a history entry was pushed, so closing goes back.
  // Arrived at directly (a deep link, a reload): closing replaces the URL
  // with the section's.
  let openedHere = false;
  const openEditor = (id: string): void => {
    openedHere = true;
    navigate(editorHref(id), { scroll: false });
  };
  const closeEditor = (): void => {
    if (openedHere) {
      openedHere = false;
      navigate(-1);
    } else {
      navigate(`${base()}/schedules`, { replace: true, scroll: false });
    }
  };
  createEffect(
    on(editorId, (id) => {
      if (id === undefined) openedHere = false;
    }),
  );
  const runNow = async (schedule: Schedule): Promise<void> => {
    try {
      await runScheduleNow(home.id(), schedule.id);
      home.notify(`Started a run from "${schedule.name}"`);
      void refetchSchedules();
    } catch (err) {
      setScheduleNotice(errorMessage(err));
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
            schedules={schedules}
            flows={resourceValue(flows) ?? []}
            editorHref={editorHref}
            onOpen={openEditor}
            onChanged={() => void refetchSchedules()}
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
        return <IntegrationsSection credentials={resourceValue(credentials) ?? []} />;
      case 'branches':
        return <BranchesSection />;
      case 'danger':
        return <DangerZone repo={repo} />;
      default:
        return null;
    }
  };

  return (
    <div class="settings-page" ref={page}>
      {/* Nothing to navigate, and nothing to show, until the repo has loaded
          (the frame reports a failed load in its header). */}
      <Show when={form.saved()}>
        {(repo) => (
          <>
            <SectionNav
              base={base()}
              desktop={desktop()}
              current={current()}
              onGo={onNavGo}
              stickyTop={stripHeight()}
              chipsRef={(element) => (chips = element)}
            />
            <div class="settings-sections">
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
                const seed = createMemo<Schedule | null | 'missing' | 'failed' | undefined>(
                  (previous) => {
                    if (previous !== undefined && previous !== 'missing' && previous !== 'failed') {
                      return previous;
                    }
                    if (id === 'new') return null;
                    if (schedules.error !== undefined) return 'failed';
                    const list = resourceValue(schedules);
                    if (list === undefined) return undefined;
                    return list.find((candidate) => candidate.id === id) ?? 'missing';
                  },
                );
                return (
                  <Switch>
                    <Match when={seed() === undefined}>
                      <EditorNotice title="Loading schedule…" />
                    </Match>
                    <Match when={seed() === 'missing'}>
                      <EditorNotice title="Schedule not found" onClose={closeEditor}>
                        This schedule no longer exists.
                      </EditorNotice>
                    </Match>
                    <Match when={seed() === 'failed'}>
                      <EditorNotice title="Schedule not loaded" onClose={closeEditor}>
                        The schedules could not be loaded. {errorMessage(schedules.error)}
                      </EditorNotice>
                    </Match>
                    <Match when={typeof seed() === 'object'}>
                      <ScheduleEditor
                        repoId={repo().id}
                        schedule={seed() as Schedule | null}
                        providers={form.catalog.providers()}
                        // The layer under a Schedule's own agent pick: the
                        // SAVED repo's AFK agent, else the one the server
                        // says it inherits.
                        afkProviderId={
                          repo().afk_provider_default ??
                          form.inherited()?.afk_provider_default ??
                          null
                        }
                        flows={resourceValue(flows) ?? []}
                        url={editorHref(id)}
                        onClose={closeEditor}
                        onSaved={(saved) => {
                          closeEditor();
                          void refetchSchedules();
                          home.notify(`Saved "${saved.name}"`, {
                            action: { label: 'Run now', run: () => void runNow(saved) },
                          });
                        }}
                        onDeleted={(deleted) => {
                          closeEditor();
                          void refetchSchedules();
                          home.notify(`Deleted "${deleted.name}"`);
                        }}
                        onRan={(ran) => {
                          closeEditor();
                          void refetchSchedules();
                          home.notify(`Started a run from "${ran.name}"`);
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
 * deep link names is still loading, or when it is gone.
 */
function EditorNotice(props: { title: string; onClose?: () => void; children?: JSX.Element }) {
  const desktop = createMediaQuery(DESKTOP_QUERY);
  return (
    <>
      <Show when={desktop()}>
        <div class="schedule-editor-scrim" aria-hidden="true" onClick={() => props.onClose?.()} />
      </Show>
      <section class="schedule-editor" role="dialog" aria-modal="true" aria-label={props.title}>
        <header class="schedule-editor-head">
          <Show when={props.onClose}>
            <button
              type="button"
              class="icon-btn"
              aria-label="Back to schedules"
              onClick={() => props.onClose?.()}
            >
              <Icon name="chevron-left" />
            </button>
          </Show>
          <h2>{props.title}</h2>
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

/**
 * One section of the page: its heading (with the "applies immediately" tag
 * where its rows act at once), its one-line description, and its body. A
 * section that throws — one of its reads failed in a place it did not expect
 * — is replaced by its own error line, so the other nine stay usable.
 */
function SettingsSection(props: { category: RepoSettingsCategory; children: JSX.Element }) {
  const id = () => sectionElementId(props.category.slug);
  return (
    <section
      id={id()}
      classList={{ 'settings-section': true, danger: props.category.danger === true }}
      aria-labelledby={`${id()}-title`}
    >
      <header class="settings-section-head">
        <div class="settings-section-title">
          <h2 id={`${id()}-title`}>{props.category.title}</h2>
          <Show when={props.category.immediate === true}>
            <span class="settings-tag">applies immediately</span>
          </Show>
        </div>
        <p>{props.category.description}</p>
      </header>
      <ErrorBoundary
        fallback={(err, reset) => (
          <Banner
            message={`${props.category.title} could not be shown. ${errorMessage(err)}`}
            action={
              <button type="button" onClick={reset}>
                Try again
              </button>
            }
          />
        )}
      >
        {props.children}
      </ErrorBoundary>
    </section>
  );
}
