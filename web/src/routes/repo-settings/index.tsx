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
// into view and focuses its control. The schedule editor's URLs
// (settings/schedules/new, settings/schedules/:scheduleId) belong to
// Schedules. A chip or outline entry scrolls to its section and REPLACES the
// URL with the section's — no history entry per section.
//
// The frame owns the page container, RequireAuth, the repo header and the one
// live repo resource; this page reads the repo through the form store, whose
// `saved()` survives a failed refresh.

import { useIsRouting, useLocation, useMatch, useNavigate, useParams } from '@solidjs/router';
import {
  ErrorBoundary,
  For,
  Show,
  createEffect,
  createResource,
  createSignal,
  on,
  onCleanup,
  onMount,
  untrack,
  type Accessor,
  type JSX,
} from 'solid-js';
import { errorMessage, listCredentials, type Repo } from '../../api';
import Banner from '../../components/Banner';
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
import { fieldControlId } from './Field';
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

/** Moves focus to a field's control without scrolling; false when there is none to take it. */
function focusField(wrapper: HTMLElement, key: RepoFieldKey): boolean {
  const control =
    document.getElementById(fieldControlId(key)) ??
    // A group of controls (a segmented pick, the option bag) has no single
    // id: its tab stop is the target.
    wrapper.querySelector<HTMLElement>(
      'input, select, textarea, button[role="radio"][tabindex="0"], button:not([tabindex="-1"])',
    );
  if (control === null) return false;
  control.focus({ preventScroll: true });
  return true;
}

export default function RepoSettings() {
  const params = useParams<{ id: string; section?: string; scheduleId?: string }>();
  const location = useLocation();
  const navigate = useNavigate();
  const isRouting = useIsRouting();
  const home = useRepoHome();
  const form = useRepoSettingsForm();
  const desktop = createMediaQuery(DESKTOP_QUERY);

  // The provider catalog and the global settings load with this tab only.
  form.catalog.load();
  const [credentials] = createResource(() => listCredentials());

  const base = (): string => `/repos/${params.id}/settings`;
  // The schedule editor's URLs carry no :section; they belong to Schedules.
  const scheduleNew = useMatch(() => `${base()}/schedules/new`);
  const section = (): string | undefined =>
    params.section ??
    (params.scheduleId !== undefined || scheduleNew() !== undefined ? 'schedules' : undefined);

  // Where the URL points: a field (its own section wins over a mismatched
  // :section), else a known section, else nowhere — the top of the page.
  const urlTarget = (): RevealTarget | undefined => {
    const field = location.query.field;
    if (typeof field === 'string' && isRepoFieldKey(field)) {
      return { section: repoField(field).section, field };
    }
    const slug = repoSettingsCategory(section())?.slug;
    return slug !== undefined ? { section: slug } : undefined;
  };

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
          focused = focusField(fieldNode, target.field);
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
  // The catalog landing adds fields (the option bag) and rewrites hints: a
  // held deep link goes to its target again once they are on the page.
  createEffect(
    on(
      () => [form.catalog.providers(), form.catalog.settings(), resourceValue(credentials)],
      () => held?.(false),
      { defer: true },
    ),
  );

  // A chip or outline entry: scroll there, and put the section's URL in the
  // address bar without a history entry. `byNav` tells the arrival effect
  // below that this URL change has already been acted on.
  let byNav: string | undefined;
  const onNavGo = (slug: string): void => {
    const path = `${base()}/${slug}`;
    if (`${location.pathname}${location.search}` !== path) byNav = path;
    goTo({ section: slug }, { smooth: true, hold: false });
    navigate(path, { replace: true, scroll: false });
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
    const key = `${location.pathname}${location.search}`;
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

  const refetch = (): void => void home.refetch();
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
            repo={repo}
            providers={form.catalog.providers()}
            settings={form.catalog.settings() ?? {}}
            onSaved={refetch}
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
          </>
        )}
      </Show>
    </div>
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
