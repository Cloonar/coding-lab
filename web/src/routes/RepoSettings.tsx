// Repo settings: every PATCHable field per the M2 contract, grouped, saved
// as a dirty-fields-only PATCH; 400 {"error"} surfaces in the banner. Danger
// zone deletes the repo (confirm; force checkbox appears after a 409).

import { A, useNavigate, useParams } from '@solidjs/router';
import { For, Match, Show, Switch, createEffect, createResource, createSignal, on } from 'solid-js';
import type { Accessor } from 'solid-js';
import {
  ApiError,
  createRepoSecret,
  deleteRepo,
  deleteRepoSecret,
  errorMessage,
  getRepo,
  getSettings,
  listCredentials,
  listProviders,
  listRepoSecrets,
  rotateRepoSecret,
  updateRepo,
  type CredentialListItem,
  type Provider,
  type Repo,
  type RepoPatch,
  type RepoSecret,
  type Settings,
  type TrackerBinding,
} from '../api';
import Select, { type SelectOption } from '../components/Select';
import Crumbs, { type Crumb } from '../components/Crumbs';
import ErrorBanner from '../components/ErrorBanner';
import RequireAuth from '../components/RequireAuth';
import { createLiveResource } from '../lib/liveResource';
import { remoteHost } from '../lib/repoName';
import { resourceValue } from '../lib/resource';
import { providerFor } from '../lib/spawn';

export default function RepoSettings() {
  return (
    <RequireAuth>
      <RepoSettingsView />
    </RequireAuth>
  );
}

function RepoSettingsView() {
  const params = useParams<{ id: string }>();
  const [repo, { refetch }] = createLiveResource(
    () => getRepo(params.id),
    [{ type: 'repo.changed', match: (event) => event.repoID === params.id }],
  );
  const [credentials] = createResource(() => listCredentials());
  const [providers] = createResource(() => listProviders());
  // Global settings feed the effective-provider chains (provider_default /
  // spawn_provider_default_afk) the catalogs below resolve against.
  const [settings] = createResource(() => getSettings());

  // Non-throwing read: the crumb renders above the error <Match>, so a failed
  // getRepo must degrade to the placeholder name, not re-throw and blank it.
  const crumbs = (): Crumb[] => [
    { label: 'Repos', href: '/repos' },
    { label: resourceValue(repo)?.name ?? 'Repository', href: `/repos/${params.id}/issues` },
    { label: 'Settings' },
  ];

  return (
    <main class="page">
      <Crumbs segments={crumbs()} />
      <Switch>
        <Match when={repo.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(repo.error)}</span>
          </div>
        </Match>
        <Match when={repo()}>
          {(r) => (
            <>
              <div class="section-head">
                <h2>{r().name}</h2>
                <Show when={r().clone_status !== 'ready'}>
                  <span
                    classList={{
                      chip: true,
                      'status-cloning': r().clone_status === 'cloning',
                      'status-error': r().clone_status === 'error',
                    }}
                  >
                    {r().clone_status === 'cloning' ? 'cloning' : 'clone failed'}
                  </span>
                </Show>
              </div>
              <p class="muted card-sub mono">{remoteHost(r().remote_url)}</p>
              <SettingsForm
                repo={r}
                credentials={credentials() ?? []}
                providers={providers() ?? []}
                settings={settings() ?? {}}
                onSaved={refetch}
              />
              <RepoSecretsSection repoId={r().id} />
              <DangerZone repo={r} />
            </>
          )}
        </Match>
      </Switch>
    </main>
  );
}

/** '' ↔ null for nullable text columns (blank field → global default). */
function normText(value: string): string | null {
  const trimmed = value.trim();
  return trimmed === '' ? null : trimmed;
}

/** '' ↔ null for nullable integer columns. Returns undefined on garbage. */
function normInt(value: string): number | null | undefined {
  const trimmed = value.trim();
  if (trimmed === '') return null;
  const n = Number(trimmed);
  return Number.isInteger(n) && n >= 0 ? n : undefined;
}

/** Maps a repo afk_options bag (null = inherit) to per-key booleans. */
function toBoolMap(options: Record<string, string> | null): Record<string, boolean> {
  const out: Record<string, boolean> = {};
  for (const [key, value] of Object.entries(options ?? {})) out[key] = value === 'true';
  return out;
}

/** Key-order-independent equality probe for two boolean option maps. */
function optionsKey(options: Record<string, boolean>): string {
  return JSON.stringify(
    Object.fromEntries(
      Object.keys(options)
        .sort()
        .map((key) => [key, options[key]]),
    ),
  );
}

function SettingsForm(props: {
  repo: Accessor<Repo>;
  credentials: CredentialListItem[];
  providers: Provider[];
  settings: Settings;
  onSaved: () => void;
}) {
  // Drafts seed from a repo SNAPSHOT (`seed`). buildPatch diffs each draft
  // against that seed — never against the live props.repo() — so only fields
  // the operator actually edited enter the PATCH and a server-side change to
  // an untouched field (e.g. default-branch detection finishing mid-edit) is
  // never silently reverted by an unrelated save. The resync effect below
  // advances the seed on every SSE-driven refetch.
  // eslint-disable-next-line solid/reactivity -- snapshot by design; resynced via the effect below
  let seed = props.repo();
  const initial = seed;
  const [name, setName] = createSignal(initial.name);
  const [credentialId, setCredentialId] = createSignal(initial.credential_id ?? '');
  const [forgeCredentialId, setForgeCredentialId] = createSignal(initial.forge_credential_id ?? '');
  const [binding, setBinding] = createSignal<TrackerBinding>(initial.tracker_binding);
  const [defaultBranch, setDefaultBranch] = createSignal(initial.default_branch);
  const [afkPattern, setAfkPattern] = createSignal(initial.afk_branch_pattern);
  const [manualPrefix, setManualPrefix] = createSignal(initial.manual_branch_prefix);
  const [provider, setProvider] = createSignal(initial.provider ?? '');
  const [model, setModel] = createSignal(initial.model_default ?? '');
  const [effort, setEffort] = createSignal(initial.effort_default ?? '');
  const [afkProvider, setAfkProvider] = createSignal(initial.afk_provider_default ?? '');
  const [afkModel, setAfkModel] = createSignal(initial.afk_model_default ?? '');
  const [afkEffort, setAfkEffort] = createSignal(initial.afk_effort_default ?? '');
  const [afkPrompt, setAfkPrompt] = createSignal(initial.afk_prompt ?? '');

  // Effective providers, resolved LIVE against the DRAFT values (skip-layer,
  // ADR-0030) so the model/effort catalogs below re-catalog as the operator
  // flips the agent selects — before anything is saved. Stored values foreign
  // to the new catalog persist untouched and just show "(not in catalog)".
  const providerOptions = (): SelectOption[] =>
    props.providers.map((p) => ({ value: p.id, label: p.display_name }));
  const baseProvider = () =>
    providerFor(props.providers, provider(), props.settings.provider_default);
  const afkEffectiveProvider = () =>
    providerFor(
      props.providers,
      afkProvider(),
      props.settings.spawn_provider_default_afk,
      provider(),
      props.settings.provider_default,
    );

  // Per-key checkbox state for the provider's bool options (null bag = inherit,
  // seeded to all-unchecked). Once any checkbox differs from the seed, the full
  // declared bag is PATCHed as the explicit repo override.
  const [afkOptions, setAfkOptions] = createSignal<Record<string, boolean>>(
    toBoolMap(initial.afk_options),
  );
  const boolOptions = () =>
    (afkEffectiveProvider()?.options ?? []).filter((o) => o.type === 'bool');
  const [afkAuto, setAfkAuto] = createSignal(initial.afk_auto_enabled);
  const [budget, setBudget] = createSignal(
    initial.budget_minutes === null ? '' : String(initial.budget_minutes),
  );
  const [maxInstances, setMaxInstances] = createSignal(
    initial.max_instances_override === null ? '' : String(initial.max_instances_override),
  );
  const [authorName, setAuthorName] = createSignal(initial.git_author_name ?? '');
  const [authorEmail, setAuthorEmail] = createSignal(initial.git_author_email ?? '');
  const [incogni, setIncogni] = createSignal(initial.incogni);

  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [note, setNote] = createSignal<string | null>(null);

  // Re-seeds one draft from a refreshed repo. An untouched draft (still equal
  // to its seeded value) follows the server; a dirty draft keeps the
  // operator's in-flight edit — unless the server caught up with it (our own
  // save landing), which makes the field clean again.
  const resync = <T,>(get: () => T, set: (value: T) => void, read: (r: Repo) => T, fresh: Repo) => {
    const draft = get();
    const next = read(fresh);
    if (draft === read(seed) || draft === next) set(next);
  };

  // SSE refetch (repo.changed) landed: follow the server for untouched fields
  // and advance the seed so buildPatch keeps diffing drafts against what they
  // were seeded from. `on` runs the callback untracked, so operator keystrokes
  // never re-trigger it.
  createEffect(
    on(
      () => props.repo(),
      (fresh) => {
        resync(name, setName, (r) => r.name, fresh);
        resync(credentialId, setCredentialId, (r) => r.credential_id ?? '', fresh);
        resync(forgeCredentialId, setForgeCredentialId, (r) => r.forge_credential_id ?? '', fresh);
        resync(binding, setBinding, (r) => r.tracker_binding, fresh);
        resync(defaultBranch, setDefaultBranch, (r) => r.default_branch, fresh);
        resync(afkPattern, setAfkPattern, (r) => r.afk_branch_pattern, fresh);
        resync(manualPrefix, setManualPrefix, (r) => r.manual_branch_prefix, fresh);
        resync(provider, setProvider, (r) => r.provider ?? '', fresh);
        resync(model, setModel, (r) => r.model_default ?? '', fresh);
        resync(effort, setEffort, (r) => r.effort_default ?? '', fresh);
        resync(afkProvider, setAfkProvider, (r) => r.afk_provider_default ?? '', fresh);
        resync(afkModel, setAfkModel, (r) => r.afk_model_default ?? '', fresh);
        resync(afkEffort, setAfkEffort, (r) => r.afk_effort_default ?? '', fresh);
        resync(afkPrompt, setAfkPrompt, (r) => r.afk_prompt ?? '', fresh);
        // afk_options is an object, so resync it by value: an untouched draft
        // (still equal to the seed, or already caught up with fresh) follows
        // the server; a dirty draft keeps the operator's in-flight toggles.
        const draftOpts = afkOptions();
        if (
          optionsKey(draftOpts) === optionsKey(toBoolMap(seed.afk_options)) ||
          optionsKey(draftOpts) === optionsKey(toBoolMap(fresh.afk_options))
        ) {
          setAfkOptions(toBoolMap(fresh.afk_options));
        }
        resync(afkAuto, setAfkAuto, (r) => r.afk_auto_enabled, fresh);
        resync(
          budget,
          setBudget,
          (r) => (r.budget_minutes === null ? '' : String(r.budget_minutes)),
          fresh,
        );
        resync(
          maxInstances,
          setMaxInstances,
          (r) => (r.max_instances_override === null ? '' : String(r.max_instances_override)),
          fresh,
        );
        resync(authorName, setAuthorName, (r) => r.git_author_name ?? '', fresh);
        resync(authorEmail, setAuthorEmail, (r) => r.git_author_email ?? '', fresh);
        resync(incogni, setIncogni, (r) => r.incogni, fresh);
        seed = fresh;
      },
      { defer: true },
    ),
  );

  const gitCredentials = () =>
    props.credentials.filter((c) => c.kind === 'ssh_key' || c.kind === 'https_token');
  const forgeCredentials = () => props.credentials.filter((c) => c.kind === 'forge_token');

  const buildPatch = (): RepoPatch | string => {
    // Diff against the seed the drafts came from — NOT the live props.repo().
    // Diffing against the live repo would mark a stale draft of a field the
    // operator never touched as "dirty" and PATCH the old value back.
    const current = seed;
    const patch: RepoPatch = {};

    const trimmedName = name().trim();
    if (trimmedName === '') return 'Name must not be empty.';
    if (trimmedName !== current.name) patch.name = trimmedName;

    const cred = credentialId() === '' ? null : credentialId();
    if (cred !== current.credential_id) patch.credential_id = cred;
    const forgeCred = forgeCredentialId() === '' ? null : forgeCredentialId();
    if (forgeCred !== current.forge_credential_id) patch.forge_credential_id = forgeCred;
    if (binding() !== current.tracker_binding) patch.tracker_binding = binding();

    const branch = defaultBranch().trim();
    if (branch !== current.default_branch) patch.default_branch = branch;
    const pattern = afkPattern().trim();
    if (pattern !== current.afk_branch_pattern) patch.afk_branch_pattern = pattern;
    const prefix = manualPrefix().trim();
    if (prefix !== current.manual_branch_prefix) patch.manual_branch_prefix = prefix;

    if (normText(provider()) !== current.provider) patch.provider = normText(provider());
    if (normText(model()) !== current.model_default) patch.model_default = normText(model());
    if (normText(effort()) !== current.effort_default) patch.effort_default = normText(effort());

    if (normText(afkProvider()) !== current.afk_provider_default) {
      patch.afk_provider_default = normText(afkProvider());
    }
    if (normText(afkModel()) !== current.afk_model_default) {
      patch.afk_model_default = normText(afkModel());
    }
    if (normText(afkEffort()) !== current.afk_effort_default) {
      patch.afk_effort_default = normText(afkEffort());
    }
    if (normText(afkPrompt()) !== current.afk_prompt) patch.afk_prompt = normText(afkPrompt());
    // Send the full declared bag once any bool option differs from its seed.
    const declared = boolOptions();
    if (declared.length > 0) {
      const seeded = toBoolMap(current.afk_options);
      const checked = (key: string) => afkOptions()[key] ?? false;
      if (declared.some((o) => checked(o.key) !== (seeded[o.key] ?? false))) {
        patch.afk_options = Object.fromEntries(
          declared.map((o) => [o.key, checked(o.key) ? 'true' : 'false']),
        );
      }
    }

    if (afkAuto() !== current.afk_auto_enabled) patch.afk_auto_enabled = afkAuto();
    const budgetMinutes = normInt(budget());
    if (budgetMinutes === undefined) return 'Budget must be a whole number of minutes.';
    if (budgetMinutes !== current.budget_minutes) patch.budget_minutes = budgetMinutes;
    const cap = normInt(maxInstances());
    if (cap === undefined) return 'Max instances must be a whole number.';
    if (cap !== current.max_instances_override) patch.max_instances_override = cap;

    if (normText(authorName()) !== current.git_author_name) {
      patch.git_author_name = normText(authorName());
    }
    if (normText(authorEmail()) !== current.git_author_email) {
      patch.git_author_email = normText(authorEmail());
    }
    if (incogni() !== current.incogni) patch.incogni = incogni();

    return patch;
  };

  const save = async (event: SubmitEvent) => {
    event.preventDefault();
    setError(null);
    setNote(null);
    const patch = buildPatch();
    if (typeof patch === 'string') {
      setError(patch);
      return;
    }
    if (Object.keys(patch).length === 0) {
      setNote('Nothing to save.');
      return;
    }
    setBusy(true);
    try {
      await updateRepo(props.repo().id, patch);
      setNote('Saved.');
      props.onSaved();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={(e) => void save(e)} class="stack">
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={note()}>
        <div class="banner success" role="status">
          <span class="banner-text">{note()}</span>
        </div>
      </Show>

      <section class="card">
        <h2>Identity</h2>
        <label class="field">
          <span>Name</span>
          <input
            type="text"
            name="name"
            required
            autocomplete="off"
            spellcheck={false}
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Git author name</span>
          <input
            type="text"
            name="git_author_name"
            autocomplete="off"
            value={authorName()}
            onInput={(e) => setAuthorName(e.currentTarget.value)}
          />
          <small class="hint">Blank → global setting.</small>
        </label>
        <label class="field">
          <span>Git author email</span>
          <input
            type="text"
            name="git_author_email"
            autocomplete="off"
            spellcheck={false}
            value={authorEmail()}
            onInput={(e) => setAuthorEmail(e.currentTarget.value)}
          />
          <small class="hint">Blank → global setting.</small>
        </label>
      </section>

      <section class="card">
        <h2>Credentials & tracker</h2>
        <label class="field">
          <span>Git credential</span>
          <select
            name="credential_id"
            value={credentialId()}
            onChange={(e) => setCredentialId(e.currentTarget.value)}
          >
            <option value="">None (public remote)</option>
            <For each={gitCredentials()}>
              {(c) => (
                <option value={c.id}>
                  {c.name} ({c.kind === 'ssh_key' ? 'SSH key' : 'HTTPS token'})
                </option>
              )}
            </For>
          </select>
        </label>
        <label class="field">
          <span>Tracker binding</span>
          <select
            name="tracker_binding"
            value={binding()}
            onChange={(e) => setBinding(e.currentTarget.value as TrackerBinding)}
          >
            <option value="forge">Forge (Forgejo / GitHub issues + PRs)</option>
            <option value="builtin">Builtin (lab's own issues + CRs)</option>
          </select>
        </label>
        <label class="field">
          <span>Forge credential</span>
          <select
            name="forge_credential_id"
            value={forgeCredentialId()}
            onChange={(e) => setForgeCredentialId(e.currentTarget.value)}
          >
            <option value="">None</option>
            <For each={forgeCredentials()}>{(c) => <option value={c.id}>{c.name}</option>}</For>
          </select>
          <small class="hint">
            Forge API token for issues and PRs — required for the forge binding, never given to
            runs.
          </small>
        </label>
      </section>

      <section class="card">
        <h2>Branches</h2>
        <label class="field">
          <span>Default branch</span>
          <input
            type="text"
            name="default_branch"
            required
            autocomplete="off"
            spellcheck={false}
            value={defaultBranch()}
            onInput={(e) => setDefaultBranch(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>AFK branch pattern</span>
          <input
            type="text"
            name="afk_branch_pattern"
            required
            autocomplete="off"
            spellcheck={false}
            class="mono"
            value={afkPattern()}
            onInput={(e) => setAfkPattern(e.currentTarget.value)}
          />
          <small class="hint">
            Must contain &lt;N&gt; exactly once (the issue number), e.g. afk/&lt;N&gt; or
            issue-&lt;N&gt;. Letters, digits, . _ / - only; may not overlap the manual prefix.
          </small>
        </label>
        <label class="field">
          <span>Manual branch prefix</span>
          <input
            type="text"
            name="manual_branch_prefix"
            required
            autocomplete="off"
            spellcheck={false}
            class="mono"
            value={manualPrefix()}
            onInput={(e) => setManualPrefix(e.currentTarget.value)}
          />
          <small class="hint">
            Literal prefix for manual instance branches, e.g. lab/ or wip/.
          </small>
        </label>
      </section>

      <section class="card">
        <h2>Spawn defaults</h2>
        <Select
          skin="field"
          label="Agent"
          name="provider"
          value={provider()}
          options={providerOptions()}
          inheritLabel="Inherit global default"
          onChange={setProvider}
        />
        <Select
          skin="field"
          label="Model"
          name="model_default"
          value={model()}
          options={baseProvider()?.models ?? []}
          inheritLabel="Inherit global default"
          onChange={setModel}
        />
        <Select
          skin="field"
          label="Effort"
          name="effort_default"
          value={effort()}
          options={baseProvider()?.efforts ?? []}
          inheritLabel="Inherit global default"
          onChange={setEffort}
        />
      </section>

      <section class="card">
        <h2>AFK defaults</h2>
        <small class="hint hint-block">
          Overrides for unattended AFK runs on this repo. Blank / unchecked inherits the global AFK
          default.
        </small>
        <Select
          skin="field"
          label="AFK agent"
          name="afk_provider_default"
          value={afkProvider()}
          options={providerOptions()}
          inheritLabel="Inherit global AFK default"
          onChange={setAfkProvider}
        />
        <Select
          skin="field"
          label="Model"
          name="afk_model_default"
          value={afkModel()}
          options={afkEffectiveProvider()?.models ?? []}
          inheritLabel="Inherit global AFK default"
          onChange={setAfkModel}
        />
        <Select
          skin="field"
          label="Effort"
          name="afk_effort_default"
          value={afkEffort()}
          options={afkEffectiveProvider()?.efforts ?? []}
          inheritLabel="Inherit global AFK default"
          onChange={setAfkEffort}
        />
        <For each={boolOptions()}>
          {(option) => (
            <label class="check">
              <input
                type="checkbox"
                name={`afk_options.${option.key}`}
                checked={afkOptions()[option.key] ?? false}
                onChange={(e) =>
                  setAfkOptions({ ...afkOptions(), [option.key]: e.currentTarget.checked })
                }
              />
              <span>{option.label}</span>
            </label>
          )}
        </For>
        <Show when={boolOptions().length > 0}>
          <small class="hint hint-block">A repo option bag overrides the global AFK options.</small>
        </Show>
        <label class="field">
          <span>Seed prompt</span>
          <textarea
            name="afk_prompt"
            rows="10"
            value={afkPrompt()}
            onInput={(e) => setAfkPrompt(e.currentTarget.value)}
            placeholder={props.repo().afk_prompt_effective}
          />
        </label>
        <Show when={afkPrompt() === ''}>
          <button
            type="button"
            class="small"
            onClick={() => setAfkPrompt(props.repo().afk_prompt_effective)}
          >
            Customize
          </button>
        </Show>
        <small class="hint hint-block">
          The run is detected as done only by an open PR on its branch — a prompt that never opens a
          PR burns its budget, counts as a failure, and three failures auto-pause the repo's AFK.
        </small>
      </section>

      <section class="card">
        <h2>AFK</h2>
        <label class="check">
          <input
            type="checkbox"
            name="afk_auto_enabled"
            checked={afkAuto()}
            onChange={(e) => setAfkAuto(e.currentTarget.checked)}
          />
          <span>Auto-spawn on ready-for-agent issues</span>
        </label>
        <label class="field">
          <span>Budget (minutes)</span>
          <input
            type="number"
            name="budget_minutes"
            min="1"
            step="1"
            autocomplete="off"
            placeholder="global default"
            value={budget()}
            onInput={(e) => setBudget(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Max instances override</span>
          <input
            type="number"
            name="max_instances_override"
            min="1"
            step="1"
            autocomplete="off"
            placeholder="global default"
            value={maxInstances()}
            onInput={(e) => setMaxInstances(e.currentTarget.value)}
          />
        </label>
      </section>

      <section class="card">
        <h2>Incogni</h2>
        <label class="check">
          <input
            type="checkbox"
            name="incogni"
            checked={incogni()}
            onChange={(e) => setIncogni(e.currentTarget.checked)}
          />
          <span>Incogni</span>
        </label>
        <small class="hint hint-block">
          Strips AI attribution from this repo's output. Toggling it does NOT rewrite the branch
          pattern or prefix above — adjust those yourself if needed. It cannot hide the forge
          account of the token used, nor style or timing signals.
        </small>
      </section>

      <button type="submit" class="primary wide" disabled={busy()}>
        {busy() ? 'Saving…' : 'Save changes'}
      </button>
    </form>
  );
}

function secretUpdatedOn(timestamp: string): string {
  const date = new Date(timestamp);
  return Number.isNaN(date.getTime()) ? timestamp : date.toLocaleDateString();
}

/**
 * Secrets section (issue #104): a write-only per-repo secret store. No
 * endpoint or view here ever fetches or displays a value — create, rotate
 * and delete only. Agents consume secrets via `labctl secret exec`, not
 * through this UI.
 */
function RepoSecretsSection(props: { repoId: string }) {
  const [secrets, { refetch }] = createResource(() => listRepoSecrets(props.repoId));
  const [showCreate, setShowCreate] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  return (
    <section class="card">
      <div class="card-head">
        <h2>Secrets</h2>
        <span class="spacer" />
        <button type="button" class="primary small" onClick={() => setShowCreate(!showCreate())}>
          {showCreate() ? 'Cancel' : '+ Add secret'}
        </button>
      </div>
      <small class="hint hint-block">
        Values are write-only — lab never reads or shows them again after saving. Agents use them
        via <code>labctl secret exec</code>.
      </small>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={showCreate()}>
        <CreateSecretForm
          repoId={props.repoId}
          onCreated={() => {
            setShowCreate(false);
            void refetch();
          }}
        />
      </Show>
      <Switch>
        <Match when={secrets.error !== undefined}>
          <div class="banner error" role="alert">
            <span class="banner-text">{errorMessage(secrets.error)}</span>
          </div>
        </Match>
        <Match when={secrets()?.length === 0}>
          <p class="empty">No secrets yet — add one for agents to use via labctl secret exec.</p>
        </Match>
        <Match when={secrets()}>
          <div class="card-list">
            <For each={secrets()}>
              {(secret) => (
                <SecretRow
                  repoId={props.repoId}
                  secret={secret}
                  onChanged={() => void refetch()}
                  onError={setError}
                />
              )}
            </For>
          </div>
        </Match>
      </Switch>
    </section>
  );
}

function CreateSecretForm(props: { repoId: string; onCreated: () => void }) {
  const [name, setName] = createSignal('');
  const [description, setDescription] = createSignal('');
  const [value, setValue] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await createRepoSecret(props.repoId, name().trim(), description().trim(), value());
      setName('');
      setDescription('');
      setValue('');
      props.onCreated();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="card form-card">
      <h2>New secret</h2>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <form onSubmit={(e) => void submit(e)}>
        <label class="field">
          <span>Name</span>
          <input
            type="text"
            name="secret-name"
            required
            autocomplete="off"
            spellcheck={false}
            class="mono"
            placeholder="API_KEY"
            value={name()}
            onInput={(e) => setName(e.currentTarget.value)}
          />
          <small class="hint">
            Uppercase letters, digits, underscores; must start with a letter.
          </small>
        </label>
        <label class="field">
          <span>Description</span>
          <input
            type="text"
            name="secret-description"
            autocomplete="off"
            value={description()}
            onInput={(e) => setDescription(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Value</span>
          <input
            type="password"
            name="secret-value"
            required
            autocomplete="off"
            value={value()}
            onInput={(e) => setValue(e.currentTarget.value)}
          />
        </label>
        <button type="submit" class="primary wide" disabled={busy()}>
          {busy() ? 'Saving…' : 'Save secret'}
        </button>
      </form>
    </div>
  );
}

function SecretRow(props: {
  repoId: string;
  secret: RepoSecret;
  onChanged: () => void;
  onError: (message: string | null) => void;
}) {
  const [rotating, setRotating] = createSignal(false);
  const [newValue, setNewValue] = createSignal('');
  const [busy, setBusy] = createSignal(false);

  const rotate = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    props.onError(null);
    try {
      await rotateRepoSecret(props.repoId, props.secret.id, newValue());
      setNewValue('');
      setRotating(false);
      props.onChanged();
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!window.confirm(`Delete secret "${props.secret.name}"?`)) return;
    setBusy(true);
    props.onError(null);
    try {
      await deleteRepoSecret(props.repoId, props.secret.id);
      props.onChanged();
    } catch (err) {
      props.onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <article class="card">
      <div class="card-head">
        <span class="card-title mono">{props.secret.name}</span>
        <span class="spacer" />
        <Show when={!rotating()}>
          <button type="button" class="small" onClick={() => setRotating(true)} disabled={busy()}>
            Rotate
          </button>
          <button
            type="button"
            class="danger small"
            onClick={() => void remove()}
            disabled={busy()}
          >
            {busy() ? 'Working…' : 'Delete'}
          </button>
        </Show>
      </div>
      <Show when={props.secret.description}>
        <p class="muted card-sub">{props.secret.description}</p>
      </Show>
      <p class="muted card-sub">Updated {secretUpdatedOn(props.secret.updated_at)}</p>
      {/* The sticky exposure warning (issue #108): exposed_run_id/exposed_at
          are both null until a run's transcript surfaces the value, then both
          set until the next rotate clears them — the refetch after a
          successful rotate (props.onChanged, wired above) is what makes this
          disappear, no separate clear-exposure call needed. */}
      <Show when={props.secret.exposed_run_id}>
        {(runID) => (
          <p class="secret-exposed">
            <span class="chip exposed">Exposed</span>{' '}
            <span>
              Exposed in run <A href={`/runs/${runID()}`}>{runID()}</A>
              <Show when={props.secret.exposed_at}>
                {(exposedAt) => <> · {secretUpdatedOn(exposedAt())}</>}
              </Show>{' '}
              — rotate to clear.
            </span>
          </p>
        )}
      </Show>
      <Show when={rotating()}>
        {/* Rotation: the field starts blank by construction — the old value is
            never fetched, so there is nothing to prefill. */}
        <form onSubmit={(e) => void rotate(e)}>
          <label class="field">
            <span>New value</span>
            <input
              type="password"
              name="secret-rotate-value"
              required
              autocomplete="off"
              value={newValue()}
              onInput={(e) => setNewValue(e.currentTarget.value)}
            />
          </label>
          <div class="card-actions">
            <button type="submit" class="primary" disabled={busy()}>
              {busy() ? 'Rotating…' : 'Save new value'}
            </button>
            <button
              type="button"
              onClick={() => {
                setNewValue('');
                setRotating(false);
              }}
              disabled={busy()}
            >
              Cancel
            </button>
          </div>
        </form>
      </Show>
    </article>
  );
}

function DangerZone(props: { repo: Accessor<Repo> }) {
  const navigate = useNavigate();
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [needsForce, setNeedsForce] = createSignal(false);
  const [force, setForce] = createSignal(false);

  const remove = async () => {
    const target = props.repo();
    if (!window.confirm(`Delete repository "${target.name}"? This removes lab's clone.`)) return;
    setBusy(true);
    setError(null);
    try {
      await deleteRepo(target.id, force());
      navigate('/');
    } catch (err) {
      // 409 while the clone runs — reveal the force checkbox and let the
      // operator escalate deliberately.
      if (err instanceof ApiError && err.status === 409) setNeedsForce(true);
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <section class="card danger-zone">
      <h2>Danger zone</h2>
      <ErrorBanner message={error()} onDismiss={() => setError(null)} />
      <Show when={needsForce()}>
        <label class="check">
          <input
            type="checkbox"
            name="force"
            checked={force()}
            onChange={(e) => setForce(e.currentTarget.checked)}
          />
          <span>Force delete (abandon the running clone)</span>
        </label>
      </Show>
      <div class="card-actions">
        <button type="button" class="danger" onClick={() => void remove()} disabled={busy()}>
          {busy() ? 'Deleting…' : 'Delete repository'}
        </button>
      </div>
    </section>
  );
}
