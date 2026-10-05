// The schedule editor (issue #61 §8): one Schedule, at its own URL
// (/repos/:id/settings/schedules/:scheduleId, or …/schedules/new), over the
// settings page — full screen below 1024px with a back control, a side panel
// with a scrim from 1024px. The page stays mounted and scrolled under it.
//
// It is a dialog: role="dialog", aria-modal, focus moves in on open and is
// trapped while it shows, closing hands focus back to the row that opened
// it, and the page behind it does not scroll. Escape closes it when nothing
// is pending; a scrim click does the same.
//
// Its own Save: a new Schedule is one POST, an edited one a PATCH of the
// fields that changed. Closing with pending edits asks IN PLACE — the footer
// becomes "Discard your changes?" with Keep editing / Discard — and a URL
// change away from the editor while it is dirty (Back, a tab, the save bar)
// goes through the same ask, held by the router's useBeforeLeave: one ask,
// never two. Delete lives at the end of the editor as an inline
// confirmation. Run now, in the header, starts a run from the SAVED
// Schedule: it is disabled while edits are pending (the footer says why),
// absent for a Schedule that was never saved, and a 409 shows the server's
// reason verbatim next to the button.
//
// The cadence editor: a Schedule stores one cron expression; Daily, Weekly
// and Monthly are an editing skin over that string (lib/cronPreset owns both
// directions) and Cron is the raw expression. Whatever the mode, the
// upcoming firings are SERVER-rendered — the SPA never computes a firing.
//
// Overrides (budget, agent, model, effort) read "Inherited · <what the layer
// below gives>" while unset, and "set here" with a Default line and Reset
// once set. The agent below is the repo's AFK agent (resolved by the server,
// handed in as afkProviderId); the model and effort below it are "the
// repo's AFK model / effort" in words — the browser resolves no chain.

import { useBeforeLeave, type BeforeLeaveEventArgs } from '@solidjs/router';
import {
  For,
  Show,
  createEffect,
  createMemo,
  createResource,
  createSignal,
  createUniqueId,
  onCleanup,
  onMount,
  untrack,
  type JSX,
} from 'solid-js';
import {
  ApiError,
  createRepoSchedule,
  deleteRepoSchedule,
  errorMessage,
  patchRepoSchedule,
  previewCron,
  runScheduleNow,
  type Provider,
  type Schedule,
  type ScheduleCreate,
  type ScheduleFlow,
  type SchedulePatch,
} from '../../../api';
import Banner from '../../../components/Banner';
import Icon from '../../../components/Icon';
import InlineConfirm from '../../../components/InlineConfirm';
import Segmented from '../../../components/Segmented';
import Select, { type SelectOption } from '../../../components/Select';
import ToggleSwitch from '../../../components/Switch';
import {
  MONTH_DAY_MAX,
  WEEKDAYS,
  cronToPreset,
  presetToCron,
  type CadenceMode,
} from '../../../lib/cronPreset';
import { createMediaQuery } from '../../../lib/media';
import { inheritPickLabel } from '../Field';
import { normInt, normText } from '../shared';

/** The app shell's layout breakpoint: the side panel and its scrim from here. */
const DESKTOP_QUERY = '(min-width: 1024px)';
/** Cadence default for a brand-new Schedule: early enough to be done by morning. */
const DEFAULT_TIME = '06:00';
/** Weekly's default pick (cron Monday), so the mode is never illegal on arrival. */
const DEFAULT_WEEKDAYS = [1];
/** The budget a Schedule inherits when it sets none (ADR-0062). */
const DEFAULT_BUDGET_MINUTES = 30;
/**
 * How long the cadence editor waits before asking the server what a changed
 * expression fires. Long enough that typing a raw cron is not one request per
 * keystroke, short enough that the preview still feels attached to the field.
 */
export const PREVIEW_DEBOUNCE_MS = 400;

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/**
 * The prompt starters the Examples pick offers. UI-only (ADR-0062): they are
 * editable starting text about the INVESTIGATION, never about routing — what
 * happens to the findings is the flows' business, so no example here names a
 * label or a CLI verb.
 *
 * The triage starter is the one that is not an investigation (ADR-0070): its
 * whole job is tracker work, and the seeded triage skill's unattended mode
 * already says where every output goes. It therefore runs WITHOUT a flow — a
 * flow's "file what you found" block would contradict it — and its label says
 * so, since the picker has no other place to tell the operator. The maintainer
 * placeholder is deliberate: left unfilled, the run can establish no
 * maintainer and so promotes nothing, which is the safe way to be wrong.
 */
export const PROMPT_EXAMPLES: readonly { key: string; label: string; text: string }[] = [
  {
    key: 'dependency-updates',
    label: 'Check for dependency updates',
    text: [
      "Investigate this repository's dependencies for available updates.",
      '',
      'For every dependency with a newer release, read its changelog and note:',
      '- what actually changed, and whether any of it breaks how this repo uses it',
      '- whether the release closes a security advisory that reaches us',
      '- how much of this repo an update would touch',
      '',
      'Then summarize which updates are worth doing now, which can wait, and why.',
    ].join('\n'),
  },
  {
    key: 'security-audit',
    label: 'Security audit',
    text: [
      'Run the security-review skill over the current state of the default branch.',
      '',
      'Cover at least: how untrusted input is validated at the process boundaries,',
      'the authentication and authorization checks, secret handling, and every path',
      'that writes to disk or shells out.',
      '',
      'Write up the findings that deserve action, worst first — each with the file',
      'it lives in and why it matters.',
    ].join('\n'),
  },
  {
    key: 'triage-inbox',
    label: 'Prepare the triage inbox (no flow)',
    text: [
      "Work through this repository's triage inbox with the triage skill, in its",
      'unattended mode. Nobody is in this session.',
      '',
      'Maintainer accounts — the only ones whose comments count as decisions:',
      '<fill in the logins>',
      '',
      'Prepare every waiting issue for a decision and leave promotion to the',
      'maintainers, as the skill describes.',
    ].join('\n'),
  },
];

/**
 * The selection in CATALOG order, always — the order a firing appends the
 * flows' instruction blocks in, and the order the server normalizes to on
 * write, so the form shows what the run will read. Keys the catalog does not
 * know (a flow retired under a stored Schedule) survive at the end rather than
 * being silently dropped.
 */
export function canonicalFlows(selected: readonly string[], catalog: ScheduleFlow[]): string[] {
  const known = catalog.map((flow) => flow.key).filter((key) => selected.includes(key));
  const unknown = selected.filter((key) => !catalog.some((flow) => flow.key === key));
  return [...known, ...unknown];
}

/** Both sides are already canonical, so equality is a plain ordered compare. */
function sameFlows(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((key, i) => key === b[i]);
}

/** The editor's fields, as its problems and the server's `field` keys name them. */
type EditorFieldKey =
  | 'name'
  | 'prompt'
  | 'flows'
  | 'weekdays'
  | 'time'
  | 'cron'
  | 'day'
  | 'cadence'
  | 'budget'
  | 'provider'
  | 'model'
  | 'effort'
  | 'enabled';

/** The server's PATCH/POST keys → the field that shows the refusal. */
const SERVER_FIELDS: Record<string, EditorFieldKey> = {
  name: 'name',
  prompt: 'prompt',
  flows: 'flows',
  cadence: 'cadence',
  budget_minutes: 'budget',
  provider: 'provider',
  model: 'model',
  effort: 'effort',
  enabled: 'enabled',
};

/** The order Save walks the problems in — the first one gets the focus. */
const FOCUS_ORDER: EditorFieldKey[] = [
  'name',
  'prompt',
  'flows',
  'weekdays',
  'time',
  'day',
  'cron',
  'cadence',
  'budget',
  'provider',
  'model',
  'effort',
  'enabled',
];

type Problems = Partial<Record<EditorFieldKey, string>>;

export interface ScheduleEditorProps {
  repoId: string;
  /** null = a new Schedule; otherwise the saved row the editor starts from. */
  schedule: Schedule | null;
  providers: Provider[];
  /**
   * The provider id this repo's AFK runs resolve to — the layer under a
   * Schedule's own agent pick. null while it is not known.
   */
  afkProviderId: string | null;
  flows: ScheduleFlow[];
  /** The editor's own URL: a navigation that stays on it is not a leave. */
  url: string;
  /** Close without saving (the page owns the navigation). */
  onClose: () => void;
  /** Saved (created or patched): the page closes the editor and says so. */
  onSaved: (schedule: Schedule, created: boolean) => void;
  /** Deleted: the page closes the editor and says so. */
  onDeleted: (schedule: Schedule) => void;
  /** Run now accepted: the page closes the editor and says so. */
  onRan: (schedule: Schedule) => void;
}

export default function ScheduleEditor(props: ScheduleEditorProps) {
  const uid = createUniqueId();
  const id = (key: string): string => `se-${uid}-${key}`;
  const titleId = id('title');
  const noteId = id('note');
  const askId = id('ask');
  const desktop = createMediaQuery(DESKTOP_QUERY);

  // The editor mounts fresh for every Schedule it opens on (the page keys it
  // on the URL) and unmounts when it closes, so seeding the drafts once here
  // IS the pre-fill — which is what the untracked read says out loud.
  const seed = untrack(() => props.schedule);
  const seedPreset = seed === null ? null : cronToPreset(seed.cadence);

  const [name, setName] = createSignal(seed?.name ?? '');
  const [prompt, setPrompt] = createSignal(seed?.prompt ?? '');
  const [picked, setPicked] = createSignal<string[]>(seed?.flows ?? []);
  // The selection the form SHOWS and SENDS: always the catalog's order, never
  // the click order, derived so it settles the moment the catalog arrives.
  const selected = (): string[] => canonicalFlows(picked(), props.flows);
  const [enabled, setEnabled] = createSignal(seed?.enabled ?? true);
  const [budget, setBudget] = createSignal(
    seed?.budget_minutes == null ? '' : String(seed.budget_minutes),
  );
  const [provider, setProvider] = createSignal(seed?.provider ?? '');
  const [model, setModel] = createSignal(seed?.model ?? '');
  const [effort, setEffort] = createSignal(seed?.effort ?? '');

  // Cadence drafts. A stored expression that decomposes opens in its preset;
  // one that does not opens in Cron with the expression untouched.
  const [mode, setMode] = createSignal<CadenceMode>(
    seedPreset?.mode ?? (seed === null ? 'daily' : 'advanced'),
  );
  const [time, setTime] = createSignal(seedPreset?.time ?? DEFAULT_TIME);
  const [weekdays, setWeekdays] = createSignal(
    seedPreset !== null && seedPreset.mode === 'weekly' ? seedPreset.weekdays : DEFAULT_WEEKDAYS,
  );
  const [monthDay, setMonthDay] = createSignal(
    seedPreset !== null && seedPreset.mode === 'monthly' ? seedPreset.day : 1,
  );
  const [rawCron, setRawCron] = createSignal(seed?.cadence ?? '');

  const [busy, setBusy] = createSignal<'save' | 'delete' | 'run' | null>(null);
  const [problems, setProblems] = createSignal<Problems>({});
  const [banner, setBanner] = createSignal<string | null>(null);
  /** The server's reason for refusing a Run now — shown verbatim, by the button. */
  const [refusal, setRefusal] = createSignal<string | null>(null);
  /** The prompt an example replaced, while Undo is offered. */
  const [replaced, setReplaced] = createSignal<string | null>(null);

  /** The one thing a Schedule stores about time: whatever the mode renders. */
  const cadence = (): string => {
    const current = mode();
    if (current === 'advanced') return rawCron().trim();
    if (current === 'daily') return presetToCron({ mode: 'daily', time: time() });
    if (current === 'weekly') {
      return presetToCron({ mode: 'weekly', time: time(), weekdays: weekdays() });
    }
    return presetToCron({ mode: 'monthly', time: time(), day: monthDay() });
  };

  // Pending edits: the drafts against what the editor opened on. Any edit
  // counts, including a mode switch that renders the same expression — the
  // operator is in the middle of something either way.
  const snapshot = (): string =>
    JSON.stringify({
      name: name().trim(),
      prompt: prompt().trim(),
      flows: selected(),
      enabled: enabled(),
      budget: budget().trim(),
      provider: provider(),
      model: model(),
      effort: effort(),
      mode: mode(),
      cadence: cadence(),
    });
  const initial = untrack(snapshot);
  const dirty = createMemo(() => snapshot() !== initial);

  // Debounced preview source: the first value is immediate (an editor that
  // just opened already knows its cadence), every later change waits out the
  // typing. The fetch itself is the server's — see the file header.
  const [previewExpr, setPreviewExpr] = createSignal(cadence());
  createEffect(() => {
    const expr = cadence();
    const timer = setTimeout(() => setPreviewExpr(expr), PREVIEW_DEBOUNCE_MS);
    onCleanup(() => clearTimeout(timer));
  });
  const [preview] = createResource(
    () => (previewExpr() === '' ? null : previewExpr()),
    (expr) => previewCron(expr),
  );

  const clearProblem = (key: EditorFieldKey): void => {
    if (problems()[key] === undefined) return;
    const next = { ...problems() };
    delete next[key];
    setProblems(next);
  };

  const toggleFlow = (key: string): void => {
    setPicked(picked().includes(key) ? picked().filter((k) => k !== key) : [...picked(), key]);
    clearProblem('prompt');
    clearProblem('flows');
  };

  const toggleWeekday = (value: number): void => {
    setWeekdays(
      weekdays().includes(value)
        ? weekdays().filter((day) => day !== value)
        : [...weekdays(), value].sort((a, b) => a - b),
    );
    clearProblem('weekdays');
  };

  // An example fills the prompt at once. When it replaces words the operator
  // wrote, those stay one Undo away — inside the editor, which covers the
  // page's toast below 1024px.
  const applyExample = (key: string): void => {
    const example = PROMPT_EXAMPLES.find((candidate) => candidate.key === key);
    if (example === undefined) return;
    const current = prompt();
    setReplaced(current.trim() !== '' && current !== example.text ? current : null);
    setPrompt(example.text);
    clearProblem('prompt');
  };
  const undoExample = (): void => {
    const previous = replaced();
    if (previous === null) return;
    setPrompt(previous);
    setReplaced(null);
    document.getElementById(id('prompt'))?.focus();
  };

  const providerOptions = (): SelectOption[] =>
    props.providers.map((p) => ({ value: p.id, label: p.display_name }));
  const providerName = (providerId: string | null): string | null =>
    props.providers.find((candidate) => candidate.id === providerId)?.display_name ?? null;
  // A Schedule's override is one more default rung ABOVE the AFK layering
  // (ADR-0062), so the provider its model/effort catalogs come from is this
  // Schedule's own pick when it has one, else the provider the repo's AFK
  // runs resolve to — which the server resolved (afkProviderId); no chain is
  // walked here. A stored value foreign to the new catalog stays selected as
  // "(not in catalog)" rather than silently changing.
  const effectiveProvider = (): Provider | null => {
    const providerId = provider() !== '' ? provider() : props.afkProviderId;
    return props.providers.find((candidate) => candidate.id === providerId) ?? null;
  };

  const budgetMinutes = (): number | null => {
    const parsed = normInt(budget());
    return parsed === undefined ? null : parsed;
  };

  /** Client-side refusals, so an obviously incomplete form costs no request. */
  const validate = (): Problems => {
    const found: Problems = {};
    if (name().trim() === '') found.name = 'Give the schedule a name.';
    if (prompt().trim() === '' && selected().length === 0) {
      found.prompt = 'Add a prompt, a flow, or both.';
    }
    if (mode() === 'weekly' && weekdays().length === 0) {
      found.weekdays = 'Pick at least one weekday.';
    } else if (mode() === 'advanced' && rawCron().trim() === '') {
      found.cron = 'Enter a cron expression.';
    } else if (cadence() === '') {
      found.time = 'Enter a time.';
    }
    const parsed = normInt(budget());
    if (parsed === undefined || (parsed !== null && parsed < 1)) {
      found.budget = 'Use a whole number of minutes, 1 or more, or leave it empty.';
    }
    return found;
  };

  const createBody = (): ScheduleCreate => {
    const body: ScheduleCreate = {
      name: name().trim(),
      cadence: cadence(),
      prompt: prompt().trim(),
      flows: selected(),
      enabled: enabled(),
    };
    // The overrides ride the create only when actually set — an unset override
    // is the absence of a key, not a null.
    const minutes = budgetMinutes();
    if (minutes !== null) body.budget_minutes = minutes;
    const providerOverride = normText(provider());
    if (providerOverride !== null) body.provider = providerOverride;
    const modelOverride = normText(model());
    if (modelOverride !== null) body.model = modelOverride;
    const effortOverride = normText(effort());
    if (effortOverride !== null) body.effort = effortOverride;
    return body;
  };

  /** Only what actually changed — a PATCH is a diff, never a re-send. */
  const patchBody = (current: Schedule): SchedulePatch => {
    const patch: SchedulePatch = {};
    if (name().trim() !== current.name) patch.name = name().trim();
    if (cadence() !== current.cadence) patch.cadence = cadence();
    if (prompt().trim() !== current.prompt) patch.prompt = prompt().trim();
    if (!sameFlows(selected(), current.flows)) patch.flows = selected();
    if (enabled() !== current.enabled) patch.enabled = enabled();
    if (budgetMinutes() !== current.budget_minutes) patch.budget_minutes = budgetMinutes();
    if (normText(provider()) !== current.provider) patch.provider = normText(provider());
    if (normText(model()) !== current.model) patch.model = normText(model());
    if (normText(effort()) !== current.effort) patch.effort = normText(effort());
    return patch;
  };

  const focusProblem = (found: Problems): void => {
    const first = FOCUS_ORDER.find((key) => found[key] !== undefined);
    if (first === undefined) return;
    queueMicrotask(() => {
      const wrapper = document.getElementById(id(`field-${first}`));
      const control =
        document.getElementById(id(first)) ??
        wrapper?.querySelector<HTMLElement>(
          'input, select, textarea, button[role="radio"][tabindex="0"], button:not([tabindex="-1"])',
        ) ??
        null;
      control?.focus();
    });
  };

  // Closing. `closing` tells the leave guard below that the URL change it is
  // about to see is this editor's own doing.
  let closing = false;
  const close = (): void => {
    closing = true;
    props.onClose();
  };

  const submit = async (event: SubmitEvent): Promise<void> => {
    event.preventDefault();
    if (busy() !== null) return;
    setBanner(null);
    const found = validate();
    setProblems(found);
    if (Object.keys(found).length > 0) {
      focusProblem(found);
      return;
    }
    const repoID = props.repoId;
    let send: () => Promise<Schedule>;
    let created = false;
    if (seed === null) {
      const body = createBody();
      send = () => createRepoSchedule(repoID, body);
      created = true;
    } else {
      const patch = patchBody(seed);
      if (Object.keys(patch).length === 0) {
        // Nothing moved: the saved version already is this one.
        close();
        return;
      }
      send = () => patchRepoSchedule(repoID, seed.id, patch);
    }
    setBusy('save');
    try {
      const saved = await send();
      closing = true;
      props.onSaved(saved, created);
    } catch (err) {
      // A refusal that names a field shows at that field; anything else at
      // the top of the editor.
      const field = err instanceof ApiError ? err.field : undefined;
      const key = field !== undefined ? SERVER_FIELDS[field] : undefined;
      if (key !== undefined) {
        const next = { ...problems(), [key]: errorMessage(err) };
        setProblems(next);
        focusProblem(next);
      } else {
        setBanner(errorMessage(err));
      }
    } finally {
      setBusy(null);
    }
  };

  const remove = async (): Promise<void> => {
    if (seed === null) return;
    setBusy('delete');
    setBanner(null);
    try {
      await deleteRepoSchedule(props.repoId, seed.id);
      closing = true;
      props.onDeleted(seed);
    } catch (err) {
      setBanner(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  // Run now starts a run from the SAVED Schedule through the spawn pass. A
  // 409 is the server's reason, shown verbatim by the button; anything else
  // is a plain error.
  const runNow = async (): Promise<void> => {
    if (seed === null || busy() !== null) return;
    setBusy('run');
    setRefusal(null);
    setBanner(null);
    try {
      await runScheduleNow(props.repoId, seed.id);
      closing = true;
      props.onRan(seed);
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) setRefusal(err.message);
      else setBanner(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  // --- closing with pending edits: the in-place ask ------------------------
  const [asking, setAsking] = createSignal(false);
  // The navigation being held while the ask shows (none when the ask came
  // from the editor's own Cancel, back control, scrim or Escape).
  let held: BeforeLeaveEventArgs | null = null;
  let cancelButton: HTMLButtonElement | undefined;
  let keepButton: HTMLButtonElement | undefined;

  const requestClose = (): void => {
    if (busy() !== null) return;
    if (!dirty()) {
      close();
      return;
    }
    setAsking(true);
    queueMicrotask(() => keepButton?.focus());
  };
  const keepEditing = (): void => {
    held = null;
    setAsking(false);
    queueMicrotask(() => cancelButton?.focus());
  };
  const discard = (): void => {
    const event = held;
    held = null;
    setAsking(false);
    closing = true;
    // retry() without force: the guard lets it pass now, and any other guard
    // on the page still gets its say.
    if (event !== null) event.retry();
    else props.onClose();
  };

  // Any URL change away from the editor while it is dirty — Back, a tab, a
  // section chip — is held and asked about in place, exactly like Cancel.
  useBeforeLeave((event) => {
    if (event.defaultPrevented || closing || !dirty()) return;
    // A number is a history move (Back/Forward): the browser has already put
    // the destination in the address bar when the router asks.
    const destination = typeof event.to === 'number' ? window.location.pathname : event.to;
    if ((destination.split(/[?#]/, 1)[0] ?? '') === props.url) return;
    event.preventDefault();
    held = event;
    setAsking(true);
    queueMicrotask(() => keepButton?.focus());
  });
  // Nothing left to ask about (the edits were taken back): close as asked.
  createEffect(() => {
    if (asking() && !dirty()) discard();
  });

  // --- the dialog: focus, Escape, scroll lock ------------------------------
  let panel: HTMLElement | undefined;
  let heading: HTMLHeadingElement | undefined;
  // Captured before focus moves in, so closing can hand focus back to the
  // row (or the "+ New schedule" control) that opened the editor.
  const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const focusables = (): HTMLElement[] =>
    panel === undefined ? [] : Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE));

  const onKeyDown = (event: KeyboardEvent): void => {
    // A control that handled Escape itself (an open pick, an open inline
    // confirmation) says so; the editor stays.
    if (event.defaultPrevented) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      if (asking()) keepEditing();
      else requestClose();
      return;
    }
    if (event.key !== 'Tab' || panel === undefined) return;
    const items = focusables();
    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement;
    if (first === undefined || last === undefined) {
      event.preventDefault();
      heading?.focus();
      return;
    }
    if (event.shiftKey && (active === first || active === heading || !panel.contains(active))) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && (active === last || !panel.contains(active))) {
      event.preventDefault();
      first.focus();
    }
  };
  // Focus that lands outside the panel (the scrim blocks clicks on the page
  // behind, but programmatic focus or assistive tech can still move it) is
  // pulled back in.
  const onFocusIn = (event: FocusEvent): void => {
    if (panel === undefined || !(event.target instanceof Node)) return;
    if (!panel.contains(event.target)) (focusables()[0] ?? heading)?.focus();
  };
  document.addEventListener('keydown', onKeyDown);
  document.addEventListener('focusin', onFocusIn);
  const savedOverflow = document.body.style.overflow;
  document.body.style.overflow = 'hidden';
  onCleanup(() => {
    document.removeEventListener('keydown', onKeyDown);
    document.removeEventListener('focusin', onFocusIn);
    document.body.style.overflow = savedOverflow;
    if (opener?.isConnected === true) opener.focus();
  });
  onMount(() => heading?.focus());

  const problem = (key: EditorFieldKey): string | null => problems()[key] ?? null;
  const flowLabel = (key: string): string =>
    props.flows.find((flow) => flow.key === key)?.label ?? key;
  const previewText = (): { text: string; invalid: boolean } | null => {
    if (cadence() === '') return null;
    // The server could not answer: no firings to show, and no claim about them.
    if (preview.error !== undefined) {
      return { text: `Preview unavailable: ${errorMessage(preview.error)}`, invalid: false };
    }
    const fired = preview();
    if (fired === undefined) return null;
    return fired.valid
      ? { text: `Next runs: ${(fired.next_display ?? []).join(', ')}`, invalid: false }
      : { text: fired.error ?? 'This cadence never fires.', invalid: true };
  };

  return (
    <>
      <Show when={desktop()}>
        <div class="schedule-editor-scrim" aria-hidden="true" onClick={requestClose} />
      </Show>
      <section
        ref={panel}
        class="schedule-editor"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-busy={busy() !== null ? 'true' : undefined}
      >
        <header class="schedule-editor-head">
          <button
            type="button"
            class="icon-btn"
            aria-label="Back to schedules"
            onClick={requestClose}
            disabled={busy() !== null}
          >
            <Icon name="chevron-left" />
          </button>
          <h2 id={titleId} ref={heading} tabIndex={-1}>
            {seed === null ? 'New schedule' : 'Edit schedule'}
          </h2>
          <Show when={seed !== null}>
            <button
              type="button"
              class="schedule-run-now"
              disabled={dirty() || busy() !== null}
              aria-describedby={dirty() ? noteId : undefined}
              onClick={() => void runNow()}
            >
              <Icon name="play" size={16} />
              {busy() === 'run' ? 'Starting…' : 'Run now'}
            </button>
          </Show>
        </header>
        {/* The server's reason for refusing a Run now, where the button is. */}
        <div class="schedule-editor-refusal" role="alert">
          <Show when={refusal()}>{(reason) => <p>{reason()}</p>}</Show>
        </div>

        <form class="schedule-editor-form" novalidate onSubmit={(e) => void submit(e)}>
          <div class="schedule-editor-body">
            <Banner message={banner()} onDismiss={() => setBanner(null)} />
            <Show when={problem('enabled')}>
              {(message) => (
                <Banner message={message()} onDismiss={() => clearProblem('enabled')} />
              )}
            </Show>

            <div class="card settings-card">
              <EditorField field="name" id={id} label="Name" error={problem('name')}>
                {(control) => (
                  <input
                    type="text"
                    id={control.id}
                    name="schedule-name"
                    autocomplete="off"
                    placeholder="Weekly dependency check"
                    value={name()}
                    aria-invalid={control.invalid() ? 'true' : undefined}
                    aria-describedby={control.describedBy()}
                    onInput={(e) => {
                      setName(e.currentTarget.value);
                      clearProblem('name');
                    }}
                  />
                )}
              </EditorField>

              <EditorField
                field="prompt"
                id={id}
                label="Prompt"
                error={problem('prompt')}
                hint="What the run should look into. Flows add where the findings go."
              >
                {(control) => (
                  <textarea
                    id={control.id}
                    name="schedule-prompt"
                    rows="6"
                    value={prompt()}
                    placeholder="What should the run investigate?"
                    aria-invalid={control.invalid() ? 'true' : undefined}
                    aria-describedby={control.describedBy()}
                    onInput={(e) => {
                      setPrompt(e.currentTarget.value);
                      setReplaced(null);
                      clearProblem('prompt');
                    }}
                  />
                )}
              </EditorField>
              {/* The Examples pick fills the prompt above at once and falls
                  straight back to its placeholder row — it is a starter, not
                  a stored choice, so it never carries a selected value. */}
              <div class="schedule-editor-examples">
                <Select
                  skin="field"
                  label="Examples"
                  name="schedule-example"
                  value=""
                  options={PROMPT_EXAMPLES.map((example) => ({
                    value: example.key,
                    label: example.label,
                  }))}
                  inheritLabel="Start from an example…"
                  onChange={applyExample}
                />
                <div role="status" class="schedule-editor-undo">
                  <Show when={replaced() !== null}>
                    <span>Replaced your prompt with the example.</span>{' '}
                    <button type="button" class="settings-link-action" onClick={undoExample}>
                      Undo
                    </button>
                  </Show>
                </div>
              </div>

              <div
                class="sfield"
                classList={{ invalid: problem('flows') !== null }}
                id={id('field-flows')}
              >
                <div class="sfield-label">
                  <span id={id('flows-label')}>Flows</span>
                </div>
                <div
                  class="label-picker schedule-editor-flows"
                  role="group"
                  aria-labelledby={id('flows-label')}
                  aria-describedby={id('flows-hint')}
                >
                  <For each={props.flows}>
                    {(flow) => (
                      <button
                        type="button"
                        name={`flow-${flow.key}`}
                        classList={{ 'chip-toggle': true, on: selected().includes(flow.key) }}
                        aria-pressed={selected().includes(flow.key)}
                        onClick={() => toggleFlow(flow.key)}
                      >
                        {flow.label}
                      </button>
                    )}
                  </For>
                </div>
                <Show when={problem('flows')}>
                  {(message) => (
                    <p class="sfield-error" role="alert">
                      {message()}
                    </p>
                  )}
                </Show>
                <small class="sfield-hint" id={id('flows-hint')}>
                  <Show
                    when={selected().length > 0}
                    fallback="No flow: a prompt-only schedule. A flow adds where the findings go."
                  >
                    <For each={selected()}>
                      {(key) => (
                        <span class="schedule-editor-flow-desc">
                          {flowLabel(key)}:{' '}
                          {props.flows.find((flow) => flow.key === key)?.description ??
                            'not in the catalog any more.'}
                        </span>
                      )}
                    </For>
                  </Show>
                </small>
              </div>
            </div>

            <div class="card settings-card">
              <div class="settings-group" id={id('field-cadence')}>
                <h3 class="settings-sub" id={id('cadence-label')}>
                  Cadence
                </h3>
                <Segmented
                  name="cadence_mode"
                  labelledBy={id('cadence-label')}
                  value={mode()}
                  fill
                  options={[
                    { value: 'daily', label: 'Daily' },
                    { value: 'weekly', label: 'Weekly' },
                    { value: 'monthly', label: 'Monthly' },
                    { value: 'advanced', label: 'Cron' },
                  ]}
                  onChange={(value) => {
                    setMode(value as CadenceMode);
                    clearProblem('weekdays');
                    clearProblem('cron');
                    clearProblem('time');
                    clearProblem('cadence');
                  }}
                />
                <Show when={mode() === 'weekly'}>
                  <div
                    class="sfield"
                    classList={{ invalid: problem('weekdays') !== null }}
                    id={id('field-weekdays')}
                  >
                    <div class="sfield-label">
                      <span id={id('weekdays-label')}>Weekdays</span>
                    </div>
                    <div
                      class="label-picker schedule-editor-weekdays"
                      role="group"
                      aria-labelledby={id('weekdays-label')}
                    >
                      <For each={WEEKDAYS}>
                        {(day) => (
                          <button
                            type="button"
                            name={`weekday-${day.value}`}
                            classList={{ 'chip-toggle': true, on: weekdays().includes(day.value) }}
                            aria-pressed={weekdays().includes(day.value)}
                            onClick={() => toggleWeekday(day.value)}
                          >
                            {day.label}
                          </button>
                        )}
                      </For>
                    </div>
                    <Show when={problem('weekdays')}>
                      {(message) => (
                        <p class="sfield-error" role="alert">
                          {message()}
                        </p>
                      )}
                    </Show>
                  </div>
                </Show>
                <Show when={mode() === 'monthly'}>
                  <EditorField
                    field="day"
                    id={id}
                    label="Day of month"
                    error={problem('day')}
                    hint="Days 29–31 skip the months that are too short — write those as a cron expression."
                  >
                    {(control) => (
                      <select
                        id={control.id}
                        name="cadence_day"
                        value={String(monthDay())}
                        aria-describedby={control.describedBy()}
                        onChange={(e) => setMonthDay(Number(e.currentTarget.value))}
                      >
                        <For each={Array.from({ length: MONTH_DAY_MAX }, (_, i) => i + 1)}>
                          {(day) => <option value={String(day)}>{day}</option>}
                        </For>
                      </select>
                    )}
                  </EditorField>
                </Show>
                <Show when={mode() !== 'advanced'}>
                  <EditorField
                    field="time"
                    id={id}
                    label="Time, server-local"
                    error={problem('time')}
                  >
                    {(control) => (
                      <input
                        type="time"
                        id={control.id}
                        name="cadence_time"
                        value={time()}
                        aria-invalid={control.invalid() ? 'true' : undefined}
                        aria-describedby={control.describedBy()}
                        onInput={(e) => {
                          setTime(e.currentTarget.value);
                          clearProblem('time');
                        }}
                      />
                    )}
                  </EditorField>
                </Show>
                <Show when={mode() === 'advanced'}>
                  <EditorField
                    field="cron"
                    id={id}
                    label="Cron expression"
                    error={problem('cron')}
                    hint="Five fields, minute granularity: minute hour day-of-month month day-of-week."
                  >
                    {(control) => (
                      <input
                        type="text"
                        id={control.id}
                        name="cadence_expr"
                        class="mono"
                        autocomplete="off"
                        spellcheck={false}
                        placeholder="30 6 * * 1"
                        value={rawCron()}
                        aria-invalid={control.invalid() ? 'true' : undefined}
                        aria-describedby={control.describedBy()}
                        onInput={(e) => {
                          setRawCron(e.currentTarget.value);
                          clearProblem('cron');
                          clearProblem('cadence');
                        }}
                      />
                    )}
                  </EditorField>
                </Show>
                {/* Every mode: the server's own answer to "when does this
                    actually fire", so a preset and a hand-written expression
                    are checked by exactly the parser the engine uses. */}
                <Show when={problem('cadence')}>
                  {(message) => (
                    <p class="sfield-error" role="alert">
                      {message()}
                    </p>
                  )}
                </Show>
                <Show when={previewText()}>
                  {(fired) => (
                    <p classList={{ 'cadence-preview': true, invalid: fired().invalid }}>
                      {fired().text}
                    </p>
                  )}
                </Show>
              </div>
            </div>

            <div class="card settings-card">
              <div class="settings-group">
                <h3 class="settings-sub">Run settings</h3>
                <EditorField
                  field="budget"
                  id={id}
                  label="Budget, minutes"
                  error={problem('budget')}
                  override={{
                    set: budget().trim() !== '',
                    defaultText: `${DEFAULT_BUDGET_MINUTES} minutes`,
                    onReset: () => {
                      setBudget('');
                      clearProblem('budget');
                    },
                  }}
                  hint="The budget clock is what ends a scheduled run; expiry counts as a success."
                >
                  {(control) => (
                    <input
                      type="number"
                      id={control.id}
                      name="schedule_budget_minutes"
                      min="1"
                      step="1"
                      autocomplete="off"
                      placeholder={String(DEFAULT_BUDGET_MINUTES)}
                      value={budget()}
                      aria-invalid={control.invalid() ? 'true' : undefined}
                      aria-describedby={control.describedBy()}
                      onInput={(e) => {
                        setBudget(e.currentTarget.value);
                        clearProblem('budget');
                      }}
                    />
                  )}
                </EditorField>
                <div class="settings-grid3">
                  <EditorField
                    field="provider"
                    id={id}
                    label="Agent"
                    labelMode="id"
                    error={problem('provider')}
                    override={{
                      set: provider() !== '',
                      defaultText: providerName(props.afkProviderId) ?? "the repo's AFK agent",
                      onReset: () => {
                        setProvider('');
                        clearProblem('provider');
                      },
                    }}
                  >
                    {(control) => (
                      <Select
                        skin="field"
                        label="Agent"
                        labelledBy={control.labelId}
                        id={control.id}
                        name="schedule_provider"
                        value={provider()}
                        options={providerOptions()}
                        inheritLabel={inheritPickLabel(providerName(props.afkProviderId))}
                        describedBy={control.describedBy()}
                        invalid={control.invalid()}
                        onChange={(value) => {
                          setProvider(value);
                          clearProblem('provider');
                        }}
                      />
                    )}
                  </EditorField>
                  <EditorField
                    field="model"
                    id={id}
                    label="Model"
                    labelMode="id"
                    error={problem('model')}
                    override={{
                      set: model() !== '',
                      defaultText: "the repo's AFK model",
                      onReset: () => {
                        setModel('');
                        clearProblem('model');
                      },
                    }}
                  >
                    {(control) => (
                      <Select
                        skin="field"
                        label="Model"
                        labelledBy={control.labelId}
                        id={control.id}
                        name="schedule_model"
                        value={model()}
                        options={effectiveProvider()?.models ?? []}
                        inheritLabel={inheritPickLabel("the repo's AFK model")}
                        describedBy={control.describedBy()}
                        invalid={control.invalid()}
                        onChange={(value) => {
                          setModel(value);
                          clearProblem('model');
                        }}
                      />
                    )}
                  </EditorField>
                  <EditorField
                    field="effort"
                    id={id}
                    label="Effort"
                    labelMode="id"
                    error={problem('effort')}
                    override={{
                      set: effort() !== '',
                      defaultText: "the repo's AFK effort",
                      onReset: () => {
                        setEffort('');
                        clearProblem('effort');
                      },
                    }}
                  >
                    {(control) => (
                      <Select
                        skin="field"
                        label="Effort"
                        labelledBy={control.labelId}
                        id={control.id}
                        name="schedule_effort"
                        value={effort()}
                        options={effectiveProvider()?.efforts ?? []}
                        inheritLabel={inheritPickLabel("the repo's AFK effort")}
                        describedBy={control.describedBy()}
                        invalid={control.invalid()}
                        onChange={(value) => {
                          setEffort(value);
                          clearProblem('effort');
                        }}
                      />
                    )}
                  </EditorField>
                </div>
                <ToggleSwitch
                  label="Enabled"
                  description="Off keeps the schedule but skips its runs."
                  name="schedule_enabled"
                  id={id('enabled')}
                  checked={enabled()}
                  onChange={(next) => {
                    setEnabled(next);
                    clearProblem('enabled');
                  }}
                />
              </div>
            </div>

            <Show when={seed}>
              {(saved) => (
                <div class="schedule-editor-delete">
                  <InlineConfirm
                    label="Delete schedule"
                    confirmLabel="Delete for good"
                    busyLabel="Deleting…"
                    prompt={`Delete "${saved().name}"?`}
                    disabled={busy() !== null}
                    onConfirm={remove}
                  />
                </div>
              )}
            </Show>
          </div>

          <footer class="schedule-editor-foot">
            <Show
              when={asking()}
              fallback={
                <>
                  <Show when={seed !== null && dirty()}>
                    <span class="schedule-editor-note" id={noteId}>
                      Run now uses the saved version.
                    </span>
                  </Show>
                  <span class="spacer" />
                  <button
                    type="button"
                    ref={cancelButton}
                    onClick={requestClose}
                    disabled={busy() !== null}
                  >
                    Cancel
                  </button>
                  <button type="submit" class="primary" disabled={busy() !== null}>
                    {busy() === 'save' ? 'Saving…' : 'Save schedule'}
                  </button>
                </>
              }
            >
              <div class="schedule-editor-ask" role="group" aria-labelledby={askId}>
                <span class="schedule-editor-note strong" id={askId}>
                  Discard your changes?
                </span>
                <span class="spacer" />
                <button type="button" ref={keepButton} onClick={keepEditing}>
                  Keep editing
                </button>
                <button type="button" class="solid-danger" onClick={discard}>
                  Discard
                </button>
              </div>
            </Show>
          </footer>
        </form>
      </section>
    </>
  );
}

/**
 * One field of the editor: the label row (with the "inherited" / "set here"
 * chip of an override), the control, the problem under it as an alert, the
 * Default line with Reset while an override is set, and the hint. The same
 * vocabulary as the page's Field (Field.tsx), without its form-store binding:
 * a Schedule's drafts live in the editor alone.
 */
function EditorField(props: {
  /** The field; its control id is `id(key)`, its wrapper `id('field-<key>')`. */
  field: EditorFieldKey;
  /** The editor's id maker. */
  id: (key: string) => string;
  label: string;
  /** 'for' (default): a <label for>; 'id': plain text the control points at. */
  labelMode?: 'for' | 'id';
  error?: string | null;
  hint?: JSX.Element;
  /** An override: whether it is set here, what it would inherit, and the reset. */
  override?: { set: boolean; defaultText: string; onReset: () => void };
  children: (control: {
    id: string;
    labelId: string;
    describedBy: () => string | undefined;
    invalid: () => boolean;
  }) => JSX.Element;
}) {
  // One field per EditorField and one id maker per editor, so both are read
  // once.
  /* eslint-disable solid/reactivity -- the field an EditorField shows never changes */
  const field = props.field;
  const controlId = props.id(field);
  const wrapperId = props.id(`field-${field}`);
  /* eslint-enable solid/reactivity */
  const labelId = `${controlId}-label`;
  const errorId = `${controlId}-error`;
  const hintId = `${controlId}-hint`;
  const stateId = `${controlId}-state`;
  const invalid = (): boolean => props.error !== null && props.error !== undefined;
  const setHere = (): boolean => props.override?.set === true;
  const describedBy = (): string | undefined => {
    const ids = [
      props.override !== undefined ? stateId : null,
      invalid() ? errorId : null,
      props.hint !== undefined ? hintId : null,
    ].filter(Boolean);
    return ids.length > 0 ? ids.join(' ') : undefined;
  };
  const reset = (): void => {
    props.override?.onReset();
    // Reset is gone with the click: hand the focus to the control.
    queueMicrotask(() => document.getElementById(controlId)?.focus());
  };

  return (
    <div classList={{ sfield: true, invalid: invalid() }} id={wrapperId}>
      <div class="sfield-label">
        <Show
          when={props.labelMode === 'id'}
          fallback={
            <label id={labelId} for={controlId}>
              {props.label}
            </label>
          }
        >
          <span id={labelId}>{props.label}</span>
        </Show>
        <Show when={props.override}>
          <span classList={{ 'sfield-state': true, set: setHere() }} id={stateId}>
            {setHere() ? 'set here' : 'inherited'}
          </span>
        </Show>
      </div>
      {props.children({ id: controlId, labelId, describedBy, invalid })}
      <Show when={props.error}>
        {(message) => (
          <p class="sfield-error" id={errorId} role="alert">
            {message()}
          </p>
        )}
      </Show>
      <Show when={setHere() ? props.override : undefined}>
        {(override) => (
          <div class="sfield-default">
            <small>Default: {override().defaultText}</small>
            <button
              type="button"
              class="settings-link-action"
              aria-label={`Reset ${props.label} to inherited`}
              onClick={reset}
            >
              Reset
            </button>
          </div>
        )}
      </Show>
      <Show when={props.hint}>
        <small class="sfield-hint" id={hintId}>
          {props.hint}
        </small>
      </Show>
    </div>
  );
}
