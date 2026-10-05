// Autoland section (issues #181, #189, #61 / ADR-0048): the per-repo
// pipeline that validates the PRs AFK runs open — as a thin renderer over the
// form store. Every control edits a draft; the page's save bar sends the
// changed ones (form.tsx, fields.ts).
//
// Only what applies (issue #61 §7), read from the DRAFTS so the section
// follows an edit before anything is saved:
//
//   - Autoland is forge-only: the poller reads PR comments for lander
//     verdicts, and the builtin tracker binding has none to read. With that
//     binding the section says why it is unavailable, links to the tracker
//     binding field, and disables the switch (the reason is the switch's
//     description, so assistive tech hears it too) — but only a switch that
//     is OFF: an Autoland that is on can always be turned off, which is the
//     one way out when the binding is being changed to builtin. Save checks
//     that pair in the browser (fields.ts) and sends nothing while it holds.
//   - With Autoland off, the merge policy, the fix-attempt bound and the
//     lander's agent, model and effort are replaced by a note.
//
// A folded field is never out of reach: it shows while it has a pending
// change or a problem, and when a link or a Save points at it. Folding only
// hides — it never clears or changes a value.
//
// The lander's three picks are overridable (Field.tsx); its model and effort
// catalogs belong to its effective provider — its own drafted agent, else
// the inherited one.

import { Show, createComputed, createSignal } from 'solid-js';
import type { SelectOption } from '../../../components/Select';
import { FieldGroup, SelectField, SwitchField, TextField } from '../Field';
import type { RepoFieldKey } from '../fields';
import { useRepoSettingsForm } from '../form';
import { useRepoHome } from '../../repo-home/context';

/** What only an Autoland that is on reads. */
const AUTOLAND_OPTIONS = [
  'auto_merge',
  'max_fix_attempts',
  'lander_provider',
  'lander_model',
  'lander_effort',
] as const satisfies readonly RepoFieldKey[];

export default function AutolandSection() {
  const form = useRepoSettingsForm();
  const home = useRepoHome();
  const catalog = form.catalog;
  const providerOptions = (): SelectOption[] =>
    catalog.providers().map((p) => ({ value: p.id, label: p.display_name }));

  const builtin = (): boolean => form.field('tracker_binding').value() !== 'forge';
  const enabled = (): boolean => form.field('autoland_enabled').value();
  const on = (): boolean => enabled() && !builtin();
  const switchNote = (): string => {
    if (!builtin()) return 'A lander run validates each PR an AFK run opens.';
    return enabled()
      ? 'Autoland needs a forge tracker binding. Turn it off to use the built-in one.'
      : 'Not available: Autoland needs a forge tracker binding.';
  };
  // An option something pointed at stays shown from then on.
  const [pointed, setPointed] = createSignal(false);
  createComputed(() => {
    const field = form.pointedAt();
    if (field !== undefined && (AUTOLAND_OPTIONS as readonly string[]).includes(field)) {
      setPointed(true);
    }
  });
  const inUse = (): boolean =>
    AUTOLAND_OPTIONS.some((key) => form.field(key).changed() || form.field(key).error() !== null);
  const optionsShown = (): boolean => on() || pointed() || inUse();

  const openBinding = (event: MouseEvent): void => {
    if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return;
    }
    event.preventDefault();
    form.reveal({ section: 'integrations', field: 'tracker_binding' });
  };

  return (
    <div class="card settings-card">
      <Show when={builtin()}>
        <p class="settings-na">
          Autoland needs a forge tracker binding, and this repository uses the built-in one.{' '}
          <a
            href={`/repos/${home.id()}/settings/integrations?field=tracker_binding`}
            class="settings-link"
            on:click={openBinding}
          >
            Change in Integrations
          </a>
        </p>
      </Show>
      <SwitchField
        name="autoland_enabled"
        description={switchNote()}
        // Never while it is on: turning it off must stay possible.
        disabled={builtin() && !enabled()}
      />
      <Show
        when={optionsShown()}
        fallback={
          <Show when={!builtin()}>
            <p class="settings-na">Merge policy and lander options appear when Autoland is on.</p>
          </Show>
        }
      >
        <SwitchField name="auto_merge" description="Off means approve only, and a human merges." />
        <TextField
          name="max_fix_attempts"
          type="number"
          required
          hint="After that the PR is handed to a human."
        />
        <FieldGroup title="Lander">
          <div class="settings-grid3">
            <SelectField name="lander_provider" options={providerOptions()} />
            <SelectField name="lander_model" options={catalog.landerProvider()?.models ?? []} />
            <SelectField name="lander_effort" options={catalog.landerProvider()?.efforts ?? []} />
          </div>
        </FieldGroup>
      </Show>
    </div>
  );
}
