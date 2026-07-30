// Schedules section suite (issue #247 / ADR-0062) on
// /repos/:id/settings/schedules. The section is immediate-CRUD like Secrets —
// every action is its own server call, so there is no unsaved-changes guard —
// wrapped around one editor form at a time.
//
// The properties worth pinning here: the preset cadence editor renders the
// exact cron the server will store, a stored cron decomposes back into the
// preset that made it (and into Advanced when it does not), a PATCH carries
// only what changed, client-side refusals cost no request, and the upcoming
// firings shown under the editor are always the SERVER's answer.

import { describe, expect, it, vi } from 'vitest';
import {
  BAD_CRON,
  REPO_ID,
  baseSchedule,
  button,
  chooseFromSelect,
  chooseNative,
  container,
  h,
  input,
  installRepoSettingsHooks,
  mountSettings,
  schedulesSection,
  settle,
  settlePreview,
  submitFormWithin,
  textarea,
  toggleCheckbox,
  typeInto,
  waitFor,
} from '../harness';

installRepoSettingsHooks();

const mountSchedules = () => mountSettings(`/repos/${REPO_ID}/settings/schedules`);

/** A chip-toggle (flow / weekday) by its form name. */
const chip = (name: string): HTMLButtonElement => {
  const el = container.querySelector<HTMLButtonElement>(`button[name="${name}"]`);
  if (!el) throw new Error(`missing chip-toggle button[name="${name}"]`);
  return el;
};

const openEditor = async (): Promise<void> => {
  button('+ Add schedule').click();
  await settle();
};

describe('RepoSettings schedules list', () => {
  it('renders the empty state before any schedule exists', async () => {
    await mountSchedules();
    await waitFor(
      () => (schedulesSection().textContent?.includes('No schedules yet') ? true : null),
      'empty state',
    );
    expect(schedulesSection().textContent).toContain('No schedules yet');
  });

  it('renders a row per schedule with its human cadence, state and flow labels', async () => {
    h.schedules = [
      baseSchedule({ name: 'Weekly deps', cadence: '30 6 * * 1,4', flows: ['autolander'] }),
      baseSchedule({
        id: 'sched_2',
        name: 'Nightly audit',
        cadence: '*/15 * * * *',
        flows: ['human-triage'],
        enabled: false,
      }),
    ];
    await mountSchedules();
    await waitFor(
      () => (schedulesSection().textContent?.includes('Weekly deps') ? true : null),
      'schedule rows',
    );

    const text = schedulesSection().textContent ?? '';
    // A recognized cadence reads back in words…
    expect(text).toContain('Weekly on Mon, Thu at 06:30');
    // …an Advanced one has no honest short form but itself.
    expect(text).toContain('*/15 * * * *');
    expect(text).toContain('enabled');
    expect(text).toContain('disabled');
    // Flow keys render as their catalog labels, never as raw keys.
    expect(text).toContain('Autolander');
    expect(text).toContain('Human triage');
  });
});

describe('RepoSettings schedules create', () => {
  it('POSTs the preset-rendered cron with the flows in catalog order', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(input('schedule-name'), 'Weekly deps');
    typeInto(textarea('schedule-prompt'), 'Investigate dependency updates.');
    // Picked in the reverse of catalog order on purpose — the body must not
    // carry the click order.
    chip('flow-human-triage').click();
    chip('flow-autolander').click();
    await settle();

    chooseNative('cadence_mode', 'weekly');
    await settle();
    typeInto(input('cadence_time'), '06:30');
    // Weekly opens on Monday so the mode is never illegal on arrival; adding
    // Thursday is the whole edit.
    expect(chip('weekday-1').getAttribute('aria-pressed')).toBe('true');
    chip('weekday-4').click();
    await settle();

    submitFormWithin(schedulesSection());
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
    // The editor collapses and the new row joins the list.
    expect(schedulesSection().querySelector('input[name="schedule-name"]')).toBeNull();
    expect(schedulesSection().textContent).toContain('Weekly deps');
  });

  it('renders daily and monthly presets as their cron expressions', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(input('schedule-name'), 'Daily'); // daily is the default mode
    typeInto(textarea('schedule-prompt'), 'Look around.');
    typeInto(input('cadence_time'), '07:05');
    submitFormWithin(schedulesSection());
    await settle();

    expect(h.scheduleBodies[0]?.cadence).toBe('5 7 * * *');

    await openEditor();
    typeInto(input('schedule-name'), 'Monthly');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    chooseNative('cadence_mode', 'monthly');
    await settle();
    typeInto(input('cadence_time'), '09:15');
    chooseNative('cadence_day', '3');
    submitFormWithin(schedulesSection());
    await settle();

    expect(h.scheduleBodies[1]?.cadence).toBe('15 9 3 * *');
  });

  it('sends the overrides only when they are actually set', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(input('schedule-name'), 'With overrides');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    typeInto(input('schedule_budget_minutes'), '45');
    await chooseFromSelect('schedule_model', 'Sonnet');
    toggleCheckbox(input('schedule_enabled'), false);
    submitFormWithin(schedulesSection());
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
});

describe('RepoSettings schedules example picker', () => {
  it('fills an empty prompt and keeps the picker on its placeholder', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    await chooseFromSelect('schedule-example', 'Check for dependency updates');

    const prompt = textarea('schedule-prompt');
    expect(prompt.value).toContain("Investigate this repository's dependencies");
    expect(prompt.value.split('\n').length).toBeGreaterThan(1);
    // The example is a starter, never a stored choice: the picker goes
    // straight back to its placeholder row.
    expect(
      container.querySelector('button[name="schedule-example"] .select-field-label')?.textContent,
    ).toBe('Start from an example…');
  });

  it('asks before overwriting a written prompt, and honours a decline', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(textarea('schedule-prompt'), 'my own words');
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValueOnce(false);
    await chooseFromSelect('schedule-example', 'Security audit');

    expect(confirmSpy).toHaveBeenCalledWith('Replace the prompt with this example?');
    expect(textarea('schedule-prompt').value).toBe('my own words');

    confirmSpy.mockReturnValueOnce(true);
    await chooseFromSelect('schedule-example', 'Security audit');
    expect(textarea('schedule-prompt').value).toContain('security-review skill');
  });
});

describe('RepoSettings schedules validation', () => {
  const expectBlocked = (message: string): void => {
    expect(h.scheduleBodies).toHaveLength(0);
    expect(schedulesSection().textContent).toContain(message);
  };

  it('refuses an empty name without a request', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(textarea('schedule-prompt'), 'Look around.');
    submitFormWithin(schedulesSection());
    await settle();

    expectBlocked('Name is required.');
  });

  it('refuses a schedule with neither a prompt nor a flow', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(input('schedule-name'), 'Empty');
    submitFormWithin(schedulesSection());
    await settle();

    expectBlocked('A schedule needs a prompt, a flow, or both.');
  });

  it('refuses a weekly cadence with no weekday picked', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(input('schedule-name'), 'Weekly');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    chooseNative('cadence_mode', 'weekly');
    await settle();
    chip('weekday-1').click(); // the default pick, toggled back off
    await settle();
    submitFormWithin(schedulesSection());
    await settle();

    expectBlocked('Pick at least one weekday.');
  });

  it('refuses a budget below one minute', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(input('schedule-name'), 'Zero budget');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    typeInto(input('schedule_budget_minutes'), '0');
    submitFormWithin(schedulesSection());
    await settle();

    expectBlocked('Budget must be a whole number of minutes, 1 or more.');
  });

  it('surfaces a server refusal verbatim', async () => {
    // A name is unique per repo; the 409 text is the operator-facing message.
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    typeInto(input('schedule-name'), 'Weekly deps');
    typeInto(textarea('schedule-prompt'), 'Look around.');
    submitFormWithin(schedulesSection());
    await settle();

    expect(schedulesSection().textContent).toContain('name already taken');
    // The editor stays open on a refusal — the operator has a name to fix.
    expect(schedulesSection().querySelector('input[name="schedule-name"]')).not.toBeNull();
  });
});

describe('RepoSettings schedules edit', () => {
  it('prefills a preset cadence and PATCHes only the dirty fields', async () => {
    h.schedules = [
      baseSchedule({
        name: 'Weekly deps',
        cadence: '30 6 * * 1,4',
        prompt: 'Investigate available dependency updates.',
        flows: ['autolander'],
      }),
    ];
    await mountSchedules();
    await waitFor(() => button('Edit'), 'edit button');
    button('Edit').click();
    await settle();

    // The stored cron decomposed back into the preset that renders it.
    expect(input('schedule-name').value).toBe('Weekly deps');
    expect(container.querySelector<HTMLSelectElement>('select[name="cadence_mode"]')?.value).toBe(
      'weekly',
    );
    expect(input('cadence_time').value).toBe('06:30');
    expect(chip('weekday-1').getAttribute('aria-pressed')).toBe('true');
    expect(chip('weekday-4').getAttribute('aria-pressed')).toBe('true');
    expect(chip('weekday-2').getAttribute('aria-pressed')).toBe('false');
    expect(chip('flow-autolander').getAttribute('aria-pressed')).toBe('true');

    typeInto(input('cadence_time'), '07:00');
    submitFormWithin(schedulesSection());
    await settle();

    // Only the cadence moved, so only the cadence rides the PATCH.
    expect(h.scheduleBodies).toEqual([{ cadence: '0 7 * * 1,4' }]);
    expect(h.schedules[0]?.cadence).toBe('0 7 * * 1,4');
  });

  it('opens an unrecognizable cadence in Advanced with the expression untouched', async () => {
    h.schedules = [baseSchedule({ cadence: '*/15 * * * *' })];
    await mountSchedules();
    await waitFor(() => button('Edit'), 'edit button');
    button('Edit').click();
    await settle();

    expect(container.querySelector<HTMLSelectElement>('select[name="cadence_mode"]')?.value).toBe(
      'advanced',
    );
    expect(input('cadence_expr').value).toBe('*/15 * * * *');
    expect(container.querySelector('input[name="cadence_time"]')).toBeNull();
  });

  it('clears an override back to inherit and drops a flow in one PATCH', async () => {
    h.schedules = [baseSchedule({ flows: ['autolander', 'human-triage'], model: 'sonnet' })];
    await mountSchedules();
    await waitFor(() => button('Edit'), 'edit button');
    button('Edit').click();
    await settle();

    chip('flow-autolander').click();
    await chooseFromSelect('schedule_model', 'Inherit repo AFK default');
    submitFormWithin(schedulesSection());
    await settle();

    expect(h.scheduleBodies).toEqual([{ flows: ['human-triage'], model: null }]);
  });

  it('reports an unchanged edit instead of PATCHing nothing', async () => {
    h.schedules = [baseSchedule()];
    await mountSchedules();
    await waitFor(() => button('Edit'), 'edit button');
    button('Edit').click();
    await settle();

    submitFormWithin(schedulesSection());
    await settle();

    expect(h.scheduleBodies).toHaveLength(0);
    expect(schedulesSection().textContent).toContain('Nothing to save.');
  });
});

describe('RepoSettings schedules delete', () => {
  it('asks for confirmation before removing the schedule', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await mountSchedules();
    await waitFor(() => button('Delete'), 'delete button');

    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValueOnce(false);
    button('Delete').click();
    await settle();
    expect(confirmSpy).toHaveBeenCalledWith('Delete schedule "Weekly deps"?');
    expect(h.schedules).toHaveLength(1);

    confirmSpy.mockReturnValueOnce(true);
    button('Delete').click();
    await settle();
    expect(h.schedules).toHaveLength(0);
    expect(schedulesSection().textContent).toContain('No schedules yet');
  });
});

describe('RepoSettings schedules pause', () => {
  it('banners a struck-out schedule and re-enables it through its own endpoint', async () => {
    h.schedules = [baseSchedule({ paused: true, consecutive_failures: 3 })];
    await mountSchedules();
    await waitFor(() => container.querySelector('.banner.error'), 'paused banner');

    const banner = schedulesSection().querySelector('.banner.error');
    expect(banner?.getAttribute('role')).toBe('alert');
    expect(banner?.textContent).toContain('Paused after 3 failures');

    button('Re-enable').click();
    await settle();

    expect(h.schedules[0]?.paused).toBe(false);
    expect(h.schedules[0]?.consecutive_failures).toBe(0);
    // Re-enabling is its own endpoint, never a PATCH — the edit form cannot
    // clear a pause.
    expect(h.scheduleBodies).toHaveLength(0);
    expect(schedulesSection().querySelector('.banner.error')).toBeNull();
  });
});

describe('RepoSettings schedules cadence preview', () => {
  it('renders the upcoming firings the server reports', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();
    await settlePreview();

    // The preview is asked about the cron the form would store, never about
    // the preset — one parser, and it is the server's.
    expect(h.cronPreviewExprs).toContain('0 6 * * *');
    const preview = schedulesSection().querySelector('.cadence-preview');
    expect(preview?.textContent).toBe('Next: Mon 2026-08-03 06:00, Thu 2026-08-06 06:00');
    expect(preview?.classList.contains('invalid')).toBe(false);
  });

  it('renders the parser refusal verbatim for an expression it cannot read', async () => {
    await mountSchedules();
    await waitFor(() => button('+ Add schedule'), 'add button');
    await openEditor();

    chooseNative('cadence_mode', 'advanced');
    await settle();
    typeInto(input('cadence_expr'), BAD_CRON);
    await settlePreview();

    const preview = schedulesSection().querySelector('.cadence-preview');
    expect(preview?.textContent).toBe('cron: expected 5 fields, got 1');
    expect(preview?.classList.contains('invalid')).toBe(true);
  });
});
