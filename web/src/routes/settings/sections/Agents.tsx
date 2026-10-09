// Global settings › Agents (issues #198, #85): the global defaults of every
// run, as a thin renderer over the form store — four groups in one card,
// worded like the repo page's Agents section where the fields match:
//
//   Runs you start   agent, model, effort, remote control, dialog auto-dismiss
//   AFK runs         agent, model, effort and remote control as AFK
//                    overrides, the provider's option bag, and the seed
//                    prompt with its "Customize" action and done-signal hint
//   Lander           model and effort overrides for the Autoland lander
//   Capacity         max instances, the AFK budget, the two loop ticks and
//                    the sweep interval
//
// Nothing here saves: every control edits a draft in the form store, and the
// page's save bar sends the changed ones in one PATCH (form.tsx, fields.ts).
//
// The agent of runs you start is the root of every provider chain: no
// inherit entry, and an unseeded store shows the first registered provider.
// The AFK and lander fields are overrides: each says "inherited" or "set
// here" at its label and names what it inherits — the drafted field it
// stands on, live (inherited.ts) — and Reset returns it to inherited. The
// model and effort catalogs follow the DRAFTED agents, so they re-catalog as
// the operator flips an agent, before anything is saved; a stored value
// foreign to the catalog stays selectable, marked "(not in catalog)". There
// is no global lander agent: the lander's catalogs are the base agent's.
//
// Remote control is a provider capability: a provider without the knob
// ignores the field, which then renders disabled and says so by name. The
// base remote control is a plain switch (nothing above it to inherit); the
// AFK one is a three-way pick — inherited, on, off — and off is a value.

import { For, Show } from 'solid-js';
import Select, { type SelectOption } from '../../../components/Select';
import { remoteBlocker } from '../../repo-settings/shared';
import { Field, FieldGroup, SegmentedField, SelectField, SwitchField, TextField } from '../Field';
import { useGlobalSettingsForm } from '../form';

/** The explicit picks of remote control; the inherit segment comes first. */
const REMOTE_PICKS = [
  { value: 'true', label: 'On' },
  { value: 'false', label: 'Off' },
];

const DONE_SIGNAL_HINT =
  "The run is detected as done only by an open PR on its branch — a prompt that never opens a PR burns its budget, counts as a failure, and three failures auto-pause the repo's AFK.";

export default function AgentsSection() {
  const form = useGlobalSettingsForm();
  const catalog = form.catalog;
  const providerOptions = (): SelectOption[] =>
    catalog.providers().map((p) => ({ value: p.id, label: p.display_name }));

  const baseBlocker = () => remoteBlocker(catalog.baseProvider());
  const afkBlocker = () => remoteBlocker(catalog.afkProvider());

  // The option bag (issue #19): the first toggle drafts the FULL declared
  // bag; toggling back to the stored state is no change (fields.ts).
  const bag = form.field('spawn_options_afk');
  const toggleOption = (key: string, checked: boolean): void => {
    const shown = bag.value();
    bag.set(
      Object.fromEntries(
        catalog
          .afkBoolOptions()
          .map((option) => [
            option.key,
            option.key === key ? checked : (shown[option.key] ?? false),
          ]),
      ),
    );
  };

  return (
    <div class="card settings-card">
      <FieldGroup title="Runs you start">
        <div class="settings-grid3">
          {/* The root of the provider chain — nothing to inherit from, so no
              inherit entry. An unseeded store shows the provider runs fall
              back to (the first registered one). */}
          <Field name="provider_default" labelMode="id">
            {(control) => (
              <Select
                skin="field"
                label={control.binding.spec.label}
                labelledBy={control.labelId}
                id={control.id}
                name={control.binding.key}
                value={control.binding.value() || (catalog.baseProvider()?.id ?? '')}
                options={providerOptions()}
                describedBy={control.describedBy()}
                invalid={control.invalid()}
                onChange={(value) => control.binding.set(value)}
              />
            )}
          </Field>
          <SelectField name="spawn_model_default" options={catalog.baseProvider()?.models ?? []} />
          <SelectField
            name="spawn_effort_default"
            options={catalog.baseProvider()?.efforts ?? []}
          />
        </div>
        <Show when={catalog.loaded() && catalog.baseProvider() === null}>
          <p class="settings-note">
            Provider catalog unavailable — only the stored values are offered.
          </p>
        </Show>
        <SwitchField
          name="spawn_remote_default"
          disabled={baseBlocker() !== null}
          hint={
            <Show
              when={baseBlocker()}
              fallback="Registers the session with the agent's web app so it can be opened and driven from there."
            >
              {(name) => <>{name()} ignores this.</>}
            </Show>
          }
        />
        <TextField
          name="dialog_timeout_minutes"
          type="number"
          hint="0 = never. Applies to manual sessions at the next spawn; running sessions keep their spawn-time value."
        />
      </FieldGroup>

      <FieldGroup title="AFK runs">
        <p class="settings-note">
          Used for unattended AFK runs. A field left inherited follows the one for runs you start
          above.
        </p>
        <div class="settings-grid3">
          <SelectField name="spawn_provider_default_afk" options={providerOptions()} />
          <SelectField
            name="spawn_model_default_afk"
            options={catalog.afkProvider()?.models ?? []}
          />
          <SelectField
            name="spawn_effort_default_afk"
            options={catalog.afkProvider()?.efforts ?? []}
          />
        </div>
        <SegmentedField
          name="spawn_remote_default_afk"
          options={REMOTE_PICKS}
          disabled={afkBlocker() !== null}
          hint={<Show when={afkBlocker()}>{(name) => <>{name()} ignores this.</>}</Show>}
        />
        <Show when={catalog.afkBoolOptions().length > 0}>
          <Field name="spawn_options_afk" labelMode="id">
            {(control) => (
              <div
                class="settings-checks"
                role="group"
                aria-labelledby={control.labelId}
                aria-describedby={control.describedBy()}
              >
                <For each={catalog.afkBoolOptions()}>
                  {(option) => (
                    <label class="check">
                      <input
                        type="checkbox"
                        name={`spawn_options_afk.${option.key}`}
                        checked={control.binding.value()[option.key] ?? false}
                        onChange={(event) => toggleOption(option.key, event.currentTarget.checked)}
                      />
                      <span>{option.label}</span>
                    </label>
                  )}
                </For>
              </div>
            )}
          </Field>
        </Show>
        <Field name="afk_prompt" hint={DONE_SIGNAL_HINT}>
          {(control) => (
            <>
              <textarea
                id={control.id}
                name="afk_prompt"
                rows="10"
                aria-describedby={control.describedBy()}
                aria-invalid={control.invalid() ? 'true' : undefined}
                value={control.binding.value()}
                // Blank runs the built-in template: the placeholder is the
                // prompt that then runs.
                placeholder={form.saved()?.afk_prompt_default ?? ''}
                onInput={(event) => control.binding.set(event.currentTarget.value)}
              />
              {/* Customize copies the built-in prompt in as a starting point. */}
              <Show when={control.binding.value() === ''}>
                <button
                  type="button"
                  class="settings-inline-action"
                  onClick={() => control.binding.set(form.saved()?.afk_prompt_default ?? '')}
                >
                  Customize
                </button>
              </Show>
            </>
          )}
        </Field>
      </FieldGroup>

      <FieldGroup title="Lander">
        <p class="settings-note">
          Used for the Autoland lander that validates and merges AFK pull requests. A repo's own
          lander model in its Autoland settings wins over this.
        </p>
        <div class="settings-grid2">
          <SelectField
            name="spawn_model_default_lander"
            options={catalog.baseProvider()?.models ?? []}
          />
          <SelectField
            name="spawn_effort_default_lander"
            options={catalog.baseProvider()?.efforts ?? []}
          />
        </div>
      </FieldGroup>

      <FieldGroup title="Capacity">
        <div class="settings-grid2">
          <TextField
            name="max_instances"
            type="number"
            hint="Global cap on live sessions across all repos (login session exempt)."
          />
          <TextField
            name="afk_budget_minutes"
            type="number"
            hint="Wall-clock budget per AFK run before the reaper times it out."
          />
          <TextField
            name="afk_tick_seconds"
            type="number"
            hint="How often AFK runs are classified (success / death / timeout)."
          />
          <TextField
            name="afk_schedule_seconds"
            type="number"
            hint="How often auto-enabled repos are considered for a new AFK run."
          />
          <TextField
            name="sweep_interval_minutes"
            type="number"
            hint="Throttle for the merged-worktree/branch GC sweep."
          />
        </div>
      </FieldGroup>
    </div>
  );
}
