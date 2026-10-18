// Write / Preview (issue #103): the one tab control both issue-view forms use
// — the title/body editor and the comment form. Write shows the caller's
// textarea (the caller keeps its name, value and input handler, so the draft
// lives with the form and survives every tab switch); Preview renders the
// current draft through the shared markdown renderer with the caller's `#N`
// resolver and task lists on — what Save or Comment will show. An empty or
// whitespace draft previews as a muted "Nothing to preview".
//
// The caller owns the selected tab (`tab` / `onTabChange`) so it can return to
// Write after posting. Semantics follow the ARIA tabs pattern: role="tablist"
// of role="tab" buttons with aria-selected and aria-controls, one tab stop
// (roving tabindex on the selected tab), ←/→ and Home/End move to and select a
// tab. Both panels stay mounted (the inactive one `hidden`), so the textarea
// keeps its caret and scroll; the preview only renders while it is shown.
//
// `label` is the visible name above the tabs; the caller's textarea points at
// it through the id the children function receives (aria-labelledby).

import { For, Show, createUniqueId, untrack, type JSX } from 'solid-js';
import { Markdown, type RefResolver } from './Markdown';

export type WritePreviewTab = 'write' | 'preview';

const TABS: { value: WritePreviewTab; label: string }[] = [
  { value: 'write', label: 'Write' },
  { value: 'preview', label: 'Preview' },
];

export default function WritePreview(props: {
  /** Visible label above the tabs; names the textarea via `labelId`. */
  label: string;
  /** The current draft — what Preview renders. */
  value: string;
  tab: WritePreviewTab;
  onTabChange: (tab: WritePreviewTab) => void;
  resolveRef?: RefResolver;
  /** Extra classes on the wrapper (per-form sizing). */
  class?: string;
  /** The Write pane: the caller's textarea, given the label's id. */
  children: (labelId: string) => JSX.Element;
}) {
  const uid = createUniqueId();
  const labelId = `wp-${uid}-label`;
  const tabId = (t: WritePreviewTab) => `wp-${uid}-tab-${t}`;
  const panelId = (t: WritePreviewTab) => `wp-${uid}-panel-${t}`;
  const buttons: HTMLButtonElement[] = [];
  // Built once (the component body is untracked): the textarea is the caller's
  // node and must never be recreated by a tab switch.
  const writePane = untrack(() => props.children(labelId));

  const select = (index: number) => {
    const next = TABS[(index + TABS.length) % TABS.length];
    if (next === undefined) return;
    props.onTabChange(next.value);
    buttons[TABS.indexOf(next)]?.focus();
  };
  const onKeyDown = (event: KeyboardEvent, index: number) => {
    const moves: Record<string, number> = {
      ArrowLeft: index - 1,
      ArrowRight: index + 1,
      Home: 0,
      End: TABS.length - 1,
    };
    const target = moves[event.key];
    if (target === undefined) return;
    event.preventDefault();
    select(target);
  };

  return (
    <div class={props.class ? `write-preview ${props.class}` : 'write-preview'}>
      <span class="write-preview-label" id={labelId}>
        {props.label}
      </span>
      <div class="write-preview-tabs" role="tablist" aria-labelledby={labelId}>
        <For each={TABS}>
          {(t, index) => (
            <button
              ref={(el) => (buttons[index()] = el)}
              type="button"
              role="tab"
              id={tabId(t.value)}
              aria-controls={panelId(t.value)}
              aria-selected={props.tab === t.value}
              tabIndex={props.tab === t.value ? 0 : -1}
              onClick={() => props.onTabChange(t.value)}
              onKeyDown={(e) => onKeyDown(e, index())}
            >
              {t.label}
            </button>
          )}
        </For>
        <span class="write-preview-hint">Markdown</span>
      </div>
      <div
        role="tabpanel"
        id={panelId('write')}
        aria-labelledby={tabId('write')}
        hidden={props.tab !== 'write'}
      >
        {writePane}
      </div>
      <div
        role="tabpanel"
        class="preview-pane"
        id={panelId('preview')}
        aria-labelledby={tabId('preview')}
        hidden={props.tab !== 'preview'}
      >
        <Show when={props.tab === 'preview'}>
          <Show
            when={props.value.trim() !== ''}
            fallback={<p class="muted preview-empty">Nothing to preview</p>}
          >
            <Markdown
              source={props.value}
              class="md issue-md"
              resolveRef={props.resolveRef}
              taskLists
            />
          </Show>
        </Show>
      </div>
    </div>
  );
}
