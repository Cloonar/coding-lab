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
- **License**: the upstream skills are MIT-licensed (© Matt Pocock); the required notice
  is vendored alongside them as [`LICENSE.upstream`](LICENSE.upstream) and must travel
  with the bundle. The cloonar-local skills below are part of this project and covered
  by the repository's own license (`/LICENSE`).

## Cloonar-local skills (not from upstream)

- **`land-pr`** — validates a Forgejo (`git.cloonar.com`) PR, resolves conflicts, and merges
  it on explicit free-text approval, then closes the linked issue. Vendored from
  `cloonar-nixos/utils/home-manager/claude-code/skills/land-pr/`, with its `cloonar-nixos`-specific
  asides (pre-commit dry-build gate, `secrets.yaml`, `bento-upgrade` deploy, "no PR CI")
  genericised so it stays accurate for this Go repo. Referenced by `docs/agents/issue-tracker.md`.
  A future upstream bump must **not** delete these — they are not part of mattpocock/skills.
- **`update-docs`** — syncs the documentation with code changes made since the last
  recorded docs sync (`docs/.docs-sync` marker: SHA + date). Generic by design (the
  bundle is seeded into every repo lab manages): it follows the repo's own
  `docs/agents/docs-map.md` when one exists — this repo commits one — and otherwise
  derives the code-area → doc mapping by searching the doc tree for the changed
  identifiers. Written for this project (2026-08-16).

## Local prompt tuning

On top of the vendored rev, the files below carry local edits. An upstream bump replaces
upstream skill directories wholesale — re-apply these afterwards.

**2026-08, Claude 5 family** — more literal instruction-following, longer default
deliverables:

- `grill-me/SKILL.md` — questions explicitly wait for feedback before continuing (matches
  `grill-with-docs`).
- `to-prd/SKILL.md` — "don't interview" scoped to *re*-interviewing so it no longer
  contradicts the step-2 module check; user-story instruction de-inflated ("LONG …
  extremely extensive" produces padded lists on models that already write long).
- `triage/AGENT-BRIEF.md` — tracker-neutral wording (was GitHub/`gh`-specific).
- `land-pr/validation-core.md` (cloonar-local, survives bumps anyway) — explicit bar for
  `CONCERNS`, so higher-recall reviewers don't stall autoland's auto-merge on nitpicks.

**2026-10, Fable 5.1 / Opus 5.5** — the workflow changes of
[ADR-0070](../../docs/adr/0070-agent-brief-in-the-issue-body.md). The agents had outgrown
thin slices and a human pass over every issue; these edits move the human to the
decisions:

- `to-issues/SKILL.md` — rewritten around the fewest issues that can each land on their
  own (was "prefer many thin slices"), in the description too, since that line rides in
  every session. Issues of an approved plan are labeled for their destination
  (`ready-for-agent` / `ready-for-human`) instead of `needs-triage`, and each body is
  written as an agent brief. Links across skills to `../triage/AGENT-BRIEF.md`.
- `triage/AGENT-BRIEF.md` — the brief lives in the issue body (an `## Agent Brief`
  comment only on an issue someone else reported); file paths allowed as starting hints;
  new sections for decisions with reasons, how to verify, and assumptions to check; a
  body-form template and reworked examples.
- `triage/SKILL.md` — reproduce before recommending; recommendation and draft brief in
  one turn; the grilling skill referenced by file (`../grill-with-docs/SKILL.md` — it is
  `disable-model-invocation`, so `/grill-with-docs` cannot be invoked by the model);
  glossary/ADR decisions recorded in the brief instead of edited from the triage
  worktree; brief placement; an **unattended mode** for scheduled runs.
- `grill-me/SKILL.md`, `grill-with-docs/SKILL.md` — one added sentence: how the plan is
  cut into issues is one of the decisions to resolve. The interview itself (relentless,
  one question at a time) is deliberately untouched — the maintainer wants it that way.
- `land-pr/validation-core.md` — one sentence naming the issue's contract (the body, or
  the latest `## Agent Brief` comment), so the lander's diff-scope check reads the same
  text the AFK run implemented.

The recommended model/effort per workflow stage lives in
[`docs/model-selection.md`](../../docs/model-selection.md).

## Bundle contents

Upstream (mattpocock/skills): `caveman`, `diagnose`, `grill-me`, `grill-with-docs`,
`improve-codebase-architecture`, `setup-matt-pocock-skills`, `setup-pre-commit`, `tdd`,
`to-issues`, `to-prd`, `triage`, `zoom-out`. Cloonar-local: `land-pr` (see above).

## Bumping

1. Fetch the new upstream rev, re-apply the patch (regenerate with `diff -u` if it no
   longer applies), and replace the **upstream** skill directories here wholesale — leave
   the cloonar-local skills above (`land-pr`) in place.
2. Update the rev in this README.
3. Making the bundle source runtime-configurable is a roadmap item; until then this
   vendored copy is the single source embedded into the binary.
