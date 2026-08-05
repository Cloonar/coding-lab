# Issue tracker: GitHub Issues

Issues and PRDs for this repo live as GitHub issues on https://github.com/Cloonar/coding-lab. The agent surface for all tracker work is `labctl` — it reaches the tracker over the lab API using `LAB_URL` and `LAB_TOKEN` from the session environment, so there is no login step and no `--login`/`--repo` flags: it targets the run's repo from the run token and works from anywhere in the worktree.

## Conventions

- **Create an issue**: `labctl issue create --title "..." --body "..."`. Add `--labels "a,b"` (comma-separated) to label at creation — every label must already exist (see below). Multi-line bodies go through ordinary shell quoting or a heredoc into the `--body` argument: `--body "$(cat <<'EOF' ... EOF)"`.
- **Read an issue**: `labctl issue view <n>` — always prints the issue *with its comments* (there is no `--comments` flag to remember). `labctl issue view` with no number shows the run's own claimed issue.
- **List issues**: `labctl issue list` — open issues, one per line (number, state, created, labels, title).
- **Comment on an issue**: `labctl issue comment <n> "..."` (multi-line via the same quoting/heredoc as `--body`).
- **Edit title/body**: `labctl issue edit <n> [--title "..."] [--body "..."]`. An omitted flag leaves that field untouched; `--body ""` clears the body, but `--title ""` is rejected — the title must stay non-empty.
- **Apply / remove labels**: `labctl issue label add <n> "a,b"` / `labctl issue label remove <n> "a,b"` (comma-separated, two separate verbs).
- **Labels must exist before you apply them.** GitHub rejects an unknown label. Create it first — `labctl label create --name "..." [--color "#..." --description "..."]` is idempotent (create-if-missing, safe to run unconditionally). `labctl label list` shows the repo's labels.
- **Close**: `labctl issue close <n>`. There is no closing-comment flag, so post the explanation first with `labctl issue comment <n> "..."`, then close.
- **Reopen**: not on the agent surface — `labctl` has no reopen verb. If an issue needs reopening, ask the operator to do it from the forge.
- **Pull requests**: managed with `labctl pr ...` (`create`, `view`, `list`, `checks`, `merge`, plus the verdict verbs `reject` / `approve` / `rerequest` / `escalate` and `comment`). The `land-pr` skill drives the GitHub pull request flow.

## When a skill says "publish to the issue tracker"

Create a GitHub issue with `labctl issue create --title "..." --body "..."`.

## When a skill says "fetch the relevant ticket"

Run `labctl issue view <n>` (it includes the comments).

## Archive

Issues numbered before the migration live on the old Forgejo tracker at https://git.cloonar.com/Cloonar/coding-lab and were not migrated. Issue numbers referenced in ADRs, code comments, and docs (e.g. "#220", "ADR-0054 / issue #205") point there — GitHub issue numbering restarts independently, so a given number on GitHub is not the same issue as that number on Forgejo.
