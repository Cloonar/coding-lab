// Non-throwing resource reads. Solid re-throws a resource's stored error on
// every value read, so a raw `repo()` outside the guarded error <Match> — an
// h2 title, a canMutate() gate, another resource's source function — aborts
// the whole render batch and the designed error banner never appears (the
// page goes silently blank instead). resourceValue() maps the errored state
// to undefined so such reads stay safe anywhere, while the dedicated
// `resource.error` branch reports the failure.

import type { Resource } from 'solid-js';

/** Reads a resource, answering undefined for an errored one instead of throwing. */
export function resourceValue<T>(resource: Resource<T>): T | undefined {
  return resource.error !== undefined ? undefined : resource();
}
