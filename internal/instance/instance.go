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
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
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

// repoScopedPayload deliberately mirrors its siblings in afk/reconcile/pull
// key-for-key (brief §8.1 — duplicated per package by design, never shared).
// RunID names the one run a run.changed concerns (issue #175), so the SPA can
// refetch that run alone instead of the repo's whole run list; it stays empty
// (omitted) on genuinely repo-scoped emits — stop-all — and on parked.changed,
// which is never run-scoped.
type repoScopedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
	RunID  string `json:"runID,omitempty"`
}

// WorkspaceSeeder is the lab-side worktree seeding seam (design §1
// internal/seeder; brief D13), run on every Launch after the provider's own
// SeedWorkspace: the embedded skills bundle, the generated context file, and
// their .git/info/exclude entries — all shaped by the launching provider's
// meta (issue #51 decision 8: the seeder is generic, the provider declares the
// skills dir / context-file name / exclude entries). Satisfied by
// *seeder.Seeder; tests substitute a failing stub to drive the rollback.
type WorkspaceSeeder interface {
	SeedWorkspace(worktree string, repo store.Repo, meta provider.SeedMeta, opts seeder.Opts) error
}

// Options configures a Service. Everything except Logger, GitEnv, CaptureCtx,
// Seeder, and Now is required.
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

	// LabURL is the value handed to spawned sessions as LAB_URL so labctl can
	// reach the agent API. It comes from labURL()'s precedence (issue #201):
	// the dedicated agent URL if set, else the agent unix socket
	// unix://<state-dir>/agent.sock.
	LabURL string

	// GitEnv is prepended to every git subprocess (before the per-credential
	// env). Production leaves it nil; tests pass testutil.HermeticGitEnv so
	// service-driven worktree ops never read the developer's git config.
	GitEnv []string

	// CaptureCtx bounds the background deep-link capture goroutines; nil →
	// context.Background(). cmd/lab passes a shutdown-linked context.
	CaptureCtx context.Context

	// Seeder overrides the lab-side workspace seeding (tests); nil → the real
	// internal/seeder, which needs no configuration.
	Seeder WorkspaceSeeder

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
	seeder    WorkspaceSeeder
	log       *slog.Logger

	reposDir     string
	worktreeRoot string
	labURL       string
	gitEnv       []string
	captureCtx   context.Context
	now          func() time.Time

	// afkStop is the M5 AFK engine's neutral-Stop delegation (design §4c),
	// wired once at startup via SetAFKStopper; nil refuses AFK stops.
	afkStop AFKStopper

	// chatState is the embedded-chat tailer's conversational-state source
	// (issue #7), wired once at startup via SetChatState; nil omits the state
	// field from the instance list.
	chatState ConversationStater
}

// ConversationStater is the chat tailer's derived-state seam (issue #7): the
// instance list annotates each live run with its conversational state
// (working|needs_input|question|idle). Satisfied by *chat.Service; nil-safe.
type ConversationStater interface {
	State(session string) (string, bool)
}

// SetChatState wires the conversational-state source (cmd/lab, once at
// startup). Idempotent-by-construction: called before the service serves.
func (s *Service) SetChatState(cs ConversationStater) { s.chatState = cs }

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
	seed := o.Seeder
	if seed == nil {
		seed = seeder.New()
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
		seeder:       seed,
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

// publishRunChanged emits run.changed. runID is the one run the event
// concerns (issue #175) — "" for a genuinely repo-scoped emit (stop-all),
// which omitempty serializes back to the plain 2-field envelope.
func (s *Service) publishRunChanged(repoID, runID string) {
	s.bus.Publish(events.Event{Type: EventRunChanged, Payload: repoScopedPayload{Type: EventRunChanged, RepoID: repoID, RunID: runID}})
}

func (s *Service) publishParkedChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventParkedChanged, Payload: repoScopedPayload{Type: EventParkedChanged, RepoID: repoID}})
}

// LiveInstanceCount counts live sessions against the instance cap, excluding
// every provider login session (design §4d — the one predicate every
// exclusion keys on). Exported for the M5 AFK engine's locked cap check — one
// counting rule, never two.
func LiveInstanceCount(live []string) int {
	n := 0
	for _, name := range live {
		if !tmuxx.IsLoginSession(name) {
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
		if !tmuxx.IsLoginSession(name) && gitx.BelongsTo(name, repo.Name) {
			n++
		}
	}
	return n, nil
}

// deepLinker resolves a run's provider to its optional DeepLinker capability
// (ADR-0017). (nil, false) when the provider is unregistered or has no web
// surface — no deep-link capture machinery ever arms for such a provider.
func (s *Service) deepLinker(providerID string) (provider.DeepLinker, bool) {
	prov, ok := s.providers.Get(providerID)
	if !ok {
		return nil, false
	}
	dl, ok := prov.(provider.DeepLinker)
	return dl, ok
}

// runCapture polls for a run's deep link in the background and persists it on a
// real hit only (write-only-on-hit: a miss returns "" — ADR-0017 — which must
// never overwrite a stored real link). Idempotent per session inside the
// provider; the in-flight set drives the "connecting…" render state
// (Connecting).
func (s *Service) runCapture(run store.Run, dl provider.DeepLinker) {
	url, err := dl.CaptureDeepLink(s.captureCtx, run.SessionName, run.WorktreePath)
	if err != nil || url == "" {
		return // miss (or an in-flight duplicate call) → nothing to persist
	}
	if err := s.store.UpdateRunDeepLink(s.captureCtx, run.ID, url); err != nil {
		s.log.Warn("persisting captured deep link", "component", "instance",
			"run", run.ID, "session", run.SessionName, "err", err)
		return
	}
	s.publishRunChanged(run.RepoID, run.ID)
}

// ArmCapture (re-)starts deep-link capture for a live run whose deep_link_url
// is still NULL — used at Start and by the reconcile re-adoption hook (design
// §3b). No goroutine ever arms when the run is not remote (issue #163) or when
// the run's provider does not implement the optional DeepLinker capability
// (ADR-0017); capture is idempotent per session inside providers that do.
//
// This is the ONE choke point where the remote gate belongs: both arming call
// sites — Launch at spawn time and reconcile's readopt after a restart — pass
// through here, and the run row is the only thing that still knows a session's
// remote-ness once the process that spawned it is gone. Hence runs.remote is a
// persisted NOT NULL column, not a launch-time-only decision.
func (s *Service) ArmCapture(run store.Run) {
	if !run.Remote {
		// A non-remote session registers nothing to capture; arming here would log
		// ADR-0017's loud capture-miss on every run — the miss that decision
		// deliberately made loud precisely so a genuinely missing link is never
		// silent. Gating here keeps that alarm meaningful.
		return
	}
	if run.DeepLinkURL != nil && *run.DeepLinkURL != "" {
		return
	}
	dl, ok := s.deepLinker(run.Provider)
	if !ok {
		return
	}
	go s.runCapture(run, dl)
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
