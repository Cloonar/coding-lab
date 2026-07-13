// Spawn-form option resolution: repo default → settings default → first
// option (D12d layering), with unknown candidates skipped so a stale
// persisted value can never pre-select something the catalog lacks.

import { describe, expect, it } from 'vitest';
import type { Provider, ProviderModelOption } from '../api';
import { providerFor, resolveEffortOption, resolveSpawnOption } from './spawn';

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

describe('providerFor', () => {
  const auth = { kind: 'oauth-code' } as const;
  const providers: Provider[] = [
    {
      id: 'claude-code',
      display_name: 'Claude Code',
      models: MODELS,
      efforts: [],
      options: [],
      auth,
    },
    { id: 'other', display_name: 'Other', models: [], efforts: [], options: [], auth },
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
