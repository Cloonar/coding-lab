// Package afk is lab's unattended-run engine (brief D12, M5; afk-engine
// port-spec): it selects the lowest-numbered claimable ready-for-agent
// issue, claims it by creating the isolated worktree on the repo's rendered
// claim branch (the branch existing IS the claim — ADR-0013, no tracker
// label ever), launches a seeded remote-control session, reaps runs on the
// afk_tick_seconds cadence (done-signal = an open/merged PR/CR whose head is
// the run's branch; death and timeout are failures feeding the three-strikes
// pause), and schedules automatic runs per repo on the afk_schedule_seconds
// cadence — one auto run per repo, manual runs additive, all under the
// live-instance cap.
//
// The port keeps v0's two-lock shape: mu single-flights the ENTIRE
// select→claim→spawn (one claim path, shared by the manual start and the
// scheduler — two concurrent launches can never claim the same issue), and
// runsMu serializes the neutral Stop against the reap decision (design §4c:
// Stop writes outcome 'stopped' and deletes the run's tokens BEFORE killing
// the session, all under runsMu; the reaper re-reads the run row and fresh
// liveness under the same lock before claiming a terminal outcome, so a
// stopped run is never reclassified and a half-done Stop is never observed).
//
// D12 deltas from v0: every run has a runs row (a), the budget clock is the
// persisted runs.budget_deadline — restart re-adoption keeps it (b),
// caps/budgets/intervals come from settings with per-repo overrides,
// re-read every tick (c), and model/effort resolve per repo through the
// shared instance rule (d). The launch sequence itself is the shared
// instance.Launch core — one spawn/rollback implementation, never two.
package afk

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// SSE event types this engine publishes (brief §8.1). Each service owns its
// own constants (no shared coupling).
const (
	EventRunChanged    = "run.changed"
	EventRepoChanged   = "repo.changed"
	EventParkedChanged = "parked.changed"
)

// runTokenSlack is the §3a run-token expiry margin past the budget deadline:
// an AFK run's token dies 30 minutes after its budget would have timed the
// run out.
const runTokenSlack = 30 * time.Minute

// Interval/budget fallbacks — reached only when a settings row is missing or
// garbled (production seeds them all at first start; design §3a defaults).
const (
	defaultBudgetMinutes   = 120
	defaultTickSeconds     = 30
	defaultScheduleSeconds = 45
	defaultSweepMinutes    = 10
)

// TrackerResolver resolves a repo-scoped Tracker — the seam the engine goes
// through so tests can inject fakes. *tracker.Registry satisfies it.
type TrackerResolver interface {
	TrackerFor(ctx context.Context, repo store.Repo) (tracker.Tracker, error)
}

// Options configures a Service. Everything except Logger, GitEnv, Sweep, and
// Now is required.
type Options struct {
	Store        *store.Store
	Git          *gitx.Engine
	Runner       tmuxx.SessionRunner
	Providers    *provider.Registry
	Trackers     TrackerResolver
	Instances    *instance.Service
	Materializer *vault.Materializer
	Bus          *events.Bus
	Logger       *slog.Logger

	// Guard is the shared starting-set race guard (design §4b) — the SAME
	// *startguard.Guard instance/ and reconcile/ are built with. The reaper
	// consults it to skip a mid-Launch run whose row is active but whose tmux
	// session is not yet live (instance.Launch marks BEFORE the runs row and
	// clears only after the session is live or a rollback completes), so a
	// not-yet-spawned session is never misclassified as a death.
	Guard *startguard.Guard

	// ReposDir is <state>/repos (bare reference clones); WorktreeRoot is
	// <state>/worktrees.
	ReposDir     string
	WorktreeRoot string

	// GitEnv is prepended to every git subprocess (hermetic env in tests, nil
	// in production).
	GitEnv []string

	// Sweep is the throttled runtime worktree/branch GC the reaper loop
	// carries (reconcile.RuntimeSweep) — v0 piggybacked it on the watcher so
	// lab keeps one janitorial goroutine. Nil → no sweep (tests).
	Sweep func(context.Context)

	// Metrics receives the terminal-outcome reports (lab_afk_runs_total,
	// lab_afk_run_duration_seconds) at the reaper/stop chokepoints. Nil is a
	// no-op (the report methods are nil-safe).
	Metrics *metrics.Metrics

	// Now overrides the clock (tests); nil → time.Now.
	Now func() time.Time
}

// Service is the AFK engine. Construct with New.
type Service struct {
	store     *store.Store
	git       *gitx.Engine
	runner    tmuxx.SessionRunner
	providers *provider.Registry
	trackers  TrackerResolver
	instances *instance.Service
	mat       *vault.Materializer
	bus       *events.Bus
	guard     *startguard.Guard
	log       *slog.Logger

	reposDir     string
	worktreeRoot string
	gitEnv       []string
	sweep        func(context.Context)
	metrics      *metrics.Metrics // nil-safe report methods
	now          func() time.Time

	// mu single-flights the entire select→claim→spawn in launch — the ONE
	// claim path (manual starts, scheduler ticks, toggle-on kicks); v0's
	// afkMu.
	mu sync.Mutex

	// runsMu serializes the neutral Stop against the reap decision (§4c);
	// v0's afkRunsMu. Stop moves the run to outcome 'stopped' and kills the
	// session under it; the reaper re-reads the row and fresh liveness under
	// it before claiming a terminal outcome.
	runsMu sync.Mutex
}

// New validates o and returns a Service.
func New(o Options) (*Service, error) {
	switch {
	case o.Store == nil:
		return nil, fmt.Errorf("afk: Options.Store is required")
	case o.Git == nil:
		return nil, fmt.Errorf("afk: Options.Git is required")
	case o.Runner == nil:
		return nil, fmt.Errorf("afk: Options.Runner is required")
	case o.Providers == nil:
		return nil, fmt.Errorf("afk: Options.Providers is required")
	case o.Trackers == nil:
		return nil, fmt.Errorf("afk: Options.Trackers is required")
	case o.Instances == nil:
		return nil, fmt.Errorf("afk: Options.Instances is required")
	case o.Materializer == nil:
		return nil, fmt.Errorf("afk: Options.Materializer is required")
	case o.Bus == nil:
		return nil, fmt.Errorf("afk: Options.Bus is required")
	case o.Guard == nil:
		return nil, fmt.Errorf("afk: Options.Guard is required")
	case o.ReposDir == "":
		return nil, fmt.Errorf("afk: Options.ReposDir is required")
	case o.WorktreeRoot == "":
		return nil, fmt.Errorf("afk: Options.WorktreeRoot is required")
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
		store:        o.Store,
		git:          o.Git,
		runner:       o.Runner,
		providers:    o.Providers,
		trackers:     o.Trackers,
		instances:    o.Instances,
		mat:          o.Materializer,
		bus:          o.Bus,
		guard:        o.Guard,
		log:          logger,
		reposDir:     o.ReposDir,
		worktreeRoot: o.WorktreeRoot,
		gitEnv:       o.GitEnv,
		sweep:        o.Sweep,
		metrics:      o.Metrics,
		now:          now,
	}, nil
}

// bareDir is the repo's bare reference clone (design §7).
func (s *Service) bareDir(repoID string) string {
	return filepath.Join(s.reposDir, repoID+".git")
}

// worktreePath is the on-disk worktree of an AFK run for issue n:
// <worktrees>/<repoName>-<N> (the issue number for BOTH afk kinds; dash-
// joined, never "~" — the Windows-8.3 stall, design §7).
func (s *Service) worktreePath(repoName string, n int) string {
	return filepath.Join(s.worktreeRoot, gitx.WorktreeDir(repoName, fmt.Sprintf("%d", n)))
}

type repoScopedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
}

func (s *Service) publish(eventType, repoID string) {
	s.bus.Publish(events.Event{Type: eventType, Payload: repoScopedPayload{Type: eventType, RepoID: repoID}})
}

// settingSeconds/settingMinutes read a runtime-mutable interval each tick
// (D12c: loops re-read settings — no restart needed). A missing, blank,
// garbled, or non-positive value falls back to def.
func (s *Service) settingSeconds(ctx context.Context, key string, def int) time.Duration {
	return s.settingDuration(ctx, key, def, time.Second)
}

func (s *Service) settingMinutes(ctx context.Context, key string, def int) time.Duration {
	return s.settingDuration(ctx, key, def, time.Minute)
}

func (s *Service) settingDuration(ctx context.Context, key string, def int, unit time.Duration) time.Duration {
	n, err := s.store.GetInt(ctx, key, def)
	if err != nil {
		s.log.Warn("reading interval setting; using default", "component", "afk", "setting", key, "err", err)
		n = def
	}
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * unit
}

// effectiveBudget is a repo's AFK run budget: repos.budget_minutes when set,
// else the afk_budget_minutes setting (default 120 — D12c).
func (s *Service) effectiveBudget(ctx context.Context, repo store.Repo) time.Duration {
	if repo.BudgetMinutes != nil && *repo.BudgetMinutes > 0 {
		return time.Duration(*repo.BudgetMinutes) * time.Minute
	}
	return s.settingMinutes(ctx, store.SettingAFKBudgetMinutes, defaultBudgetMinutes)
}

// cleanupCredential removes a run's materialized credential files at a
// terminal outcome (opID = run id). A repo without a git credential is a
// no-op.
func (s *Service) cleanupCredential(repo store.Repo, runID string) {
	if repo.CredentialID == nil {
		return
	}
	if err := s.mat.Cleanup(*repo.CredentialID, runID); err != nil {
		s.log.Warn("cleaning materialized credential at reap", "component", "afk", "run", runID, "err", err)
	}
}
