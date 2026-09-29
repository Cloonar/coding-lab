// SSH-bastion reachability (issue #39 / ADR-0068): a read-only status card on
// global settings › General, mounted directly after CredentialGatewayStatus —
// the non-HTTP sibling of the OneCLI integration it sits beside (OneCLI fronts
// outbound HTTPS, Warpgate fronts outbound SSH). GET /warpgate/health always
// answers 200 — `off` means the integration isn't configured, which is normal
// and must never render as a failure. A failed fetch (network error, or the
// endpoint not deployed yet) falls back to the same muted, non-alarming
// treatment as the loading state — never a crash.
//
// Chip vocabulary and Switch/Match structure mirror CredentialGatewayStatus
// exactly: bare `.chip` for off, `.chip.in-use` for ok, `.chip.status-warn`
// for degraded, `.chip.status-error` for unreachable.
//
// Two things this card adds that the credential gateway's does not, because
// Warpgate's health carries them and OneCLI's does not: an "admin token
// rejected" line when the API dials fine but the token itself was refused
// (`api.reachable && !api.authenticated`), and the host-key state. That state
// is read-only here: which key runs trust is `--warpgate-ssh-host-key`, a
// server setting, so on a `mismatch` the card shows both sides and says what
// to change, and with no trusted key configured (`unpinned`) it lists the
// fingerprints the bastion presents so the operator can pin one.

import { For, Match, Show, Switch, createResource } from 'solid-js';
import { errorMessage, getWarpgateHealth, type WarpgateHealth } from '../api';
import SectionCard from './SectionCard';

/** Configured-but-unreachable components, each with its raw dial/request
 *  error when the server reported one. */
function unreachableComponents(health: WarpgateHealth): { label: string; error?: string }[] {
  const out: { label: string; error?: string }[] = [];
  if (health.api.configured && !health.api.reachable) {
    out.push({ label: 'Warpgate API', error: health.api.error });
  }
  if (health.ssh.configured && !health.ssh.reachable) {
    out.push({ label: 'SSH listener', error: health.ssh.error });
  }
  return out;
}

export default function BastionStatus() {
  const [health] = createResource(() => getWarpgateHealth());

  /** The failing components plus a possible admin-token line, joined as one
   *  detail line — empty when nothing's wrong (ok, or no data yet). */
  const detail = (): string => {
    const current = health();
    if (current === undefined) return '';
    const parts = unreachableComponents(current).map((c) =>
      c.error ? `${c.label} unreachable: ${c.error}` : `${c.label} unreachable`,
    );
    // Reachable but rejected is a distinct failure from a dial error: the
    // admin token file exists and lab can reach Warpgate, but Warpgate itself
    // refused it.
    if (current.api.reachable && current.api.authenticated === false) {
      parts.push('admin token rejected');
    }
    return parts.join(' · ');
  };

  return (
    <SectionCard
      title="SSH bastion"
      action={
        <Switch>
          <Match when={health.loading && health() === undefined && health.error === undefined}>
            <span class="muted">checking…</span>
          </Match>
          <Match when={health.error !== undefined}>
            <span class="muted">unknown</span>
          </Match>
          <Match when={health()?.state === 'off'}>
            <span class="chip">Off</span>
          </Match>
          <Match when={health()?.state === 'ok'}>
            <span class="chip in-use">Reachable</span>
          </Match>
          <Match when={health()?.state === 'degraded'}>
            <span class="chip status-warn">Degraded</span>
          </Match>
          <Match when={health()?.state === 'unreachable'}>
            <span class="chip status-error">Unreachable</span>
          </Match>
        </Switch>
      }
    >
      <Switch>
        <Match when={health.error !== undefined}>
          <p class="muted card-sub">{errorMessage(health.error)}</p>
        </Match>
        <Match when={health()?.state === 'off'}>
          <p class="muted card-sub">Not configured — the SSH bastion integration is off.</p>
        </Match>
        <Match when={health() !== undefined}>
          <>
            <Show when={detail() !== ''}>
              <p class="muted card-sub" title={detail()}>
                {detail()}
              </p>
            </Show>
            <HostKeyDetail health={health() as WarpgateHealth} />
          </>
        </Match>
      </Switch>
    </SectionCard>
  );
}

/** A comma-joined run of fingerprints, each in monospace. */
function Fingerprints(props: { list: string[] }) {
  return (
    <For each={props.list}>
      {(fp, i) => (
        <>
          {i() > 0 ? ', ' : ''}
          <code class="mono">{fp}</code>
        </>
      )}
    </For>
  );
}

/**
 * The host-key state, rendered under the reachability detail line.
 * `unpinned` and `mismatch` are the two states an operator must act on or
 * understand; `pinned` (the normal, healthy state with a trusted key) and
 * `unreachable` (the scan itself failed — already named in the reachability
 * detail above, since a failed scan means the SSH listener probe failed too)
 * render nothing extra.
 */
function HostKeyDetail(props: { health: WarpgateHealth }) {
  const hostKey = () => props.health.hostKey;

  return (
    <Switch>
      <Match when={hostKey()?.state === 'unpinned'}>
        <div class="card-sub">
          <p class="muted">
            No trusted host key is configured — runs trust whatever the bastion presents. To pin it,
            set <code class="mono">--warpgate-ssh-host-key</code> to one of the keys it presents
            now.
          </p>
          <Show when={(hostKey()?.observed ?? []).length > 0}>
            <p>
              Observed: <Fingerprints list={hostKey()?.observed ?? []} />
            </p>
          </Show>
        </div>
      </Match>
      <Match when={hostKey()?.state === 'mismatch'}>
        <div class="card-sub">
          <p>
            Warpgate's SSH host key does not match the trusted key configured with{' '}
            <code class="mono">--warpgate-ssh-host-key</code>. Target-bearing spawns are blocked
            until that setting names a key the bastion presents — verify the observed fingerprint
            out of band (e.g. with ssh-keyscan run on the Warpgate host itself) before changing it.
          </p>
          <p>
            Trusted: <Fingerprints list={hostKey()?.pinned ?? []} />
          </p>
          <p>
            Observed: <Fingerprints list={hostKey()?.observed ?? []} />
          </p>
        </div>
      </Match>
    </Switch>
  );
}
