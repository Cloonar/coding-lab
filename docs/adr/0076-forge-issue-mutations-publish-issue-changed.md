# Issue and label mutations lab makes publish `issue.changed` on every tracker binding

`issue.changed` used to fire only for a builtin-bound mutation. Agent API mutations on a forge-bound (GitHub) repo published nothing, and the operator issue edit (ADR-0046) followed the same rule, on the reasoning that the forge is the source of truth and the operator UI reads it on navigation. That held while every issue view was a page visited fresh. The New run page (ADR-0075) is the home page: it stays mounted, fetches the open list once per repository selection and refetches only on `issue.changed` for that repo or on an SSE resync. So after a run created, edited, labelled or closed an issue through `labctl` on a GitHub-bound repo, the Issues card, its open count and the picker's state chips stayed stale until the operator reloaded or switched repositories (issue #74).

The decisions, pinned:

- **Every issue or label mutation lab performs publishes `issue.changed`, whatever the binding.** The agent API's `publishIssueChanged` no longer checks the tracker binding: issue create, issue edit, label add and remove, issue close and label ensure each publish one event once the tracker call succeeds. The operator issue edit publishes on a forge binding too. A failed tracker call still publishes nothing. The other operator mutations (create, state change, label set, comment, label CRUD) stay builtin-only behind their pinned 409/400, so they have no forge path to publish for.
- **Publish, not poll.** The server knows exactly when lab changed the list, so one event is exact and free. A focus refetch or an interval poll on the card would add forge reads and still catch nothing lab did that the event misses.
- **The event shape is unchanged.** `{type, repoID}`; every subscriber already matches on `repoID`, so no web change is needed.
- **Changes made directly on the forge still publish nothing.** Lab sees no signal for them without polling; that is a separate decision.

## Status

Accepted 2026-10-08. Resolves issue #74. Supersedes the "A forge-bound edit publishes no bus event" half of the last decision in [ADR-0046](0046-operator-issue-edit-through-the-seam.md) (which gains a status pointer here); its "last write wins" half stands.
