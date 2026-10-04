# Model selection for the agent workflow

Recommended claude-code model/effort settings per stage of the grill → issues → triage → AFK → land workflow, in the provider catalog's own values. As of 2026-10 the catalog's aliases resolve to Fable 5.1 (`fable`), Opus 5.5 (`opus[1m]`) and Sonnet 5.5 (`sonnet`):

| Stage | Lab surface | Model | Effort | Why |
|---|---|---|---|---|
| Grilling, `/to-prd`, `/to-issues` | manual instance | `fable` | `high` | Highest-leverage thinking stage: design sessions are low token volume, a human is present, and question quality compounds through every later stage. `/to-issues` writes each issue body as the agent brief the AFK run implements, so this session produces the contract itself. |
| Triage that grills or writes a brief | manual instance | `fable` | `high` | It ends in the same artifact as a grilling session — the agent brief — so it gets the same model. |
| Inbox passes, interactive `/land-pr` | manual instance | `opus[1m]` | `high` | Sorting, reproducing a bug and reviewing a diff want strong reasoning, but not frontier pricing. |
| Unattended triage (a Schedule) | the Schedule's model/effort override | `opus[1m]` | `high` | Exploration, reproduction and drafting with nobody waiting. The maintainer reads every brief before promoting it, so a human still gates what reaches an agent. |
| AFK runs | repo AFK model/effort defaults | `opus[1m]` | `high` | Opus is the agentic-coding workhorse and a good agent brief removes the ambiguity Fable would otherwise be for. `high` rather than `xhigh`: see the effort note below. |
| Autoland lander + escalate | repo `lander_model` / `lander_effort` | `opus[1m]` | `high` | The unattended gate in front of the default branch. Don't go below `high` on an auto-merging path. |

- **Effort names do not carry across model generations.** Opus 5.5 thinks more at a given level than Opus 5 did, most of all at `xhigh` and `max`, and Anthropic's guidance for it is to start at `medium` and reserve `xhigh` and `max` for work where a gain was measured. `high` is the cautious step down from the `xhigh` this page recommended for Opus 5. Compare the lander's rejection rate and AFK timeouts before and after; go lower if nothing moves, back up if rejections rise. Re-check this table whenever an alias moves to a new model — `internal/compat/compat.md` records when that happens.
- Keep `fable` off unattended surfaces (AFK, lander, Schedules): its list price is 2.5× Opus 5.5 ($10 / $50 against $4 / $20 per million tokens, 2026-09), and its safety classifiers can occasionally refuse benign security-adjacent work — interactively that's a nudge, in a pipeline it's a stalled run.
- `sonnet` at `medium` or `high` is a credible budget tier for small, mechanical AFK issues (dependency bumps) if per-issue model routing ever exists; while model/effort stay per-repo, the practical AFK default remains `opus[1m]`.
- `haiku` has no role in the workflow (lab uses it internally only for the credential-refresh poke).
- Roadmap idea: a Fable escalation tier — rerun an issue on `fable` after Opus has failed it twice, before routing it to `ready-for-human`.
