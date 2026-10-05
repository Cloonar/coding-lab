// Add repository (issue #61): remote URL, name, git credential, tracker
// binding (Auto / Forge / Built-in), forge credential and Incogni. There is no
// Agent pick — a new repo inherits the global default like every other
// overridable field, and the pick lives in Settings → Agents — so the request
// never carries `provider`.
//
// The name follows the URL as the operator types (a preview of the server's
// own derivation; an untouched name is not sent, so the server derives the
// authoritative one) until the operator edits it, after which the URL never
// overwrites it again. The forge credential is hidden for the Built-in
// binding, and never sent with it.
//
// Errors appear under the field they belong to (aria-invalid plus
// aria-describedby), and focus moves to the first field in error: an empty or
// unparsable URL is caught here before anything is sent (permissively — the
// server is the authority), and a server refusal lands at the field its
// `ApiError.field` names; one that names no field shows in the form's banner.
// On success the app opens the new repo's home, whose readiness block shows
// the clone progress.

import { A, useNavigate } from '@solidjs/router';
import { For, Show, createResource, createSignal } from 'solid-js';
import {
  ApiError,
  createRepo,
  errorMessage,
  listCredentials,
  type CreateRepoRequest,
  type TrackerBinding,
} from '../api';
import FormCard from '../components/FormCard';
import Icon from '../components/Icon';
import RequireAuth from '../components/RequireAuth';
import Segmented from '../components/Segmented';
import ToggleSwitch from '../components/Switch';
import { parseRemote, remoteUrlProblem, REMOTE_EXAMPLE } from '../lib/remoteUrl';
import { deriveRepoName } from '../lib/repoName';
import { resourceValue } from '../lib/resource';
import { noticeState } from '../lib/routeNotice';

type BindingChoice = 'auto' | TrackerBinding;

/** The fields an error can be shown under, in form order (focus goes to the first). */
const FIELDS = [
  'remote_url',
  'name',
  'credential_id',
  'tracker_binding',
  'forge_credential_id',
] as const;
type FieldKey = (typeof FIELDS)[number];
type FieldErrors = Partial<Record<FieldKey, string>>;

const isFieldKey = (key: string | undefined): key is FieldKey =>
  key !== undefined && (FIELDS as readonly string[]).includes(key);

const fieldId = (key: FieldKey) => `add-repo-${key.replaceAll('_', '-')}`;
const errorId = (key: FieldKey) => `${fieldId(key)}-error`;
const hintId = (key: FieldKey) => `${fieldId(key)}-hint`;

const BINDING_OPTIONS = [
  { value: 'auto', label: 'Auto' },
  { value: 'forge', label: 'Forge' },
  { value: 'builtin', label: 'Built-in' },
];

const BINDING_HINTS: Record<BindingChoice, string> = {
  auto: 'Auto binds to the forge when lab knows the host and a forge credential is set, otherwise to Built-in.',
  forge: 'Issues and pull requests stay on the forge. Needs a forge credential.',
  builtin: "Issues and change requests live in lab's built-in tracker.",
};

/** The message under a field, tied to its control by `id` (aria-describedby). */
function FieldError(props: { id: string; message: string | undefined }) {
  return (
    <Show when={props.message}>
      {(message) => (
        <small class="field-error" id={props.id}>
          <Icon name="circle-alert" size={16} />
          <span>{message()}</span>
        </small>
      )}
    </Show>
  );
}

export default function AddRepo() {
  return (
    <RequireAuth>
      <AddRepoView />
    </RequireAuth>
  );
}

function AddRepoView() {
  const navigate = useNavigate();
  const [credentials, { refetch: reloadCredentials }] = createResource(() => listCredentials());
  const allCredentials = () => resourceValue(credentials) ?? [];
  // A failed credentials read says so (with a retry) instead of reading as
  // "you have no credentials".
  const credentialsFailed = () => credentials.error !== undefined;
  const gitCredentials = () =>
    allCredentials().filter((c) => c.kind === 'ssh_key' || c.kind === 'https_token');
  const forgeCredentials = () => allCredentials().filter((c) => c.kind === 'forge_token');

  const [url, setUrl] = createSignal('');
  const [name, setName] = createSignal('');
  const [nameTouched, setNameTouched] = createSignal(false);
  const [credentialId, setCredentialId] = createSignal('');
  const [forgeCredentialId, setForgeCredentialId] = createSignal('');
  const [binding, setBinding] = createSignal<BindingChoice>('auto');
  const [incogni, setIncogni] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [fieldErrors, setFieldErrors] = createSignal<FieldErrors>({});

  // Preview only — see the header: the untouched value is never sent.
  const shownName = () => (nameTouched() ? name() : deriveRepoName(url()));
  const showForgeCredential = () => binding() !== 'builtin';

  const fieldError = (key: FieldKey) => fieldErrors()[key];
  const clearError = (key: FieldKey) => {
    if (fieldErrors()[key] === undefined) return;
    setFieldErrors((prev) => {
      const next = { ...prev };
      delete next[key];
      return next;
    });
  };
  /** aria-describedby for a field: its error first (when shown), then its hint. */
  const describedBy = (key: FieldKey, hint = true) =>
    [fieldError(key) !== undefined ? errorId(key) : null, hint ? hintId(key) : null]
      .filter((id) => id !== null)
      .join(' ') || undefined;

  const urlHint = () => {
    const parsed = parseRemote(url());
    if (parsed === null) return 'Paste an SSH or HTTPS remote. The name is filled in from it.';
    if (parsed.host === null) return "A repository on lab's own host.";
    return `Remote on ${parsed.host}.`;
  };

  let page: HTMLElement | undefined;
  /** Moves focus to the control of the first field in error, in form order. */
  const focusFirstError = (errors: FieldErrors) => {
    const first = FIELDS.find((key) => errors[key] !== undefined);
    if (first === undefined || page === undefined) return;
    const control =
      first === 'tracker_binding'
        ? page.querySelector<HTMLElement>(`#${fieldId(first)} [role="radio"][aria-checked="true"]`)
        : page.querySelector<HTMLElement>(`#${fieldId(first)}`);
    control?.focus();
  };

  const submit = async (event: SubmitEvent) => {
    event.preventDefault();
    if (busy()) return;
    setError(null);
    const problem = remoteUrlProblem(url());
    if (problem !== null) {
      const errors: FieldErrors = { remote_url: problem };
      setFieldErrors(errors);
      focusFirstError(errors);
      return;
    }
    setFieldErrors({});
    const req: CreateRepoRequest = { remote_url: url().trim() };
    if (nameTouched() && name().trim() !== '') req.name = name().trim();
    if (credentialId() !== '') req.credential_id = credentialId();
    if (binding() !== 'auto') req.tracker_binding = binding();
    if (showForgeCredential() && forgeCredentialId() !== '') {
      req.forge_credential_id = forgeCredentialId();
    }
    if (incogni()) req.incogni = true;
    setBusy(true);
    try {
      const created = await createRepo(req);
      navigate(`/repos/${created.id}`, {
        state: noticeState(`Added ${created.name}. Cloning has started.`),
      });
    } catch (err) {
      setBusy(false);
      const field = err instanceof ApiError ? err.field : undefined;
      // A field the form shows gets the message under it; anything else
      // (no field, a hidden one, a network error) goes to the banner.
      if (isFieldKey(field) && (field !== 'forge_credential_id' || showForgeCredential())) {
        const errors: FieldErrors = { [field]: errorMessage(err) };
        setFieldErrors(errors);
        focusFirstError(errors);
      } else {
        setError(errorMessage(err));
      }
    }
  };

  return (
    <main ref={page} class="page page-wide add-repo">
      <A href="/repos" class="back-link">
        <Icon name="chevron-left" size={20} />
        Repositories
      </A>
      <h1 class="add-repo-title">Add repository</h1>
      <FormCard
        error={error()}
        onDismissError={() => setError(null)}
        onSubmit={(e) => void submit(e)}
        busy={busy()}
        wide
        submitLabel="Add and start cloning"
        busyLabel="Adding…"
        // The note belongs under the button, inside the card: the actions
        // slot is the one place the shell renders after it.
        actions={
          <p class="hint add-repo-note">
            The agent, the Runner and every other setting start from your global settings. The
            repository page shows the clone and the readiness checks.
          </p>
        }
      >
        <div class="field" classList={{ invalid: fieldError('remote_url') !== undefined }}>
          <label for={fieldId('remote_url')}>Remote URL</label>
          <input
            id={fieldId('remote_url')}
            type="text"
            name="remote_url"
            class="mono"
            inputmode="url"
            autocomplete="off"
            autocapitalize="off"
            spellcheck={false}
            placeholder={REMOTE_EXAMPLE}
            value={url()}
            aria-invalid={fieldError('remote_url') !== undefined ? 'true' : undefined}
            aria-describedby={describedBy('remote_url')}
            onInput={(e) => {
              setUrl(e.currentTarget.value);
              clearError('remote_url');
            }}
          />
          <FieldError id={errorId('remote_url')} message={fieldError('remote_url')} />
          <small class="hint" id={hintId('remote_url')}>
            {urlHint()}
          </small>
        </div>

        <div class="field" classList={{ invalid: fieldError('name') !== undefined }}>
          <label for={fieldId('name')}>Name</label>
          <input
            id={fieldId('name')}
            type="text"
            name="name"
            autocomplete="off"
            autocapitalize="off"
            spellcheck={false}
            placeholder="Taken from the URL"
            value={shownName()}
            aria-invalid={fieldError('name') !== undefined ? 'true' : undefined}
            aria-describedby={describedBy('name', false)}
            onInput={(e) => {
              // The value first: marking the name touched re-renders the
              // field from the draft, which must already hold what was typed.
              setName(e.currentTarget.value);
              setNameTouched(true);
              clearError('name');
            }}
          />
          <FieldError id={errorId('name')} message={fieldError('name')} />
        </div>

        <div class="field" classList={{ invalid: fieldError('credential_id') !== undefined }}>
          <label for={fieldId('credential_id')}>Git credential</label>
          <select
            id={fieldId('credential_id')}
            name="credential_id"
            value={credentialId()}
            aria-invalid={fieldError('credential_id') !== undefined ? 'true' : undefined}
            aria-describedby={describedBy(
              'credential_id',
              credentialsFailed() || gitCredentials().length === 0,
            )}
            onChange={(e) => {
              setCredentialId(e.currentTarget.value);
              clearError('credential_id');
            }}
          >
            <option value="">None, public remote</option>
            <For each={gitCredentials()}>
              {(c) => (
                <option value={c.id}>
                  {c.name} · {c.kind === 'ssh_key' ? 'SSH key' : 'HTTPS token'}
                </option>
              )}
            </For>
          </select>
          <FieldError id={errorId('credential_id')} message={fieldError('credential_id')} />
          <Show
            when={credentialsFailed()}
            fallback={
              <Show when={gitCredentials().length === 0}>
                <small class="hint" id={hintId('credential_id')}>
                  A private remote needs one — <A href="/credentials">add a git credential</A>{' '}
                  first.
                </small>
              </Show>
            }
          >
            <small class="hint add-repo-credentials-failed" id={hintId('credential_id')}>
              <span>
                Your credentials could not be loaded ({errorMessage(credentials.error)}), so none
                are listed.
              </span>
              <button
                type="button"
                class="add-repo-retry"
                disabled={credentials.loading}
                onClick={() => void reloadCredentials()}
              >
                {credentials.loading ? 'Loading…' : 'Load credentials again'}
              </button>
            </small>
          </Show>
        </div>

        <div
          id={fieldId('tracker_binding')}
          class="field"
          classList={{ invalid: fieldError('tracker_binding') !== undefined }}
        >
          <Segmented
            label="Tracker binding"
            name="tracker_binding"
            value={binding()}
            options={BINDING_OPTIONS}
            describedBy={describedBy('tracker_binding')}
            invalid={fieldError('tracker_binding') !== undefined}
            onChange={(value) => {
              setBinding(value as BindingChoice);
              clearError('tracker_binding');
            }}
          />
          <FieldError id={errorId('tracker_binding')} message={fieldError('tracker_binding')} />
          <small class="hint" id={hintId('tracker_binding')}>
            {BINDING_HINTS[binding()]}
          </small>
        </div>

        <Show when={showForgeCredential()}>
          <div
            class="field"
            classList={{ invalid: fieldError('forge_credential_id') !== undefined }}
          >
            <label for={fieldId('forge_credential_id')}>Forge credential</label>
            <select
              id={fieldId('forge_credential_id')}
              name="forge_credential_id"
              value={forgeCredentialId()}
              aria-invalid={fieldError('forge_credential_id') !== undefined ? 'true' : undefined}
              aria-describedby={describedBy('forge_credential_id')}
              onChange={(e) => {
                setForgeCredentialId(e.currentTarget.value);
                clearError('forge_credential_id');
              }}
            >
              <option value="">None</option>
              <For each={forgeCredentials()}>{(c) => <option value={c.id}>{c.name}</option>}</For>
            </select>
            <FieldError
              id={errorId('forge_credential_id')}
              message={fieldError('forge_credential_id')}
            />
            <small class="hint" id={hintId('forge_credential_id')}>
              The forge API token for issues and pull requests. It is never given to runs.
              <Show when={credentialsFailed()}>
                {' '}
                Your credentials could not be loaded, so none are listed.
              </Show>
            </small>
          </div>
        </Show>

        <ToggleSwitch
          class="add-repo-incogni"
          name="incogni"
          label="Incogni"
          description="Strips AI attribution from this repo's output."
          checked={incogni()}
          onChange={setIncogni}
        />
      </FormCard>
    </main>
  );
}
