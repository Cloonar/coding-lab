// Schedule editor suite (issue #61 §8, ADR-0062): the editor at its own URL
// over the settings page — how it opens and closes by breakpoint, the
// in-place ask when edits are pending (from Cancel, from a URL change), its
// own Save (a POST, or a PATCH of what changed), Run now in the header, the
// inline Delete, the Examples pick with Undo, and the server-rendered
// cadence preview in every mode.
//
// The properties worth pinning: the preset cadence editor renders the exact
// cron the server will store, a stored cron decomposes back into the preset
// that made it (and into Cron when it does not), a PATCH carries only what
// changed, client-side refusals cost no request and land at their field, and
// the upcoming firings are always the SERVER's answer.

import { describe, expect, it } from 'vitest';
import {
  BAD_CRON,
  REPO_ID,
  baseSchedule,
  button,
  chooseFromSelect,
  chooseNative,
  container,
  followLink,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  openDialog,
  repoTab,
  routerHistory,
  scheduleEditor,
  schedulesSection,
  segment,
  segmentValue,
  setDesktop,
  settle,
  settlePreview,
  switchButton,
  textarea,
  toastText,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const REPO = `/repos/${REPO_ID}`;
const BASE = `${REPO}/settings`;

const editor = (): HTMLElement => {
  const el = scheduleEditor();
  if (!el) throw new Error('the schedule editor is not open');
  return el;
};
const footer = (): string => editor().querySelector('.schedule-editor-foot')?.textContent ?? '';
const runNowButton = (): HTMLButtonElement | null =>
  editor().querySelector<HTMLButtonElement>('button.schedule-run-now');
const refusalText = (): string =>
  editor().querySelector('.schedule-editor-refusal')?.textContent ?? '';
const fieldError = (name: string): string | null => {
  const control = container.querySelector(`[name="${name}"]`);
  const wrapper = control?.closest('.sfield');
  return wrapper?.querySelector('.sfield-error')?.textContent ?? null;
};
const preview = (): HTMLElement | null => editor().querySelector('.cadence-preview');
const submitEditor = (): void => {
  const form = editor().querySelector('form');
  if (!form) throw new Error('the editor has no form');
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
};
/** A chip-toggle (flow / weekday) by its form name. */
const chip = (name: string): HTMLButtonElement => {
  const el = container.querySelector<HTMLButtonElement>(`button[name="${name}"]`);
  if (!el) throw new Error(`missing chip-toggle button[name="${name}"]`);
  return el;
};
/** An in-app link appended to the page, the way a tab or a save bar link is. */
const linkTo = (href: string): HTMLAnchorElement => {
  const a = document.createElement('a');
  a.setAttribute('href', href);
  a.textContent = href;
  container.appendChild(a);
  return a;
};
const keydown = (key: string, init: KeyboardEventInit = {}): void => {
  (document.activeElement ?? document.body).dispatchEvent(
    new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...init }),
  );
};

/** Mounts the schedules section and opens the editor of sched_1 from its row. */
const openFromRow = async (): Promise<HTMLAnchorElement> => {
  await mountSettings(`${BASE}/schedules`);
  const link = await waitFor(
    () => schedulesSection().querySelector<HTMLAnchorElement>('a.schedule-row-main'),
    'the row',
  );
  link.focus();
  link.click();
  await settle();
  await waitFor(scheduleEditor, 'the editor');
  return link;
};
/** Holds every schedule write until the returned function is called. */
const hold = (): (() => void) => {
  let release = (): void => {};
  h.scheduleHold = new Promise<void>((resolve) => (release = resolve));
  return () => {
    release();
    h.scheduleHold = null;
  };
};
const rowLinkOf = (id: string): HTMLAnchorElement => {
  const link = schedulesSection().querySelector<HTMLAnchorElement>(
    `a.schedule-row-main[href="${BASE}/schedules/${id}"]`,
  );
  if (!link) throw new Error(`no row link for ${id}`);
  return link;
};
const openNew = async (): Promise<void> => {
  await mountSettings(`${BASE}/schedules/new`);
  await waitFor(scheduleEditor, 'the editor');
  await settle();
};

describe('schedule editor: opening and closing', () => {
  it('is a dialog over the page: labelled, modal, focused, trapped', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await mountSettings(`${BASE}/schedules/sched_1`);
    await waitFor(scheduleEditor, 'the editor');
    await settle();

    const dialog = editor();
    expect(dialog.getAttribute('role')).toBe('dialog');
    expect(dialog.getAttribute('aria-modal')).toBe('true');
    const title = dialog.querySelector('h2');
    expect(title?.textContent).toBe('Edit schedule');
    expect(dialog.getAttribute('aria-labelledby')).toBe(title?.id);
    expect(document.activeElement).toBe(title);
    expect(document.body.style.overflow).toBe('hidden');
    // The page is all there under it.
    expect(container.querySelectorAll('section.settings-section')).toHaveLength(10);
    // Below 1024px: full screen, no scrim.
    expect(container.querySelector('.schedule-editor-scrim')).toBeNull();

    // Tab from the last control wraps to the first; Shift+Tab the other way.
    const focusables = Array.from(
      dialog.querySelectorAll<HTMLElement>('button:not([disabled]), input, textarea, a[href]'),
    );
    focusables.at(-1)?.focus();
    keydown('Tab');
    expect(document.activeElement).toBe(focusables[0]);
    keydown('Tab', { shiftKey: true });
    expect(document.activeElement).toBe(focusables.at(-1));
  });

  it('from 1024px it is a side panel with a scrim, and the scrim closes it', async () => {
    setDesktop(true);
    h.schedules = [baseSchedule()];
    await openFromRow();

    const scrim = container.querySelector<HTMLElement>('.schedule-editor-scrim');
    expect(scrim).not.toBeNull();
    scrim?.click();
    await settle();

    expect(scheduleEditor()).toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
  });

  it('opened from a row, the back control goes back and returns focus to the row', async () => {
    h.schedules = [baseSchedule()];
    const link = await openFromRow();
    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_1`);

    button('Back to schedules').click();
    await settle();

    expect(scheduleEditor()).toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
    expect(document.activeElement).toBe(link);
    expect(document.body.style.overflow).toBe('');
  });

  it('arrived at directly, closing replaces the URL with the section', async () => {
    h.schedules = [baseSchedule()];
    await mountSettings(`${BASE}/schedules/sched_1`);
    await waitFor(scheduleEditor, 'the editor');
    await settle();

    button('Cancel').click();
    await settle();

    expect(scheduleEditor()).toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
  });

  it('Browser Back closes it', async () => {
    h.schedules = [baseSchedule()];
    await openFromRow();

    routerHistory.go(-1);
    await settle();

    expect(scheduleEditor()).toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
  });

  it('Escape closes it when nothing is pending', async () => {
    h.schedules = [baseSchedule()];
    await openFromRow();

    keydown('Escape');
    await settle();

    expect(scheduleEditor()).toBeNull();
  });

  it('says so when the schedule no longer exists', async () => {
    await mountSettings(`${BASE}/schedules/sched_gone`);
    await waitFor(scheduleEditor, 'the editor frame');

    expect(editor().textContent).toContain('This schedule no longer exists.');
    button('Back to schedules').click();
    await settle();
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
  });
});

describe('schedule editor: pending edits', () => {
  it('marks the editor dirty: Run now off with its note, Cancel asks in place', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    expect(runNowButton()?.disabled).toBe(false);
    expect(footer()).not.toContain('Run now uses the saved version.');

    typeInto(input('schedule-name'), 'Weekly deps v2');
    await settle();

    expect(runNowButton()?.disabled).toBe(true);
    expect(footer()).toContain('Run now uses the saved version.');

    button('Cancel').click();
    await settle();

    expect(footer()).toContain('Discard your changes?');
    expect(button('Keep editing')).not.toBeNull();
    expect(document.activeElement).toBe(button('Keep editing'));
    expect(openDialog()).toBeNull();
    expect(scheduleEditor()).not.toBeNull();

    button('Keep editing').click();
    await settle();
    expect(footer()).not.toContain('Discard your changes?');
    expect(input('schedule-name').value).toBe('Weekly deps v2');
    expect(document.activeElement).toBe(button('Cancel'));

    button('Cancel').click();
    await settle();
    button('Discard').click();
    await settle();

    expect(scheduleEditor()).toBeNull();
    expect(h.scheduleBodies).toHaveLength(0);
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
  });

  it('holds a URL change away from the editor behind the same in-place ask', async () => {
    h.schedules = [baseSchedule()];
    await openFromRow();
    typeInto(input('schedule-name'), 'Renamed');
    await settle();

    const link = linkTo(`${REPO}/issues`);
    link.click();
    await settle();

    // Held, asked in place — not the leave dialog, and nothing moved yet.
    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_1`);
    expect(scheduleEditor()).not.toBeNull();
    expect(footer()).toContain('Discard your changes?');
    expect(openDialog()).toBeNull();

    button('Discard').click();
    await settle();

    expect(routerHistory.get()).toBe(`${REPO}/issues`);
    expect(scheduleEditor()).toBeNull();
  });

  it('Escape while asking keeps editing', async () => {
    h.schedules = [baseSchedule()];
    await openFromRow();
    typeInto(input('schedule-name'), 'Renamed');
    button('Cancel').click();
    await settle();

    keydown('Escape');
    await settle();

    expect(footer()).not.toContain('Discard your changes?');
    expect(scheduleEditor()).not.toBeNull();
  });

  it('closes without asking once the edits are taken back', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    typeInto(input('schedule-name'), 'Renamed');
    typeInto(input('schedule-name'), 'Weekly deps');
    await settle();

    expect(runNowButton()?.disabled).toBe(false);
    button('Cancel').click();
    await settle();
    expect(scheduleEditor()).toBeNull();
  });
});

describe('schedule editor: save', () => {
  it('prefills a preset cadence and PATCHes only the dirty fields, then offers Run now', async () => {
    h.schedules = [
      baseSchedule({
        name: 'Weekly deps',
        cadence: '30 6 * * 1,4',
        prompt: 'Investigate available dependency updates.',
        flows: ['autolander'],
      }),
    ];
    await openFromRow();

    // The stored cron decomposed back into the preset that renders it.
    expect(input('schedule-name').value).toBe('Weekly deps');
    expect(segmentValue('cadence_mode')).toBe('weekly');
    expect(input('cadence_time').value).toBe('06:30');
    expect(chip('weekday-1').getAttribute('aria-pressed')).toBe('true');
    expect(chip('weekday-4').getAttribute('aria-pressed')).toBe('true');
    expect(chip('weekday-2').getAttribute('aria-pressed')).toBe('false');
    expect(chip('flow-autolander').getAttribute('aria-pressed')).toBe('true');

    typeInto(input('cadence_time'), '07:00');
    submitEditor();
    await settle();

    // Only the cadence moved, so only the cadence rides the PATCH.
    expect(h.scheduleBodies).toEqual([{ cadence: '0 7 * * 1,4' }]);
    expect(h.schedules[0]?.cadence).toBe('0 7 * * 1,4');
    expect(scheduleEditor()).toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
    expect(toastText()).toContain('Saved "Weekly deps"');

    // The toast's Run now starts a run from the saved Schedule.
    const action = container.querySelector<HTMLButtonElement>('.toast button');
    expect(action?.textContent).toBe('Run now');
    action?.click();
    await settle();

    expect(h.runNowRequests).toEqual(['sched_1']);
    expect(toastText()).toBe('Started a run from "Weekly deps"');
  });

  it('POSTs a new schedule with the preset-rendered cron and the flows in catalog order', async () => {
    await openNew();
    expect(runNowButton()).toBeNull(); // never saved: nothing to run

    typeInto(input('schedule-name'), 'Weekly deps');
    typeInto(textarea('schedule-prompt'), 'Investigate dependency updates.');
    // Picked in the reverse of catalog order on purpose — the body must not
    // carry the click order.
    chip('flow-human-triage').click();
    chip('flow-autolander').click();
    segment('cadence_mode', 'weekly').click();
    await settle();
    typeInto(input('cadence_time'), '06:30');
    // Weekly opens on Monday so the mode is never illegal on arrival; adding
    // Thursday is the whole edit.
    expect(chip('weekday-1').getAttribute('aria-pressed')).toBe('true');
    chip('weekday-4').click();
    await settle();

    submitEditor();
    await settle();

    expect(h.scheduleBodies).toEqual([
      {
        name: 'Weekly deps',
        cadence: '30 6 * * 1,4',
        prompt: 'Investigate dependency updates.',
        flows: ['autolander', 'human-triage'],
        enabled: true,
      },
    ]);
    expect(scheduleEditor()).toBeNull();
    expect(toastText()).toContain('Saved "Weekly deps"');
    expect(schedulesSection().textContent).toContain('Weekly deps');
  });

  it('sends the overrides only when they are actually set', async () => {
    await openNew();

    typeInto(input('schedule-name'), 'With overrides');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    typeInto(input('schedule_budget_minutes'), '45');
    await chooseFromSelect('schedule_model', 'Sonnet');
    switchButton('schedule_enabled').click();
    await settle();
    submitEditor();
    await settle();

    expect(h.scheduleBodies).toEqual([
      {
        name: 'With overrides',
        cadence: '0 6 * * *',
        prompt: 'Look around.',
        flows: [],
        enabled: false,
        budget_minutes: 45,
        model: 'sonnet',
      },
    ]);
  });

  it('clears an override back to inherited and drops a flow in one PATCH', async () => {
    h.schedules = [baseSchedule({ flows: ['autolander', 'human-triage'], model: 'sonnet' })];
    await openFromRow();

    chip('flow-autolander').click();
    await chooseFromSelect('schedule_model', "Inherited · the repo's AFK model");
    submitEditor();
    await settle();

    expect(h.scheduleBodies).toEqual([{ flows: ['human-triage'], model: null }]);
  });

  it('closes without a request when nothing changed', async () => {
    h.schedules = [baseSchedule()];
    await openFromRow();

    submitEditor();
    await settle();

    expect(h.scheduleBodies).toHaveLength(0);
    expect(scheduleEditor()).toBeNull();
    expect(toastText()).toBe('');
  });

  it('shows a server refusal at the field it names, else at the top', async () => {
    h.schedules = [baseSchedule()];
    h.scheduleRefusal = { error: 'name already taken', field: 'name' };
    await openFromRow();
    typeInto(input('schedule-name'), 'Taken');
    submitEditor();
    await settle();

    expect(fieldError('schedule-name')).toBe('name already taken');
    expect(input('schedule-name').getAttribute('aria-invalid')).toBe('true');
    expect(document.activeElement).toBe(input('schedule-name'));
    expect(scheduleEditor()).not.toBeNull();

    h.scheduleRefusal = { error: 'schedules are read-only right now' };
    typeInto(input('schedule-name'), 'Other');
    submitEditor();
    await settle();

    expect(editor().querySelector('.banner.error')?.textContent).toContain(
      'schedules are read-only right now',
    );
  });
});

describe('schedule editor: problems found before a request', () => {
  const expectBlocked = (name: string, message: string): void => {
    expect(h.scheduleBodies).toHaveLength(0);
    expect(fieldError(name)).toBe(message);
    expect(scheduleEditor()).not.toBeNull();
  };

  it('asks for a name, and puts the focus there', async () => {
    await openNew();
    typeInto(textarea('schedule-prompt'), 'Look around.');
    submitEditor();
    await settle();

    expectBlocked('schedule-name', 'Give the schedule a name.');
    expect(document.activeElement).toBe(input('schedule-name'));
    // Typing clears the problem.
    typeInto(input('schedule-name'), 'N');
    expect(fieldError('schedule-name')).toBeNull();
  });

  it('refuses a schedule with neither a prompt nor a flow', async () => {
    await openNew();
    typeInto(input('schedule-name'), 'Empty');
    submitEditor();
    await settle();

    expectBlocked('schedule-prompt', 'Add a prompt, a flow, or both.');
  });

  it('blocks a weekly cadence with no weekday picked', async () => {
    await openNew();
    typeInto(input('schedule-name'), 'Weekly');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    segment('cadence_mode', 'weekly').click();
    await settle();
    chip('weekday-1').click(); // the default pick, toggled back off
    await settle();
    expect(preview()).toBeNull(); // nothing to preview without a weekday
    submitEditor();
    await settle();

    expect(h.scheduleBodies).toHaveLength(0);
    expect(editor().textContent).toContain('Pick at least one weekday.');
  });

  it('refuses a budget below one minute', async () => {
    await openNew();
    typeInto(input('schedule-name'), 'Zero budget');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    typeInto(input('schedule_budget_minutes'), '0');
    submitEditor();
    await settle();

    expectBlocked(
      'schedule_budget_minutes',
      'Use a whole number of minutes, 1 or more, or leave it empty.',
    );
  });

  it('asks for a cron expression in Cron mode', async () => {
    await openNew();
    typeInto(input('schedule-name'), 'Cron');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    segment('cadence_mode', 'advanced').click();
    await settle();
    submitEditor();
    await settle();

    expectBlocked('cadence_expr', 'Enter a cron expression.');
  });
});

describe('schedule editor: Run now', () => {
  it('starts a run from the saved schedule and closes', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps', enabled: false })];
    await openFromRow();

    runNowButton()?.click();
    await settle();

    // Works for a switched-off Schedule too.
    expect(h.runNowRequests).toEqual(['sched_1']);
    expect(scheduleEditor()).toBeNull();
    expect(toastText()).toBe('Started a run from "Weekly deps"');
    expect(h.scheduleBodies).toHaveLength(0);
  });

  it('shows a refusal verbatim by the button and stays open', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    h.runNowRefusal = "schedule's previous run is still live";
    await openFromRow();

    runNowButton()?.click();
    await settle();

    expect(scheduleEditor()).not.toBeNull();
    const region = editor().querySelector('.schedule-editor-refusal');
    expect(region?.getAttribute('role')).toBe('alert');
    expect(refusalText()).toBe("schedule's previous run is still live");
    expect(toastText()).toBe('');
    expect(runNowButton()?.disabled).toBe(false);
  });

  it('a refused Run now from the Saved toast is reported in the Schedules section', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    typeInto(input('schedule-name'), 'Weekly deps v2');
    submitEditor();
    await settle();
    expect(toastText()).toContain('Saved "Weekly deps v2"');

    h.runNowRefusal = 'schedule is paused after consecutive failures — re-enable to re-arm';
    container.querySelector<HTMLButtonElement>('.toast button')?.click();
    await settle();

    expect(schedulesSection().querySelector('.banner.error')?.textContent).toContain(
      'schedule is paused after consecutive failures — re-enable to re-arm',
    );
  });
});

describe('schedule editor: delete', () => {
  it('deletes behind an inline confirmation, closes and says so', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();

    button('Delete schedule').click();
    await settle();
    expect(editor().textContent).toContain('Delete "Weekly deps"?');
    expect(h.schedules).toHaveLength(1);
    expect(openDialog()).toBeNull();

    button('Delete for good').click();
    await settle();

    expect(h.schedules).toHaveLength(0);
    expect(scheduleEditor()).toBeNull();
    expect(toastText()).toBe('Deleted "Weekly deps"');
    expect(schedulesSection().textContent).toContain('No schedules yet');
  });

  it('has no Delete for a schedule that was never saved', async () => {
    await openNew();
    expect(container.querySelector('.schedule-editor-delete')).toBeNull();
  });
});

describe('schedule editor: Examples', () => {
  it('fills an empty prompt at once and keeps the pick on its placeholder', async () => {
    await openNew();

    await chooseFromSelect('schedule-example', 'Check for dependency updates');

    const prompt = textarea('schedule-prompt');
    expect(prompt.value).toContain("Investigate this repository's dependencies");
    expect(prompt.value.split('\n').length).toBeGreaterThan(1);
    expect(
      container.querySelector('button[name="schedule-example"] .select-field-label')?.textContent,
    ).toBe('Start from an example…');
    // Nothing was replaced, so there is nothing to undo.
    expect(editor().textContent).not.toContain('Replaced your prompt');
  });

  it('replaces a written prompt at once, with Undo', async () => {
    await openNew();
    typeInto(textarea('schedule-prompt'), 'my own words');

    await chooseFromSelect('schedule-example', 'Security audit');

    expect(textarea('schedule-prompt').value).toContain('security-review skill');
    expect(editor().textContent).toContain('Replaced your prompt with the example.');

    button('Undo').click();
    await settle();

    expect(textarea('schedule-prompt').value).toBe('my own words');
    expect(editor().textContent).not.toContain('Replaced your prompt');
    expect(document.activeElement).toBe(textarea('schedule-prompt'));
  });

  it('offers the triage starter with its maintainer placeholder', async () => {
    await openNew();

    await chooseFromSelect('schedule-example', 'Prepare the triage inbox (no flow)');

    const prompt = textarea('schedule-prompt');
    expect(prompt.value).toContain('unattended mode');
    // The placeholder is the safe default (ADR-0070): left unfilled, the run
    // can establish no maintainer and so promotes nothing.
    expect(prompt.value).toContain('<fill in the logins>');
  });
});

describe('schedule editor: cadence preview', () => {
  it('renders the upcoming firings the server reports, for the cron each mode stores', async () => {
    await openNew();
    await settlePreview();

    // The preview is asked about the cron the form would store, never about
    // the preset — one parser, and it is the server's.
    expect(h.cronPreviewExprs).toContain('0 6 * * *');
    expect(preview()?.textContent).toBe('Next runs: Mon 2026-08-03 06:00, Thu 2026-08-06 06:00');
    expect(preview()?.classList.contains('invalid')).toBe(false);

    segment('cadence_mode', 'weekly').click();
    await settlePreview();
    expect(h.cronPreviewExprs.at(-1)).toBe('0 6 * * 1');

    segment('cadence_mode', 'monthly').click();
    await settle();
    chooseNative('cadence_day', '3');
    typeInto(input('cadence_time'), '09:15');
    await settlePreview();
    expect(h.cronPreviewExprs.at(-1)).toBe('15 9 3 * *');
    expect(preview()?.textContent).toContain('Next runs:');
  });

  it('renders the parser refusal verbatim for an expression it cannot read', async () => {
    await openNew();
    segment('cadence_mode', 'advanced').click();
    await settle();
    typeInto(input('cadence_expr'), BAD_CRON);
    await settlePreview();

    expect(preview()?.textContent).toBe('cron: expected 5 fields, got 1');
    expect(preview()?.classList.contains('invalid')).toBe(true);
  });

  it('opens an unrecognizable cadence in Cron with the expression untouched', async () => {
    h.schedules = [baseSchedule({ cadence: '*/15 * * * *' })];
    await openFromRow();

    expect(segmentValue('cadence_mode')).toBe('advanced');
    expect(input('cadence_expr').value).toBe('*/15 * * * *');
    expect(container.querySelector('input[name="cadence_time"]')).toBeNull();
  });
});

describe('schedule editor: overrides', () => {
  it('reads inherited values in words and shows Default + Reset once set', async () => {
    h.schedules = [baseSchedule({ budget_minutes: 45, model: 'sonnet' })];
    await openFromRow();

    const budgetField = input('schedule_budget_minutes').closest('.sfield');
    expect(budgetField?.querySelector('.sfield-state')?.textContent).toBe('set here');
    expect(budgetField?.querySelector('.sfield-default')?.textContent).toContain(
      'Default: 30 minutes',
    );
    // The agent below a Schedule is the repo's AFK agent, by its display name.
    expect(
      container.querySelector('button[name="schedule_provider"] .select-field-label')?.textContent,
    ).toBe('Inherited · Claude Code');
    const modelField = container.querySelector('button[name="schedule_model"]')?.closest('.sfield');
    expect(modelField?.querySelector('.sfield-state')?.textContent).toBe('set here');
    expect(modelField?.querySelector('.sfield-default')?.textContent).toContain(
      "Default: the repo's AFK model",
    );
    const effortField = container
      .querySelector('button[name="schedule_effort"]')
      ?.closest('.sfield');
    expect(effortField?.querySelector('.sfield-state')?.textContent).toBe('inherited');

    button('Reset Budget, minutes to inherited').click();
    await settle();
    expect(input('schedule_budget_minutes').value).toBe('');
    expect(budgetField?.querySelector('.sfield-state')?.textContent).toBe('inherited');
    expect(input('schedule_budget_minutes').placeholder).toBe('30');

    submitEditor();
    await settle();
    expect(h.scheduleBodies).toEqual([{ budget_minutes: null }]);
  });
});

describe('schedule editor under the leave dialog', () => {
  // A pending repo setting, the editor open (and clean), and a navigation
  // that leaves the repo: the repo form's leave dialog opens OVER the editor.
  const openBoth = async (): Promise<void> => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await mountSettings(`${BASE}/schedules`);
    const link = await waitFor(
      () => schedulesSection().querySelector<HTMLAnchorElement>('a.schedule-row-main'),
      'the row',
    );
    typeInto(input('git_author_name'), 'Dominik');
    await settle();
    link.focus();
    link.click();
    await settle();
    await waitFor(scheduleEditor, 'the editor');
    linkTo('/elsewhere').click();
    await settle();
  };

  it('the dialog opens over the editor, and the two do not fight over the focus', async () => {
    // Two focus traps that both insist bounce the focus between them until
    // the stack overflows. Opening the dialog must simply work.
    await openBoth();

    expect(openDialog()).not.toBeNull();
    expect(scheduleEditor()).not.toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_1`);
    expect(document.activeElement?.textContent).toBe('Keep editing');
    expect(openDialog()?.contains(document.activeElement)).toBe(true);

    // Focus sent into the editor underneath comes back to the dialog — once.
    expect(() => input('schedule-name').focus()).not.toThrow();
    expect(openDialog()?.contains(document.activeElement)).toBe(true);
  });

  it('Escape closes the dialog only; the editor underneath stays', async () => {
    await openBoth();

    keydown('Escape');
    await settle();

    expect(openDialog()).toBeNull();
    expect(scheduleEditor()).not.toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_1`);
    // The editor is the modal again: focus is in it, and the page is locked.
    expect(editor().contains(document.activeElement)).toBe(true);
    expect(document.body.style.overflow).toBe('hidden');
  });

  it('Keep editing returns to the editor, which then has the Escape key again', async () => {
    await openBoth();

    Array.from(openDialog()?.querySelectorAll('button') ?? [])
      .find((b) => b.textContent === 'Keep editing')
      ?.click();
    await settle();

    expect(openDialog()).toBeNull();
    expect(editor().contains(document.activeElement)).toBe(true);
    // Focus sent to the page behind is pulled back into the editor.
    linkTo('/elsewhere').focus();
    expect(editor().contains(document.activeElement)).toBe(true);

    keydown('Escape');
    await settle();
    expect(scheduleEditor()).toBeNull();
    expect(document.body.style.overflow).toBe('');
  });
});

describe('schedule editor: the stand-in while a Schedule loads', () => {
  it('is as modal as the editor, and can be left while the list has not answered', async () => {
    let release = (): void => {};
    h.schedulesHold = new Promise<void>((resolve) => (release = resolve));
    h.schedules = [baseSchedule()];
    await mountSettings(`${BASE}/schedules/sched_1`);
    const frame = await waitFor(scheduleEditor, 'the frame');

    expect(frame.textContent).toContain('Loading schedule…');
    expect(frame.getAttribute('role')).toBe('dialog');
    expect(frame.getAttribute('aria-modal')).toBe('true');
    expect(document.activeElement).toBe(frame.querySelector('h2'));
    expect(document.body.style.overflow).toBe('hidden');
    // A way out, also now.
    expect(frame.querySelector('button[aria-label="Back to schedules"]')).not.toBeNull();
    linkTo('/elsewhere').focus();
    expect(frame.contains(document.activeElement)).toBe(true);

    keydown('Escape');
    await settle();
    expect(scheduleEditor()).toBeNull();
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
    expect(document.body.style.overflow).toBe('');
    release();
  });

  it('gives way to the editor once the Schedule is there', async () => {
    let release = (): void => {};
    h.schedulesHold = new Promise<void>((resolve) => (release = resolve));
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await mountSettings(`${BASE}/schedules/sched_1`);
    await waitFor(scheduleEditor, 'the frame');

    release();
    await waitFor(
      () => (scheduleEditor()?.querySelector('h2')?.textContent === 'Edit schedule' ? true : null),
      'the editor',
    );

    expect(input('schedule-name').value).toBe('Weekly deps');
    expect(document.activeElement).toBe(editor().querySelector('h2'));
    expect(document.body.style.overflow).toBe('hidden');
  });
});

describe('schedule editor: an answer that arrives late', () => {
  // Run now holds its answer for a whole spawn pass. (The memory router's
  // history move is not held by a leave guard, which is how these tests get
  // the editor off the page while a request is in flight.)
  it('a Run now that lands after the editor is gone only reports — it navigates nowhere', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    const release = hold();
    await openFromRow();
    runNowButton()?.click();
    await settle();
    expect(runNowButton()?.textContent).toContain('Starting…');

    routerHistory.go(-1);
    await settle();
    expect(scheduleEditor()).toBeNull();
    await followLink(repoTab('Issues'));
    expect(routerHistory.get()).toBe(`${REPO}/issues`);

    release();
    await settle();

    // The run did start, and that is said — on the tab the operator is on.
    expect(routerHistory.get()).toBe(`${REPO}/issues`);
    expect(toastText()).toBe('Started a run from "Weekly deps"');
  });

  it('…and never closes another editor the operator opened meanwhile', async () => {
    h.schedules = [
      baseSchedule({ name: 'Weekly deps' }),
      baseSchedule({ id: 'sched_2', name: 'Nightly audit' }),
    ];
    const release = hold();
    await mountSettings(`${BASE}/schedules`);
    await waitFor(() => schedulesSection().querySelector('a.schedule-row-main'), 'the rows');
    rowLinkOf('sched_1').click();
    await settle();
    await waitFor(scheduleEditor, 'the first editor');
    runNowButton()?.click();
    await settle();

    routerHistory.go(-1);
    await settle();
    rowLinkOf('sched_2').click();
    await settle();
    await waitFor(scheduleEditor, 'the second editor');
    expect(input('schedule-name').value).toBe('Nightly audit');

    release();
    await settle();

    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_2`);
    expect(input('schedule-name').value).toBe('Nightly audit');
    expect(toastText()).toBe('Started a run from "Weekly deps"');
  });

  it('a Save that lands after the editor is gone is said to have landed, without navigating', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    typeInto(input('schedule-name'), 'Weekly deps v2');
    const release = hold();
    submitEditor();
    await settle();

    routerHistory.go(-1);
    await settle();
    expect(scheduleEditor()).toBeNull();
    await followLink(repoTab('Issues'));

    release();
    await settle();

    expect(h.schedules[0]?.name).toBe('Weekly deps v2');
    expect(routerHistory.get()).toBe(`${REPO}/issues`);
    expect(toastText()).toContain('Saved "Weekly deps v2"');
  });

  it('a refusal that lands after the editor is gone is said through the frame, verbatim', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    h.runNowRefusal = "schedule's previous run is still live";
    const release = hold();
    await openFromRow();
    runNowButton()?.click();
    await settle();
    routerHistory.go(-1);
    await settle();

    release();
    await settle();

    expect(toastText()).toBe(
      'No run started from "Weekly deps": schedule\'s previous run is still live',
    );
    expect(routerHistory.get()).toBe(`${BASE}/schedules`);
  });
});

describe('schedule editor: leaving while a request is in flight', () => {
  it('a navigation during Run now waits for the answer, then goes ahead', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    const release = hold();
    await openFromRow();
    runNowButton()?.click();
    await settle();

    linkTo(`${REPO}/issues`).click();
    await settle();

    // Not gone, and nothing asked: the run is being started.
    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_1`);
    expect(scheduleEditor()).not.toBeNull();
    expect(footer()).not.toContain('Discard your changes?');

    release();
    await settle();

    expect(routerHistory.get()).toBe(`${REPO}/issues`);
    expect(scheduleEditor()).toBeNull();
    expect(toastText()).toBe('Started a run from "Weekly deps"');
  });

  it('a navigation during a Save is not asked about: nothing can be discarded any more', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    typeInto(input('schedule-name'), 'Weekly deps v2');
    const release = hold();
    submitEditor();
    await settle();

    linkTo(`${REPO}/issues`).click();
    await settle();

    // No "Discard your changes?" for edits that are on their way to the server.
    expect(footer()).not.toContain('Discard your changes?');
    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_1`);

    release();
    await settle();

    expect(h.scheduleBodies).toEqual([{ name: 'Weekly deps v2' }]);
    expect(routerHistory.get()).toBe(`${REPO}/issues`);
    expect(toastText()).toContain('Saved "Weekly deps v2"');
  });

  it('…and stays, with the reason, when the request is refused', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    h.scheduleRefusal = { error: 'name already taken', field: 'name' };
    await openFromRow();
    typeInto(input('schedule-name'), 'Taken');
    const release = hold();
    submitEditor();
    await settle();
    linkTo(`${REPO}/issues`).click();
    await settle();

    release();
    await settle();

    // The navigation that waited is dropped: the operator has a name to fix.
    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_1`);
    expect(fieldError('schedule-name')).toBe('name already taken');
    // Leaving now asks, as it always does with pending edits.
    linkTo(`${REPO}/issues`).click();
    await settle();
    expect(footer()).toContain('Discard your changes?');
  });

  it('a second click on Run now while the first is on its way sends nothing', async () => {
    h.schedules = [baseSchedule()];
    const release = hold();
    await openFromRow();

    runNowButton()?.click();
    runNowButton()?.click();
    await settle();
    runNowButton()?.click();
    release();
    await settle();

    expect(h.runNowRequests).toEqual(['sched_1']);
  });
});

describe('schedule editor: what a request says is always in view', () => {
  it('a refusal that names no field shows in the strip under the header, and takes the focus', async () => {
    h.schedules = [baseSchedule()];
    h.scheduleRefusal = { error: 'schedules are read-only right now' };
    await openFromRow();
    typeInto(input('schedule-name'), 'Other');
    submitEditor();
    await settle();

    const strip = editor().querySelector<HTMLElement>('.schedule-editor-alerts');
    expect(strip?.querySelector('.banner.error')?.textContent).toContain(
      'schedules are read-only right now',
    );
    expect(strip?.querySelector('.banner.error')?.getAttribute('role')).toBe('alert');
    // Not inside the scrolling body: a tap on Save at the bottom of a long
    // form must not be answered somewhere off screen.
    expect(
      editor()
        .querySelector('.schedule-editor-body')
        ?.contains(strip ?? null),
    ).toBe(false);
    expect(document.activeElement).toBe(strip);
  });

  it('a failed Delete is said there too', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    h.scheduleRefusal = { error: 'schedule has a live run' };
    button('Delete schedule').click();
    await settle();
    button('Delete for good').click();
    await settle();

    expect(scheduleEditor()).not.toBeNull();
    expect(editor().querySelector('.schedule-editor-alerts .banner.error')?.textContent).toContain(
      'schedule has a live run',
    );
    expect(h.schedules).toHaveLength(1);
  });

  it('a Run now that fails for another reason than a refusal shows by the button as well', async () => {
    h.schedules = [baseSchedule()];
    h.runNowError = { status: 500, error: 'spawn pass: store unavailable' };
    await openFromRow();

    runNowButton()?.click();
    await settle();

    expect(scheduleEditor()).not.toBeNull();
    expect(refusalText()).toBe('spawn pass: store unavailable');
    expect(editor().querySelector('.schedule-editor-refusal')?.getAttribute('role')).toBe('alert');
    expect(editor().querySelector('.banner.error')).toBeNull();
    expect(runNowButton()?.disabled).toBe(false);
    expect(toastText()).toBe('');
  });
});

describe('schedule editor: the Saved toast, away from the Settings tab', () => {
  it('a refused Run now is said through the frame, with the reason verbatim', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    typeInto(input('schedule-name'), 'Weekly deps v2');
    submitEditor();
    await settle();
    expect(toastText()).toContain('Saved "Weekly deps v2"');

    // The toast outlives the tab; the page that would show a notice does not.
    await followLink(repoTab('Issues'));
    h.runNowRefusal = 'schedule is paused after consecutive failures — re-enable to re-arm';
    container.querySelector<HTMLButtonElement>('.toast button')?.click();
    await settle();

    expect(h.runNowRequests).toEqual(['sched_1']);
    expect(toastText()).toBe(
      'No run started from "Weekly deps v2": schedule is paused after consecutive failures — re-enable to re-arm',
    );
  });

  it('and a run that did start is said there just the same', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    typeInto(input('schedule-name'), 'Weekly deps v2');
    submitEditor();
    await settle();
    await followLink(repoTab('Issues'));

    container.querySelector<HTMLButtonElement>('.toast button')?.click();
    await settle();

    expect(toastText()).toBe('Started a run from "Weekly deps v2"');
  });
});

describe('schedule editor: focus after it closes', () => {
  it('after Save the focus is back on the row, and stays there through the reload', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    const link = await openFromRow();
    typeInto(input('schedule-name'), 'Weekly deps v2');
    submitEditor();
    await settle();

    // The SAME row: patched in place, not rebuilt under the focus.
    expect(link.isConnected).toBe(true);
    expect(link.textContent).toContain('Weekly deps v2');
    expect(document.activeElement).toBe(link);
  });

  it('after Run now the focus is back on the row as well', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    const link = await openFromRow();
    runNowButton()?.click();
    await settle();

    expect(link.isConnected).toBe(true);
    expect(document.activeElement).toBe(link);
  });

  it('after Delete the focus goes to the row that takes its place', async () => {
    h.schedules = [
      baseSchedule({ name: 'Weekly deps' }),
      baseSchedule({ id: 'sched_2', name: 'Nightly audit' }),
    ];
    await mountSettings(`${BASE}/schedules`);
    await waitFor(() => schedulesSection().querySelector('a.schedule-row-main'), 'the rows');
    const first = rowLinkOf('sched_1');
    first.focus();
    first.click();
    await settle();
    await waitFor(scheduleEditor, 'the editor');

    button('Delete schedule').click();
    await settle();
    button('Delete for good').click();
    await settle();

    expect(scheduleEditor()).toBeNull();
    expect(first.isConnected).toBe(false);
    expect(document.activeElement).toBe(rowLinkOf('sched_2'));
  });

  it('…and to New schedule when it was the last one', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();

    button('Delete schedule').click();
    await settle();
    button('Delete for good').click();
    await settle();

    expect(document.activeElement).toBe(schedulesSection().querySelector('a.schedule-new'));
  });
});

describe("schedule editor: Examples keep the operator's own words one Undo away", () => {
  it('trying a second example over the first still undoes to what the operator wrote', async () => {
    await openNew();
    typeInto(textarea('schedule-prompt'), 'my own words');

    await chooseFromSelect('schedule-example', 'Security audit');
    await chooseFromSelect('schedule-example', 'Check for dependency updates');
    expect(textarea('schedule-prompt').value).toContain("Investigate this repository's");

    button('Undo').click();
    await settle();

    expect(textarea('schedule-prompt').value).toBe('my own words');
  });

  it('an example over an empty prompt, then another: there was nothing to undo', async () => {
    await openNew();

    await chooseFromSelect('schedule-example', 'Security audit');
    await chooseFromSelect('schedule-example', 'Check for dependency updates');

    expect(editor().textContent).not.toContain('Replaced your prompt');
  });

  it('an example the operator then edited is their text: the next example offers it back', async () => {
    await openNew();
    await chooseFromSelect('schedule-example', 'Security audit');
    typeInto(textarea('schedule-prompt'), 'Security audit, but only the API.');

    await chooseFromSelect('schedule-example', 'Check for dependency updates');
    button('Undo').click();
    await settle();

    expect(textarea('schedule-prompt').value).toBe('Security audit, but only the API.');
  });
});

describe('schedule editor: a tab close or reload', () => {
  const unloadHeld = (): boolean => {
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  };

  it('keeps the browser prompt armed only while the editor has pending edits', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await openFromRow();
    expect(unloadHeld()).toBe(false);

    typeInto(input('schedule-name'), 'Renamed');
    await settle();
    expect(unloadHeld()).toBe(true);

    typeInto(input('schedule-name'), 'Weekly deps');
    await settle();
    expect(unloadHeld()).toBe(false);

    // Gone with the editor.
    typeInto(input('schedule-name'), 'Renamed');
    button('Cancel').click();
    await settle();
    button('Discard').click();
    await settle();
    expect(scheduleEditor()).toBeNull();
    expect(unloadHeld()).toBe(false);
  });
});
