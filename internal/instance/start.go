package instance

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// StartParams is the manual-instance start input (pinned API: POST
// /api/v1/repos/{id}/instances {label?, provider?, model?, effort?, remote?,
// first_message?}).
type StartParams struct {
	RepoID   string
	Label    string // optional user label; sanitized + timestamped into the instance label
	Provider string // optional per-spawn provider pick (issue #66); "" → repo/settings default
	Model    string // optional per-spawn override; "" → repo/settings default
	Effort   string // optional per-spawn override; "" → repo/settings default
	// Remote is the optional per-spawn remote-control pick (issue #163). A
	// POINTER because the knob is boolean: nil = no explicit pick (inherit the
	// repo/settings layers), &false = an explicit "off" that must beat a global
	// "on" — a plain bool could not tell those two apart.
	Remote *bool
	// FirstMessage is the operator's first chat message (issue #96), already
	// shape-validated by the httpapi layer (whitespace-only normalized to "",
	// size-capped). Threaded into LaunchSpec.SeedPrompt so it rides the spawn
	// argv as the trailing positional; "" → no trailing argument.
	FirstMessage string
}

// Start spawns a manual instance following the v0-pinned sequence with full
// rollback (package doc): the manual preflight (repo ready, provider, cap,
// forced auth refresh, label derivation) followed by the shared Launch core.
// It returns the created run on success, or a typed error the API maps to a
// status code: ErrRepoNotReady/ErrOverCap/ErrLoggedOut → 409,
// BadRequestError → 400, StartFailedError (git/spawn cause verbatim) → 500,
// store.ErrNotFound (unknown repo) → 404.
func (s *Service) Start(ctx context.Context, p StartParams) (store.Run, error) {
	repo, err := s.store.RepoByID(ctx, p.RepoID)
	if err != nil {
		return store.Run{}, err // ErrNotFound → 404
	}
	if repo.CloneStatus != store.CloneStatusReady {
		return store.Run{}, ErrRepoNotReady
	}
	// The effective provider (issue #66): explicit request (strict — unknown →
	// 400) → repo.provider → global provider_default → first registered, the
	// default layers skipping unregistered ids. Model/effort then resolve
	// against the RESOLVED provider's catalogs.
	prov, err := s.ResolveProvider(ctx, repo, store.RunKindManual, p.Provider)
	if err != nil {
		return store.Run{}, err // BadRequestError → 400 (or a store error)
	}

	model, effort, err := s.ResolveModelEffort(ctx, prov, repo, store.RunKindManual, p.Model, p.Effort)
	if err != nil {
		return store.Run{}, err // BadRequestError → 400 (or a store error)
	}
	// The remote-control knob (issue #163), resolved against the SAME effective
	// provider: request → repo.remote_default → spawn_remote_default → false, then
	// clamped to false for a provider that does not honor it. The resolved bool is
	// both spawned on (SpawnSpec.Remote) and STAMPED on the run row — the deep-link
	// capture gate reads it back, including after a restart.
	remote, err := s.ResolveRemote(ctx, prov, repo, store.RunKindManual, p.Remote)
	if err != nil {
		return store.Run{}, err
	}
	// Manual runs carry no provider options (the operator types keywords like
	// ultracode into the Start prompt themselves); ResolveSpawnOptions returns
	// an empty bag for a manual kind.
	options, err := s.ResolveSpawnOptions(ctx, prov, repo, store.RunKindManual)
	if err != nil {
		return store.Run{}, err
	}

	// One tmux listing serves both the cap check and the label-collision set
	// (v0: a direct POST must not exceed the cap even though the UI disables
	// the button).
	live, err := s.runner.List(ctx)
	if err != nil {
		return store.Run{}, err
	}
	if LiveInstanceCount(live) >= s.EffectiveCap(ctx, repo) {
		return store.Run{}, ErrOverCap
	}

	// FORCE the auth refresh — never the 30s render cache: a spawn while logged
	// out strands a doomed remote-control session (v0-pinned).
	if st, _ := prov.AuthStatus(ctx, true); !st.LoggedIn {
		return store.Run{}, ErrLoggedOut
	}

	taken := make(map[string]bool, len(live))
	for _, name := range live {
		taken[name] = true
	}
	label := gitx.UniqueManualLabel(repo.Name, gitx.SanitizeLabel(p.Label), s.now(), taken)

	return s.Launch(ctx, LaunchSpec{
		Repo:         repo,
		Provider:     prov,
		Kind:         store.RunKindManual,
		SessionName:  gitx.ComposeSessionName(repo.Name, label),
		Branch:       repo.ManualBranchPrefix + label,
		WorktreePath: s.worktreePath(repo.Name, label),
		Model:        model,
		Effort:       effort,
		Remote:       remote,
		Options:      options,
		// The operator's first chat message (issue #96), when given, rides the
		// same SeedPrompt → spawn-argv trailing-positional mechanism the AFK
		// seed prompt uses. "" (the common case) leaves the manual spawn with
		// no trailing argument, unchanged from before #96.
		SeedPrompt: p.FirstMessage,
	})
}

// EffectiveCap is the live-instance cap for a start on repo: the repo's
// max_instances_override when set, else the global settings max_instances.
// A missing/blank/garbled setting falls back to the config default (6).
// Exported for the M5 AFK engine, whose locked claim path re-checks the same
// cap on fresh liveness.
func (s *Service) EffectiveCap(ctx context.Context, repo store.Repo) int {
	if repo.MaxInstancesOverride != nil {
		return *repo.MaxInstancesOverride
	}
	n, err := s.store.GetInt(ctx, store.SettingMaxInstances, defaultMaxInstances)
	if err != nil {
		s.log.Warn("reading max_instances setting; using default", "component", "instance", "err", err)
		return defaultMaxInstances
	}
	return n
}

// spawnEnv assembles a session's extra environment (design §3a/§6/M3 contract):
// the repo credential's git env + LAB_URL + LAB_TOKEN + the git author/committer
// identity when configured. The forge token is never included (§3a).
func (s *Service) spawnEnv(ctx context.Context, repo store.Repo, credEnv []string, runToken string) ([]string, error) {
	env := append([]string{}, credEnv...)
	env = append(env, "LAB_URL="+s.labURL, "LAB_TOKEN="+runToken)
	author, err := s.authorEnv(ctx, repo)
	if err != nil {
		return nil, err
	}
	return append(env, author...), nil
}

// defaultMaxInstances mirrors config.DefaultMaxInstances (6) without a service
// → config import edge. It is only a last-resort fallback: production always
// seeds the max_instances setting (from --max-instances), so this is reached
// only if that row is missing or unparseable.
const defaultMaxInstances = 6

// seedOpts derives the provider's SeedOpts for a launch on repo: the incogni
// flag flows through so the provider seeds attribution-off settings into the
// worktree (D15 §9 measure 1).
func seedOpts(repo store.Repo) provider.SeedOpts {
	return provider.SeedOpts{Incogni: repo.Incogni}
}
