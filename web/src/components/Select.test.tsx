// Unified select contract (issue #66):
// - trigger button opens an ARIA listbox panel; clicking an enabled row
//   commits its value and closes;
// - keyboard: ArrowDown on the trigger opens; arrows move the active row
//   skipping disabled rows; Home/End jump (on the listbox); Enter commits;
//   Escape closes and refocuses the trigger;
// - the filter input appears automatically from 8 options up (and the
//   `search` prop forces it both ways), filtering case-insensitively over
//   label AND value;
// - CatalogSelect semantics survive: an inherit entry (value "") emits '',
//   a value missing from the catalog renders marked but selectable, '' with
//   no inherit shows "(unset)";
// - disabled rows render (with status text) but are not selectable;
// - panelPlacement (pure): below preferred, flip above only when below lacks
//   room and above has more, max-height clamped to the chosen side.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import Select, { PANEL_MAX_HEIGHT, panelPlacement, type SelectOption } from './Select';

type SelectProps = Parameters<typeof Select>[0];

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

const flush = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

function mountSelect(overrides: Partial<SelectProps> & Pick<SelectProps, 'options'>): void {
  container = document.createElement('div');
  document.body.appendChild(container);
  const props: SelectProps = {
    skin: 'field',
    label: 'Fruit',
    value: '',
    onChange: () => {},
    ...overrides,
  };
  dispose = render(() => <Select {...props} />, container);
}

function unmount(): void {
  dispose?.();
  dispose = undefined;
  container?.remove();
}

afterEach(unmount);

const FRUIT: SelectOption[] = [
  { value: 'apple', label: 'Apple' },
  { value: 'banana', label: 'Banana', disabled: true, status: 'cloning 42%' },
  { value: 'cherry', label: 'Cherry' },
];

function trigger(): HTMLButtonElement {
  const el = container.querySelector<HTMLButtonElement>('button[aria-haspopup="listbox"]');
  if (!el) throw new Error('missing select trigger');
  return el;
}

function listbox(): HTMLElement | null {
  return container.querySelector<HTMLElement>('[role="listbox"]');
}

function rows(): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll<HTMLButtonElement>('[role="option"]'));
}

function rowLabels(): (string | null | undefined)[] {
  return rows().map((row) => row.querySelector('.select-option-label')?.textContent);
}

function searchInput(): HTMLInputElement | null {
  return container.querySelector<HTMLInputElement>('.select-search');
}

function keydown(el: Element, key: string): void {
  el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
}

async function openViaClick(): Promise<void> {
  trigger().click();
  await flush();
}

function manyOptions(count: number): SelectOption[] {
  return Array.from({ length: count }, (_, i) => ({ value: `v${i}`, label: `Option ${i}` }));
}

describe('Select rows and commit', () => {
  it('renders all rows and commits an enabled row on click', async () => {
    const onChange = vi.fn();
    mountSelect({ options: FRUIT, value: 'apple', onChange });

    await openViaClick();
    expect(rowLabels()).toEqual(['Apple', 'Banana', 'Cherry']);
    expect(rows()[0]?.getAttribute('aria-selected')).toBe('true');

    rows()[2]?.click();
    await flush();
    expect(onChange).toHaveBeenCalledExactlyOnceWith('cherry');
    expect(listbox()).toBeNull(); // committed → closed
  });

  it('shows the selected option label on the trigger', () => {
    mountSelect({ options: FRUIT, value: 'cherry' });
    expect(trigger().textContent).toContain('Cherry');
  });

  it('does not commit a disabled row and keeps the panel open', async () => {
    const onChange = vi.fn();
    mountSelect({ options: FRUIT, value: 'apple', onChange });

    await openViaClick();
    rows()[1]?.click();
    await flush();
    expect(onChange).not.toHaveBeenCalled();
    expect(listbox()).not.toBeNull();
  });

  it('renders a disabled row with its status text', async () => {
    mountSelect({ options: FRUIT, value: 'apple' });
    await openViaClick();

    const banana = rows()[1];
    expect(banana?.disabled).toBe(true);
    expect(banana?.getAttribute('aria-disabled')).toBe('true');
    expect(banana?.querySelector('.select-option-status')?.textContent).toBe('cloning 42%');
  });

  it('fires onOpen as the panel opens (host closes sibling popovers)', async () => {
    const onOpen = vi.fn();
    mountSelect({ options: FRUIT, value: 'apple', onOpen });
    await openViaClick();
    expect(onOpen).toHaveBeenCalledOnce();
  });
});

describe('Select keyboard', () => {
  it('opens on ArrowDown with the selected row active, arrows skip disabled rows, Enter commits', async () => {
    const onChange = vi.fn();
    mountSelect({ options: FRUIT, value: 'apple', onChange });

    keydown(trigger(), 'ArrowDown');
    await flush();
    const list = listbox();
    expect(list).not.toBeNull();
    // No search at 3 options → focus lands on the listbox itself.
    expect(document.activeElement).toBe(list);
    expect(list?.getAttribute('aria-activedescendant')).toBe(rows()[0]?.id);

    keydown(list as Element, 'ArrowDown'); // skips disabled Banana → Cherry
    expect(list?.getAttribute('aria-activedescendant')).toBe(rows()[2]?.id);
    expect(rows()[2]?.classList.contains('active')).toBe(true);

    keydown(list as Element, 'Enter');
    await flush();
    expect(onChange).toHaveBeenCalledExactlyOnceWith('cherry');
    expect(listbox()).toBeNull();
    expect(document.activeElement).toBe(trigger()); // commit refocuses
  });

  it('jumps with Home/End on the listbox, skipping disabled edges', async () => {
    const options: SelectOption[] = [
      { value: 'a', label: 'A', disabled: true },
      { value: 'b', label: 'B' },
      { value: 'c', label: 'C' },
      { value: 'd', label: 'D', disabled: true },
    ];
    mountSelect({ options, value: 'c' });
    await openViaClick();
    const list = listbox() as Element;

    keydown(list, 'Home');
    expect(list.getAttribute('aria-activedescendant')).toBe(rows()[1]?.id);
    keydown(list, 'End');
    expect(list.getAttribute('aria-activedescendant')).toBe(rows()[2]?.id);
  });

  it('closes on Escape and returns focus to the trigger without committing', async () => {
    const onChange = vi.fn();
    mountSelect({ options: FRUIT, value: 'apple', onChange });

    await openViaClick();
    keydown(listbox() as Element, 'Escape');
    await flush();
    expect(listbox()).toBeNull();
    expect(onChange).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(trigger());
  });
});

describe('Select search', () => {
  it('shows the filter input from 8 options up and focuses it on open', async () => {
    mountSelect({ options: manyOptions(8), value: 'v0' });
    await openViaClick();
    expect(searchInput()).not.toBeNull();
    expect(document.activeElement).toBe(searchInput());
  });

  it('hides the filter input below 8 options', async () => {
    mountSelect({ options: manyOptions(7), value: 'v0' });
    await openViaClick();
    expect(searchInput()).toBeNull();
  });

  it('search=true forces the input on at few options', async () => {
    mountSelect({ options: manyOptions(3), value: 'v0', search: true });
    await openViaClick();
    expect(searchInput()).not.toBeNull();
  });

  it('search=false forces the input off at many options', async () => {
    mountSelect({ options: manyOptions(9), value: 'v0', search: false });
    await openViaClick();
    expect(searchInput()).toBeNull();
  });

  it('filters case-insensitively over label AND value', async () => {
    const options: SelectOption[] = [
      { value: 'sonata-9', label: 'Sonata' },
      { value: 'opal-3', label: 'Opal' },
      { value: 'x9', label: 'Nine' },
    ];
    mountSelect({ options, value: 'sonata-9', search: true });
    await openViaClick();
    const input = searchInput() as HTMLInputElement;

    input.value = 'OPA'; // label match, case-insensitive
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await flush();
    expect(rowLabels()).toEqual(['Opal']);

    input.value = 'x9'; // value-only match
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await flush();
    expect(rowLabels()).toEqual(['Nine']);
  });
});

describe('Select catalog semantics (former CatalogSelect)', () => {
  it('prepends the inherit entry and emits "" when picked', async () => {
    const onChange = vi.fn();
    mountSelect({
      options: FRUIT,
      value: 'apple',
      inheritLabel: 'Same as default',
      onChange,
    });

    await openViaClick();
    expect(rowLabels()[0]).toBe('Same as default');
    rows()[0]?.click();
    await flush();
    expect(onChange).toHaveBeenCalledExactlyOnceWith('');
  });

  it('titles the trigger with the inherit label when the value is empty', () => {
    mountSelect({ options: FRUIT, value: '', inheritLabel: 'Same as default' });
    expect(trigger().textContent).toContain('Same as default');
  });

  it('keeps a not-in-catalog value selectable and marked', async () => {
    const onChange = vi.fn();
    mountSelect({ options: FRUIT, value: 'ghost', onChange });

    expect(trigger().textContent).toContain('ghost (not in catalog)');
    await openViaClick();
    const marker = rows().find(
      (row) => row.querySelector('.select-option-label')?.textContent === 'ghost (not in catalog)',
    );
    expect(marker).toBeDefined();
    marker?.click();
    await flush();
    expect(onChange).toHaveBeenCalledExactlyOnceWith('ghost');
  });

  it('shows (unset) for an empty value without an inherit entry', () => {
    mountSelect({ options: FRUIT, value: '' });
    expect(trigger().textContent).toContain('(unset)');
  });
});

describe('Select chip skin', () => {
  it('renders a composer-chip pill with aria-label, leading icon and caret', () => {
    mountSelect({
      skin: 'chip',
      label: 'Repository',
      options: FRUIT,
      value: 'apple',
      icon: <span class="test-icon" />,
    });

    const chip = trigger();
    expect(chip.classList.contains('composer-chip')).toBe(true);
    expect(chip.getAttribute('aria-label')).toBe('Repository');
    expect(chip.querySelector('.test-icon')).not.toBeNull();
    expect(chip.querySelector('.composer-chip-caret')).not.toBeNull();
    expect(chip.querySelector('.composer-chip-label')?.textContent).toBe('Apple');
    // No visible .field label block in the chip skin.
    expect(container.querySelector('.field')).toBeNull();
  });
});

describe('panelPlacement', () => {
  it('opens below with the full cap when below has plenty of room', () => {
    expect(panelPlacement(100, 144, 800, 6)).toEqual({
      openAbove: false,
      maxHeight: PANEL_MAX_HEIGHT,
    });
  });

  it('flips above when below is cramped and above has more free space', () => {
    expect(panelPlacement(700, 744, 800, 6)).toEqual({
      openAbove: true,
      maxHeight: PANEL_MAX_HEIGHT,
    });
  });

  it('clamps max-height to the chosen side when both are cramped', () => {
    // above (168px free) beats below (64px free) → open above, clamped
    expect(panelPlacement(180, 224, 300, 6)).toEqual({ openAbove: true, maxHeight: 168 });
    // below (184px free) beats above (48px free) → stay below, clamped
    expect(panelPlacement(60, 104, 300, 6)).toEqual({ openAbove: false, maxHeight: 184 });
  });
});
