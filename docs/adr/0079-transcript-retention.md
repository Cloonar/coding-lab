# Ended runs keep their provider-native transcript under `<state>/transcripts` for a configurable number of days

ADR-0016 made the chat read-through: the only chat state lab persists is `runs.transcript_path`, and an ended run stays readable "as long as the provider retains the file", with a retired file degrading to "transcript no longer available". That bet held while the transcript lived in the operator's own `~/.claude`. Since issue #202 every run has a private HOME under `<state>/instances/<runID>/home`, and the provider writes its session file inside it. Every teardown path removes that whole per-run tree: `instance.Stop`, `afk.Stop`, the AFK reaper, the reconcile discard of a parked run, launch rollback, and the orphan sweep after downtime. So the chat of every ended run showed "Transcript no longer available", by construction, for every provider and run kind (issue #81).

The read side for ended runs already works and is unchanged: `chat.Service.Read` reads an ended run from its transcript alone, forces `StateEnded`, masks secret values, and the chat renders it read-only; the Runs page's Ended view links each row to its chat. Only the file was missing.

The decisions, pinned:

- **Purpose: re-read the conversation in the chat UI.** Retention exists so an ended run's chat renders as it did while the run was live. There is no pane capture for a run that died, and no cross-run index or search; those would be separate issues.

- **The provider-native file is moved, not mirrored.** No message table and no lab-owned snapshot format: ADR-0016's rejection of both stands. The retained copy is the provider's own file, byte for byte, read later through the same `ReadChat` fold. Only the run's current transcript path is retained; the segments a `/clear` left behind earlier in the run are not.

- **The provider owns the file work, through a new required method.** `provider.AgentProvider` gains, next to `LocateTranscript`:

  ```go
  RetainTranscript(ctx context.Context, worktree, home, transcriptPath, destDir string) (retainedPath string, err error)
  ```

  It moves what the adapter needs to render the conversation after the HOME is wiped into `destDir` (created by core, empty) and returns the path `ReadChat` should read from; `""` means nothing to keep. It never reads live signals and never touches `destDir`'s siblings. The move is a rename when source and destination share a filesystem, and a copy (new 0600 file, fsynced, then the source removed) when the rename fails across devices. `provider.RetainFile` is the shared implementation: it refuses a source outside the run's HOME (lexically and after resolving symlinks) or one that is not a regular file, never overwrites, and treats an empty path or a missing file as nothing to keep. Core never learns either provider's layout. Claude Code moves `<home>/.claude/projects/<slug>/<sessionId>.jsonl` alone: the `<sessionId>/` sidecar directory beside it (subagent transcripts, tool results) is never read by `ReadChat`, which renders a subagent from the parent's tool call and result, so bringing it along would only lengthen the life of more secret-bearing bytes. Codex moves its rollout file out of `<home>/.codex/sessions/YYYY/MM/DD/` and stores it flat, since only `LocateTranscript` walks the date tree and `ReadChat` folds whatever path it is given. The conformance suite checks it (`retain-transcript`): locate, retain into a temporary directory, remove the HOME, then `ReadChat` with an ended spec against the retained path yields the same messages.

- **One global setting: `transcript_retention_days`.** Seeded to 30 on the insert-if-absent footing of `runner_default`, so a fresh install retains without operator action and an operator's value survives re-seeding. Edited in **Settings → General → Transcripts**. `0` retains nothing, which is the off switch; 365 is the cap, enforced by the settings PATCH (a value outside 0..365, a non-integer or `null` is a 400 that writes nothing) and by the web field. There is no per-repo override, no CLI flag, no NixOS option and no "forever". A hand-edited row outside the range reads as the default with a warning, so neither retaining nor expiry stalls on it.

- **Retain runs on the pre-wipe seam, which becomes a chain.** `instancehome`'s pre-wipe hook (issue #222, ADR-0055) already fires on the only two code paths that destroy a per-run tree, `Wipe` and `SweepAll`, so a step installed there covers all six wipe sites without each caller remembering, including the orphan sweep at startup after downtime. The seam becomes a hook chain, and the retain step runs after the #222 credential adopt-check. The step, installed by the `instance` package: load the run row (no row is a no-op, which is what a launch rollback and a true orphan look like); skip when the setting is 0 or the run has no transcript path; create `<state>/transcripts/<runID>/` with mode 0700; call `RetainTranscript`; persist the returned path through the existing transcript-path setter (`""` stores NULL). Any failure is logged at warn and the wipe proceeds: a retain must never keep a run's tree, and its credentials, alive.

- **Expiry is a reconcile step keyed on `runs.ended_at`.** It runs in the throttled runtime sweep, beside `homes.SweepAll`, and once at startup. It lists ended runs that still carry a transcript path and whose `ended_at` is older than now minus the window, removes each run's `<state>/transcripts/<runID>/`, and clears its `transcript_path` to NULL. A second guard removes any run-shaped directory under `<state>/transcripts` whose run row no longer exists. Expiry never looks at file mtimes. Shortening the window applies retroactively at the next sweep, and setting it to 0 removes every retained copy; that is intended. An expired run shows the existing "Transcript no longer available" state; nothing new is drawn.

- **The secrets stance is unchanged (issue #108).** A secret value an agent printed is in the transcript, and so in the retained copy. Lab never rewrites the file; masking stays render-time, on every surface lab draws; rotating the secret is the remedy. What changes is that the retention window now bounds how long the dirty copy lives on disk after the run ends.

- **`<state>/transcripts/` is disposable.** It is listed in the state directory layout and explicitly excluded from the backup set: losing it loses only old chat history, and the runs themselves stay in the database. There is no size cap in this slice.

- **No UI beyond the settings field.** No "transcript present" or "expired" marker on Ended rows, and no per-run delete.

## Status

Accepted 2026-10-09. Resolves issue #81. Amends [ADR-0016](0016-embedded-chat.md): ended runs no longer depend on the provider keeping its file; lab keeps the provider-native file for the retention window, and `runs.transcript_path` now points at the retained copy once a run is wiped. ADR-0016's rejection of a message table and of a lab-owned format stands. Extends the pre-wipe seam of [ADR-0055](0055-server-owned-credential-authority.md) from one hook to a chain, with the adopt-check first. Adds one required method to the `AgentProvider` seam.

## Considered options

- **Capture the tmux pane at teardown.** Rejected: a pane holds only the visible screen, not the conversation, and rendering it would need a second reader beside `ReadChat`. It would also cover a case retention does not try to (a run that died with nothing useful in its transcript), which is a separate want.
- **A message table, or a lab-owned snapshot format written at teardown.** Rejected for the reasons ADR-0016 gave: the provider's file is already the complete record, and a second format doubles storage, invites drift and needs its own renderer. Moving the file keeps one reader for live and ended runs.
- **Mirror the transcript continuously while the run is live.** Rejected: it would copy secret-bearing bytes on every write, and the pre-wipe seam already sees every teardown.
- **A per-repo retention override, or a CLI flag.** Rejected: one window answers the only question asked (how long can I re-read old chats), and a second layer would need its own inheritance UI. The global row is the only source.
- **Expire by file mtime.** Rejected: a rename keeps the source's mtime and a copy resets it, so the same run would expire at different times depending on the filesystem layout. `ended_at` is the run's own fact, and it makes a shortened window apply to every older run at once.
- **Let core move the files itself.** Rejected: core would have to learn each provider's layout (Claude Code's project slug and session id, Codex's date tree) and which sibling files `ReadChat` needs. The adapter already owns `LocateTranscript` and `ReadChat`, so it owns the move.
- **Keep transcripts forever, or put them in the backup set.** Rejected: the copies can carry secret values (issue #108), so their life must be bounded, and old chat history is not worth restoring.

## Consequences

- An ended run's chat shows its whole final conversation, read-only, for `transcript_retention_days` after it ended, for manual, AFK, scheduled, lander and escalate runs alike. After the window it reads "Transcript no longer available", as every ended run did before.
- `<state>/transcripts/<runID>/` (0700) appears beside `<state>/instances/`. It holds one file per retained run. Disk use grows with the number of runs that end within the window; the window is the only bound.
- A run's `transcript_path` moves once: from the file inside its HOME to the retained copy at wipe, and to NULL at expiry. When the provider keeps nothing, the row is left as it was and the chat reads "Transcript no longer available" once the HOME is gone.
- Every `AgentProvider` must implement `RetainTranscript`; `providertest.Fake` scripts it and the conformance suite checks it. A provider with nothing to keep returns `""` and no error.
- A retain failure costs only that run's history. It is logged at warn and never blocks the wipe, the credential adopt-check before it, or the teardown that called it.
- Turning retention off (`0`) stops new copies at once and removes the existing ones at the next sweep.
