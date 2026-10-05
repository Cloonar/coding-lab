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
            <div class="settings-savebar-msg" role="status">
              <span class="settings-savebar-dot" aria-hidden="true" />
              <span class="settings-savebar-text">
                <strong>
                  {hasProblems()
                    ? `${plural(problemCount(), 'problem')} to fix`
                    : plural(form.changed().length, 'unsaved change')}
                </strong>
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
            <button type="button" onClick={() => form.discard()} disabled={form.busy()}>
              Discard
            </button>
            <button
              type="button"
              class="primary"
              onClick={() => void form.save()}
              disabled={form.busy()}
            >
              {form.busy() ? 'Saving…' : 'Save'}
            </button>
          </div>
        </div>
      </section>
    </Show>
  );
}
