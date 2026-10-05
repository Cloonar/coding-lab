// How an inherited value is worded (issue #61): pure — the value is the
// server's answer, and this only turns it into the text the page prints after
// "Inherited · " and "Default: ". A value that is not known words as null:
// the page then shows the state without a value, never a guessed one.

import { describe, expect, it } from 'vitest';
import type { Provider, RepoInherited } from '../../api';
import { OVERRIDABLE_FIELD_KEYS, REPO_FIELD_KEYS, isOverridable } from './fields';
import { CODEX, baseProviders } from './harness';
import { AGENT_DEFAULT, CHAIN_FIELD_KEYS, inheritedText, type InheritedWording } from './inherited';

const AGENT: Provider = baseProviders()[0]!;

const WORDING: InheritedWording = {
  providers: [AGENT, CODEX],
  baseProvider: AGENT,
  afkProvider: AGENT,
  landerProvider: CODEX,
  afkBoolOptions: AGENT.options,
};

const ANSWER: RepoInherited = {
  provider: AGENT.id,
  afk_provider_default: AGENT.id,
  lander_provider: CODEX.id,
  model_default: 'sonnet',
  effort_default: 'high',
  afk_model_default: 'opus[1m]',
  afk_effort_default: 'high',
  lander_model: 'gpt-5-codex',
  lander_effort: 'medium',
  remote_default: true,
  afk_remote_default: false,
  afk_options: { ultracode: 'true' },
  budget_minutes: 120,
  max_instances_override: 6,
  git_author_name: 'lab-bot',
  git_author_email: 'lab-bot@example.com',
  runner: 'container',
  image_ref: 'ghcr.io/cloonar/dev:1.4@sha256:0123',
  container_memory: '8g',
  container_pids: 4096,
  container_nofile: 16384,
};

const text = (
  key: Parameters<typeof inheritedText>[0],
  answer: RepoInherited | undefined = ANSWER,
) => inheritedText(key, answer, WORDING);

describe('inheritedText', () => {
  it('names an agent by its display name from the catalog', () => {
    expect(text('provider')).toBe(AGENT.display_name);
    expect(text('afk_provider_default')).toBe(AGENT.display_name);
    expect(text('lander_provider')).toBe(CODEX.display_name);
  });

  it('labels a model and an effort from the catalog of the run class they belong to', () => {
    expect(text('model_default')).toBe('Sonnet');
    expect(text('afk_model_default')).toBe('Opus (1M)');
    // The lander resolves to another provider: its catalog words its model.
    expect(text('lander_model')).toBe('GPT-5 Codex');
    expect(text('effort_default')).toBe('high');
    expect(text('lander_effort')).toBe('medium');
  });

  it('shows an id the catalog does not carry as it is', () => {
    expect(text('provider', { ...ANSWER, provider: 'retired-agent' })).toBe('retired-agent');
    expect(text('model_default', { ...ANSWER, model_default: 'weird-model' })).toBe('weird-model');
    expect(inheritedText('model_default', ANSWER, { ...WORDING, baseProvider: null })).toBe(
      'sonnet',
    );
  });

  it("words an empty model or effort as the provider's own default", () => {
    expect(text('model_default', { ...ANSWER, model_default: '' })).toBe(AGENT_DEFAULT);
    expect(text('afk_effort_default', { ...ANSWER, afk_effort_default: '' })).toBe(AGENT_DEFAULT);
    expect(text('lander_effort', { ...ANSWER, lander_effort: '' })).toBe(AGENT_DEFAULT);
  });

  it('reads a boolean as on / off and a Runner by its name', () => {
    expect(text('remote_default')).toBe('on');
    expect(text('afk_remote_default')).toBe('off');
    expect(text('runner')).toBe('Container');
    expect(text('runner', { ...ANSWER, runner: 'host' })).toBe('Host');
  });

  it('prints numbers and text as they are', () => {
    expect(text('budget_minutes')).toBe('120');
    expect(text('max_instances_override')).toBe('6');
    expect(text('container_pids')).toBe('4096');
    expect(text('container_nofile')).toBe('16384');
    expect(text('container_memory')).toBe('8g');
    expect(text('git_author_name')).toBe('lab-bot');
    expect(text('git_author_email')).toBe('lab-bot@example.com');
    expect(text('image_ref')).toBe('ghcr.io/cloonar/dev:1.4@sha256:0123');
  });

  it('says so when nothing is set below the repo', () => {
    expect(text('git_author_name', { ...ANSWER, git_author_name: '' })).toBe('none set');
    expect(text('git_author_email', { ...ANSWER, git_author_email: '' })).toBe('none set');
    // '' = no dev image is configured anywhere below the repo.
    expect(text('image_ref', { ...ANSWER, image_ref: '' })).toBe('none configured');
  });

  it('words an option bag by the options it switches on', () => {
    expect(text('afk_options')).toBe('Ultracode (multi-agent workflows) on');
    expect(text('afk_options', { ...ANSWER, afk_options: {} })).toBe('all off');
    expect(text('afk_options', { ...ANSWER, afk_options: { ultracode: 'false' } })).toBe('all off');
  });

  it('is null for every entry the server could not resolve', () => {
    const unresolved = Object.fromEntries(
      Object.keys(ANSWER).map((key) => [key, null]),
    ) as unknown as RepoInherited;
    for (const key of OVERRIDABLE_FIELD_KEYS) {
      if (key === 'afk_prompt') continue; // rides the repo, not this answer
      expect(text(key, unresolved)).toBeNull();
    }
  });

  it('is null while the answer is not there — loading, or a failed request', () => {
    for (const key of OVERRIDABLE_FIELD_KEYS) {
      if (key === 'afk_prompt') continue;
      expect(inheritedText(key, undefined, WORDING)).toBeNull();
    }
  });

  it('words the seed prompt without the answer: its text rides the repo', () => {
    expect(inheritedText('afk_prompt', undefined, WORDING)).toBe('the inherited seed prompt');
  });

  it('has nothing to say for a field that cannot inherit', () => {
    for (const key of REPO_FIELD_KEYS.filter((k) => !isOverridable(k))) {
      expect(text(key)).toBeNull();
    }
  });

  it('covers every key of the answer: each overridable field but the seed prompt', () => {
    expect(Object.keys(ANSWER).sort()).toEqual(
      OVERRIDABLE_FIELD_KEYS.filter((key) => key !== 'afk_prompt').sort(),
    );
    for (const key of OVERRIDABLE_FIELD_KEYS) expect(text(key)).not.toBeNull();
  });
});

describe('chain fields', () => {
  it('are the drafts the server accepts with the question — all of them overridable', () => {
    expect([...CHAIN_FIELD_KEYS].sort()).toEqual(
      [
        'provider',
        'model_default',
        'effort_default',
        'remote_default',
        'afk_provider_default',
        'afk_model_default',
        'afk_effort_default',
        'lander_provider',
        // The inherited lander effort is resolved against the lander's model.
        'lander_model',
      ].sort(),
    );
    for (const key of CHAIN_FIELD_KEYS) expect(isOverridable(key)).toBe(true);
  });
});
