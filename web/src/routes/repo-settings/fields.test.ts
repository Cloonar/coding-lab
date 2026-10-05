// The repo settings field table (issue #61): pure checks of what the one save
// rule is built from — page order and sections, each field's '' <-> null
// normalisation, the AFK option bag rule, and every rule the browser checks
// before a Save sends anything.

import { describe, expect, it } from 'vitest';
import type { Repo, RepoPatch } from '../../api';
import { REPO_SETTINGS_CATEGORIES } from './categories';
import {
  REPO_FIELDS,
  REPO_FIELD_KEYS,
  buildRepoPatch,
  changedFields,
  isRepoFieldKey,
  repoField,
  validateDraft,
  validateEdits,
  type FieldContext,
  type RepoEdits,
} from './fields';
import { baseRepo } from './harness';

const NO_OPTIONS: FieldContext = { afkOptionKeys: [] };
const patchOf = (edits: RepoEdits, repo: Repo = baseRepo(), context = NO_OPTIONS): RepoPatch =>
  buildRepoPatch(edits, repo, context);

describe('repo settings field table', () => {
  it('lists the fields in page order: the sections follow the page, the form ones only', () => {
    const sections = REPO_FIELD_KEYS.map((key) => repoField(key).section).filter(
      (section, index, all) => all.indexOf(section) === index,
    );
    const pageOrder = REPO_SETTINGS_CATEGORIES.map((category) => category.slug).filter((slug) =>
      (sections as string[]).includes(slug),
    );
    expect(sections).toEqual(pageOrder);
    expect(sections).toEqual([
      'agents',
      'runner',
      'autoland',
      'general',
      'integrations',
      'branches',
    ]);
  });

  it('keys every entry by its own PATCH key', () => {
    for (const key of REPO_FIELD_KEYS) expect(REPO_FIELDS[key].key).toBe(key);
    expect(REPO_FIELD_KEYS).toHaveLength(34);
  });

  it('tells a field key from any other string', () => {
    expect(isRepoFieldKey('forge_credential_id')).toBe(true);
    expect(isRepoFieldKey('afk_branch_pattern')).toBe(true);
    expect(isRepoFieldKey('toString')).toBe(false);
    expect(isRepoFieldKey('remote_url')).toBe(false);
    expect(isRepoFieldKey('')).toBe(false);
    expect(isRepoFieldKey(undefined)).toBe(false);
  });

  it('seeds every draft from a saved repo without an edit reading as a change', () => {
    const repo: Repo = {
      ...baseRepo(),
      provider: 'claude-code',
      remote_default: false,
      afk_options: { ultracode: 'true' },
      budget_minutes: 45,
      runner: null,
      container_pids: 2048,
      image_ref: 'docker.io/library/debian:bookworm@sha256:abc',
    };
    const edits: RepoEdits = {};
    for (const key of REPO_FIELD_KEYS) {
      (edits as Record<string, unknown>)[key] = repoField(key).seed(repo);
    }
    expect(changedFields(edits, repo, { afkOptionKeys: ['ultracode'] })).toEqual([]);
    expect(buildRepoPatch(edits, repo, { afkOptionKeys: ['ultracode'] })).toEqual({});
  });
});

describe('buildRepoPatch: only the changed fields, in their wire form', () => {
  it('sends nothing for no edits', () => {
    expect(patchOf({})).toEqual({});
  });

  it('nullable text: a value is trimmed, blank is null', () => {
    expect(patchOf({ git_author_name: '  Dominik ' })).toEqual({ git_author_name: 'Dominik' });
    const named = { ...baseRepo(), git_author_name: 'Dominik' };
    expect(patchOf({ git_author_name: '   ' }, named)).toEqual({ git_author_name: null });
    // Whitespace around an unchanged value is no change.
    expect(patchOf({ git_author_name: ' Dominik ' }, named)).toEqual({});
    expect(patchOf({ git_author_name: '  ' })).toEqual({});
  });

  it('required text is trimmed', () => {
    expect(patchOf({ name: ' lab-core ' })).toEqual({ name: 'lab-core' });
    expect(patchOf({ default_branch: ' trunk ' })).toEqual({ default_branch: 'trunk' });
    expect(patchOf({ afk_branch_pattern: ' issue-<N> ' })).toEqual({
      afk_branch_pattern: 'issue-<N>',
    });
    expect(patchOf({ manual_branch_prefix: ' wip/ ' })).toEqual({ manual_branch_prefix: 'wip/' });
    expect(patchOf({ name: 'coding-lab ' })).toEqual({});
  });

  it('nullable integers: a number, or null for blank', () => {
    expect(patchOf({ budget_minutes: '45' })).toEqual({ budget_minutes: 45 });
    expect(patchOf({ max_instances_override: ' 3 ' })).toEqual({ max_instances_override: 3 });
    const set = { ...baseRepo(), container_pids: 2048, container_nofile: 8192 };
    expect(patchOf({ container_pids: '', container_nofile: '' }, set)).toEqual({
      container_pids: null,
      container_nofile: null,
    });
    expect(patchOf({ container_pids: '2048' }, set)).toEqual({});
  });

  it('max fix attempts is a plain integer', () => {
    expect(patchOf({ max_fix_attempts: '5' })).toEqual({ max_fix_attempts: 5 });
    expect(patchOf({ max_fix_attempts: '0' })).toEqual({ max_fix_attempts: 0 });
    expect(patchOf({ max_fix_attempts: '2' })).toEqual({});
  });

  it('remote control is tri-state: null inherits, false is an explicit off', () => {
    expect(patchOf({ remote_default: 'false' })).toEqual({ remote_default: false });
    expect(patchOf({ afk_remote_default: 'true' })).toEqual({ afk_remote_default: true });
    const pinned = { ...baseRepo(), remote_default: false };
    expect(patchOf({ remote_default: '' }, pinned)).toEqual({ remote_default: null });
    expect(patchOf({ remote_default: 'false' }, pinned)).toEqual({});
  });

  it('picks: "" is null — the runner, the agents, the credentials', () => {
    expect(patchOf({ runner: '' })).toEqual({ runner: null }); // baseRepo pins host
    expect(patchOf({ runner: 'container' })).toEqual({ runner: 'container' });
    expect(patchOf({ provider: 'codex' })).toEqual({ provider: 'codex' });
    expect(patchOf({ credential_id: 'cred_git' })).toEqual({ credential_id: 'cred_git' });
    const withCredential = { ...baseRepo(), credential_id: 'cred_git' };
    expect(patchOf({ credential_id: '' }, withCredential)).toEqual({ credential_id: null });
    expect(patchOf({ tracker_binding: 'builtin' })).toEqual({ tracker_binding: 'builtin' });
  });

  it('switches send their boolean', () => {
    expect(patchOf({ incogni: true, afk_auto_enabled: true, auto_merge: false })).toEqual({
      incogni: true,
      afk_auto_enabled: true,
      auto_merge: false,
    });
    expect(patchOf({ incogni: false, auto_merge: true })).toEqual({});
  });

  it('the option bag: the FULL declared bag once any declared option differs', () => {
    const context: FieldContext = { afkOptionKeys: ['ultracode', 'plan'] };
    expect(patchOf({ afk_options: { ultracode: true } }, baseRepo(), context)).toEqual({
      afk_options: { ultracode: 'true', plan: 'false' },
    });
    // Nothing declared differs: no bag is sent, whatever else the draft holds.
    expect(patchOf({ afk_options: { ultracode: false } }, baseRepo(), context)).toEqual({});
    expect(patchOf({ afk_options: { retired: true } }, baseRepo(), context)).toEqual({});
    // No declared options at all: the bag never enters a PATCH.
    expect(patchOf({ afk_options: { ultracode: true } })).toEqual({});
    // Against a stored bag: unchecking is the change.
    const stored = { ...baseRepo(), afk_options: { ultracode: 'true' } };
    expect(patchOf({ afk_options: { ultracode: false } }, stored, context)).toEqual({
      afk_options: { ultracode: 'false', plan: 'false' },
    });
    expect(patchOf({ afk_options: { ultracode: true } }, stored, context)).toEqual({});
  });

  it('several sections at once: one patch with every changed field and nothing else', () => {
    const edits: RepoEdits = {
      model_default: 'sonnet',
      budget_minutes: '90',
      afk_branch_pattern: 'issue-<N>',
      name: 'coding-lab', // unchanged
    };
    expect(changedFields(edits, baseRepo(), NO_OPTIONS)).toEqual([
      'model_default',
      'budget_minutes',
      'afk_branch_pattern',
    ]);
    expect(patchOf(edits)).toEqual({
      model_default: 'sonnet',
      budget_minutes: 90,
      afk_branch_pattern: 'issue-<N>',
    });
  });
});

describe('validation in the browser', () => {
  it('the name, the default branch and the manual prefix may not be empty', () => {
    expect(validateDraft('name', '   ')).toBe('Enter a name.');
    expect(validateDraft('name', 'lab')).toBeNull();
    expect(validateDraft('default_branch', '')).toBe('Enter the default branch.');
    expect(validateDraft('default_branch', 'main')).toBeNull();
    expect(validateDraft('manual_branch_prefix', ' ')).toBe('Enter a prefix, for example lab/.');
    expect(validateDraft('manual_branch_prefix', 'lab/')).toBeNull();
  });

  it('the AFK branch pattern contains <N> exactly once', () => {
    const message = 'The pattern needs <N> exactly once. It stands for the issue number.';
    expect(validateDraft('afk_branch_pattern', 'afk/')).toBe(message);
    expect(validateDraft('afk_branch_pattern', '')).toBe(message);
    expect(validateDraft('afk_branch_pattern', 'afk/<N>/<N>')).toBe(message);
    expect(validateDraft('afk_branch_pattern', 'afk/<n>')).toBe(message);
    expect(validateDraft('afk_branch_pattern', 'afk/<N>')).toBeNull();
    expect(validateDraft('afk_branch_pattern', 'issue-<N>')).toBeNull();
  });

  it('the overridable numbers are whole numbers of 1 or more, or blank', () => {
    const message = 'Use a whole number, 1 or more, or leave it empty.';
    for (const key of [
      'budget_minutes',
      'max_instances_override',
      'container_pids',
      'container_nofile',
    ] as const) {
      expect(validateDraft(key, '')).toBeNull();
      expect(validateDraft(key, '1')).toBeNull();
      expect(validateDraft(key, '4096')).toBeNull();
      expect(validateDraft(key, '0')).toBe(message);
      expect(validateDraft(key, '-3')).toBe(message);
      expect(validateDraft(key, '1.5')).toBe(message);
      expect(validateDraft(key, 'many')).toBe(message);
    }
  });

  it('max fix attempts is a whole number of 0 or more, never blank', () => {
    const message = 'Use a whole number, 0 or more.';
    expect(validateDraft('max_fix_attempts', '0')).toBeNull();
    expect(validateDraft('max_fix_attempts', '7')).toBeNull();
    expect(validateDraft('max_fix_attempts', '')).toBe(message);
    expect(validateDraft('max_fix_attempts', '-1')).toBe(message);
    expect(validateDraft('max_fix_attempts', '2.5')).toBe(message);
  });

  it('checks the changed fields only, and reports them by key', () => {
    const edits: RepoEdits = {
      afk_branch_pattern: 'afk/',
      budget_minutes: '0',
      git_author_name: 'Dominik',
    };
    expect(validateEdits(edits, baseRepo(), NO_OPTIONS)).toEqual({
      afk_branch_pattern: 'The pattern needs <N> exactly once. It stands for the issue number.',
      budget_minutes: 'Use a whole number, 1 or more, or leave it empty.',
    });
    // An untouched field is never sent, so a stored oddity never blocks a save.
    const legacy = { ...baseRepo(), afk_branch_pattern: 'legacy' };
    expect(validateEdits({ git_author_name: 'Dominik' }, legacy, NO_OPTIONS)).toEqual({});
    expect(validateEdits({ afk_branch_pattern: 'legacy ' }, legacy, NO_OPTIONS)).toEqual({});
  });
});
