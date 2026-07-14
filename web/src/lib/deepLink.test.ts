// Open-affordance logic (ADR-0017): a captured link always wins; the row
// pulses while capture runs; a finished capture with no link falls back to the
// provider's generic web link (from the providers API, not hardcoded); a
// provider with no web surface offers a copyable tmux-attach command instead.
//
// On top of that, the remote-control gate (issue #163): the whole affordance
// disappears for a remote-CAPABLE provider's run that spawned with remote
// control off — and for nobody else.

import { describe, expect, it } from 'vitest';
import type { Provider } from '../api';
import {
  ATTACH_TITLE,
  openState,
  providerOpen,
  providerSupportsRemote,
  remoteGated,
} from './deepLink';

const claudeFallback = {
  url: 'https://claude.ai/code',
  title: "Opens the claude.ai session picker — the exact deep link wasn't captured",
};

// A remote-controlled run by default: that is the only state in which a deep
// link (or its web fallback) can exist at all.
const row = (
  over: Partial<{ connecting: boolean; deep_link_url: string | null; remote: boolean }>,
) => ({
  connecting: false,
  deep_link_url: null,
  session_name: 'proj~dom-20260706-1500',
  remote: true,
  ...over,
});

describe('openState', () => {
  it('shows the connecting pulse while capture runs and no link exists', () => {
    expect(openState(row({ connecting: true }), claudeFallback, true)).toEqual({
      kind: 'connecting',
    });
  });

  it('renders the exact deep link once captured', () => {
    expect(
      openState(row({ deep_link_url: 'https://claude.ai/code/session_abc' }), claudeFallback, true),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('a captured link beats a still-set connecting flag', () => {
    expect(
      openState(
        row({ connecting: true, deep_link_url: 'https://claude.ai/code/session_abc' }),
        claudeFallback,
        true,
      ),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('falls back to the provider generic web link after a missed capture', () => {
    expect(openState(row({}), claudeFallback, true)).toEqual({
      kind: 'link',
      url: claudeFallback.url,
      exact: false,
      title: claudeFallback.title,
    });
  });

  it('treats an empty-string link like a missing one', () => {
    expect(openState(row({ deep_link_url: '' }), claudeFallback, true)).toMatchObject({
      kind: 'link',
      url: claudeFallback.url,
      exact: false,
    });
  });

  it('offers a copyable tmux-attach for a provider with no web surface', () => {
    // A link-less provider has no remote knob either, so its runs are clamped
    // to remote:false server-side — the affordance must survive that.
    expect(openState(row({ remote: false }), null, false)).toEqual({
      kind: 'attach',
      command: 'tmux attach -t proj~dom-20260706-1500',
      title: ATTACH_TITLE,
    });
  });

  it('a captured link still wins even for a link-less provider', () => {
    expect(
      openState(row({ remote: false, deep_link_url: 'https://example.test/x' }), null, false),
    ).toEqual({
      kind: 'link',
      url: 'https://example.test/x',
      exact: true,
    });
  });

  it('pulses (not attach) while a link-less-but-connecting capture is in flight', () => {
    // Defensive: connecting still means "wait", never the attach fallthrough.
    expect(openState(row({ remote: false, connecting: true }), null, false)).toEqual({
      kind: 'connecting',
    });
  });

  it('renders nothing (unknown) on a miss until the providers list has loaded', () => {
    // undefined fallback = providers still loading; must NOT flash tmux-attach
    // on a web provider whose capture merely missed.
    expect(openState(row({}), undefined, undefined)).toEqual({ kind: 'unknown' });
  });

  it('an exact link still shows before providers load', () => {
    expect(
      openState(row({ deep_link_url: 'https://claude.ai/code/session_abc' }), undefined, undefined),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('the connecting pulse still shows before providers load', () => {
    expect(openState(row({ connecting: true }), undefined, undefined)).toEqual({
      kind: 'connecting',
    });
  });
});

// The capability-scoped gate (issue #163). The four combinations of
// (provider supports remote) × (run spawned remote):
describe('openState remote-control gate', () => {
  it('supporting provider + remote ON: unchanged — the captured link renders', () => {
    expect(
      openState(
        row({ remote: true, deep_link_url: 'https://claude.ai/code/session_abc' }),
        claudeFallback,
        true,
      ),
    ).toEqual({ kind: 'link', url: 'https://claude.ai/code/session_abc', exact: true });
  });

  it('supporting provider + remote OFF: nothing at all — no link, no fallback, no pulse', () => {
    // There is no registered web session, so even the generic fallback link
    // would point at a session that does not exist.
    expect(openState(row({ remote: false }), claudeFallback, true)).toBeNull();
    expect(openState(row({ remote: false, connecting: true }), claudeFallback, true)).toBeNull();
    // Even a (impossible) stale captured link is withheld: the run is not remote.
    expect(
      openState(
        row({ remote: false, deep_link_url: 'https://claude.ai/code/session_abc' }),
        claudeFallback,
        true,
      ),
    ).toBeNull();
  });

  it('NON-supporting provider + remote OFF: completely unaffected — keeps tmux attach', () => {
    // The regression a bare `if (!run.remote) return null` would cause: a
    // link-less provider is ALWAYS remote:false (the server clamps it), so its
    // only way into the session must not be gated on the flag.
    expect(openState(row({ remote: false }), null, false)).toEqual({
      kind: 'attach',
      command: 'tmux attach -t proj~dom-20260706-1500',
      title: ATTACH_TITLE,
    });
    // …and a non-supporting provider that DOES have a web surface keeps it too.
    expect(openState(row({ remote: false }), claudeFallback, false)).toMatchObject({
      kind: 'link',
      exact: false,
    });
  });

  it('NON-supporting provider + remote ON: still unaffected (the flag is moot)', () => {
    expect(openState(row({ remote: true }), null, false)).toMatchObject({ kind: 'attach' });
  });

  it('hides nothing while the providers list is still loading', () => {
    // Capability unknown → never guess: the pre-existing "unknown" degradation
    // covers this window, and a captured link still renders.
    expect(openState(row({ remote: false }), undefined, undefined)).toEqual({ kind: 'unknown' });
  });
});

describe('providerOpen', () => {
  const providers: Provider[] = [
    {
      id: 'claude-code',
      display_name: 'Claude Code',
      models: [],
      efforts: [],
      options: [],
      supports_remote: true,
      auth: { kind: 'oauth-code' },
      fallback_open: claudeFallback,
    },
    {
      id: 'codex',
      display_name: 'Codex CLI',
      models: [],
      efforts: [],
      options: [],
      supports_remote: false,
      auth: { kind: 'external' },
    },
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

  it('reports the remote-control capability per provider', () => {
    expect(providerSupportsRemote(providers, 'claude-code')).toBe(true);
    expect(providerSupportsRemote(providers, 'codex')).toBe(false);
  });

  it('reports an unknown provider as non-supporting, and a loading list as unknown', () => {
    // Nothing provable → nothing gated; undefined keeps the "never guess" rule.
    expect(providerSupportsRemote(providers, 'nope')).toBe(false);
    expect(providerSupportsRemote(undefined, 'claude-code')).toBeUndefined();
  });
});

// The gate is exported on its own because the Open affordance is not its only
// consumer: RunChat's "answer it elsewhere" hint names the provider's web host
// in prose, and a hint reading "open it at claude.ai" beside a hidden Open
// button is the same broken promise spelled in words. Pinning it here keeps the
// two surfaces from drifting.
describe('remoteGated', () => {
  it('gates a remote-capable provider run that spawned with remote control off', () => {
    expect(remoteGated({ remote: false }, true)).toBe(true);
  });

  it('does not gate a remote-controlled run', () => {
    expect(remoteGated({ remote: true }, true)).toBe(false);
  });

  it('never gates a provider without the knob, whose runs are always remote:false', () => {
    // The capability scope: a bare !remote here would strip codex's tmux-attach
    // affordance and mute its hint, for a feature codex does not participate in.
    expect(remoteGated({ remote: false }, false)).toBe(false);
  });

  it('gates nothing while the providers list is still loading', () => {
    expect(remoteGated({ remote: false }, undefined)).toBe(false);
  });
});
