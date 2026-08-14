// Credential-gateway grant picker (issue #25 / ADR-0067): the lab's whole
// OneCLI project pool rendered as a per-repo toggle list. A grant here is the
// gateway sense of the word — one pool resource attached to this repo's OneCLI
// agent identity — never the read-only import's repo→repo code-read
// permission, which is always written out in full (CONTEXT.md's flagged
// ambiguities). "Agent" bare means the coding agent everywhere in this app, so
// the OneCLI object is only ever "agent identity" / "OneCLI agent" here.
//
// Device-local/immediate like Secrets.tsx and Imports.tsx: a toggle talks to
// the server the moment it is clicked, so there is no useSettingsForm, no Save
// button and no unsaved-changes guard. It is deliberately NOT optimistic — a
// successful call refetches the grant set and the row re-renders from that,
// because the picker's whole job is to show what the gateway will actually
// admit, and a row that says "granted" off a local guess is worse than one
// that takes a moment to say it off the server's answer.
//
// The four states are distinct on purpose and none of them spins forever:
// loading is a muted line (CredentialGatewayStatus's house treatment, there is
// no spinner component); `configured: false` is the integration simply not
// being set up in this lab, which is normal and healthy and never an error; a
// throw is unreachable-or-erroring and gets a banner WITH a Retry; and a
// configured-but-empty pool is an empty state pointing at the dashboard.
//
// Values never appear here and no screen in lab ever creates one: the OneCLI
// dashboard is the only place a secret value is created, rotated or deleted.
// The link-out to it is composed from GET /onecli/dashboard and from nothing
// else — never from window.location or any other origin lab happens to know —
// which is what keeps this section topology-free and the login bounce-back
// structurally incapable of being an open redirect (ADR-0067, amended
// 2026-08-14). No exposure, no link: an annotation instead.

import { For, Match, Show, Switch, createMemo, createResource, createSignal } from 'solid-js';
import {
  attachRepoOneCLIGrant,
  detachRepoOneCLIGrant,
  errorMessage,
  getOneCLIDashboard,
  getOneCLIPool,
  listRepoOneCLIGrants,
  type OneCLIGrantKind,
  type OneCLIPoolEntry,
} from '../../../api';
import Banner from '../../../components/Banner';
import EmptyState from '../../../components/EmptyState';
import Icon from '../../../components/Icon';
import ListRowCard from '../../../components/ListRowCard';
import SectionCard from '../../../components/SectionCard';
import { resourceValue } from '../../../lib/resource';

/** Where an operator reads how to switch the credential gateway on. */
const GATEWAY_DOCS_URL =
  'https://github.com/Cloonar/coding-lab/blob/main/docs/ops.md#onecli-credential-gateway';

/** One pool entry paired with the half of the pool it came from. The PAIR is
 *  the identity: an id is only unique within its kind, so matching a row
 *  against the grant list on the id alone would cross-light a secret and a
 *  connection that happen to share one. */
interface PoolRow {
  kind: OneCLIGrantKind;
  entry: OneCLIPoolEntry;
}

const KIND_LABEL: Record<OneCLIGrantKind, string> = {
  secrets: 'Secret',
  connections: 'Connection',
};

const grantKey = (kind: OneCLIGrantKind, id: string): string => `${kind}/${id}`;

export default function RepoSecretGrantsSection(props: { repoId: string }) {
  const [pool, { refetch: refetchPool }] = createResource(() => getOneCLIPool());
  const [grants, { refetch: refetchGrants }] = createResource(() =>
    listRepoOneCLIGrants(props.repoId),
  );
  // Its own resource, and its failure is deliberately NOT folded into the two
  // above: a dashboard whose exposure lab cannot report is a missing link, not
  // a broken picker.
  const [exposure] = createResource(() => getOneCLIDashboard());
  const [error, setError] = createSignal<string | null>(null);

  const readError = () => pool.error ?? grants.error;
  const loaded = () => resourceValue(pool) !== undefined && resourceValue(grants) !== undefined;

  // Secrets first, then connections — the pool's own order, kept stable so a
  // refetch never reshuffles the rows under the operator's cursor.
  const rows = createMemo<PoolRow[]>(() => {
    const current = resourceValue(pool);
    if (current === undefined) return [];
    return [
      ...current.secrets.map((entry) => ({ kind: 'secrets' as const, entry })),
      ...current.connections.map((entry) => ({ kind: 'connections' as const, entry })),
    ];
  });

  const grantedKeys = createMemo(
    () => new Set((resourceValue(grants)?.grants ?? []).map((g) => grantKey(g.kind, g.id))),
  );

  /** The dashboard's browser-facing origin, or null when there is nothing to
   *  link to — off, no URL reported, or the exposure read itself failed. */
  const dashboardUrl = (): string | null => {
    const current = resourceValue(exposure);
    if (current === undefined || current.mode === 'off') return null;
    return current.url !== undefined && current.url !== '' ? current.url : null;
  };
  /** True once the exposure read has an answer either way — until then neither
   *  the link nor the "not exposed" line is a truthful thing to render. */
  const exposureSettled = () =>
    resourceValue(exposure) !== undefined || exposure.error !== undefined;

  /** Re-reads the grant set. Awaited by a row so its toggle stays busy until
   *  the answer that will actually be rendered has landed. */
  const refreshGrants = async (): Promise<void> => {
    await refetchGrants();
  };

  const retry = () => {
    setError(null);
    void refetchPool();
    void refetchGrants();
  };

  return (
    <SectionCard
      title="Credential gateway"
      hint={
        <>
          These secrets live in the OneCLI project pool, shared by the whole lab. Toggling one here
          decides whether this repo's runs may use it — lab never creates, stores or shows a value.
        </>
      }
    >
      <Banner message={error()} onDismiss={() => setError(null)} />
      <Show when={resourceValue(pool)?.configured === true}>
        <Switch>
          <Match when={dashboardUrl()}>
            {(url) => (
              <p class="card-sub">
                <a href={url()} target="_blank" rel="noopener noreferrer" class="card-link">
                  <Icon name="external-link" class="action-icon" />
                  <span>
                    Open the OneCLI dashboard — secret values are created, rotated and deleted
                    there, and only there
                  </span>
                </a>
              </p>
            )}
          </Match>
          <Match when={exposureSettled()}>
            <p class="muted card-sub">
              The OneCLI dashboard is not exposed by this lab — secret values are created, rotated
              and deleted there, never in lab.
            </p>
          </Match>
        </Switch>
      </Show>
      <Switch>
        {/* First, and reading only `.error`: Solid re-throws a resource's error
            on every value read, so every branch below is reached with both
            reads known-good. */}
        <Match when={readError() !== undefined}>
          <Banner
            message={errorMessage(readError())}
            action={
              <button type="button" class="small" onClick={retry}>
                Retry
              </button>
            }
          />
        </Match>
        <Match when={!loaded()}>
          <span class="muted">Loading the OneCLI project pool…</span>
        </Match>
        <Match when={resourceValue(pool)?.configured === false}>
          <p class="muted card-sub">
            Not configured — the credential gateway integration is off in this lab, so this repo has
            nothing to grant. See the{' '}
            <a href={GATEWAY_DOCS_URL} target="_blank" rel="noopener noreferrer">
              credential gateway setup docs
            </a>
            .
          </p>
        </Match>
        <Match when={rows().length === 0}>
          <EmptyState>
            The OneCLI project pool is empty — create the first secret in the OneCLI dashboard, then
            grant it to this repo here.
          </EmptyState>
        </Match>
        <Match when={rows().length > 0}>
          <div class="card-list" role="group" aria-label="Credential gateway grants">
            <For each={rows()}>
              {(row) => (
                <GrantRow
                  repoId={props.repoId}
                  row={row}
                  granted={grantedKeys().has(grantKey(row.kind, row.entry.id))}
                  onChanged={refreshGrants}
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
 * One pool resource, with the toggle that attaches or detaches it. `granted`
 * comes from the parent's grant resource and is never shadowed by local state:
 * the row reads as granted only once the server said so.
 */
function GrantRow(props: {
  repoId: string;
  row: PoolRow;
  granted: boolean;
  onChanged: () => Promise<void>;
  onError: (message: string | null) => void;
}) {
  const [busy, setBusy] = createSignal(false);

  const toggle = async () => {
    setBusy(true);
    props.onError(null);
    try {
      if (props.granted) {
        await detachRepoOneCLIGrant(props.repoId, props.row.kind, props.row.entry.id);
      } else {
        await attachRepoOneCLIGrant(props.repoId, props.row.kind, props.row.entry.id);
      }
      // Awaited, not fired and forgotten: the row stays busy until the
      // refetched grant set is what it renders from.
      await props.onChanged();
    } catch (err) {
      // No flip happened to roll back — the failed call leaves the row showing
      // the unchanged server state, and the section banner says why.
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <ListRowCard
      title={props.row.entry.name}
      sub={`${KIND_LABEL[props.row.kind]} · unlocks ${props.row.entry.provider}`}
      actions={
        <button
          type="button"
          name={`grant-${props.row.kind}-${props.row.entry.id}`}
          classList={{ 'chip-toggle': true, on: props.granted }}
          aria-pressed={props.granted}
          disabled={busy()}
          onClick={() => void toggle()}
        >
          {busy() ? 'Working…' : props.granted ? '✓ Granted' : 'Not granted'}
        </button>
      }
    />
  );
}
