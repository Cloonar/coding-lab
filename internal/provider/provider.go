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

// DialogHooker is the optional capability (ADR-0020) that makes a *pending*
// interactive dialog observable while it is still open — working around Claude
// Code not flushing a pending tool_use to the transcript until it resolves
// (compat §5). lab owns the runtime spool directory and its lifecycle (it
// creates the dir, writes the per-run settings file, and GCs spools for ended
// runs); the provider owns the hook-settings shape and the spool file protocol,
// a fragile Claude Code coupling pinned in compat §9. Advertised structurally
// (a type assertion at the call site, exactly like ConnectingReporter and
// DeepLinker); a provider with no live-hook surface omits it and lab keeps the
// transcript-only behavior — the messages-scan dialog path stays as a dormant
// fallback that lights up automatically if a future provider flushes pending
// tool_use.
type DialogHooker interface {
	// HookSettings builds the per-run settings file that wires the agent's
	// dialog-capture hooks (PreToolUse/PostToolUse/Notification) to spool into
	// the runtime dir keyed by runID. It returns the file bytes, the path lab
	// must write them to (the provider owns the runtime layout under dir), and
	// the spawn args to append (e.g. ["--settings", settingsPath]). lab writes
	// the bytes at settingsPath and appends the args to the spawn argv. The
	// hook commands self-create their spool subdirs, so a run whose dialog
	// opens after a lab restart still spools with no re-arming.
	HookSettings(runID, dir string) (settings []byte, settingsPath string, args []string)

	// PendingDialog reports the run's live pending interactive dialog, read
	// from the PreToolUse spool under dir and mapped through the same mapper as
	// the transcript (one mapper, two sources). ok is false when no spool
	// exists, it is unreadable, or the spooled dialog's tool_use_id already
	// appears in transcriptPath — the tool_use is flushed to the transcript
	// only on resolution, so its presence there means the dialog is answered
	// (the PostToolUse-hook spool delete is the primary clear; this
	// resolved-in-transcript scan is the backstop for a missed hook / Esc /
	// process death). transcriptPath "" skips the resolved check.
	PendingDialog(runID, dir, transcriptPath string) (Dialog, bool)

	// BlockedState reports a residual blocked conversational state derived from
	// the Notification spool marker under dir (permission_prompt / idle_prompt /
	// agent_needs_input → StateNeedsInput) — the badge fix for blocked states
	// that carry no structured dialog, including a plain tool-permission prompt
	// and the post-decline "stuck on working" bug. ok is false when no marker
	// exists or the transcript has advanced past it (transcriptPath written
	// after the marker → next activity resolved the block). transcriptPath ""
	// treats any marker as current.
	BlockedState(runID, dir, transcriptPath string) (state string, ok bool)

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
