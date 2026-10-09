// One section of a one-page settings form (issue #61, issue #85): its
// heading (with the "applies immediately" tag where its rows act at once),
// its one-line description, and its body. The section's element id
// (sectionElementId) is what the page's scroll logic (sectionScroll.ts)
// measures and scrolls to. A section that throws — one of its reads failed
// in a place it did not expect — is replaced by its own error line, so the
// other sections stay usable.

import { ErrorBoundary, Show, type JSX } from 'solid-js';
import { errorMessage } from '../../api';
import Banner from '../Banner';
import { sectionElementId, type SettingsCategory } from './categories';

export default function SettingsSection(props: {
  category: SettingsCategory;
  children: JSX.Element;
}) {
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
