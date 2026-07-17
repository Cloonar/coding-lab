# One Tracker vocabulary over Forgejo REST and the built-in issue tracker

Everything that reads or writes issue state goes through the `Tracker` interface: ready queue, issue read with comments, comment create, pulls listing, pull create, issue close. Two implementations ship in the MVP — a Forgejo REST client and the built-in tracker backed by lab's own tables — resolved per repository from `tracker_binding` by a registry (GitHub REST is fast-follow, #1). The `tea` CLI dependency is gone entirely (D10): the forge client speaks `https://<host>/api/v1` directly, authenticated by the repo's forge credential (`Authorization: token …`), with the host taken from the credential payload and owner/repo parsed from the remote URL. The forge token is used server-side only; it never reaches a session (ADR-0006).

The semantics ported from v0 are the ones that carry the AFK engine (M5):

- **Pulls are fetched in ALL states.** The done-signal is "a PR whose head branch equals the run branch, open or merged" — listing only open PRs silently misclassifies a quickly-merged run as a timeout. Head-collision precedence is open > merged > closed, as pure functions (`PullState`, `PRPresent`) over the pull list.
- **The ready queue is verified client-side.** Forgejo discards nonexistent label names from `labels=` filters instead of returning an empty set — a fresh forge repo without a `ready-for-agent` label would otherwise answer the ready queue with *every open issue*, and the scheduler would happily claim work nobody marked ready. The client keeps the server-side filter as an optimization but only returns issues whose decoded labels actually contain `ready-for-agent`.
- **Pagination terminates on an empty page, not a short one.** Forgejo clamps `limit` to the instance's `MAX_RESPONSE_ITEMS`; a short-page exit is only correct while the client's page size happens to be ≤ the clamp. The per-issue comments endpoint ignores pagination parameters entirely and always returns the full list — it is fetched with a single un-paginated GET (a paginating loop never terminates once an issue has ≥ page-size comments).

The built-in tracker (D11) stores issues, comments, and labels in lab's own tables: per-repo issue numbers allocated from the repo row's counter inside the insert transaction, the five triage labels seeded at repo creation, comments attributed to the operator or to a run. It implements the same `Tracker` interface, so the M5 agent surface and reaper are binding-agnostic. Operator mutations are builtin-only — a forge-bound repo answers 409 ("manage issues on the forge"); lab never writes labels or issue state to a forge (claim-is-the-branch, reference ADR-0013, killed the label-writing bug class and it stays dead).

## Status

Accepted. Implements D10's tracker seam and D11's issue half; shipped in M4. Change requests (built-in PRs, diff, merge) complete D11 in M6. The "lab never writes labels or issue state to a forge" sentence is revised by [ADR-0014](0014-agent-triage-surface.md): engine-initiated tracker writes stay dead, but a deliberate agent workflow action (issue create, label ops, close) flows through the seam to the bound tracker. The "Operator mutations are builtin-only" clause is narrowed by [ADR-0046](0046-operator-issue-edit-through-the-seam.md): an operator title/body edit flows through `Tracker.EditIssue` on every binding — everything else (create, state, labels, comments) stays builtin-only.

## Considered options

- **Keep `tea` for Forgejo.** Rejected by D10: a CLI dependency with interactive edge cases, per-user login state, and no per-repo token scoping; REST with the repo's forge credential is smaller and testable against `httptest`.
- **Write triage labels to the forge at repo add.** Rejected: lab does not mutate forge label sets uninvited; the ready queue tolerates a missing label by verifying membership client-side, and the operator creates the label when they adopt the workflow.
- **Trust server-side label filtering.** Rejected after live verification of the discarded-label behavior — correctness cannot depend on a label existing.
