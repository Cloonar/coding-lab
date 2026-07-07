// Open-affordance logic (ADR-0017): a captured link always wins; the row
// pulses while capture runs; a finished capture with no link falls back to the
// provider's generic web link (from the providers API, not hardcoded); a
// provider with no web surface offers a copyable tmux-attach command instead.

import { describe, expect, it } from 'vitest';
import type { Provider } from '../api';
import { ATTACH_TITLE, openState, providerOpen } from './deepLink';

const claudeFallback = {
  url: 'https://claude.ai/code',
  title: "Opens the claude.ai session picker — the exact deep link wasn't captured",
};

const row = (over: Partial<{ connecting: boolean; deep_link_url: string | null }>) => ({
  connecting: false,
  deep_link_url: null,
  session_name: 'proj~dom-20260706-1500',
  ...over,
});

describe('openState', () => {
  it('shows the connecting pulse while capture runs and no link exists', () => {
    expect(openState(row({ connecting: true }), claudeFallback)).toEqual({ kind: 'connecting' });
  });

  it('renders the exact deep link once captured', () => {
    expect(
      openState(row({ deep_link_url: 'https://claude.ai/code/session_abc' }), claudeFallback),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('a captured link beats a still-set connecting flag', () => {
    expect(
      openState(
        row({ connecting: true, deep_link_url: 'https://claude.ai/code/session_abc' }),
        claudeFallback,
      ),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('falls back to the provider generic web link after a missed capture', () => {
    expect(openState(row({}), claudeFallback)).toEqual({
      kind: 'link',
      url: claudeFallback.url,
      exact: false,
      title: claudeFallback.title,
    });
  });

  it('treats an empty-string link like a missing one', () => {
    expect(openState(row({ deep_link_url: '' }), claudeFallback)).toMatchObject({
      kind: 'link',
      url: claudeFallback.url,
      exact: false,
    });
  });

  it('offers a copyable tmux-attach for a provider with no web surface', () => {
    expect(openState(row({}), null)).toEqual({
      kind: 'attach',
      command: 'tmux attach -t proj~dom-20260706-1500',
      title: ATTACH_TITLE,
    });
  });

  it('a captured link still wins even for a link-less provider', () => {
    expect(openState(row({ deep_link_url: 'https://example.test/x' }), null)).toEqual({
      kind: 'link',
      url: 'https://example.test/x',
      exact: true,
    });
  });

  it('pulses (not attach) while a link-less-but-connecting capture is in flight', () => {
    // Defensive: connecting still means "wait", never the attach fallthrough.
    expect(openState(row({ connecting: true }), null)).toEqual({ kind: 'connecting' });
  });

  it('renders nothing (unknown) on a miss until the providers list has loaded', () => {
    // undefined fallback = providers still loading; must NOT flash tmux-attach
    // on a web provider whose capture merely missed.
    expect(openState(row({}), undefined)).toEqual({ kind: 'unknown' });
  });

  it('an exact link still shows before providers load', () => {
    expect(
      openState(row({ deep_link_url: 'https://claude.ai/code/session_abc' }), undefined),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('the connecting pulse still shows before providers load', () => {
    expect(openState(row({ connecting: true }), undefined)).toEqual({ kind: 'connecting' });
  });
});

describe('providerOpen', () => {
  const providers: Provider[] = [
    { id: 'claude-code', models: [], efforts: [], options: [], fallback_open: claudeFallback },
    { id: 'codex', models: [], efforts: [], options: [] },
  ];

  it('returns a provider fallback-open by id', () => {
    expect(providerOpen(providers, 'claude-code')).toEqual(claudeFallback);
  });

  it('returns null for a provider without a web surface', () => {
    expect(providerOpen(providers, 'codex')).toBeNull();
  });

  it('returns null for an unknown provider', () => {
    expect(providerOpen(providers, 'nope')).toBeNull();
  });

  it('returns undefined while the providers list is still loading', () => {
    expect(providerOpen(undefined, 'claude-code')).toBeUndefined();
  });
});
