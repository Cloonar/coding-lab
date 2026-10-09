// The composer draft store (issue #92): the pure parse with garbage input, the
// round-trip through localStorage, the seven-day prune on read (the read entry
// and every other stale draft), and a localStorage that throws on every access.

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  DRAFT_MAX_AGE_MS,
  NEW_RUN_DRAFT_KEY,
  chatDraftKey,
  clearDraft,
  parseDraft,
  readDraft,
  writeDraft,
} from './drafts';

const asString = (v: unknown): string | null => (typeof v === 'string' ? v : null);
const NOW = 1_800_000_000_000;

describe('parseDraft', () => {
  it('reads a fresh entry', () => {
    expect(parseDraft(JSON.stringify({ v: 'hi', at: NOW - 1000 }), NOW)).toEqual({ value: 'hi' });
    expect(parseDraft(JSON.stringify({ v: { a: [1] }, at: NOW }), NOW)).toEqual({
      value: { a: [1] },
    });
  });

  it('drops missing, non-JSON and garbage input', () => {
    for (const raw of [
      null,
      '',
      'not json',
      '{"v":',
      '"a string"',
      '42',
      'null',
      '[1,2]',
      '{}',
      '{"v":"x"}',
      '{"v":"x","at":"yesterday"}',
      '{"at":1800000000000}',
    ]) {
      expect(parseDraft(raw, NOW), String(raw)).toBeNull();
    }
  });

  it('drops an entry older than seven days', () => {
    expect(parseDraft(JSON.stringify({ v: 'x', at: NOW - DRAFT_MAX_AGE_MS + 1 }), NOW)).toEqual({
      value: 'x',
    });
    expect(parseDraft(JSON.stringify({ v: 'x', at: NOW - DRAFT_MAX_AGE_MS - 1 }), NOW)).toBeNull();
  });
});

describe('draft storage', () => {
  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('round-trips under lab.draft.* keys and clears', () => {
    expect(chatDraftKey('run_1')).toBe('lab.draft.chat.run_1');
    expect(NEW_RUN_DRAFT_KEY).toBe('lab.draft.new-run');
    expect(readDraft(chatDraftKey('run_1'), asString)).toBeNull();
    writeDraft(chatDraftKey('run_1'), 'half a reply');
    expect(readDraft(chatDraftKey('run_1'), asString)).toBe('half a reply');
    expect(readDraft(chatDraftKey('run_2'), asString)).toBeNull();
    clearDraft(chatDraftKey('run_1'));
    expect(localStorage.getItem(chatDraftKey('run_1'))).toBeNull();
  });

  it('applies accept: a rejected or throwing shape reads as no draft', () => {
    writeDraft('lab.draft.x', 42);
    expect(readDraft('lab.draft.x', asString)).toBeNull();
    expect(
      readDraft('lab.draft.x', () => {
        throw new Error('bad shape');
      }),
    ).toBeNull();
  });

  it('prunes stale entries on read, leaving fresh drafts and other keys alone', () => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW - DRAFT_MAX_AGE_MS - 1000);
    writeDraft(chatDraftKey('old'), 'old');
    vi.setSystemTime(NOW);
    writeDraft(chatDraftKey('new'), 'new');
    localStorage.setItem('lab.draft.garbage', 'nope');
    localStorage.setItem('lab.last-repo', 'not a draft');

    expect(readDraft(chatDraftKey('old'), asString)).toBeNull();
    expect(localStorage.getItem(chatDraftKey('old'))).toBeNull();
    expect(localStorage.getItem('lab.draft.garbage')).toBeNull();
    expect(localStorage.getItem('lab.last-repo')).toBe('not a draft');
    expect(readDraft(chatDraftKey('new'), asString)).toBe('new');
  });

  it('survives a localStorage that throws on every access', () => {
    for (const m of ['getItem', 'setItem', 'removeItem', 'key'] as const) {
      vi.spyOn(Storage.prototype, m).mockImplementation(() => {
        throw new Error('denied');
      });
    }
    expect(() => writeDraft('lab.draft.x', 'a')).not.toThrow();
    expect(readDraft('lab.draft.x', asString)).toBeNull();
    expect(() => clearDraft('lab.draft.x')).not.toThrow();
  });
});
