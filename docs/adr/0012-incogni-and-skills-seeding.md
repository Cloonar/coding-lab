# Skills seeded into every worktree; incogni is leak-proof by construction, not by hope

Every spawn — manual or AFK — gets the vendored skills bundle copied into `<worktree>/.claude/skills/` and a generated `CLAUDE.local.md` at the worktree root carrying the repo's tracker binding, the `labctl` vocabulary, the five-triage-label table, and an explicit note that `labctl` supersedes any committed `tea`/`gh` instructions (D13). Everything lab seeds is listed in `.git/info/exclude` — never `.gitignore`, so lab's files never appear in a diff the operator reviews and never become a committable artifact. The bundle is embedded in the binary (`//go:embed all:skills`), so a fresh host with no user-level Claude install seeds identically.

Incogni (D15) is a per-repo flag whose seven measures are defense in depth, each independently sufficient to keep a given leak out: attribution disabled in the worktree's `.claude/settings.local.json`; the seed prompt instructing plain commits; the agent API stripping attribution footers from PR/CR bodies; neutral branch names from the repo's own patterns; commits authored with the repo's real identity; every seeded file excluded; and a pre-push hook in the bare reference repo that rejects any push carrying AI markers or seeded paths. The operator docs state honestly what incogni *cannot* hide — the forge account identity of the token, and statistical style/timing signals.

The review of this milestone was where "leak-proof by construction" earned its keep — the guard, meant to be the last line, had leaks of its own:

- **The pre-push hook now fails CLOSED.** Its scan loop was `for commit in $(git rev-list $range)`; a `rev-list` that could not compute the range — a force-push over an unfetched remote tip, or a SHA-256 repo whose 64-zero new-branch sentinel didn't match the hardcoded 40 zeros — printed a fatal to stderr, yielded an empty list, scanned nothing, and exited 0. A poisoned commit pushed straight through. The range is now enumerated with an explicit failure check, the zero sentinel matches any hash length, and an unscannable ref refuses the push.
- **Merge commits are scanned.** `git diff-tree` prints nothing for a merge without `-m`, so a file the merge itself introduced (an evil merge, a conflict resolution dropping in `settings.local.json`) was invisible to the path check. `-m` closes it.
- **The guard cannot be routed around.** A global/system `core.hooksPath` (husky and friends set one) takes precedence over the bare repo's hooks dir, silently disabling the guard for every agent push. Install now pins the bare repo's *local* `core.hooksPath` to the absolute hooks directory, and a startup pass reconciles every ready repo's guard against its incogni flag so a crash between a toggle and its hook op self-heals.
- **The guard blocks only what lab seeds, and matches Anthropic by email.** The path check was derived from the exclude set (`.claude/`), which would reject a repo's *own* tracked `.claude/` content and strand the run; it now matches only lab's actual seeded paths. Both the hook and the body sanitizer key the `Co-Authored-By` match on a `@anthropic.com` email in addition to a "Claude" display name — the model name is variable (this family already renamed to Fable), the email is the stable discriminator.

## Status

Accepted. Implements D13 and D15's seven measures; shipped in M7. The full incogni cycle is proven end to end by the acceptance test: a run on an incogni repo claims a neutral branch, the agent commits cleanly, the API sanitizes a poisoned PR body, and the remote shows zero AI markers with the configured author — while a run that commits a poisoned trailer has its push rejected by the guard.

## Considered options

- **Derive the guard's path patterns from the exclude set (one source of truth).** Rejected after review: the exclude set is a deliberately broad superset (harmless for `.git/info/exclude`, which only hides untracked files) but overbroad as a push guard. Seeded-path patterns are their own explicit list.
- **Trust `$GIT_DIR/hooks` resolution.** Rejected: `core.hooksPath` out-precedences it; the guarantee has to be pinned in the bare repo's local config.
- **Match attribution on the "Claude" display name only.** Rejected: a model rename leaks through; the `@anthropic.com` email is the durable signal.
