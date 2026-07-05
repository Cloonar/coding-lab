# lab v0 — read-only reference snapshot

This directory is a **frozen snapshot** of the original `lab` prototype, vendored here
as the behavioral reference for the production rewrite specified in
[`docs/agent-brief.md`](../../agent-brief.md). It is **not part of the build**. Do not
edit it, import it, or compile it — read it.

- **Source**: `cloonar-nixos` repo, `utils/modules/lab/` at the state of 2026-07-05
  (plus `hosts/fw/vms/web/lab.nix` as `deployment-example-lab.nix`).
- **What it is**: a single-binary Go 1.22 web service (stdlib-only, ~4.6k lines production
  code, ~5.8k lines tests) that manages `claude --remote-control` tmux sessions in
  per-instance git worktrees and drives AFK runs off Forgejo `ready-for-agent` issues.
- **Why it's here**: it encodes hard-won lifecycle decisions (documented in `adr/`):
  claim-is-the-branch, the guarded teardown rule, AFK run classification, restart
  re-adoption, trust seeding. The rewrite ports that decision logic; it does not
  re-derive it.

## Map

| File | What to port from it |
| --- | --- |
| `afk.go` | AFK lifecycle: selection, claim, launch, reaper, scheduler, 3-strikes pause, budget classification (`classifyAFKRun`, `shouldLaunchAuto`, `pickLowestIssue`) |
| `git.go` | Worktree add/rollback, guarded teardown (`decideTeardown`), branch/merge queries |
| `reconcile.go` | Startup orphan reconciliation + throttled merged-sweep |
| `parked.go` | Parked view gathering + unguarded Discard |
| `sessions.go` | tmux wrapper (start/stop/liveness/send-keys/pane scrape), nofile cap |
| `spawn.go` | Spawn argv construction, model/effort allowlists, seed prompt |
| `registry.go` | claude.ai deep-link capture from `~/.claude/sessions/*.json` |
| `auth.go` | Claude OAuth login flow (pane scrape, code paste, status polling, 30s cache) |
| `trust.go` | Trust/settings seeding into `~/.claude.json` + worktree `settings.local.json` |
| `tracker.go` | The tea-based tracker (replaced by REST clients — read for semantics only) |
| `store.go` | JSON state file (replaced by SQL — read for what state exists) |
| `handlers.go`, `templates/` | UI flows, fragment/poll model, design language |
| `adr/` | The five ADRs that explain *why* the model is shaped this way |

Every `*_test.go` file doubles as a behavioral specification — the table tests on the
pure decision functions are the most precise statement of intended behavior.
