// New run (Home, `/`) — Phase 1 PLACEHOLDER. Phase 2b replaces the body with
// the big centered composer (repo/model/effort chips, textarea, send, AFK
// strip). For now it is deliberately minimal: the heading, a note about what
// lands here, and the zero-repos empty state whose "No repositories yet" text
// the login/setup round-trip and the Playwright smoke assert on `/`.

import { A } from '@solidjs/router';
import { Show, createResource } from 'solid-js';
import { listRepos } from '../api';
import RequireAuth from '../components/RequireAuth';
import { resourceValue } from '../lib/resource';

export default function NewRun() {
  return (
    <RequireAuth>
      <NewRunView />
    </RequireAuth>
  );
}

function NewRunView() {
  const [repos] = createResource(() => listRepos());
  const noRepos = () => {
    const value = resourceValue(repos);
    return value !== undefined && value.length === 0;
  };

  return (
    <main class="page">
      <h2>New run</h2>
      <p class="muted">The new-run composer lands here.</p>
      <Show when={noRepos()}>
        <p class="empty">
          No repositories yet — <A href="/repos/new">add one</A> to get started.
        </p>
      </Show>
    </main>
  );
}
