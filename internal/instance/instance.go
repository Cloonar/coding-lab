// Package instance owns the manual instance lifecycle (design §1; brief M3):
// Start (the synchronous fail-loud spawn sequence with full rollback and the
// starting-set race guard), Stop (guarded teardown + terminal outcome + token
// deletion + per-run tree wipe), and StopAll. It codes against the real core
// seams — gitx worktrees, the tmuxx SessionRunner, the provider registry, the
// vault materializer, and startguard — and is the single owner of runs-row
// creation and the run.changed SSE event on the manual path. AFK-labeled
// sessions are a seam delegated to the M5 AFK engine (Stop refuses them with
// a 501-mapped error in M3).
//
// The Start sequence is v0-pinned (sessions-spawn + git-worktrees port specs):
// cap check → FORCE auth refresh (never trust the 30s cache before a spawn) →
// label/branch/worktree derivation → startguard.Mark → per-run tree + per-run
// credential materialization (issues #202/#205) → gitx.AddWorktree (fail-loud
// fetch, no fallback base) → seed workspace → create runs row + mint run
// token → tmux Start → StampOpened → async deep-link capture → run.changed.
// Any failure after worktree creation rolls back to the exact pre-Start state
// (RemoveWorktree + force DeleteBranch + delete row/token + per-run tree
// wipe, which removes the run's credential files and dialog settings); a
// failure before worktree creation rolls back nothing but the per-run tree.
package instance

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
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

// GatewayAPI is the OneCLI REST seam the launch path resolves a run's
// credential-gateway wiring through (issue #24 / ADR-0067): the repo's agent
// identity — whose listing row carries the stable gateway access token the
// run authenticates with — and the grants on it. Satisfied by
// *onecli.Client; nil = the integration is unconfigured, which is the normal
// state of a lab and must stay indistinguishable from a lab built before this
// existed (gatewayActive is the one gate).
//
// The identity is resolved by the IDENTIFIER derived from the repo's store ID
// (onecli.AgentIdentifier) — the match key, immutable and unique upstream —
// while displayName is the repo's own name riding along so the OneCLI
// dashboard reads as a list of repositories rather than of store ids. Which
// of the two is load-bearing is the whole of issue #35, and prepareGateway's
// call site carries the reasoning.
//
// Narrow on purpose, like WorkspaceSeeder and ConversationStater beside it:
// two methods is exactly what a spawn needs, so a test drives the whole
// gateway precheck with a struct literal and no HTTP, and the pool/attach/
// detach half of internal/onecli (the #25 grant picker's surface) cannot be
// reached from a launch even by accident.
type GatewayAPI interface {
	EnsureAgent(ctx context.Context, identifier, displayName string) (onecli.Agent, error)
	ListGrants(ctx context.Context, agentID string) ([]onecli.Grant, error)
}

// BastionAPI is the Warpgate admin-API seam the launch path wires a run's SSH
// targets through, and the revocation path removes its key through (issue #39
// / ADR-0068). Satisfied by *warpgate.Client; nil = no Warpgate REST client
// configured, the normal state of a lab (bastionActive is the spawn gate,
// and RevokeBastionKey/SweepBastionKeys gate on this field alone).
//
// Narrow on purpose, like GatewayAPI beside it: exactly what a spawn needs
// (heal the repo's identity, read its role's targets fresh, register the run
// key), what a wipe needs (remove one key) and what the startup sweep needs
// (find an identity read-only, list its keys). The picker's assign/unassign
// and the identity delete stay unreachable from here even by accident.
type BastionAPI interface {
	EnsureRepoIdentity(ctx context.Context, repoID, repoName string) (warpgate.Identity, error)
	FindRepoIdentity(ctx context.Context, repoID string) (warpgate.Identity, bool, error)
	RoleSSHTargets(ctx context.Context, roleID string) ([]warpgate.Target, error)
	AddPublicKey(ctx context.Context, userID, label, authorizedKey string) (warpgate.PublicKey, error)
	ListPublicKeys(ctx context.Context, userID string) ([]warpgate.PublicKey, error)
	RemovePublicKey(ctx context.Context, userID, keyID string) error
}

// BastionHostKeys is the host-key pin seam (ADR-0068 decision 4): KnownHosts
// scans Warpgate's SSH listener and returns the known_hosts text a run gets —
// rendered from the trusted keys in --warpgate-ssh-host-key when that is set
// and the listener presents one of them, else from every key the listener
// presented — or an actionable error on a mismatch or an unreachable
// listener. Satisfied by *warpgate.HostKeyPin. Every target-bearing spawn
// calls it; the scan doubles as the SSH listener's reachability probe.
type BastionHostKeys interface {
	KnownHosts(ctx context.Context) (string, error)
}

// Compile-time proof that the two concrete types cmd/lab wires satisfy the
// seams.
var (
	_ BastionAPI      = (*warpgate.Client)(nil)
	_ BastionHostKeys = (*warpgate.HostKeyPin)(nil)
)

// Options configures a Service. Everything except Logger, GitEnv, CaptureCtx,
// Seeder, and Now is required.
type Options struct {
	Store     *store.Store
	Git       *gitx.Engine
	Runner    tmuxx.SessionRunner
	Providers *provider.Registry
	Vault     *vault.Vault

	// Materializer is the GLOBAL runtime materializer (<state>/runtime).
	// Since issue #205 the launch path materializes run credentials into a
	// PER-RUN materializer over Homes.RuntimePath(runID) instead; the global
	// one remains only as the seed source for each run's known_hosts
	// (SeedKnownHosts — clone/fetch ops keep accumulating TOFU pins there).
	Materializer *vault.Materializer
	Guard        *startguard.Guard
	Bus          *events.Bus
	Logger       *slog.Logger

	// Homes owns <state>/instances — the per-run tree lifecycle (issues
	// #202/#205): Launch materializes a run's home + runtime dirs,
	// Stop/rollback wipes the tree, and the boot/runtime sweeps GC orphans.
	// Required — the launch path always materializes the tree so the spawned
	// CLI's HOME is the run's private store (never the machine's master
	// ~/.claude*/~/.codex) and its git credential files live in the run's
	// private runtime dir (never the global one).
	Homes *instancehome.Manager

	// ReposDir is <state>/repos — the parent of every bare reference clone
	// (design §7: repos/<repoID>.git). WorktreeRoot is <state>/worktrees, the
	// parent of every instance worktree.
	ReposDir     string
	WorktreeRoot string

	// LabURL is the value handed to spawned sessions as LAB_URL so labctl can
	// reach the agent API. It comes from labURL()'s precedence (issue #201):
	// the dedicated agent URL if set, else the agent unix socket
	// unix://<state-dir>/agent/agent.sock.
	LabURL string

	// --- Container runner wiring (issue #205). All optional: with
	// ContainerPreflight nil, container mode is structurally unavailable —
	// a Runner=container repo's spawn is refused and the podman rm backstops
	// are no-ops — and every host-mode path is byte-identical to before.

	// PodmanBin is the podman binary (--podman) the container pane argv and
	// the rm backstop shell out to.
	PodmanBin string
	// ContainerImage is the deployed fallback dev image (--container-image):
	// the LAST layer of a container run's dev image chain, used only when
	// neither the repo's image_ref nor the dev_image_default setting is set
	// (EffectiveDevImage, issue #55 / ADR-0071). Deliberately no default:
	// ADR-0051 makes the container userland the operator's, so with all three
	// layers empty the spawn is refused, never handed an image lab picked.
	ContainerImage string
	// ContainerToolsImages maps provider id → agent-tools image ref
	// (--container-tools-image, digest-pinned per ADR-0051), the read-only
	// /opt/lab injection. A provider without an entry cannot spawn in
	// container mode — its CLI would not exist inside the container.
	ContainerToolsImages map[string]string
	// PodmanRun is the exec seam the rm backstop runs podman through; nil →
	// podmanx.ExecRunner(). Injectable so container Stop/rollback tests
	// record argv instead of spawning podman.
	PodmanRun podmanx.CmdRunner
	// ContainerPreflight reads the startup preflight verdict (podmanx.Gate's
	// Result method): (r, true) once the boot preflight finished, (_, false)
	// while it is still running. nil — the cmd/lab wiring when no container
	// config is present — means container mode is structurally unavailable on
	// this server. Consulted per spawn, so a preflight finishing after boot
	// unblocks container spawns without a restart.
	ContainerPreflight func() (podmanx.Result, bool)
	// AgentSockDir is <state>/agent (agentapi.SocketDir) — the directory
	// holding agent.sock, bind-mounted whole into every container so the
	// socket survives a server restart (a bind of the FILE would pin the dead
	// inode; see agentapi.SocketDir). Also the anchor of the container-side
	// LAB_URL rewrite: a container always talks over this mounted socket.
	AgentSockDir string

	// --- OneCLI credential-gateway wiring (issue #24 / ADR-0067). All
	// optional, and the same shape as the container block above: with OneCLI
	// nil or OneCLIGatewayURL empty the wiring is structurally OFF — no
	// precheck runs, no trust bundle is written, no proxy env is assembled,
	// the seeder is handed no GatewayRef, and every spawn is byte-identical to
	// a lab that never had these fields. That parity is the acceptance
	// criterion of issue #24, so it is expressed as one gate (gatewayActive)
	// rather than as a nil-check per call site.

	// OneCLI is the REST seam a spawn resolves its repo's agent identity,
	// proxy token and grants through. Production passes *onecli.Client; nil is
	// the unconfigured lab.
	OneCLI GatewayAPI
	// OneCLIGatewayURL is the gateway PROXY address (--onecli-gateway-url,
	// e.g. http://10.88.0.1:10255) a run's HTTPS_PROXY points at, with the
	// run's agent token folded in as userinfo. Deliberately NOT derived from
	// the REST URL: ADR-0067 keeps the two independently settable because the
	// address lab dials is loopback and a containerized run cannot reach
	// lab's loopback (ADR-0052's host.containers.internal pin).
	OneCLIGatewayURL string
	// OneCLICAFile is the host path of the gateway's interception CA
	// (--onecli-ca-file), composed with the host's system roots into each
	// run's trust bundle. Empty WITH a gateway URL set is a spawn refusal
	// (prepareGateway) rather than a config error at Parse: the two settings
	// are independent in cmd/lab, and this is the one place that knows a run
	// is about to be pointed at a TLS-terminating proxy it could not verify.
	OneCLICAFile string

	// --- Warpgate SSH-bastion wiring (issue #39 / ADR-0068). All optional,
	// the OneCLI block's shape again: a run is wired only when ALL THREE are
	// set (bastionActive), and even then only when its repo has at least one
	// SSH target. With any of them unset every spawn is byte-identical to a
	// lab that never had these fields — no Warpgate call, no key, no file, no
	// PATH change, no seeder section.
	//
	// The two interface fields share the OneCLI fields' nil-interface trap:
	// a nil *warpgate.Client or nil *warpgate.HostKeyPin assigned straight
	// into them is a NON-nil interface holding a nil pointer, which would read
	// as "configured" and panic inside the client on the first target-bearing
	// spawn. cmd/lab must leave them nil when unconfigured (declare the
	// interface, assign only a non-nil pointer); New also normalizes the two
	// concrete typed nils back to nil as the belt to those braces.

	// Warpgate is the admin-API seam (--warpgate-url + the paired
	// --warpgate-admin-token-file). Production passes *warpgate.Client. Set
	// on its own — the REST pair without an SSH address — it drives nothing
	// at spawn (a lab with no address has nothing to hand a run), but it
	// still arms RevokeBastionKey and SweepBastionKeys: keys registered under
	// an earlier configuration must stay revocable.
	Warpgate BastionAPI
	// WarpgateSSHAddr is the bastion's SSH listener as a RUN dials it
	// (--warpgate-ssh-addr, host:port) — the HostName/Port of every alias.
	// Independently settable for --onecli-gateway-url's reason: lab's REST
	// address is loopback, and a containerized run cannot reach the host's
	// loopback (ADR-0052's host.containers.internal pin).
	WarpgateSSHAddr string
	// WarpgateHostKeys is the host-key pin over WarpgateSSHAddr. Production
	// passes *warpgate.HostKeyPin built on the same address.
	WarpgateHostKeys BastionHostKeys

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
	homes     *instancehome.Manager
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

	// Container runner wiring (issue #205; see the Options fields). podmanRun
	// is always non-nil (New defaults it); containerPreflight nil = container
	// mode structurally unavailable.
	podmanBin            string
	containerImage       string
	containerToolsImages map[string]string
	podmanRun            podmanx.CmdRunner
	containerPreflight   func() (podmanx.Result, bool)
	agentSockDir         string

	// OneCLI credential-gateway wiring (issue #24 / ADR-0067; see the Options
	// fields). onecli nil or oneCLIGatewayURL empty = the integration is off.
	onecli           GatewayAPI
	oneCLIGatewayURL string
	oneCLICAFile     string

	// Warpgate SSH-bastion wiring (issue #39 / ADR-0068; see the Options
	// fields). warpgate nil = no REST client at all; bastionActive() is the
	// spawn gate over all three.
	warpgate         BastionAPI
	warpgateSSHAddr  string
	warpgateHostKeys BastionHostKeys

	// hostPATH returns the base PATH a wired HOST run's wrapper dir is
	// prepended to. New sets it to read os.Getenv("PATH") AT SPAWN, not once
	// at startup, deliberately: tmuxx computes a pane's baseline PATH from the
	// lab process environment per call as well, so a wired run's PATH is the
	// wrapper dir followed by exactly the PATH an unwired pane spawned at the
	// same instant inherits. Tests replace it with a constant.
	hostPATH func() string
	// userSSHConfig returns the lab service user's own per-user OpenSSH
	// config (<passwd home>/.ssh/config), which a wired host run's config
	// re-includes, or "" when the home is unknown. New sets it to
	// serviceUserSSHConfig; it is consulted only for a wired host run. Tests
	// replace it so they never depend on the machine's passwd entry.
	userSSHConfig func() string

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
	case o.Homes == nil:
		return nil, fmt.Errorf("instance: Options.Homes is required")
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
	podmanRun := o.PodmanRun
	if podmanRun == nil {
		podmanRun = podmanx.ExecRunner()
	}
	bastion, hostKeys := normalizeBastion(o.Warpgate, o.WarpgateHostKeys)
	return &Service{
		store:        o.Store,
		git:          o.Git,
		runner:       o.Runner,
		providers:    o.Providers,
		vault:        o.Vault,
		mat:          o.Materializer,
		homes:        o.Homes,
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

		podmanBin:            o.PodmanBin,
		containerImage:       o.ContainerImage,
		containerToolsImages: o.ContainerToolsImages,
		podmanRun:            podmanRun,
		containerPreflight:   o.ContainerPreflight,
		agentSockDir:         o.AgentSockDir,

		onecli:           o.OneCLI,
		oneCLIGatewayURL: o.OneCLIGatewayURL,
		oneCLICAFile:     o.OneCLICAFile,

		warpgate:         bastion,
		warpgateSSHAddr:  o.WarpgateSSHAddr,
		warpgateHostKeys: hostKeys,
		hostPATH:         func() string { return os.Getenv("PATH") },
		userSSHConfig:    serviceUserSSHConfig,
	}, nil
}

// normalizeBastion turns a typed nil — a nil *warpgate.Client or nil
// *warpgate.HostKeyPin stored in the interface-typed Options fields — into
// the nil interface it was meant to be (httpapi's normalizeWarpgate, the same
// belt for the same braces). Without it a lab that configured no Warpgate but
// passed a nil pointer through would read as configured, and its first
// target-bearing spawn would panic inside the client. Only the two concrete
// types lab wires are recognized; a test fake is whatever it is.
func normalizeBastion(api BastionAPI, keys BastionHostKeys) (BastionAPI, BastionHostKeys) {
	if c, ok := api.(*warpgate.Client); ok && c == nil {
		api = nil
	}
	if p, ok := keys.(*warpgate.HostKeyPin); ok && p == nil {
		keys = nil
	}
	return api, keys
}

// serviceUserSSHConfig is the per-user OpenSSH config a HOST run's ssh read
// before it was wired: the passwd home of lab's own uid plus /.ssh/config.
// The PASSWD home, via os/user, not $HOME — that is where OpenSSH looks
// (pw->pw_dir), and the reason a config in the instance HOME is invisible to
// it (ADR-0068). os/user caches its answer, so calling this per spawn costs
// one lookup per process. "" when the home cannot be determined; the run's
// config then simply omits the per-user Include.
func serviceUserSSHConfig() string {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return ""
	}
	return filepath.Join(u.HomeDir, ".ssh", "config")
}

// gatewayActive reports whether this lab has the OneCLI run wiring turned on.
// The gate is BOTH halves: the REST client (--onecli-url + the paired
// --onecli-api-key-file, which is what makes Options.OneCLI non-nil in
// cmd/lab) AND --onecli-gateway-url.
//
// The second half is not redundant with the first. ADR-0067 makes the gateway
// URL independently settable — the REST address lab dials is loopback, and a
// containerized run cannot reach lab's loopback — so a lab CAN legitimately be
// configured with the REST pair and no gateway URL (issue #23's health surface
// is exactly such a deployment). Such a lab has nothing to point HTTPS_PROXY
// at, so the wiring stays OFF and its spawns are unchanged. It must NOT become
// a spawn refusal: the fail-closed pin is about a gateway that was configured
// and is unreachable, never about a gateway nobody asked for.
func (s *Service) gatewayActive() bool {
	return s.onecli != nil && s.oneCLIGatewayURL != ""
}

// bastionActive reports whether this lab wires runs to the Warpgate SSH
// bastion (issue #39 / ADR-0068): the REST client (--warpgate-url + the
// paired --warpgate-admin-token-file), the SSH address a run dials
// (--warpgate-ssh-addr), and the host-key pin over that address — all three.
//
// The REST pair alone is a legitimate deployment, not a half-configured one:
// it gives health and the per-repo identity lifecycle, and its spawns are
// UNCHANGED — ADR-0068 calls it "unconfigured for this purpose, not a
// refusal", because a lab with no address has nothing to hand a run, and
// refusing would turn a deployment choice into a fleet outage. Even with all
// three set, a spawn is wired only when its repo has at least one SSH target
// (prepareBastion); this gate only decides whether that question is asked.
func (s *Service) bastionActive() bool {
	return s.warpgate != nil && s.warpgateSSHAddr != "" && s.warpgateHostKeys != nil
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
	// The run's private instance HOME (issue #202): the deep-link registry the
	// provider reads lives under it. HomePath is a PURE derivation, so a
	// pre-upgrade run adopted after a restart yields a nonexistent dir → the
	// provider finds no registry → a capture miss → the loud ADR-0017 miss log
	// plus the fallback link, which is the designed degradation for a run that
	// never had a per-run home.
	url, err := dl.CaptureDeepLink(s.captureCtx, run.SessionName, run.WorktreePath, s.homes.HomePath(run.ID))
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
