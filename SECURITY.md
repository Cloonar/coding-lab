# Security policy

## Reporting a vulnerability

Report vulnerabilities through GitHub's private vulnerability reporting: repo Security tab → "Report a vulnerability", or go directly to
[github.com/Cloonar/coding-lab/security/advisories/new](https://github.com/Cloonar/coding-lab/security/advisories/new).

Do not file vulnerabilities as public issues. This is a solo-maintained project — expect a best-effort response, not a guaranteed SLA.

## Supported versions

Tagged releases exist (`v0.1.0` onward; prebuilt static binaries ship from each `v*` tag — see the README's release section), but only the latest `main` and the latest tagged release are supported; fixes land on `main` and reach a release with the next tag.

## Why this matters

lab holds credentials that reach real infrastructure and spawns agent sessions against the operator's own git repositories, so a few things are worth understanding:

- **Master key** — the vault encrypts every stored credential at rest with AES-256-GCM, keyed by a 32-byte master key kept in a 0600 file.
- **Run token** — each run gets its own short-lived `lab_run_…` credential, scoped to that run's repo and handed to the session as `LAB_TOKEN`; it is the agent's only tracker surface.
- **Runner** — the `host` runner is the deliberate break-glass: both Runner settings (global Settings → Runner and a repo's) warn "Host runs are unsandboxed — the agent has full host access to the server." whenever the runner they set is `host`, saving a switch of the global default to `host` asks first in a dialog naming how many repos inherit it, as does the New run page above its composer ("Runs on the host, unsandboxed, with full host access."); the `container` runner runs the pane command as rootless podman in the repo's dev image instead.
- **Preview listener** — off unless `--preview-addr` is set. When on, it serves every port an agent exposes with `labctl expose` at `https://<name>.<preview-domain>` — lab terminates TLS itself, with the operator's certificate or a leaf under a lab-minted CA — and with **no authentication**: anyone who can reach the listener can open every live preview. Traffic only ever goes to the run's own loopback (`127.0.0.1:<port>`, else `[::1]:<port>`), never to another host. Bind it to a trusted interface. The lab CA's key (`<state>/preview/ca.key`) can mint a certificate for any name a device that trusts the CA will accept, and a `host` run executes as the lab user, so keep the state directory private ([ops.md § Previews](docs/ops.md#previews), ADR-0084, ADR-0085).

### Already documented, not a vulnerability

- The `host` runner is unsandboxed by design — that is its stated purpose, not a bug.
- The preview listener has no authentication by design: reachable equals allowed. Who can reach it is set by where `--preview-addr` binds and where the wildcard DNS name resolves.
- Incogni mode's leak-prevention is bounded by design: it cannot hide the forge account of the token used, nor style/timing signals.
