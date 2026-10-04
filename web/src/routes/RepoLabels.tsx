// Labels management (/repos/:id/labels, builtin repos only): the five seeded
// triage labels plus any custom ones, with create / edit (name, color as hex
// or picker, description) / delete-with-confirm. A name collision 409s and
// the server's message lands in the form banner; deleting cascades the label
// off every issue (the confirm says so). Forge repos manage labels on the
// forge. Label mutations emit issue.changed, which refetches here too. It
// renders inside the repo home frame's Issues tab (issue #61): the frame owns
// the page, the repo heading and the repo fetch; this page keeps an
// "Issues / Labels" trail within the tab. Delete asks in place (InlineConfirm),
// never through a browser confirm.

import { useParams } from '@solidjs/router';
import { For, Match, Show, Switch, createEffect, createSignal } from 'solid-js';
import { createStore, reconcile } from 'solid-js/store';
import {
  createLabel,
  deleteLabel,
  errorMessage,
  listLabels,
  updateLabel,
  type Label,
  type LabelPatch,
} from '../api';
import Crumbs, { type Crumb } from '../components/Crumbs';
import Banner from '../components/Banner';
import LabelChip from '../components/LabelChip';
import InlineConfirm from '../components/InlineConfirm';
import SectionCard from '../components/SectionCard';
import SectionHead from '../components/SectionHead';
import { canMutateTracker } from '../lib/issues';
import { DEFAULT_LABEL_COLOR, normalizeHex } from '../lib/labels';
import { createLiveResource } from '../lib/liveResource';
import { resourceValue } from '../lib/resource';
import { useRepoHome } from './repo-home/context';

export default function RepoLabels() {
  const params = useParams<{ id: string }>();
  const home = useRepoHome();

  // The frame's repo: undefined while loading or after a failed getRepo (the
  // frame shows that banner).
  const repoData = () => home.repo();
  const builtin = () => {
    const r = repoData();
    return r !== undefined && canMutateTracker(r.tracker_binding);
  };
  const [labels, { refetch }] = createLiveResource(
    () => (builtin() ? params.id : null),
    (id) => listLabels(id),
    [{ type: 'issue.changed', match: (event) => event.repoID === params.id && builtin() }],
  );
  // The rows render from a store reconciled by label id, NOT from the raw
  // resource: an SSE-triggered refetch returns fresh object identities, and a
  // reference-keyed <For each={labels()}> would tear down every row —
  // remounting an open LabelForm and silently wiping the operator's
  // in-progress edit. reconcile() keeps the same row object (and thus the
  // form's draft state) for every label id that survives the refetch.
  const [rows, setRows] = createStore<Label[]>([]);
  createEffect(() => {
    const next = resourceValue(labels);
    if (next !== undefined) setRows(reconcile(next, { key: 'id' }));
  });

  const crumbs = (): Crumb[] => [
    { label: 'Issues', href: `/repos/${params.id}/issues` },
    { label: 'Labels' },
  ];

  const [error, setError] = createSignal<string | null>(null);
  const [editing, setEditing] = createSignal<string | null>(null); // label id
  const [creating, setCreating] = createSignal(false);

  // Asked in place first (InlineConfirm): deleting cannot be undone, and it
  // strips the label from every issue carrying it — the confirm button says so.
  const remove = async (label: Label) => {
    setError(null);
    try {
      await deleteLabel(params.id, label.id);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      void refetch();
    }
  };

  return (
    <>
      <Crumbs segments={crumbs()} />
      <SectionHead title="Labels" />
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Switch>
        <Match when={repoData() !== undefined && !builtin()}>
          <p class="muted forge-note">Managed on the forge — labels live there.</p>
        </Match>
        <Match when={labels.error !== undefined}>
          <Banner message={errorMessage(labels.error)} />
        </Match>
        <Match when={labels()}>
          <div class="stack">
            <section class="card">
              <ul class="label-list">
                <For each={rows}>
                  {(label) => (
                    <li>
                      <Show
                        when={editing() === label.id}
                        fallback={
                          <div class="label-row">
                            <LabelChip name={label.name} color={label.color} />
                            <span class="muted label-desc">{label.description}</span>
                            <span class="spacer" />
                            <button
                              type="button"
                              class="small"
                              onClick={() => {
                                setCreating(false);
                                setEditing(label.id);
                              }}
                            >
                              Edit
                            </button>
                            <InlineConfirm
                              label="Delete"
                              confirmLabel="Delete from every issue"
                              aria-label={`Delete label ${label.name}`}
                              class="danger"
                              small
                              onConfirm={() => remove(label)}
                            />
                          </div>
                        }
                      >
                        <LabelForm
                          repoID={params.id}
                          existing={label}
                          onDone={() => {
                            setEditing(null);
                            void refetch();
                          }}
                          onCancel={() => setEditing(null)}
                        />
                      </Show>
                    </li>
                  )}
                </For>
              </ul>
            </section>
            <Show
              when={creating()}
              fallback={
                <button
                  type="button"
                  class="wide"
                  onClick={() => {
                    setEditing(null);
                    setCreating(true);
                  }}
                >
                  + New label
                </button>
              }
            >
              <SectionCard title="New label">
                <LabelForm
                  repoID={params.id}
                  onDone={() => {
                    setCreating(false);
                    void refetch();
                  }}
                  onCancel={() => setCreating(false)}
                />
              </SectionCard>
            </Show>
          </div>
        </Match>
      </Switch>
    </>
  );
}

/**
 * Create/edit form. Color is a hex text input synced with a native picker;
 * edits PATCH only the fields that actually changed (name collision → the
 * server's 409 message shows here).
 */
function LabelForm(props: {
  repoID: string;
  existing?: Label;
  onDone: () => void;
  onCancel: () => void;
}) {
  // eslint-disable-next-line solid/reactivity -- snapshot by design: the form edits a draft copy
  const initial = props.existing;
  const [name, setName] = createSignal(initial?.name ?? '');
  const [color, setColor] = createSignal(initial?.color ?? DEFAULT_LABEL_COLOR);
  const [description, setDescription] = createSignal(initial?.description ?? '');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    const trimmedName = name().trim();
    if (trimmedName === '') {
      setError('Name must not be empty.');
      return;
    }
    const hex = normalizeHex(color());
    if (hex === '') {
      setError('Color must be a hex value like #0e8a16.');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      if (props.existing === undefined) {
        await createLabel(props.repoID, {
          name: trimmedName,
          color: hex,
          description: description().trim(),
        });
      } else {
        const patch: LabelPatch = {};
        if (trimmedName !== props.existing.name) patch.name = trimmedName;
        if (hex !== props.existing.color) patch.color = hex;
        if (description().trim() !== props.existing.description) {
          patch.description = description().trim();
        }
        if (Object.keys(patch).length > 0) {
          await updateLabel(props.repoID, props.existing.id, patch);
        }
      }
      props.onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="label-form" onSubmit={(e) => void submit(e)}>
      <Banner message={error()} onDismiss={() => setError(null)} />
      <label class="field">
        <span>Name</span>
        <input
          type="text"
          name="label-name"
          required
          autocomplete="off"
          spellcheck={false}
          value={name()}
          onInput={(e) => setName(e.currentTarget.value)}
        />
      </label>
      <div class="field-row color-row">
        <label class="field">
          <span>Color</span>
          <input
            type="text"
            name="label-color"
            class="mono"
            autocomplete="off"
            spellcheck={false}
            value={color()}
            onInput={(e) => setColor(e.currentTarget.value)}
          />
        </label>
        <input
          type="color"
          aria-label="Pick color"
          value={normalizeHex(color()) || DEFAULT_LABEL_COLOR}
          onInput={(e) => setColor(e.currentTarget.value)}
        />
        <span class="label-preview">
          <LabelChip
            name={name().trim() === '' ? 'preview' : name().trim()}
            color={normalizeHex(color()) || DEFAULT_LABEL_COLOR}
          />
        </span>
      </div>
      <label class="field">
        <span>Description</span>
        <input
          type="text"
          name="label-description"
          autocomplete="off"
          value={description()}
          onInput={(e) => setDescription(e.currentTarget.value)}
        />
      </label>
      <div class="card-actions">
        <button type="submit" class="primary" disabled={busy()}>
          {busy() ? 'Saving…' : props.existing === undefined ? 'Create label' : 'Save label'}
        </button>
        <button type="button" onClick={() => props.onCancel()} disabled={busy()}>
          Cancel
        </button>
      </div>
    </form>
  );
}
