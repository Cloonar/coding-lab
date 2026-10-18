// WritePreview contract (issue #103): ARIA tabs (tablist / tab / tabpanel,
// aria-selected, one roving tab stop, ←/→ and Home/End select), the caller's
// textarea stays mounted across switches and is named by the visible label,
// and Preview renders the draft — or "Nothing to preview" when it is blank.

import { createSignal } from 'solid-js';
import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import WritePreview, { type WritePreviewTab } from './WritePreview';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;
let setDraft: (v: string) => void;

function mount(initial: string): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(() => {
    const [draft, set] = createSignal(initial);
    const [tab, setTab] = createSignal<WritePreviewTab>('write');
    setDraft = set;
    return (
      <WritePreview label="Body" value={draft()} tab={tab()} onTabChange={setTab}>
        {(labelId) => (
          <textarea
            aria-labelledby={labelId}
            value={draft()}
            onInput={(e) => set(e.currentTarget.value)}
          />
        )}
      </WritePreview>
    );
  }, container);
}

const tabs = () => Array.from(container.querySelectorAll<HTMLButtonElement>('[role="tab"]'));
const panels = () => Array.from(container.querySelectorAll<HTMLElement>('[role="tabpanel"]'));

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

describe('WritePreview', () => {
  it('exposes ARIA tabs wired to their panels, starting on Write', () => {
    mount('**hi**');
    const [write, preview] = tabs();
    expect(container.querySelector('[role="tablist"]')).not.toBeNull();
    expect(tabs().map((t) => t.textContent)).toEqual(['Write', 'Preview']);
    expect(write?.getAttribute('aria-selected')).toBe('true');
    expect(preview?.getAttribute('aria-selected')).toBe('false');
    expect(write?.tabIndex).toBe(0);
    expect(preview?.tabIndex).toBe(-1);
    expect(panels().map((p) => p.hidden)).toEqual([false, true]);
    expect(write?.getAttribute('aria-controls')).toBe(panels()[0]?.id);
    expect(container.textContent).toContain('Markdown');
    const labelId = container.querySelector('textarea')?.getAttribute('aria-labelledby') ?? '';
    expect(document.getElementById(labelId)?.textContent).toBe('Body');
  });

  it('switches with the arrow keys and keeps the same textarea node', () => {
    mount('**hi**');
    const textarea = container.querySelector('textarea');
    tabs()[0]?.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
    expect(tabs()[1]?.getAttribute('aria-selected')).toBe('true');
    expect(tabs()[1]?.tabIndex).toBe(0);
    expect(panels().map((p) => p.hidden)).toEqual([true, false]);
    expect(panels()[1]?.querySelector('.md strong')?.textContent).toBe('hi');

    tabs()[1]?.dispatchEvent(new KeyboardEvent('keydown', { key: 'Home', bubbles: true }));
    expect(tabs()[0]?.getAttribute('aria-selected')).toBe('true');
    expect(container.querySelector('textarea')).toBe(textarea);
  });

  it('previews a blank draft as "Nothing to preview"', () => {
    mount('');
    setDraft('  \n ');
    tabs()[1]?.click();
    expect(panels()[1]?.textContent).toBe('Nothing to preview');
  });
});
