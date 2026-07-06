// Credential payload inputs, switched by kind. Used by both the create form
// and the replace-secret flow — fields ALWAYS start blank (secrets are
// write-only, never read back or prefilled). Secrets use password inputs;
// the SSH private key is a textarea (multi-line PEM), also never prefilled.

import { Match, Show, Switch, createSignal, type Accessor } from 'solid-js';
import type { CredentialKind, CredentialPayload, ForgeFlavor } from '../api';

// The host GitHub operators want by default; a GHE root is entered by hand.
const GITHUB_DEFAULT_HOST = 'api.github.com';

export interface PayloadDraft {
  privateKey: Accessor<string>;
  passphrase: Accessor<string>;
  username: Accessor<string>;
  token: Accessor<string>;
  host: Accessor<string>;
  forge: Accessor<ForgeFlavor>;
  setPrivateKey: (v: string) => void;
  setPassphrase: (v: string) => void;
  setUsername: (v: string) => void;
  setToken: (v: string) => void;
  setHost: (v: string) => void;
  setForge: (v: ForgeFlavor) => void;
  /** Assembles the payload for the given kind from the current field values. */
  build(kind: CredentialKind): CredentialPayload;
  reset(): void;
}

export function createPayloadDraft(): PayloadDraft {
  const [privateKey, setPrivateKey] = createSignal('');
  const [passphrase, setPassphrase] = createSignal('');
  const [username, setUsername] = createSignal('');
  const [token, setToken] = createSignal('');
  const [host, setHost] = createSignal('');
  const [forge, setForge] = createSignal<ForgeFlavor>('forgejo');

  return {
    privateKey,
    passphrase,
    username,
    token,
    host,
    forge,
    setPrivateKey,
    setPassphrase,
    setUsername,
    setToken,
    setHost,
    setForge,
    build(kind) {
      switch (kind) {
        case 'ssh_key':
          return passphrase() === ''
            ? { private_key: privateKey() }
            : { private_key: privateKey(), passphrase: passphrase() };
        case 'https_token':
          return { username: username(), token: token() };
        case 'forge_token':
          return { host: host().trim(), token: token(), forge: forge() };
      }
    },
    reset() {
      setPrivateKey('');
      setPassphrase('');
      setUsername('');
      setToken('');
      setHost('');
      setForge('forgejo');
    },
  };
}

export default function PayloadFields(props: { kind: CredentialKind; draft: PayloadDraft }) {
  return (
    <Switch>
      <Match when={props.kind === 'ssh_key'}>
        <label class="field">
          <span>Private key</span>
          <textarea
            name="private_key"
            rows="6"
            required
            autocomplete="off"
            spellcheck={false}
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
            value={props.draft.privateKey()}
            onInput={(e) => props.draft.setPrivateKey(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Passphrase (optional)</span>
          <input
            type="password"
            name="passphrase"
            autocomplete="off"
            value={props.draft.passphrase()}
            onInput={(e) => props.draft.setPassphrase(e.currentTarget.value)}
          />
        </label>
      </Match>
      <Match when={props.kind === 'https_token'}>
        <label class="field">
          <span>Username</span>
          <input
            type="text"
            name="git_username"
            required
            autocomplete="off"
            value={props.draft.username()}
            onInput={(e) => props.draft.setUsername(e.currentTarget.value)}
          />
        </label>
        <label class="field">
          <span>Token</span>
          <input
            type="password"
            name="token"
            required
            autocomplete="off"
            value={props.draft.token()}
            onInput={(e) => props.draft.setToken(e.currentTarget.value)}
          />
        </label>
      </Match>
      <Match when={props.kind === 'forge_token'}>
        <label class="field">
          <span>Forge</span>
          <select
            name="forge"
            value={props.draft.forge()}
            onChange={(e) => {
              const flavor = e.currentTarget.value as ForgeFlavor;
              props.draft.setForge(flavor);
              // Prefill the host that matches the flavor (overridable): GitHub's
              // API origin, or a blank field for a Forgejo instance.
              props.draft.setHost(flavor === 'github' ? GITHUB_DEFAULT_HOST : '');
            }}
          >
            <option value="forgejo">Forgejo</option>
            <option value="github">GitHub</option>
          </select>
        </label>
        <label class="field">
          <span>{props.draft.forge() === 'github' ? 'API host' : 'Forge host'}</span>
          <input
            type="text"
            name="host"
            required
            autocomplete="off"
            placeholder={props.draft.forge() === 'github' ? 'api.github.com' : 'git.example.com'}
            value={props.draft.host()}
            onInput={(e) => props.draft.setHost(e.currentTarget.value)}
          />
          <Show when={props.draft.forge() === 'github'}>
            <small class="hint">
              github.com → <code>api.github.com</code>. GitHub Enterprise → your API root, e.g.{' '}
              <code>ghe.example.com/api/v3</code>.
            </small>
          </Show>
        </label>
        <label class="field">
          <span>API token</span>
          <input
            type="password"
            name="token"
            required
            autocomplete="off"
            value={props.draft.token()}
            onInput={(e) => props.draft.setToken(e.currentTarget.value)}
          />
          <Show when={props.draft.forge() === 'github'}>
            <small class="hint">
              Fine-grained PAT: Issues (RW), Pull requests (RW), Metadata (R). Classic PAT:{' '}
              <code>repo</code>.
            </small>
          </Show>
        </label>
      </Match>
    </Switch>
  );
}
