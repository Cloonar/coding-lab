# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual label strings used in this repo's issue tracker (Forgejo — see `issue-tracker.md`).

| Label in mattpocock/skills | Label in our tracker | Meaning                                  |
| -------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`       | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-info`         | Waiting on reporter for more information |
| `ready-for-agent`          | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`    | Requires human implementation            |
| `wontfix`                  | `wontfix`            | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string from this table.

`ready-for-agent` alongside a live `## Blocked by` reference is a valid combination: don't withhold the label until blockers merge. The AFK scheduler reads the `## Blocked by` section and orders the work itself, holding a blocked issue back until its referenced issues close. Blockers must be `#N` refs in the **issue body** — prose and comments are invisible to the scheduler, so promote a dependency discovered later into the body's `## Blocked by` section by editing the issue.

All five labels exist on the Forgejo repo. Forgejo rejects applying a label that doesn't exist — if you change a string here, create the label first: `labctl label create --name "..." [--color "#..." --description "..."]` (idempotent, create-if-missing).

Edit the right-hand column to match whatever vocabulary you actually use.
