package instance

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// LaunchSpec is the fully-derived identity of one session launch — the shared
// claim→seed→spawn core behind the manual Start and the M5 AFK engine's
// locked claim path (design: ONE launch/rollback implementation, never two).
// The caller has already made every decision: preflight checks (cap, auth,
// model/effort validation), the session name / branch / worktree derivation,
// and — for AFK runs — the issue selection and the persisted budget clock.
type LaunchSpec struct {
	Repo     store.Repo
	Provider provider.AgentProvider

	Kind        string // store.RunKindManual | RunKindAFKManual | RunKindAFKAuto | RunKindLander | RunKindFix | RunKindEscalate | RunKindScheduled
	IssueNumber *int   // AFK/lander runs: the claimed issue; manual and scheduled: nil

	// PullNumber is the PR an autoland run works (issue #188 / migration 0022 /
	// ADR-0048's amendment) — the lander validating it, the fix run
	// re-engaging it, the escalate run handing it off; all three launch paths
	// already hold it. nil for manual/afk_manual/afk_auto/scheduled, which
	// touch no PR. Like IssueNumber and ScheduleID it is run IDENTITY stamped
	// onto the row rather than re-derived, and for a sharper reason than
	// either: the escalation-terminality gate (store.EscalatedRunForPull) is
	// PR-scoped, and afk/<N> claim branches derive from the ISSUE number — so a
	// requeued issue's brand-new PR shares its predecessor's branch and ONLY
	// the pull number tells the two apart, across restarts included.
	PullNumber *int

	// ScheduleID links a scheduled run to the Schedule it is a firing of
	// (issue #247 / ADR-0062) — identity, exactly like IssueNumber is for an
	// AFK run: skip-on-overlap and the per-Schedule failure counter attribute
	// the run through the persisted runs.schedule_id column, never by parsing
	// the session label. nil for every other kind.
	ScheduleID *string

	// AdoptBranch checks out the EXISTING Branch DETACHED, at its origin tip
	// (gitx.AddWorktreeExisting), instead of forking a fresh one — the lander
	// run's mode (issue #181): the PR head branch pre-exists the run, so it IS
	// someone's claim. A rollback must remove the worktree but never delete
	// the branch; because the adopt is detached it creates no local branch
	// either, so there is nothing of its own for the rollback to undo.
	AdoptBranch bool

	SessionName  string
	Branch       string
	WorktreePath string
	Model        string
	Effort       string
	// Remote is the RESOLVED remote-control value (issue #163) — already layered
	// and capability-clamped by ResolveRemote, so a plain bool here (unlike the
	// *bool of the request layers) carries no "unset" state left to interpret.
	// Launch spends it twice: into provider.SpawnSpec (the provider decides what
	// remote control means) and onto the runs row, which is the ONLY thing that
	// still knows after a restart — the deep-link capture gate reads it there.
	Remote bool
	// Options is the resolved provider-owned spawn-options bag (issue #19 /
	// ADR-0021), already filtered + validated to the provider's schema by
	// ResolveSpawnOptions. Empty for manual runs; for AFK runs it carries the
	// resolved bag (e.g. {"ultracode":"true"}). Threaded verbatim into
	// SpawnSpec.Options — the provider applies it.
	Options map[string]string

	// BudgetDeadline is the persisted budget clock (D12b; AFK runs only) and
	// TokenExpiry the run token's expires_at (§3a: AFK → budget_deadline +
	// 30min; manual → nil, no wall clock).
	BudgetDeadline *time.Time
	TokenExpiry    *time.Time

	// SeedPrompt, when non-empty, is the initial prompt carried into the spawn
	// as claude's trailing positional argument (provider.SpawnArgv), the
	// pinned v0 mechanism — so the prompt exists before the process and is
	// never lost to the cold-start TUI race that a post-spawn keystroke would
	// hit. For an AFK run it is the resolved AFK seed prompt (built-in
	// template or afk_prompt override); for a manual run it is the operator's
	// first chat message when given (issue #96), else "" (no trailing
	// argument, the pre-#96 behavior). Either way the field carries no
	// run-kind semantics of its own — Kind alone decides AFK vs. manual.
	SeedPrompt string
}

// Launch runs the v0-pinned spawn sequence for a fully-derived spec: the
// credential-gateway precheck, which refuses the spawn before the claim when
// the gateway is configured and unreachable (issue #24 / ADR-0067) →
// startguard.Mark → per-run tree materialization (home + runtime + imports,
// issues #202/#205/#261) → the run's gateway trust bundle → credential
// materialization into the per-run runtime
// dir → read-only import snapshots, in parallel, refusing the spawn before the
// claim if any target fails (issue #261) →
// gitx.AddWorktree (fail-loud fetch, no fallback base) → workspace seeding
// (provider trust/MCP, then lab's skills bundle + CLAUDE.local.md) → runs
// row + run token → tmux spawn (SeedPrompt, when set, carried as the spawn
// argv's trailing positional — the AFK seed prompt or a manual run's first
// chat message, issue #96) → StampOpened → async deep-link capture →
// run.changed. Any failure after worktree creation rolls back to the exact
// pre-launch state (session kill, RemoveWorktree, force DeleteBranch, run
// row/token delete, per-run tree wipe — the wipe subsumes the old per-file
// credential/settings cleanup, issue #205); a failure before worktree
// creation rolls back nothing but the per-run tree. For an AFK spec the
// worktree/branch creation IS the claim, so the rollback is what releases a
// claimed issue back into the selectable queue.
func (s *Service) Launch(ctx context.Context, spec LaunchSpec) (store.Run, error) {
	repo := spec.Repo
	name := spec.SessionName
	branch := spec.Branch
	wtPath := spec.WorktreePath
	bareDir := s.bareDir(repo.ID)
	runID := ids.NewID("run")

	// Container-mode gate + image/limit resolution (issues #205, #207), FIRST
	// — before the guard, the per-run tree, and above all before AddWorktree:
	// for an AFK spec the worktree IS the claim, so a container refusal (host
	// not ready, missing tools image, no dev image for the repo, an
	// unresolvable dev-image ref) landing any later would park the issue behind
	// a host/config problem. The gate and limit reads are pure; the one side
	// effect is EnsureImage's pull-if-missing (#207), placed here on purpose so
	// a failed pull refuses PRE-claim rather than stranding one. A refusal here
	// rolls back nothing because nothing exists yet. The resolved image and
	// limits carry to the spawn branch below.
	container := repo.Runner == store.RunnerContainer
	var ctrImage, ctrMemory string
	var ctrPids, ctrNofile int
	if container {
		var err error
		if ctrImage, err = s.refuseContainerSpawn(spec.Provider.ID(), repo); err != nil {
			return store.Run{}, err
		}
		if ctrMemory, ctrPids, ctrNofile, err = s.effectiveContainerLimits(ctx, repo); err != nil {
			return store.Run{}, &StartFailedError{cause: err}
		}
		// Pull-if-missing the effective dev image before the claim (issue #207):
		// EnsureImage probes `image exists` and pulls once on a miss, so a bad
		// ref or an unreachable registry is refused HERE — never after
		// AddWorktree, where a failed pull would park the AFK claim. The failure
		// is a *BadRequestError (the #205 posture: operator/config-fixable
		// refusals are 400s carrying the actionable text verbatim, never 500s).
		// A cold pull can block the request for the length of the download — the
		// documented trade of pinning the pull to spawn time (ADR-0053).
		if err = podmanx.EnsureImage(ctx, s.podmanRun, s.podmanBin, ctrImage); err != nil {
			return store.Run{}, badRequestf("%s", err)
		}
	}

	// The credential-gateway precheck (issue #24 / ADR-0067), in the SAME
	// pre-claim spot and for the same reason as the container gate above and
	// the read-only-import materialization below: for an AFK spec AddWorktree
	// IS the claim, so a refusal landing any later would park the issue behind
	// a host/config problem — an unreachable sidecar would take the issue out
	// of the selectable queue instead of just refusing the spawn. Nothing
	// exists yet, so a refusal here rolls back nothing. The resolved wiring
	// (proxy URL + granted service names) carries down to the trust bundle,
	// the spawn env, and the seeder; the zero value means this lab has no
	// gateway and every step below behaves as it did before #24.
	gw, err := s.prepareGateway(ctx, repo)
	if err != nil {
		return store.Run{}, err
	}

	// Sweep guard spans worktree-creation → session-live (§4b). Cleared on
	// every return via defer (success or rollback), matching v0's
	// markStarting/defer clearStarting.
	s.guard.Mark(name)
	defer s.guard.Clear(name)

	// The run's private per-run tree (issues #202/#205), materialized FIRST:
	// the runtime subdir must exist before the credential materializer writes
	// into it, and the credential env must exist before the worktree fetch
	// authenticates with it. The homeDirMinAge sweep guard covers the whole
	// launch window, so materializing this early is safe. wipeHome is the
	// pre-worktree failure cleanup (nothing else exists yet); the full
	// rollback below reuses the same wipe as its last step. A materialize
	// failure is a real I/O problem — abort.
	home, err := s.homes.Materialize(runID)
	if err != nil {
		return store.Run{}, &StartFailedError{cause: err}
	}
	wipeHome := func() {
		if err := s.homes.Wipe(runID); err != nil {
			s.log.Warn("start rollback: wipe instance home", "component", "instance", "run", runID, "err", err)
		}
	}

	// The run's private runtime materializer (issue #205): credential files,
	// known_hosts, the dialog settings file, and the hook spools all live
	// under <state>/instances/<runID>/runtime — one run's surface, wiped with
	// the run, bind-mountable into a future container without exposing any
	// other run's secrets.
	runMat, err := vault.NewMaterializer(s.homes.RuntimePath(runID))
	if err != nil {
		wipeHome()
		return store.Run{}, &StartFailedError{cause: err}
	}
	// Seed ssh's TOFU state from the global runtime dir's known_hosts so a
	// fresh per-run dir does not reset previously pinned host keys (issue
	// #205). Best-effort: a failed seed only degrades to accept-new
	// re-pinning for this run — never a reason to block a launch.
	if err := runMat.SeedKnownHosts(s.mat.KnownHostsPath()); err != nil {
		s.log.Warn("seeding per-run known_hosts", "component", "instance", "run", runID, "err", err)
	}

	// The gateway trust bundle and this run's proxy env (issue #24 /
	// ADR-0067), written HERE: after vault.NewMaterializer above, which
	// MkdirAll'd the run's runtime dir 0700 (writeTrustBundle deliberately
	// does no MkdirAll of its own, so it would fail against a directory
	// nobody mounts), and still BEFORE AddWorktree, keeping the whole gateway
	// story on the pre-claim side of the launch. The container runner already
	// binds that dir rw at its HOST-IDENTICAL path (podmanx.RunSpec.RuntimeDir),
	// which is what makes the bundle path in the env valid on both sides of
	// the container boundary — no new mount, no new podman flag, no path
	// translation.
	//
	// A failure is a *BadRequestError (400), not a StartFailedError (500), and
	// the choice is deliberate: the realistic members of this error set are
	// operator config — an --onecli-ca-file that does not exist, is
	// unreadable, or is a DER blob / an HTML error page rather than a PEM, or
	// a host with no system CA bundle at all — which is exactly the
	// refuseContainerSpawn posture of naming an operator-fixable
	// host/config mismatch as a client error with the text verbatim. The one
	// genuine I/O member (the write itself, e.g. a full disk) is thereby
	// misfiled as a 400; that costs the operator nothing, since both mappings
	// surface the same message and it names the path, whereas the reverse
	// choice would misfile the COMMON case as a lab fault. wipeHome is the
	// whole cleanup — nothing else exists yet.
	//
	// The resolving provider's declaration is read ONCE here and consumed
	// twice: DirectAPIHosts by the NO_PROXY list just below (issue #24 — the
	// hosts this run must reach directly, declared by the provider precisely so
	// core never names one, ADR-0033) and the seeding shapes by the generic
	// seeder further down. SeedMeta() clones its slices per call, so a second
	// call would only mint a second copy of the same static declaration.
	seedMeta := spec.Provider.SeedMeta()
	var proxyEnv []string
	if gw.active() {
		bundlePath, err := writeTrustBundle(s.homes.RuntimePath(runID), s.oneCLICAFile)
		if err != nil {
			wipeHome()
			return store.Run{}, badRequestf("%s", err)
		}
		proxyEnv = proxyBundleEnv(gw.proxyURL, bundlePath, noProxyValue(s.labURL, repo.RemoteURL, seedMeta.DirectAPIHosts))
	}

	// Materialize the repo's GIT credential (opID = run id) into the per-run
	// runtime dir, for the worktree fetch AND the spawned session's git env.
	// The files live for the whole session; the per-run tree wipe (rollback
	// here, Stop/reap later) removes them — no per-file cleanup.
	credEnv, err := s.credentialEnv(ctx, repo, runID, runMat)
	if err != nil {
		wipeHome()
		return store.Run{}, &StartFailedError{cause: err}
	}
	gitEnv := append(append([]string{}, s.gitEnv...), credEnv...)

	// The repo's read-only imports (issue #261 / ADR-0063), materialized HERE
	// — after the git env exists (each import fetch authenticates with its own
	// target's credential, built inside) and BEFORE AddWorktree, which is the
	// claim. A target-side failure (dead remote, rotated credential, a target
	// whose own clone has not finished) therefore refuses the spawn with
	// nothing claimed: an AFK spec's issue stays selectable instead of parked
	// behind another repo's outage, the same refusal-before-claim rule the
	// container gate above obeys. Because Launch is the ONE path every run kind
	// goes through, spawn parity across manual/AFK/lander/fix/escalate/
	// scheduled runs is by construction, not by keeping call sites in step. The
	// refusal names the failing target; the cleanup is wipeHome alone, since
	// the snapshots live INSIDE the per-run tree.
	importRefs, importDirs, err := s.materializeImports(ctx, repo, runID, runMat)
	if err != nil {
		wipeHome()
		return store.Run{}, &StartFailedError{cause: err}
	}

	// AddWorktree: fail-loud fetch → fork from origin/<default>, NO fallback
	// base. A failure here created nothing beyond the per-run tree (the run
	// row/token do not exist yet) — wipe it and stop. An adopt-branch spec
	// (the lander) checks out the EXISTING branch instead, aligned to what
	// the forge sees.
	addWorktree := func() error {
		if spec.AdoptBranch {
			return s.git.AddWorktreeExisting(ctx, bareDir, wtPath, branch, gitEnv)
		}
		return s.git.AddWorktree(ctx, bareDir, wtPath, branch, repo.DefaultBranch, gitEnv)
	}
	if err := addWorktree(); err != nil {
		wipeHome()
		return store.Run{}, &StartFailedError{cause: err}
	}

	// rollback restores the exact pre-launch state after the worktree exists:
	// RemoveWorktree + force DeleteBranch (both attempted, each logged) + the
	// run row/token (when created) + the whole per-run tree — home AND
	// runtime, so the credential files and the dialog settings file go with
	// it (issue #205; no per-file removal). The wipe comes LAST, after the
	// session kill, so no live process is stranded mid-teardown with its
	// credential surface already gone. Runs on a detached context so a client
	// disconnect can't strand a half-built instance.
	rollback := func(rowCreated bool) {
		rctx := context.WithoutCancel(ctx)
		// Kill the session first. runner.Start can return an error while the
		// tmux session is already live — the daemonized session survives the
		// request context being cancelled during tmuxx's post-spawn recheck.
		// Stop is idempotent (no-op when nothing is live), so it is harmless on
		// the pre-spawn rollback paths and reaps the orphan on the spawn-error
		// path (sessions-spawn: "full rollback, nothing left behind").
		if err := s.runner.Stop(rctx, name); err != nil {
			s.log.Warn("start rollback: stop session", "component", "instance", "session", name, "err", err)
		}
		// Container backstop (issue #205): the kill above SIGHUPs a container
		// pane's podman client and --rm reaps, but a CLI ignoring SIGHUP would
		// leave the container (and its claimed name) behind a rolled-back
		// launch. Deterministic name, --ignore no-op for host panes.
		s.removeRunContainer(rctx, name)
		if err := s.git.RemoveWorktree(rctx, bareDir, wtPath, gitEnv); err != nil {
			s.log.Warn("start rollback: remove worktree", "component", "instance", "session", name, "worktree", wtPath, "err", err)
		}
		// An adopted branch pre-exists the run (it IS the claim, issue #181):
		// deleting it here would destroy the claim. An adopt is also detached,
		// so it never created a local branch in the first place — there is
		// nothing for this rollback to undo, and skipping is both safe and
		// complete.
		if !spec.AdoptBranch {
			if err := s.git.DeleteBranch(rctx, bareDir, branch, gitEnv); err != nil {
				s.log.Warn("start rollback: delete branch", "component", "instance", "session", name, "branch", branch, "err", err)
			}
		}
		if rowCreated {
			if err := s.store.DeleteRun(rctx, runID); err != nil {
				s.log.Warn("start rollback: delete run row", "component", "instance", "run", runID, "err", err)
			}
		}
		// Wipe the run's private per-run tree LAST (issues #202/#205): the
		// credential files, dialog settings, provider credential copy, and
		// config all go with the rolled-back run. Wipe is idempotent and
		// logged like every other rollback step so one failure never skips
		// the rest (the boot/runtime sweep is the backstop).
		wipeHome()
	}

	// Seed the worktree: the provider's grants first (trust/MCP,
	// .git/info/exclude), then lab's own seeding (D13, EVERY spawn — the
	// embedded skills bundle into .claude/skills/, the generated
	// CLAUDE.local.md, and their exclude entries). Either failure aborts the
	// launch — nothing stranded.
	if err := spec.Provider.SeedWorkspace(wtPath, seedOpts(repo, home)); err != nil {
		rollback(false)
		return store.Run{}, &StartFailedError{cause: err}
	}
	// The repo's secret INVENTORY (metadata only — names + descriptions, never
	// values; issue #104) flows into the generic seeder so the generated
	// context file can document how to use them. Fetched here rather than
	// inside the seeder because the instance service already holds the store
	// handle every other launch step uses; a fetch failure is treated exactly
	// like the seed calls around it — nothing has been created past the
	// worktree yet, so roll back the same way.
	secrets, err := s.store.RepoSecrets(ctx, repo.ID)
	if err != nil {
		rollback(false)
		return store.Run{}, &StartFailedError{cause: err}
	}
	// The generic seeder consumes the SAME provider's declared shapes (issue
	// #51 decision 8): skills dir, context-file name, exclude entries — plus
	// the repo's secret inventory (issue #104) for the generated Secrets
	// section and this run's materialized read-only imports (issue #261) for
	// the Read-only imports section. An import the agent cannot find is not an
	// import, so the refs carry the name, the absolute snapshot path, and the
	// commit it was taken at.
	//
	// The gateway ref (issue #24) switches that Secrets section to the
	// gateway's — the run holds no secret VALUE, only grants on its repo's
	// agent identity — and carries the granted service names, metadata the
	// context file documents. NIL when the wiring is off, and that nil IS the
	// parity guarantee: a non-nil ref with an empty slice would flip the
	// render to the gateway section on a lab that has no gateway, which is the
	// exact byte-for-byte regression #24's acceptance criterion forbids.
	var gwRef *seeder.GatewayRef
	if gw.active() {
		gwRef = &seeder.GatewayRef{Services: gw.services}
	}
	if err := s.seeder.SeedWorkspace(wtPath, repo, seedMeta, seeder.Opts{Secrets: secrets, Imports: importRefs, Gateway: gwRef}); err != nil {
		rollback(false)
		return store.Run{}, &StartFailedError{cause: err}
	}

	// Install the run's model credentials into its private HOME (issue #202):
	// the provider copies the machine's MASTER credential store into the layout
	// its CLI reads under home, and returns the env the spawn must carry for the
	// CLI to resolve its state there — each adapter pins its CLI's master-store
	// override variable against tmux inheritance (claude:
	// CLAUDE_CONFIG_DIR=<home>/.claude; codex: CODEX_HOME=<home>/.codex).
	// This is the seam's ONLY call
	// site, so a future server-side credential proxy can replace the copy
	// wholesale behind it. A MISSING master credential is NOT an error — the
	// injector logs it and returns nil, and the CLI shows its own login prompt
	// in the pane (today's unauthenticated-host behavior); an error here is a
	// real I/O problem, so it rolls the launch back like the seed steps.
	injEnv, err := spec.Provider.InjectCredentials(home)
	if err != nil {
		rollback(false)
		return store.Run{}, &StartFailedError{cause: err}
	}
	// The POST-inject signature is this run's persisted adopt-check baseline
	// (issue #222): the rotation loop's adopt-scan and every pre-wipe
	// adopt-check compare the home's FUTURE signature against this stamped
	// value to detect a self-refresh. "" — a missing master credential at
	// launch, the not-an-error case just above — stays "never stamped",
	// same as a pre-upgrade row.
	credSig, _ := spec.Provider.CredentialsSig(home)

	run := store.Run{
		ID:             runID,
		RepoID:         repo.ID,
		Kind:           spec.Kind,
		Provider:       spec.Provider.ID(),
		IssueNumber:    spec.IssueNumber,
		PullNumber:     spec.PullNumber,
		ScheduleID:     spec.ScheduleID,
		Branch:         branch,
		WorktreePath:   wtPath,
		SessionName:    name,
		Model:          spec.Model,
		Effort:         spec.Effort,
		Remote:         spec.Remote,
		StartedAt:      s.now(),
		BudgetDeadline: spec.BudgetDeadline,
		Outcome:        store.RunOutcomeActive,
		CredSig:        credSig,
	}
	created, err := s.store.CreateRun(ctx, run)
	if err != nil {
		rollback(false)
		return store.Run{}, err
	}

	// Mint the run token (§3a expiry rule: the spec carries nil for manual
	// runs, budget_deadline+30min for AFK runs).
	token, tokenHash := ids.NewToken("run")
	if err := s.store.CreateRunToken(ctx, runID, tokenHash, spec.TokenExpiry, s.now()); err != nil {
		rollback(true)
		return store.Run{}, err
	}

	extraEnv, err := s.spawnEnv(ctx, repo, credEnv, token, home, injEnv, proxyEnv)
	if err != nil {
		rollback(true)
		return store.Run{}, err
	}

	// Arm live dialog capture (ADR-0020): write the per-run hook settings file
	// under the run's private runtime dir (issue #205) and inject --settings
	// into the spawn argv, so a pending AskUserQuestion/ExitPlanMode spools
	// where the chat can read it (Claude Code never flushes a pending
	// tool_use to the transcript live). Best-effort — a write failure logs
	// and spawns without capture (the chat keeps transcript-only behavior);
	// dialog capture must never block a launch. A provider without the
	// LiveSignals capability contributes nothing.
	//
	// Build the spawn argv from the provider (issue #19 SpawnSpec seam): the
	// seed prompt rides as the trailing positional and the provider applies any
	// provider-owned spawn options to it (e.g. ultracode prepends a directive) —
	// lab never sees the option mechanism. The dialog-capture --settings flag
	// (ADR-0020) must still land among the flags, BEFORE that trailing prompt
	// (claude CLI: `[options] [prompt]`, never after where the parser could
	// swallow it), so inject it before the prompt element when a seed prompt is
	// present, else at the end.
	spawnArgv := spec.Provider.SpawnArgv(provider.SpawnSpec{
		SessionName:   name,
		Model:         spec.Model,
		Effort:        spec.Effort,
		Remote:        spec.Remote,
		Options:       spec.Options,
		InitialPrompt: spec.SeedPrompt,
	})
	if extra, path := s.armDialogHooks(ctx, spec.Provider, runID, spec.Kind, runMat.Dir()); path != "" {
		spawnArgv = injectBeforePrompt(spawnArgv, extra, spec.SeedPrompt != "")
	}

	// Spawn in the worktree (never the reference repo). SeedPrompt (the AFK
	// seed prompt, or a manual run's first chat message per issue #96) rides
	// the spawn argv as claude's trailing positional (v0-pinned) — no
	// post-spawn keystroke, so there is no cold-start TUI race to leave a run
	// unseeded. A spawn failure rolls the whole launch back, releasing an AFK
	// claim.
	//
	// The dual-runner seam (issue #205): host mode is EXACTLY the pre-#205
	// spawn — provider argv, prlimit-wrapped, full spawn env via tmux -e.
	// Container mode wraps the SAME provider argv in `podman run` (the pane
	// command becomes the podman client; tmux stays untouched otherwise):
	// non-secret env rides the podman argv, the secret forwards (LAB_TOKEN)
	// ride tmux -e + name-only --env so no argv ever carries them, and the
	// host prlimit cap is retired in favor of the container's own --ulimit
	// (WithoutNofileCap — capping the podman client would aim at the wrong
	// process).
	var spawnErr error
	if container {
		env, forward := containerEnv(extraEnv, home, s.containerSockURL())
		paneArgv := podmanx.RunArgv(podmanx.RunSpec{
			Bin:         s.podmanBin,
			Name:        podmanx.ContainerName(name),
			Image:       ctrImage,
			ToolsImage:  s.containerToolsImages[spec.Provider.ID()],
			WorktreeDir: wtPath,
			BareDir:     bareDir,
			AgentDir:    s.agentSockDir,
			HomeDir:     home,
			RuntimeDir:  s.homes.RuntimePath(runID),
			// The read-only import snapshots (issue #261): one `:ro` bind each
			// at its host-identical path, in store order (by name), so the
			// path the seeded context file printed is the path inside.
			ImportDirs: importDirs,
			Memory:     ctrMemory,
			Pids:       ctrPids,
			Nofile:     ctrNofile,
			Env:        env,
			ForwardEnv: forward,
			Argv:       spawnArgv,
		})
		spawnErr = s.runner.Start(ctx, name, wtPath, paneArgv, secretForwardEnv(extraEnv, forward), tmuxx.WithoutNofileCap())
	} else {
		spawnErr = s.runner.Start(ctx, name, wtPath, spawnArgv, extraEnv)
	}
	if spawnErr != nil {
		rollback(true)
		return store.Run{}, &StartFailedError{cause: spawnErr}
	}

	// Recency is keyed by repo and stamped BEFORE the capture so the sort is
	// right even if the deep link never lands (error only logged).
	if err := s.store.TouchRepoOpened(ctx, repo.ID, s.now()); err != nil {
		s.log.Warn("stamping repo opened", "component", "instance", "repo", repo.ID, "err", err)
	}
	s.ArmCapture(created)
	s.publishRunChanged(repo.ID, created.ID)
	return created, nil
}

// gatewayWiring is a run's RESOLVED credential-gateway wiring (issue #24 /
// ADR-0067). The zero value means "this lab has no gateway", which is what
// every step of Launch checks through active() — so the unwired path has one
// shape, not one per call site.
type gatewayWiring struct {
	// proxyURL is SECRET-BEARING: it carries the repo's agent-identity token
	// as userinfo (gatewayProxyURL). It must never be logged, never enter an
	// error, and never reach an argv — containerEnv's isProxySecretEnv split
	// is the enforcement on the one path where it could.
	proxyURL string
	// services are the granted service names, for the seeded context file
	// only. Documentation, not capability: the run's access comes from the
	// grants themselves, which live in OneCLI.
	services []string
}

// active reports whether a run is gateway-wired. Keyed on proxyURL because
// that is the field a wired run cannot be missing — an empty service list is
// the normal state of a freshly created agent identity.
func (w gatewayWiring) active() bool { return w.proxyURL != "" }

// prepareGateway resolves a run's credential-gateway wiring, or refuses the
// spawn (issue #24 / ADR-0067's fail-closed pin). Launch calls it BEFORE the
// start guard and long before AddWorktree — see the call site for why the
// ordering is not negotiable.
//
// It lives here, next to the launch sequence it is a step of, rather than in
// gateway.go: that file's header pins it as the PURE core (no Service field,
// no dial, no log) precisely so a table test can own the exact-output
// contracts, and this function is the impure half — it dials the sidecar,
// mints a credential, and logs.
//
// Every refusal is a *BadRequestError → 400, the mapping refuseContainerSpawn
// documents for operator-fixable spawn refusals: what is wrong here is always
// the deployment (no CA file, a sidecar that is down, an API key that lost its
// project), the operator's own text is the actionable part, and rendering that
// as a 500 would file a host problem as a lab fault. Messages name the REPO,
// because ADR-0067's refusal is supposed to say which run it is refusing and
// whose secrets are missing — and quoting the onecli error verbatim is safe by
// that package's construction: its errors carry method, escaped path and
// status, never the base URL and never the key (see internal/onecli's do).
//
// The one step that does NOT fail closed is the grant listing; its reason is
// at the call below.
func (s *Service) prepareGateway(ctx context.Context, repo store.Repo) (gatewayWiring, error) {
	if !s.gatewayActive() {
		return gatewayWiring{}, nil
	}
	// No CA, no gateway. A run pointed at a TLS-terminating proxy whose
	// interception CA it does not trust has broken HTTPS in a way nothing
	// inside the run can diagnose: every proxied request fails certificate
	// verification, and the agent reads that as the world being down rather
	// than as lab having handed it half a configuration. Refuse where the flag
	// that fixes it can be named.
	if s.oneCLICAFile == "" {
		return gatewayWiring{}, badRequestf("onecli gateway: --onecli-gateway-url is set but --onecli-ca-file is not, so a run for repo %s could not verify the gateway's intercepted TLS — set --onecli-ca-file to the sidecar's interception CA certificate (PEM) on this host", repo.Name)
	}
	// ADR-0067's fail-closed pin, landing at the spawn: a run that starts
	// without credential injection does not fail cleanly, it fails minutes in
	// as 401s from services the agent is certain it was granted, and burns its
	// budget clock on a permissions bug that does not exist. The probe's own
	// message already names the address and what to check.
	if err := onecli.ProbeGateway(ctx, s.oneCLIGatewayURL); err != nil {
		return gatewayWiring{}, badRequestf("refusing to spawn for repo %s without credential-gateway access: %s", repo.Name, err)
	}
	// The agent identity is named by the repo's STORE ID, never its name.
	// Grants are attached to the agent (ADR-0067: the grant set IS the
	// per-repo secret assignment), so the name a run authenticates under has
	// to survive a repo rename — keying on the name would silently create a
	// second, grant-less agent the first time an operator renamed a repo, and
	// the symptom would be "my secrets vanished" with nothing pointing at the
	// rename. EnsureAgent is idempotent and 409-tolerant, so calling it on
	// every spawn from every kind of run is the whole mapping.
	agent, err := s.onecli.EnsureAgent(ctx, repo.ID)
	if err != nil {
		return gatewayWiring{}, badRequestf("onecli gateway: resolving the agent identity for repo %s: %s", repo.Name, err)
	}
	token, err := s.proxyToken(ctx, agent.ID)
	if err != nil {
		return gatewayWiring{}, badRequestf("onecli gateway: obtaining the proxy token for repo %s's agent identity: %s", repo.Name, err)
	}
	proxyURL, err := gatewayProxyURL(s.oneCLIGatewayURL, token)
	if err != nil {
		return gatewayWiring{}, badRequestf("%s", err)
	}

	// Grants: BEST EFFORT, the one step here that does not fail closed, and
	// the asymmetry is the point rather than an oversight. The grant list is
	// DOCUMENTATION — it renders the context file's inventory of what this
	// repo may reach. A run that reached the gateway and holds a valid token
	// has exactly the secret access its grants describe whether or not lab
	// could render the inventory, so refusing here would trade a working run
	// for a missing paragraph. Documentation must never be the reason a run
	// refuses to start. An empty list is not a failure at all: a freshly
	// created agent identity has no grants yet, and #25's picker is how it
	// gets them.
	var services []string
	grants, err := s.onecli.ListGrants(ctx, agent.ID)
	if err != nil {
		s.log.Warn("listing onecli grants for the run's context file; seeding it without the service inventory",
			"component", "instance", "repo", repo.ID, "agent", agent.ID, "err", err)
	} else {
		services = grantServiceNames(grants)
	}

	// One line, and deliberately three IDs and a count: enough to correlate a
	// run with the identity it authenticated as, and nothing that could ever
	// carry a credential. NEVER the token, NEVER the proxy URL (it contains
	// the token), NEVER the assembled env bundle.
	s.log.Info("credential gateway wired for run", "component", "instance",
		"repo", repo.ID, "agent", agent.ID, "services", len(services))
	return gatewayWiring{proxyURL: proxyURL, services: services}, nil
}

// proxyToken returns the proxy token for OneCLI agent identity agentID,
// minting it AT MOST ONCE per lab process (see the proxyTokens field for why
// that is a correctness property and not a cache optimization:
// onecli.AgentToken is a POST that regenerates, so a second mint silently
// invalidates every already-running run of the same repo).
//
// The lock is held ACROSS the mint, on purpose. The obvious "check, unlock,
// mint, relock, store" shape would let two concurrent spawns of the same repo
// — the AFK engine and a manual Start racing, which is an ordinary Tuesday —
// both mint, and the loser's token would be dead on arrival. Since the whole
// point is that exactly one mint ever happens per agent, the critical section
// has to contain the call. The cost is that a cold mint serializes concurrent
// spawns of OTHER repos behind one HTTP round trip; a per-agent lock map would
// avoid that and buy nothing worth the extra state, because the warm path —
// every spawn after the first for a given repo — takes the lock, reads a map,
// and returns without any I/O at all.
func (s *Service) proxyToken(ctx context.Context, agentID string) (string, error) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	if tok, ok := s.proxyTokens[agentID]; ok {
		return tok, nil
	}
	minted, err := s.onecli.AgentToken(ctx, agentID)
	if err != nil {
		return "", err
	}
	if s.proxyTokens == nil {
		s.proxyTokens = make(map[string]string, 1)
	}
	s.proxyTokens[agentID] = minted.Token
	return minted.Token, nil
}

// grantServiceNames renders an agent identity's grants as the service names
// the seeded context file lists: the grant's Name when it has one, else its
// ID, so a grant whose display name upstream never filled in still appears as
// something an operator can look up rather than vanishing from the inventory.
//
// SORTED, because the seeder renders them in caller order and a run's context
// file should be identical across spawns of the same repo — OneCLI's listing
// order is not promised anywhere, and a file that reshuffles between two runs
// of the same repo is a diff an operator has to read and discard every time.
func grantServiceNames(grants []onecli.Grant) []string {
	if len(grants) == 0 {
		return nil
	}
	names := make([]string, 0, len(grants))
	for _, g := range grants {
		if g.Name != "" {
			names = append(names, g.Name)
			continue
		}
		names = append(names, g.ID)
	}
	slices.Sort(names)
	return names
}

// materializeImports materializes every read-only import the repo declares
// (issue #261 / ADR-0063) into this run's private imports dir and returns the
// seeder's inventory (name, absolute path, short commit — ordered by name,
// which is the store's order) plus the snapshot directories in the same order
// for the container runner's `:ro` binds. An import-less repo — the common
// case — costs one indexed query and returns (nil, nil, nil), so nothing about
// a launch changes for it.
//
// Every failure refuses the launch, shaped as `read-only import "<target>":
// <cause>`: the caller cannot tell WHICH sibling broke from gitx's error
// (which only ever knew a directory), and "which target, what to fix" is the
// whole point of the message. Two things must therefore happen before any work
// starts: the not-clone-ready check runs over ALL targets first, so a
// misconfiguration refuses without firing a single fetch, and only then do the
// fetches run — one goroutine per target, because spawn latency should be the
// slowest target's fetch, not the sum of all of them. A plain WaitGroup over
// an indexed result slice is the whole concurrency story (no shared writes, no
// errgroup dependency); the first error IN TARGET ORDER wins, so a two-target
// failure names the same target on every run.
func (s *Service) materializeImports(ctx context.Context, repo store.Repo, runID string, runMat *vault.Materializer) ([]seeder.ImportRef, []string, error) {
	targets, err := s.store.RepoImports(ctx, repo.ID)
	if err != nil {
		return nil, nil, err
	}
	if len(targets) == 0 {
		return nil, nil, nil
	}
	// A target whose own clone has not finished has no reference repo to
	// export from. Refused up front — before any goroutine, any fetch, and
	// any file — because this one is a state problem, not an outage: the
	// operator either waits for that repo's clone or drops the import.
	for _, t := range targets {
		if t.CloneStatus != store.CloneStatusReady {
			return nil, nil, fmt.Errorf("read-only import %q: the imported repository is not ready (clone status %q) — wait for its clone to finish, or remove the import from this repo's settings", t.Name, t.CloneStatus)
		}
	}

	refs := make([]seeder.ImportRef, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs[i], errs[i] = s.materializeImport(ctx, t, runID, runMat)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, nil, fmt.Errorf("read-only import %q: %w", targets[i].Name, err)
		}
	}

	// Host runner only: a host pane has no mount namespace, so read-only-ness
	// there is advisory (ADR-0063) — strip the write bits and log on failure,
	// never refuse. What it buys is that the same path holds the same content
	// under both runners, so nothing an agent or a generated context file
	// knows about an import is runner-specific. Under the container runner the
	// snapshots stay writable HOST-side on purpose: the `:ro` bind is the
	// enforcement, and /pull-base must be able to re-materialize in place.
	if repo.Runner != store.RunnerContainer {
		for _, ref := range refs {
			if err := protectSnapshot(ref.Path); err != nil {
				s.log.Warn("write-protecting import snapshot", "component", "instance",
					"run", runID, "import", ref.Name, "err", err)
			}
		}
	}

	dirs := make([]string, len(refs))
	for i, ref := range refs {
		dirs[i] = ref.Path
	}
	return refs, dirs, nil
}

// materializeImport materializes ONE import target: the target's own vault
// credential (that is what makes the feature credential-free — no second
// acquisition path, no new credential kind), then gitx's fetch → resolve →
// clear-and-extract into <state>/instances/<runID>/imports/<target name>, then
// the sidecar recording the snapshotted commit. Errors travel up bare; the
// caller names the target.
//
// The sidecar <name>.commit is a SIBLING of the snapshot directory, not a file
// inside it: the directory is a byte-faithful export of the target's tree
// (mounted read-only, write-protected on the host runner), so lab's own
// bookkeeping must not appear inside it — an agent reading the import would
// see a file the imported repo does not have. /pull-base reads it back to
// report what each snapshot moved from.
func (s *Service) materializeImport(ctx context.Context, target store.Repo, runID string, runMat *vault.Materializer) (seeder.ImportRef, error) {
	credEnv, cleanup, err := s.importCredentialEnv(ctx, target, runID, runMat)
	if err != nil {
		return seeder.ImportRef{}, err
	}
	// The import credential is needed for THIS fetch only — unlike the run's
	// own credential, no spawned process ever uses it — so it goes as soon as
	// the fetch is done rather than living inside the container for the whole
	// session.
	defer cleanup()

	dest := filepath.Join(s.homes.ImportsPath(runID), target.Name)
	gitEnv := append(append([]string{}, s.gitEnv...), credEnv...)
	commit, err := s.git.MaterializeSnapshot(ctx, s.bareDir(target.ID), dest, target.DefaultBranch, gitEnv)
	if err != nil {
		return seeder.ImportRef{}, err
	}
	if err := os.WriteFile(dest+".commit", []byte(commit+"\n"), 0o600); err != nil {
		return seeder.ImportRef{}, fmt.Errorf("record snapshot commit: %w", err)
	}
	return seeder.ImportRef{Name: target.Name, Path: dest, Commit: shortCommit(commit)}, nil
}

// importCredentialEnv materializes an import TARGET's git credential into the
// launching run's runtime dir and returns the git env plus the cleanup that
// removes exactly those files. The op id is (run, target)-unique rather than
// the plain run id every other credential materialization in this file uses,
// because a run can materialize several credentials at once here: vault keys
// its files by (credID, opID), so two imports sharing one credential would
// otherwise write the same filenames and the first cleanup would unlink the
// second's live key mid-fetch. A credential-less target yields no env and a
// no-op cleanup (a public target over https needs none).
func (s *Service) importCredentialEnv(ctx context.Context, target store.Repo, runID string, runMat *vault.Materializer) ([]string, func(), error) {
	noop := func() {}
	opID := runID + "-import-" + target.ID
	cleanup := noop
	if target.CredentialID != nil {
		credID := *target.CredentialID
		cleanup = func() {
			if err := runMat.Cleanup(credID, opID); err != nil {
				s.log.Warn("cleaning import credential", "component", "instance",
					"run", runID, "import", target.Name, "err", err)
			}
		}
	}
	env, err := s.credentialEnv(ctx, target, opID, runMat)
	if err != nil {
		cleanup() // a partial materialization leaves nothing behind
		return nil, noop, err
	}
	return env, cleanup, nil
}

// protectSnapshot strips the write bits from a materialized snapshot tree
// (dirs → 0555, files → their mode minus 0222, symlinks skipped so the chmod
// never follows one out of the snapshot). BEST-EFFORT by design, and only on
// the host runner: ADR-0063 makes read-only-ness advisory there because a host
// pane has no mount namespace and that runner is the labeled "unsandboxed —
// full host access" break-glass, whose agent can already read and write every
// repo on the box. Refusing a launch over a failed chmod would trade a real
// spawn for an enforcement gap that runner does not close anyway; the caller
// logs and continues. Directories keep r-x, so the walk can still descend
// after chmod'ing a directory on the way in — and instancehome.Wipe restores
// the write bits when the tree is removed.
func protectSnapshot(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, info.Mode().Perm()&^0o222)
	})
}

// shortCommit is the 12-char short hash the context file prints for an import
// snapshot — long enough to be unambiguous, short enough to read. Anything
// already shorter (never a real commit id) passes through untouched.
func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

// injectBeforePrompt places the dialog-capture --settings flag among the spawn
// flags: before the trailing prompt positional when a seed prompt is present
// (hasPrompt — the provider appended it as the last argv element, possibly
// option-transformed), else at the end. Keeps claude's `[options] [prompt]`
// order intact so the parser never swallows --settings as prompt text (ADR-0020;
// mirrors the pre-issue-#19 flags-first construction).
func injectBeforePrompt(argv, extra []string, hasPrompt bool) []string {
	if len(extra) == 0 {
		return argv
	}
	if !hasPrompt {
		return append(argv, extra...)
	}
	n := len(argv) - 1 // the trailing prompt positional
	out := make([]string, 0, len(argv)+len(extra))
	out = append(out, argv[:n]...)
	out = append(out, extra...)
	return append(out, argv[n])
}

// armDialogHooks writes the per-run dialog-capture settings file (ADR-0020)
// into runtimeDir — the run's PRIVATE runtime dir (issue #205), never the
// global one — and returns the spawn args to append (--settings <path>) plus
// the settings path (the caller's armed/not-armed signal; the file itself
// needs no rollback bookkeeping — it dies with the per-run tree). The run
// kind resolves the dialog auto-dismiss window folded into the settings
// payload (issue #124, via dialogTimeout). A provider without the
// LiveSignals capability contributes nothing. Best-effort: a write failure
// logs and returns no args — the run still spawns, its chat just keeps
// transcript-only behavior.
func (s *Service) armDialogHooks(ctx context.Context, prov provider.AgentProvider, runID, kind, runtimeDir string) (args []string, settingsPath string) {
	signals, ok := prov.(provider.LiveSignals)
	if !ok {
		return nil, ""
	}
	opts := provider.SetupOpts{DialogTimeout: s.dialogTimeout(ctx, kind)}
	settings, path, extra := signals.Setup(runID, runtimeDir, opts)
	if err := writeFileAtomic0600(path, settings); err != nil {
		s.log.Warn("arming dialog hooks", "component", "instance", "run", runID, "err", err)
		return nil, ""
	}
	return extra, path
}

// dialogTimeout resolves the run's unattended-dialog auto-dismiss window
// (issue #124), the local instance policy behind SetupOpts.DialogTimeout.
// Manual runs get the dialog_timeout_minutes setting with never-by-default:
// absent or 0 means "effectively never" (≈24.8 days), defeating the CLI's own
// auto-dismiss for an attended session; N>0 means N minutes. AFK runs return
// zero — pass nothing, keep the CLI's default — because unattended
// auto-advance is a feature there. Values above the adapter's cap pass
// through untouched: the adapter clamps, and the 2^31−1 rationale lives with
// its clamp constant (claudecode.maxDialogTimeout). Best-effort like the rest
// of dialog arming: a settings read failure logs and falls back to
// effectively-never.
func (s *Service) dialogTimeout(ctx context.Context, kind string) time.Duration {
	if kind != store.RunKindManual {
		return 0
	}
	// Raw GetInt with a 0 default: 0 is a meaningful stored value (never),
	// never a hole to re-default.
	minutes, err := s.store.GetInt(ctx, store.SettingDialogTimeoutMinutes, 0)
	if err != nil {
		s.log.Warn("reading dialog timeout", "component", "instance", "err", err)
		minutes = 0
	}
	if minutes <= 0 {
		return time.Duration(1<<31-1) * time.Millisecond // effectively never
	}
	return time.Duration(minutes) * time.Minute
}

// writeFileAtomic0600 writes data to path via a temp sibling + rename (0600), so
// a reader never sees a half-written file. The per-run runtime dir already
// exists (Launch materialized it 0700).
func writeFileAtomic0600(path string, data []byte) error {
	// A temp sibling orphaned by a crash between CreateTemp and rename needs
	// no dedicated GC (the pre-#205 SweepSpools coupling): it lives inside
	// the run's per-run runtime dir, so it is wiped with the run's tree at
	// stop/rollback, or reaped by instancehome.SweepAll once the tree
	// orphans.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings.tmp-*")
	if err != nil {
		return fmt.Errorf("tmpfile: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
