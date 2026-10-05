// The repo settings field wrapper (issue #61), through the real page: every
// field has a label that names its control, a hint and a problem that
// describe it, and a changed mark at the label made of a dot AND words — so
// the state never rests on colour alone and reaches assistive tech as part of
// the control's name. An overridable field also says "inherited" or "set
// here" at its label, as part of the control's description.

import { beforeEach, describe, expect, it } from 'vitest';
import {
  CODEX,
  REPO_ID,
  baseProviders,
  baseRepo,
  chooseFromSelect,
  container,
  fieldChanged,
  fieldHint,
  fieldState,
  fieldWrapper,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  save,
  resetButton,
  segment,
  selectTrigger,
  setSwitch,
  settle,
  settleInherited,
  switchButton,
  typeInto,
  waitFor,
} from './harness';
import { REPO_FIELDS, REPO_FIELD_KEYS } from './fields';

installRepoSettingsHooks();
// A repo on the container Runner with Autoland on: nothing is folded away.
beforeEach(() => {
  h.repoOnServer = { ...h.repoOnServer, runner: 'container', autoland_enabled: true };
});

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
    // Its state describes the control; so does a hint, where there is one.
    expect(textOf(input('git_author_name').getAttribute('aria-describedby'))).toBe('inherited');
    expect(textOf(input('afk_branch_pattern').getAttribute('aria-describedby'))).toContain(
      '<N> stands for the issue number',
    );
    // A field that may not be empty says so.
    expect(input('name').getAttribute('aria-required')).toBe('true');
    expect(input('git_author_name').hasAttribute('aria-required')).toBe(false);
  });

  it('names a select by its label and the value it shows, and a switch by its own', async () => {
    await mount();

    // The pick is part of the name: "Inherited · <value>" is heard, not only seen.
    expect(textOf(selectTrigger('afk_model_default').getAttribute('aria-labelledby'))).toBe(
      'Model Inherited · Opus (1M)',
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
    expect(textOf(group?.getAttribute('aria-describedby') ?? null)).toBe('inherited');
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
      'Model (unsaved change) Sonnet',
    );
    // A switch draws its own label: the words are inside it, so they are part
    // of the switch's name too — and nowhere else in the field.
    expect(fieldChanged('afk_auto_enabled')).toBe(true);
    const auto = switchButton('afk_auto_enabled');
    const autoLabel = container.querySelector(`label[for="${auto.id}"]`);
    expect(autoLabel?.textContent).toBe('Auto-spawn (unsaved change)');
    expect(
      Array.from(fieldWrapper('afk_auto_enabled').querySelectorAll('.visually-hidden')).every(
        (el) => autoLabel?.contains(el),
      ),
    ).toBe(true);
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

  it('a switch is described by its own line, then its problem and hint, and marked invalid', async () => {
    await mount(); // Autoland on, forge binding
    const autoland = switchButton('autoland_enabled');
    expect(textOf(autoland.getAttribute('aria-describedby'))).toBe(
      'A lander run validates each PR an AFK run opens.',
    );
    expect(autoland.hasAttribute('aria-invalid')).toBe(false);

    // Autoland on under the builtin binding is a problem Save finds, shown at
    // the switch.
    segment('tracker_binding', 'builtin').click();
    await settle();
    await save();

    const error = fieldWrapper('autoland_enabled').querySelector('.sfield-error');
    expect(error?.textContent).toBe('Turn Autoland off before switching to the built-in tracker.');
    expect(autoland.getAttribute('aria-invalid')).toBe('true');
    const described = autoland.getAttribute('aria-describedby')?.split(' ') ?? [];
    expect(described).toContain(error?.id);
    // Its own description still comes first.
    expect(document.getElementById(described[0] ?? '')?.textContent).toBe(
      'Autoland needs a forge tracker binding. Turn it off to use the built-in one.',
    );
  });

  it('a number field is a text input with the numeric keypad, so a typo reaches the check', async () => {
    h.repoOnServer = { ...h.repoOnServer, budget_minutes: 45 };
    await mount();

    for (const name of [
      'budget_minutes',
      'max_instances_override',
      'container_pids',
      'container_nofile',
      'max_fix_attempts',
    ]) {
      expect([name, input(name).type]).toEqual([name, 'text']);
      expect([name, input(name).getAttribute('inputmode')]).toEqual([name, 'numeric']);
      expect([name, input(name).getAttribute('pattern')]).toEqual([name, '[0-9]*']);
    }
    // A text field carries neither.
    expect(input('git_author_name').hasAttribute('inputmode')).toBe(false);

    // Letters stay in the field as typed — a number input would hand "" to
    // the form, which means "inherit" — and Save refuses them at the field.
    typeInto(input('budget_minutes'), 'abc');
    await settle();
    expect(input('budget_minutes').value).toBe('abc');
    await save();
    expect(h.patchBodies).toEqual([]);
    expect(fieldWrapper('budget_minutes').querySelector('.sfield-error')?.textContent).toBe(
      'Use a whole number, 1 or more, or leave it empty.',
    );

    // Spaces around a number are no problem.
    typeInto(input('budget_minutes'), '30 ');
    await settle();
    await save();
    expect(h.patchBodies).toEqual([{ budget_minutes: 30 }]);

    // And empty is what it always was: back to inherited.
    typeInto(input('budget_minutes'), '');
    await settle();
    expect(fieldState('budget_minutes')).toBe('inherited');
    await save();
    expect(h.patchBodies.at(-1)).toEqual({ budget_minutes: null });
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

    const remoteGroup = (name: string) =>
      container
        .querySelector(`button[role="radio"][name="${name}"]`)
        ?.closest('[role="radiogroup"]');

    // Both remote controls work for this provider: only the manual one explains itself.
    expect(fieldHint('remote_default')).toContain("Registers the session with the agent's web app");
    expect(fieldWrapper('afk_remote_default').querySelector('.sfield-hint')).toBeNull();
    expect(
      textOf(remoteGroup('afk_remote_default')?.getAttribute('aria-describedby') ?? null),
    ).toBe('inherited');

    // A provider without the knob: both say that it ignores the field (the
    // AFK agent follows the drafted agent once the server has answered).
    await chooseFromSelect('provider', 'Codex');
    await settleInherited();
    expect(fieldHint('remote_default')).toBe('Codex ignores this.');
    expect(fieldHint('afk_remote_default')).toBe('Codex ignores this.');
    expect(
      textOf(remoteGroup('afk_remote_default')?.getAttribute('aria-describedby') ?? null),
    ).toBe('inherited Codex ignores this.');
  });

  it('keeps the seed prompt placeholder and Customize on the effective prompt', async () => {
    h.repoOnServer = { ...baseRepo(), afk_prompt_effective: 'Do the issue, open a PR.' };
    await mount();
    const prompt = container.querySelector<HTMLTextAreaElement>('textarea[name="afk_prompt"]');
    expect(prompt?.placeholder).toBe('Do the issue, open a PR.');
    expect(fieldState('afk_prompt')).toBe('inherited');
    expect(prompt?.id).toBe('rs-afk_prompt');

    const customize = Array.from(fieldWrapper('afk_prompt').querySelectorAll('button')).find(
      (b) => b.textContent === 'Customize',
    );
    customize?.click();
    await settle();

    expect(prompt?.value).toBe('Do the issue, open a PR.');
    expect(fieldChanged('afk_prompt')).toBe(true);
    expect(fieldState('afk_prompt')).toBe('set here');
    // Customize is only offered while the field is blank; Reset takes its place.
    expect(
      Array.from(fieldWrapper('afk_prompt').querySelectorAll('button')).map((b) => b.textContent),
    ).toEqual(['Reset']);
    await save();
    expect(h.patchBodies).toEqual([{ afk_prompt: 'Do the issue, open a PR.' }]);

    // Reset returns it to the inherited prompt: blank again, saved as null.
    resetButton('afk_prompt')?.click();
    await settle();
    expect(prompt?.value).toBe('');
    expect(fieldState('afk_prompt')).toBe('inherited');
    await save();
    expect(h.patchBodies[1]).toEqual({ afk_prompt: null });
  });
});
