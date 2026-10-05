// rescueFocus contract: it moves focus to the first connected target only
// when focus fell to <body> (or onto a removed element), and never steals
// focus the operator placed somewhere else.

import { afterEach, describe, expect, it } from 'vitest';
import { focusLost, rescueFocus } from './focus';

const made: HTMLElement[] = [];
function button(label: string, attach = true): HTMLButtonElement {
  const el = document.createElement('button');
  el.textContent = label;
  if (attach) document.body.appendChild(el);
  made.push(el);
  return el;
}

afterEach(() => {
  for (const el of made.splice(0)) el.remove();
});

describe('rescueFocus', () => {
  it('focuses the first connected target once focus fell to the body', () => {
    const doomed = button('Retry');
    doomed.focus();
    doomed.remove(); // the control went away with its row
    expect(focusLost()).toBe(true);

    const detached = button('Gone', false);
    const heading = button('Heading');
    expect(rescueFocus(detached, null, undefined, heading)).toBe(true);
    expect(document.activeElement).toBe(heading);
  });

  it('leaves focus alone when it is somewhere useful', () => {
    const elsewhere = button('Elsewhere');
    elsewhere.focus();
    const heading = button('Heading');
    expect(focusLost()).toBe(false);
    expect(rescueFocus(heading)).toBe(false);
    expect(document.activeElement).toBe(elsewhere);
  });

  it('does nothing without a target on the page', () => {
    (document.activeElement as HTMLElement | null)?.blur();
    expect(rescueFocus(null, button('Detached', false))).toBe(false);
    expect(document.activeElement).toBe(document.body);
  });
});
