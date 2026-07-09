# Per-provider login sessions: `lab-login-<providerID>`, matched by prefix predicate

`tmuxx.LoginSession` was a single fixed tmux session name, `"lab-login"`, for the provider login flow. Every exclusion site that must never mistake a login session for a normal instance session — cap counting, branch ownership, stop-all (design §4d), reconcile naming and parked — keyed on equality against that one symbol. That forced every provider's login flow to share one tmux session name, a constraint invisible until it was tripped. With a second provider arriving (#2, Codex), a concurrent login would collide with claude-code's in the same session, corrupting both.

The pins, decided:

- **Login sessions are named per provider**: `lab-login-<providerID>` (e.g. `lab-login-claude-code`), derived via `tmuxx.LoginSessionName(providerID)`.
- **All exclusion sites go through one predicate**, `tmuxx.IsLoginSession(name)` — a prefix match keyed on the single const `tmuxx.LoginSessionPrefix = "lab-login"`, so derivation and predicate cannot drift against each other. No equality comparison against a login-session constant remains anywhere in the tree.
- **The bare legacy name `lab-login` still satisfies the predicate.** A login session left over from a pre-#77 binary is not orphaned from the exclusions — it is still recognized, still excluded from cap counting, branch ownership, and stop-all, until the host that wrote it is gone.
- **claude-code derives its session name from its own provider id**; no other behavior of its login flow changes.
- **Timing**: safer to make this change while exactly one login flow exists than after a second is live and the two would need migrating in lockstep.

## Status

Accepted. Extends the provider seam (ADR-0026): a second provider (#2, Codex) gets login-session isolation for free by deriving its session name from its provider id, with no change to any exclusion site. `internal/tmuxx` gains `LoginSessionPrefix`, `LoginSessionName`, and `IsLoginSession`; `LoginSession` is removed, not aliased — every call site (`internal/instance`, `internal/reconcile`, `internal/afk/reaper.go`, `internal/provider/claudecode`) moves to the predicate or the derived name.

## Considered options

- **Keep one shared login session and serialize concurrent logins behind a lock.** Rejected: a lock only hides the collision, it doesn't remove it — two providers' login flows would still fight over one tmux session's pane, one operator's OAuth paste landing in the wrong provider's `claude`/`codex` process. Per-provider naming removes the shared resource instead of arbitrating access to it.
- **Equality-match a small, explicit set of known login-session names at each exclusion site.** Rejected: this is exactly the shape that broke — a new provider means finding and editing every exclusion site again, and a missed site silently readmits a login session into cap counting or stop-all. A single prefix predicate, derived from the same const the name generator uses, means a new provider needs zero exclusion-site changes.
- **Drop the bare-`lab-login` legacy allowance and require a clean cutover.** Rejected: an operator's existing host can have a `lab-login` session outstanding across the binary upgrade that ships this change; failing to recognize it would let a stale login session leak into a normal instance's exclusions until it's manually killed. The allowance costs one `||` in `IsLoginSession` and can be dropped later once no pre-#77 host remains — worth revisiting as cleanup, not urgent.

## Consequences

- New providers get login-session isolation for free by deriving `LoginSessionName` from their provider id; no exclusion site needs touching again for a new provider.
- The legacy bare-`lab-login` allowance in `IsLoginSession` is a deliberate, temporary broadening — safe to drop once no pre-#77 host remains, but not scheduled; a future cleanup ADR or issue can retire it once that's established.
