# Credentials encrypted at rest; per-op materialization; the agent never sees a forge token

Credentials live in the database encrypted with AES-256-GCM under a 32-byte master key whose file path is configurable (`--master-key-file`, default `<state>/master.key`; point it at a sops-nix / `LoadCredential` path on NixOS). The file is 64 hex chars, auto-generated 0600 on first start — published via `os.Link` so two racing first-starts get exactly one winner — and lab refuses to start if the perms are looser than 0600 (D9). Each encryption uses a fresh random 12-byte nonce; blobs are stored `nonce||ciphertext`. Payloads are kind-specific JSON: `ssh_key {private_key, passphrase?}`, `https_token {username, token}`, `forge_token {host, token}`. Secrets are write-only through the API — create and rotate, never read back — and delete is refused with 409 while any repository references the credential through either FK column.

Key material touches disk only inside `<state>/runtime/` (0700), materialized per operation and removed when the operation ends. Filenames are per-op — `<credID>.<opID>.key|.askpass|.sshpass` — because two concurrent operations sharing one credential must not unlink each other's files; the review that forced this found exactly that race between two clones of repos sharing a token. Git auth is env-only: `GIT_SSH_COMMAND` with `-i <key> -o IdentitiesOnly=yes -o UserKnownHostsFile=<state>/runtime/known_hosts -o StrictHostKeyChecking=accept-new`, or a `GIT_ASKPASS` helper script for HTTPS tokens; passphrase-protected SSH keys get an `SSH_ASKPASS` helper with `SSH_ASKPASS_REQUIRE=force`. Tokens never appear in URLs, repo config, argv, or logs; single-line validation (no CR/LF/NUL) keeps token-bearing values inside the line-oriented askpass protocols.

The load-bearing split: a repository has **two** credential slots. `credential_id` (ssh_key or https_token) is the *git* credential — it is materialized into clone/fetch/push env and handed to spawned sessions so the agent's own `git push` works (D10 allows exactly this). `forge_credential_id` (forge_token) is the *tracker* credential — it is used server-side by the Forgejo REST client and **never** reaches a session's environment or the runtime dir. One combined slot would hand every agent a forge-API-capable token and defeat run-token scoping; `tracker_binding = forge` therefore also requires a forge credential to be attached, and auto-detection only resolves to `forge` when one is.

## Status

Accepted. Implements D9 (with D10's boundary); shipped in M2.

## Considered options

- **One credential slot per repo.** Rejected: for an HTTPS-cloned forge repo the same token would do REST auth *and* be materialized into the agent session — the agent could touch any repo that token can. The whole point of run tokens (D10) is that agents get per-repo, tracker-scoped credentials only.
- **Per-credential materialized filenames with refcounting.** Rejected in favor of per-op filenames: refcounts need shared mutable state and still leak on crash; per-op names make cleanup trivially correct and crash-orphans reapable by pattern.
- **External secret manager (vault server, sops at runtime).** Rejected: lab is self-contained (D2); sops integration happens at the *master key file* boundary, which NixOS provisions.
