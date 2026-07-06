// Credential payload inputs, switched by kind. Used by both the create form
// and the replace-secret flow — fields ALWAYS start blank (secrets are
// write-only, never read back or prefilled). Secrets use password inputs;
// the SSH private key is a textarea (multi-line PEM), also never prefilled.

import { Match, Switch, createSignal, type Accessor } from 'solid-js';
import type { CredentialKind, CredentialPayload } from '../api';

export interface PayloadDraft {
  privateKey: Accessor<string>;
  passphrase: Accessor<string>;
  username: Accessor<string>;
  token: Accessor<string>;
  host: Accessor<string>;
  setPrivateKey: (v: string) => void;
  setPassphrase: (v: string) => void;
  setUsername: (v: string) => void;
  setToken: (v: string) => void;
  setHost: (v: string) => void;
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

  return {
    privateKey,
    passphrase,
    username,
    token,
    host,
    setPrivateKey,
    setPassphrase,
    setUsername,
    setToken,
    setHost,
    build(kind) {
      switch (kind) {
        case 'ssh_key':
          return passphrase() === ''
            ? { private_key: privateKey() }
            : { private_key: privateKey(), passphrase: passphrase() };
        case 'https_token':
          return { username: username(), token: token() };
        case 'forge_token':
          return { host: host().trim(), token: token() };
      }
    },
    reset() {
      setPrivateKey('');
      setPassphrase('');
      setUsername('');
      setToken('');
      setHost('');
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
          <span>Forge host</span>
          <input
            type="text"
            name="host"
            required
            autocomplete="off"
            placeholder="git.example.com"
            value={props.draft.host()}
            onInput={(e) => props.draft.setHost(e.currentTarget.value)}
          />
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
        </label>
      </Match>
    </Switch>
  );
}
