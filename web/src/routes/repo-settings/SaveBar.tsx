// The save bar (issue #61): the ONE place pending repo settings are saved or
// discarded. The repo home frame renders it, so it shows on every tab of the
// repo — Overview and Issues included — for as long as a field differs from
// the saved repo, and disappears the moment nothing does.
//
// It says how many fields changed and names their sections as links that
// jump there (opening the Settings tab first when another tab shows). After a
// Save that found problems it says how many there are and where instead, and
// its links go to the first problem of each section. A failed save that names
// no field — a refusal without one, a network error — is spelled out above
// the row.
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
// operator is — the repo's heading after Save, the toast's Undo after
// Discard — instead of dropping to the top of the document.

import { For, Show, createSignal, onCleanup } from 'solid-js';
import { REPO_FIELD_KEYS, repoField, type FormSectionSlug } from './fields';
import { repoSettingsCategory } from './categories';
import { plural, useRepoSettingsForm } from './form';
import { useRepoHome } from '../repo-home/context';

/** A click the browser should handle itself (new tab, new window). */
function isModifiedClick(event: MouseEvent): boolean {
  return event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
}

export default function SaveBar() {
  const form = useRepoSettingsForm();
  const home = useRepoHome();

  const problemCount = (): number => form.problems().length;
  const hasProblems = (): boolean => problemCount() > 0;
  // With problems the bar lists where THEY are; otherwise where the changes are.
  const sections = (): FormSectionSlug[] =>
    hasProblems() ? form.problemSections() : form.changedSections();
  const title = (slug: string): string => repoSettingsCategory(slug)?.title ?? slug;

  const open = (event: MouseEvent, slug: FormSectionSlug): void => {
    if (isModifiedClick(event)) return;
    event.preventDefault();
    // A problem section's link goes to its first problem; a changed section's
    // to the section.
    const field = REPO_FIELD_KEYS.find(
      (key) => repoField(key).section === slug && form.problems().includes(key),
    );
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
    const target = preferred?.() ?? document.querySelector<HTMLElement>('.repo-head h1');
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
                            href={`/repos/${home.id()}/settings/${slug}`}
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
