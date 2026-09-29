// SSH-bastion target picker (issue #39 / ADR-0068): the per-repo analogue of
// the credential-gateway grant picker (SecretGrants.tsx / issue #25), but for
// Warpgate SSH targets instead of the OneCLI pool. Targets and their
// credentials are defined by the operator in Warpgate's own admin UI — lab
// never creates, edits or reads one — so this section only ever toggles
// whether the repo's Warpgate role carries a target, exactly as the grant
// picker only ever toggles whether the repo's OneCLI agent identity carries a
// pool resource.
//
// One difference from the grant picker shapes this section simpler: there is
// no separate "lab-wide pool" resource here — GET /repos/{id}/warpgate/targets
// already answers with every target AND whether the repo's role carries it in
// one body, so there is exactly one resource to load, not two to join.
//
// Device-local/immediate, same as SecretGrants: a toggle talks to the server
// the moment it's clicked and is deliberately NOT optimistic — a successful
// call refetches the target list and the row re-renders from that, because
// this picker's job is to show what Warpgate will actually admit, not a local
// guess. A failed toggle leaves the row exactly as the server last described
// it and surfaces the failure in a banner.

import { For, Match, Switch, createResource, createSignal } from 'solid-js';
import {
  assignRepoSSHTarget,
  errorMessage,
  listRepoSSHTargets,
  unassignRepoSSHTarget,
  type RepoSSHTarget,
} from '../../../api';
import Banner from '../../../components/Banner';
import EmptyState from '../../../components/EmptyState';
import ListRowCard from '../../../components/ListRowCard';
import SectionCard from '../../../components/SectionCard';
import { resourceValue } from '../../../lib/resource';

export default function RepoSSHTargetsSection(props: { repoId: string }) {
  const [targets, { refetch }] = createResource(() => listRepoSSHTargets(props.repoId));
  const [error, setError] = createSignal<string | null>(null);

  const rows = () => resourceValue(targets)?.targets ?? [];

  /** Re-reads the target list. Awaited by a row so its toggle stays busy
   *  until the answer that will actually be rendered has landed. */
  const refreshTargets = async (): Promise<void> => {
    await refetch();
  };

  const retry = () => {
    setError(null);
    void refetch();
  };

  return (
    <SectionCard
      title="SSH targets"
      hint={
        <>
          Runs of this repo reach assigned targets as <code>ssh &lt;name&gt;</code> through the
          bastion; the target's credential never enters the run.
        </>
      }
    >
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Switch>
        {/* Listing is the one call this picker allows to degrade to an error
            message rather than failing closed (unlike a target-bearing spawn,
            which fails closed — see DESIGN.md). */}
        <Match when={targets.error !== undefined}>
          <Banner
            message={errorMessage(targets.error)}
            action={
              <button type="button" class="small" onClick={retry}>
                Retry
              </button>
            }
          />
        </Match>
        <Match when={resourceValue(targets) === undefined}>
          <span class="muted">Loading SSH targets…</span>
        </Match>
        <Match when={resourceValue(targets)?.configured === false}>
          <p class="muted card-sub">
            Not configured — the SSH bastion integration is off in this lab. Set{' '}
            <code>--warpgate-url</code> and <code>--warpgate-admin-token-file</code> to enable it.
          </p>
        </Match>
        <Match when={rows().length === 0}>
          <EmptyState>
            No SSH targets yet — targets are defined by the operator in Warpgate's own admin UI;
            once one exists there, it can be assigned to this repo here.
          </EmptyState>
        </Match>
        <Match when={rows().length > 0}>
          <div class="card-list" role="group" aria-label="SSH targets">
            <For each={rows()}>
              {(target) => (
                <TargetRow
                  repoId={props.repoId}
                  target={target}
                  onChanged={refreshTargets}
                  onError={setError}
                />
              )}
            </For>
          </div>
        </Match>
      </Switch>
    </SectionCard>
  );
}

/**
 * One SSH target, with the toggle that assigns or unassigns the repo's
 * Warpgate role. `assigned` comes from the parent's resource and is never
 * shadowed by local state: the row reads as assigned only once the server
 * said so — same discipline as SecretGrants' `GrantRow`.
 */
function TargetRow(props: {
  repoId: string;
  target: RepoSSHTarget;
  onChanged: () => Promise<void>;
  onError: (message: string | null) => void;
}) {
  const [busy, setBusy] = createSignal(false);

  const toggle = async () => {
    setBusy(true);
    props.onError(null);
    try {
      if (props.target.assigned) {
        await unassignRepoSSHTarget(props.repoId, props.target.id);
      } else {
        await assignRepoSSHTarget(props.repoId, props.target.id);
      }
      // Awaited, not fired and forgotten: the row stays busy until the
      // refetched target list is what it renders from.
      await props.onChanged();
    } catch (err) {
      // No flip happened to roll back — the failed call leaves the row
      // showing the unchanged server state, and the section banner says why.
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <ListRowCard
      title={<span class="mono">{props.target.name}</span>}
      sub={props.target.description || undefined}
      actions={
        <button
          type="button"
          name={`ssh-target-${props.target.id}`}
          classList={{ 'chip-toggle': true, on: props.target.assigned }}
          aria-pressed={props.target.assigned}
          disabled={busy()}
          onClick={() => void toggle()}
        >
          {busy() ? 'Working…' : props.target.assigned ? '✓ Assigned' : 'Not assigned'}
        </button>
      }
    />
  );
}
