# Model selection for the agent workflow

Recommended claude-code model/effort settings per stage of the grill → PRD → issues → triage → AFK → land workflow, in the provider catalog's own values (as of the Claude 5 family, 2026-08):

| Stage | Lab surface | Model | Effort | Why |
|---|---|---|---|---|
| Grilling, `/to-prd`, `/to-issues` | manual instance | `fable` | `high` | Highest-leverage thinking stage: design sessions are low token volume, a human is present, and question/slicing quality compounds through every later stage. Fable's edge — scoping ambiguous problems — is exactly what grilling is. |
| Triage, interactive `/land-pr` | manual instance | `opus[1m]` | `high` | Brief-writing and bug reproduction want strong reasoning, but not frontier pricing. |
| AFK runs | repo AFK model/effort defaults | `opus[1m]` | `xhigh` | Opus is the agentic-coding workhorse and `xhigh` its documented setting for unattended coding; nobody waits on latency. A good agent brief removes exactly the ambiguity Fable would otherwise be for. |
| Autoland lander + escalate | repo `lander_model` / `lander_effort` | `opus[1m]` | `high` | The unattended gate in front of the default branch. Review accuracy holds at `high`; don't go below it on an auto-merging path. |

- Keep `fable` off unattended surfaces (AFK, lander): ~2× Opus pricing, and its safety classifiers can occasionally refuse benign security-adjacent work — interactively that's a nudge, in a pipeline it's a stalled run.
- `sonnet` at `xhigh` is a credible budget tier for small, well-briefed AFK slices (dependency bumps, mechanical changes) if per-issue model routing ever exists; while model/effort stay per-repo, the practical AFK default remains `opus[1m]`.
- `haiku` has no role in the workflow (lab uses it internally only for the credential-refresh poke).
- Roadmap idea: a Fable escalation tier — rerun an issue on `fable` after Opus has failed it twice, before routing it to `ready-for-human`.
