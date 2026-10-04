---
name: to-issues
description: Turn a plan, spec, or PRD into issues on the project issue tracker — the fewest issues that can each land on their own, written as agent-ready briefs. Use when user wants to convert a plan into issues, create implementation tickets, or break down work into issues.
---

# To Issues

Turn a plan into issues that an agent or a human can pick up and land. Aim for the **fewest issues that can each land on their own** — one issue for the whole plan is a good outcome, not a failure to break it down.

The issue tracker and triage label vocabulary should have been provided to you — run `/setup-matt-pocock-skills` if not.

## Process

### 1. Gather context

Work from whatever is already in the conversation context. If the user passes an issue reference (issue number, URL, or path) as an argument, fetch it from the issue tracker and read its full body and comments.

### 2. Explore the codebase (optional)

If you have not already explored the codebase, do so to understand the current state of the code. Issue titles and descriptions should use the project's domain glossary vocabulary, and respect ADRs in the area you're touching.

### 3. Draft the issues

An issue is one unit of work that lands together: one agent run, one pull request, verifiable on its own. Every extra issue costs another run, another review and another merge, and parallel issues that touch the same code conflict with each other — so a split has to buy something. Start from the whole plan as a single issue and split only where one of these holds:

<slicing-rules>
- The parts can land and be verified independently **and** the split gains something: they can run in parallel without touching the same code, one is blocked on something the other is not, or one needs a human and the other does not.
- The whole is more than one unattended run can plausibly finish. Judge by breadth — how many subsystems it touches and how much has to be verified — not by line count, and err on the side of keeping it whole: an integration spanning configuration, a client package, lifecycle hooks, an HTTP API, a UI section and its docs is a normal single issue for a current agent.
</slicing-rules>

Keep together what has to land together to make sense: a feature's schema, API, UI, tests and docs are one issue, and so are a decision record and the code that implements it.

When you do split, slice vertically: each issue cuts through every layer it needs end-to-end and is demoable or verifiable on its own. Don't slice by layer (all the schema, then all the API) — a horizontal slice can't be verified until its siblings land.

If the plan already settled the slicing — a grilling session often pins it ("one issue, one PR") — follow that decision instead of re-deriving it.

Issues may be 'HITL' or 'AFK'. HITL issues require human interaction, such as an architectural decision or a design review. AFK issues can be implemented and merged without human interaction. Prefer AFK over HITL where possible.

### 4. Quiz the user

Present the proposed breakdown as a numbered list. For each issue, show:

- **Title**: short descriptive name
- **Type**: HITL / AFK
- **Why it is separate**: what the split buys (omit when there is only one issue)
- **Blocked by**: which other issues (if any) must complete first
- **User stories covered**: which user stories this addresses (if the source material has them)

Ask the user:

- Should any of these be merged into one issue, or does anything need splitting out?
- Are the dependency relationships correct?
- Are the correct issues marked as HITL and AFK?
- Is any decision still open that the implementer should not make alone?

Iterate until the user approves the breakdown.

### 5. Publish the issues to the issue tracker

For each approved issue, publish a new issue to the issue tracker. Write its body as an agent brief — the body is the contract the implementer works from. Follow the body template and the writing rules in [the agent brief guide](../triage/AGENT-BRIEF.md), and add a `## Parent` section at the top that references the parent issue when the source was an existing issue.

The user's approval in step 4 is the triage decision, so label each issue for where it goes next instead of sending it back through triage:

- **AFK** — the `ready-for-agent` triage label.
- **HITL** — the `ready-for-human` triage label, with the brief saying what needs the human.
- **A decision is still open** — the `needs-triage` triage label, with the open questions listed in the body.

Add the category role (`bug` or `enhancement`) as well. Attach the labels in the create call: the body already carries the brief, so the label never announces a contract that isn't there yet.

Publish issues in dependency order (blockers first) so you can reference real issue identifiers in the "Blocked by" section.

The `## Blocked by` section is machine-read by the AFK scheduler, so write blockers as `#N` issue references. A prose-only blocker ("blocked by the auth work") does NOT block scheduling — the scheduler only sees `#N` refs. `None - can start immediately` (or any text without a `#N`) means unblocked. Cross-repo refs (`owner/repo#N`) are not evaluated. An issue labeled `ready-for-agent` with an open blocker is fine — the scheduler holds it back until the blocker closes.

Do not close or modify any parent issue.
