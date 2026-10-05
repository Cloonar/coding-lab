// The repo settings field wrapper (issue #61), through the real page: every
// field has a label that names its control, a hint and a problem that
// describe it, and a changed mark at the label made of a dot AND words — so
// the state never rests on colour alone and reaches assistive tech as part of
// the control's name.

import { describe, expect, it } from 'vitest';
import {
  CODEX,
  REPO_ID,
  baseProviders,
  baseRepo,
  chooseFromSelect,
  container,
  fieldChanged,
  fieldHint,
  fieldWrapper,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  save,
  selectTrigger,
  setSwitch,
  settle,
  switchButton,
  typeInto,
  waitFor,
} from './harness';
import { REPO_FIELDS, REPO_FIELD_KEYS } from './fields';

installRepoSettingsHooks();

const mount = async (): Promise<void> => {
  await mountSettings(`/repos/${REPO_ID}/settings`);
  await waitFor(() => container.querySelector('input[name="afk_options.ultracode"]'), 'the page');
};

/** The text an id list (aria-labelledby / aria-describedby) points at. */
const textOf = (ids: string | null): string =>
  (ids ?? '')
    .split(' ')
    .filter(Boolean)
    .map((id) => document.getElementById(id)?.textContent ?? `<missing #${id}>`)
    .join(' ');

describe('repo settings fields', () => {
  it('renders one field per PATCH key, each under its own section', async () => {
    await mount();

    const rendered = Array.from(container.querySelectorAll('[data-field]')).map((el) =>
      el.getAttribute('data-field'),
    );
    expect(rendered).toEqual(REPO_FIELD_KEYS);
    for (const key of REPO_FIELD_KEYS) {
      const section = fieldWrapper(key).closest('section.settings-section');
      expect(section?.id).toBe(`settings-${REPO_FIELDS[key].section}`);
    }
  });

  it('labels a text field with <label for>, from the field table', async () => {
    await mount();

    const label = container.querySelector<HTMLLabelElement>('label[for="rs-git_author_name"]');
    expect(label?.textContent).toBe('Git author name');
    expect(label?.control).toBe(input('git_author_name'));
    expect(input('git_author_name').id).toBe('rs-git_author_name');
    // The hint describes the control.
    expect(textOf(input('git_author_name').getAttribute('aria-describedby'))).toBe(
      'Blank inherits the global setting.',
    );
    // A field that may not be empty says so.
    expect(input('name').getAttribute('aria-required')).toBe('true');
    expect(input('git_author_name').hasAttribute('aria-required')).toBe(false);
  });

  it('names a select by its label, and a switch by its own', async () => {
    await mount();

    expect(textOf(selectTrigger('afk_model_default').getAttribute('aria-labelledby'))).toBe(
      'Model',
    );
    expect(selectTrigger('afk_model_default').id).toBe('rs-afk_model_default');

    const auto = switchButton('afk_auto_enabled');
    const label = container.querySelector<HTMLLabelElement>(`label[for="${auto.id}"]`);
    expect(label?.textContent).toBe('Auto-spawn');
    expect(textOf(auto.getAttribute('aria-describedby'))).toBe(
      'Claim ready-for-agent issues as they appear.',
    );
  });

  it('names the option bag as a group, by its label', async () => {
    await mount();

    const group = input('afk_options.ultracode').closest('[role="group"]');
    expect(textOf(group?.getAttribute('aria-labelledby') ?? null)).toBe('Options');
    expect(textOf(group?.getAttribute('aria-describedby') ?? null)).toBe(
      'A repo option bag overrides the global AFK options.',
    );
  });

  it('marks a changed field at its label with a dot and words that join the control name', async () => {
    await mount();
    expect(fieldChanged('git_author_name')).toBe(false);

    typeInto(input('git_author_name'), 'Dominik');
    await chooseFromSelect('afk_model_default', 'Sonnet');
    setSwitch('afk_auto_enabled', true);
    await settle();

    expect(fieldChanged('git_author_name')).toBe(true);
    expect(container.querySelector('label[for="rs-git_author_name"]')?.textContent).toBe(
      'Git author name (unsaved change)',
    );
    expect(textOf(selectTrigger('afk_model_default').getAttribute('aria-labelledby'))).toBe(
      'Model (unsaved change)',
    );
    // A switch brings its own label; the words sit beside it in the field.
    expect(fieldChanged('afk_auto_enabled')).toBe(true);
    // The words are for assistive tech: visually the mark is the dot.
    expect(
      fieldWrapper('git_author_name')
        .querySelector('label span')
        ?.classList.contains('visually-hidden'),
    ).toBe(true);

    await save();
    expect(fieldChanged('git_author_name')).toBe(false);
    expect(container.querySelector('label[for="rs-git_author_name"]')?.textContent).toBe(
      'Git author name',
    );
  });

  it('puts a problem under the control as an alert that describes it, before the hint', async () => {
    await mount();
    typeInto(input('afk_branch_pattern'), 'afk/');
    await settle();
    await save();

    const wrapper = fieldWrapper('afk_branch_pattern');
    const error = wrapper.querySelector('.sfield-error');
    expect(error?.getAttribute('role')).toBe('alert');
    expect(wrapper.classList.contains('invalid')).toBe(true);
    // Control, then the problem, then the hint.
    const order = Array.from(wrapper.children).map((el) =>
      el === input('afk_branch_pattern') ? 'control' : el.className,
    );
    expect(order).toEqual(['sfield-label', 'control', 'sfield-error', 'sfield-hint']);
    expect(input('afk_branch_pattern').getAttribute('aria-describedby')).toBe(
      'rs-afk_branch_pattern-error rs-afk_branch_pattern-hint',
    );
  });

  it('shows a hint only where there is something to say', async () => {
    h.providersOnServer = [...baseProviders(), CODEX];
    await mount();

    // Both remote controls work for this provider: only the manual one explains itself.
    expect(fieldHint('remote_default')).toContain("Registers the session with the agent's web app");
    expect(fieldWrapper('afk_remote_default').querySelector('.sfield-hint')).toBeNull();
    expect(selectTrigger('afk_remote_default').hasAttribute('aria-describedby')).toBe(false);

    // A provider without the knob: both say that it ignores the field.
    await chooseFromSelect('provider', 'Codex');
    expect(fieldHint('remote_default')).toBe('Codex ignores this.');
    expect(fieldHint('afk_remote_default')).toBe('Codex ignores this.');
    expect(selectTrigger('remote_default').disabled).toBe(true);
    expect(textOf(selectTrigger('afk_remote_default').getAttribute('aria-describedby'))).toBe(
      'Codex ignores this.',
    );
  });

  it('keeps the seed prompt placeholder and Customize on the effective prompt', async () => {
    h.repoOnServer = { ...baseRepo(), afk_prompt_effective: 'Do the issue, open a PR.' };
    await mount();
    const prompt = container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]');
    expect(prompt?.placeholder).toBe('Do the issue, open a PR.');
    expect(prompt?.id).toBe('rs-afk_prompt');

    const customize = Array.from(fieldWrapper('afk_prompt').querySelectorAll('button')).find(
      (b) => b.textContent === 'Customize',
    );
    customize?.click();
    await settle();

    expect(prompt?.value).toBe('Do the issue, open a PR.');
    expect(fieldChanged('afk_prompt')).toBe(true);
    // Customize is only offered while the field is blank.
    expect(fieldWrapper('afk_prompt').querySelector('button')).toBeNull();
    await save();
    expect(h.patchBodies).toEqual([{ afk_prompt: 'Do the issue, open a PR.' }]);
  });
});
