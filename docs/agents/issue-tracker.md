# Issue tracker: Forgejo

Issues and PRDs for this repo live as Forgejo issues on https://git.cloonar.com/Cloonar/coding-lab. Use the `tea` CLI (the Gitea/Forgejo CLI, logged in against `git.cloonar.com`) for all operations.

## Conventions

- **Create an issue**: `tea issues create --title "..." --description "..."`. Add `--labels "a,b"` (comma-separated) to label at creation. Use `--description "$(cat <<'EOF' ... EOF)"` for multi-line bodies.
- **Read an issue**: `tea issues <index> --comments`. Always pass `--comments` explicitly — omitting it makes tea prompt interactively. Add `--output json` for machine-readable output.
- **List issues**: `tea issues list --state open --output json --fields index,title,body,labels`. Filter with `--labels "..."` and `--state all|open|closed`.
- **Comment on an issue**: `tea comment <index> "..."`
- **Apply / remove labels**: `tea issues edit <index> --add-labels "..."` / `tea issues edit <index> --remove-labels "..."`. Use two separate calls — when both flags are passed at once, `--add-labels` takes precedence and `--remove-labels` is ignored.
- **Close**: `tea issues close <index>`. There is no closing-comment flag, so post the explanation first with `tea comment <index> "..."`, then close. Reopen with `tea issues reopen <index>`.
- **Pull requests**: managed with `tea pulls ...` (list, detail, create, merge — see `tea pulls -h`). The `land-pr` skill drives the Forgejo PR flow.

`tea` infers login and repo from `git remote -v` when run inside this clone — no `--login`/`--repo` flags needed. Outside the clone, pass `--login git.cloonar.com --repo Cloonar/coding-lab`.

## When a skill says "publish to the issue tracker"

Create a Forgejo issue with `tea issues create`.

## When a skill says "fetch the relevant ticket"

Run `tea issues <index> --comments`.
