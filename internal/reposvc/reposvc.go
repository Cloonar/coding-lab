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
	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// Branch-pattern defaults (design §3a): incogni repos are seeded with
// neutral names at CREATE; a later incogni PATCH never rewrites patterns.
const (
	defaultAFKPattern   = "afk/<N>"
	defaultManualPrefix = "lab/"
	incogniAFKPattern   = "issue-<N>"
	incogniManualPrefix = "wip/"
)

// Autoland defaults (issue #181 / ADR-0048): default OFF, a 2-attempt
// fix-run bound, and auto-merge on for when a repo does opt in.
const (
	defaultMaxFixAttempts = 2
	defaultAutoMerge      = true
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

// OneCLIAgents is the OneCLI REST seam the repo lifecycle keeps a repo's
// credential-gateway identity in step through (issue #35 / ADR-0067): the agent
// a repo's grants hang off is created with the repo, converged at startup, and
// deleted with it. Satisfied by *onecli.Client; nil = the integration is
// unconfigured, which is the normal state of a lab and must leave every path
// here indistinguishable from a lab built before this existed — no behavior
// change and, just as importantly, no log line (oneCLIActive is the one gate).
//
// The agent is addressed by the IDENTIFIER derived from the repo's STORE ID
// (onecli.AgentIdentifier) — unique and immutable upstream — while the repo's
// NAME rides along as a display string lab owns and overwrites. Which of the
// two is load-bearing is the whole of issue #35: matching on the name would
// hand a renamed repo a second, grant-less agent, with "my secrets vanished"
// as the only symptom. Every call site here derives the identifier rather than
// passing a repo id through, since EnsureAgent refuses anything that is not
// already a well-formed slug.
//
// Narrow on purpose, like instance.GatewayAPI: two methods is everything the
// three lifecycle hooks need, so a test drives all of them from a struct
// literal with no HTTP, and the grant pool/attach half of internal/onecli (the
// #25 picker's surface) cannot be reached from a repo create or delete even by
// accident.
//
// The Agent EnsureAgent answers with carries the repo's live gateway access
// token. reposvc has no use for it and drops it at every call site (assigned to
// _): never keep it, and never log an onecli.Agent — no %v, no %+v, not folded
// into an error.
type OneCLIAgents interface {
	EnsureAgent(ctx context.Context, identifier, displayName string) (onecli.Agent, error)
	DeleteAgent(ctx context.Context, identifier string) (bool, error)
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
	// Providers backs provider-id validation (Get, issue #66) and the incogni
	// pre-push hook's scrub + seeded-path patterns, which are the UNION of
	// every registered provider's declared SeedMeta rather than one repo's
	// resolved provider (ADR-0033: a per-session provider override, ADR-0030,
	// can run any registered provider on any repo, so the guard can no longer
	// be keyed to a single provider). Nil is the degraded boot with no
	// provider configured (cmd/lab: "instance features disabled") — an
	// incogni hook then guards nothing on content, which is inert because no
	// agent runs in that mode.
	Providers *provider.Registry
	// PinImageRef resolves and digest-pins a dev image ref on save (issue #207;
	// ADR-0053 — pin-on-save is the contract: a tag is a moving reference, so the
	// digest recorded on save is exactly what the spawn-time pull runs, and a
	// same-tag re-push between save and spawn changes nothing; the store persists
	// this returned pinned string verbatim). Production injects imageref's
	// Resolver.Pin. Nil is the no-pinner degraded state — an image_ref update then
	// FAILS rather than persisting an unpinned ref: storing what spawn cannot
	// trust is never the safe fallback.
	PinImageRef func(ctx context.Context, ref string) (string, error)
	// OneCLI is the credential-gateway agent seam (issue #35): repo create,
	// startup heal and repo delete become the touchpoints that keep a repo's
	// OneCLI identity in step with its row. Production passes *onecli.Client.
	// nil — the lab with no OneCLI configured, which is most of them — makes all
	// three SILENT no-ops: not a degraded mode to warn about, just a feature
	// nobody turned on. Injected from cmd/lab as the interface with an explicit
	// nil-pointer guard; a nil *onecli.Client assigned straight in would be a
	// non-nil interface and this gate would read backwards.
	OneCLI OneCLIAgents
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
	pinImageRef    func(ctx context.Context, ref string) (string, error) // nil = no-pinner degraded state (image_ref updates fail)
	metrics        *metrics.Metrics                                      // nil-safe report methods
	providers      *provider.Registry                                    // nil in the no-provider degraded boot
	oneCLI         OneCLIAgents                                          // nil = OneCLI unconfigured (oneCLIActive)

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
		pinImageRef:    o.PinImageRef,
		metrics:        o.Metrics,
		providers:      o.Providers,
		oneCLI:         o.OneCLI,
		jobs:           make(map[string]*cloneJob),
	}, nil
}

// oneCLIActive reports whether this lab has the OneCLI integration configured.
// One predicate for all three lifecycle hooks (Add, StartupHeal, Delete), in
// the spirit of instance.gatewayActive(): "off" is the normal state of a lab
// and the hooks must be invisible in it, so the check belongs in one named
// place rather than as a nil test per call site that can drift.
//
// Unlike instance's gate there is no second half to check: the repo lifecycle
// talks only to the REST API, never to the gateway PROXY address a run dials
// (ADR-0067 keeps the two independently settable), so a lab configured with
// the REST pair alone — issue #23's health-only deployment — still keeps its
// agents in step even though no spawn there wires a gateway.
func (s *Service) oneCLIActive() bool { return s.oneCLI != nil }

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
	// Provider is the repo's optional agent-CLI override (issue #66). Nil or
	// empty = inherit (NULL — the global provider_default, then the first
	// registered provider, resolve at spawn). A non-empty value must name a
	// registered provider (400).
	Provider *string
	Incogni  bool
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
		// A forge credential alone suffices (ADR-0015): the credential's flavor
		// routes the tracker, so a detected forge host is no longer required —
		// this unlocks arbitrary Forgejo instances (codeberg, a second private
		// instance) and GitHub Enterprise hosts (forge_kind 'none') the operator
		// binds explicitly. The cross-field invariant — a forge binding needs a
		// forge credential — stays.
		if p.ForgeCredentialID == nil {
			return store.Repo{}, badRequestf("tracker_binding: %q requires a forge_token credential (set forge_credential_id or use %q)", store.TrackerBindingForge, store.TrackerBindingBuiltin)
		}
		binding = store.TrackerBindingForge
	case store.TrackerBindingBuiltin:
		binding = store.TrackerBindingBuiltin
	default:
		return store.Repo{}, badRequestf("tracker_binding: must be \"auto\", %q or %q", store.TrackerBindingForge, store.TrackerBindingBuiltin)
	}

	// Provider is stored only when the operator explicitly chose one (issue
	// #66): NULL = inherit the global default chain at spawn — no code-stamped
	// value that would read as an operator decision later.
	prov, err := s.validateProvider("provider", p.Provider)
	if err != nil {
		return store.Repo{}, err
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
		Provider:           prov,
		Incogni:            p.Incogni,
		AFKBranchPattern:   afkPattern,
		ManualBranchPrefix: manualPrefix,
		CloneStatus:        store.CloneStatusCloning,
		CreatedAt:          s.now(),
		MaxFixAttempts:     defaultMaxFixAttempts,
		AutoMerge:          defaultAutoMerge,
		// Runner (issue #205) is NOT NULL: stamp the host default explicitly
		// so CreateRepo never inserts the Go zero string "" into it.
		Runner: store.RunnerHost,
	}
	created, err := s.store.CreateRepo(ctx, repo)
	if err != nil {
		return store.Repo{}, err // ErrNameTaken → 409 at the API layer
	}
	s.publishRepoChanged(created.ID)
	s.startCloneJob(created)

	// Eager agent creation (issue #35): a repo's OneCLI identity now exists from
	// the moment the repo does, which is what lets an operator pre-provision the
	// repo's secrets from the OneCLI dashboard — or through the grant picker —
	// before it has ever spawned. Without it the first spawn doubles as the
	// "make my repo appear over there" step, and an operator preparing a repo
	// finds nothing to attach grants to.
	//
	// Best-effort, and the asymmetry is deliberate: a sidecar that is down must
	// never block repo creation, because the SPAWN is the fail-closed
	// enforcement point (ADR-0067 — a run that starts without credential
	// injection is the failure worth refusing, not a repo row). The lazy ensures
	// at spawn and at grant-attach stay as backstops — they still have to, for
	// every repo created before OneCLI was configured — so all a warn here costs
	// is one later round-trip.
	//
	// It runs AFTER startCloneJob because that call only registers the job and
	// returns, while this one is a synchronous round-trip bounded by
	// internal/onecli's 30s per-request timeout: ensuring first would hold the
	// clone at the starting line for the whole of a wedged sidecar's timeout,
	// and the clone needs nothing from the agent.
	if s.oneCLIActive() {
		// The returned Agent's token is a live gateway credential; dropped here
		// and never logged (see OneCLIAgents).
		if _, err := s.oneCLI.EnsureAgent(ctx, onecli.AgentIdentifier(created.ID), created.Name); err != nil {
			s.log.Warn("ensuring onecli agent for new repo", "component", "reposvc", "repo", created.ID, "err", err)
		}
	}
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
	if u.Provider.Set {
		v, err := s.validateProvider("provider", u.Provider.Value)
		if err != nil {
			return store.Repo{}, err
		}
		u.Provider.Value = v
	}
	if u.AFKProviderDefault.Set {
		v, err := s.validateProvider("afk_provider_default", u.AFKProviderDefault.Value)
		if err != nil {
			return store.Repo{}, err
		}
		u.AFKProviderDefault.Value = v
	}
	if u.LanderProvider.Set {
		v, err := s.validateProvider("lander_provider", u.LanderProvider.Value)
		if err != nil {
			return store.Repo{}, err
		}
		u.LanderProvider.Value = v
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
			// A forge credential alone suffices (ADR-0015): the detected-host
			// requirement is dropped. The cross-field invariant below still
			// requires the forge credential.
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

	// Runner (issue #205): a plain two-value enum, exactly like tracker_binding
	// above — no inherit state, since repos.runner is NOT NULL. An empty string
	// is not valid either: a PATCH must send a concrete "host" or "container".
	if u.Runner.Set {
		switch u.Runner.Value {
		case store.RunnerHost, store.RunnerContainer:
		default:
			return store.Repo{}, badRequestf("runner: must be %q or %q", store.RunnerHost, store.RunnerContainer)
		}
	}
	// container_memory (issue #205): nullable — null clears the per-repo
	// override back to inherit the global container_memory setting — but a
	// non-null value must match podman's --memory grammar (store.ValidContainerMemory,
	// shared with the global setting's own validation in httpapi/settings.go),
	// e.g. "8g".
	if u.ContainerMemory.Set && u.ContainerMemory.Value != nil && !store.ValidContainerMemory(*u.ContainerMemory.Value) {
		return store.Repo{}, badRequestf("container_memory: must look like a podman --memory value, e.g. %q", "8g")
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
	// container_pids/container_nofile (issue #205): nullable per-repo overrides
	// of the podman --pids-limit / --ulimit nofile floors; null clears back to
	// the global default, same "null clears the override" wording as the pair
	// above.
	if u.ContainerPids.Set && u.ContainerPids.Value != nil && *u.ContainerPids.Value < 1 {
		return store.Repo{}, badRequestf("container_pids: must be at least 1 (null clears the override)")
	}
	if u.ContainerNofile.Set && u.ContainerNofile.Value != nil && *u.ContainerNofile.Value < 1 {
		return store.Repo{}, badRequestf("container_nofile: must be at least 1 (null clears the override)")
	}
	if u.MaxFixAttempts.Set && u.MaxFixAttempts.Value < 0 {
		return store.Repo{}, badRequestf("max_fix_attempts: must be at least 0")
	}
	// Autoland (issue #181 / ADR-0048) is forge-only: the engine polls PR
	// comments for lander verdicts, and the builtin tracker binding has no
	// comment listing to poll. Validate the (tracker_binding, autoland_enabled)
	// pair that would result, the same way the forge-credential invariant above
	// does, so neither turning autoland on under a builtin binding nor flipping
	// an autoland-enabled repo to builtin can persist. Turning autoland OFF is
	// always allowed, on any binding.
	if u.AutolandEnabled.Set || u.TrackerBinding.Set {
		binding, enabled := current.TrackerBinding, current.AutolandEnabled
		if u.TrackerBinding.Set {
			binding = u.TrackerBinding.Value
		}
		if u.AutolandEnabled.Set {
			enabled = u.AutolandEnabled.Value
		}
		if enabled && binding != store.TrackerBindingForge {
			return store.Repo{}, badRequestf("autoland_enabled: requires a forge tracker binding")
		}
	}

	// image_ref (issue #207) is validated LAST because it is the only field that
	// touches the network: every cheap enum/grammar check above gates the
	// registry round-trip, so a request invalid for another reason never pays for
	// a pin. Set-to-nil clears the per-repo override back to inherit the global
	// default dev image, with no pinner call. A non-nil value is TrimSpace'd, and
	// blank-after-trim is treated as a clear too (defensive — httpapi's
	// patchNullableString already folds blank → nil). Otherwise the ref is pinned
	// on save (ADR-0053 — a tag moves, the digest does not), so the column only
	// ever holds a canonical host/path:tag@sha256:… form: a nil pinner is the
	// degraded no-pinner boot and fails plainly (a config gap — 500-family, not
	// the operator's fault, and never a silently-stored unpinned ref); a pinner
	// error IS the operator's, and imageref's messages already name the ref and
	// the offending part, so it surfaces verbatim as a 400 with no double-wrap. On
	// success the RETURNED pinned string is stored, never the input tag.
	if u.ImageRef.Set && u.ImageRef.Value != nil {
		trimmed := strings.TrimSpace(*u.ImageRef.Value)
		switch {
		case trimmed == "":
			u.ImageRef.Value = nil
		case s.pinImageRef == nil:
			return store.Repo{}, errors.New("image ref pinning unavailable")
		default:
			pinned, err := s.pinImageRef(ctx, trimmed)
			if err != nil {
				return store.Repo{}, badRequestf("%s", err)
			}
			u.ImageRef.Value = &pinned
		}
	}

	// Incogni toggle-on (D15 §9 measure 7): re-render the guard WITH the
	// incogni patterns BEFORE the row update, so a failed install never
	// leaves an incogni row without its patterned guard — the PATCH fails
	// and the flag stays off. A repo whose bare dir does not exist yet
	// (clone in flight or failed) is skipped: the clone-completion path
	// re-reads the row and renders the matching hook. Idempotent over an
	// already-guarded repo.
	bareExists := true
	if u.Incogni.Set && u.Incogni.Value {
		if _, err := os.Stat(s.bareDir(id)); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return store.Repo{}, fmt.Errorf("stat bare repo: %w", err)
			}
			bareExists = false // clone in flight/failed — clone completion installs
		}
		if bareExists {
			if err := s.installGuardHook(ctx, id, true); err != nil {
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
			if err := s.installGuardHook(ctx, id, true); err != nil {
				s.log.Warn("installing incogni pre-push hook after clone race", "component", "reposvc", "repo", id, "err", err)
			}
		}
	}

	// Toggle-off: RE-RENDER the hook WITHOUT the incogni patterns AFTER the
	// row update — the guard itself stays on every repo (issue #106: the
	// secret leak scan is per-repo, orthogonal to incogni), only its content
	// changes, and core.hooksPath stays pinned. Ordered this way a re-render
	// failure leaves the repo over-protected, never leaky: the stale incogni
	// patterns linger until the next startup reconcile; the failure is
	// logged, not fatal. A missing bare dir (clone in flight/failed) is
	// skipped silently — clone completion installs the matching hook.
	if u.Incogni.Set && !u.Incogni.Value {
		if _, err := os.Stat(s.bareDir(id)); err == nil {
			if err := s.installGuardHook(ctx, id, false); err != nil {
				s.log.Warn("re-rendering pre-push guard after incogni toggle-off", "component", "reposvc", "repo", id, "err", err)
			}
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

	// The repo's OneCLI identity goes with the repo (issue #35): an agent that
	// outlives its repo keeps that repo's grants attached to an identifier
	// nothing will ever derive again — standing access to the project's shared
	// credentials that no lab surface can show or revoke. Detached from ctx for
	// the same reason the store delete above is: past the point of no return, a
	// client hanging up must not decide how far the teardown got. The identifier
	// derives from the store id we were handed, so nothing needs reading back
	// after the row is gone.
	//
	// Best-effort, warn-and-orphan: an unreachable sidecar leaves the agent
	// behind, which is exactly the status quo for every repo deleted before this
	// existed — never a reason to keep a repo the operator asked to remove.
	// (false, nil) — nothing carried that identifier — is ORDINARY and must not
	// warn: a repo created before OneCLI was configured never got an agent, and
	// a delete with nothing to delete has done its job. The warn path is also
	// where upstream's 400 "Cannot delete the default agent" lands: lab-created
	// agents are never the default, so that answer means the identifier resolved
	// to an agent lab did not create — something to report, never to work
	// around. What a successful delete does upstream: the agent's grants cascade
	// away with it while the project's secret POOL is untouched — deleting a
	// repo revokes its access to the shared credentials, it does not destroy
	// them for the repos and tools still using them.
	//
	// Ordered after the publish so a wedged sidecar cannot hold the repo.changed
	// event (and every client's refetch) behind a network round-trip: the agent
	// is not part of any repo state a client reads back.
	if s.oneCLIActive() {
		if _, err := s.oneCLI.DeleteAgent(context.WithoutCancel(ctx), onecli.AgentIdentifier(id)); err != nil {
			s.log.Warn("deleting onecli agent for removed repo", "component", "reposvc", "repo", id, "err", err)
		}
	}
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

	// Converge the pre-push guard on EVERY ready repo: the hook exists
	// unconditionally (the secret leak scan, issue #106) with content
	// matching the incogni flag (D15 §9 measure 7 patterns in or out). A
	// crash between an incogni toggle and its hook re-render, or the
	// healed-clone case above, leaves the content out of sync, and a repo
	// cloned by an older lab may lack the hook entirely — this pass is also
	// how hook-content changes between lab versions roll out. Install is
	// idempotent and safe on foreign hooks, so this converges every restart.
	s.reconcileGuardHooks(ctx)

	// Converge the OneCLI agent identities LAST, after the guard reconcile
	// above: that pass is local filesystem work every repo needs and must not
	// queue behind a network call to a sidecar that may still be booting, while
	// this one reads the repo NAMES the clone healing above may just have
	// touched. Nothing downstream depends on it, which is what makes it the
	// right thing to put at the end of the boot path.
	s.reconcileOneCLIAgents(ctx)
	if err := s.mat.CleanupAll(s.credentialKeep); err != nil {
		s.log.Warn("sweeping runtime dir", "component", "reposvc", "err", err)
	}
	return nil
}

// oneCLIAgentHealTimeout bounds the WHOLE startup agent sweep, not one call.
// Priced for the expected case rather than the happy one: the sidecar comes up
// in parallel with lab in the same compose stack, so "not listening yet" is
// routine — and it fails instantly with connection refused, so a hundred repos
// cost microseconds and the bound is never approached. What the bound is for is
// the other shape, a sidecar that ACCEPTS and then hangs, where
// internal/onecli's own 30s per-request timeout would otherwise be paid once
// per repo with lab's boot waiting behind all of it.
const oneCLIAgentHealTimeout = 30 * time.Second

// reconcileOneCLIAgents converges every repo's OneCLI agent at startup (issue
// #35): a missing agent is created and a stale display name healed in place —
// EnsureAgent does both — so a lab that ran before OneCLI was configured, or
// renamed a repo while the sidecar was down, catches up on the next boot with
// no stored state and no operator action.
//
// One-way on purpose: it NEVER deletes. A OneCLI project is SHARED surface —
// NanoClaw's agents, agents an operator created by hand, another lab's — so a
// reconcile that removed whatever it does not recognize would destroy another
// tool's identity, and the grants attached to it, the first time lab booted
// against a shared project. Repo deletion is the only path that removes an
// agent, and it removes exactly the one identifier it derived.
//
// It stops at the FIRST failure and warns ONCE. Every error reachable here is
// sidecar-level — unreachable, wedged, wrong API key — so it is the same error
// waiting for every remaining repo: pressing on would render one outage as a
// wall of identical warnings and bury whatever else the boot logged. The
// warning names the repo it stopped on and how many it never reached, which is
// what separates "OneCLI is down" from "one repo is odd".
//
// And it never fails boot: StartupHeal's error return is fatal in cmd/lab (see
// the call site), and a credential sidecar that is slow to start is not a
// reason to refuse to run a lab — every skipped repo is ensured again lazily at
// its next spawn or grant-attach.
func (s *Service) reconcileOneCLIAgents(ctx context.Context) {
	if !s.oneCLIActive() {
		return
	}
	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("reconciling onecli agents: list repos", "component", "reposvc", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, oneCLIAgentHealTimeout)
	defer cancel()
	for i, repo := range repos {
		// Token dropped, never logged (see OneCLIAgents).
		if _, err := s.oneCLI.EnsureAgent(ctx, onecli.AgentIdentifier(repo.ID), repo.Name); err != nil {
			s.log.Warn("reconciling onecli agents", "component", "reposvc", "repo", repo.ID, "err", err, "skipped", len(repos)-i-1)
			return
		}
	}
}

// installGuardHook writes the pre-push guard AND pins the bare repo's local
// core.hooksPath to the absolute hooks dir, so a global/system core.hooksPath
// (husky &c.) cannot route ANY agent push past the guard — the pin now
// protects the secret scan on every repo, not just incogni ones. The guard is
// two-part: the secret leak scan (issue #106) renders unconditionally, and
// the incogni attribution/seeded-path scans (D15 §9 measure 7) render only
// when incogni is set. When they do, the scrub + seeded-path patterns are the
// union of every registered provider's declared patterns (ADR-0033), not just
// repo id's own — since ADR-0030 made the agent CLI a per-spawn override, any
// registered provider can push through this repo's guard in a given session,
// so a guard keyed to one repo's provider would miss the others.
func (s *Service) installGuardHook(ctx context.Context, id string, incogni bool) error {
	var scrub, seededPaths []string
	if incogni {
		scrub, seededPaths = s.incogniPatterns()
	}
	bareDir := s.bareDir(id)
	if err := seeder.InstallPrePushHook(bareDir, scrub, seededPaths); err != nil {
		return err
	}
	return s.git.PinHooksPath(ctx, bareDir, s.gitEnv)
}

// incogniPatterns returns the incogni pre-push guard's scrub + seeded-path
// patterns: the union of EVERY registered provider's declared SeedMeta
// (ADR-0033), never one repo's own effective provider. ADR-0030 made the
// agent CLI a per-session override — any registered provider can run against
// any repo for one push — so a guard keyed to a single provider is exactly
// the race ADR-0033 closes; the union is screened regardless of which
// provider the pushing session actually ran. A nil registry (the no-provider
// degraded boot) and an empty registry both yield empty pattern lists — a
// content-inert guard, matching seeder.InstallPrePushHook's existing
// empty-list handling — rather than an error: unlike the old per-repo
// resolution, there is no "unresolvable provider" case left to reject.
func (s *Service) incogniPatterns() (scrub, seededPaths []string) {
	if s.providers == nil {
		return nil, nil
	}
	return s.providers.UnionScrubPatterns(), s.providers.UnionSeededPathPatterns()
}

// reconcileGuardHooks converges the pre-push guard on every ready repo
// (called at startup): the hook exists unconditionally, its content matches
// the incogni flag — install-only, never remove. Best-effort per repo — a
// failure (e.g. a foreign pre-push hook the seeder refuses to overwrite) is
// logged and the others still reconcile.
func (s *Service) reconcileGuardHooks(ctx context.Context) {
	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("reconciling pre-push guards: list repos", "component", "reposvc", "err", err)
		return
	}
	for _, repo := range repos {
		if repo.CloneStatus != store.CloneStatusReady {
			continue
		}
		if err := s.installGuardHook(ctx, repo.ID, repo.Incogni); err != nil {
			s.log.Warn("reconciling pre-push guard: install", "component", "reposvc", "repo", repo.ID, "err", err)
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

// validateProvider normalizes and validates an optional provider id (issue
// #66): nil or empty/whitespace means inherit and normalizes to nil (NULL);
// a non-empty id must be registered — a typo must 400, never wedge future
// spawns. Without a registry (the no-provider degraded boot) the value passes
// unchecked, mirroring the settings PATCH; the spawn path re-resolves anyway.
func (s *Service) validateProvider(field string, v *string) (*string, error) {
	if v == nil {
		return nil, nil
	}
	id := strings.TrimSpace(*v)
	if id == "" {
		return nil, nil
	}
	if s.providers != nil {
		if _, ok := s.providers.Get(id); !ok {
			return nil, badRequestf("%s: unknown provider %q", field, id)
		}
	}
	return &id, nil
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
