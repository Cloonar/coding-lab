// The settings page's section navigation (issue #61): below 1024px a sticky
// row of chips under the repo tabs — it scrolls sideways inside itself, never
// the page — and from 1024px a sticky outline with the group labels. Exactly
// one of the two is in the document at a time.
//
// Both mark the section in view with aria-current="location" (plus a filled
// chip / a heavier outline entry — never colour alone), and a section with
// pending changes with a dot plus the words "unsaved changes". An entry is a
// real link to its section's URL; a plain click scrolls there instead of
// navigating (the page replaces the URL itself), a modified click opens the
// URL the usual way.

import { For, Show, createEffect, on } from 'solid-js';
import { REPO_SETTINGS_CATEGORIES, type RepoSettingsCategory } from './categories';
import { useRepoSettingsForm } from './form';

interface OutlineGroup {
  label: string | null;
  sections: RepoSettingsCategory[];
}

/** The sections bundled under their group labels, in page order. */
function outlineGroups(): OutlineGroup[] {
  const groups: OutlineGroup[] = [];
  for (const category of REPO_SETTINGS_CATEGORIES) {
    const last = groups[groups.length - 1];
    if (last !== undefined && last.label === category.group) last.sections.push(category);
    else groups.push({ label: category.group, sections: [category] });
  }
  return groups;
}

/** A click the browser should handle itself (new tab, new window, download). */
function isModifiedClick(event: MouseEvent): boolean {
  return event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
}

export default function SectionNav(props: {
  /** The settings index path, `/repos/:id/settings`. */
  base: string;
  /** True from 1024px: the outline replaces the chips. */
  desktop: boolean;
  /** The slug of the section in view. */
  current: string;
  /** Scrolls to a section (and puts its URL in the address bar). */
  onGo: (slug: string) => void;
  /** How far below the viewport top the chips stick, in px (the mobile top strip). */
  stickyTop: number;
  /** Receives the chips row, whose height the page's scroll offsets include. */
  chipsRef?: (element: HTMLElement | undefined) => void;
}) {
  const form = useRepoSettingsForm();
  const hasChanges = (slug: string): boolean => (form.changedSections() as string[]).includes(slug);

  const go = (event: MouseEvent, slug: string): void => {
    if (isModifiedClick(event)) return;
    // Before the router's own link handler sees the click: it must not push
    // a history entry and jump to the top of the page.
    event.preventDefault();
    props.onGo(slug);
  };

  const Mark = (mark: { slug: string }) => (
    <Show when={hasChanges(mark.slug)}>
      <span class="settings-nav-dot" aria-hidden="true" />
      <span class="visually-hidden"> (unsaved changes)</span>
    </Show>
  );

  let chips: HTMLElement | undefined;
  // Keep the current chip in sight: the row scrolls sideways under the thumb,
  // but the section in view changes with the PAGE's scroll.
  createEffect(
    on(
      () => [props.current, props.desktop] as const,
      ([current]) => {
        const chip = chips?.querySelector<HTMLElement>(`[data-section="${current}"]`);
        if (chips === undefined || chip == null || chips.clientWidth === 0) return;
        chips.scrollLeft = chip.offsetLeft - chips.clientWidth / 2 + chip.clientWidth / 2;
      },
    ),
  );

  return (
    <Show
      when={props.desktop}
      fallback={
        <nav
          class="settings-chips"
          aria-label="Settings sections"
          style={{ top: `${props.stickyTop}px` }}
          ref={(element) => {
            chips = element;
            props.chipsRef?.(element);
          }}
        >
          <For each={REPO_SETTINGS_CATEGORIES}>
            {(category) => (
              <a
                href={`${props.base}/${category.slug}`}
                classList={{ 'settings-chip': true, danger: category.danger === true }}
                data-section={category.slug}
                aria-current={props.current === category.slug ? 'location' : undefined}
                on:click={(event) => go(event, category.slug)}
              >
                <span class="settings-chip-pill">
                  {category.title}
                  <Mark slug={category.slug} />
                </span>
              </a>
            )}
          </For>
        </nav>
      }
    >
      <nav class="settings-outline" aria-label="Settings sections">
        <For each={outlineGroups()}>
          {(group, index) => (
            <div
              class="settings-outline-group"
              role={group.label !== null ? 'group' : undefined}
              aria-labelledby={
                group.label !== null ? `settings-outline-group-${index()}` : undefined
              }
            >
              <Show when={group.label}>
                <span class="settings-outline-label" id={`settings-outline-group-${index()}`}>
                  {group.label}
                </span>
              </Show>
              <For each={group.sections}>
                {(category) => (
                  <a
                    href={`${props.base}/${category.slug}`}
                    classList={{ 'settings-outline-link': true, danger: category.danger === true }}
                    data-section={category.slug}
                    aria-current={props.current === category.slug ? 'location' : undefined}
                    on:click={(event) => go(event, category.slug)}
                  >
                    {category.title}
                    <Mark slug={category.slug} />
                  </a>
                )}
              </For>
            </div>
          )}
        </For>
      </nav>
    </Show>
  );
}
