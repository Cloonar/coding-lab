// "Is the agent provider logged out?" (issue #76) — the one setup fault the
// More tab's dot and the More page's banner report. The agent provider is the
// global default (Settings' provider_default, else the first registered
// provider — lib/spawn's providerFor), the one a new run uses unless a repo or
// a per-spawn pick overrides it. Its machine-level auth status refetches on
// provider.auth.changed. Must run under a component inside <EventsProvider>.

import { createResource } from 'solid-js';
import { getSpawnDefaults, listProviders, providerAuthStatus, type Provider } from '../api';
import { createLiveResource } from './liveResource';
import { resourceValue } from './resource';
import { providerFor } from './spawn';

export interface ProviderLogin {
  /** The default agent provider, undefined until providers and defaults load. */
  provider: () => Provider | undefined;
  /** True only on a positive "logged out" answer — unknown/loading/errored is false. */
  loggedOut: () => boolean;
  /** True only on a positive "logged in" answer — unknown/loading/errored is false. */
  loggedIn: () => boolean;
}

export function createProviderLogin(): ProviderLogin {
  const [providers] = createResource(() => listProviders());
  const [defaults] = createResource(() => getSpawnDefaults());
  const provider = (): Provider | undefined => {
    const list = resourceValue(providers);
    const d = resourceValue(defaults);
    if (list === undefined || d === undefined) return undefined;
    return providerFor(list, d.provider) ?? undefined;
  };
  const [status] = createLiveResource(
    () => provider()?.id,
    (id) => providerAuthStatus(id),
    [{ type: 'provider.auth.changed' }],
  );
  return {
    provider,
    loggedOut: () => resourceValue(status)?.logged_in === false,
    loggedIn: () => resourceValue(status)?.logged_in === true,
  };
}
