// Credential-gateway reachability (issue #23): a read-only status card on
// global settings › General, letting an operator confirm the OneCLI sidecar
// (CONTEXT.md "credential gateway") is up before a run depends on it.
// GET /onecli/health always answers 200 — `off` means the integration isn't
// configured, which is normal and must never render as a failure. A failed
// fetch (network error, or the endpoint not deployed yet) falls back to the
// same muted, non-alarming treatment as the loading state — never a crash.
//
// Chip vocabulary follows ProviderAuthCard, the established precedent for a
// live server-side status badge: a bare `.chip` (muted/neutral) for off, the
// shared `.chip.in-use` / `.chip.status-error` for ok / unreachable, and a new
// `.chip.status-warn` (chips.css) — the same banner-tone family as
// `.chip.outcome-escalated` — for the partial (degraded) state.

import { Match, Switch, createResource } from 'solid-js';
import { errorMessage, getOneCLIHealth, type OneCLIHealth } from '../api';
import SectionCard from './SectionCard';

/** Configured-but-unreachable components, each with its raw dial error when
 *  the server reported one — the detail an operator needs to act on. */
function unreachableComponents(health: OneCLIHealth): { label: string; error?: string }[] {
  const out: { label: string; error?: string }[] = [];
  if (health.api.configured && !health.api.reachable) {
    out.push({ label: 'OneCLI API', error: health.api.error });
  }
  if (health.gateway.configured && !health.gateway.reachable) {
    out.push({ label: 'credential gateway', error: health.gateway.error });
  }
  return out;
}

export default function CredentialGatewayStatus() {
  const [health] = createResource(() => getOneCLIHealth());

  /** The failing components rendered as one line, e.g. "credential gateway
   *  unreachable: dial tcp …: connection refused" — empty when nothing's
   *  down (ok, or no data yet). */
  const detail = (): string => {
    const current = health();
    if (current === undefined) return '';
    return unreachableComponents(current)
      .map((c) => (c.error ? `${c.label} unreachable: ${c.error}` : `${c.label} unreachable`))
      .join(' · ');
  };

  return (
    <SectionCard
      title="Credential gateway"
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
            <span class="chip status-warn">Partly reachable</span>
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
          <p class="muted card-sub">Not configured — the credential gateway integration is off.</p>
        </Match>
        <Match when={detail() !== ''}>
          <p class="muted card-sub" title={detail()}>
            {detail()}
          </p>
        </Match>
      </Switch>
    </SectionCard>
  );
}
