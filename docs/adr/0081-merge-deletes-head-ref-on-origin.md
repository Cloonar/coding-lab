# Merge deletes the head ref on origin

Merged head branches piled up on origin. ADR-0024 pinned "the head branch is never deleted on merge" and left the remote ref to whoever landed the PR, so the land-pr skill told the lander to run `git push origin --delete <head>` after a merge. The lander skipped that step often: this repository carried eight fully merged remote branches (`afk/1`, `afk/20`, `afk/23`, `fix/*`, `lab/*`) that had landed through it. Lab's own cleanup never reached the remote either. Guarded teardown and the reconcile sweep delete branches in the bare clone with a local `git branch -D` and nothing more.

ADR-0024's reason for the pin was that an open PR/CR's head is owned by teardown and the sweep, and that it is the retry path for an unrecorded builtin merge (ADR-0011). That argument covers only the window before the merge is durably recorded. Once the forge has confirmed the merge, or `store.MergeCR` has written the row, nothing retries through the head any more. The argument never covered the ref on a forge remote at all: teardown and the sweep own the branch in the bare clone, and on GitHub or Forgejo no part of lab ever owned the remote head.

A second, smaller leak came from the skill's conflict resolution, which said `git checkout <head>`. Worktrees share the bare clone's refs, so when the head was still checked out in another worktree (a parked AFK tree), git refused, and the lander improvised a sibling branch (`land/afk-58` exists here, its upstream gone). Such a branch is outside the managed prefixes and nothing ever sweeps it.

The maintainer decided in a grilling session on 2026-10-09 (issue #90) that the merge deletes the head ref on origin once the merge is durably recorded, and that the local branch lifecycle stays where it is. The decisions, pinned:

- **One global setting, on by default.** `merge_delete_head` is a boolean settings key, seeded explicitly as `"true"` by `SeedDefaultSettings` like the other boolean knobs, so an operator's `"false"` survives re-seeding. It is global only: there is no per-repo override. Global Settings shows it as one toggle, in General under Merging, with the hint "Deletes the PR/CR head branch on origin after a merge; local branches are unaffected." The setting is read in exactly two places, both with `GetBool(key, true)`: the agent handler behind `labctl pr merge` (`POST /agent/v1/prs/{n}/merge`) and the operator route `POST /api/v1/repos/{id}/crs/{n}/merge`, so a CR merged from the web UI behaves the same as one landed by an agent. The tracker adapters never read settings; they take the choice as an option.

- **The delete runs after the merge is recorded, never before.** On a forge binding it runs only after the forge confirms the merge. On the builtin binding it runs only after `store.MergeCR` has recorded the row. The per-CR mutex of ADR-0011 does not cover the delete and does not need to: a concurrent close is refused on the CR's state, so the delete may run after the unlock. The retry and convergence path of ADR-0011 and ADR-0024 is untouched, because the delete happens only where that path has nothing left to retry.

- **Where it lives.** `Tracker.MergePull(ctx, n, MergeOptions{DeleteHead})` returns a `MergeResult` that embeds the unchanged `PullRef` and adds the head outcome. `tracker.PullRef` itself does not change, since it sits on the reaper's hot path. The instrument and secretscan decorators pass the new shape through. GitHub and Forgejo merge as before, then apply the same-repo guard, then delete the head ref through the forge API. Forgejo's own `delete_branch_after_merge` flag stays off. On the builtin binding, `crmerge.Service.Merge` takes the same option and, after the row is recorded, makes a best-effort `git push origin :refs/heads/<head>` from the bare clone; the builtin adapter forwards the option.

- **A strict same-repo guard.** GitHub and Forgejo read `head.repo.full_name` from the pull and delete only when it equals the bound owner/name, compared case-insensitively. Anything else is kept, with the reason "head lives in <fork full name>". There is no allowance for a fork the forge token happens to own: lab deletes in the repository it is bound to and nowhere else. The builtin binding has no forks; a CR head is always a managed branch, which is its existing guard. Because the server enforces this, the land-pr skill's rule against deleting a branch the human does not own is removed.

- **Three head outcomes, and the merge stays a success in all three.** The merge result carries an outcome and a reason:
  - `deleted`: the head ref is absent on origin after the merge. An already-gone ref on a convergent re-merge counts, and so does a builtin head that was never pushed to origin.
  - `kept`: the setting is off (reason "setting off"), or the same-repo guard refused (reason "head lives in <fork full name>").
  - `failed`: a push or API refusal, or a network failure, carrying the backend's own words.

  The merge is the irreversible part. A delete failure must never turn a landed merge into an error, because a non-zero exit would make the lander retry or escalate a PR that is already merged. So `labctl pr merge` exits zero in all three cases, and the agent API's JSON response gains `head_outcome` and `head_reason`. A failed delete is logged at warn with the repo, the PR number and the head, like teardown's keep-or-delete lines.

- **labctl prints the outcome as a fourth field.** `labctl pr merge <n>` prints one tab-separated line: number, state, URL, then `head-deleted`, `head-kept: <reason>` or `head-delete-failed: <backend's words>`. The land-pr skill reads that field and relays it. On `head-delete-failed` it reports the words and stops; it does not fall back to a manual `git push --delete`.

- **The local branch lifecycle is unchanged.** Nothing about the branch in the bare clone moves. Guarded teardown and sweep pass B still own it, and still delete a merged head once its PR/CR leaves the open state, exactly as ADR-0011 describes. On the builtin binding the push-delete removes only origin's ref.

- **The lander resolves conflicts on a detached HEAD.** The land-pr skill fetches the head and checks out `origin/<head>` detached, merges `origin/<base>`, commits, pushes with `git push origin HEAD:refs/heads/<head>`, and returns with `git checkout -`. It never checks out the head branch and never creates a branch of its own, so no sibling branch can appear in the shared ref namespace. The rules are unchanged: an ordinary push, never a force push, never to the base.

## Known behaviour

An instance may still be on the head branch after its PR merges. This repository has manual branches that opened and landed two PRs in a row. When such an instance pushes again after the delete, the push simply recreates the ref on origin, and the merged PR keeps its head name on the forge. This is recorded here as expected behaviour. Lab does not check for a live instance before deleting.

## Status

Accepted 2026-10-09. Resolves issue #90, the GitHub issue.

- **ADR-0024:** narrows the pin "The head branch is never deleted on merge" and the rejected option "Delete the head branch on merge". The merge now deletes the head ref on origin once the merge is recorded; the branch in the bare clone is still never deleted by the merge, and the convergent re-merge is unchanged. ADR-0024 gains a status pointer here.
- **ADR-0011:** unchanged in substance. An open CR's head is still owned until the CR leaves the open state, the merge still pushes first and records after, and the sweep still GCs the merged local head. Only origin's ref is removed, after the record. ADR-0011 gains a status pointer here.

## Considered options

- **Turn on Forgejo's own `delete_branch_after_merge` flag.** Rejected: the forge would delete the branch itself, bypassing the same-repo guard and reporting nothing back, so lab could neither refuse a fork's head nor tell the lander what happened.
- **A per-repo override of the setting.** Deferred: no repo needs a different answer yet. The nullable per-repo column pattern used for other knobs can be added later without changing the global key.
- **A reconciler-side sweep of remote branches.** Out of scope: it would also have to judge branches merged outside lab, which is a different decision. The existing backlog of merged remote branches is cleaned up by hand, separately.
- **Keep the manual `git push origin --delete` in the skill.** Rejected: the step was skipped often enough to leave eight merged branches on this repository, and a web UI merge of a CR would never run it at all.
- **Fail the merge when the delete fails.** Rejected: the merge has already landed. A non-zero exit would make the lander retry or escalate a merged PR.

## Consequences

- Landing a PR on a forge binding with the setting on leaves no head branch on origin, and `labctl pr merge` prints `head-deleted`. With the setting off, the branch stays and the line ends `head-kept: setting off`.
- A failed delete leaves the merge recorded as merged and exits zero. The operator sees the backend's words in the lander's report and in the warn log, and deletes the branch by hand if it matters.
- Every caller of `Tracker.MergePull` passes `MergeOptions` and receives a `MergeResult`. Code that only needs the merged state reads the embedded `PullRef`.
- A live instance that pushes to a deleted head recreates the ref on origin. If that becomes a problem, a guard against live instances is the decision to revisit.
- Resolving a conflict through the land-pr skill creates no branch in the shared ref namespace.
