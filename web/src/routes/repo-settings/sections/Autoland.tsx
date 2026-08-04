// Autoland section (issue #181 / ADR-0048, split out per issue #198): the
// monolith's Autoland card as one per-section form — drafts seeded via
// createSeededDrafts, saved as a dirty-fields-only PATCH through
// useSettingsForm.

import { Show } from 'solid-js';
import type { Accessor } from 'solid-js';
import { updateRepo, type Provider, type Repo, type RepoPatch, type Settings } from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';
import SectionCard from '../../../components/SectionCard';
import Select, { type SelectOption } from '../../../components/Select';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import { createSeededDrafts } from '../../../lib/seededDrafts';
import { providerFor } from '../../../lib/spawn';
import { normText } from '../shared';

export default function AutolandSection(props: {
  repo: Accessor<Repo>;
  providers: Provider[];
  settings: Settings;
  onSaved: () => void;
}) {
  // Autoland (issue #181 / ADR-0048), per-repo and default off. maxFixAttempts
  // is a plain (non-nullable) integer draft, same shape as the Agents
  // section's budget/maxInstances but without the blank-means-inherit escape
  // hatch. landerProvider rides a Select like the other provider knobs:
  // '' ↔ null (inherit THIS repo's own provider chain, not a global).
  const drafts = createSeededDrafts(() => props.repo());
  const [autolandEnabled, setAutolandEnabled] = drafts.field((r) => r.autoland_enabled);
  const [autoMerge, setAutoMerge] = drafts.field((r) => r.auto_merge);
  const [maxFixAttempts, setMaxFixAttempts] = drafts.field((r) => String(r.max_fix_attempts));
  const [landerProvider, setLanderProvider] = drafts.field((r) => r.lander_provider ?? '');
  // Lander model/effort overrides (issue #189): '' ↔ null (inherit), same shape
  // as the base/AFK model+effort selects. Unlike lander_provider these carry no
  // registry check — any non-empty string is accepted; the lander launch, where
  // the effective provider is known, is what enforces the catalog.
  const [landerModel, setLanderModel] = drafts.field((r) => r.lander_model ?? '');
  const [landerEffort, setLanderEffort] = drafts.field((r) => r.lander_effort ?? '');

  const providerOptions = (): SelectOption[] =>
    props.providers.map((p) => ({ value: p.id, label: p.display_name }));
  // The lander's effective provider (issue #189): lander_provider if set, else
  // this repo's own provider chain — mirroring how a NULL lander_provider
  // inherits the repo's Provider at launch. Resolved LIVE against the lander
  // draft so the lander model/effort catalogs below re-catalog as the operator
  // flips the lander agent. The repo-provider layer reads the SAVED
  // props.repo().provider — its draft lives in the Agents section now and
  // cross-section drafts don't exist (issue #198), so the saved value is the
  // truth here.
  const landerEffectiveProvider = () =>
    providerFor(
      props.providers,
      landerProvider(),
      props.repo().provider,
      props.settings.provider_default,
    );

  // Autoland is forge-only (ADR-0048): the poller reads PR comments for
  // lander verdicts, and the builtin binding has no comment listing to read.
  // The monolith resolved this against the tracker-binding DRAFT; that draft
  // now lives in the Integrations section and cross-section drafts don't
  // exist (issue #198) — so this deliberately resolves against the SAVED
  // binding, the truth for everything outside Integrations' own edit session.
  const autolandBlocked = () => props.repo().tracker_binding !== 'forge';

  const buildPatch = (): RepoPatch | string => {
    // Diff against the seed the drafts came from — NOT the live props.repo().
    // Diffing against the live repo would mark a stale draft of a field the
    // operator never touched as "dirty" and PATCH the old value back.
    const current = drafts.seed();
    const patch: RepoPatch = {};

    if (autolandEnabled() !== current.autoland_enabled) {
      patch.autoland_enabled = autolandEnabled();
    }
    if (autoMerge() !== current.auto_merge) patch.auto_merge = autoMerge();
    const attempts = maxFixAttempts().trim();
    const attemptsNum = Number(attempts);
    if (attempts === '' || !Number.isInteger(attemptsNum) || attemptsNum < 0) {
      return 'Max fix attempts must be a whole number of 0 or more.';
    }
    if (attemptsNum !== current.max_fix_attempts) patch.max_fix_attempts = attemptsNum;
    if (normText(landerProvider()) !== current.lander_provider) {
      patch.lander_provider = normText(landerProvider());
    }
    if (normText(landerModel()) !== current.lander_model) {
      patch.lander_model = normText(landerModel());
    }
    if (normText(landerEffort()) !== current.lander_effort) {
      patch.lander_effort = normText(landerEffort());
    }

    return patch;
  };

  const dirty = () => {
    const p = buildPatch();
    return typeof p === 'string' || Object.keys(p).length > 0;
  };

  const form = useSettingsForm<RepoPatch>({
    dirty,
    buildPatch,
    submit: (patch) => updateRepo(props.repo().id, patch),
    onSaved: () => props.onSaved(),
  });

  return (
    <form onSubmit={(e) => void form.save(e)} class="stack">
      <ErrorBanner message={form.error()} onDismiss={() => form.setError(null)} />
      <Show when={form.note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{form.note()}</span>
        </div>
      </Show>

      <SectionCard title="Autoland">
        <label class="check">
          <input
            type="checkbox"
            name="autoland_enabled"
            checked={autolandEnabled()}
            disabled={autolandBlocked()}
            onChange={(e) => setAutolandEnabled(e.currentTarget.checked)}
          />
          <span>Autoland claim PRs (spawn a lander to validate; merge on clean PASS)</span>
        </label>
        <Show when={autolandBlocked()}>
          <small class="hint hint-block">Autoland needs a forge tracker binding.</small>
        </Show>
        <label class="check">
          <input
            type="checkbox"
            name="auto_merge"
            checked={autoMerge()}
            onChange={(e) => setAutoMerge(e.currentTarget.checked)}
          />
          <span>Merge on clean PASS (off: approve only, human merges)</span>
        </label>
        <label class="field">
          <span>Max fix attempts</span>
          <input
            type="number"
            name="max_fix_attempts"
            min="0"
            step="1"
            autocomplete="off"
            value={maxFixAttempts()}
            onInput={(e) => setMaxFixAttempts(e.currentTarget.value)}
          />
        </label>
        <Select
          skin="field"
          label="Lander agent"
          name="lander_provider"
          value={landerProvider()}
          options={providerOptions()}
          inheritLabel="Inherit repo agent"
          onChange={setLanderProvider}
        />
        {/* Model/effort for the lander (issue #189): same component + catalog
            source as the base/AFK pickers, resolved against the lander's
            effective provider (lander agent above, else this repo's chain). */}
        <Select
          skin="field"
          label="Model"
          name="lander_model"
          value={landerModel()}
          options={landerEffectiveProvider()?.models ?? []}
          inheritLabel="Inherit repo default"
          onChange={setLanderModel}
        />
        <Select
          skin="field"
          label="Effort"
          name="lander_effort"
          value={landerEffort()}
          options={landerEffectiveProvider()?.efforts ?? []}
          inheritLabel="Inherit repo default"
          onChange={setLanderEffort}
        />
      </SectionCard>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save changes'}
      </button>
    </form>
  );
}
