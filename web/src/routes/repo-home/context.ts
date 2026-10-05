// The repo home's shared state (issue #61): the frame at /repos/:id owns the
// ONE live repo resource (getRepo + repo.changed) and the one toast, and hands
// both to every tab through useRepoHome(). Tabs never fetch the repo
// themselves — Overview, Issues, CRs, Labels and Settings all read it here.

import { createContext, useContext, type Accessor } from 'solid-js';
import type { Repo } from '../../api';
import type { ToastOptions } from '../../components/Toast';

export interface RepoHomeState {
  /** The route's repo id. Reactive: the frame stays mounted from repo to repo. */
  id: Accessor<string>;
  /**
   * The repo, or undefined while it loads and after a failed first load. A
   * failed REFETCH of the same repo keeps answering the last good repo (the
   * frame's banner reports the failure), so the tabs stay mounted. Never
   * throws, and never answers another repo's data: after a move to another
   * id it reads undefined until that repo has loaded.
   */
  repo: Accessor<Repo | undefined>;
  /** The load failure (the frame shows it in a banner), or undefined. */
  error: Accessor<unknown>;
  /** True while the repo is being fetched or refetched. */
  loading: Accessor<boolean>;
  /** Refetches the repo; resolves to the fresh repo, or undefined on failure. */
  refetch: () => Promise<Repo | undefined>;
  /**
   * Replaces the cached repo at once (an optimistic update before a refetch,
   * or a mutation's response). Pass only a repo of the current id.
   */
  mutate: (next: Repo) => void;
  /** Shows the frame's toast — one per repo home, shared by every tab. */
  notify: (message: string, options?: ToastOptions) => void;
}

export const RepoHomeContext = createContext<RepoHomeState>();

/** The repo home's state; only valid inside the /repos/:id frame. */
export function useRepoHome(): RepoHomeState {
  const state = useContext(RepoHomeContext);
  if (state === undefined) {
    throw new Error('useRepoHome() must be used inside the repo home frame (/repos/:id)');
  }
  return state;
}
