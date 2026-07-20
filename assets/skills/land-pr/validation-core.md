# Validation core — what "landable" means

The single definition of **landable**. The interactive `land-pr` skill reads this
file, and the autoland lander reads it too — it is the *one* source, so keep the
definition here and reference it, never restate it. Self-contained on purpose: no
repo-relative links, because this file is seeded next to `SKILL.md` in every
managed repo's worktree.

A PR is **landable** when three things hold — its **checks** vouch, its
**conventions** are met, and any **conflict** is resolved — and the verdict rolls
up to `PASS` (or `CONCERNS` that no one has marked blocking). Everything below
defines those terms and the verdict they feed.

## Checks — the CI verdict

The check verdict comes from `labctl pr checks <N>` — add `--wait` to poll an
in-flight run to rest (~10s poll, ~5 min cap, all inside the one call). It prints
an aggregate line, then one tab-separated row per check.

The aggregate is one of four words, precedence `failure > pending > success`:

- **`success`** — every check green. A signal vouches; **do not re-run** what it
  already ran. Name the signal you relied on in the verdict.
- **`failure`** — at least one red row (a red row counts the moment it appears,
  even while other jobs still run). A **blocker**: report the failing rows. The
  fix belongs to the PR author, not the lander.
- **`pending`** — still running, nothing red yet. **Never land on pending** —
  wait it out (`--wait`) or stop for the human.
- **`none`** — zero check rows: no CI is configured, or the repo gates at commit
  time where `checks` can't see it. Nothing vouches — run the project's own
  checks now (learn them from the repo's `CLAUDE.md`, else ecosystem
  auto-detect: `go.mod`, `package.json`, `composer.json`, `flake.nix`,
  `Makefile`).

Exit codes for the verb: `0` = success or none · `2` = failure · `3` = still
pending at the cap. (`1` folds in every other error — usage, env, transport,
API.)

Know the reach of the signal you trust: if CI splits so that an expensive gate
runs only conditionally (e.g. a build gate behind dependency-file changes), a
green aggregate may not have exercised the path this diff touches. Call that out
when it matters.

## Conventions — what the PR must satisfy

- **Conventional Commits title** — `feat:`, `fix(scope):`, `docs:`, `perf:`, …
  Apply any extra title rules the repo's `CLAUDE.md` states.
- **`Closes #<n>` linkage** — the body must carry a working `Closes #<issue>`
  tying the PR to the issue it resolves. For an AFK run (head branch `afk/<N>`)
  this is **non-negotiable**: without it the merge won't auto-close the issue,
  which then lingers open while its `afk/<N>` branch holds the claim — parked out
  of the ready queue until the branch is deleted. A missing or malformed link is
  a blocker.
- **The repo's own rules** — whatever `CLAUDE.md` declares: build / test / lint
  commands, the gate model, commit conventions. If a rule already ran as a gate,
  it vouches — read it, don't re-run it.
- **Diff-scope sanity** — the diff matches the linked issue's stated intent and
  nothing else. Flag drive-by changes unrelated to the issue; if it's ambiguous
  whether a divergence is intentional, escalate to the human rather than guess.

## Conflict policy

- **Mergeability is the backend's call — never pre-reason it.** Do not pre-flight
  required checks or branch protection. Attempt the merge; if the forge or a
  pre-receive hook refuses (required check red, protected base, conflict),
  surface the refusal's own words **verbatim**.
- **Resolve on the PR head branch only** — never on the base, never a
  force-push. Bring the base in with a merge (`git merge origin/<base>`), never a
  rebase (a rebase force-push rewrites the open PR).
- **Auto-resolve silently only when the resolution is deterministic and
  behaviour-preserving** — disjoint additions both sides made (union),
  regenerated lock/generated files, pure formatting/whitespace.
- **A semantic conflict escalates to a human — never silently pick a side.** The
  moment both sides change the same statement/value/logic differently, or one
  side deletes what the other edited, stop and ask the single question you need,
  showing both sides; apply the answer. For a genuinely tangled, multi-point
  resolution, escalate to a full grilling conversation.
- **A resolution commit is one no gate has seen** — the "a signal already
  vouches" shortcut lapses. Re-verify the resolved tree with the project's own
  checks, and show the resolution diff before the merge gate.

## Verdict — PASS / CONCERNS / FAIL

Emit exactly one verdict, with concrete **file-and-line** findings, and **name
the verification signal you relied on** (which aggregate, or which checks you ran
yourself).

- **`PASS`** — landable as-is. Nothing blocks.
- **`CONCERNS`** — landable, but with noteworthy findings. Non-blocking by
  default (worth the human's eye, doesn't stop the merge). A concern **becomes
  blocking only when explicitly marked blocking** — then nothing lands until it
  is resolved. Where no human is in the loop (an autoland lander), any
  `CONCERNS` caps the outcome at approve-only — auto-merge needs a clean
  `PASS`.
- **`FAIL`** — must not land. Findings are actionable: say what to fix, and
  where.

What each verdict leads to:

- **`PASS`** → the merge path: approve the PR (`labctl pr approve`), then merge
  on whatever confirmation the invoking mode requires — the interactive skill
  waits for the human's free-text go-ahead; an autoland lander merges only on a
  clean `PASS` with `auto_merge` on, and otherwise stops at the approve.
- **blocking `CONCERNS`, or `FAIL`** → **reject** with the findings as the review
  body (`labctl pr reject`). The PR stays open in *changes-requested* for the
  author (or an AFK re-run) to fix; re-invoke later.
