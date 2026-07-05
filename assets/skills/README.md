# Vendored agent skills bundle

Skills seeded by `lab` into every agent worktree (`.claude/skills/`, excluded from git
via `.git/info/exclude`). See `docs/agent-brief.md`, decision D13.

- **Upstream**: https://github.com/mattpocock/skills
  rev `4369256220a6f69b3a66dd140e1162967a68376c`
- **Local patch applied**: `tdd-refactor-the-tests-too.patch` (from
  `cloonar-nixos/utils/home-manager/claude-code/patches/`) — the tdd Refactor step also
  consolidates point tests into table/contract tests.
- **Vendored from**: the installed (patched) home-manager state on 2026-07-05, i.e. the
  exact bundle in production use on the dev host.

## Bundle contents

`caveman`, `diagnose`, `grill-me`, `grill-with-docs`, `improve-codebase-architecture`,
`setup-matt-pocock-skills`, `setup-pre-commit`, `tdd`, `to-issues`, `to-prd`, `triage`,
`zoom-out`.

## Bumping

1. Fetch the new upstream rev, re-apply the patch (regenerate with `diff -u` if it no
   longer applies), and replace the skill directories here wholesale.
2. Update the rev in this README.
3. Making the bundle source runtime-configurable is a roadmap item; until then this
   vendored copy is the single source embedded into the binary.
