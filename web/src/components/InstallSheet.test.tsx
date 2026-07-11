// InstallSheet contract (issue #142): closed renders nothing; android offers
// Install + Not now; iOS walks Share -> Add to Home Screen with only Not now
// (there is no install API on iOS); the scrim dismisses but the card never
// does, since the dismiss handler lives on the scrim element alone.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it, vi } from 'vitest';
import InstallSheet, { type InstallSheetProps } from './InstallSheet';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

function mountSheet(overrides: Partial<InstallSheetProps> = {}): {
  onInstall: ReturnType<typeof vi.fn>;
  onDismiss: ReturnType<typeof vi.fn>;
} {
  container = document.createElement('div');
  document.body.appendChild(container);
  const onInstall = vi.fn();
  const onDismiss = vi.fn();
  const props: InstallSheetProps = {
    open: true,
    variant: 'android',
    onInstall,
    onDismiss,
    ...overrides,
  };
  dispose = render(() => <InstallSheet {...props} />, container);
  return { onInstall, onDismiss };
}

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container?.remove();
});

function dialog(): HTMLElement | null {
  return container.querySelector<HTMLElement>('[role="dialog"]');
}

function scrim(): HTMLElement | null {
  return container.querySelector<HTMLElement>('.install-scrim');
}

function buttonWithText(text: string): HTMLButtonElement | undefined {
  return Array.from(container.querySelectorAll('button')).find((b) =>
    b.textContent?.includes(text),
  );
}

describe('InstallSheet closed', () => {
  it('renders nothing when open is false', () => {
    mountSheet({ open: false });
    expect(dialog()).toBeNull();
    expect(scrim()).toBeNull();
    expect(container.innerHTML).toBe('');
  });
});

describe('InstallSheet android variant', () => {
  it('shows Install and Not now; Install fires onInstall only', () => {
    const { onInstall, onDismiss } = mountSheet({ variant: 'android' });

    const install = buttonWithText('Install');
    const notNow = buttonWithText('Not now');
    expect(install).toBeDefined();
    expect(notNow).toBeDefined();

    install?.click();
    expect(onInstall).toHaveBeenCalledOnce();
    expect(onDismiss).not.toHaveBeenCalled();
  });

  it('Not now fires onDismiss', () => {
    const { onDismiss } = mountSheet({ variant: 'android' });
    buttonWithText('Not now')?.click();
    expect(onDismiss).toHaveBeenCalledOnce();
  });
});

describe('InstallSheet ios variant', () => {
  it('shows both instruction steps and no Install button', () => {
    mountSheet({ variant: 'ios' });

    expect(container.textContent).toContain('Share');
    expect(container.textContent).toContain('Add to Home Screen');
    expect(buttonWithText('Install')).toBeUndefined();
  });

  it('Not now fires onDismiss', () => {
    const { onDismiss } = mountSheet({ variant: 'ios' });
    buttonWithText('Not now')?.click();
    expect(onDismiss).toHaveBeenCalledOnce();
  });
});

describe('InstallSheet scrim vs card', () => {
  it('scrim click fires onDismiss', () => {
    const { onDismiss } = mountSheet();
    scrim()?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(onDismiss).toHaveBeenCalledOnce();
  });

  it('a click inside the card does not dismiss', () => {
    const { onDismiss, onInstall } = mountSheet({ variant: 'android' });
    dialog()?.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    expect(onDismiss).not.toHaveBeenCalled();
    expect(onInstall).not.toHaveBeenCalled();
  });
});

describe('InstallSheet accessibility', () => {
  it('the card is a labelled modal dialog', () => {
    mountSheet();
    const el = dialog();
    expect(el?.getAttribute('aria-modal')).toBe('true');

    const labelledBy = el?.getAttribute('aria-labelledby');
    expect(labelledBy).toBeTruthy();
    const heading = container.querySelector(`#${labelledBy}`);
    expect(heading?.textContent).toBe('Install lab');
  });
});
