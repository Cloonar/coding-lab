// Spawn-form option resolution: repo default → settings default → first
// option (D12d layering), with unknown candidates skipped so a stale
// persisted value can never pre-select something the catalog lacks.

import { describe, expect, it } from 'vitest';
import type { Provider, ProviderModelOption } from '../api';
import { providerFor, resolveEffortOption, resolveRemote, resolveSpawnOption } from './spawn';

const MODELS: ProviderModelOption[] = [
  { value: 'opus[1m]', label: 'Opus (1M)', efforts: [] },
  { value: 'sonnet', label: 'Sonnet', efforts: [] },
  { value: 'fable', label: 'Fable', efforts: [] },
  { value: 'haiku', label: 'Haiku', efforts: [] },
];

describe('resolveSpawnOption', () => {
  it('prefers the first candidate present in the catalog', () => {
    expect(resolveSpawnOption(MODELS, 'sonnet', 'haiku')).toBe('sonnet');
  });

  it('skips null/empty/unknown candidates', () => {
    expect(resolveSpawnOption(MODELS, null, undefined, '', 'gpt-4', 'haiku')).toBe('haiku');
  });

  it('falls back to the first option when nothing matches', () => {
    expect(resolveSpawnOption(MODELS, 'gpt-4')).toBe('opus[1m]');
    expect(resolveSpawnOption(MODELS)).toBe('opus[1m]');
  });

  it('yields "" on an empty catalog', () => {
    expect(resolveSpawnOption([], 'sonnet')).toBe('');
  });
});

// Per-model effort resolution (issue #156): the effort catalog belongs to the
// SELECTED MODEL, and the model's reported default_effort beats the
// first-entry rule — mirroring the server's layerSpawnDefault effort pass.
describe('resolveEffortOption', () => {
  const EFFORTS = [
    { value: 'low', label: 'Low' },
    { value: 'medium', label: 'Medium' },
    { value: 'high', label: 'High' },
  ];
  const modelWith = (overrides: Partial<ProviderModelOption> = {}): ProviderModelOption => ({
    value: 'gpt-5.6-terra',
    label: 'GPT-5.6-Terra',
    efforts: EFFORTS,
    ...overrides,
  });

  it('prefers the first candidate present in the model catalog', () => {
    expect(resolveEffortOption(modelWith(), 'high', 'medium')).toBe('high');
  });

  it('skips null/empty/unknown candidates', () => {
    expect(resolveEffortOption(modelWith(), null, undefined, '', 'ultra', 'medium')).toBe('medium');
  });

  it('falls back to the reported default_effort when no candidate matches', () => {
    expect(resolveEffortOption(modelWith({ default_effort: 'medium' }), 'ultra')).toBe('medium');
    expect(resolveEffortOption(modelWith({ default_effort: 'medium' }))).toBe('medium');
  });

  it('a matching candidate beats the reported default_effort', () => {
    expect(resolveEffortOption(modelWith({ default_effort: 'medium' }), 'high')).toBe('high');
  });

  it('ignores a default_effort that is not a member of the model catalog', () => {
    expect(resolveEffortOption(modelWith({ default_effort: 'turbo' }))).toBe('low');
  });

  it('falls back to the first effort with no default_effort reported', () => {
    expect(resolveEffortOption(modelWith(), 'ultra')).toBe('low');
    expect(resolveEffortOption(modelWith())).toBe('low');
  });

  it('yields "" on a model without an effort knob', () => {
    expect(resolveEffortOption(modelWith({ efforts: [] }), 'high')).toBe('');
  });

  it('yields "" with no model at all', () => {
    expect(resolveEffortOption(undefined, 'high')).toBe('');
  });
});

// Remote-control resolution (issue #163): the same skip-layer walk, but over a
// BOOLEAN. Unlike every other knob here, `false` is a legal value — so "unset"
// is null/undefined, and an explicit false is a real answer that ends the walk.
describe('resolveRemote', () => {
  it('an explicit false EARLIER in the chain beats a true later', () => {
    // The whole point of the tri-state: a repo that turned remote control off
    // must not be overridden by an on global default.
    expect(resolveRemote(false, true)).toBe(false);
    expect(resolveRemote(null, false, true)).toBe(false);
    expect(resolveRemote(undefined, null, false, true, true)).toBe(false);
  });

  it('the first non-null layer wins', () => {
    expect(resolveRemote(true, false)).toBe(true);
    expect(resolveRemote(null, true, false)).toBe(true);
  });

  it('skips null and undefined layers (the only "unset" spellings)', () => {
    expect(resolveRemote(null, undefined, true)).toBe(true);
    expect(resolveRemote(undefined, undefined, false)).toBe(false);
  });

  it('defaults OFF when nothing is set anywhere', () => {
    expect(resolveRemote()).toBe(false);
    expect(resolveRemote(null, undefined, null)).toBe(false);
  });

  it('mirrors the AFK chain: repo AFK → global AFK → repo base → global base', () => {
    // Global base on, everything else inherit → on.
    expect(resolveRemote(null, null, null, true)).toBe(true);
    // The repo's own AFK override wins over an on global base.
    expect(resolveRemote(false, null, null, true)).toBe(false);
    // The global AFK override wins over the repo's manual default.
    expect(resolveRemote(null, true, false, false)).toBe(true);
    // Nothing AFK-specific → the manual layers decide.
    expect(resolveRemote(null, null, true, false)).toBe(true);
  });
});

describe('providerFor', () => {
  const auth = { kind: 'oauth-code' } as const;
  const providers: Provider[] = [
    {
      id: 'claude-code',
      display_name: 'Claude Code',
      models: MODELS,
      efforts: [],
      options: [],
      supports_remote: true,
      auth,
    },
    {
      id: 'other',
      display_name: 'Other',
      models: [],
      efforts: [],
      options: [],
      supports_remote: false,
      auth,
    },
  ];

  it('finds the repo provider by id', () => {
    expect(providerFor(providers, 'other')?.id).toBe('other');
  });

  it('falls back to the first provider on an unknown id', () => {
    expect(providerFor(providers, 'ghost')?.id).toBe('claude-code');
  });

  it('yields null when no providers exist', () => {
    expect(providerFor([], 'claude-code')).toBeNull();
  });

  it('walks candidates skip-layer: the first REGISTERED id wins', () => {
    expect(providerFor(providers, 'other', 'claude-code')?.id).toBe('other');
    expect(providerFor(providers, 'ghost', 'other', 'claude-code')?.id).toBe('other');
  });

  it('skips null, undefined and empty layers', () => {
    expect(providerFor(providers, null, undefined, '', 'other')?.id).toBe('other');
  });

  it('falls back to the first provider when every layer misses', () => {
    expect(providerFor(providers, 'ghost', null, '', undefined)?.id).toBe('claude-code');
    expect(providerFor(providers)?.id).toBe('claude-code');
  });

  it('yields null on an empty registry regardless of candidates', () => {
    expect(providerFor([], 'ghost', 'other', null)).toBeNull();
  });
});
