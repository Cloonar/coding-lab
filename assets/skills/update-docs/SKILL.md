---
name: update-docs
description: Update the documentation to match code changes made since the last docs sync, using the docs/.docs-sync marker as the baseline. Use when user says "update the docs", "sync docs", "docs drift", "bring documentation up to date", or after landing code changes that touched user-facing behavior.
---

# Update docs from code changes

Bring the documentation back in line with the code, covering everything that
changed since the last recorded sync. The docs are a promise to users — every
command, flag, endpoint, and default they name must be true of the code as it
is **now**, not as it was when the paragraph was written.

## 1. Find the baseline

The last synced commit is recorded in `docs/.docs-sync` (a committed file:
first line the full commit SHA, second line the date `YYYY-MM-DD`).

- If the file exists: `BASE=$(head -1 docs/.docs-sync)`.
- If it does not exist (first run): use the newest commit whose subject starts
  with `docs:`; if none in the last ~50 commits, tell the user there is no
  baseline and ask how far back to sync before doing anything else.
- If `BASE..HEAD` contains no code changes, report "docs already in sync" and
  stop.

## 2. Collect the code changes

```
git log --oneline BASE..HEAD -- . ':(exclude)docs' ':(exclude)*.md'
git diff --stat BASE..HEAD -- . ':(exclude)docs' ':(exclude)*.md' ':(exclude)*lock*' ':(exclude)*.sum'
```

Read the commit subjects first — they usually name the user-visible change.
Then group the touched paths by area.

## 3. Map changed code to affected docs

- If the repo defines a docs map (`docs/agents/docs-map.md`), follow it — it
  carries the repo's area → doc-file mapping and any site-build rules, and it
  overrides the generic steps below where they conflict.
- Otherwise derive the mapping mechanically: for each changed path, package,
  command, flag, endpoint, or renamed identifier, search the doc tree
  (`README*`, `docs/`, `CONTRIBUTING*`, and any glossary/context file) for
  mentions of it. A doc that names a changed thing is affected.
- Either way, also consider docs that *should* mention a new feature but
  don't: a new user-facing capability with zero doc hits is a gap, not a pass.

## 4. Update the docs

For each affected doc:

1. Read the doc section that covers the changed area.
2. **Verify against the current code, not the diff.** Open the flag/route/
   interface definition as it exists now and write from that. A diff shows
   what moved, not the whole current truth.
3. Edit in the doc's existing voice and depth. Match surrounding style; don't
   restructure sections the change doesn't touch.
4. If a documented claim was already wrong *before* BASE (pre-existing drift
   you notice along the way), fix it and mention it in the report.

Standing rules:

- **Usage-first.** Write for the person using the feature: what to do and
  what happens, in workflow order. Implementation rationale belongs in
  decision records and code comments, and developer material belongs in the
  contributor pages — don't let it leak into user-facing docs.
- **Decision records (ADRs and dated briefs) are records, never edited to
  match new code.** If code now contradicts one, flag it in the report — a
  new record or an amendment is the feature author's job.
- New doc pages must be wired into the repo's docs-site config (nav, index)
  if one exists; validate with the site's own build command when available,
  otherwise check every link you added or moved resolves to a real file.

## 5. Record the sync and commit

1. Write `docs/.docs-sync`: line 1 `git rev-parse HEAD` (the HEAD you synced
   against), line 2 today's date.
2. Commit the doc edits together with the marker:
   `docs: sync with code changes <short-BASE>..<short-HEAD>`
   (body: one bullet per doc file, saying which code change it reflects).
   Do not push unless asked.

## 6. Report

End with a short summary: which code changes had doc impact and what was
updated; which changes were checked and needed nothing; any decision-record
contradictions or open questions flagged for a human.
