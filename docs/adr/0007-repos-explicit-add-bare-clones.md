# Repositories are added explicitly and cloned by lab into bare reference clones

A repository enters lab through the UI: the operator supplies a remote URL, picks a credential, and lab clones it itself into a lab-owned **bare** reference clone at `<state>/repos/<repoID>.git` (D8). There is no directory scanning and no linking of pre-existing checkouts. The clone runs as an async job — the API answers immediately with `clone_status = cloning`, progress streams over SSE (`clone.progress`, parsed from git's stderr percentages), and completion flips the row to `ready` with the detected default branch, or to `error` with the stderr tail (scrubbed of any credential material). A repo stuck in `cloning` after a crash is healed at startup: probe the bare dir anchored via `--git-dir` (never a walk-up from the daemon's cwd), re-derive the default branch from the bare HEAD symref offline, and mark `ready` or `error` accordingly — the DB is never the only witness to a clone that the filesystem contradicts.

Bare is the load-bearing property: a bare clone has no working tree, so it is *structurally* never dirty, and it is the native parent for the per-instance worktrees that M3 adds (reference ADR-0017 explains why every instance gets its own worktree). The clone also configures `+refs/heads/*:refs/remotes/origin/*` at creation so `refs/remotes/origin/<branch>` exists from the start — worktree forks and merged-checks compare against the local origin ref.

Repo identity: `name` is derived from the URL basename and sanitized with the v0 scanner rules (only `[A-Za-z0-9_-]`, `.` → `_` — tmux parses `.` as window.pane, so a dot in a name breaks every session lookup), unique per instance, editable. `forge_kind` is detected from the remote host (the v0 detection table, extended with github.com → `github`); `tracker_binding` resolves at add time — `auto` becomes `forge` only when a forge host is detected *and* a forge credential is attached, else `builtin` (ADR-0006 explains why the credential is required). Incogni repos seed neutral branch patterns (`issue-<N>`, `wip/`) at creation; toggling incogni later never silently rewrites patterns. The five triage labels are seeded into the built-in tracker on create so a builtin-bound repo is immediately usable.

Removal is guarded like everything else that can destroy work: refused while a clone is running (409) unless forced, and from M3 on the same guarded-teardown rule that protects worktrees extends to repo deletion across all its worktrees, with an explicit force as the only override. The teardown tail runs on a detached context — a client disconnect must not brick a repo halfway through deletion.

## Status

Accepted. Implements D8; shipped in M2 (worktree guard seam lands with M3).

## Considered options

- **Scan a projects directory (v0's model).** Rejected by D8: explicit add is production behavior; scanning made v0 treat any stray `.git` marker as a project and forced worktrees out of the scanned root.
- **Non-bare reference clone.** Rejected: a working tree can get dirty and needs its own guarded lifecycle; bare cannot, and worktrees hang off it natively.
- **Synchronous clone in the add request.** Rejected: large repos would hold an HTTP request open for minutes; the async job + SSE progress model is also what the embedded-UI roadmap needs.
