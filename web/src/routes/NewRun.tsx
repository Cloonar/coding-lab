// New run (Home, `/`) — issue #66, design direction A ("Sheet", revision 2;
// reference docs/reference/new-run-mockup.html). The page is three things and
// nothing else: the repository pills (RepoPills — recent repos plus "All N",
// which opens the repository picker, never the Repositories page), the
// composer, and the Issues card of the selected repo (IssuesCard — its AFK
// line replaces the old AFK strip; tapping an issue attaches an action to the
// composer). Status shows only where it blocks a run: ComposerBlockers, right
// above the field. Never auto-navigates on load; the only navigations are a
// sent run (→ its chat), a blocker's remedy and the Runner settings link.
//
// The composer keeps the Chat's dock shape: below 1024px it is docked at the
// bottom edge (sticky, safe-area inset), so the field is where the Chat's
// composer will be a second later; from 1024px it sits under the pills in a
// centered 720px column. One DOM order serves both (pills, composer, Issues):
// it is the desktop visual order, so keyboard focus follows what is seen
// where Tab is used most, and on the phone CSS `order` moves the dock last
// (styles/newrun.css).
//
// The field: an optional attached issue action (AttachmentChip), the
// autogrowing textarea, and a bar with the run-option chips — Model, Effort
// (hidden when the model has no efforts), ⋯ More options (agent, remote
// control, label, the Runner) — scrolling sideways on a phone, and the accent
// circular send. Send spawns an instance carrying the first_message (issue
// #96: the attached action's line plus any typed text, else the typed text)
// and navigates to the chat.
//
// Resolution mirrors the server: per-spawn pick → repo override → global
// default (ADR-0030 for the agent; issue #156 for per-model efforts; issue
// #163's tri-state for remote control). Picks are ephemeral and reset with the
// repo; only explicit agent/remote picks ride the request. Manual spawn has no
// provider-options bag (internal/httpapi/instances.go), so issue #21 stays open.

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
  retryClone,
  startInstance,
  type IssueSummary,
  type Repo,
  type Run,
  type StartInstanceRequest,
} from '../api';
import Banner from '../components/Banner';
import EmptyState from '../components/EmptyState';
import Icon from '../components/Icon';
import ComposerBlockers from '../components/newrun/ComposerBlockers';
import IssuesCard from '../components/newrun/IssuesCard';
import RepoPills from '../components/newrun/RepoPills';
import {
  AttachmentChip,
  ChoiceChip,
  MoreOptions,
  type ChoiceOption,
} from '../components/newrun/RunOptions';
import RequireAuth from '../components/RequireAuth';
import { createToast } from '../components/Toast';
import { useEvents } from '../events';
import { isComposerSend } from '../lib/composerKeys';
import { createLiveResource } from '../lib/liveResource';
import {
  RECENT_REPOS_PILLS,
  attachmentText,
  composeFirstMessage,
  composerBlockers,
  composerPlaceholder,
  fieldDisabled,
  isStartable,
  preselectedRepo,
  pushRecentRepo,
  readRecentRepos,
  recentRepos,
  runLabelFor,
  sendLabel,
  writeRecentRepos,
  type AttachmentRef,
  type IssueAction,
} from '../lib/newRun';
import { resourceValue } from '../lib/resource';
import { providerFor, resolveEffortOption, resolveRemote, resolveSpawnOption } from '../lib/spawn';
import { createCloneProgressStore } from '../stores/cloneProgress';

/** The issue action attached to the composer. */
interface Attachment {
  action: IssueAction;
  issue: IssueSummary;
}

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

  // repo.changed keeps clone_status and the readiness summary fresh, so a
  // cloning repo becomes startable (and its banner goes) the moment it lands.
  const [repos, { refetch: refetchRepos }] = createLiveResource(
    () => listRepos(),
    [{ type: 'repo.changed' }],
  );
  const [providers] = createResource(() => listProviders());
  const [defaults] = createResource(() => getSpawnDefaults());
  const progress = createCloneProgressStore(events);
  const toast = createToast();
  onCleanup(progress.dispose);

  const repoList = (): Repo[] => resourceValue(repos) ?? [];
  // Awaitable, so the AFK controls and the clone Retry stay busy until the
  // fresh list is in.
  const reloadRepos = async (): Promise<void> => {
    await refetchRepos();
  };

  // --- Repository selection ---

  // The stored recent list (lab.last-repo, most recent first): the picker's
  // Recent group, updated on every pick. The pill row is read from the list
  // as it was when the page opened (plus repos picked here since), so a tap
  // on a pill never reorders the row under the operator's thumb.
  const openedWith = readRecentRepos();
  const [recentIds, setRecentIds] = createSignal<string[]>(openedWith);
  const [pickedHere, setPickedHere] = createSignal<string[]>([]);
  // null = nothing picked on this visit: the most recent usable repo.
  const [pickedId, setPickedId] = createSignal<string | null>(null);

  // The picked repo while it still exists (even if it went un-startable, so
  // its banner shows), else the preselection — which falls back to a
  // non-startable repo when none can start, so the composer can say why.
  const selectedRepo = (): Repo | null => {
    const list = repoList();
    const id = pickedId();
    const picked = id !== null ? list.find((r) => r.id === id) : undefined;
    return picked ?? preselectedRepo(list, recentIds());
  };

  const pills = (): Repo[] => {
    const list = repoList();
    const out: Repo[] = [];
    const add = (repo: Repo | undefined): void => {
      if (repo !== undefined && !out.includes(repo)) out.push(repo);
    };
    for (const id of pickedHere()) add(list.find((r) => r.id === id));
    for (const repo of recentRepos(list, openedWith)) add(repo);
    const selected = selectedRepo();
    if (selected !== null && !out.includes(selected)) out.unshift(selected);
    return out.slice(0, RECENT_REPOS_PILLS);
  };

  const pickRepo = (repo: Repo): void => {
    if (!pills().includes(repo))
      setPickedHere((ids) => [repo.id, ...ids.filter((x) => x !== repo.id)]);
    setPickedId(repo.id);
    // Only a repo a run can start in is remembered (the issue: "startable
    // repos only"); the picker never offers another, but a pill may be one.
    if (isStartable(repo)) {
      const next = pushRecentRepo(recentIds(), repo.id);
      setRecentIds(next);
      writeRecentRepos(next);
    }
  };

  const providerList = () => resourceValue(providers) ?? [];
  const defaultsValue = () => resourceValue(defaults) ?? {};

  // Per-spawn provider pick ('' = no pick). Ephemeral by design (ADR-0030): it
  // resets on repo change (below) and on page load; the repo override and the
  // global default are the durable levers.
  const [providerPick, setProviderPick] = createSignal('');
  // Per-spawn remote-control pick (issue #163). null = untouched — NOT false:
  // `false` is a real pick here (an operator turning an inherited-on default
  // off), so only null can mean "let the layers decide".
  const [remotePick, setRemotePick] = createSignal<boolean | null>(null);
  // The attached issue action belongs to the repo's tracker.
  const [attachment, setAttachment] = createSignal<Attachment | null>(null);
  // Picks and the attachment were made against one repo: another repo
  // (a pick, or the selected one disappearing) starts clean.
  createEffect(
    on(
      () => selectedRepo()?.id,
      () => {
        setProviderPick('');
        setRemotePick(null);
        setAttachment(null);
      },
      { defer: true },
    ),
  );

  // --- Agent, model, effort ---

  // The INHERITED provider (repo override → global default → first
  // registered) and the EFFECTIVE one (the per-spawn pick first), skip-layer
  // like the backend resolver.
  const inheritedProvider = () => {
    const repo = selectedRepo();
    if (repo === null) return null;
    return providerFor(providerList(), repo.provider, defaultsValue().provider);
  };
  const provider = () => {
    const repo = selectedRepo();
    if (repo === null) return null;
    return providerFor(providerList(), providerPick(), repo.provider, defaultsValue().provider);
  };
  const models = () => provider()?.models ?? [];
  // Picking the inherited agent is no pick: nothing to send, no accent.
  const pickProvider = (id: string): void => {
    setProviderPick(id === inheritedProvider()?.id ? '' : id);
  };

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
  const loggedOut = () => resourceValue(authStatus)?.logged_in === false;

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
  // The inherited model: the resolution without the per-spawn pick.
  const modelDefault = () =>
    resolveSpawnOption(models(), selectedRepo()?.model_default, defaultsValue().model);
  const model = () => (modelPick() !== '' ? modelPick() : modelDefault());
  // The RESOLVED model entry — explicit pick or layered default. The composer
  // always sends the resolved effort, so the effort catalog must follow the
  // model that actually rides the spawn (issue #156): effort support varies
  // per model and codex does not clamp, so an unsupported combo would 400.
  const selectedModel = () => models().find((m) => m.value === model());
  const efforts = () => selectedModel()?.efforts ?? [];
  // When the resolved model changes and the explicit effort pick is not in
  // the new model's catalog, drop it — mirrors the server's skip-layer
  // resolution so a stale pick can never 400 a spawn. With no stored defaults
  // this displays/sends the new model's default; a still-valid pick is kept.
  createEffect(
    on(
      () => model(),
      () => {
        const pick = effortPick();
        if (pick !== '' && !efforts().some((o) => o.value === pick)) setEffortPick('');
      },
      { defer: true },
    ),
  );
  const effortDefault = () =>
    resolveEffortOption(selectedModel(), selectedRepo()?.effort_default, defaultsValue().effort);
  const effort = () => (effortPick() !== '' ? effortPick() : effortDefault());

  // Where an inherited default comes from, for the picker's hint: the repo's
  // own override when it is the one that applies, else global Settings.
  const defaultSource = (repoValue: string | null | undefined, inherited: string): string => {
    const repo = selectedRepo();
    return repo !== null && repoValue != null && repoValue !== '' && repoValue === inherited
      ? `${repo.name}'s settings`
      : 'global Settings';
  };
  const modelOptions = (): ChoiceOption[] =>
    models().map((m) => ({ value: m.value, label: m.label }));
  const effortOptions = (): ChoiceOption[] =>
    efforts().map((o) => ({ value: o.value, label: o.label }));
  // Picking the inherited default clears the pick: the chip loses its accent.
  const pickModel = (value: string): void => {
    setModelPick(value === modelDefault() ? '' : value);
  };
  const pickEffort = (value: string): void => {
    setEffortPick(value === effortDefault() ? '' : value);
  };

  // --- Remote control, label, runner ---

  // Remote control (issue #163), mirroring the server's manual chain:
  // per-spawn pick → repo.remote_default → spawn_remote_default → false.
  const resolvedRemote = () =>
    resolveRemote(selectedRepo()?.remote_default, defaultsValue().remote);
  const remote = () =>
    resolveRemote(remotePick(), selectedRepo()?.remote_default, defaultsValue().remote);
  const remoteSetHere = () => remotePick() !== null && remotePick() !== resolvedRemote();
  // A provider without the knob ignores remote control entirely (its runs are
  // clamped to off server-side): the switch is disabled and says so, named by
  // display_name — never a hardcoded brand.
  const remoteBlocker = (): string | null => {
    const p = provider();
    return p !== null && !p.supports_remote ? p.display_name : null;
  };

  const [label, setLabel] = createSignal('');
  // The effective Runner: the repo's own, else the global runner_default.
  const runner = () => {
    const repo = selectedRepo();
    if (repo === null) return null;
    return repo.runner ?? defaultsValue().runner ?? null;
  };

  // --- Blockers and the field ---

  const blockers = () => {
    const repo = selectedRepo();
    return composerBlockers({
      repo,
      progress: repo === null ? null : progress.progress(repo.id),
      loggedOut: loggedOut(),
      providerName: providerName(),
    });
  };
  const disabled = () => selectedRepo() === null || fieldDisabled(blockers());
  // The chips stay usable while only the agent's login blocks: More options
  // is where another agent is picked. A repo that cannot start yet (cloning,
  // clone failed) has nothing to configure.
  const chipsDisabled = () => {
    const repo = selectedRepo();
    return repo === null || !isStartable(repo);
  };

  const [retrying, setRetrying] = createSignal(false);
  const [text, setText] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const retry = async (): Promise<void> => {
    const repo = selectedRepo();
    if (repo === null || retrying()) return;
    setRetrying(true);
    try {
      await retryClone(repo.id);
      await reloadRepos();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setRetrying(false);
    }
  };

  const attachmentRef = (): AttachmentRef | null => {
    const att = attachment();
    return att === null ? null : { action: att.action, number: att.issue.number };
  };

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

  const attach = (action: IssueAction, issue: IssueSummary): void => {
    setAttachment({ action, issue });
    inputEl?.focus();
  };

  const canSend = () => !disabled() && !busy();

  const send = async (): Promise<void> => {
    const repo = selectedRepo();
    if (repo === null || !canSend()) return;
    setBusy(true);
    setError(null);
    try {
      const req: StartInstanceRequest = {};
      // A typed label wins; an attached action defaults it (`triage-47`).
      const runLabel = runLabelFor(label(), attachmentRef());
      if (runLabel !== '') req.label = runLabel;
      // Only an EXPLICIT per-spawn pick rides along — the resolved default
      // chain is the server's to walk (and validate) itself.
      if (providerPick() !== '') req.provider = providerPick();
      if (model() !== '') req.model = model();
      if (effort() !== '') req.effort = effort();
      // Same discipline for remote control (issue #163): only a value that
      // DIFFERS from the resolved default rides the request. A provider that
      // has no remote knob never sends the key at all.
      if (remoteBlocker() === null && remote() !== resolvedRemote()) req.remote = remote();
      // The run's first_message (issue #96), delivered on the agent's argv: the
      // attached action's line (then the typed text), else the typed text.
      // Empty = a plain spawn with no first_message.
      const att = attachment();
      const body =
        att !== null ? composeFirstMessage(att.action, att.issue, text()) : text().trim();
      if (body !== '') req.first_message = body;
      const run = await startInstance(repo.id, req);
      navigate('/runs/' + run.id);
    } catch (err) {
      // 409 (cap / provider logged out / repo not ready) et al. surface
      // verbatim; the text and the attachment STAY so nothing is lost.
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // Bare Enter sends on fine-pointer (mouse/trackpad) setups, matching the chat
  // composer via the shared gate (ADR-0031, issue #70); Cmd/Ctrl+Enter keeps
  // sending everywhere. An empty body is a valid plain spawn via Send or
  // Cmd/Ctrl+Enter, but an accidental bare Enter on an empty box must not
  // launch one — unless an issue action is attached, which is a whole request
  // on its own (the mockup). preventDefault fires even on the guarded case, so
  // the empty box never gains a leading newline (issue #70 decision 8).
  const onKeyDown = (e: KeyboardEvent): void => {
    if (!isComposerSend(e)) return;
    e.preventDefault();
    if (!(e.metaKey || e.ctrlKey) && text().trim() === '' && attachment() === null) return;
    void send();
  };

  const noRepos = () => {
    const value = resourceValue(repos);
    return value !== undefined && value.length === 0;
  };
  const hasRepos = () => (resourceValue(repos)?.length ?? 0) > 0;

  return (
    <main class="page newrun" classList={{ 'newrun-docked': hasRepos() }}>
      <Switch>
        <Match when={repos.error !== undefined}>
          <Banner message={errorMessage(repos.error)} />
        </Match>
        {/* Zero repos: the composer is hidden and the empty state carries the
            "No repositories yet" text the login/setup round-trip and the
            Playwright smoke assert on `/`. */}
        <Match when={noRepos()}>
          <EmptyState>
            No repositories yet — <A href="/repos/new">add one</A> to get started.
          </EmptyState>
        </Match>
        <Match when={hasRepos()}>
          <div class="newrun-pills">
            <RepoPills
              repos={repoList()}
              pills={pills()}
              recentIds={recentIds()}
              selectedId={selectedRepo()?.id ?? null}
              progress={progress.progress}
              onPick={pickRepo}
            />
          </div>

          <div class="newrun-dock">
            <Banner message={error()} onDismiss={() => setError(null)} />
            <ComposerBlockers
              blockers={blockers()}
              hostRunner={runner() === 'host'}
              onRetryClone={() => void retry()}
              retrying={retrying()}
            />
            <div classList={{ 'composer-field': true, disabled: disabled() }}>
              <Show when={attachment()}>
                {(att) => (
                  <AttachmentChip
                    text={attachmentText(att().action, att().issue)}
                    onRemove={() => {
                      setAttachment(null);
                      inputEl?.focus();
                    }}
                  />
                )}
              </Show>
              <textarea
                ref={(el) => {
                  inputEl = el;
                  queueMicrotask(autoGrow);
                }}
                class="composer-input"
                rows={1}
                aria-label="Task"
                placeholder={composerPlaceholder(attachmentRef(), selectedRepo()?.name ?? '')}
                value={text()}
                onInput={(e) => setText(e.currentTarget.value)}
                onKeyDown={onKeyDown}
                disabled={disabled()}
              />
              <div class="composer-bar">
                <div class="composer-chips">
                  <ChoiceChip
                    name="Model"
                    value={model()}
                    defaultValue={modelDefault()}
                    options={modelOptions()}
                    changed={modelPick() !== '' && modelPick() !== modelDefault()}
                    defaultSource={defaultSource(selectedRepo()?.model_default, modelDefault())}
                    onPick={pickModel}
                    disabled={chipsDisabled() || models().length === 0}
                  />
                  {/* No efforts catalog = the model has no effort knob at
                      all — hide the chip rather than pin a disabled control. */}
                  <Show when={efforts().length > 0}>
                    <ChoiceChip
                      name="Effort"
                      value={effort()}
                      defaultValue={effortDefault()}
                      options={effortOptions()}
                      changed={effortPick() !== '' && effortPick() !== effortDefault()}
                      defaultSource={defaultSource(selectedRepo()?.effort_default, effortDefault())}
                      onPick={pickEffort}
                      disabled={chipsDisabled()}
                    />
                  </Show>
                  <MoreOptions
                    providers={providerList().map((p) => ({ id: p.id, label: p.display_name }))}
                    providerId={provider()?.id ?? ''}
                    onProvider={pickProvider}
                    remote={remote()}
                    remoteSetHere={remoteSetHere()}
                    remoteBlocker={remoteBlocker()}
                    onRemote={setRemotePick}
                    label={label()}
                    onLabel={setLabel}
                    runner={runner()}
                    runnerInherited={selectedRepo()?.runner === null}
                    runnerHref={`/repos/${encodeURIComponent(selectedRepo()?.id ?? '')}/settings/runner`}
                    changed={providerPick() !== '' || remoteSetHere() || label().trim() !== ''}
                    disabled={chipsDisabled()}
                  />
                </div>
                <button
                  type="button"
                  class="composer-send icon-btn"
                  classList={{ busy: busy() }}
                  aria-label={sendLabel(attachmentRef())}
                  title={attachment() === null ? 'Start run (Enter)' : sendLabel(attachmentRef())}
                  disabled={!canSend()}
                  onClick={() => void send()}
                >
                  <Icon name="send" />
                </button>
              </div>
            </div>
          </div>

          <div class="newrun-issues">
            {/* Keyed on the repo id: another repo mounts a fresh card, so
                nothing read for the previous repo (its issue list, an open
                picker or sheet) can show under the new one. A refetch of the
                same repo keeps the card. */}
            <Show when={selectedRepo()?.id} keyed>
              {(id) => (
                <Show when={selectedRepo()?.id === id ? selectedRepo() : null}>
                  {(repo) => (
                    <IssuesCard
                      repo={repo()}
                      onAction={attach}
                      onRepoChanged={reloadRepos}
                      onStarted={(run) => toast.show(afkStartedMessage(run))}
                      onError={setError}
                    />
                  )}
                </Show>
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
