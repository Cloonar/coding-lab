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
// (`api.reachable && !api.authenticated`), and the host-key pin state —
// including, on a `mismatch`, an explicit accept action. Accepting is the
// only mutating call this card makes, gated by the app's existing
// confirm-then-call pattern (Danger.tsx, SecretGrants.tsx's delete): the
// operator must confirm having verified the new fingerprint out of band
// before lab starts trusting it, since accepting the wrong key would let a
// spoofed bastion intercept every target-bearing run's SSH traffic.

import { For, Match, Show, Switch, createResource, createSignal } from 'solid-js';
import {
  acceptWarpgateHostKey,
  errorMessage,
  getWarpgateHealth,
  type WarpgateHealth,
} from '../api';
import Banner from './Banner';
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
  const [health, { refetch }] = createResource(() => getWarpgateHealth());

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
            <HostKeyDetail health={health() as WarpgateHealth} onAccepted={() => void refetch()} />
          </>
        </Match>
      </Switch>
    </SectionCard>
  );
}

/**
 * The pinned host-key state, rendered under the reachability detail line.
 * `unpinned` and `mismatch` are the two states an operator must act on or
 * understand; `pinned` (the normal, healthy state) and `unreachable` (the
 * scan itself failed — already named in the reachability detail above, since
 * a failed scan means the SSH listener probe failed too) render nothing extra.
 */
function HostKeyDetail(props: { health: WarpgateHealth; onAccepted: () => void }) {
  const [busyFingerprint, setBusyFingerprint] = createSignal<string | null>(null);
  const [error, setError] = createSignal<string | null>(null);

  const hostKey = () => props.health.hostKey;

  const accept = async (fingerprint: string) => {
    if (
      !window.confirm(
        `Accept new Warpgate host key ${fingerprint}?\n\n` +
          'Only accept after verifying this fingerprint out of band — accepting the wrong ' +
          'key lets a spoofed bastion intercept every affected run’s SSH traffic.',
      )
    ) {
      return;
    }
    setBusyFingerprint(fingerprint);
    setError(null);
    try {
      await acceptWarpgateHostKey(fingerprint);
      props.onAccepted();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusyFingerprint(null);
    }
  };

  return (
    <Switch>
      <Match when={hostKey()?.state === 'unpinned'}>
        <p class="muted card-sub">Host key not pinned yet.</p>
      </Match>
      <Match when={hostKey()?.state === 'mismatch'}>
        <div class="card-sub">
          <p>
            Warpgate's SSH host key changed. Target-bearing spawns are blocked until the new key is
            accepted — verify the observed fingerprint out of band (e.g. with ssh-keyscan run on the
            Warpgate host itself) before accepting it.
          </p>
          <p>
            Pinned:{' '}
            <For each={hostKey()?.pinned ?? []}>
              {(fp, i) => (
                <>
                  {i() > 0 ? ', ' : ''}
                  <code class="mono">{fp}</code>
                </>
              )}
            </For>
          </p>
          <For each={hostKey()?.observed ?? []}>
            {(fp) => (
              <p>
                Observed: <code class="mono">{fp}</code>{' '}
                <button
                  type="button"
                  class="small"
                  name={`accept-host-key-${fp}`}
                  disabled={busyFingerprint() !== null}
                  onClick={() => void accept(fp)}
                >
                  {busyFingerprint() === fp ? 'Accepting…' : 'Accept new host key'}
                </button>
              </p>
            )}
          </For>
          <Banner message={error()} onDismiss={() => setError(null)} />
        </div>
      </Match>
    </Switch>
  );
}
