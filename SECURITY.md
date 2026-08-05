# Security policy

## Reporting a vulnerability

Report vulnerabilities through GitHub's private vulnerability reporting: repo Security tab → "Report a vulnerability", or go directly to
[github.com/Cloonar/coding-lab/security/advisories/new](https://github.com/Cloonar/coding-lab/security/advisories/new).

Do not file vulnerabilities as public issues. This is a solo-maintained project — expect a best-effort response, not a guaranteed SLA.

## Supported versions

There are no tagged releases yet. Only the latest `main` is supported; fixes land there.

## Why this matters

lab holds credentials that reach real infrastructure and spawns agent sessions against the operator's own git repositories, so a few things are worth understanding:

- **Master key** — the vault encrypts every stored credential at rest with AES-256-GCM, keyed by a 32-byte master key kept in a 0600 file.
- **Run token** — each run gets its own short-lived `lab_run_…` credential, scoped to that run's repo and handed to the session as `LAB_TOKEN`; it is the agent's only tracker surface.
- **Runner** — the per-repo `host` runner is the deliberate break-glass, labeled "unsandboxed — full host access" in the UI; the `container` runner runs the pane command as rootless podman in the repo's dev image instead.

### Already documented, not a vulnerability

- The `host` runner is unsandboxed by design — that is its stated purpose, not a bug.
- Incogni mode's leak-prevention is bounded by design: it cannot hide the forge account of the token used, nor style/timing signals.
