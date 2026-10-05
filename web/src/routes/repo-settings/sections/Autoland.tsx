// Autoland section (issues #181, #189, #61 / ADR-0048): the per-repo
// pipeline that validates the PRs AFK runs open — as a thin renderer over the
// form store. Every control edits a draft; the page's save bar sends the
// changed ones (form.tsx, fields.ts).
//
// Autoland is forge-only: the poller reads PR comments for lander verdicts,
// and the builtin tracker binding has none to read. The switch therefore
// follows the tracker binding DRAFT — flipping the binding in Integrations
// disables or enables it here at once, before anything is saved. The lander's
// model/effort catalogs follow its effective provider the same way: its own
// drafted agent, else this repo's drafted provider chain.

import { Show } from 'solid-js';
import type { SelectOption } from '../../../components/Select';
import { FieldGroup, SelectField, SwitchField, TextField } from '../Field';
import { useRepoSettingsForm } from '../form';

export default function AutolandSection() {
  const form = useRepoSettingsForm();
  const catalog = form.catalog;
  const providerOptions = (): SelectOption[] =>
    catalog.providers().map((p) => ({ value: p.id, label: p.display_name }));
  const blocked = () => form.field('tracker_binding').value() !== 'forge';

  return (
    <div class="card settings-card">
      <Show when={blocked()}>
        <p class="settings-note">Autoland needs a forge tracker binding.</p>
      </Show>
      <SwitchField
        name="autoland_enabled"
        description="A lander run validates each PR an AFK run opens."
        disabled={blocked()}
      />
      <SwitchField name="auto_merge" description="Off means approve only, and a human merges." />
      <TextField
        name="max_fix_attempts"
        type="number"
        min={0}
        required
        hint="After that the PR is handed to a human."
      />
      {/* Inherit names the NEXT layer down: the lander agent falls back to
          this repo's agent, its model/effort to the global lander default
          (Settings › Agents), which itself falls through to the repo's and
          then the global spawn default. */}
      <FieldGroup title="Lander">
        <div class="settings-grid3">
          <SelectField
            name="lander_provider"
            options={providerOptions()}
            inheritLabel="Inherit repo agent"
          />
          <SelectField
            name="lander_model"
            options={catalog.landerProvider()?.models ?? []}
            inheritLabel="Inherit global lander default"
          />
          <SelectField
            name="lander_effort"
            options={catalog.landerProvider()?.efforts ?? []}
            inheritLabel="Inherit global lander default"
          />
        </div>
      </FieldGroup>
    </div>
  );
}
