// New run (`/new`; it was Home at `/` until issue #76 made Runs the home and
// gave the composer its own URL) — issue #66, design direction A ("Sheet",
// revision 2; reference docs/reference/new-run-mockup.html). Below 1024px the
// page opens with a "New run" header (the New tab's root, issue #76; the
// desktop centered column has none — the rail's `+ New run` names it) whose
// subtitle names the selected repository (issue #87). Otherwise the page is
// three things and nothing else: the repository pills (RepoPills — recent
// repos plus "All N", which opens the repository picker, never the
// Repositories page), the composer, and the Issues card of the selected repo
// (IssuesCard — its AFK line replaces the old AFK strip; tapping an issue
// attaches an action to the composer). Status shows only where it blocks a
// run: ComposerBlockers, right above the field. Never auto-navigates on load;
// the only navigations are a sent run (→ its chat), a blocker's remedy and the
// Runner settings link.
//
// The composer keeps the Chat's dock shape: below 1024px it is docked at the
// bottom edge, so the field is where the Chat's composer will be a second
// later, and the pills ride in the dock as its first row (issue #87) — the
// repo is picked where the thumb already is, right above the blockers and the
// field. From 1024px the pills sit above the composer in a centered 720px
// column and the document scrolls. The DOM follows each layout's reading
// order: desktop is pills, composer, Issues, so keyboard focus follows what is
// seen where Tab is used most; on the phone the dock (pills inside) precedes
// the Issues card and CSS `order` moves it last (styles/newrun.css). One
// RepoPills serves both: the same node moves between the two spots on a
// breakpoint crossing (see the hasRepos branch below).
//
// Below 1024px the docked page is BOUNDED like the Chat (issue #97, ADR-0083):
// a flex column as tall as the visual viewport less the tab bar's room (the
// bar slides out while the keyboard is open here, and that room goes to 0),
// with the Issues card region as its one scroll container and the dock as its
// last flex item, so the document never scrolls and the dock rides the
// keyboard's top edge. iOS never resizes the layout viewport for the
// keyboard, so the page binds window.visualViewport onto itself
// (bindVisualViewport, the Chat's primitive: --vv-height / --vv-top, which
// only the phone CSS reads); without visualViewport or while pinch-zoomed the
// page is 100dvh less the bar, at top 0.
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
// #163's tri-state for remote control). Picks reset with the repo and never
// outlive the spawn; only explicit agent/remote picks ride the request. Manual
// spawn has no provider-options bag (internal/httpapi/instances.go), so issue
// #21 stays open.
//
// The unsent composition — text, label, repo, picks, the attachment — is a
// draft in localStorage (issue #92, lib/drafts): restored on open (stale
// picks dropped against what loads), saved on every change, cleared when the
// run starts. It is never a default for the next run (ADR-0030).

import { A, useNavigate } from '@solidjs/router';
import {
  Match,
  Show,
  Switch,
  createEffect,
  createMemo,
  createResource,
  createSignal,
  on,
  onCleanup,
  onMount,
  untrack,
  type Accessor,
  type JSX,
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
import SectionHead from '../components/SectionHead';
import { createToast } from '../components/Toast';
import { useEvents } from '../events';
import { isComposerSend } from '../lib/composerKeys';
import { NEW_RUN_DRAFT_KEY, clearDraft, readDraft, writeDraft } from '../lib/drafts';
import { createLiveResource } from '../lib/liveResource';
import { createMediaQuery } from '../lib/media';
import {
  EMPTY_NEW_RUN_DRAFT,
  RECENT_REPOS_PILLS,
  attachmentText,
  composeFirstMessage,
  composerBlockers,
  composerPlaceholder,
  fieldDisabled,
  isEmptyNewRunDraft,
  isStartable,
  parseNewRunDraft,
  preselectedRepo,
  pushRecentRepo,
  readRecentRepos,
  recentRepos,
  runLabelFor,
  sendLabel,
  writeRecentRepos,
  type AttachmentRef,
  type IssueAction,
  type NewRunAttachment,
  type NewRunDraft,
} from '../lib/newRun';
import { resourceValue } from '../lib/resource';
import { providerFor, resolveEffortOption, resolveRemote, resolveSpawnOption } from '../lib/spawn';
import { bindVisualViewport } from '../lib/visualViewport';
import { createCloneProgressStore } from '../stores/cloneProgress';

export default function NewRun() {
  return (
    <RequireAuth>
      <NewRunView />
    </RequireAuth>
  );
}

/** The shell breakpoint (AppShell's DESKTOP_MIN_PX): from here the rail names the page. */
const DESKTOP_QUERY = '(min-width: 1024px)';

function NewRunView() {
  const events = useEvents();
  const navigate = useNavigate();
  const desktop = createMediaQuery(DESKTOP_QUERY);
  // Where the one pills row sits (issue #87): before the dock from 1024px, in
  // it below. Follows `desktop`, but hands the node over in two steps — see
  // createHandoff — so a breakpoint crossing moves it instead of losing it.
  const pillsSpot = createHandoff(() => (desktop() ? 'top' : 'dock'));

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

  // The unsent composition (issue #92), read once: it seeds the signals
  // below, every change writes it back (see "The draft"), a started run
  // clears it. No draft, or no storage, is the untouched page.
  const draft = readDraft(NEW_RUN_DRAFT_KEY, parseNewRunDraft) ?? EMPTY_NEW_RUN_DRAFT;
  // The restore window. repos, providers and defaults land independently,
  // and each landing can move the selected repo or the effective provider
  // (undefined → the first real id; the first registered provider → the
  // defaults' one when defaults land last). Neither is an operator's switch,
  // but either would fire the resets below and wipe the restored picks. So
  // the page is restoring until all three have settled (resolved or errored):
  // the resets hold off, the restored picks are then checked ONCE against
  // what loaded (dropStalePicks), and only an id change after that resets.
  const [restored, setRestored] = createSignal(false);

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
  // null = nothing picked on this visit (or in its draft): the most recent
  // usable repo.
  const [pickedId, setPickedId] = createSignal<string | null>(draft.pickedId);

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
    // The reset effect holds off during the restore; a switch made in it is
    // still the operator's own, so it resets here.
    if (!restored() && repo.id !== selectedRepo()?.id) resetRepoPicks();
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
  // resets on repo change (below) and never outlives the spawn — an unsent
  // draft restores it on page load (issue #92), a started run clears it. The
  // repo override and the global default are the durable levers.
  const [providerPick, setProviderPick] = createSignal(draft.providerPick);
  // Per-spawn remote-control pick (issue #163). null = untouched — NOT false:
  // `false` is a real pick here (an operator turning an inherited-on default
  // off), so only null can mean "let the layers decide".
  const [remotePick, setRemotePick] = createSignal<boolean | null>(draft.remotePick);
  // The attached issue action belongs to the repo's tracker; it carries the
  // repo it was attached under, so a restore under another repo drops it.
  const [attachment, setAttachment] = createSignal<NewRunAttachment | null>(draft.attachment);
  // Picks and the attachment were made against one repo: another repo
  // (a pick, or the selected one disappearing) starts clean.
  const resetRepoPicks = (): void => {
    setProviderPick('');
    setRemotePick(null);
    setAttachment(null);
  };
  // The id is a memo on purpose: `on` re-runs its callback whenever anything
  // it reads changes, and selectedRepo() reads the repos resource, which the
  // repo.changed subscription refetches (a readiness verdict, an AFK sweep).
  // Every refetch is a new array, so keyed on the raw accessor the resets
  // fired while the operator was typing and threw their picks away. The memo
  // only notifies when the id itself differs.
  const selectedRepoId = createMemo(() => selectedRepo()?.id);
  // The reset keys on the id once the restore is over (undefined until then),
  // and skips the step from undefined: what lands during the restore, and the
  // restore's own end, are not switches.
  const settledRepoId = createMemo(() => (restored() ? selectedRepoId() : undefined));
  createEffect(
    on(
      settledRepoId,
      (_, prev) => {
        if (prev !== undefined) resetRepoPicks();
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
    const before = provider()?.id;
    setProviderPick(id === inheritedProvider()?.id ? '' : id);
    // As in pickRepo: the operator's own switch during the restore resets at once.
    if (!restored() && provider()?.id !== before) resetCatalogPicks();
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
  const [modelPick, setModelPick] = createSignal(draft.modelPick);
  const [effortPick, setEffortPick] = createSignal(draft.effortPick);
  // A pick belongs to the catalog it was made from: when the EFFECTIVE
  // provider changes (a provider pick, a repo switch, a late defaults load
  // after the restore), stale model/effort picks reset so a foreign value can
  // never 400 a spawn. During the restore the check is dropStalePicks'.
  const resetCatalogPicks = (): void => {
    setModelPick('');
    setEffortPick('');
  };
  // Memoized for the same reason as selectedRepoId: provider() reads the
  // repos resource, and a refetch of the same repo must not count as a change.
  // Settled like settledRepoId.
  const providerId = createMemo(() => provider()?.id);
  const settledProviderId = createMemo(() => (restored() ? providerId() : undefined));
  createEffect(
    on(
      settledProviderId,
      (_, prev) => {
        if (prev !== undefined) resetCatalogPicks();
      },
      { defer: true },
    ),
  );
  // The inherited model: the resolution without the per-spawn pick.
  const modelDefault = () =>
    resolveSpawnOption(models(), selectedRepo()?.model_default, defaultsValue().model);
  // A memo, so the effort reset below keys on the resolved value, not on
  // every repos refetch modelDefault() reads through.
  const model = createMemo(() => (modelPick() !== '' ? modelPick() : modelDefault()));
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
  // Held off during the restore like the resets (dropStalePicks checks then).
  createEffect(
    on(
      model,
      () => {
        if (!restored()) return;
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

  const [label, setLabel] = createSignal(draft.label);
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
  const [text, setText] = createSignal(draft.text);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  // --- The draft (issue #92) ---

  // The end of the restore window: once repos, providers and defaults have
  // all settled, each restored pick is checked against what loaded and a
  // stale one dropped on its own, so the rest of the restore stands. An
  // errored resource offers nothing, so the picks it would vouch for go.
  const loaded = (r: { state: string }): boolean =>
    r.state !== 'unresolved' && r.state !== 'pending';
  const dropStalePicks = (): void => {
    const id = pickedId();
    // A gone repo: selectedRepo() already fell back to the preselection.
    if (id !== null && !repoList().some((r) => r.id === id)) setPickedId(null);
    const att = attachment();
    if (att !== null && att.repoId !== selectedRepo()?.id) setAttachment(null);
    const pick = providerPick();
    if (pick !== '' && !providerList().some((p) => p.id === pick)) setProviderPick('');
    // The catalogs of the provider that resolved with the surviving picks.
    const picked = modelPick();
    if (picked !== '' && !models().some((m) => m.value === picked)) setModelPick('');
    const resolved = models().find((m) => m.value === (modelPick() || modelDefault()));
    const effortValue = effortPick();
    if (effortValue !== '' && !(resolved?.efforts ?? []).some((o) => o.value === effortValue))
      setEffortPick('');
  };
  createEffect(() => {
    if (restored() || ![repos, providers, defaults].every(loaded)) return;
    untrack(dropStalePicks);
    setRestored(true);
  });

  // Every change writes the composition back; the untouched one clears the
  // entry. Once a run started, its cleared draft stays cleared: nothing may
  // write it back while the page is on its way out.
  let started = false;
  createEffect(() => {
    const value: NewRunDraft = {
      text: text(),
      label: label(),
      pickedId: pickedId(),
      providerPick: providerPick(),
      modelPick: modelPick(),
      effortPick: effortPick(),
      remotePick: remotePick(),
      attachment: attachment(),
    };
    if (started) return;
    if (isEmptyNewRunDraft(value)) clearDraft(NEW_RUN_DRAFT_KEY);
    else writeDraft(NEW_RUN_DRAFT_KEY, value);
  });

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
    setAttachment({ action, issue, repoId: selectedRepo()?.id ?? '' });
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
      // The composition left with the run: its draft goes (issue #92), so the
      // next visit opens clean. A failure keeps it, like everything else.
      started = true;
      clearDraft(NEW_RUN_DRAFT_KEY);
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

  // Size the page to the visible area above the on-screen keyboard (issue
  // #97, as RunChat does since issue #82): --vv-height / --vv-top on the
  // page, a no-op without visualViewport. Bound for the page's whole life —
  // the vars are read only by the phone's bounded `.newrun-docked` rules, so
  // the error and empty states, and the desktop column, ignore them. onMount:
  // the ref is set by then, and the binding's listeners and effect belong to
  // this component and go with it.
  let pageEl: HTMLElement | undefined;
  onMount(() => {
    if (pageEl !== undefined) bindVisualViewport(pageEl);
  });

  return (
    <main ref={pageEl} class="page newrun" classList={{ 'newrun-docked': hasRepos() }}>
      {/* The phone page header (issue #76): the New tab's root names itself,
          like Runs, Repos and More. Desktop keeps the bare centered column.
          Its subtitle is the selected repository (issue #87): with the pills
          down in the dock, the top of the page still says where a run goes. */}
      <Show when={!desktop()}>
        <div class="newrun-head">
          <SectionHead title="New run" subtitle={selectedRepo()?.name} />
        </div>
      </Show>
      <Switch>
        <Match when={repos.error !== undefined}>
          <Banner message={errorMessage(repos.error)} />
        </Match>
        {/* Zero repos: the composer is hidden and the empty state carries the
            "No repositories yet" text the login/setup round-trip and the
            Playwright smoke assert on. */}
        <Match when={noRepos()}>
          <EmptyState>
            No repositories yet — <A href="/repos/new">add one</A> to get started.
          </EmptyState>
        </Match>
        <Match when={hasRepos()}>
          {/* The pills row is built once for this branch (and disposed with
              it) and placed by breakpoint (issue #87): before the dock from
              1024px, as the dock's first row below it. The two spots are
              exclusive and hold the SAME node — never a second RepoPills —
              so a breakpoint crossing moves it (inserting an attached node
              re-parents it) and its state, such as an open picker, survives. */}
          <BuildOnce
            node={() => (
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
            )}
          >
            {(pillsRow) => (
              <>
                <Show when={pillsSpot() === 'top'}>{pillsRow}</Show>

                <div class="newrun-dock">
                  <Show when={pillsSpot() === 'dock'}>{pillsRow}</Show>
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
                        // Also fits a box that mounts with restored draft text.
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
                          defaultSource={defaultSource(
                            selectedRepo()?.model_default,
                            modelDefault(),
                          )}
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
                            defaultSource={defaultSource(
                              selectedRepo()?.effort_default,
                              effortDefault(),
                            )}
                            onPick={pickEffort}
                            disabled={chipsDisabled()}
                          />
                        </Show>
                        <MoreOptions
                          providers={providerList().map((p) => ({
                            id: p.id,
                            label: p.display_name,
                          }))}
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
                          changed={
                            providerPick() !== '' || remoteSetHere() || label().trim() !== ''
                          }
                          disabled={chipsDisabled()}
                        />
                      </div>
                      <button
                        type="button"
                        class="composer-send icon-btn"
                        classList={{ busy: busy() }}
                        aria-label={sendLabel(attachmentRef())}
                        title={
                          attachment() === null ? 'Start run (Enter)' : sendLabel(attachmentRef())
                        }
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
                      nothing read for the previous repo (its issue list, an
                      open picker or sheet) can show under the new one. A
                      refetch of the same repo keeps the card. */}
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
              </>
            )}
          </BuildOnce>
        </Match>
      </Switch>
      {toast.Toast()}
    </main>
  );
}

/**
 * Calls `node` once — under this component's owner, so whatever it builds is
 * disposed with it — and hands that one result to `children` (issue #87). The
 * render function may place it in several spots, as long as they are mutually
 * exclusive (opposite `Show`s): a DOM node can only be in one place, and
 * inserting it into the newly shown spot moves it out of the other. Both
 * calls are deliberately untracked: this runs once, like any component body,
 * and reactivity lives inside what `node` and `children` build.
 */
function BuildOnce(props: {
  node: () => JSX.Element;
  children: (node: JSX.Element) => JSX.Element;
}): JSX.Element {
  return untrack(() => props.children(props.node()));
}

/**
 * A spot for one shared node that releases before it acquires (issue #87).
 * Two `Show`s keyed straight on one signal would swap in a single flush, in
 * whatever order Solid runs their inserts — and a releasing insert cleans up
 * in its node's CURRENT parent, so when the acquiring spot ran first, the
 * release then pulled the freshly moved node out of its new home. Here a
 * change of `target` first empties the spot (null) and only then names the
 * new one; written from a microtask, outside any update, each write flushes on
 * its own, so the release has finished before the acquire starts. Both land
 * before the next paint, so the row never visibly blinks out.
 */
function createHandoff<T extends string>(target: () => T): Accessor<T | null> {
  const [spot, setSpot] = createSignal<T | null>(untrack(target));
  let disposed = false;
  onCleanup(() => (disposed = true));
  createEffect(
    on(
      target,
      () =>
        queueMicrotask(() => {
          if (disposed) return;
          setSpot(null);
          setSpot(() => untrack(target));
        }),
      { defer: true },
    ),
  );
  return spot;
}

/** Toast copy for an AFK start — the claimed issue is the run's issue_number. */
function afkStartedMessage(run: Run): string {
  return run.issue_number !== null ? `AFK run started on #${run.issue_number}` : 'AFK run started';
}
