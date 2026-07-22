// General section (issue #198): the monolith's Identity and Incogni cards as
// one per-section form — drafts seeded via createSeededDrafts, saved as a
// dirty-fields-only PATCH through useSettingsForm.

import { Show } from 'solid-js';
import type { Accessor } from 'solid-js';
import { updateRepo, type Repo, type RepoPatch } from '../../../api';
import ErrorBanner from '../../../components/ErrorBanner';
import { useSettingsForm } from '../../../components/settings/useSettingsForm';
import { createSeededDrafts } from '../../../lib/seededDrafts';
import { normText } from '../shared';

export default function GeneralSection(props: { repo: Accessor<Repo>; onSaved: () => void }) {
  const drafts = createSeededDrafts(() => props.repo());
  const [name, setName] = drafts.field((r) => r.name);
  const [authorName, setAuthorName] = drafts.field((r) => r.git_author_name ?? '');
  const [authorEmail, setAuthorEmail] = drafts.field((r) => r.git_author_email ?? '');
  const [incogni, setIncogni] = drafts.field((r) => r.incogni);

  const buildPatch = (): RepoPatch | string => {
    // Diff against the seed the drafts came from — NOT the live props.repo().
    // Diffing against the live repo would mark a stale draft of a field the
    // operator never touched as "dirty" and PATCH the old value back.
    const current = drafts.seed();
    const patch: RepoPatch = {};

    const trimmedName = name().trim();
    if (trimmedName === '') return 'Name must not be empty.';
    if (trimmedName !== current.name) patch.name = trimmedName;

    if (normText(authorName()) !== current.git_author_name) {
      patch.git_author_name = normText(authorName());
    }
    if (normText(authorEmail()) !== current.git_author_email) {
      patch.git_author_email = normText(authorEmail());
    }
    if (incogni() !== current.incogni) patch.incogni = incogni();

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

      <section class="card">
        <h2>Identity</h2>
        <label class="field">
          <span>Name</span>
          <input
            type="text"
            name="name"
            required
            autocomplete="off"
            spellcheck={false}
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Git author name</span>
          <input
            type="text"
            name="git_author_name"
            autocomplete="off"
            value={authorName()}
            onInput={(e) => setAuthorName(e.currentTarget.value)}
          />
          <small class="hint">Blank → global setting.</small>
        </label>
        <label class="field">
          <span>Git author email</span>
          <input
            type="text"
            name="git_author_email"
            autocomplete="off"
            spellcheck={false}
            value={authorEmail()}
            onInput={(e) => setAuthorEmail(e.currentTarget.value)}
          />
          <small class="hint">Blank → global setting.</small>
        </label>
      </section>

      <section class="card">
        <h2>Incogni</h2>
        <label class="check">
          <input
            type="checkbox"
            name="incogni"
            checked={incogni()}
            onChange={(e) => setIncogni(e.currentTarget.checked)}
          />
          <span>Incogni</span>
        </label>
        <small class="hint hint-block">
          Strips AI attribution from this repo's output. Toggling it does NOT rewrite the branch
          pattern or prefix above — adjust those yourself if needed. It cannot hide the forge
          account of the token used, nor style or timing signals.
        </small>
      </section>

      <button type="submit" class="primary wide" disabled={form.busy()}>
        {form.busy() ? 'Saving…' : 'Save changes'}
      </button>
    </form>
  );
}
