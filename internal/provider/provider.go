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
	"time"
)

// Option is one entry of a provider-owned catalog (model or effort
// dropdown): Value is what the provider's CLI accepts, Label the human text
// (pinned API shape: {value,label}).
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// AuthStatus is the machine-level login state of a provider (pinned API:
// GET /api/v1/providers/claude/auth/status). CheckedAt is when the
// underlying status command actually ran — a cached read keeps the older
// stamp. Serialize timestamps with store.FormatTime at the API layer.
type AuthStatus struct {
	LoggedIn  bool      `json:"logged_in"`
	Email     string    `json:"email"`
	Method    string    `json:"method"`
	CheckedAt time.Time `json:"checked_at"`
}

// SeedOpts parametrizes SeedWorkspace. The trust + MCP grants and the
// .git/info/exclude entries are unconditional; Incogni (the repo's flag,
// D15 §9 measure 1) additionally makes the provider disable its own
// attribution output in the worktree's local settings, so commits and PRs
// from the session carry no AI markers at the source.
type SeedOpts struct {
	Incogni bool
}

// Universal chat schema (issue #7 / ADR-0016). A provider maps its native
// transcript into these provider-neutral kinds so lab's chat view — and any
// future provider — speaks one vocabulary. Every message the chat renders is
// exactly one MessageKind.
const (
	MessageText      = "text"      // user or assistant prose (Thinking marks hidden-by-default)
	MessageTool      = "tool"      // a tool call, rendered as a one-line chip
	MessageDialog    = "dialog"    // a pending interactive prompt awaiting the operator
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
	Name   string `json:"name"`             // Bash, Edit, Skill, …
	Title  string `json:"title"`            // chip text: "Edit main.go", "Ran go test"
	Input  string `json:"input,omitempty"`  // truncated tool input
	Output string `json:"output,omitempty"` // truncated tool result
	Status string `json:"status"`           // running|ok|error
}

// Dialog is the dialog-kind payload: an interactive prompt detected as an
// unanswered tool call in the transcript. When Answerable, the UI renders
// native option buttons from Options and answers via AnswerDialog; otherwise
// it degrades to a "open in claude.ai" deep-link hint (unknown/unsupported
// dialog shapes — never scrape the TUI widget).
type Dialog struct {
	ToolID     string         `json:"tool_id"`
	DialogKind string         `json:"dialog_kind"` // question|plan|unknown
	Prompt     string         `json:"prompt"`
	Options    []DialogOption `json:"options,omitempty"`
	Multi      bool           `json:"multi,omitempty"` // multi-select (Space toggles)
	Answerable bool           `json:"answerable"`
}

// DialogOption is one tappable choice of a Dialog.
type DialogOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	IsOther     bool   `json:"is_other,omitempty"` // free-text "Other" row (select, type, Enter)
}

// DialogAnswer is the operator's response to a pending Dialog. Indices are
// into Dialog.Options. Single-select uses Index; multi-select uses Selected
// (the options to toggle on). OtherText is the free text when the chosen row
// IsOther.
type DialogAnswer struct {
	Index     int    `json:"index"`
	Selected  []int  `json:"selected,omitempty"`
	OtherText string `json:"other_text,omitempty"`
}

// Chat is a transcript read into the universal schema: the ordered messages,
// the derived conversational state, and the cursor (the highest Seq present,
// 0 when empty).
type Chat struct {
	Messages []Message `json:"messages"`
	State    string    `json:"state"`
	Cursor   int64     `json:"cursor"`
}

// AgentProvider is everything lab needs from a coding agent (design §4d).
// Implementations own their fragile CLI couplings; callers never shell out
// to the agent binary themselves.
type AgentProvider interface {
	// ID is the stable identifier stored in repos.provider / runs.provider.
	ID() string
	// Models and Efforts are the provider-owned catalogs, in dropdown order.
	Models() []Option
	Efforts() []Option
	// SpawnArgv builds the full command an instance session runs. Empty
	// model/effort omit the respective flag. A non-empty initialPrompt is
	// carried as the agent's trailing positional argument (the AFK seed
	// prompt) — the pinned v0 mechanism, so the prompt exists before the
	// process and is never raced by a post-spawn keystroke injection; manual
	// spawns pass "" and get no trailing argument.
	SpawnArgv(sessionName, model, effort, initialPrompt string) []string
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
	// SeedWorkspace pre-approves a fresh worktree so the agent launches
	// unattended (trust/MCP grants, ignore entries). Called after the
	// worktree exists and before the session spawns; a failure aborts the
	// Start (caller rolls back).
	SeedWorkspace(worktree string, opts SeedOpts) error

	// --- Chat surface (issue #7 / ADR-0016) ---------------------------------
	// Every method below owns a fragile Claude Code coupling pinned in
	// internal/compat: the transcript location + JSONL schema, and the
	// send-keys reply/dialog/interrupt recipes.

	// LocateTranscript finds the provider-native transcript file for the
	// session running in worktree, keyed by cwd-match analogous to
	// CaptureDeepLink. Returns "" (no error) when no transcript is found yet.
	// The path is persisted on the run row so ended runs stay readable.
	LocateTranscript(ctx context.Context, sessionName, worktree string) (string, error)
	// ReadTranscript reads and maps the transcript at path into the universal
	// schema. A path that no longer exists returns ErrTranscriptGone so the
	// caller can show the "transcript no longer available" state.
	ReadTranscript(path string) (Chat, error)
	// Reply delivers a free-text operator reply to the live session (mid-
	// session send-keys; the argv-only stance covers the initial prompt
	// only). If the agent is mid-turn the provider's target TUI queues it.
	Reply(ctx context.Context, sessionName, text string) error
	// AnswerDialog answers a pending interactive dialog via the pinned
	// keystroke recipe. dialog is the Dialog the caller read; answer is the
	// operator's selection. Errors if dialog is not Answerable.
	AnswerDialog(ctx context.Context, sessionName string, dialog Dialog, answer DialogAnswer) error
	// Interrupt sends the session's interrupt keystroke (Escape) — the chat
	// Stop-generating affordance, distinct from a run Stop.
	Interrupt(ctx context.Context, sessionName string) error
}

// ErrTranscriptGone is returned by ReadTranscript when the transcript file is
// no longer on disk (the provider retired it after the run ended). Callers
// render a graceful "transcript no longer available" state.
var ErrTranscriptGone = errors.New("provider: transcript no longer available")

// ConnectingReporter is the optional render-state extension: a provider
// whose deep-link capture has an in-flight set reports it here, driving the
// UI's "connecting…" state (pinned instances API field `connecting`).
type ConnectingReporter interface {
	Connecting(sessionName string) bool
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
