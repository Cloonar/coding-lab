// Add-repo page: URL + auto-derived (editable) name preview, git credential
// select, tracker binding, forge credential and incogni toggle. On submit the
// clone starts async — the dashboard card shows live progress via SSE.

import { A, useNavigate } from '@solidjs/router';
import { For, Show, createResource, createSignal } from 'solid-js';
import {
  createRepo,
  errorMessage,
  listCredentials,
  listProviders,
  type CreateRepoRequest,
  type TrackerBinding,
} from '../api';
import FormCard from '../components/FormCard';
import RequireAuth from '../components/RequireAuth';
import Select from '../components/Select';
import { deriveRepoName } from '../lib/repoName';

type BindingChoice = 'auto' | TrackerBinding;

export default function AddRepo() {
  return (
    <RequireAuth>
      <AddRepoView />
    </RequireAuth>
  );
}

function AddRepoView() {
  const navigate = useNavigate();
  const [credentials] = createResource(() => listCredentials());
  const [providers] = createResource(() => listProviders());
  const gitCredentials = () =>
    (credentials() ?? []).filter((c) => c.kind === 'ssh_key' || c.kind === 'https_token');
  const forgeCredentials = () => (credentials() ?? []).filter((c) => c.kind === 'forge_token');
  const providerOptions = () =>
    (providers() ?? []).map((p) => ({ value: p.id, label: p.display_name }));

  const [url, setUrl] = createSignal('');
  const [name, setName] = createSignal('');
  const [nameTouched, setNameTouched] = createSignal(false);
  const [credentialId, setCredentialId] = createSignal('');
  const [forgeCredentialId, setForgeCredentialId] = createSignal('');
  const [binding, setBinding] = createSignal<BindingChoice>('auto');
  // '' = inherit the global provider_default; only a non-empty pick is sent.
  const [provider, setProvider] = createSignal('');
  const [incogni, setIncogni] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  // Preview only — the untouched value is never sent; the server derives the
  // authoritative name itself when the request omits `name`.
  const derivedName = () => deriveRepoName(url());
  const shownName = () => (nameTouched() ? name() : derivedName());

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    const req: CreateRepoRequest = { remote_url: url().trim() };
    if (nameTouched() && name().trim() !== '') req.name = name().trim();
    if (credentialId() !== '') req.credential_id = credentialId();
    if (provider() !== '') req.provider = provider();
    if (binding() !== 'auto') req.tracker_binding = binding();
    if (binding() !== 'builtin' && forgeCredentialId() !== '') {
      req.forge_credential_id = forgeCredentialId();
    }
    if (incogni()) req.incogni = true;
    try {
      await createRepo(req);
      navigate('/'); // the new card shows clone progress live
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <main class="page">
      <FormCard
        title="Add repository"
        intro={
          <p class="muted">
            lab clones a bare mirror it owns; runs get their own worktrees. Private remotes need a
            git credential — <A href="/credentials">add one</A> first if the list is empty.
          </p>
        }
        error={error()}
        onDismissError={() => setError(null)}
        onSubmit={(e) => void submit(e)}
        busy={busy()}
        wide
        submitLabel="Add repository"
        busyLabel="Adding…"
      >
        <label class="field">
          <span>Remote URL</span>
          <input
            type="text"
            name="remote_url"
            required
            autocomplete="off"
            spellcheck={false}
            placeholder="git@git.cloonar.com:Cloonar/coding-lab.git"
            value={url()}
            onInput={(e) => setUrl(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Name</span>
          <input
            type="text"
            name="name"
            autocomplete="off"
            spellcheck={false}
            value={shownName()}
            onInput={(e) => {
              setNameTouched(true);
              setName(e.currentTarget.value);
            }}
          />
          <small class="hint">
            {nameTouched()
              ? 'Custom name — the server validates it.'
              : 'Auto-derived from the URL — edit to override.'}
          </small>
        </label>
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
        <Select
          skin="field"
          label="Agent"
          name="provider"
          value={provider()}
          options={providerOptions()}
          inheritLabel="Global default"
          onChange={setProvider}
        />
        <label class="field">
          <span>Tracker binding</span>
          <select
            name="tracker_binding"
            value={binding()}
            onChange={(e) => setBinding(e.currentTarget.value as BindingChoice)}
          >
            <option value="auto">Auto (forge when detected, else builtin)</option>
            <option value="forge">Forge (Forgejo / GitHub issues + PRs)</option>
            <option value="builtin">Builtin (lab's own issues + CRs)</option>
          </select>
        </label>
        <Show when={binding() !== 'builtin'}>
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
            <small class="hint">Forge API token for issues and PRs — never given to runs.</small>
          </label>
        </Show>
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
          Strips AI attribution from this repo's output: neutral branch names (issue-N, wip/),
          sanitized PR bodies, your git identity. It cannot hide the forge account of the token
          used, nor style or timing signals.
        </small>
      </FormCard>
    </main>
  );
}
