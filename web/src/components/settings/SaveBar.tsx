// The save bar (issue #61, issue #85): the ONE place a settings page's
// pending changes are saved or discarded. It shows for as long as a field
// differs from the saved snapshot, and disappears the moment nothing does.
// The repo home frame renders the repo page's, so it shows on every tab of
// the repo — Overview and Issues included.
//
// It says how many fields changed and names their sections as links that
// jump there (navigating to the settings page first when it does not show).
// After a Save that found problems it says how many there are and where
// instead, and its links go to the first problem of each section. A failed
// save that names no field — a refusal without one, a network error — is
// spelled out above the row.
//
// The bar is fixed to the bottom of the content column. A spacer of its own
// height sits in the page flow while it shows, so nothing is ever hidden
// behind it, and its bottom padding clears the phone's safe area.
//
// Announcing: the count lives in a status region that is ALWAYS in the page
// (empty while nothing is pending) — a live region inserted together with
// its text is never read out, so "1 unsaved change" would go unheard.
//
// Focus: Save and Discard take the bar off the page, and with it the button
// that had the focus. Focus then goes to something that says where the
// operator is — the page's heading after Save (`heading`), the toast's Undo
// after Discard — instead of dropping to the top of the document.

import { For, Show, createSignal, onCleanup } from 'solid-js';
import { findCategory, type SettingsCategory } from './categories';
import { plural, useSettingsFormContext } from './form';
import { isModifiedClick } from './links';

export default function SaveBar(props: {
  /** The page's sections: their titles name the links. */
  categories: readonly SettingsCategory[];
  /** The settings page's path; a section link is `<base>/<slug>`. */
  base: string;
  /** Selects the heading focus goes to after a Save (the repo page: `.repo-head h1`). */
  heading: string;
}) {
  const form = useSettingsFormContext();

  const problemCount = (): number => form.problems().length;
  const hasProblems = (): boolean => problemCount() > 0;
  // With problems the bar lists where THEY are; otherwise where the changes are.
  const sections = (): string[] =>
    hasProblems() ? form.problemSections() : form.changedSections();
  const title = (slug: string): string => findCategory(props.categories, slug)?.title ?? slug;

  const open = (event: MouseEvent, slug: string): void => {
    if (isModifiedClick(event)) return;
    event.preventDefault();
    // A problem section's link goes to its first problem (problems are in
    // page order); a changed section's to the section.
    const field = form.problems().find((key) => form.field(key).spec.section === slug);
    form.reveal({ section: slug, field });
  };

  const count = (): string =>
    hasProblems()
      ? `${plural(problemCount(), 'problem')} to fix`
      : plural(form.changed().length, 'unsaved change');
  const announcement = (): string =>
    form.dirty() ? `${count()} in ${sections().map(title).join(', ')}` : '';

  // Where focus goes once the bar is gone. A no-op while focus still sits on
  // something real: the bar stayed (a newer edit, a refusal), or a problem
  // took the focus to its field.
  const focusAfter = (preferred?: () => HTMLElement | null): void => {
    const active = document.activeElement;
    if (active instanceof HTMLElement && active !== document.body && active.isConnected) return;
    const target = preferred?.() ?? document.querySelector<HTMLElement>(props.heading);
    if (target === null) return;
    // A heading takes focus only when told it may.
    if (target.tabIndex < 0 && !target.hasAttribute('tabindex')) {
      target.setAttribute('tabindex', '-1');
    }
    target.focus({ preventScroll: true });
  };
  const save = async (): Promise<void> => {
    if (await form.save()) focusAfter();
  };
  let discardButton: HTMLButtonElement | undefined;
  const discard = (): void => {
    form.discard();
    const undo = document.querySelector<HTMLElement>('.toast .toast-action');
    focusAfter(() => undo);
    // Undo takes the toast off the page in turn, and brings the bar back:
    // focus returns to where it was before the Discard.
    undo?.addEventListener('click', () => queueMicrotask(() => discardButton?.focus()), {
      once: true,
    });
  };

  // The spacer follows the bar's real height (two text lines on a phone, an
  // error row above them); the stylesheet's height stands in before the first
  // measurement and wherever ResizeObserver is missing.
  const [height, setHeight] = createSignal<number | null>(null);
  let observer: ResizeObserver | undefined;
  const watch = (bar: HTMLElement): void => {
    if (typeof ResizeObserver === 'undefined') return;
    observer?.disconnect();
    observer = new ResizeObserver(() => setHeight(bar.offsetHeight));
    observer.observe(bar);
  };
  onCleanup(() => observer?.disconnect());

  return (
    <>
      <Show when={form.dirty()}>
        <div
          class="settings-savebar-space"
          aria-hidden="true"
          style={height() !== null ? { height: `${height()}px` } : undefined}
        />
        <section
          classList={{ 'settings-savebar': true, bad: hasProblems() || form.barError() !== null }}
          aria-label="Unsaved changes"
          ref={watch}
        >
          <div class="settings-savebar-inner">
            <Show when={form.barError()}>
              {(message) => (
                <p class="settings-savebar-error" role="alert">
                  Not saved. {message()}
                </p>
              )}
            </Show>
            <div class="settings-savebar-row">
              <div class="settings-savebar-msg">
                <span class="settings-savebar-dot" aria-hidden="true" />
                <span class="settings-savebar-text">
                  <strong>{count()}</strong>
                  <small>
                    in{' '}
                    <For each={sections()}>
                      {(slug, index) => (
                        <>
                          <Show when={index() > 0}>, </Show>
                          <a
                            href={`${props.base}/${slug}`}
                            class="settings-savebar-link"
                            on:click={(event) => open(event, slug)}
                          >
                            {title(slug)}
                          </a>
                        </>
                      )}
                    </For>
                  </small>
                </span>
              </div>
              <button type="button" ref={discardButton} onClick={discard} disabled={form.busy()}>
                Discard
              </button>
              <button
                type="button"
                class="primary"
                onClick={() => void save()}
                disabled={form.busy()}
              >
                {form.busy() ? 'Saving…' : 'Save'}
              </button>
            </div>
          </div>
        </section>
      </Show>
      {/* Always in the page — after the bar, so the bar stays the sibling
          the toast's offset rule looks for. */}
      <div class="visually-hidden settings-savebar-live" role="status">
        {announcement()}
      </div>
    </>
  );
}
