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
//
// Every field but Auto-spawn is overridable: it says "inherited" or "set
// here" at its label and names what it inherits (Field.tsx). The model and
// effort catalogs belong to the run class's EFFECTIVE provider — the drafted
// agent when one is set here, else the inherited one — so they re-catalog as
// the operator flips an agent, before anything is saved; a stored value
// foreign to the new catalog stays selectable, marked "(not in catalog)".
// Remote control is a three-way pick over a tri-state field: inherited, on,
// off — and off is a value, saved as false.
//
// Nothing is guessed. While the repo has no option bag of its own and the one
// it inherits is not known (still loading, never answered), the boxes show no
// state and cannot be toggled — a first toggle would pin the whole bag from
// values nobody knows. And the seed prompt's inherited template is the one
// the server computed for the SAVED repo: it depends on Incogni, so with an
// unsaved Incogni change the page says the template will follow once saved.

import { For, Show, createEffect, createUniqueId } from 'solid-js';
import type { SelectOption } from '../../../components/Select';
import { Field, FieldGroup, SegmentedField, SelectField, SwitchField, TextField } from '../Field';
import { useRepoSettingsForm } from '../form';
import { remoteBlocker, toBoolMap } from '../shared';

/** The explicit picks of remote control; the inherit segment comes first. */
const REMOTE_PICKS = [
  { value: 'true', label: 'On' },
  { value: 'false', label: 'Off' },
];

export default function AgentsSection() {
  const form = useRepoSettingsForm();
  const catalog = form.catalog;
  const providerOptions = (): SelectOption[] =>
    catalog.providers().map((p) => ({ value: p.id, label: p.display_name }));

  // Remote control is a provider capability: a provider without the knob
  // ignores the field, which then renders disabled and says so by name.
  const baseBlocker = () => remoteBlocker(catalog.baseProvider());
  const afkBlocker = () => remoteBlocker(catalog.afkProvider());

  // The inherited seed prompt is incogni-aware, and only the server composes
  // it — for the saved repo. An unsaved Incogni change is said, not guessed.
  const incogniPending = (): boolean => form.field('incogni').changed();
  const templateNoteId = `rs-afk_prompt-template-${createUniqueId()}`;

  // The option bag (issue #19). While the repo has no bag of its own the
  // boxes show the bag it inherits; the first toggle gives it one — the full
  // declared bag — and Reset returns it to inherited.
  const bag = form.field('afk_options');
  const inheritedBag = (): Record<string, boolean> =>
    toBoolMap(form.inherited()?.afk_options ?? null);
  const shownBag = (): Record<string, boolean> => bag.value() ?? inheritedBag();
  // Known: the repo's own bag, or the server's answer about the inherited one.
  const bagKnown = (): boolean => bag.value() !== null || form.inherited() !== undefined;
  const toggleOption = (key: string, checked: boolean): void => {
    if (!bagKnown()) return;
    const declared = catalog.afkBoolOptions();
    const next = Object.fromEntries(
      declared.map((option) => [
        option.key,
        option.key === key ? checked : (shownBag()[option.key] ?? false),
      ]),
    );
    // Toggled back to exactly what an inheriting repo inherits: that is no
    // bag of its own, and nothing to save.
    const inheritsAgain =
      form.saved()?.afk_options === null &&
      declared.every((option) => next[option.key] === (inheritedBag()[option.key] ?? false));
    bag.set(inheritsAgain ? null : next);
  };

  return (
    <div class="card settings-card">
      <FieldGroup title="Runs you start">
        <div class="settings-grid3">
          <SelectField name="provider" options={providerOptions()} />
          <SelectField name="model_default" options={catalog.baseProvider()?.models ?? []} />
          <SelectField name="effort_default" options={catalog.baseProvider()?.efforts ?? []} />
        </div>
        <SegmentedField
          name="remote_default"
          options={REMOTE_PICKS}
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
          <SelectField name="afk_provider_default" options={providerOptions()} />
          <SelectField name="afk_model_default" options={catalog.afkProvider()?.models ?? []} />
          <SelectField name="afk_effort_default" options={catalog.afkProvider()?.efforts ?? []} />
        </div>
        <SegmentedField
          name="afk_remote_default"
          options={REMOTE_PICKS}
          disabled={afkBlocker() !== null}
          hint={<Show when={afkBlocker()}>{(name) => <>{name()} ignores this.</>}</Show>}
        />
        <Show when={catalog.afkBoolOptions().length > 0}>
          <Field
            name="afk_options"
            labelMode="id"
            hint={
              bagKnown()
                ? undefined
                : 'The inherited options are not known yet, so they cannot be changed here.'
            }
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
                        checked={bagKnown() && (shownBag()[option.key] ?? false)}
                        // Neither on nor off while it is not known.
                        ref={(el) =>
                          createEffect(() => {
                            el.indeterminate = !bagKnown();
                          })
                        }
                        disabled={!bagKnown()}
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
                aria-describedby={
                  incogniPending()
                    ? `${control.describedBy() ?? ''} ${templateNoteId}`.trim()
                    : control.describedBy()
                }
                aria-invalid={control.invalid() ? 'true' : undefined}
                value={control.binding.value()}
                // Blank inherits: the placeholder is the prompt that then runs.
                placeholder={form.saved()?.afk_prompt_effective}
                onInput={(event) => control.binding.set(event.currentTarget.value)}
              />
              {/* Customize copies the inherited prompt in as a starting point. */}
              <Show when={control.binding.value() === ''}>
                <button
                  type="button"
                  class="settings-inline-action"
                  onClick={() => control.binding.set(form.saved()?.afk_prompt_effective ?? '')}
                >
                  Customize
                </button>
              </Show>
              <Show when={incogniPending()}>
                <p class="settings-note" id={templateNoteId}>
                  The inherited template follows Incogni once saved. The one shown is for the saved
                  setting.
                </p>
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
          <TextField name="budget_minutes" type="number" />
          <TextField name="max_instances_override" type="number" />
        </div>
      </FieldGroup>
    </div>
  );
}
