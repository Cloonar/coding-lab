// Package instance owns the manual instance lifecycle (design §1; brief M3):
// Start (the synchronous fail-loud spawn sequence with full rollback and the
// starting-set race guard), Stop (guarded teardown + terminal outcome + token
// and credential cleanup), and StopAll. It codes against the real core seams —
// gitx worktrees, the tmuxx SessionRunner, the provider registry, the vault
// materializer, and startguard — and is the single owner of runs-row creation
// and the run.changed SSE event on the manual path. AFK-labeled sessions are a
// seam delegated to the M5 AFK engine (Stop refuses them with a 501-mapped
// error in M3).
//
// The Start sequence is v0-pinned (sessions-spawn + git-worktrees port specs):
// cap check → FORCE auth refresh (never trust the 30s cache before a spawn) →
// label/branch/worktree derivation → startguard.Mark → gitx.AddWorktree
// (fail-loud fetch, no fallback base) → seed workspace → create runs row + mint
// run token → tmux Start → StampOpened → async deep-link capture → run.changed.
// Any failure after worktree creation rolls back to the exact pre-Start state
// (RemoveWorktree + force DeleteBranch + delete row/token + credential cleanup);
// a failure before worktree creation rolls back nothing.
package instance

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// Event types this service publishes (brief §8.1 SSE contract). Payloads are
// the small envelopes clients refetch on; run.changed and parked.changed carry
// the repo id.
const (
	EventRunChanged    = "run.changed"
	EventParkedChanged = "parked.changed"
)

type repoScopedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
}

// Options configures a Service. Everything except Logger, GitEnv, CaptureCtx,
// and Now is required.
type Options struct {
	Store        *store.Store
	Git          *gitx.Engine
	Runner       tmuxx.SessionRunner
	Providers    *provider.Registry
	Vault        *vault.Vault
	Materializer *vault.Materializer
	Guard        *startguard.Guard
	Bus          *events.Bus
	Logger       *slog.Logger

	// ReposDir is <state>/repos — the parent of every bare reference clone
	// (design §7: repos/<repoID>.git). WorktreeRoot is <state>/worktrees, the
	// parent of every instance worktree.
	ReposDir     string
	WorktreeRoot string

	// LabURL is the value handed to spawned sessions as LAB_URL (the external
	// base URL, or http://127.0.0.1:<port> when unset) so labctl can reach the
	// agent API.
	LabURL string

	// GitEnv is prepended to every git subprocess (before the per-credential
	// env). Production leaves it nil; tests pass testutil.HermeticGitEnv so
	// service-driven worktree ops never read the developer's git config.
	GitEnv []string

	// CaptureCtx bounds the background deep-link capture goroutines; nil →
	// context.Background(). cmd/lab passes a shutdown-linked context.
	CaptureCtx context.Context

	// Now overrides the clock (tests); nil → time.Now.
	Now func() time.Time
}

// Service is the manual instance lifecycle owner. Construct with New.
type Service struct {
	store     *store.Store
	git       *gitx.Engine
	runner    tmuxx.SessionRunner
	providers *provider.Registry
	vault     *vault.Vault
	mat       *vault.Materializer
	guard     *startguard.Guard
	bus       *events.Bus
	log       *slog.Logger

	reposDir     string
	worktreeRoot string
	labURL       string
	gitEnv       []string
	captureCtx   context.Context
	now          func() time.Time
}

// New validates o and returns a Service.
func New(o Options) (*Service, error) {
	switch {
	case o.Store == nil:
		return nil, fmt.Errorf("instance: Options.Store is required")
	case o.Git == nil:
		return nil, fmt.Errorf("instance: Options.Git is required")
	case o.Runner == nil:
		return nil, fmt.Errorf("instance: Options.Runner is required")
	case o.Providers == nil:
		return nil, fmt.Errorf("instance: Options.Providers is required")
	case o.Vault == nil:
		return nil, fmt.Errorf("instance: Options.Vault is required")
	case o.Materializer == nil:
		return nil, fmt.Errorf("instance: Options.Materializer is required")
	case o.Guard == nil:
		return nil, fmt.Errorf("instance: Options.Guard is required")
	case o.Bus == nil:
		return nil, fmt.Errorf("instance: Options.Bus is required")
	case o.ReposDir == "":
		return nil, fmt.Errorf("instance: Options.ReposDir is required")
	case o.WorktreeRoot == "":
		return nil, fmt.Errorf("instance: Options.WorktreeRoot is required")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	captureCtx := o.CaptureCtx
	if captureCtx == nil {
		captureCtx = context.Background()
	}
	return &Service{
		store:        o.Store,
		git:          o.Git,
		runner:       o.Runner,
		providers:    o.Providers,
		vault:        o.Vault,
		mat:          o.Materializer,
		guard:        o.Guard,
		bus:          o.Bus,
		log:          logger,
		reposDir:     o.ReposDir,
		worktreeRoot: o.WorktreeRoot,
		labURL:       o.LabURL,
		gitEnv:       o.GitEnv,
		captureCtx:   captureCtx,
		now:          now,
	}, nil
}

// bareDir is the repo's bare reference clone (design §7).
func (s *Service) bareDir(repoID string) string {
	return filepath.Join(s.reposDir, repoID+".git")
}

// worktreePath is the on-disk worktree for an instance labelled label of repo
// repoName: <worktrees>/<repoName>-<label> (dash-joined, never "~").
func (s *Service) worktreePath(repoName, label string) string {
	return filepath.Join(s.worktreeRoot, gitx.WorktreeDir(repoName, label))
}

func (s *Service) publishRunChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventRunChanged, Payload: repoScopedPayload{Type: EventRunChanged, RepoID: repoID}})
}

func (s *Service) publishParkedChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventParkedChanged, Payload: repoScopedPayload{Type: EventParkedChanged, RepoID: repoID}})
}

// liveInstanceCount counts live sessions against the instance cap, excluding
// the provider login session (design §4d — the one symbol every exclusion keys
// on).
func liveInstanceCount(live []string) int {
	n := 0
	for _, name := range live {
		if name != tmuxx.LoginSession {
			n++
		}
	}
	return n
}

// LiveInstances counts the live sessions belonging to a repo (excluding the
// provider login session) — the reposvc delete-guard seam: a repo with live
// worktrees/instances is refused deletion unless forced.
func (s *Service) LiveInstances(ctx context.Context, repoID string) (int, error) {
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return 0, err
	}
	live, err := s.runner.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, name := range live {
		if name != tmuxx.LoginSession && gitx.BelongsTo(name, repo.Name) {
			n++
		}
	}
	return n, nil
}

// runCapture polls for a run's deep link in the background and persists it on a
// real hit only (write-only-on-hit: the provider returns its generic fallback
// on a miss, which must never overwrite a stored real link). Idempotent per
// session inside the provider; the in-flight set drives the "connecting…"
// render state (Connecting).
func (s *Service) runCapture(run store.Run) {
	prov, ok := s.providers.Get(run.Provider)
	if !ok {
		return
	}
	url, err := prov.CaptureDeepLink(s.captureCtx, run.SessionName, run.WorktreePath)
	if err != nil || url == "" || url == claudecode.GenericDeepLink {
		return // miss (or an in-flight duplicate call) → keep the generic fallback
	}
	if err := s.store.UpdateRunDeepLink(s.captureCtx, run.ID, url); err != nil {
		s.log.Warn("persisting captured deep link", "component", "instance",
			"run", run.ID, "session", run.SessionName, "err", err)
		return
	}
	s.publishRunChanged(run.RepoID)
}

// ArmCapture (re-)starts deep-link capture for a re-adopted live run whose
// deep_link_url is still NULL — the reconcile re-adoption hook (design §3b).
// Runs is idempotent per session inside the provider.
func (s *Service) ArmCapture(run store.Run) {
	if run.DeepLinkURL != nil && *run.DeepLinkURL != "" {
		return
	}
	go s.runCapture(run)
}

// connecting reports the provider's "connecting…" state for a session, when
// the provider exposes it (design §4d ConnectingReporter).
func (s *Service) connecting(providerID, session string) bool {
	prov, ok := s.providers.Get(providerID)
	if !ok {
		return false
	}
	if cr, ok := prov.(provider.ConnectingReporter); ok {
		return cr.Connecting(session)
	}
	return false
}
