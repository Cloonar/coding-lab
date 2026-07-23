package instance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
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

	Kind        string // store.RunKindManual | RunKindAFKManual | RunKindAFKAuto | RunKindLander
	IssueNumber *int   // AFK/lander runs: the claimed issue; manual: nil

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

// Launch runs the v0-pinned spawn sequence for a fully-derived spec:
// startguard.Mark → credential materialization → gitx.AddWorktree (fail-loud
// fetch, no fallback base) → workspace seeding (provider trust/MCP, then
// lab's skills bundle + CLAUDE.local.md) → runs row + run token → tmux
// spawn (SeedPrompt, when set, carried as the spawn argv's trailing
// positional — the AFK seed prompt or a manual run's first chat message,
// issue #96) → StampOpened → async deep-link capture → run.changed.
// Any failure after worktree creation rolls back to the exact pre-launch
// state (session kill, RemoveWorktree, force DeleteBranch, run row/token
// delete, credential cleanup); a failure before worktree creation rolls back
// nothing but the credential files. For an AFK spec the worktree/branch
// creation IS the claim, so the rollback is what releases a claimed issue
// back into the selectable queue.
func (s *Service) Launch(ctx context.Context, spec LaunchSpec) (store.Run, error) {
	repo := spec.Repo
	name := spec.SessionName
	branch := spec.Branch
	wtPath := spec.WorktreePath
	bareDir := s.bareDir(repo.ID)
	runID := ids.NewID("run")

	// Sweep guard spans worktree-creation → session-live (§4b). Cleared on
	// every return via defer (success or rollback), matching v0's
	// markStarting/defer clearStarting.
	s.guard.Mark(name)
	defer s.guard.Clear(name)

	// Materialize the repo's GIT credential (opID = run id) for the worktree
	// fetch AND the spawned session's git env. Kept alive for the session on
	// success; cleaned only on rollback here / at Stop or reap.
	credEnv, credCleanup, err := s.credentialEnv(ctx, repo, runID)
	if err != nil {
		return store.Run{}, &StartFailedError{cause: err}
	}
	gitEnv := append(append([]string{}, s.gitEnv...), credEnv...)

	// AddWorktree: fail-loud fetch → fork from origin/<default>, NO fallback
	// base. A failure here created nothing — roll back only the credential
	// files (the run row/token do not exist yet). An adopt-branch spec (the
	// lander) checks out the EXISTING branch instead, aligned to what the
	// forge sees.
	addWorktree := func() error {
		if spec.AdoptBranch {
			return s.git.AddWorktreeExisting(ctx, bareDir, wtPath, branch, gitEnv)
		}
		return s.git.AddWorktree(ctx, bareDir, wtPath, branch, repo.DefaultBranch, gitEnv)
	}
	if err := addWorktree(); err != nil {
		credCleanup()
		return store.Run{}, &StartFailedError{cause: err}
	}

	// dialogSettingsPath is the per-run dialog-capture settings file (ADR-0020),
	// written just before the spawn; rollback unlinks it. "" until armed (or
	// for a provider without the LiveSignals capability).
	var dialogSettingsPath string

	// rollback restores the exact pre-launch state after the worktree exists:
	// RemoveWorktree + force DeleteBranch (both attempted, each logged) + the
	// run row/token (when created) + the credential files + the dialog
	// settings file. Runs on a detached context so a client disconnect can't
	// strand a half-built instance.
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
		if dialogSettingsPath != "" {
			if err := os.Remove(dialogSettingsPath); err != nil && !os.IsNotExist(err) {
				s.log.Warn("start rollback: remove dialog settings", "component", "instance", "run", runID, "err", err)
			}
		}
		// Wipe the run's private instance HOME (issue #202): the credential copy
		// and config the injector/seeder wrote go with the rolled-back run. Wipe
		// is idempotent, so it is harmless on the paths where Materialize never
		// ran (a failure before it) and removes the tree on the paths where it
		// did — logged like every other rollback step so one failure never skips
		// the rest (the boot/runtime sweep is the backstop).
		if err := s.homes.Wipe(runID); err != nil {
			s.log.Warn("start rollback: wipe instance home", "component", "instance", "run", runID, "err", err)
		}
		credCleanup()
	}

	// The run's private instance HOME (issue #202): materialized right after the
	// worktree add so a failure here still rolls back the worktree, and BEFORE
	// seeding/injection so the provider's HOME-global grants and its credential
	// copy land under it. A materialize failure is a real I/O problem — abort.
	home, err := s.homes.Materialize(runID)
	if err != nil {
		rollback(false)
		return store.Run{}, &StartFailedError{cause: err}
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
	// section.
	if err := s.seeder.SeedWorkspace(wtPath, repo, spec.Provider.SeedMeta(), seeder.Opts{Secrets: secrets}); err != nil {
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

	run := store.Run{
		ID:             runID,
		RepoID:         repo.ID,
		Kind:           spec.Kind,
		Provider:       spec.Provider.ID(),
		IssueNumber:    spec.IssueNumber,
		Branch:         branch,
		WorktreePath:   wtPath,
		SessionName:    name,
		Model:          spec.Model,
		Effort:         spec.Effort,
		Remote:         spec.Remote,
		StartedAt:      s.now(),
		BudgetDeadline: spec.BudgetDeadline,
		Outcome:        store.RunOutcomeActive,
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

	extraEnv, err := s.spawnEnv(ctx, repo, credEnv, token, home, injEnv)
	if err != nil {
		rollback(true)
		return store.Run{}, err
	}

	// Arm live dialog capture (ADR-0020): write the per-run hook settings file
	// under runtime/ and inject --settings into the spawn argv, so a pending
	// AskUserQuestion/ExitPlanMode spools where the chat can read it (Claude
	// Code never flushes a pending tool_use to the transcript live). Best-
	// effort — a write failure logs and spawns without capture (the chat keeps
	// transcript-only behavior); dialog capture must never block a launch. A
	// provider without the LiveSignals capability contributes nothing.
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
	if extra, path := s.armDialogHooks(ctx, spec.Provider, runID, spec.Kind); path != "" {
		dialogSettingsPath = path
		spawnArgv = injectBeforePrompt(spawnArgv, extra, spec.SeedPrompt != "")
	}

	// Spawn in the worktree (never the reference repo), prlimit-wrapped by the
	// runner. SeedPrompt (the AFK seed prompt, or a manual run's first chat
	// message per issue #96) rides the spawn argv as claude's trailing
	// positional (v0-pinned) — no post-spawn keystroke, so there is no
	// cold-start TUI race to leave a run unseeded. A spawn failure
	// rolls the whole launch back, releasing an AFK claim.
	if err := s.runner.Start(ctx, name, wtPath, spawnArgv, extraEnv); err != nil {
		rollback(true)
		return store.Run{}, &StartFailedError{cause: err}
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

// armDialogHooks writes the per-run dialog-capture settings file (ADR-0020) into
// the runtime dir and returns the spawn args to append (--settings <path>) plus
// the settings path (for rollback). The run kind resolves the dialog
// auto-dismiss window folded into the settings payload (issue #124, via
// dialogTimeout). A provider without the LiveSignals capability contributes
// nothing. Best-effort: a write failure logs and returns no args — the run
// still spawns, its chat just keeps transcript-only behavior.
func (s *Service) armDialogHooks(ctx context.Context, prov provider.AgentProvider, runID, kind string) (args []string, settingsPath string) {
	signals, ok := prov.(provider.LiveSignals)
	if !ok {
		return nil, ""
	}
	opts := provider.SetupOpts{DialogTimeout: s.dialogTimeout(ctx, kind)}
	settings, path, extra := signals.Setup(runID, s.mat.Dir(), opts)
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
// a reader never sees a half-written file. The runtime dir already exists (the
// materializer created it 0700).
func writeFileAtomic0600(path string, data []byte) error {
	// The ".settings.tmp-" prefix must match claudecode.settingsTempPrefix so a
	// crash-orphaned temp is reaped by the provider's SweepSpools GC (the two
	// live in different packages — instance must not import a concrete provider,
	// ADR-0017 — so the prefix is a documented coupling, not a shared constant).
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
