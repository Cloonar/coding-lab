// Branches section (issue #198): the monolith's Branches card as one
// per-section form — drafts seeded via createSeededDrafts, saved as a
// dirty-fields-only PATCH through useSettingsForm.

import { Show } from 'solid-js';
import type { Accessor } from 'solid-js';
import { updateRepo, type Repo, type RepoPatch } from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';
import SectionCard from '../../../components/SectionCard';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import { createSeededDrafts } from '../../../lib/seededDrafts';

export default function BranchesSection(props: { repo: Accessor<Repo>; onSaved: () => void }) {
  const drafts = createSeededDrafts(() => props.repo());
  const [defaultBranch, setDefaultBranch] = drafts.field((r) => r.default_branch);
  const [afkPattern, setAfkPattern] = drafts.field((r) => r.afk_branch_pattern);
  const [manualPrefix, setManualPrefix] = drafts.field((r) => r.manual_branch_prefix);

  const buildPatch = (): RepoPatch | string => {
    // Diff against the seed the drafts came from — NOT the live props.repo().
    // Diffing against the live repo would mark a stale draft of a field the
    // operator never touched as "dirty" and PATCH the old value back.
    const current = drafts.seed();
    const patch: RepoPatch = {};

    const branch = defaultBranch().trim();
    if (branch !== current.default_branch) patch.default_branch = branch;
    const pattern = afkPattern().trim();
    if (pattern !== current.afk_branch_pattern) patch.afk_branch_pattern = pattern;
    const prefix = manualPrefix().trim();
    if (prefix !== current.manual_branch_prefix) patch.manual_branch_prefix = prefix;

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

      <SectionCard title="Branches">
        <label class="field">
          <span>Default branch</span>
          <input
            type="text"
            name="default_branch"
            required
            autocomplete="off"
            spellcheck={false}
            value={defaultBranch()}
            onInput={(e) => setDefaultBranch(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>AFK branch pattern</span>
          <input
            type="text"
            name="afk_branch_pattern"
            required
            autocomplete="off"
            spellcheck={false}
            class="mono"
            value={afkPattern()}
            onInput={(e) => setAfkPattern(e.currentTarget.value)}
          />
          <small class="hint">
            Must contain &lt;N&gt; exactly once (the issue number), e.g. afk/&lt;N&gt; or
            issue-&lt;N&gt;. Letters, digits, . _ / - only; may not overlap the manual prefix.
          </small>
        </label>
        <label class="field">
          <span>Manual branch prefix</span>
          <input
            type="text"
            name="manual_branch_prefix"
            required
            autocomplete="off"
            spellcheck={false}
            class="mono"
            value={manualPrefix()}
            onInput={(e) => setManualPrefix(e.currentTarget.value)}
          />
          <small class="hint">
            Literal prefix for manual instance branches, e.g. lab/ or wip/.
          </small>
        </label>
      </SectionCard>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save changes'}
      </button>
    </form>
  );
}
