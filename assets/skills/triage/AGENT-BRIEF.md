# Writing Agent Briefs

An agent brief is the specification an AFK agent works from: the contract for one issue. It is written when an issue becomes `ready-for-agent`, and a `ready-for-human` issue gets the same structure. Everything else on the issue — earlier wording, discussion, triage notes — is context.

## Where the brief lives

An issue has exactly one brief, in one of two places.

**In the issue body — the default.** The implementing agent reads the body as its contract, the AFK scheduler reads `## Blocked by` from the same place, and comments stay what they are good at: history and discussion. Write the body as a brief when you create an issue. When you triage an existing issue that a maintainer or one of these workflows wrote, rewrite its body into the brief, folding in everything the original text and the discussion established. Keep material worth having verbatim (logs, reproduction steps, a quoted decision) under an `## Original report` section at the end.

**In a comment — when someone else reported the issue.** A reporter's words are theirs: leave the body as they wrote it and post the brief as a comment headed `## Agent Brief`. The implementing agent treats the latest such comment as the contract and the body as background. To change a comment-form brief, post a new complete brief — the latest one wins, so never post a delta. Dependencies are the one thing that still goes in the body: the scheduler reads the `## Blocked by` section of the **issue body** only, so add or edit that section there and leave the rest of the reporter's text alone.

The tracker shows who filed an issue. Which accounts are maintainers comes from the maintainer you are working with, the prompt that started the run, or the repo's issue-tracker doc. If you can't tell, treat the issue as someone else's.

Never leave two specs behind. An issue whose body is a brief does not also get an `## Agent Brief` comment; when the contract changes, edit the body.

## Principles

### Behavioral, not procedural

Describe **what** the system should do, not **how** to implement it. The agent explores the codebase fresh and makes its own implementation decisions.

- **Good:** "The `SkillConfig` type should accept an optional `schedule` field of type `CronExpression`"
- **Bad:** "Open src/types/skill.ts and add a schedule field on line 42"
- **Good:** "When a user runs `/triage` with no arguments, they should see a summary of issues needing attention"
- **Bad:** "Add a switch statement in the main handler function"

### Point at the code, don't script it

Name the interfaces, types, function signatures and config shapes the agent should look for or change — they survive refactors. A file or package path is welcome as a **starting hint** ("session spawning lives in `src/session/`"): it saves the agent a search and costs nothing if the file has moved. Don't give line numbers, and don't turn a location into an instruction ("edit X in file Y") — the brief may wait in the queue while the code changes underneath it.

### Carry the decisions and their reasons

Everything settled before the brief was written — in a grilling session, by the maintainer, by an ADR — goes into the brief as a decision with its reason. The agent implements settled decisions; it does not reopen them. The reason matters as much as the decision: it is what lets the agent extend the decision correctly to a case the brief did not foresee.

### Complete acceptance criteria

The agent needs to know when it is done. Every brief has concrete, testable acceptance criteria, each independently verifiable.

- **Good:** "Listing issues carrying the `needs-triage` label on the issue tracker returns issues that have been through initial classification"
- **Bad:** "Triage should work correctly"

### Say how to verify

Name the commands, tests or observations that prove the work is done: the test suites and linters to run, a behavior to exercise, a check that should now fail where it used to pass. For a bug, include the confirmed reproduction.

### Flag what nobody verified

A brief often relies on facts no one checked: an upstream API's paths, a version's behavior, an assumption about how another subsystem works. List them, so the agent verifies each before building on it instead of trusting the brief.

### Explicit scope boundaries

State what is out of scope. This keeps the agent from gold-plating or making assumptions about adjacent features.

### Size the brief to the work

One issue is one unit of work that lands together. Don't shrink the scope to keep the brief short — a large, well-specified brief is fine, and a thin or vague one burns a whole agent run.

## Template

The body form. Leave out a section that has nothing to say (a small bug has no decisions to record); always keep Acceptance criteria, How to verify, Out of scope and Blocked by.

```markdown
## Summary

One or two sentences: what changes, and why it matters.

## Current behavior

What happens now. For bugs, the broken behavior and its reproduction.
For enhancements, the status quo the feature builds on.

## Desired behavior

What should happen once the work is complete.
Be specific about edge cases and error conditions.

## Decisions

Settled choices with their reasons. Implement these; do not reopen them.

- Decision — why.

## Key interfaces

- `TypeName` — what needs to change and why
- `functionName()` return type — what it returns today vs what it should return
- Config shape — any new configuration options
- Starting hint: where this lives today (a package or file, never a line)

## Acceptance criteria

- [ ] Specific, testable criterion 1
- [ ] Specific, testable criterion 2

## How to verify

- The commands, tests or observations that prove the criteria hold

## Assumptions to check

- A fact this brief relies on that nobody verified — check it before relying on it

## Out of scope

- Thing that should NOT be changed or addressed in this issue
- Adjacent feature that might seem related but is separate

## Blocked by

- #N

Or "None - can start immediately" if no blockers.
```

The comment form is the same brief under a single `## Agent Brief` heading, with each section title as a bold label (`**Summary:**`, `**Acceptance criteria:**`, …) and no Blocked by section — that one belongs in the issue body.

## Examples

Illustrative, not templates to copy — match the content to the issue, not the length of these.

### Body-form brief (enhancement)

```markdown
## Summary

Record rejected feature requests in an `.out-of-scope/` directory, so a repeat
request surfaces the earlier decision instead of re-litigating it.

## Current behavior

When a feature request is rejected, the issue is closed with a `wontfix` label
and a comment. There is no persistent record of the decision or reasoning.
Future similar requests require the maintainer to recall or search for the
prior discussion.

## Desired behavior

Rejected feature requests are documented in `.out-of-scope/<concept>.md` files
that capture the decision, the reasoning, and links to every issue that
requested the feature. When triaging new issues, these files are checked for
matches.

## Decisions

- One file per concept, not per issue — repeat requests should converge on one
  record rather than scatter across many.
- A human confirms every match — concept similarity is a judgment call, and a
  wrong automatic match would close a legitimate request.

## Key interfaces

- Markdown file format in `.out-of-scope/` — a `# Concept Name` heading, a
  `**Decision:**` line, a `**Reason:**` line, and a `**Prior requests:**` list
  with issue links
- The triage workflow reads all `.out-of-scope/*.md` files early and matches
  incoming issues against them by concept similarity
- Starting hint: the triage skill's "gather context" step

## Acceptance criteria

- [ ] Closing a feature as wontfix creates or updates a file in `.out-of-scope/`
- [ ] The file includes the decision, the reasoning, and a link to the closed issue
- [ ] If a matching file already exists, the new issue is appended to its
      "Prior requests" list rather than creating a duplicate
- [ ] During triage, existing files are checked and surfaced when a new issue
      matches a prior rejection

## How to verify

- Reject two requests for the same concept in a scratch repository: one file
  exists afterwards, listing both issues
- Triage a third matching request: the prior rejection is surfaced before any
  recommendation

## Out of scope

- Reopening previously rejected features
- Bug reports (only enhancement rejections are recorded)

## Blocked by

None - can start immediately
```

### Comment-form brief (bug, reported by someone else)

```markdown
## Agent Brief

**Summary:** Skill description truncation drops mid-word, producing broken output.

**Current behavior:**
When a skill description exceeds 1024 characters, it is truncated at exactly
1024 characters regardless of word boundaries. This produces descriptions
that end mid-word (e.g. "Use when the user wants to confi"). Reproduced with a
1100-character description.

**Desired behavior:**
Truncation breaks at the last word boundary before 1024 characters and appends
"..." to indicate truncation.

**Key interfaces:**
- The `SkillMetadata` type's `description` field — no type change needed, but
  the processing that populates it must respect word boundaries
- Any function that reads SKILL.md frontmatter and extracts the description

**Acceptance criteria:**
- [ ] Descriptions under 1024 chars are unchanged
- [ ] Descriptions over 1024 chars are truncated at the last word boundary
      before 1024 chars
- [ ] Truncated descriptions end with "..."
- [ ] The total length including "..." does not exceed 1024 chars

**How to verify:**
- A regression test with the 1100-character description from the report fails
  before the fix and passes after it

**Assumptions to check:**
- That 1024 is enforced in one place — look for a second truncation site before
  fixing only the first

**Out of scope:**
- Changing the 1024 char limit itself
- Multi-line description support
```

### Bad agent brief

```markdown
## Summary

Fix the triage bug.

## What to do

The triage thing is broken. Look at the main file and fix it.
The function around line 150 has the issue.

## Files to change

- src/triage/handler.ts (line 150)
- src/types.ts (line 42)
```

This is bad because:
- Vague description ("the triage thing is broken") with no current vs desired behavior
- Procedural: it tells the agent where to type instead of what should be true
- Line numbers, and locations given as instructions rather than hints
- No acceptance criteria and no way to verify
- No scope boundaries
