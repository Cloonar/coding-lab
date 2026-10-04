# Global runner and dev image defaults: a repo's Runner can inherit a live `runner_default` setting, a pinned `dev_image_default` setting sits between the repo's Dev image and `--container-image`, and a Runner section in global Settings edits both alongside the container limits

ADR-0052 made the **Runner** a per-repo pick with no inherit state: `repos.runner` is NOT NULL, the migration gave existing repos `host`, and repo creation stamps `host`. There is no global knob. An operator who wants every repo in containers edits every repo, and every repo added afterwards starts on the unsandboxed host runner until someone edits it too.

The dev image does have a global default, but only as a deployment setting. ADR-0053 made `--container-image` the optional default under a repo's own `image_ref`, and ADR-0056 had the NixOS module ship a digest-pinned value for it as `container.defaultImage`. Changing it means a deploy. Selecting ADR-0069's full dev image host-wide meant a module change with a digest pinned by hand.

The global container limits (`container_memory`, `container_pids`, `container_nofile`, ADR-0052) are settings rows with API validation, and the repo page already shows them as "Inherit global default — currently …", but no screen edits them. Global Settings has three sections: General, Agents, Notifications.

This ADR gives the operator global defaults for the Runner and the dev image that can be changed at runtime. They are edited in a new Runner section of global Settings, and a repo can now inherit its Runner.

The pins, decided (with the maintainer, 2026-10-04):

- **A repo's Runner gains a third state: inherit.** `repos.runner` becomes nullable in both dialects. NULL means "use the global runner default". The effective Runner is the repo's value when one is set, else the `runner_default` setting. The repo JSON `runner` is `"host" | "container" | null`. The repo PATCH accepts `null` to inherit and rejects any other value outside the enum. Repo creation stamps NULL instead of `host`.

- **Existing repos keep their pin.** The migration changes no row: every existing repo keeps its explicit `host` or `container`. Only repos created after it start as inherit. The down-migration restores the NOT NULL column and maps inheriting rows to `host`.

- **`runner_default` is a settings row, seeded `host`, with no flag and no module option.** It holds `host` or `container`, and the settings PATCH rejects anything else. It is seeded like the other settings that must always hold a concrete value (such as the container limits), so a fresh database and an upgraded one both end up with `host` without operator action, and seeding never overwrites an existing row. An upgrade therefore changes no repo's effective Runner by itself. There is no server flag and no NixOS option for it: it is runtime state, edited in Settings.

- **Inheritance is live.** Changing `runner_default` changes the effective Runner of every inheriting repo, starting with its next spawn. Runs already alive are not touched. A repo pinned to `host` or `container` is unaffected.

- **One default for every run kind.** Manual, AFK, scheduled, lander and escalate runs all resolve the same effective Runner. There are no per-kind layers.

- **One effective-Runner resolver, used everywhere.** Every place that branches on a repo's Runner asks one shared resolver in the instance layer instead of reading the repo row: the container gate at spawn, the advisory write protection of **read-only import** snapshots at spawn (ADR-0063's best-effort `chmod a-w` under the `host` runner), and the same protection in the import refresh behind `/pull-base`.

- **An unresolvable default refuses the spawn. It never falls back to `host`.** If `runner_default` cannot be read, or holds a value that is neither `host` nor `container`, a spawn of an inheriting repo is refused before the run claims anything, and the message names the setting. No host pane is started, and for AFK work the issue stays unclaimed. Falling back to `host` would turn a lost setting into an unsandboxed run. The import refresh is the one place that does not refuse: there an unresolvable Runner skips the advisory protection and logs, rather than failing the refresh.

- **A global default dev image sits between the repo's Dev image and the flag.** A new `dev_image_default` setting. A container run's dev image resolves as: the repo's `image_ref`, else `dev_image_default`, else `--container-image`. With none of the three set, ADR-0053's no-dev-image spawn refusal applies, and its message now names all three knobs. Like the Runner, there is one resolver for the dev image in the instance layer, and every consumer uses it instead of reading the flag directly.

- **`dev_image_default` is unseeded, and blank falls through.** Blank or absent means "use the flag", so clearing the field returns to the deployed default.

- **Saving it pins it, exactly as a repo's Dev image is pinned (ADR-0053).** A non-blank value must be fully qualified. It is resolved tag→digest anonymously over HTTPS and stored pinned as `host/path:tag@sha256:…`, and an already pinned ref is stored as it is. A ref that cannot be resolved fails the save with a 400 carrying the registry's error. If the pinner is unavailable, the save fails the same way the repo field's does. The setting reuses the pinner a repo's `image_ref` already uses; there is no second implementation.

- **An unreadable `dev_image_default` refuses the spawn.** If the setting cannot be read at spawn, the spawn is refused before the claim. It does not silently drop to the flag image. Pull-if-missing before the claim (ADR-0053) is unchanged and applies to whichever image the chain resolved.

- **Provider login keeps the flag image.** The login pane and the non-interactive provider CLI containers (auth status, logout, the refresh poke, the catalog probe) do not follow `dev_image_default`. They run the `--container-image` image exactly as ADR-0057 pins it, and keep its refusal when the flag is unset.

- **The settings response carries the fallback, read-only.** `GET /api/v1/settings` gains `dev_image_fallback`: the `--container-image` value, empty when the flag is unset. The settings PATCH cannot write it. It exists so the UI can say what a blank field falls through to.

- **Global Settings gains a fourth section, Runner, with three cards: Runner, Dev image, Container limits.** The order and the cards mirror the Runner section of repo settings.
  - **Runner card.** A picker with the same two options and labels the repo page uses, and the "unsandboxed, full host access" hint while `host` is selected. A line states how many repos currently inherit the default. Saving a change *to* `host` asks for one confirmation that names that count; cancelling saves nothing. Saving `container` is never confirmed, and never refused because of the container preflight state: the spawn refusal stays the one place that reports an unready host.
  - **Dev image card.** One text field, pinned on save, with errors shown in the section banner. While the field is blank, its hint names the fallback ("Blank uses the deployed image `<dev_image_fallback>`"), or says that no image is configured when the fallback is empty too. The fallback never appears as a field or row of its own.
  - **Container limits card.** Edits the three existing global limit settings, with their existing validation.

- **Repo settings show what inheriting means.** The repo's Runner picker gains a first option, "Inherit global default — currently Container" (or Host), worded like the container-limit hints. The host hint shows whenever the effective Runner is `host`, whether pinned or inherited. The repo's Dev image hint names the image a blank field would inherit (the setting, else the fallback), or says none is configured.

- **No new glossary term.** The prose says "global runner default" and "global default dev image". That second phrase changes what it refers to. ADR-0057 and the earlier docs used "global default dev image" for the `--container-image` flag. From this ADR on, it names the `dev_image_default` setting, and the flag's image is the deployed fallback, the last layer.

## Status

Accepted. Settled via issue #55 (2026-10-04).

Amends ADR-0052: the Runner had no inherit state. `repos.runner` is now nullable, NULL inherits `runner_default`, and new repos are created inheriting. The default is still `host` after an upgrade, through the seed, and existing repos keep their values. The runner itself (mount inventory, argv, preflight, refusal before the claim) is untouched.

Amends ADR-0053 and ADR-0056: a settings layer now sits between the repo's `image_ref` and `--container-image`, and the no-dev-image refusal names three knobs instead of two. ADR-0053's pin-on-save rules and its demotion of the flag to an optional default stand; the flag is now the last of three layers. ADR-0056's module default (`container.defaultImage`, the pinned `buildpack-deps:stable-scm`) and its explicit `null` opt-out are unchanged.

ADR-0057 is unchanged: provider login and the provider CLI containers run the `--container-image` image and nothing else.

ADR-0069's rule that the full dev image is opt-in and never a shipped default is unchanged. The operator may now select it host-wide at runtime through the global default dev image; the module's shipped default does not change.

Uses ADR-0063's advisory write protection unchanged, now routed through the effective-Runner resolver. Scheduled runs (ADR-0062) and the other run kinds get the effective Runner through the one launch path, with no per-kind code.

## Considered options

- **Per-run-kind layers** (an AFK-only or lander-only Runner or dev image). Rejected: one default serves every run kind.
- **A server flag or NixOS option for the runner default.** Rejected: the point is a default the operator can change at runtime, and a flag or module option needs a deploy to change.
- **Seeding `runner_default` as `container`.** Rejected: an upgrade must not change any repo's effective Runner by itself. The operator flips the default in Settings.
- **Converting existing `host` repos to inherit**, or changing any existing repo's Runner or image. Rejected: the migration changes no row, and an existing value stays a pin.
- **Falling back to `host` when `runner_default` is unreadable or invalid.** Rejected: it would run a repo unsandboxed because a setting was lost. The spawn is refused before the claim instead.
- **Falling back to the flag image when `dev_image_default` is unreadable.** Rejected for the same reason: the spawn would silently run a different image from the one the operator configured.
- **Refusing or confirming `runner_default = container` based on preflight state.** Rejected: the spawn refusal already reports an unready host, with the actionable preflight text, and is the one place that does.
- **Making provider login and the provider CLI containers follow `dev_image_default`.** Rejected: ADR-0057 pins them to the flag image.
- **Changing the module's shipped default dev image.** Out of scope: ADR-0056's default and ADR-0069's opt-in rule stand.
- **Private or authenticated registries, pre-pulling on save, automatic re-pinning of a moved tag.** Out of scope, as they were for a repo's Dev image in ADR-0053. Updating the global default dev image is an explicit re-save.

Deferred: recording a run's Runner on the run itself. It is tracked as a separate follow-up (see the caveat below).

## Consequences

- **An upgrade changes nothing by itself.** Every existing repo keeps its pinned Runner and its Dev image, `runner_default` is seeded `host`, and `dev_image_default` starts unset, so container runs keep resolving to the repo's ref or the flag as before.
- **New repos follow the global runner default.** Out of the box that is `host`, as before. After the operator switches the default to `container`, every newly added repo starts in containers with no per-repo edit.
- **Switching every inheriting repo is one setting.** It takes effect at each inheriting repo's next spawn; live runs keep the Runner they started with.
- **A host-wide dev image no longer needs a deploy.** The operator saves it in Settings → Runner → Dev image, where it is pinned exactly like a repo's ref; moving to a newer digest is a re-save. The full dev image can be selected host-wide this way.
- **Login can run a different image from the sessions.** With `dev_image_default` set, container sessions of repos with a blank Dev image run that image, while login and the provider CLI containers still run the flag image. A deployment that sets `container.defaultImage = null` and relies on the setting alone has no image for login, which ADR-0057 refuses, naming `--container-image`.
- **Two more settings can refuse a spawn.** An unreadable or invalid `runner_default` refuses spawns of inheriting repos, and a `dev_image_default` that cannot be read at spawn refuses the spawn. Both refusals land before the claim.
- **Accepted caveat: the run does not record its Runner.** The `/pull-base` import refresh resolves the Runner when it runs, not when the run started. A Runner change during a live run, of the repo or of the global default it inherits, can therefore make the refresh apply or skip the advisory snapshot write protection for that run. The protection is advisory only (ADR-0063: the `host` runner is full-host-access break-glass), the same was already true of a per-repo flip before this change, and it is accepted until the follow-up records the Runner on the run.
- **Where it lands.** Migrations in both dialects make `repos.runner` nullable; `runner_default` is seeded with the other always-concrete settings; the settings keys `runner_default` and `dev_image_default`, and `dev_image_fallback` in the settings response; the effective-Runner and effective-dev-image resolvers in the instance layer, used by spawn and by the import refresh; repo creation stamping NULL; the web UI's Runner section in global Settings and the inherit option and hints in repo settings.
- **Docs.** `CONTEXT.md`'s **Runner**, **Dev image** and **Full dev image** entries are amended (no new term). `docs/ops.md` covers the settings, the flag and module option rows, and the Container runner section. `docs/getting-started.md`'s Runner step says new repos inherit the global runner default.
