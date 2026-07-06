# Change requests merge from the UI: three-dot diff, ancestry-checked ff, merge commits in a throwaway worktree

A change request is the built-in tracker's pull request (D11): head branch onto base, `open → merged | closed`, the issues its body closes captured as `cr_closes` rows at create time by the shared closing-keyword grammar. `labctl pr create` on a builtin-bound repo files one through the agent API; the reaper's done-signal needs no special case because the builtin tracker's `Pulls` lists CRs in the same three-state PullRef vocabulary the Forgejo client uses. One head carries at most one open CR — a duplicate create (the agent retrying a timed-out call) is refused naming the existing number, the same 409 Forgejo answers.

Review and merge happen in lab (brief §9, verbatim): the diff is `git diff origin/<base>...<headSHA>` in the bare repo — three-dot merge-base semantics so base-side drift never pollutes the review, the *resolved sha* rather than the ref name so a concurrent Discard cannot yank the ref mid-request, output bounded at ~1MiB with an explicit truncation flag. The merge fetches origin first (fail-loud), then: if `origin/<base>` is an ancestor of the head, a pure fast-forward — `git push origin <headSHA>:refs/heads/<base>`; otherwise a merge commit built in a temporary detached worktree (`merge --no-ff`, authored with the repo's configured real identity per D15 measure 5, never a bot), pushed, the worktree always cleaned up. Push refusals surface git's stderr verbatim as the 409 body — the hook's own words are the actionable message. A merge conflict is typed too, carrying merge-ort's conflict report, which git writes to *stdout* — an stderr-only error shape renders a conflict as a blank "exit status 1". After any push, a refresh fetch updates the local origin ref the guarded-teardown merged-check reads.

Two invariants the review forced into shape:

- **Merge and close serialize per CR, and the merge is cancellation-immune.** The git work is a seconds-wide network window; a close landing inside it — or a phone dropping the connection after the push — would strand origin merged while the CR row reads closed or open-unrecorded. A per-CR mutex covers both mutations (with a state re-read under the lock), and everything from the decision to merge onward runs on a detached context. gitx itself stays convergent, not mutually exclusive: re-merging an already-merged head is a no-op returning the existing tip, which is also the crash-recovery story.
- **An open CR's head branch is owned.** The branch is the CR's reviewable substance and the retry path for an unrecorded merge — but a pushed-and-merged head reads *merged*, which is exactly what the branch GC eats. The sweep and startup pass B treat every open CR's head as owned, releasing it only when the CR leaves the open state.

Merging records `merged_at`/`merge_commit`, closes every `Closes #N` built-in issue (best-effort per issue), and publishes `cr.changed`/`issue.changed`. Closing without merging keeps the head branch and its parked work; a closed-unmerged CR reads as "no PR" to the reaper — the v0 pin that a closed PR is not a done-signal.

## Status

Accepted. Completes D11 (with ADR-0009's issue half); shipped in M6. Full circle proven by the builtin cycle integration test: claim → work → `labctl pr create` → reap success → UI merge → issue auto-closed → sweep GC of the merged head.

## Considered options

- **Merge in the bare repo without a worktree (`commit-tree` plumbing).** Rejected: `merge --no-ff` in a throwaway worktree gets conflict detection, rename handling, and merge-machinery semantics for free; the worktree is temporary by construction and cleaned on every path.
- **Record the merge before pushing.** Rejected: the DB must never claim what origin can contradict (D6); push first, record after, and make the retry convergent instead.
- **A `merging` interim state.** Rejected: a new state means a migration and UI surface for a window that a per-CR lock plus convergent retry already covers at this scale.
