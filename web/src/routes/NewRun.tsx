// New run (Home, `/`) — the composer-first surface (issue #41, Phase 2b): a big
// centered composer (repo · agent · model · effort chips, a `…` popover, an
// autogrowing textarea, an accent circular send), an AFK strip for the selected
// repo, and a slim logged-out banner. Send spawns an instance, queues any typed
// text as the run's first message, and navigates to the chat. Never
// auto-navigates on load. The agent chip appears only with ≥2 registered
// providers; the per-spawn pick is ephemeral (ADR-0030).
//
// Manual spawn accepts ONLY label/provider/model/effort (internal/httpapi/
// instances.go has no provider-options bag), so provider spawn options stay out
// of the `…` popover here and issue #21 stays open.

import { A, useNavigate } from '@solidjs/router';
import {
  Match,
  Show,
  Switch,
  createEffect,
  createResource,
  createSignal,
  on,
  onCleanup,
} from 'solid-js';
import {
  errorMessage,
  getSpawnDefaults,
  listProviders,
  listRepos,
  providerAuthStatus,
  startInstance,
  type Repo,
  type Run,
  type StartInstanceRequest,
} from '../api';
import AFKStrip from '../components/AFKStrip';
import ErrorBanner from '../components/ErrorBanner';
import Icon from '../components/Icon';
import RequireAuth from '../components/RequireAuth';
import Select, { type SelectOption } from '../components/Select';
import { createToast } from '../components/Toast';
import { useEvents } from '../events';
import { createLiveResource } from '../lib/liveResource';
import { providerFor, resolveSpawnOption } from '../lib/spawn';
import { setQueued } from '../lib/queuedMessage';
import { resourceValue } from '../lib/resource';
import { createCloneProgressStore, type CloneProgress } from '../stores/cloneProgress';

// Last-used repo, remembered so the composer opens on the repo you spawn from
// most. The stored id is validated against the live list on read (it may have
// been deleted, or gone un-ready) — the getter falls back to the first ready
// repo, so a stale value never strands the composer.
const LAST_REPO_KEY = 'lab.last-repo';

export default function NewRun() {
  return (
    <RequireAuth>
      <NewRunView />
    </RequireAuth>
  );
}

function NewRunView() {
  const events = useEvents();
  const navigate = useNavigate();

  // repo.changed keeps clone_status fresh so a cloning repo becomes selectable
  // the moment it lands; the auth banner refetches on its own SSE event.
  const [repos, { refetch: refetchRepos }] = createLiveResource(
    () => listRepos(),
    [{ type: 'repo.changed' }],
  );
  const [providers] = createResource(() => listProviders());
  const [defaults] = createResource(() => getSpawnDefaults());
  const progress = createCloneProgressStore(events);
  const toast = createToast();
  onCleanup(progress.dispose);

  const readLastRepo = (): string | null => {
    try {
      return localStorage.getItem(LAST_REPO_KEY);
    } catch {
      return null;
    }
  };
  const writeLastRepo = (id: string): void => {
    try {
      localStorage.setItem(LAST_REPO_KEY, id);
    } catch {
      // Private mode / storage disabled — the in-memory signal still works.
    }
  };

  const [selectedId, setSelectedId] = createSignal<string | null>(readLastRepo());
  const readyRepos = () => (resourceValue(repos) ?? []).filter((r) => r.clone_status === 'ready');
  // The effective repo: the stored/picked id when it still exists AND is ready,
  // otherwise the first ready repo (else null when nothing is ready).
  const selectedRepo = (): Repo | null => {
    const list = resourceValue(repos) ?? [];
    const id = selectedId();
    const picked = id !== null ? list.find((r) => r.id === id) : undefined;
    if (picked && picked.clone_status === 'ready') return picked;
    return readyRepos()[0] ?? null;
  };
  const pickRepo = (repo: Repo): void => {
    setSelectedId(repo.id);
    writeLastRepo(repo.id);
  };
  // Ready-only selection, as before: disabled rows can't be clicked, but a
  // stale commit (e.g. the synthesized empty row) must never strand the pick.
  const pickRepoById = (id: string): void => {
    const repo = (resourceValue(repos) ?? []).find((r) => r.id === id);
    if (repo !== undefined && repo.clone_status === 'ready') pickRepo(repo);
  };
  // Options for the repo Select: every repo listed; non-ready rows disabled
  // with their status text (live clone % from the cloneProgress store).
  const repoOptions = (): SelectOption[] =>
    (resourceValue(repos) ?? []).map((repo) => ({
      value: repo.id,
      label: repo.name,
      disabled: repo.clone_status !== 'ready',
      status: repoStatus(repo, progress.progress(repo.id)),
    }));

  const providerList = () => resourceValue(providers) ?? [];
  const defaultsValue = () => resourceValue(defaults) ?? {};

  // Per-spawn provider pick ('' = no pick). Ephemeral by design (ADR-0030): it
  // resets on repo change (below) and on page load; the repo override and the
  // global default are the durable levers.
  const [providerPick, setProviderPick] = createSignal('');
  createEffect(
    on(
      () => selectedRepo()?.id,
      () => setProviderPick(''),
      { defer: true },
    ),
  );

  // The EFFECTIVE provider: per-spawn pick → repo override → global default →
  // first registered provider (skip-layer, mirroring the backend resolver).
  const provider = () => {
    const repo = selectedRepo();
    if (repo === null) return null;
    return providerFor(providerList(), providerPick(), repo.provider, defaultsValue().provider);
  };
  const models = () => provider()?.models ?? [];
  const efforts = () => provider()?.efforts ?? [];
  const providerOptions = (): SelectOption[] =>
    providerList().map((p) => ({ value: p.id, label: p.display_name }));

  // Machine-level auth for the EFFECTIVE provider: the status route is
  // per-provider-id (issue #51 decision 7), so the resource keys on the
  // effective provider id and refetches on the provider-generic SSE event.
  // Copy comes from the provider's display_name — never a hardcoded agent name.
  const [authStatus] = createLiveResource(
    () => provider()?.id,
    (id) => providerAuthStatus(id),
    [{ type: 'provider.auth.changed' }],
  );
  const providerName = () => provider()?.display_name ?? 'The provider';

  // '' = untouched → submit the resolved default. Tracking the operator's pick
  // separately keeps a late providers/settings load from clobbering it.
  const [modelPick, setModelPick] = createSignal('');
  const [effortPick, setEffortPick] = createSignal('');
  // A pick belongs to the catalog it was made from: when the EFFECTIVE
  // provider changes (a provider pick, a repo switch, a late defaults load),
  // stale model/effort picks reset so a foreign value can never 400 a spawn.
  createEffect(
    on(
      () => provider()?.id,
      () => {
        setModelPick('');
        setEffortPick('');
      },
      { defer: true },
    ),
  );
  const model = () =>
    modelPick() !== ''
      ? modelPick()
      : resolveSpawnOption(models(), selectedRepo()?.model_default, defaultsValue().model);
  const effort = () =>
    effortPick() !== ''
      ? effortPick()
      : resolveSpawnOption(efforts(), selectedRepo()?.effort_default, defaultsValue().effort);

  const [label, setLabel] = createSignal('');
  const [text, setText] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  // The `…` options popover. The Select chips manage their own open state and
  // close this one via onOpen (one popover open at a time, as before).
  const [pop, setPop] = createSignal<'more' | null>(null);

  const loggedOut = () => resourceValue(authStatus)?.logged_in === false;

  // Escape closes any open popover.
  const onKeyDownGlobal = (e: KeyboardEvent): void => {
    if (e.key === 'Escape' && pop() !== null) setPop(null);
  };
  window.addEventListener('keydown', onKeyDownGlobal);
  onCleanup(() => window.removeEventListener('keydown', onKeyDownGlobal));

  // Outside-click closes the `…` popover: a document listener mounted only
  // while it is open. A click inside the .composer-pop wrapper is left to the
  // popover's own handlers (its trigger toggles); anything else closes it —
  // including clicks on the Select chips, which is exactly the old
  // one-popover-at-a-time behavior. Mounted after the opening click, so it
  // never self-closes.
  createEffect(() => {
    if (pop() === null) return;
    const onDocMouseDown = (e: MouseEvent): void => {
      const target = e.target as Element | null;
      if (target === null || target.closest('.composer-pop') === null) setPop(null);
    };
    document.addEventListener('mousedown', onDocMouseDown);
    onCleanup(() => document.removeEventListener('mousedown', onDocMouseDown));
  });

  // Auto-grow the textarea one row → content height (chat-composer decision 9b);
  // JS sets the exact height inline, CSS max-height clamps + scrolls. jsdom has
  // no layout (scrollHeight 0) so this is a harmless no-op there.
  let inputEl: HTMLTextAreaElement | undefined;
  const autoGrow = (): void => {
    const el = inputEl;
    if (el === undefined) return;
    el.style.height = 'auto';
    el.style.height = `${el.scrollHeight}px`;
  };
  createEffect(() => {
    text();
    autoGrow();
  });

  const canSend = () => selectedRepo() !== null && !busy();

  const send = async (): Promise<void> => {
    const repo = selectedRepo();
    if (repo === null || busy()) return;
    setBusy(true);
    setError(null);
    try {
      const req: StartInstanceRequest = {};
      const trimmedLabel = label().trim();
      if (trimmedLabel !== '') req.label = trimmedLabel;
      // Only an EXPLICIT per-spawn pick rides along — the resolved default
      // chain is the server's to walk (and validate) itself.
      if (providerPick() !== '') req.provider = providerPick();
      if (model() !== '') req.model = model();
      if (effort() !== '') req.effort = effort();
      const run = await startInstance(repo.id, req);
      // Only a SUCCESSFUL spawn queues the typed text (issue #41): the chat view
      // auto-sends it once the transcript is ready. Empty text = a plain spawn.
      const body = text().trim();
      if (body !== '') setQueued(run.id, body);
      navigate('/runs/' + run.id);
    } catch (err) {
      // 409 (cap / provider logged out / repo not ready) et al. surface
      // verbatim; the composer text STAYS (we never cleared it) so nothing is
      // lost.
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // Cmd/Ctrl+Enter sends; bare Enter is a newline (matches the chat composer).
  const onKeyDown = (e: KeyboardEvent): void => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void send();
    }
  };

  const noRepos = () => {
    const value = resourceValue(repos);
    return value !== undefined && value.length === 0;
  };
  const hasRepos = () => (resourceValue(repos)?.length ?? 0) > 0;

  return (
    <main class="page newrun">
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={loggedOut()}>
        <p class="newrun-warn" role="alert">
          {providerName()} is logged out — <A href="/credentials">reconnect</A>.
        </p>
      </Show>

      <Switch>
        <Match when={repos.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(repos.error)}</span>
          </div>
        </Match>
        {/* Zero repos: the composer is hidden and the empty state carries the
            "No repositories yet" text the login/setup round-trip and the
            Playwright smoke assert on `/`. */}
        <Match when={noRepos()}>
          <p class="empty">
            No repositories yet — <A href="/repos/new">add one</A> to get started.
          </p>
        </Match>
        <Match when={hasRepos()}>
          <div class="composer">
            <div classList={{ 'composer-field': true, disabled: selectedRepo() === null }}>
              <textarea
                ref={(el) => {
                  inputEl = el;
                  queueMicrotask(autoGrow);
                }}
                class="composer-input"
                rows={1}
                placeholder="Describe a task to start a new run…"
                value={text()}
                onInput={(e) => setText(e.currentTarget.value)}
                onKeyDown={onKeyDown}
                disabled={selectedRepo() === null}
              />
              <div class="composer-bar">
                <Select
                  skin="chip"
                  label="Repository"
                  icon={<Icon name="folder" size={15} class="composer-chip-icon" />}
                  value={selectedRepo()?.id ?? ''}
                  options={repoOptions()}
                  onChange={pickRepoById}
                  onOpen={() => setPop(null)}
                />
                {/* The agent chip exists only when there is a real choice —
                    with a single registered provider the composer stays
                    exactly as before (ADR-0030). */}
                <Show when={providerList().length >= 2}>
                  <Select
                    skin="chip"
                    label="Agent"
                    value={provider()?.id ?? ''}
                    options={providerOptions()}
                    onChange={setProviderPick}
                    onOpen={() => setPop(null)}
                  />
                </Show>
                <Select
                  skin="chip"
                  label="Model"
                  value={model()}
                  options={models()}
                  disabled={models().length === 0}
                  onChange={setModelPick}
                  onOpen={() => setPop(null)}
                />
                {/* No efforts catalog = the provider has no effort knob at
                    all — hide the chip rather than pin a disabled control. */}
                <Show when={efforts().length > 0}>
                  <Select
                    skin="chip"
                    label="Effort"
                    value={effort()}
                    options={efforts()}
                    onChange={setEffortPick}
                    onOpen={() => setPop(null)}
                  />
                </Show>
                <MoreChip
                  label={label()}
                  onLabel={setLabel}
                  open={pop() === 'more'}
                  onToggle={() => setPop((p) => (p === 'more' ? null : 'more'))}
                />
                <span class="spacer" />
                <button
                  type="button"
                  class="composer-send icon-btn"
                  classList={{ busy: busy() }}
                  aria-label="Start run"
                  title="Start run (Cmd/Ctrl+Enter)"
                  disabled={!canSend()}
                  onClick={() => void send()}
                >
                  <Icon name="send" />
                </button>
              </div>
            </div>

            <Show when={selectedRepo()}>
              {(repo) => (
                <AFKStrip
                  repo={repo()}
                  onRepoChanged={() => void refetchRepos()}
                  onStarted={(run) => toast.show(afkStartedMessage(run))}
                  onError={setError}
                />
              )}
            </Show>
          </div>
        </Match>
      </Switch>
      {toast.Toast()}
    </main>
  );
}

/** Toast copy for an AFK start — the claimed issue is the run's issue_number. */
function afkStartedMessage(run: Run): string {
  return run.issue_number !== null ? `AFK run started on #${run.issue_number}` : 'AFK run started';
}

// Status text for a non-ready repo row: cloning/error repos stay visible in
// the picker but disabled, showing their state (+ the live clone % from the
// cloneProgress store while cloning).
function repoStatus(repo: Repo, progress: CloneProgress | null): string | undefined {
  if (repo.clone_status === 'error') return 'clone failed';
  if (repo.clone_status === 'cloning') {
    const percent = progress?.percent;
    return percent !== null && percent !== undefined ? `cloning ${percent}%` : 'cloning…';
  }
  return undefined;
}

// The `…` popover: the optional label only (≤32 chars). Manual spawn accepts no
// provider-options bag, so nothing else belongs here (issue #21 stays open).
function MoreChip(props: {
  label: string;
  onLabel: (value: string) => void;
  open: boolean;
  onToggle: () => void;
}) {
  return (
    <div class="composer-pop">
      <button
        type="button"
        class="composer-chip more-chip icon-btn"
        aria-haspopup="dialog"
        aria-expanded={props.open}
        aria-label="More options"
        title="More options"
        onClick={() => props.onToggle()}
      >
        <Icon name="more-horizontal" size={16} />
      </button>
      <Show when={props.open}>
        <div class="composer-pop-panel more-pop" role="dialog" aria-label="Run options">
          <label class="field composer-label-field">
            <span>Label (optional)</span>
            <input
              name="label"
              maxlength="32"
              value={props.label}
              onInput={(e) => props.onLabel(e.currentTarget.value)}
              placeholder="debug"
              autocomplete="off"
            />
          </label>
        </div>
      </Show>
    </div>
  );
}
