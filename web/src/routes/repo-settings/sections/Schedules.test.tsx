// Schedules list suite (issue #61 §8, ADR-0062) on /repos/:id/settings. The
// rows act at once — a switch flips a Schedule on or off with one PATCH, a
// paused row re-enables through its own endpoint — and opening a row goes
// to the editor's own URL over the page, which stays where it was. The
// editor itself is ScheduleEditor.test.tsx.

import { describe, expect, it } from 'vitest';
import {
  REPO_ID,
  baseSchedule,
  button,
  container,
  emitRepoChanged,
  emitRunChanged,
  h,
  installRepoSettingsHooks,
  mountSettings,
  routerHistory,
  scheduleEditor,
  schedulesSection,
  settle,
  switchButton,
  toastText,
  waitFor,
} from '../harness';
import { flowsText, lastRunText, statusLine } from './Schedules';

installRepoSettingsHooks();

const BASE = `/repos/${REPO_ID}/settings`;
const mountSchedules = () => mountSettings(`${BASE}/schedules`);

const rows = (): HTMLElement[] =>
  Array.from(schedulesSection().querySelectorAll<HTMLElement>('li.schedule-row'));
const rowNamed = (name: string): HTMLElement => {
  const row = rows().find((el) => el.querySelector('strong')?.textContent === name);
  if (!row) throw new Error(`missing schedule row "${name}"`);
  return row;
};
const rowLink = (name: string): HTMLAnchorElement => {
  const link = rowNamed(name).querySelector<HTMLAnchorElement>('a.schedule-row-main');
  if (!link) throw new Error(`row "${name}" has no link`);
  return link;
};
const newLink = (): HTMLAnchorElement => {
  const link = schedulesSection().querySelector<HTMLAnchorElement>('a.schedule-new');
  if (!link) throw new Error('missing + New schedule');
  return link;
};
const waitForRows = async (): Promise<void> => {
  await waitFor(() => (rows().length > 0 ? true : null), 'schedule rows');
};

const NOW = Date.parse('2026-08-03T12:00:00.000Z');

describe('schedule row words', () => {
  it('names the flows in catalog labels, or says prompt only', () => {
    const catalog = [
      { key: 'autolander', label: 'Autolander', description: '' },
      { key: 'human-triage', label: 'Human triage', description: '' },
    ];
    expect(flowsText([], catalog)).toBe('prompt only');
    expect(flowsText(['autolander'], catalog)).toBe('Autolander flow');
    expect(flowsText(['autolander', 'human-triage'], catalog)).toBe(
      'Autolander, Human triage flows',
    );
    // A flow the catalog no longer knows keeps its key rather than vanishing.
    expect(flowsText(['retired'], catalog)).toBe('retired flow');
  });

  it('reads the last run as the server reported it, plus how long ago', () => {
    expect(lastRunText(null, NOW)).toBe('never ran');
    const at = (iso: string, outcome: 'active' | 'success' | 'death' | 'timeout' | 'stopped') => ({
      id: 'run_1',
      started_at: iso,
      ended_at: outcome === 'active' ? null : iso,
      outcome,
    });
    expect(lastRunText(at('2026-08-03T11:00:00.000Z', 'active'), NOW)).toBe('running now');
    expect(lastRunText(at('2026-08-03T10:00:00.000Z', 'success'), NOW)).toBe('succeeded 2 h ago');
    expect(lastRunText(at('2026-08-02T06:00:00.000Z', 'death'), NOW)).toBe('died yesterday');
    expect(lastRunText(at('2026-07-30T06:00:00.000Z', 'timeout'), NOW)).toBe(
      'timed out 4 days ago',
    );
    expect(lastRunText(at('2026-08-03T11:59:30.000Z', 'stopped'), NOW)).toBe('stopped just now');
  });

  it('puts the next firing, off, or the pause first, then the last run', () => {
    expect(
      statusLine(baseSchedule({ next_run_display: 'Thu 2026-08-06 06:30', last_run: null }), NOW),
    ).toBe('Next Thu 2026-08-06 06:30 · last run never ran');
    expect(statusLine(baseSchedule({ enabled: false, next_run_display: null }), NOW)).toBe(
      'Off · last run never ran',
    );
    expect(statusLine(baseSchedule({ next_run_display: null }), NOW)).toBe(
      'No next run · last run never ran',
    );
    expect(
      statusLine(
        baseSchedule({
          paused: true,
          consecutive_failures: 3,
          next_run_display: null,
          last_run: {
            id: 'run_9',
            started_at: '2026-08-03T10:00:00.000Z',
            ended_at: '2026-08-03T10:30:00.000Z',
            outcome: 'death',
          },
        }),
        NOW,
      ),
    ).toBe('Paused after 3 failed runs · last run died 1 h ago');
  });
});

describe('schedules list', () => {
  it('renders the empty state before any schedule exists, with the way to add one', async () => {
    await mountSchedules();
    await waitFor(
      () => (schedulesSection().textContent?.includes('No schedules yet') ? true : null),
      'empty state',
    );
    expect(schedulesSection().textContent).toContain('No schedules yet');
    expect(newLink().getAttribute('href')).toBe(`${BASE}/schedules/new`);
    // No duplicate card title: the page section's heading is the one heading.
    expect(schedulesSection().querySelector('h2')).toBeNull();
  });

  it('renders a row per schedule: name, cadence in words, flows, next run, last run', async () => {
    h.schedules = [
      baseSchedule({
        name: 'Weekly deps',
        cadence: '30 6 * * 1,4',
        flows: ['autolander'],
        next_run_display: 'Thu 2026-08-06 06:30',
        last_run: {
          id: 'run_1',
          started_at: '2026-08-03T05:00:00.000Z',
          ended_at: '2026-08-03T05:20:00.000Z',
          outcome: 'success',
        },
      }),
      baseSchedule({
        id: 'sched_2',
        name: 'Nightly audit',
        cadence: '*/15 * * * *',
        flows: ['autolander', 'human-triage'],
        enabled: false,
        next_run_display: null,
      }),
      baseSchedule({
        id: 'sched_3',
        name: 'Docs drift',
        cadence: '0 17 * * 5',
        flows: [],
        paused: true,
        consecutive_failures: 3,
        next_run_display: null,
      }),
    ];
    await mountSchedules();
    await waitForRows();

    const deps = rowNamed('Weekly deps');
    expect(deps.textContent).toContain('Weekly on Mon, Thu at 06:30 · Autolander flow');
    expect(deps.textContent).toContain('Next Thu 2026-08-06 06:30 · last run succeeded');
    expect(rowLink('Weekly deps').getAttribute('href')).toBe(`${BASE}/schedules/sched_1`);
    // The switch is named after the row and reads the row's state.
    const toggle = switchButton('schedule-enabled-sched_1');
    expect(toggle.getAttribute('aria-label')).toBe('Weekly deps enabled');
    expect(toggle.getAttribute('aria-checked')).toBe('true');

    const audit = rowNamed('Nightly audit');
    // An Advanced cadence has no honest short form but itself.
    expect(audit.textContent).toContain('*/15 * * * * · Autolander, Human triage flows');
    expect(audit.textContent).toContain('Off · last run never ran');
    expect(switchButton('schedule-enabled-sched_2').getAttribute('aria-checked')).toBe('false');

    const drift = rowNamed('Docs drift');
    expect(drift.querySelector('.chip')?.textContent).toBe('paused');
    expect(drift.textContent).toContain('Weekly on Fri at 17:00 · prompt only');
    expect(drift.textContent).toContain('Paused after 3 failed runs');
    // A paused row offers Re-enable instead of the switch.
    expect(drift.querySelector('button[role="switch"]')).toBeNull();
    expect(drift.querySelector('button')?.textContent).toBe('Re-enable');
  });

  it('flips a schedule off and on at once, one PATCH each, and says so', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await mountSchedules();
    await waitForRows();

    switchButton('schedule-enabled-sched_1').click();
    await settle();

    expect(h.scheduleBodies).toEqual([{ enabled: false }]);
    expect(h.schedules[0]?.enabled).toBe(false);
    expect(toastText()).toBe('"Weekly deps" turned off');
    expect(switchButton('schedule-enabled-sched_1').getAttribute('aria-checked')).toBe('false');
    expect(rowNamed('Weekly deps').textContent).toContain('Off ·');

    switchButton('schedule-enabled-sched_1').click();
    await settle();

    expect(h.scheduleBodies).toEqual([{ enabled: false }, { enabled: true }]);
    expect(toastText()).toBe('"Weekly deps" turned on');
  });

  it('re-enables a paused schedule through its own endpoint, never a PATCH', async () => {
    h.schedules = [baseSchedule({ name: 'Docs drift', paused: true, consecutive_failures: 3 })];
    await mountSchedules();
    await waitForRows();

    button('Re-enable').click();
    await settle();

    expect(h.schedules[0]?.paused).toBe(false);
    expect(h.schedules[0]?.consecutive_failures).toBe(0);
    expect(h.scheduleBodies).toHaveLength(0);
    expect(toastText()).toBe('"Docs drift" re-enabled');
    const row = rowNamed('Docs drift');
    expect(row.querySelector('.chip')).toBeNull();
    expect(row.querySelector('button[role="switch"]')).not.toBeNull();
  });

  it('refetches on repo.changed and on run.changed', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await mountSchedules();
    await waitForRows();

    h.schedules = [baseSchedule({ name: 'Weekly deps', paused: true, consecutive_failures: 3 })];
    emitRepoChanged();
    await waitFor(() => rowNamed('Weekly deps').querySelector('.chip'), 'the pause');

    h.schedules = [
      baseSchedule({
        name: 'Weekly deps',
        last_run: {
          id: 'run_2',
          started_at: '2026-08-03T05:00:00.000Z',
          ended_at: null,
          outcome: 'active',
        },
      }),
    ];
    emitRunChanged();
    await waitFor(
      () => (rowNamed('Weekly deps').textContent?.includes('running now') ? true : null),
      'the live run',
    );
  });

  it('opens a row at the editor URL over the page, which stays put', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    await mountSchedules();
    await waitForRows();
    const scrolls = h.scrolls.length;

    rowLink('Weekly deps').click();
    await settle();

    expect(routerHistory.get()).toBe(`${BASE}/schedules/sched_1`);
    expect(scheduleEditor()).not.toBeNull();
    expect(scheduleEditor()?.querySelector('h2')?.textContent).toBe('Edit schedule');
    // The page is still there, all of it, and did not scroll for the editor.
    expect(container.querySelectorAll('section.settings-section')).toHaveLength(10);
    expect(rows()).toHaveLength(1);
    expect(h.scrolls).toHaveLength(scrolls);
  });

  it('+ New schedule opens the editor for a new one', async () => {
    await mountSchedules();
    await waitFor(() => schedulesSection().querySelector('a.schedule-new'), 'the add link');

    newLink().click();
    await settle();

    expect(routerHistory.get()).toBe(`${BASE}/schedules/new`);
    expect(scheduleEditor()?.querySelector('h2')?.textContent).toBe('New schedule');
  });

  it('reports a refused row action without flipping the row', async () => {
    h.schedules = [baseSchedule({ name: 'Weekly deps' })];
    h.scheduleRefusal = { error: 'schedule is locked' };
    await mountSchedules();
    await waitForRows();

    switchButton('schedule-enabled-sched_1').click();
    await settle();

    expect(schedulesSection().querySelector('.banner.error')?.textContent).toContain(
      'schedule is locked',
    );
    expect(switchButton('schedule-enabled-sched_1').getAttribute('aria-checked')).toBe('true');
    expect(toastText()).toBe('');
  });
});
