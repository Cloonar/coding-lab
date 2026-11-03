// Runner section (issues #205, #55, #61, #115): where this repo's instances
// run — the Runner pick, the dev image, the three container limits and Nested
// containers — as a thin renderer over the form store. Every control edits a
// draft; the page's save bar sends the changed ones (form.tsx, fields.ts).
//
// All six fields are overridable: each says "inherited" or "set here" and
// names what it inherits (Field.tsx). The Runner is a three-way pick —
// inherited (naming the Runner that resolves to), Container, Host — and so is
// Nested containers: inherited (naming the global default's on / off), On,
// Off, where Off is a value, never a blank.
//
// Only what applies (issue #61 §7): the EFFECTIVE Runner is the drafted pick
// when set here, else the inherited one. While it is `host` the unsandboxed
// warning shows, and the dev image, the container limits and Nested
// containers — which only container runs read — fold into one note. "Show
// anyway" unfolds them, and Nested containers then says the host runner
// ignores it: staging a value before switching the Runner is legal. A folded field is
// never out of reach: it unfolds by itself while it has a pending change or a
// problem, and when a link or a Save points at it. Folding only hides — it
// never clears or changes a value.

import { Show, createComputed, createSignal } from 'solid-js';
import Banner from '../../../components/Banner';
import { HOST_RUNNER_HINT, NESTED_CONTAINERS_HINT, NESTED_HOST_NOTE } from '../../../lib/runner';
import { FieldGroup, SegmentedField, TextField, fieldControlId } from '../Field';
import type { RepoFieldKey } from '../fields';
import { useRepoSettingsForm } from '../form';

/** The explicit Runner picks; the inherit segment comes first. */
const RUNNER_PICKS = [
  { value: 'container', label: 'Container' },
  { value: 'host', label: 'Host' },
];

/** The explicit Nested containers picks; the inherit segment comes first. */
const NESTED_PICKS = [
  { value: 'true', label: 'On' },
  { value: 'false', label: 'Off' },
];

/** Why the container-only fields fold (or, unfolded, what they apply to). */
const CONTAINER_ONLY_NOTE =
  'Dev image, container limits and nested containers apply to container runs only.';

/** What only a container run reads. */
const CONTAINER_ONLY = [
  'image_ref',
  'container_memory',
  'container_pids',
  'container_nofile',
  'nested_containers',
] as const satisfies readonly RepoFieldKey[];

export default function RunnerSection() {
  const form = useRepoSettingsForm();
  const image = form.field('image_ref');

  const host = (): boolean => form.effectiveRunner() === 'host';
  // "Show anyway" — and a field of the fold that something pointed at stays
  // shown from then on.
  const [shown, setShown] = createSignal(false);
  createComputed(() => {
    const field = form.pointedAt();
    if (field !== undefined && (CONTAINER_ONLY as readonly string[]).includes(field)) {
      setShown(true);
    }
  });
  const inUse = (): boolean =>
    CONTAINER_ONLY.some((key) => form.field(key).changed() || form.field(key).error() !== null);
  const folded = (): boolean => host() && !shown() && !inUse();

  const showAnyway = (): void => {
    setShown(true);
    // The action is gone with the click: hand the focus to the first field
    // it revealed.
    queueMicrotask(() => document.getElementById(fieldControlId('image_ref'))?.focus());
  };

  // '' = no dev image is configured anywhere below the repo.
  const noImageBelow = (): boolean => form.inherited()?.image_ref === '';
  const inheritedImage = (): string | undefined => form.inherited()?.image_ref || undefined;

  return (
    <div class="card settings-card">
      <SegmentedField name="runner" options={RUNNER_PICKS} />
      <Show when={host()}>
        <Banner message={HOST_RUNNER_HINT} variant="notice" />
      </Show>
      <Show
        when={!folded()}
        fallback={
          <p class="settings-na">
            {CONTAINER_ONLY_NOTE}{' '}
            <button type="button" class="settings-link-action" onClick={showAnyway}>
              Show anyway
            </button>
          </p>
        }
      >
        <Show when={host()}>
          <p class="settings-note">{CONTAINER_ONLY_NOTE}</p>
        </Show>
        <TextField
          name="image_ref"
          mono
          hint={
            <>
              Resolved and pinned to a digest on save.
              <Show when={image.inherits()}>
                <Show when={inheritedImage()}>
                  {(ref) => (
                    <>
                      {' '}
                      Inherits <code>{ref()}</code>.
                    </>
                  )}
                </Show>
                <Show when={noImageBelow()}>
                  {' '}
                  No dev image is configured — container spawns are refused until one is set here,
                  in global Settings, or on the server.
                </Show>
              </Show>
            </>
          }
        />
        <FieldGroup title="Container limits">
          <div class="settings-grid3 even">
            <TextField name="container_memory" />
            <TextField name="container_pids" type="number" />
            <TextField name="container_nofile" type="number" />
          </div>
        </FieldGroup>
        <SegmentedField
          name="nested_containers"
          options={NESTED_PICKS}
          hint={
            <>
              {NESTED_CONTAINERS_HINT}
              <Show when={host()}> {NESTED_HOST_NOTE}</Show>
            </>
          }
        />
      </Show>
    </div>
  );
}
