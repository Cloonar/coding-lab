// Agents section (issue #61): the defaults of the runs this repo starts, as a
// thin renderer over the form store — three groups in one card:
//
//   Runs you start   agent, model, effort, remote control
//   AFK runs         the same four as AFK overrides, the provider's option
//                    bag, and the seed prompt with its "Customize" action and
//                    the done-signal hint
//   AFK capacity     auto-spawn, the budget clock, the instance cap
//
// Nothing here saves: every control edits a draft in the form store, and the
// page's save bar sends the changed ones in one PATCH (form.tsx, fields.ts).
// The model/effort catalogs follow the DRAFTED agents live (skip-layer,
// ADR-0030); a stored value foreign to the new catalog stays selectable,
// marked "(not in catalog)" — nothing auto-clears.

import { For, Show } from 'solid-js';
import type { SelectOption } from '../../../components/Select';
import { resolveRemote } from '../../../lib/spawn';
import { Field, FieldGroup, SelectField, SwitchField, TextField } from '../Field';
import { useRepoSettingsForm } from '../form';
import { REMOTE_OPTIONS, normBool, onOff, remoteBlocker } from '../shared';

export default function AgentsSection() {
  const form = useRepoSettingsForm();
  const catalog = form.catalog;
  const providerOptions = (): SelectOption[] =>
    catalog.providers().map((p) => ({ value: p.id, label: p.display_name }));

  // What "inherit" currently MEANS for each remote-control pick (issue #163),
  // resolved live against the drafts and the global settings:
  //   manual: repo.remote_default → spawn_remote_default → false
  //   AFK:    repo.afk_remote_default → spawn_remote_default_afk
  //             → repo.remote_default → spawn_remote_default → false
  const remote = form.field('remote_default');
  const inheritedRemote = () => resolveRemote(catalog.settings()?.spawn_remote_default);
  const inheritedAfkRemote = () =>
    resolveRemote(
      catalog.settings()?.spawn_remote_default_afk,
      normBool(remote.value()),
      catalog.settings()?.spawn_remote_default,
    );
  // Remote control is a provider capability: a provider without the knob
  // ignores the field, which then renders disabled and says so by name.
  const baseBlocker = () => remoteBlocker(catalog.baseProvider());
  const afkBlocker = () => remoteBlocker(catalog.afkProvider());

  return (
    <div class="card settings-card">
      <FieldGroup title="Runs you start">
        <div class="settings-grid3">
          <SelectField
            name="provider"
            options={providerOptions()}
            inheritLabel="Inherit global default"
          />
          <SelectField
            name="model_default"
            options={catalog.baseProvider()?.models ?? []}
            inheritLabel="Inherit global default"
          />
          <SelectField
            name="effort_default"
            options={catalog.baseProvider()?.efforts ?? []}
            inheritLabel="Inherit global default"
          />
        </div>
        <SelectField
          name="remote_default"
          options={REMOTE_OPTIONS}
          inheritLabel={`Inherit global default — currently ${onOff(inheritedRemote())}`}
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
      </FieldGroup>

      <FieldGroup title="AFK runs">
        <div class="settings-grid3">
          <SelectField
            name="afk_provider_default"
            options={providerOptions()}
            inheritLabel="Inherit global AFK default"
          />
          <SelectField
            name="afk_model_default"
            options={catalog.afkProvider()?.models ?? []}
            inheritLabel="Inherit global AFK default"
          />
          <SelectField
            name="afk_effort_default"
            options={catalog.afkProvider()?.efforts ?? []}
            inheritLabel="Inherit global AFK default"
          />
        </div>
        {/* Inherit here walks the AFK chain — the global AFK override, then
            this repo's own pick above, then the global base. */}
        <SelectField
          name="afk_remote_default"
          options={REMOTE_OPTIONS}
          inheritLabel={`Inherit global AFK default — currently ${onOff(inheritedAfkRemote())}`}
          disabled={afkBlocker() !== null}
          hint={<Show when={afkBlocker()}>{(name) => <>{name()} ignores this.</>}</Show>}
        />
        {/* The provider's bool spawn options (issue #19). A null bag seeds
            all-unchecked; once one differs, the whole declared bag is saved
            as this repo's override (fields.ts). */}
        <Show when={catalog.afkBoolOptions().length > 0}>
          <Field
            name="afk_options"
            labelMode="id"
            hint="A repo option bag overrides the global AFK options."
          >
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
                        name={`afk_options.${option.key}`}
                        checked={control.binding.value()[option.key] ?? false}
                        onChange={(event) =>
                          control.binding.set({
                            ...control.binding.value(),
                            [option.key]: event.currentTarget.checked,
                          })
                        }
                      />
                      <span>{option.label}</span>
                    </label>
                  )}
                </For>
              </div>
            )}
          </Field>
        </Show>
        <Field
          name="afk_prompt"
          hint="The run is detected as done only by an open PR on its branch — a prompt that never opens a PR burns its budget, counts as a failure, and three failures auto-pause the repo's AFK."
        >
          {(control) => (
            <>
              <textarea
                id={control.id}
                name="afk_prompt"
                rows="6"
                aria-describedby={control.describedBy()}
                aria-invalid={control.invalid() ? 'true' : undefined}
                value={control.binding.value()}
                placeholder={form.saved()?.afk_prompt_effective}
                onInput={(event) => control.binding.set(event.currentTarget.value)}
              />
              {/* Blank inherits the effective prompt (the placeholder);
                  Customize copies it in as a starting point. */}
              <Show when={control.binding.value() === ''}>
                <button
                  type="button"
                  class="settings-inline-action"
                  onClick={() => control.binding.set(form.saved()?.afk_prompt_effective ?? '')}
                >
                  Customize
                </button>
              </Show>
            </>
          )}
        </Field>
      </FieldGroup>

      <FieldGroup title="AFK capacity">
        <SwitchField
          name="afk_auto_enabled"
          description="Claim ready-for-agent issues as they appear."
        />
        <div class="settings-grid2">
          <TextField name="budget_minutes" type="number" min={1} placeholder="global default" />
          <TextField
            name="max_instances_override"
            type="number"
            min={1}
            placeholder="global default"
          />
        </div>
      </FieldGroup>
    </div>
  );
}
