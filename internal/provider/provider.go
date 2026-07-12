// Package provider defines the AgentProvider seam (design §4d) between
// lab's instance/AFK services and a concrete coding agent, plus the
// registry that resolves a repo's `provider` column to an implementation.
//
// M3 ships exactly one provider, claude-code (internal/provider/claudecode);
// the repos.provider column already defaults to it. Model/effort catalogs
// are provider-owned (brief D14/§11.5) — nothing outside a provider may
// hardcode model or effort values.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Option is one entry of a provider-owned catalog (model or effort
// dropdown): Value is what the provider's CLI accepts, Label the human text
// (pinned API shape: {value,label}).
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// OptionSpec declares one provider-owned spawn option (issue #19 / ADR-0021):
// the generic, extensible seam beside the typed model/effort. The provider
// DECLARES its options (this schema); lab STORES/VALIDATES/RENDERS the bag
// generically from it; the provider APPLIES the resolved values in SpawnArgv.
// Type is "bool" for the MVP (enum/string reserved). Default is the value used
// when the operator has set nothing (e.g. "false"). Pinned API shape.
type OptionSpec struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Type    string `json:"type"`    // "bool" (enum/string reserved)
	Default string `json:"default"` // e.g. "false"
}

// OptionTypeBool is the only spawn-option type the MVP supports; its bag value
// is the string "true" or "false" (enum/string types are reserved).
const OptionTypeBool = "bool"

// SpawnSpec is the full input to AgentProvider.SpawnArgv (issue #19 / ADR-0021).
// Passing a struct — not positional args — means a new provider spawn option
// (Options) never churns the interface signature. model/effort stay typed and
// first-class; Options is the generic bag the provider applies itself.
type SpawnSpec struct {
	SessionName string
	Model       string // "" omits the --model flag
	Effort      string // "" omits the --effort flag
	// Options is the resolved provider-owned spawn-options bag (issue #19),
	// already filtered + validated to this provider's declared schema by the
	// caller. The provider applies it however it sees fit (claude-code prepends
	// an ultracode directive to a non-empty InitialPrompt). Empty/nil → no-op.
	Options map[string]string
	// InitialPrompt is the AFK seed prompt, carried as the agent's trailing
	// positional argument (the pinned v0 mechanism, present before the process
	// so it never races the cold-start TUI). Manual spawns pass "" and get no
	// trailing argument — which also makes every prompt-scoped option (ultracode)
	// a natural no-op for manual runs.
	InitialPrompt string
}

// AuthStatus is the machine-level login state of a provider (pinned API:
// GET /api/v1/providers/{id}/auth/status). CheckedAt is when the
// underlying status command actually ran — a cached read keeps the older
// stamp. Serialize timestamps with store.FormatTime at the API layer.
type AuthStatus struct {
	LoggedIn  bool      `json:"logged_in"`
	Email     string    `json:"email"`
	Method    string    `json:"method"`
	CheckedAt time.Time `json:"checked_at"`
}

// Auth flow kinds (AuthFlow.Kind) — how an operator logs a provider in
// (issue #51 decision 7). The descriptor drives the SPA's auth card, so no
// frontend code hardcodes a provider's login mechanics.
const (
	// AuthFlowOAuthCode: LoginStart returns an authorize URL and the operator
	// pastes the resulting code back through LoginSubmitCode (claude today).
	AuthFlowOAuthCode = "oauth-code"
	// AuthFlowOAuthRedirect: a localhost browser redirect completes the flow;
	// Instructions carries the operator guidance (e.g. an SSH port-forward
	// recipe for a headless host).
	AuthFlowOAuthRedirect = "oauth-redirect"
	// AuthFlowDeviceCode: device-code authorization — LoginStart launches the
	// provider's login and returns the verification URL; the one-time user
	// code is surfaced through the optional LoginCodeReporter capability and
	// entered browser-side at that URL, never pasted back into lab.
	// LoginSubmitCode returns ErrLoginCodeUnsupported; completion is
	// provider-driven (the adapter polls its own status and publishes
	// provider.auth.changed).
	AuthFlowDeviceCode = "device-code"
	// AuthFlowAPIKey: a vault credential reference injected at spawn via the
	// existing materialization mechanism. SCHEMA ONLY in issue #51 — no
	// implementation stands behind it yet.
	AuthFlowAPIKey = "api-key"
	// AuthFlowExternal: auth is managed entirely outside lab; status-only.
	AuthFlowExternal = "external"
)

// AuthFlow is the provider-declared auth flow descriptor, surfaced on
// GET /api/v1/providers (issue #51 decision 7). Static provider metadata,
// like the model/effort catalogs. CredentialRef is reserved for the api-key
// kind (the vault reference materialized at spawn).
type AuthFlow struct {
	Kind          string `json:"kind"`
	Instructions  string `json:"instructions,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"` // reserved for api-key
}

// EventAuthChanged is the SSE event published when a provider's machine-level
// login state changes — provider-generic (issue #51 decision 7), replacing
// the old claude.auth.changed. The payload carries the provider id
// (AuthChangedPayload) so the SPA scopes its refetch to the right auth card.
const EventAuthChanged = "provider.auth.changed"

// AuthChangedPayload is EventAuthChanged's wire payload:
// {"type":"provider.auth.changed","provider":"<registered provider id>"}.
// Published by a provider's own login flow and by the httpapi logout handler
// (the two auth-mutating surfaces).
type AuthChangedPayload struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
}

// SeedOpts parametrizes SeedWorkspace. The trust + MCP grants and the
// .git/info/exclude entries are unconditional; Incogni (the repo's flag,
// D15 §9 measure 1) additionally makes the provider disable its own
// attribution output in the worktree's local settings, so commits and PRs
// from the session carry no AI markers at the source.
type SeedOpts struct {
	Incogni bool
}

// SeedMeta is the provider-declared workspace-seeding metadata (issue #51
// decision 8): the shapes internal/seeder hardcoded for claude — context file
// name, skills layout, exclude entries, incogni scrub/path patterns —
// declared by the provider so ONE generic seeder serves any agent CLI.
// internal/seeder consumes this declaration verbatim; the claude values are
// pinned by goldens on both sides (seeder testdata + claudecode's seedmeta
// test), so a drifting declaration fails a test rather than silently
// reshaping seeded worktrees. All paths are slash-separated and
// worktree-relative.
type SeedMeta struct {
	// ContextFileName is the generated per-worktree context file the agent
	// reads at the worktree root (claude: "CLAUDE.local.md").
	//
	// HARD RULE (issue #79 / ADR-0035): the name MUST be lab-owned and never
	// repo-tracked — the `.local`-suffixed convention ("CLAUDE.local.md",
	// "AGENTS.local.md"). Lab must never write a file repositories
	// legitimately track (e.g. codex's native "AGENTS.md"): a tracked file
	// cannot be hidden by .git/info/exclude, would be clobbered on every
	// re-seed, would dirty every diff, and would break incogni (D15 §9
	// measure 6). Making the agent CLI actually READ the lab-owned name is
	// the adapter's own job in its SeedWorkspace (a live-verified coupling,
	// the ADR-0008 bar) — never solved by declaring a tracked name here. The
	// conformance suite (#80) will assert the convention.
	ContextFileName string
	// SkillsDir is the directory the embedded skills bundle is seeded into
	// (claude: ".claude/skills").
	SkillsDir string
	// NativeSkillDiscovery declares whether the agent CLI discovers skills
	// seeded into SkillsDir on its own (claude: true — the .claude/skills
	// convention). When false and SkillsDir is non-empty, the seeder appends
	// a generated skills index to the context file — one line per bundled
	// skill, rendered from its frontmatter, pointing at its seeded path — so
	// an agent that only reads context files still finds the playbooks on
	// demand (issue #79 / ADR-0035). The bundle itself is identical for every
	// provider; only the discovery route differs.
	NativeSkillDiscovery bool
	// ExcludeEntries are the .git/info/exclude lines hiding every lab-seeded
	// file from git status (excludes touch only untracked files, so a
	// repo-committed sibling stays visible).
	ExcludeEntries []string
	// SeededPathPatterns are the pre-push guard BREs matching the paths lab
	// ACTUALLY seeds — deliberately narrower than ExcludeEntries, so the
	// guard never rejects a repo's own tracked provider config (commands,
	// agents, a committed settings file).
	SeededPathPatterns []string
	// ScrubPatterns are this provider's incogni attribution-marker patterns
	// (co-authored-by/session trailers) — one declaration, TWO enforcement
	// points (issue #75 / ADR-0033). The pre-push hook runs them as
	// case-insensitive POSIX BRE `grep -i` patterns against each outgoing
	// commit message; the agent-API body sanitizer compiles them (via
	// CompileScrubPatterns) into case-insensitive Go RE2 regexps and runs
	// them against outgoing agent-API bodies. Because one declaration feeds
	// both engines, every pattern must stay in the BRE∩RE2 common dialect
	// (claudecode's four patterns are the pinned example). Both enforcement
	// points apply the UNION across ALL registered providers
	// (Registry.UnionScrubPatterns/ScrubRegexps), not just the resolving
	// repo's own provider — a per-session provider override (ADR-0030) can
	// run any registered provider on any repo, so a marker only one OTHER
	// provider declares must still be caught.
	ScrubPatterns []string
}

// Universal chat schema (issue #7 / ADR-0016). A provider maps its native
// transcript into these provider-neutral kinds so lab's chat view — and any
// future provider — speaks one vocabulary. Every message the chat renders is
// exactly one MessageKind.
const (
	MessageText      = "text"      // user or assistant prose (Thinking marks hidden-by-default)
	MessageTool      = "tool"      // a tool call, rendered as a one-line chip
	MessageDialog    = "dialog"    // an interactive prompt: pending (Outcome nil) or answered (issue #56)
	MessageLifecycle = "lifecycle" // a session-level event (bridge status, error, end)
)

// Conversational state (issue #7 decision 11). The tailer derives one of these
// per live instance from the transcript tail; StateEnded is set by the caller
// from the run's terminal outcome, never from the transcript.
const (
	StateIdle       = "idle"        // spawned, no assistant activity yet
	StateWorking    = "working"     // agent mid-turn (generating or a tool is running)
	StateNeedsInput = "needs_input" // assistant ended its turn; awaiting the operator
	StateQuestion   = "question"    // an interactive dialog is pending
	StateEnded      = "ended"       // the run reached a terminal outcome
)

// Message is one entry of the universal chat schema — the provider-neutral
// projection of a transcript event. Seq is a strictly increasing, reparse-
// stable cursor (emission order); the API windows on it. Time is the event's
// raw provider timestamp string (already ISO-8601), passed through verbatim.
type Message struct {
	Seq      int64     `json:"seq"`
	Kind     string    `json:"kind"`
	Role     string    `json:"role,omitempty"`     // user|assistant for text; empty otherwise
	Time     string    `json:"time,omitempty"`     // raw provider timestamp, ISO-8601
	Text     string    `json:"text,omitempty"`     // text kind, or a lifecycle summary
	Thinking bool      `json:"thinking,omitempty"` // text kind: assistant thinking, hidden by default
	Error    bool      `json:"error,omitempty"`    // always-surface: this event is an error
	Tool     *ToolInfo `json:"tool,omitempty"`     // tool kind
	Dialog   *Dialog   `json:"dialog,omitempty"`   // dialog kind
}

// ToolInfo is the tool-kind payload: a one-line chip (Title) that expands on
// tap to the truncated Input/Output. Status moves running→ok|error when the
// tool result lands.
type ToolInfo struct {
	Name   string    `json:"name"`             // Bash, Edit, Skill, …
	Title  string    `json:"title"`            // chip text: "Edit main.go", "Ran go test"
	Input  string    `json:"input,omitempty"`  // truncated tool input
	Output string    `json:"output,omitempty"` // truncated tool result
	Status string    `json:"status"`           // running|ok|error
	View   *ToolView `json:"view,omitempty"`   // optional provider-neutral rich view (#146)
}

// Rich tool-view kinds (ToolView.Kind, issue #146). Provider-neutral: each
// kind names a rendering the web client already knows how to draw — no more.
const (
	ToolViewDiff    = "diff"    // Path + Text: a unified-diff body for an edit
	ToolViewCommand = "command" // Command: a shell command line
	ToolViewWrite   = "write"   // Path + Text: a file's written content
)

// ToolView is the optional provider-neutral rich view of a tool call (issue
// #146): the structured, panel-sized companion to ToolInfo's one-line chip.
// Each provider's chat mapper PRODUCES it from that provider's native tool
// payload; the web client RENDERS it purely by Kind, so no tool-name knowledge
// ever leaks into the SPA — a "diff" view draws identically whether it came
// from claude-code's Edit or codex's apply_patch. Absent (View nil) the client
// falls back to the raw Input/Output chip fields, so a tool the mapper does not
// recognize still renders. Path travels BESIDE the body text (a unified diff
// here carries no ---/+++ file header — see internal/unidiff), and the fields a
// given Kind does not use stay empty (see the kind constants above).
type ToolView struct {
	Kind    string `json:"kind"`              // ToolView* (diff|command|write)
	Path    string `json:"path,omitempty"`    // diff, write: the file path
	Text    string `json:"text,omitempty"`    // diff: unified-diff text; write: file content
	Command string `json:"command,omitempty"` // command: the shell command line
}

// DetailTruncateLimit bounds a rich view's panel-sized body (ToolView.Text,
// issue #146). It is an order of magnitude above the 2000-byte chip-facing
// Input/Output cap on purpose: a chip is a one-line glance, a panel shows the
// whole edit.
const DetailTruncateLimit = 20000

// TruncateDetail caps s at DetailTruncateLimit bytes for a rich-view panel,
// making the cut EXPLICIT — never a silent truncation. When it must cut, it
// backs up to a UTF-8 rune boundary (so the cut never splits a sequence) and
// appends a marker naming the ORIGINAL size, rounded up to whole KB; the marker
// rides IN the returned text so the client renders it with zero extra logic.
// Cutting the TAIL of a unified diff leaves a valid, renderable head (hunks are
// independent and read from the top), which is what makes a plain length cap
// safe here. Returns s unchanged when it already fits.
func TruncateDetail(s string) string {
	if len(s) <= DetailTruncateLimit {
		return s
	}
	n := DetailTruncateLimit
	// Back up to a rune boundary so the cut never splits a UTF-8 sequence.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	kb := (len(s) + 1023) / 1024
	return s[:n] + "\n… truncated (" + strconv.Itoa(kb) + " KB total)"
}

// Dialog kinds (Dialog.Kind). A provider maps each interactive prompt it
// surfaces onto this vocabulary (issue #51 decision 3): a question form (one
// question or several), a plan review (Prompt carries the plan body), and
// "approval" — reserved for generic yes/no approvals that are neither a
// question form nor a plan review.
const (
	DialogKindQuestion = "question"
	DialogKindPlan     = "plan"
	DialogKindApproval = "approval"
)

// Dialog is the dialog-kind payload: an interactive prompt detected as a tool
// call in the transcript (or the live signal spool). When Answerable, the UI
// renders native option buttons and answers via AnswerDialog; otherwise it
// degrades to the provider's web open affordance hint (unknown/unsupported
// dialog shapes — never scrape the TUI widget).
//
// Two shapes share this struct (issue #51 decision 3). A SINGLE-question
// dialog uses the flat fields (Prompt/Options/Multi) with Questions nil —
// the original shape, byte-for-byte unchanged, so existing compat snapshots
// stay valid; plan approval is single-question-shaped (Kind=plan, Prompt=the
// plan markdown, Options=the approve/reject rows). A MULTI-question dialog
// (len(Questions) >= 2) carries each question in Questions, Prompt degrades
// to a short summary (e.g. "3 questions"), and the flat Options/Multi stay
// empty.
//
// An ANSWERED dialog stays a dialog message in history (issue #56 decision 3
// — no more demotion to a raw tool chip): Outcome carries the resolution the
// provider recorded, and the UI renders a compact Q→A summary instead of the
// interactive picker. Outcome nil means the dialog is still pending.
type Dialog struct {
	ToolID     string         `json:"tool_id"`
	Kind       string         `json:"dialog_kind"` // DialogKind* (question|plan|approval)
	Prompt     string         `json:"prompt"`
	Options    []DialogOption `json:"options,omitempty"`
	Multi      bool           `json:"multi,omitempty"`     // multi-select (Space toggles)
	Questions  []Question     `json:"questions,omitempty"` // multi-question form ONLY (len >= 2)
	Answerable bool           `json:"answerable"`
	Outcome    *DialogOutcome `json:"outcome,omitempty"` // resolution of an answered dialog; nil while pending
}

// DialogOutcome is the recorded resolution of an answered dialog, derived
// from the transcript's toolUseResult/toolDenialKind ground truth (compat §5).
type DialogOutcome struct {
	Dismissed bool             `json:"dismissed,omitempty"` // resolved without an answer: denial, interrupt, unattended timeout, or unreadable result
	Results   []QuestionResult `json:"results,omitempty"`   // question kind: one per question, dialog order
	Approved  bool             `json:"approved,omitempty"`  // plan kind: the plan was approved
	Feedback  string           `json:"feedback,omitempty"`  // plan kind: rejection feedback text, if typed
}

// QuestionResult is one question's resolved answer for display.
type QuestionResult struct {
	Question  string   `json:"question"`             // the question text
	Chosen    []string `json:"chosen,omitempty"`     // chosen listed option label(s), recorded order
	OtherText string   `json:"other_text,omitempty"` // free-text answer (the synthesized Other row)
}

// Question is one question of a multi-question Dialog (issue #51 decision 3).
// Options include the provider-synthesized free-text "Other" row (IsOther)
// where the provider allows free text — the Other row IS the "free text
// allowed" signal, the same convention as the flat single-question shape.
type Question struct {
	Header      string         `json:"header,omitempty"` // short chip label above the question
	Text        string         `json:"text"`             // the question itself
	Options     []DialogOption `json:"options"`
	MultiSelect bool           `json:"multi_select,omitempty"`
}

// DialogOption is one tappable choice of a Dialog.
type DialogOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	IsOther     bool   `json:"is_other,omitempty"` // free-text "Other" row (the operator supplies text)
}

// QuestionAnswer is the operator's answer to one question of a multi-question
// Dialog. Index is the single-select choice into that question's Options;
// Selected the multi-select toggles; OtherText the free text when the chosen
// row IsOther.
type QuestionAnswer struct {
	Index     int    `json:"index"`
	Selected  []int  `json:"selected,omitempty"`
	OtherText string `json:"other_text,omitempty"`
}

// DialogAnswer is the operator's response to a pending Dialog. For the flat
// single-question shape, indices are into Dialog.Options: single-select uses
// Index, multi-select uses Selected (the options to toggle on), and OtherText
// is the free text when the chosen row IsOther. For a multi-question dialog,
// Answers carries one QuestionAnswer per Dialog.Questions entry, all
// collected in one submit; when Answers is non-empty the flat fields are
// ignored (issue #51 decision 3).
type DialogAnswer struct {
	Index     int              `json:"index"`
	Selected  []int            `json:"selected,omitempty"`
	OtherText string           `json:"other_text,omitempty"`
	Answers   []QuestionAnswer `json:"answers,omitempty"` // per-question; wins over the flat fields
}

// CommandRoleClear marks a provider's native clear-context command in its
// command catalog (claude /clear; a codex adapter would mark /new). The role
// is pure discovery (issue #51 decision 2): the frontend may bind a "New
// conversation" affordance to it, but execution rides the normal Reply path
// as pasted text — there is no seam-level clear method, and lab detects the
// clear by its EFFECT (the transcript identity rotating, see
// LocateTranscript). The Role vocabulary is extensible; only "clear" is
// defined so far.
const CommandRoleClear = "clear"

// CommandSpec is one entry of a provider's slash-command catalog (issue #51
// decision 5), the composer's autocomplete source. Source says where the
// command was discovered (builtin|project|user); Role optionally tags a
// semantic the frontend binds affordances to (CommandRoleClear). ChatSafe is
// the adapter's curation verdict — see Commands.
type CommandSpec struct {
	Name        string `json:"name"` // bare name WITHOUT the leading slash, e.g. "clear"
	Description string `json:"description,omitempty"`
	ArgHint     string `json:"arg_hint,omitempty"`
	Source      string `json:"source"` // builtin|project|user, or "lab" for a server-owned lab command
	Role        string `json:"role,omitempty"`
	ChatSafe    bool   `json:"chat_safe"` // false: would strand the TUI in a picker lab cannot see
}

// Chat is a conversation read through ReadChat into the universal schema: the
// ordered transcript-derived messages, the adapter-composed conversational
// state, and the cursor (the highest Seq present, 0 when empty).
type Chat struct {
	Messages []Message `json:"messages"`
	State    string    `json:"state"`
	Cursor   int64     `json:"cursor"`
	// PendingDialog is the adapter's LIVE pending interactive dialog (issue
	// #92; claude-code reads it from the PreToolUse spool, ADR-0020), nil when
	// none is pending. A side-channel field, deliberately NOT a message in
	// Messages: the message stream stays purely transcript-derived, so seq
	// numbers remain reparse-stable when the real tool_use retro-flushes
	// (issues #89/#90). When set, State is StateQuestion. A pending dialog the
	// TRANSCRIPT itself shows (the dormant flushed-tool_use fallback) stays in
	// Messages and does not populate this field.
	PendingDialog *Dialog `json:"pending_dialog,omitempty"`
}

// AgentProvider is everything lab needs from a coding agent (design §4d).
// Implementations own their fragile CLI couplings; callers never shell out
// to the agent binary themselves.
type AgentProvider interface {
	// ID is the stable identifier stored in repos.provider / runs.provider.
	ID() string
	// DisplayName is the human name driving ALL user-facing copy ("Claude
	// Code") — the SPA renders it instead of hardcoding any provider's name
	// (issue #51 decision 9).
	DisplayName() string
	// Models and Efforts are the provider-owned catalogs, in dropdown order.
	Models() []Option
	Efforts() []Option
	// SpawnOptions is the provider-owned catalog of generic spawn options
	// (issue #19 / ADR-0021) — the declared schema lab renders and validates
	// the options bag against. Empty when the provider declares none.
	SpawnOptions() []OptionSpec
	// SpawnArgv builds the full command an instance session runs from a
	// SpawnSpec. Empty model/effort omit the respective flag. A non-empty
	// InitialPrompt is carried as the agent's trailing positional argument (the
	// AFK seed prompt) — the pinned v0 mechanism, so the prompt exists before
	// the process and is never raced by a post-spawn keystroke injection; manual
	// spawns pass "" and get no trailing argument. The provider applies
	// spec.Options itself (a provider-owned mechanism, never a lab concern).
	SpawnArgv(spec SpawnSpec) []string
	// AuthFlow declares how an operator logs this provider in — the
	// descriptor the SPA's auth card renders (issue #51 decision 7). Static
	// metadata, like the catalogs.
	AuthFlow() AuthFlow
	// AuthStatus reports the machine-level login state. Results are cached
	// briefly inside the provider; force bypasses the cache — spawn
	// decisions MUST force (never trust the cache before a spawn). An error
	// is returned alongside a logged-out status; callers treat it as
	// logged-out and may log it.
	AuthStatus(ctx context.Context, force bool) (AuthStatus, error)
	// LoginStart begins (or re-joins) the interactive login flow and
	// returns the OAuth authorize URL, or "" when it could not be captured
	// yet (the flow is still usable — retry LoginStart to re-scrape).
	LoginStart(ctx context.Context) (oauthURL string, err error)
	// LoginSubmitCode delivers the pasted OAuth code to the pending login
	// flow and waits for the login to land.
	LoginSubmitCode(ctx context.Context, code string) error
	// Logout drops the machine's current provider account so a fresh one can
	// be logged in — the symmetric counterpart to the login flow. Auth is
	// machine-level, so this is a machine-wide cut: it does NOT stop running
	// instances (they survive on their in-memory token until it refreshes,
	// then fail). Success is decided from a force-refreshed status, not the
	// underlying command's exit code; the refresh leaves the provider's auth
	// cache reflecting the logged-out truth. Returns an error only when the
	// account could not be dropped.
	Logout(ctx context.Context) error
	// SeedWorkspace pre-approves a fresh worktree so the agent launches
	// unattended (trust/MCP grants, ignore entries). Called after the
	// worktree exists and before the session spawns; a failure aborts the
	// Start (caller rolls back).
	SeedWorkspace(worktree string, opts SeedOpts) error
	// SeedMeta declares the provider's workspace-seeding metadata (issue #51
	// decision 8): the context-file name, skills layout, exclude entries, and
	// incogni scrub/path patterns the generic lab-side seeder consumes —
	// SeedWorkspace stays the provider's OWN grants, SeedMeta describes what
	// lab seeds on the provider's behalf.
	SeedMeta() SeedMeta

	// --- Chat surface (issue #7 / ADR-0016, generalized in issue #51) -------
	// Every method below owns a fragile agent-CLI coupling pinned in
	// internal/compat (per adapter): the transcript location + schema, the
	// send-keys reply/dialog/interrupt recipes, and the command catalog.

	// Commands is the provider's slash-command catalog for a session running
	// in worktree (issue #51 decision 5) — the composer's autocomplete
	// source. Worktree-dependent by design: project- and user-level commands
	// are discovered relative to the worktree (builtins are not), which is
	// why the API serves it per run. The adapter CURATES the list: a command
	// that would strand the TUI in an interactive picker lab cannot see
	// ships ChatSafe=false and is filtered from the chat surface. Role tags
	// entry semantics — CommandRoleClear marks the provider's native
	// clear-context command; execution of any chosen command rides the
	// normal Reply path as pasted text, never a dedicated seam method.
	Commands(ctx context.Context, worktree string) ([]CommandSpec, error)
	// LocateTranscript finds the provider-native transcript file for the
	// session running in worktree, keyed by cwd-match analogous to
	// CaptureDeepLink. Returns "" (no error) when no transcript is found yet.
	// The path is persisted on the run row so ended runs stay readable.
	//
	// Clear/epoch obligation (issue #51 decision 2): an adapter MUST surface
	// a NEW conversation identity here whenever its provider clears context —
	// lab detects a clear by its EFFECT (the located transcript changing),
	// never by knowing the provider's clear command. Claude Code rotates to a
	// fresh transcript file on /clear (compat §5), satisfying this for free;
	// an adapter whose CLI clears IN PLACE must synthesize a new epoch (e.g.
	// an epoch-qualified path) so the identity still rotates.
	LocateTranscript(ctx context.Context, sessionName, worktree string) (string, error)
	// ReadChat reads the run's conversation and composes its conversational
	// state — the adapter owns "what state is my agent in", composed from
	// whatever its agent's best signals are (issue #92): the transcript at
	// transcriptPath plus any adapter-private live signals spooled under
	// runtimeDir for runID (claude-code: the hook spool, compat §9; codex:
	// nothing — a pure rollout fold). Core owns run lifecycle only (the
	// StateEnded override, transcript identity) and never interprets an
	// adapter's signals.
	//
	// transcriptPath may be "" — an active run whose transcript is not yet
	// located: the adapter must still consult its live signals (a pending
	// dialog can exist before LocateTranscript first hits) and otherwise
	// returns an idle empty chat, never an error. runtimeDir may be "" — no
	// runtime dir configured, or the caller wants a transcript-only read
	// (core reads ENDED runs this way): live signals are off and the read
	// degrades to the transcript alone. A non-empty transcriptPath that no
	// longer exists returns ErrTranscriptGone so the caller can show the
	// "transcript no longer available" state.
	//
	// A live pending dialog is surfaced on Chat.PendingDialog with State =
	// StateQuestion — the side-channel field, never a synthetic message (its
	// doc pins the seq-stability invariant, issues #89/#90).
	// LocateTranscript's clear/epoch obligation applies here too: reading the
	// post-clear identity yields the fresh conversation with seq restarted —
	// a cleared conversation never continues an old seq stream.
	ReadChat(runID, runtimeDir, transcriptPath string) (Chat, error)
	// Reply delivers a free-text operator reply to the live session (mid-
	// session send-keys; the argv-only stance covers the initial prompt
	// only).
	//
	// Reply while StateWorking is LEGAL and best-effort per TUI (issue #51
	// decision 10): claude queues mid-turn input natively, so lab never
	// gates a reply on conversational state. An adapter whose TUI DROPS
	// mid-turn input must return a typed error the UI can surface — never
	// silently lose the operator's text.
	Reply(ctx context.Context, sessionName, text string) error
	// AnswerDialog answers a pending interactive dialog via the pinned
	// keystroke recipe. dialog is the Dialog the caller read; answer is the
	// operator's selection (per-question Answers for a multi-question
	// dialog). Returns ErrDialogNotAnswerable if dialog is not Answerable.
	AnswerDialog(ctx context.Context, sessionName string, dialog Dialog, answer DialogAnswer) error
	// Interrupt sends the session's interrupt keystroke (claude: Escape) —
	// the chat Stop-generating affordance, distinct from a run Stop.
	//
	// Interrupt recipes are adapter-owned and MUST be hazard-checked against
	// the real TUI before shipping (issue #51 decision 10): some TUIs quit —
	// or discard work — on double-Esc/Ctrl-C, and a chat interrupt must never
	// be able to kill the session.
	Interrupt(ctx context.Context, sessionName string) error
}

// ErrTranscriptGone is returned by ReadChat when the transcript file is
// no longer on disk (the provider retired it after the run ended). Callers
// render a graceful "transcript no longer available" state.
var ErrTranscriptGone = errors.New("provider: transcript no longer available")

// Interaction/auth sentinels of the seam, provider-generic so httpapi maps
// status codes without importing any concrete provider (issue #51 decision
// 7; moved here from claudecode, which keeps thin aliases so its internal
// errors.Is chains hold). Adapters return them — usually wrapped with the
// concrete cause — from their own recipes and login flows.
var (
	// ErrDialogNotAnswerable is returned by AnswerDialog for a dialog lab
	// cannot drive from structured input (Answerable=false — an unknown or
	// unsupported shape). The UI shows the provider's web open affordance /
	// tmux attach hint for these instead of option buttons. Maps to 409.
	ErrDialogNotAnswerable = errors.New("dialog is not answerable from lab; open it in claude.ai")
	// ErrInvalidReply wraps a rejected reply (empty after trim, oversize, or
	// a forbidden control character). Maps to 400.
	ErrInvalidReply = errors.New("invalid reply")
	// ErrInvalidCode wraps a rejected login code (empty / oversize / control
	// characters). Maps to 400.
	ErrInvalidCode = errors.New("invalid login code")
	// ErrLoginTimeout is returned when auth status does not flip within the
	// provider's login timeout after the code was delivered; the stuck login
	// attempt has been torn down. Maps to 504.
	ErrLoginTimeout = errors.New("login did not complete in time — try again")
	// ErrLoginCodeUnsupported is returned by LoginSubmitCode for flows
	// (device-code) where the code travels operator→browser — entered at the
	// verification URL, never pasted back into lab. Maps to 409.
	ErrLoginCodeUnsupported = errors.New("provider: this login flow does not take a pasted code")
)

// ConnectingReporter is the optional render-state extension: a provider
// whose deep-link capture has an in-flight set reports it here, driving the
// UI's "connecting…" state (pinned instances API field `connecting`).
type ConnectingReporter interface {
	Connecting(sessionName string) bool
}

// LoginCodeReporter is the optional device-code capability: a provider whose
// auth flow is AuthFlowDeviceCode surfaces the pending login attempt's
// one-time user code here, and httpapi echoes it in the login/start response
// so the operator can enter it browser-side at the verification URL — the
// code never travels back into lab. Advertised by type assertion at the call
// site, exactly like ConnectingReporter and DeepLinker (ADR-0017).
type LoginCodeReporter interface {
	// PendingLoginCode returns the one-time user code of the login attempt
	// currently pending ("" when no login is pending or the flow has no
	// code).
	PendingLoginCode() string
}

// OpenAffordance is a provider's generic "open the session on the web" hint,
// shown when no exact deep link was captured: the URL to open and the human
// tooltip that explains where it lands (e.g. the claude.ai session picker).
// Provider-owned metadata (ADR-0017), exposed through the providers API and
// rendered by the SPA. A provider with no web surface has no OpenAffordance,
// and its instance rows show a copyable tmux-attach affordance instead.
type OpenAffordance struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// LiveSignals is the optional live-signal LIFECYCLE capability (ADR-0020;
// renamed from DialogHooker in issue #51 decision 6; narrowed in issue #92):
// the plumbing lab genuinely owns for an adapter whose agent spools live
// signals under lab's runtime dir — arming the channel at spawn (Setup),
// cheap change detection for the tailer (SpoolSig), and runtime-dir GC
// (SweepSpools). What the signals MEAN is not a seam concept: the adapter
// interprets its own spool inside ReadChat (issue #92 — state composition is
// adapter-owned), so core never reads a dialog or blocked state from here.
// lab owns the runtime spool directory and its lifecycle (it creates the
// dir, writes the per-run settings file, and GCs spools for ended runs); the
// provider owns the settings shape and the spool file protocol — for
// claude-code a fragile coupling pinned in compat §9. Advertised
// structurally (a type assertion at the call site, exactly like
// ConnectingReporter and DeepLinker).
//
// Honest degradation (issue #51 decision 6): a provider WITHOUT a verified
// structured signal channel simply omits the capability — the never-scrape
// rule is universal, auto-approval spawn defaults keep dialogs rare, and its
// ReadChat composes from the transcript alone — the messages-scan dialog
// path stays as a dormant fallback that lights up automatically if a future
// provider flushes pending tool_use.
type LiveSignals interface {
	// Setup builds the per-run settings/config payload that arms the agent's
	// live signal channel (claude: the PreToolUse/PostToolUse/Notification
	// dialog-capture hooks) to spool into the runtime dir keyed by runID. It
	// returns the file bytes, the path lab must write them to (the provider
	// owns the runtime layout under dir), and the spawn args to append (e.g.
	// ["--settings", settingsPath]). lab writes the bytes at settingsPath and
	// appends the args to the spawn argv. The spooling commands self-create
	// their subdirs, so a run whose dialog opens after a lab restart still
	// spools with no re-arming. opts carries the per-run knobs Setup folds
	// into the same payload (issue #124); the zero value yields the baseline
	// payload.
	Setup(runID, dir string, opts SetupOpts) (settings []byte, settingsPath string, args []string)

	// SpoolSig is a cheap change-detector over the run's spool + marker files
	// (existence + mtime + size) so the tailer notices a dialog appearing while
	// the transcript is byte-frozen and republishes state. "" when neither
	// file exists.
	SpoolSig(runID, dir string) string

	// SweepSpools removes the spool, marker, and per-run settings file under
	// dir for every run whose keep(runID) is false — the run-ended GC (a spool
	// for a non-active run is garbage; one for an active run survives a lab
	// restart, so keep the active set). A nil keep removes all.
	SweepSpools(dir string, keep func(runID string) bool) error
}

// SetupOpts carries the per-run knobs LiveSignals.Setup folds into the
// settings payload (issue #124). The zero value yields the baseline payload.
type SetupOpts struct {
	// DialogTimeout, when > 0, is the unattended-dialog auto-dismiss window
	// the adapter should impose on its CLI (claude: CLAUDE_AFK_TIMEOUT_MS in
	// the settings env block, compat §11). Zero leaves the CLI's own default
	// untouched (AFK runs).
	DialogTimeout time.Duration
}

// DeepLinker is the optional deep-link capability on the provider seam
// (ADR-0017), following the ConnectingReporter pattern (a type assertion at
// the call site). A provider whose sessions have a web surface implements it;
// a headless CLI with no remote host omits it, and lab arms no capture
// machinery for its runs (deep_link_url stays NULL) and renders no web open
// link for them.
type DeepLinker interface {
	// CaptureDeepLink polls for the session's deep link, keyed by its
	// worktree (the one cwd unique to the session). On a miss it returns an
	// empty string — a generic fallback is NEVER returned through capture, so
	// the caller's write-only-on-hit rule needs no provider-specific constant.
	CaptureDeepLink(ctx context.Context, sessionName, worktree string) (string, error)
	// FallbackOpen is the provider's generic web open affordance, rendered
	// when no exact link was captured (URL + explanatory tooltip). Owned by
	// the provider so no core code or SPA hardcodes a provider URL.
	FallbackOpen() OpenAffordance
}

// HasOption reports whether a catalog contains value — the validation
// helper for spawn requests against Models()/Efforts().
func HasOption(opts []Option, value string) bool {
	for _, o := range opts {
		if o.Value == value {
			return true
		}
	}
	return false
}

// FindSpawnOption returns the OptionSpec declaring key, or false — the lookup
// behind spawn-options validation/filtering.
func FindSpawnOption(specs []OptionSpec, key string) (OptionSpec, bool) {
	for _, s := range specs {
		if s.Key == key {
			return s, true
		}
	}
	return OptionSpec{}, false
}

// ValidOptionValue reports whether v is a legal value for spec's type. Bool
// options accept exactly "true"/"false" (the reserved enum/string types have
// no MVP validation and pass through).
func ValidOptionValue(spec OptionSpec, v string) bool {
	switch spec.Type {
	case OptionTypeBool:
		return v == "true" || v == "false"
	default:
		return true
	}
}

// ValidateSpawnOptions checks a bag against a provider's declared schema
// (issue #19): every key must be declared and its value legal for the option's
// type. It returns a descriptive error (an unknown key or a bad value — the
// 400 discipline that mirrors an unknown model/effort) or nil. A nil/empty bag
// is always valid.
func ValidateSpawnOptions(specs []OptionSpec, bag map[string]string) error {
	for key, val := range bag {
		spec, ok := FindSpawnOption(specs, key)
		if !ok {
			return fmt.Errorf("unknown spawn option %q", key)
		}
		if !ValidOptionValue(spec, val) {
			return fmt.Errorf("invalid value %q for spawn option %q", val, key)
		}
	}
	return nil
}

// FilterSpawnOptions returns the subset of bag whose keys this provider
// declares (issue #19): a global bag may span providers once more than one
// exists, so at spawn it is narrowed to the resolving repo's provider. A key
// the provider does not declare is dropped, not an error. Returns a non-nil map
// (possibly empty).
func FilterSpawnOptions(specs []OptionSpec, bag map[string]string) map[string]string {
	out := make(map[string]string, len(bag))
	for key, val := range bag {
		if _, ok := FindSpawnOption(specs, key); ok {
			out[key] = val
		}
	}
	return out
}
