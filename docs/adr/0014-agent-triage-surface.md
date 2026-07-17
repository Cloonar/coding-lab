# Agent triage surface: deliberate workflow actions write through the tracker seam

The maintainer's issue workflow runs on agent skills (triage, to-issues, to-prd): an agent in an interactive instance reads an issue, applies triage roles, files new issues broken out of plans, and closes what won't be fixed. Per D10 `labctl` is a session's **only** tracker surface — but until now it could only view, list, comment, and open a PR/CR, so every one of those workflows dead-ended inside the instance, on both tracker bindings.

This ADR extends the agent surface end-to-end — labctl → agent API → `Tracker` seam → both bindings — with the full triage set: issue create (labels attached at creation), label add/remove on an issue, label list, idempotent label create ("ensure"), and issue close. And it revises the ADR-0009 sentence "lab never writes labels or issue state to a forge" into the invariant it always meant:

- **What stays dead is the ENGINE writing tracker state implicitly.** Claim-is-the-branch is untouched (reference ADR-0013): no label ever encodes engine state, the `in-progress` label class stays deleted, and nothing in the scheduler/reaper/stop path writes to a tracker.
- **A deliberate agent workflow action flows through the tracker seam** to whichever backend the repo's tracker binding names — exactly as agent comments and PR/CR creation already did. The revision widens only the agent surface; operator mutations remain builtin-only (a forge-bound repo's operator UI still answers "manage issues on the forge").

The mechanics, pinned:

- **Seam**: `Tracker` grows `CreateIssue`, `AddIssueLabels`, `RemoveIssueLabels`, `Labels`, and `EnsureLabel`; `CloseIssue` (already in the seam) is exposed to agents. Callers speak label **names** only. Applying or attaching a name the repo does not define is `ErrUnknownLabel` (agent API: 400 naming it) — never an implicit create, so a typo becomes a loud error instead of a permanent garbage label. The metrics decorator forwards the new ops into the existing `(binding, op, result)` vocabulary and, per the ADR-0013 rule, keeps forwarding capability interfaces.
- **Forgejo binding**: name-to-ID resolution happens inside the client (Forgejo's labels-are-IDs quirk stays behind the seam), and strict resolution doubles as the loud-failure guarantee — Forgejo silently discards unknown label entries, the same quirk that already forces the ready queue's client-side verification. `EnsureLabel` is list-first with a re-list on the forge's duplicate answer, so it is idempotent whatever the forge does with duplicate names. ADR-0009's pagination and label-verification rules carry over unchanged.
- **Builtin binding**: label ops ride the existing `labels`/`issue_labels` tables; issue numbers come from the repo row's counter inside the insert transaction, as before. Created issues carry run attribution like comments (migration 0002 adds `issues.author_kind`/`run_id` — the one schema change, mirroring `issue_comments`), rescoped through the same `RunScoper` seam.
- **Agent API / labctl**: run-token-authenticated, repo-scoped routes; one token kind and one identical surface for interactive instances and AFK runs — the run token's repo scope stays the only security boundary. `issue list` output gains created-at and labels so the triage buckets are computable from one call. Close has no comment sugar: skills post the explanation, then close. Errors keep the canonical envelope and pinned exit codes.
- **Incogni**: the body sanitizer widens from PR/CR bodies to **every** agent-authored body — issue create and comment create included — so the defense-in-depth story has no unsanitized write path. A skill's disclaimer line is body content, not an attribution trailer, and passes through; running the triage workflow against an incogni repo is an operator-level contradiction (noted in ops.md), not something lab enforces.
- **Seeding**: the generated `CLAUDE.local.md` documents the full command set. Triage labels are still never created at repo add — `EnsureLabel` is agent-invoked, so forge label sets are only ever mutated on an invited, deliberate action.

## Status

Accepted. Revises the operator/engine-write sentence of ADR-0009 (which gains a status pointer here); everything else in ADR-0009 stands. The GitHub REST binding (fast-follow, #1) must implement the new seam ops when it lands. The "operator mutations remain builtin-only" sentence is itself revised by [ADR-0046](0046-operator-issue-edit-through-the-seam.md): operator title/body edits now flow through `Tracker.EditIssue` on every binding — everything else (create, state, labels, comments) stays builtin-only.

## Considered options

- **Keep the tracker read-only for agents and drive triage via the operator API.** Rejected: the operator API is cookie+CSRF-authenticated and unscoped per run; the run token's repo scope is the security boundary the whole agent surface is built on, and skills run inside sessions where only `LAB_URL`/`LAB_TOKEN` exist.
- **Auto-create labels on apply.** Rejected: a typo'd `--labels ready-for-agnet` would mint a permanent garbage label on the tracker; strict resolution plus an explicit idempotent ensure gives skills self-healing setup without silent writes.
- **Close-with-comment flag.** Rejected: two calls (comment, then close) match the existing convention and keep one wire shape per op.
- **Seeding the five triage labels at repo add on forges.** Still rejected (ADR-0009): lab does not mutate forge label sets uninvited; the ensure op is the invited path and skills run it unconditionally.
- **`issue edit` / `issue reopen`.** Deferred: no skill consumes them; add when a consumer exists.
