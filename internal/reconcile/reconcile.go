// Package reconcile is lab's janitorial and salvage layer for the
// worktree-per-instance model (design §3b; reconcile-parked + git-worktrees
// port specs). It re-derives instance state from the world at startup and on a
// throttled runtime sweep — the DB is never the only witness (D6): tmux answers
// "alive", git refs answer "what is claimed / what work exists".
//
// Three surfaces:
//   - StartupReconcile: run once, synchronously, before any scheduler — §3b
//     re-adoption of runs.outcome='active' against live tmux (adopt survivors,
//     mark the rest dead, re-arm deep-link capture), THEN Pass A orphan guarded
//     teardown + Pass B bare-merged-branch GC, THEN the runtime/ credential
//     keep-set cleanup.
//   - RuntimeSweep / SweepLoop: throttled merged-ONLY sweep, best-effort fetch
//     per repo first, owned/starting sessions never touched.
//   - Parked / Discard: the read-only view over guarded-rule-preserved work,
//     and the ONE unguarded destroyer (kill session, force-remove worktree,
//     force-delete branch) gated by a managed-branch check.
//
// Every guarded decision routes through gitx.TeardownGuarded (dirty always
// wins; conservative keep on any unreadable status/merged check). Discard is
// the sole documented bypass. Managed = matching the repo's configured afk
// pattern or manual prefix — nothing assumes the literal "afk/"/"lab/".
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// SSE events (brief §8.1). Same wire strings the instance service publishes;
// each service owns its own constant (no shared coupling).
const (
	EventRunChanged    = "run.changed"
	EventParkedChanged = "parked.changed"
)

// Sweep cadence (design §3a defaults: afk_tick_seconds=30, sweep_interval=10min).
// The reaper loop that carries the sweep arrives in M5; SweepLoop is a plain
// throttled ticker now, factored so M5 can absorb it. The first runtime sweep
// fires ~sweepThrottle after startup (startup reconcile just ran), not on the
// first tick.
const (
	sweepTick     = 30 * time.Second
	sweepThrottle = 10 * time.Minute
)

type repoScopedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
}

// Options configures a Service. Everything except Logger, GitEnv, ArmCapture,
// and Now is required.
type Options struct {
	Store        *store.Store
	Git          *gitx.Engine
	Runner       tmuxx.SessionRunner
	Guard        *startguard.Guard
	Materializer *vault.Materializer
	Bus          *events.Bus
	Logger       *slog.Logger

	// ReposDir is <state>/repos — the parent of every bare reference clone.
	ReposDir string

	// GitEnv is prepended to every git subprocess (hermetic env in tests, nil
	// in production). The sweep's best-effort fetch runs with only this base
	// env — private-remote fetches degrade to the last-known origin refs
	// (M3: per-repo credential materialization for the sweep fetch is a later
	// refinement; the merged-check stays conservative regardless).
	GitEnv []string

	// ArmCapture re-starts deep-link capture for a re-adopted live run whose
	// deep_link_url is NULL (§3b). Injected by cmd/lab (the instance service's
	// ArmCapture) so reconcile need not import the instance package. Nil → no
	// re-arm (tests without a provider).
	ArmCapture func(store.Run)

	// Now overrides the clock (tests); nil → time.Now.
	Now func() time.Time
}

// Service is the reconciliation + parked-view owner. Construct with New.
type Service struct {
	store  *store.Store
	git    *gitx.Engine
	runner tmuxx.SessionRunner
	guard  *startguard.Guard
	mat    *vault.Materializer
	bus    *events.Bus
	log    *slog.Logger

	reposDir   string
	gitEnv     []string
	armCapture func(store.Run)
	now        func() time.Time
}

// New validates o and returns a Service.
func New(o Options) (*Service, error) {
	switch {
	case o.Store == nil:
		return nil, fmt.Errorf("reconcile: Options.Store is required")
	case o.Git == nil:
		return nil, fmt.Errorf("reconcile: Options.Git is required")
	case o.Runner == nil:
		return nil, fmt.Errorf("reconcile: Options.Runner is required")
	case o.Guard == nil:
		return nil, fmt.Errorf("reconcile: Options.Guard is required")
	case o.Materializer == nil:
		return nil, fmt.Errorf("reconcile: Options.Materializer is required")
	case o.Bus == nil:
		return nil, fmt.Errorf("reconcile: Options.Bus is required")
	case o.ReposDir == "":
		return nil, fmt.Errorf("reconcile: Options.ReposDir is required")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:      o.Store,
		git:        o.Git,
		runner:     o.Runner,
		guard:      o.Guard,
		mat:        o.Materializer,
		bus:        o.Bus,
		log:        logger,
		reposDir:   o.ReposDir,
		gitEnv:     o.GitEnv,
		armCapture: o.ArmCapture,
		now:        now,
	}, nil
}

// bareDir is the repo's bare reference clone (design §7).
func (s *Service) bareDir(repoID string) string {
	return filepath.Join(s.reposDir, repoID+".git")
}

func (s *Service) publishRunChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventRunChanged, Payload: repoScopedPayload{Type: EventRunChanged, RepoID: repoID}})
}

func (s *Service) publishParkedChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventParkedChanged, Payload: repoScopedPayload{Type: EventParkedChanged, RepoID: repoID}})
}

// readyRepos lists the repos with a usable bare reference clone (clone_status
// 'ready') — the only ones reconcile/sweep can enumerate refs for. A repo mid
// clone or in error has no worktrees to reconcile; per-repo isolation means one
// such repo never aborts the whole pass.
func (s *Service) readyRepos(ctx context.Context) ([]store.Repo, error) {
	repos, err := s.store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	ready := repos[:0]
	for _, r := range repos {
		if r.CloneStatus == store.CloneStatusReady {
			ready = append(ready, r)
		}
	}
	return ready, nil
}

// liveSet returns the live session names as a set.
func liveSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}
