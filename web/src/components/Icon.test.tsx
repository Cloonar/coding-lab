// Icon contract: every vendored glyph renders as a decorative 24x24 outline
// (aria-hidden, currentColor stroke, no fill) with at least one drawn element,
// at the default 20px or a given size. Issue #61 vendored the redesign's set;
// the list below pins the names the repositories views rely on.

import { render } from 'solid-js/web';
import { afterEach, describe, expect, it } from 'vitest';
import Icon, { ICON_NAMES, type IconName } from './Icon';

let dispose: (() => void) | undefined;
let container: HTMLDivElement;

afterEach(() => {
  dispose?.();
  dispose = undefined;
  container.remove();
});

function mount(name: IconName, size?: number): SVGSVGElement {
  container = document.createElement('div');
  document.body.appendChild(container);
  dispose = render(() => <Icon name={name} size={size} />, container);
  return container.querySelector('svg')!;
}

describe('Icon', () => {
  for (const name of ICON_NAMES) {
    it(`draws ${name} as a decorative outline`, () => {
      const svg = mount(name);
      expect(svg.getAttribute('aria-hidden')).toBe('true');
      expect(svg.getAttribute('viewBox')).toBe('0 0 24 24');
      expect(svg.getAttribute('fill')).toBe('none');
      expect(svg.getAttribute('stroke')).toBe('currentColor');
      expect(svg.getAttribute('width')).toBe('20');
      expect(svg.children.length).toBeGreaterThan(0);
      // Namespaced as SVG, not stray HTML elements.
      expect(Array.from(svg.children).every((c) => c.namespaceURI === svg.namespaceURI)).toBe(true);
    });
  }

  it('takes a size', () => {
    const svg = mount('search', 16);
    expect(svg.getAttribute('width')).toBe('16');
    expect(svg.getAttribute('height')).toBe('16');
  });

  it('carries every glyph the repositories redesign uses', () => {
    const needed: IconName[] = [
      'menu',
      'chevron-right',
      'chevron-left',
      'chevron-down',
      'chevron-up',
      'plus',
      'search',
      'x',
      'folder',
      'history',
      'key',
      'ticket',
      'settings',
      'sliders-horizontal',
      'settings-2',
      'plug',
      'git-branch',
      'bot',
      'box',
      'container',
      'git-merge',
      'lock',
      'calendar',
      'calendar-clock',
      'arrow-down-to-line',
      'folder-input',
      'triangle-alert',
      'circle-alert',
      'exclamation',
      'play',
      'zap',
      'check',
      'circle-check',
      'clock',
      'trash-2',
      'rotate-ccw',
      'undo-2',
    ];
    expect(needed.filter((name) => !ICON_NAMES.includes(name))).toEqual([]);
  });
});
