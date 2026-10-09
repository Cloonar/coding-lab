// The Runner vocabulary the two Runner settings pages share (issue #55): the
// repo's own pick (repo-settings › Runner) and the global runner default
// (settings › Runner) — one module so the copy can never drift between them.
// Both pick "Container" / "Host" (issue #85); "host" is spelled out as
// unsandboxed/break-glass by the warning below, shown under either pick
// whenever it is set to host.

/** Shown under a Runner picker whenever the effective runner is `host`. */
export const HOST_RUNNER_HINT =
  'Host runs are unsandboxed — the agent has full host access to the server. Break-glass only; use the container runner once available.';

/** Placeholder of every dev image field (repo override and global default). */
export const DEV_IMAGE_PLACEHOLDER = 'docker.io/library/debian:bookworm';

/**
 * Operator-facing name of a stored runner value ("Container" / "Host"), for the
 * "Inherit global default — currently …" row. null for anything that is not a
 * real runner (an absent setting, or a value the server would refuse) so the
 * caller can drop the "currently …" suffix rather than state something false.
 */
export function runnerName(value: string | null | undefined): string | null {
  if (value === 'container') return 'Container';
  if (value === 'host') return 'Host';
  return null;
}
