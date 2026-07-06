// Package reposvc owns the repository lifecycle for M2 (brief D8/D9; design
// §3a/§6/§7): repo creation with the pinned defaults (name derivation via the
// v0 scanner rules, incogni branch-pattern seeding, tracker-binding
// resolution, credential-kind enforcement), the async bare-clone job into
// <state>/repos/<id>.git with throttled progress events, settings-PATCH
// validation, guarded deletion, clone retry, and the startup healing of
// interrupted clones plus the runtime-dir credential sweep.
//
// It is the single writer of repos.clone_status and the single publisher of
// 'repo.changed' and 'clone.progress' events; the HTTP layer stays a thin
// translation of these methods onto the pinned M2 API contract.
package reposvc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// DefaultProvider is the provider every repo starts with (design §3a).
const DefaultProvider = "claude-code"

// Branch-pattern defaults (design §3a): incogni repos are seeded with
// neutral names at CREATE; a later incogni PATCH never rewrites patterns.
const (
	defaultAFKPattern   = "afk/<N>"
	defaultManualPrefix = "lab/"
	incogniAFKPattern   = "issue-<N>"
	incogniManualPrefix = "wip/"
)

// Event types this service publishes (brief §8.1). Payloads are the small
// envelopes the SSE contract pins; clients refetch on event.
const (
	EventRepoChanged   = "repo.changed"
	EventCloneProgress = "clone.progress"
)

type repoChangedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
}

type cloneProgressPayload struct {
	Type    string `json:"type"`
	RepoID  string `json:"repoID"`
	Phase   string `json:"phase"`
	Percent int    `json:"percent"`
	Line    string `json:"line"`
}

// ErrCloneInProgress refuses operations that conflict with a running clone
// job: delete without force, retry. The API answers 409.
var ErrCloneInProgress = errors.New("clone in progress")

// ErrCloneNotFailed refuses a clone retry unless the repo is in
// clone_status 'error' (pinned M2 contract). The API answers 409.
var ErrCloneNotFailed = errors.New("clone is not in error state")

// ErrHasLiveInstances refuses an unforced delete while the repo has live
// instances/worktrees (design §3a; M3 guard). The API answers 409; a forced
// delete tears the instances down first.
var ErrHasLiveInstances = errors.New("repository has live instances")

// BadRequestError marks invalid operator input; the API answers 400 with
// the message. Messages name fields and credential ids, never secrets.
type BadRequestError struct{ msg string }

func (e *BadRequestError) Error() string { return e.msg }

func badRequestf(format string, args ...any) *BadRequestError {
	return &BadRequestError{msg: fmt.Sprintf(format, args...)}
}

// Options configures a Service. Everything except Logger, GitEnv and Now is
// required.
type Options struct {
	Store        *store.Store
	Vault        *vault.Vault
	Materializer *vault.Materializer
	Git          *gitx.Engine
	Bus          *events.Bus
	Logger       *slog.Logger
	// ReposDir is <state>/repos — the parent of every bare clone (design
	// §7: repos/<repoID>.git). Created if absent.
	ReposDir string
	// GitEnv is appended to every git subprocess the service runs, before
	// the per-credential env. Production leaves it nil; tests pass
	// testutil.HermeticGitEnv so service-driven clones never read the
	// developer's git config.
	GitEnv []string
	// CredentialKeep is the keep predicate StartupHeal hands to the runtime-dir
	// credential sweep (design §6). nil → keep nothing (M2/tests: no live runs,
	// every materialized file is an orphan). In M3 cmd/lab passes a keep-all
	// predicate so a restart's live-session credential files survive here — the
	// authoritative keep-set sweep runs later in reconcile.StartupReconcile,
	// after re-adoption has computed which runs are still live.
	CredentialKeep func(filename string) bool
	// LiveInstances reports the number of live instances (worktrees) of a repo
	// — the M3 delete guard (design §3a: a repo with live worktrees is refused
	// deletion unless forced). nil → no guard (M2 behavior). Injected from
	// cmd/lab (the instance service) to avoid an import cycle.
	LiveInstances func(ctx context.Context, repoID string) (int, error)
	// StopInstances tears down every instance of a repo — run on a force delete
	// before the row/bare-dir removal so no session outlives its repo. nil → no
	// teardown (M2 behavior).
	StopInstances func(ctx context.Context, repoID string) (int, error)
	// Metrics receives the clone-job reports (lab_clone_jobs_total,
	// lab_clones_in_flight). Nil is a no-op (the report methods are
	// nil-safe).
	Metrics *metrics.Metrics
	// Now overrides the clock (tests); nil → time.Now.
	Now func() time.Time
}

// Service is the repository lifecycle owner. Construct with New.
type Service struct {
	store    *store.Store
	vault    *vault.Vault
	mat      *vault.Materializer
	git      *gitx.Engine
	bus      *events.Bus
	log      *slog.Logger
	reposDir string
	gitEnv   []string
	now      func() time.Time

	credentialKeep func(filename string) bool
	liveInstances  func(ctx context.Context, repoID string) (int, error)
	stopInstances  func(ctx context.Context, repoID string) (int, error)
	metrics        *metrics.Metrics // nil-safe report methods

	// mu guards jobs: the single-flight registry of running clone jobs,
	// keyed by repo id.
	mu   sync.Mutex
	jobs map[string]*cloneJob
}

// New validates o, ensures the repos dir exists, and returns a Service.
func New(o Options) (*Service, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("reposvc: Options.Store is required")
	case o.Vault == nil:
		return nil, errors.New("reposvc: Options.Vault is required")
	case o.Materializer == nil:
		return nil, errors.New("reposvc: Options.Materializer is required")
	case o.Git == nil:
		return nil, errors.New("reposvc: Options.Git is required")
	case o.Bus == nil:
		return nil, errors.New("reposvc: Options.Bus is required")
	case o.ReposDir == "":
		return nil, errors.New("reposvc: Options.ReposDir is required")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(o.ReposDir, 0o755); err != nil {
		return nil, fmt.Errorf("reposvc: repos dir: %w", err)
	}
	return &Service{
		store:          o.Store,
		vault:          o.Vault,
		mat:            o.Materializer,
		git:            o.Git,
		bus:            o.Bus,
		log:            logger,
		reposDir:       o.ReposDir,
		gitEnv:         o.GitEnv,
		now:            now,
		credentialKeep: o.CredentialKeep,
		liveInstances:  o.LiveInstances,
		stopInstances:  o.StopInstances,
		metrics:        o.Metrics,
		jobs:           make(map[string]*cloneJob),
	}, nil
}

// Close cancels every running clone job and waits for them to finish.
// Interrupted repos stay in clone_status 'cloning'; StartupHeal repairs
// them on the next start (design §3a) — exactly as if the process had been
// killed. For graceful shutdown and tests.
func (s *Service) Close() {
	s.mu.Lock()
	jobs := make([]*cloneJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
	for _, j := range jobs {
		<-j.done
	}
}

// bareDir is the repo's bare reference clone location (design §7).
func (s *Service) bareDir(repoID string) string {
	return filepath.Join(s.reposDir, repoID+".git")
}

func (s *Service) publishRepoChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventRepoChanged, Payload: repoChangedPayload{
		Type: EventRepoChanged, RepoID: repoID,
	}})
}

// AddParams is the repo-create input (pinned M2 contract POST /api/v1/repos).
type AddParams struct {
	RemoteURL         string
	Name              string  // optional; derived from RemoteURL when empty
	CredentialID      *string // GIT credential: ssh_key|https_token
	ForgeCredentialID *string // forge_token
	TrackerBinding    string  // ""|"auto"|"forge"|"builtin"
	Incogni           bool
}

// Add validates p, creates the repo row (defaults per the pinned contract;
// triage labels seeded by store.CreateRepo), publishes repo.changed, and
// starts the async clone job. Tracker binding "auto" resolves at CREATE
// time: forge-kind detection is host-based (v0 rule), so it needs only the
// URL, never the clone.
func (s *Service) Add(ctx context.Context, p AddParams) (store.Repo, error) {
	remote := strings.TrimSpace(p.RemoteURL)
	if remote == "" {
		return store.Repo{}, badRequestf("remote_url is required")
	}

	var name string
	if strings.TrimSpace(p.Name) != "" {
		name = gitx.SanitizeRepoName(p.Name)
	} else {
		name = gitx.NameFromURL(remote)
	}
	if name == "" {
		return store.Repo{}, badRequestf("cannot derive a repository name from remote_url; provide name")
	}

	if p.CredentialID != nil {
		if err := s.checkCredentialKind(ctx, "credential_id", *p.CredentialID, vault.IsGitKind, "ssh_key or https_token"); err != nil {
			return store.Repo{}, err
		}
	}
	if p.ForgeCredentialID != nil {
		if err := s.checkCredentialKind(ctx, "forge_credential_id", *p.ForgeCredentialID, vault.IsForgeKind, "forge_token"); err != nil {
			return store.Repo{}, err
		}
	}

	info := tracker.Detect(remote)
	var binding string
	switch p.TrackerBinding {
	case "", "auto":
		// Auto resolves to forge only when BOTH hold: a detected forge host
		// AND an attached forge credential (design §3a pins
		// forge_credential_id as required when tracker_binding='forge', so
		// auto must never produce that binding without one). Else builtin.
		if info.Kind != tracker.ForgeKindNone && p.ForgeCredentialID != nil {
			binding = store.TrackerBindingForge
		} else {
			binding = store.TrackerBindingBuiltin
		}
	case store.TrackerBindingForge:
		if info.Kind == tracker.ForgeKindNone {
			return store.Repo{}, badRequestf("tracker_binding: %q requires a detected forge host (forge_kind is none)", store.TrackerBindingForge)
		}
		if p.ForgeCredentialID == nil {
			return store.Repo{}, badRequestf("tracker_binding: %q requires a forge_token credential (set forge_credential_id or use %q)", store.TrackerBindingForge, store.TrackerBindingBuiltin)
		}
		binding = store.TrackerBindingForge
	case store.TrackerBindingBuiltin:
		binding = store.TrackerBindingBuiltin
	default:
		return store.Repo{}, badRequestf("tracker_binding: must be \"auto\", %q or %q", store.TrackerBindingForge, store.TrackerBindingBuiltin)
	}

	afkPattern, manualPrefix := defaultAFKPattern, defaultManualPrefix
	if p.Incogni {
		afkPattern, manualPrefix = incogniAFKPattern, incogniManualPrefix
	}

	repo := store.Repo{
		ID:                 ids.NewID("repo"),
		Name:               name,
		RemoteURL:          remote,
		CredentialID:       p.CredentialID,
		ForgeCredentialID:  p.ForgeCredentialID,
		TrackerBinding:     binding,
		ForgeKind:          string(info.Kind),
		DefaultBranch:      "main", // provisional; detection updates it at clone completion
		Provider:           DefaultProvider,
		Incogni:            p.Incogni,
		AFKBranchPattern:   afkPattern,
		ManualBranchPrefix: manualPrefix,
		CloneStatus:        store.CloneStatusCloning,
		CreatedAt:          s.now(),
	}
	created, err := s.store.CreateRepo(ctx, repo)
	if err != nil {
		return store.Repo{}, err // ErrNameTaken → 409 at the API layer
	}
	s.publishRepoChanged(created.ID)
	s.startCloneJob(created)
	return created, nil
}

// UpdateSettings validates and applies a repo-settings PATCH (pinned M2
// contract), publishes repo.changed, and returns the updated row. The
// caller assembles u from the request body; only fields with Set=true are
// touched. An incogni flip never rewrites branch patterns.
func (s *Service) UpdateSettings(ctx context.Context, id string, u store.RepoSettingsUpdate) (store.Repo, error) {
	current, err := s.store.RepoByID(ctx, id)
	if err != nil {
		return store.Repo{}, err
	}

	if u.Name.Set {
		name := gitx.SanitizeRepoName(u.Name.Value)
		if name == "" {
			return store.Repo{}, badRequestf("name: must not be empty after sanitization")
		}
		u.Name.Value = name
	}
	if u.CredentialID.Set && u.CredentialID.Value != nil {
		if err := s.checkCredentialKind(ctx, "credential_id", *u.CredentialID.Value, vault.IsGitKind, "ssh_key or https_token"); err != nil {
			return store.Repo{}, err
		}
	}
	if u.ForgeCredentialID.Set && u.ForgeCredentialID.Value != nil {
		if err := s.checkCredentialKind(ctx, "forge_credential_id", *u.ForgeCredentialID.Value, vault.IsForgeKind, "forge_token"); err != nil {
			return store.Repo{}, err
		}
	}
	if u.TrackerBinding.Set {
		switch u.TrackerBinding.Value {
		case store.TrackerBindingBuiltin:
		case store.TrackerBindingForge:
			if current.ForgeKind == string(tracker.ForgeKindNone) {
				return store.Repo{}, badRequestf("tracker_binding: %q requires a detected forge host (forge_kind is none)", store.TrackerBindingForge)
			}
		default:
			return store.Repo{}, badRequestf("tracker_binding: must be %q or %q", store.TrackerBindingForge, store.TrackerBindingBuiltin)
		}
	}
	// Cross-field invariant (design §3a): a forge-bound repo must hold a
	// forge credential. Like the branch-pattern pair below, validate the
	// (tracker_binding, forge_credential_id) combination that would result,
	// so neither flipping the binding to forge without a credential nor
	// clearing the credential under a forge binding can persist an invalid
	// row.
	if u.TrackerBinding.Set || u.ForgeCredentialID.Set {
		binding, forgeCred := current.TrackerBinding, current.ForgeCredentialID
		if u.TrackerBinding.Set {
			binding = u.TrackerBinding.Value
		}
		if u.ForgeCredentialID.Set {
			forgeCred = u.ForgeCredentialID.Value
		}
		if binding == store.TrackerBindingForge && forgeCred == nil {
			return store.Repo{}, badRequestf("tracker_binding: %q requires a forge_token credential (set forge_credential_id or use %q)", store.TrackerBindingForge, store.TrackerBindingBuiltin)
		}
	}
	if u.DefaultBranch.Set {
		branch := strings.TrimSpace(u.DefaultBranch.Value)
		if branch == "" {
			return store.Repo{}, badRequestf("default_branch: must not be empty")
		}
		if strings.HasPrefix(branch, "-") {
			return store.Repo{}, badRequestf("default_branch: must not start with '-'")
		}
		u.DefaultBranch.Value = branch
	}

	// Pattern grammar (design §4a): validate the pair that would result,
	// so changing one side cannot silently create an overlap with the other.
	if u.AFKBranchPattern.Set || u.ManualBranchPrefix.Set {
		pattern, prefix := current.AFKBranchPattern, current.ManualBranchPrefix
		if u.AFKBranchPattern.Set {
			pattern = u.AFKBranchPattern.Value
		}
		if u.ManualBranchPrefix.Set {
			prefix = u.ManualBranchPrefix.Value
		}
		if err := gitx.ValidatePatternPair(pattern, prefix); err != nil {
			return store.Repo{}, badRequestf("%s", err)
		}
	}

	if u.BudgetMinutes.Set && u.BudgetMinutes.Value != nil && *u.BudgetMinutes.Value < 1 {
		return store.Repo{}, badRequestf("budget_minutes: must be at least 1 (null clears the override)")
	}
	if u.MaxInstancesOverride.Set && u.MaxInstancesOverride.Value != nil && *u.MaxInstancesOverride.Value < 1 {
		return store.Repo{}, badRequestf("max_instances_override: must be at least 1 (null clears the override)")
	}

	// Incogni pre-push guard, toggle-on (D15 §9 measure 7): install BEFORE
	// the row update, so a failed install never leaves an incogni row
	// without its guard — the PATCH fails and the flag stays off. A repo
	// whose bare dir does not exist yet (clone in flight or failed) is
	// skipped: the clone-completion path re-reads the row and installs.
	// Idempotent over an already-guarded repo.
	bareExists := true
	if u.Incogni.Set && u.Incogni.Value {
		if _, err := os.Stat(s.bareDir(id)); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return store.Repo{}, fmt.Errorf("stat bare repo: %w", err)
			}
			bareExists = false // clone in flight/failed — clone completion installs
		}
		if bareExists {
			if err := s.installIncogniHook(ctx, id); err != nil {
				return store.Repo{}, fmt.Errorf("installing incogni pre-push hook: %w", err)
			}
		}
	}

	repo, err := s.store.UpdateRepoSettings(ctx, id, u)
	if err != nil {
		return store.Repo{}, err
	}

	// Close the toggle-on race: the row now says incogni; if the bare dir has
	// since appeared (clone finished between our stat and here, and its
	// completion re-read the not-yet-committed incogni=false row), install
	// now so whichever writer acted second still lands the guard.
	if u.Incogni.Set && u.Incogni.Value && !bareExists {
		if _, err := os.Stat(s.bareDir(id)); err == nil {
			if err := s.installIncogniHook(ctx, id); err != nil {
				s.log.Warn("installing incogni pre-push hook after clone race", "component", "reposvc", "repo", id, "err", err)
			}
		}
	}

	// Toggle-off: remove AFTER the row update — the guard is lab's, not the
	// user's policy, and it must be gone once incogni is off. Ordered this
	// way a removal failure leaves the repo over-protected, never leaky;
	// the leftover is logged, not fatal.
	if u.Incogni.Set && !u.Incogni.Value {
		if err := s.removeIncogniHook(ctx, id); err != nil {
			s.log.Warn("removing incogni pre-push hook", "component", "reposvc", "repo", id, "err", err)
		}
	}

	s.publishRepoChanged(id)
	return repo, nil
}

// Delete removes a repo: its row (runs/issues/labels/CRs cascade) and its
// bare clone. While the clone job is running it refuses with
// ErrCloneInProgress unless force is set, in which case the job is
// cancelled and awaited so no git process survives the directory removal.
func (s *Service) Delete(ctx context.Context, id string, force bool) error {
	if _, err := s.store.RepoByID(ctx, id); err != nil {
		return err
	}

	s.mu.Lock()
	job := s.jobs[id]
	s.mu.Unlock()
	if job != nil {
		if !force {
			return ErrCloneInProgress
		}
		// Point of no return: the clone job is being killed, so the
		// teardown below must complete even if the client disconnects —
		// runClone's cancelled path deliberately writes no status, so
		// aborting here would strand the repo in clone_status 'cloning'
		// with no job. Await the job unconditionally (it self-terminates;
		// Close waits the same way) and run the tail on a detached context.
		job.cancel()
		<-job.done
	}

	// M3 worktree guard (design §3a): refuse (409) while the repo has live
	// instances unless forced; a forced delete tears them down first so no
	// session outlives its repo. The seam is injected from cmd/lab (the
	// instance service) to avoid an import cycle.
	if s.liveInstances != nil {
		n, err := s.liveInstances(ctx, id)
		if err != nil {
			return fmt.Errorf("checking live instances: %w", err)
		}
		if n > 0 {
			if !force {
				return ErrHasLiveInstances
			}
			if s.stopInstances != nil {
				if _, err := s.stopInstances(context.WithoutCancel(ctx), id); err != nil {
					s.log.Warn("tearing down instances before force delete", "component", "reposvc", "repo", id, "err", err)
				}
			}
		}
	}

	if err := s.store.DeleteRepo(context.WithoutCancel(ctx), id); err != nil {
		return err
	}
	if err := os.RemoveAll(s.bareDir(id)); err != nil {
		s.log.Warn("removing bare repo dir", "component", "reposvc", "repo", id, "err", err)
	}
	s.publishRepoChanged(id)
	return nil
}

// Retry restarts the clone of a repo in clone_status 'error' (pinned M2
// contract: only from error). The stale bare dir, if any, is removed first —
// an interrupted or corrupt clone left behind garbage the fresh clone must
// not trip over.
func (s *Service) Retry(ctx context.Context, id string) error {
	// One critical section from the single-flight check through the job
	// registration: two concurrent Retries must never both pass the checks,
	// or the loser would RemoveAll the directory the winner's live clone is
	// writing into — or observe a just-failed job still in the registry and
	// strand the repo in 'cloning' with no job. runClone's completion path
	// writes the final status before taking s.mu to deregister, so the
	// status read below is always consistent with the registry. The store
	// I/O held under s.mu is bounded and M2-cheap; Delete/Close touch s.mu
	// only for map reads and never block on a running Retry's job.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, running := s.jobs[id]; running {
		return ErrCloneInProgress
	}

	repo, err := s.store.RepoByID(ctx, id)
	if err != nil {
		return err
	}
	if repo.CloneStatus != store.CloneStatusError {
		return ErrCloneNotFailed
	}

	if err := os.RemoveAll(s.bareDir(id)); err != nil {
		return fmt.Errorf("removing stale bare repo dir: %w", err)
	}
	if err := s.store.UpdateRepoCloneStatus(ctx, id, store.CloneStatusCloning, ""); err != nil {
		return err
	}
	repo.CloneStatus = store.CloneStatusCloning
	repo.CloneError = nil
	s.publishRepoChanged(id) // Bus.Publish never blocks (events package contract)
	s.startCloneJobLocked(repo)
	return nil
}

// StartupHeal runs before serving: repos stuck in clone_status 'cloning'
// are probed (design §3a: bare dir exists + `git rev-parse --git-dir`
// succeeds) and moved to ready or error, and the runtime dir is swept per
// the design §6 restart rule. In M2 no live runs exist yet, so the keep-set
// is empty: every materialized credential file is an orphan and is removed
// (known_hosts is structurally untouchable).
func (s *Service) StartupHeal(ctx context.Context) error {
	ready, failed, err := s.store.HealInterruptedClones(ctx, s.probeBareRepo)
	if err != nil {
		return err
	}
	for _, id := range ready {
		// A healed-to-ready repo still carries the provisional
		// default_branch from Add — the process died before clone
		// completion wrote the detected one. Re-derive it from the bare
		// repo's own HEAD symref (git pointed it at the remote's default
		// branch during the clone; answers offline). On failure the
		// provisional value stays — the repo is still usable.
		if branch, ok := s.git.HeadBranch(ctx, s.bareDir(id), s.gitEnv); ok {
			if _, err := s.store.UpdateRepoSettings(ctx, id, store.RepoSettingsUpdate{
				DefaultBranch: store.Set(branch),
			}); err != nil {
				s.log.Warn("recording healed default branch", "component", "reposvc", "repo", id, "err", err)
			}
		}
		s.log.Info("healed interrupted clone", "component", "reposvc", "repo", id, "clone_status", store.CloneStatusReady)
		s.publishRepoChanged(id)
	}
	for _, id := range failed {
		s.log.Info("healed interrupted clone", "component", "reposvc", "repo", id, "clone_status", store.CloneStatusError)
		s.publishRepoChanged(id)
	}

	// Reconcile the incogni guard against the incogni flag on EVERY ready
	// repo (D15 §9 measure 7): a crash between an incogni toggle and its hook
	// install/remove, or the healed-clone case above, leaves the guard out of
	// sync with the flag. Install-when-incogni / remove-when-not are both
	// idempotent and safe on foreign hooks, so this converges every restart.
	s.reconcileIncogniHooks(ctx)
	if err := s.mat.CleanupAll(s.credentialKeep); err != nil {
		s.log.Warn("sweeping runtime dir", "component", "reposvc", "err", err)
	}
	return nil
}

// installIncogniHook writes the pre-push guard AND pins the bare repo's local
// core.hooksPath to the absolute hooks dir, so a global/system core.hooksPath
// (husky &c.) cannot route agent pushes past the guard (D15 §9 measure 7).
func (s *Service) installIncogniHook(ctx context.Context, id string) error {
	bareDir := s.bareDir(id)
	if err := seeder.InstallPrePushHook(bareDir); err != nil {
		return err
	}
	return s.git.PinHooksPath(ctx, bareDir, s.gitEnv)
}

// removeIncogniHook removes lab's guard and unpins core.hooksPath, restoring
// git's default hook resolution for a now-non-incogni repo.
func (s *Service) removeIncogniHook(ctx context.Context, id string) error {
	bareDir := s.bareDir(id)
	if err := s.git.UnpinHooksPath(ctx, bareDir, s.gitEnv); err != nil {
		return err
	}
	return seeder.RemovePrePushHook(bareDir)
}

// reconcileIncogniHooks converges every ready repo's guard with its incogni
// flag (called at startup). Best-effort per repo — a failure is logged and
// the others still reconcile.
func (s *Service) reconcileIncogniHooks(ctx context.Context) {
	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("reconciling incogni hooks: list repos", "component", "reposvc", "err", err)
		return
	}
	for _, repo := range repos {
		if repo.CloneStatus != store.CloneStatusReady {
			continue
		}
		if repo.Incogni {
			if err := s.installIncogniHook(ctx, repo.ID); err != nil {
				s.log.Warn("reconciling incogni hook: install", "component", "reposvc", "repo", repo.ID, "err", err)
			}
		} else if err := s.removeIncogniHook(ctx, repo.ID); err != nil {
			s.log.Warn("reconciling incogni hook: remove", "component", "reposvc", "repo", repo.ID, "err", err)
		}
	}
}

// probeBareRepo implements the design §3a healing probe for one repo id.
// The probe is anchored (gitx.ProbeBareRepo runs `git --git-dir <dir>
// rev-parse --git-dir` with the engine's scrubbed env): a leftover empty
// dir must probe false even when the repos dir sits inside some git
// checkout or the daemon environment carries GIT_DIR — an unanchored
// rev-parse would resolve the ancestor (or $GIT_DIR) and heal a garbage
// dir to 'ready', which Retry (error-only) could never recover.
func (s *Service) probeBareRepo(id string) bool {
	dir := s.bareDir(id)
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.git.ProbeBareRepo(ctx, dir, s.gitEnv)
}

// checkCredentialKind enforces the design §3a credential/forge split: the
// referenced credential must exist and be of an accepted kind, else the
// request is a 400 (never a FK error later).
func (s *Service) checkCredentialKind(ctx context.Context, field, credID string, kindOK func(string) bool, want string) error {
	cred, err := s.store.CredentialByID(ctx, credID)
	if errors.Is(err, store.ErrNotFound) {
		return badRequestf("%s: credential %s not found", field, credID)
	}
	if err != nil {
		return err
	}
	if !kindOK(cred.Kind) {
		return badRequestf("%s: credential %s has kind %s, want %s", field, credID, cred.Kind, want)
	}
	return nil
}
