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

	Kind        string // store.RunKindManual | RunKindAFKManual | RunKindAFKAuto
	IssueNumber *int   // AFK runs: the claimed issue; manual: nil

	SessionName  string
	Branch       string
	WorktreePath string
	Model        string
	Effort       string

	// BudgetDeadline is the persisted budget clock (D12b; AFK runs only) and
	// TokenExpiry the run token's expires_at (§3a: AFK → budget_deadline +
	// 30min; manual → nil, no wall clock).
	BudgetDeadline *time.Time
	TokenExpiry    *time.Time

	// SeedPrompt, when non-empty, is the AFK seed prompt. It is carried into
	// the spawn as claude's trailing positional argument (provider.SpawnArgv),
	// the pinned v0 mechanism — so the prompt exists before the process and is
	// never lost to the cold-start TUI race that a post-spawn keystroke would
	// hit. Manual runs leave it "" (no trailing argument).
	SeedPrompt string
}

// Launch runs the v0-pinned spawn sequence for a fully-derived spec:
// startguard.Mark → credential materialization → gitx.AddWorktree (fail-loud
// fetch, no fallback base) → workspace seeding (provider trust/MCP, then
// lab's skills bundle + CLAUDE.local.md) → runs row + run token → tmux
// spawn (AFK seed prompt carried as the spawn argv's trailing positional) →
// StampOpened → async deep-link capture → run.changed.
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
	// files (the run row/token do not exist yet).
	if err := s.git.AddWorktree(ctx, bareDir, wtPath, branch, repo.DefaultBranch, gitEnv); err != nil {
		credCleanup()
		return store.Run{}, &StartFailedError{cause: err}
	}

	// dialogSettingsPath is the per-run dialog-capture settings file (ADR-0020),
	// written just before the spawn; rollback unlinks it. "" until armed (or
	// for a provider without the DialogHooker capability).
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
		if err := s.git.DeleteBranch(rctx, bareDir, branch, gitEnv); err != nil {
			s.log.Warn("start rollback: delete branch", "component", "instance", "session", name, "branch", branch, "err", err)
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
		credCleanup()
	}

	// Seed the worktree: the provider's grants first (trust/MCP,
	// .git/info/exclude), then lab's own seeding (D13, EVERY spawn — the
	// embedded skills bundle into .claude/skills/, the generated
	// CLAUDE.local.md, and their exclude entries). Either failure aborts the
	// launch — nothing stranded.
	if err := spec.Provider.SeedWorkspace(wtPath, seedOpts(repo)); err != nil {
		rollback(false)
		return store.Run{}, &StartFailedError{cause: err}
	}
	if err := s.seeder.SeedWorkspace(wtPath, repo, seeder.Opts{}); err != nil {
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

	extraEnv, err := s.spawnEnv(ctx, repo, credEnv, token)
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
	// provider without the DialogHooker capability contributes nothing.
	//
	// The flag must precede the trailing prompt positional (claude CLI:
	// `claude [options] [prompt]`), so build the argv flags-first with an empty
	// prompt, inject --settings among the flags, then append the seed prompt
	// last — never after --settings, where the parser could swallow it.
	spawnArgv := spec.Provider.SpawnArgv(name, spec.Model, spec.Effort, "")
	if extra, path := s.armDialogHooks(spec.Provider, runID); path != "" {
		dialogSettingsPath = path
		spawnArgv = append(spawnArgv, extra...)
	}
	if spec.SeedPrompt != "" {
		spawnArgv = append(spawnArgv, spec.SeedPrompt)
	}

	// Spawn in the worktree (never the reference repo), prlimit-wrapped by the
	// runner. The AFK seed prompt (spec.SeedPrompt) rides the spawn argv as
	// claude's trailing positional (v0-pinned) — no post-spawn keystroke, so
	// there is no cold-start TUI race to leave a run unseeded. A spawn failure
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
	s.publishRunChanged(repo.ID)
	return created, nil
}

// armDialogHooks writes the per-run dialog-capture settings file (ADR-0020) into
// the runtime dir and returns the spawn args to append (--settings <path>) plus
// the settings path (for rollback). A provider without the DialogHooker
// capability contributes nothing. Best-effort: a write failure logs and returns
// no args — the run still spawns, its chat just keeps transcript-only behavior.
func (s *Service) armDialogHooks(prov provider.AgentProvider, runID string) (args []string, settingsPath string) {
	hooker, ok := prov.(provider.DialogHooker)
	if !ok {
		return nil, ""
	}
	settings, path, extra := hooker.HookSettings(runID, s.mat.Dir())
	if err := writeFileAtomic0600(path, settings); err != nil {
		s.log.Warn("arming dialog hooks", "component", "instance", "run", runID, "err", err)
		return nil, ""
	}
	return extra, path
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
