# coding-lab

## Agent skills

### Issue tracker

Issues live on GitHub at github.com/Cloonar/coding-lab, managed with `labctl`. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles map 1:1 to repo labels of the same names. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.

### Docs sync

The `update-docs` skill syncs documentation with code changes since the last
sync (`docs/.docs-sync` marker). This repo's code-area → doc mapping and
docs-site rules live in `docs/agents/docs-map.md`.
