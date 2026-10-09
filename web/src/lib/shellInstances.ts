// The shell's single instances list, shared by context (issue #76). AppShell
// owns the ONE listInstances resource (refetched on run.changed, patched in
// place by run.messages.changed — see AppShell's header); the rail's RunList,
// the Runs page at `/`, the tab bar's Runs badge, the app badge and the
// `(N) lab` document title all read it through this context. A page must never
// fetch GET /api/v1/instances a second time for what the shell already holds.

import { createContext, useContext } from 'solid-js';
import type { Instance } from '../api';

export interface ShellInstances {
  /** Every instance the shell's resource holds; [] until the first load and on error. */
  all: () => Instance[];
  /** True once the first fetch has settled (successfully or not). */
  loaded: () => boolean;
  /** The resource's error, undefined when the last fetch succeeded. */
  error: () => unknown;
}

export const ShellInstancesContext = createContext<ShellInstances>();

/** Reads the shell's instances; throws outside AppShell (tests wrap in the provider). */
export function useShellInstances(): ShellInstances {
  const ctx = useContext(ShellInstancesContext);
  if (ctx === undefined) throw new Error('useShellInstances must be used under AppShell');
  return ctx;
}

/** A live run waiting on the operator: needs_input or question. */
export function needsYou(instance: Instance): boolean {
  return instance.live && (instance.state === 'needs_input' || instance.state === 'question');
}

/** The Runs badge / `(N) lab` / app-badge count: live runs that need you. */
export function attentionCount(instances: Instance[]): number {
  return instances.filter(needsYou).length;
}
