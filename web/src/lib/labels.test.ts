// Label helpers: hex normalization + contrast for the colored chips, and the
// selection algebra (toggle + set-equality) the label editor's dirty gate and
// multi-select are built on.

import { describe, expect, it } from 'vitest';
import {
  DEFAULT_LABEL_COLOR,
  labelChipStyle,
  normalizeHex,
  readableTextColor,
  sameLabelSet,
  toggleLabel,
} from './labels';

describe('normalizeHex', () => {
  it('normalizes 6-digit hex with or without # to lowercase #rrggbb', () => {
    expect(normalizeHex('#0E8A16')).toBe('#0e8a16');
    expect(normalizeHex('0e8a16')).toBe('#0e8a16');
    expect(normalizeHex('  #0e8a16  ')).toBe('#0e8a16');
  });

  it('expands 3-digit hex', () => {
    expect(normalizeHex('#f0a')).toBe('#ff00aa');
    expect(normalizeHex('abc')).toBe('#aabbcc');
  });

  it('rejects anything else', () => {
    expect(normalizeHex('')).toBe('');
    expect(normalizeHex('#12345')).toBe('');
    expect(normalizeHex('red')).toBe('');
    expect(normalizeHex('#0e8a1g')).toBe('');
  });
});

describe('readableTextColor', () => {
  it('puts black text on light backgrounds and white on dark', () => {
    expect(readableTextColor('#fbca04')).toBe('#000000'); // triage yellow
    expect(readableTextColor('#cccccc')).toBe('#000000'); // wontfix grey
    expect(readableTextColor('#0e8a16')).toBe('#ffffff'); // ready-for-agent green
    expect(readableTextColor('#1d76db')).toBe('#ffffff'); // ready-for-human blue
  });

  it('falls back to black for garbage input', () => {
    expect(readableTextColor('nope')).toBe('#000000');
  });
});

describe('labelChipStyle', () => {
  it('pairs the normalized background with its contrast color', () => {
    expect(labelChipStyle('#0E8A16')).toEqual({ background: '#0e8a16', color: '#ffffff' });
  });

  it('falls back to the store default color on empty/invalid input', () => {
    expect(labelChipStyle('').background).toBe(DEFAULT_LABEL_COLOR);
    expect(labelChipStyle('garbage').background).toBe(DEFAULT_LABEL_COLOR);
  });
});

describe('toggleLabel (label-editor selection)', () => {
  it('adds an absent name at the end', () => {
    expect(toggleLabel(['bug'], 'ui')).toEqual(['bug', 'ui']);
    expect(toggleLabel([], 'bug')).toEqual(['bug']);
  });

  it('removes a present name, keeping the rest in order', () => {
    expect(toggleLabel(['bug', 'ui', 'wontfix'], 'ui')).toEqual(['bug', 'wontfix']);
  });

  it('never mutates the input', () => {
    const input = ['bug'];
    toggleLabel(input, 'ui');
    toggleLabel(input, 'bug');
    expect(input).toEqual(['bug']);
  });
});

describe('sameLabelSet (the dirty gate)', () => {
  it('is order-insensitive set equality', () => {
    expect(sameLabelSet(['a', 'b'], ['b', 'a'])).toBe(true);
    expect(sameLabelSet([], [])).toBe(true);
  });

  it('detects additions and removals', () => {
    expect(sameLabelSet(['a'], ['a', 'b'])).toBe(false);
    expect(sameLabelSet(['a', 'b'], ['a'])).toBe(false);
    expect(sameLabelSet(['a'], ['b'])).toBe(false);
  });
});
