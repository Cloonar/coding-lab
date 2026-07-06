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

// SeedOpts parametrizes SeedWorkspace. Empty in M3: the trust + MCP grants
// and the .git/info/exclude entries are unconditional. M7 adds the incogni
// attribution-disabling keys here.
type SeedOpts struct{}

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
	// model/effort omit the respective flag.
	SpawnArgv(sessionName, model, effort string) []string
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
	// CaptureDeepLink polls for the session's deep link, keyed by its
	// worktree (the one cwd unique to the session). On a miss it returns
	// the provider's generic fallback link — callers must never persist
	// the generic link over a previously captured real one.
	CaptureDeepLink(ctx context.Context, sessionName, worktree string) (string, error)
	// SeedWorkspace pre-approves a fresh worktree so the agent launches
	// unattended (trust/MCP grants, ignore entries). Called after the
	// worktree exists and before the session spawns; a failure aborts the
	// Start (caller rolls back).
	SeedWorkspace(worktree string, opts SeedOpts) error
}

// ConnectingReporter is the optional render-state extension: a provider
// whose deep-link capture has an in-flight set reports it here, driving the
// UI's "connecting…" state (pinned instances API field `connecting`).
type ConnectingReporter interface {
	Connecting(sessionName string) bool
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
