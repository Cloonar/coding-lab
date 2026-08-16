# Docs map — where code changes land in the documentation

Consulted by the `update-docs` skill (and anyone syncing docs by hand): which
documentation files a change in each code area can affect, plus the rules of
this repo's docs site. The sync baseline lives in `docs/.docs-sync` (line 1:
last-synced commit SHA, line 2: date).

**Usage-first.** The docs are written for the person *using* lab — operator
or agent — not for the person developing it. New doc text explains what to do
and what happens, in workflow order; implementation rationale belongs in the
ADRs and code comments, and developer material stays in its own pages
(`CONTRIBUTING.md`, `definition-of-done.md`, `provider-authoring.md`, the
ADRs — grouped under Contributing/Decisions in the site nav). When a new
feature lands, its *usage* story (README feature bullet, a getting-started
step if a first session meets it, the ops.md how-to) is the required part;
paragraphs about internals are not.

## Area → documentation

| Code area | Docs that may need updating |
| --- | --- |
| `cmd/lab`, `internal/config` | `docs/ops.md` (flags, env vars, defaults), `docs/getting-started.md` |
| `cmd/labctl`, `internal/labctl`, `internal/agentapi` | `docs/agents/issue-tracker.md`, `README.md` (labctl block in "Surfaces at a glance") |
| `internal/httpapi` | `README.md` (surfaces/endpoints), `docs/ops.md` (auth, probes, forward-auth) |
| `internal/provider*` (incl. adapters, `providertest`) | `docs/agents/provider-authoring.md`, `docs/model-selection.md`, `README.md` features |
| `internal/tracker`, `internal/afk`, `internal/crmerge` | `README.md` (AFK loop), `docs/agents/triage-labels.md`, `docs/getting-started.md` |
| `internal/vault`, `internal/secrets`, `internal/onecli`, `internal/credrotate` | `docs/ops.md` (vault, OneCLI credential gateway), `README.md` |
| `internal/store`, `migrations/` | `docs/ops.md` (database, backup/restore) |
| `internal/metrics`, metric label vocabularies (e.g. `internal/tracker/instrument.go`, run kinds/outcomes in `internal/store/runs.go`) | `docs/ops.md` § Metrics — the label enums there must list every value the code can emit |
| `internal/instance`, `internal/instancehome`, `internal/seeder`, `internal/podmanx`, `containers/` | `docs/ops.md` (runners, state directory tree), `README.md` |
| `web/` | `docs/getting-started.md` (UI walkthrough), `README.md` (feature claims), `SECURITY.md` (quoted UI labels) |
| `nix/`, `flake.nix` | `docs/ops.md` (NixOS module options, unit PATH/invariants), `docs/getting-started.md`, `README.md` requirements |
| `Makefile`, `.github/workflows/`, `.forgejo/workflows/` | `CONTRIBUTING.md`, `docs/definition-of-done.md` |
| release tags / packaging | `README.md` (release section), `SECURITY.md` (supported versions) |
| new/renamed packages, new domain terms | `CONTEXT.md` (glossary), `docs/agents/domain.md` |
| `assets/skills/` | `assets/skills/README.md` (bundle manifest) |

The mapping is a starting point, not a fence — if a diff plainly affects a doc
not listed here, update that doc too.

## Docs-site rules

Read the comments in `mkdocs.yml` and `mkdocs_hooks.py` before touching
either file. In short:

- A **new** doc page must be added to `nav:` in `mkdocs.yml` unless it is
  agent-facing (`docs/agents/*` is excluded by default; only
  `provider-authoring.md` is published).
- `README.md`, `CONTEXT.md`, `CONTRIBUTING.md` are injected as virtual pages
  by `mkdocs_hooks.py` — never copy them under `docs/`.
- Validate with `nix develop .#docs --command mkdocs build --strict` (remove
  the generated `site/` afterwards).

## Records, not living docs

- `docs/adr/*` and `docs/agent-brief.md` are dated records: never rewritten
  to match new code. Contradictions get flagged for a new ADR/amendment;
  agent-brief.md carries a "historical snapshot" banner listing known drift.
- `docs/definition-of-done.md` **is** living — its claims (CI shape, test
  citations) must track the code.
