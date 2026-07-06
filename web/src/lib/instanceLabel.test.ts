// Transcribed v0 contract tables (sessions-spawn port spec §2.7). The rows
// are the behavioral contract — do not trim or "improve" them.

import { describe, expect, it } from 'vitest';
import { instanceTitle, parseManualLabel, sessionLabel } from './instanceLabel';

describe('sessionLabel (parseSessionName label part, first-~ split)', () => {
  const rows: [string, string][] = [
    ['foo~20260608-1530', '20260608-1530'],
    ['foo~debug-20260608-1530', 'debug-20260608-1530'],
    ['foo~afk-7', 'afk-7'],
    ['foo-bar~20260608-1530', '20260608-1530'],
    ['foo~a~b', 'a~b'], // first ~ only; label verbatim (hand-made robustness)
    ['foo', ''], // no separator = legacy bare name
    ['foo~', ''],
  ];

  for (const [input, want] of rows) {
    it(`${JSON.stringify(input)} → ${JSON.stringify(want)}`, () => {
      expect(sessionLabel(input)).toBe(want);
    });
  }
});

describe('parseManualLabel (v0 table, complete)', () => {
  const rows: [string, string, string][] = [
    ['20260608-1530', '', '15:30'],
    ['debug-20260608-1530', 'debug', '15:30'],
    ['my-feature-20260608-0905', 'my-feature', '09:05'],
    ['20260608-0000', '', '00:00'],
    ['debug', 'debug', ''],
    ['', '', ''],
    ['123', '123', ''],
  ];

  for (const [label, user, hhmm] of rows) {
    it(`${JSON.stringify(label)} → user=${JSON.stringify(user)} hhmm=${JSON.stringify(hhmm)}`, () => {
      expect(parseManualLabel(label)).toEqual({ user, hhmm });
    });
  }
});

describe("instanceTitle ('label · 15:30' rendering like v0)", () => {
  const rows: [string, string][] = [
    ['20260608-1530', '15:30'],
    ['debug-20260608-1530', 'debug · 15:30'],
    ['my-feature-20260608-0905', 'my-feature · 09:05'],
    ['debug', 'debug'], // hand-made/legacy label renders verbatim
    ['afk-7', 'AFK #7'], // v0 badge string 'AFK #<N>'
    ['afk-auto-7', 'AFK #7'], // auto runs render the same title (kind chip differs)
    ['afk-feature', 'afk-feature'], // not an AFK label — verbatim like v0
    ['', ''],
  ];

  for (const [label, want] of rows) {
    it(`${JSON.stringify(label)} → ${JSON.stringify(want)}`, () => {
      expect(instanceTitle(label)).toBe(want);
    });
  }
});
