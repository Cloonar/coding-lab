// Spawn-form option resolution: repo default → settings default → first
// option (D12d layering), with unknown candidates skipped so a stale
// persisted value can never pre-select something the catalog lacks.

import { describe, expect, it } from 'vitest';
import type { Provider } from '../api';
import { providerFor, resolveSpawnOption } from './spawn';

const MODELS = [
  { value: 'opus[1m]', label: 'Opus (1M)' },
  { value: 'sonnet', label: 'Sonnet' },
  { value: 'fable', label: 'Fable' },
  { value: 'haiku', label: 'Haiku' },
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
