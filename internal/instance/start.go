package instance

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// StartParams is the manual-instance start input (pinned API: POST
// /api/v1/repos/{id}/instances {label?, model?, effort?}).
type StartParams struct {
	RepoID string
	Label  string // optional user label; sanitized + timestamped into the instance label
	Model  string // optional per-spawn override; "" → repo/settings default
	Effort string // optional per-spawn override; "" → repo/settings default
}

// Start spawns a manual instance following the v0-pinned sequence with full
// rollback (package doc). It returns the created run on success, or a typed
// error the API maps to a status code: ErrRepoNotReady/ErrOverCap/ErrLoggedOut
// → 409, BadRequestError → 400, StartFailedError (git/spawn cause verbatim) →
// 500, store.ErrNotFound (unknown repo) → 404.
func (s *Service) Start(ctx context.Context, p StartParams) (store.Run, error) {
	repo, err := s.store.RepoByID(ctx, p.RepoID)
	if err != nil {
		return store.Run{}, err // ErrNotFound → 404
	}
	if repo.CloneStatus != store.CloneStatusReady {
		return store.Run{}, ErrRepoNotReady
	}
	prov, ok := s.providers.Get(repo.Provider)
	if !ok {
		return store.Run{}, badRequestf("repository provider %q is not registered", repo.Provider)
	}

	model, effort, err := s.resolveModelEffort(ctx, prov, repo, p.Model, p.Effort)
	if err != nil {
		return store.Run{}, err // BadRequestError → 400 (or a store error)
	}

	// One tmux listing serves both the cap check and the label-collision set
	// (v0: a direct POST must not exceed the cap even though the UI disables
	// the button).
	live, err := s.runner.List(ctx)
	if err != nil {
		return store.Run{}, err
	}
	if liveInstanceCount(live) >= s.effectiveCap(ctx, repo) {
		return store.Run{}, ErrOverCap
	}

	// FORCE the auth refresh — never the 30s render cache: a spawn while logged
	// out strands a doomed remote-control session (v0-pinned).
	if st, _ := prov.AuthStatus(ctx, true); !st.LoggedIn {
		return store.Run{}, ErrLoggedOut
	}

	taken := make(map[string]bool, len(live))
	for _, name := range live {
		taken[name] = true
	}
	label := gitx.UniqueManualLabel(repo.Name, gitx.SanitizeLabel(p.Label), s.now(), taken)
	name := gitx.ComposeSessionName(repo.Name, label)
	branch := repo.ManualBranchPrefix + label
	wtPath := s.worktreePath(repo.Name, label)
	bareDir := s.bareDir(repo.ID)
	runID := ids.NewID("run")

	// Sweep guard spans worktree-creation → session-live (§4b). Cleared on
	// every return via defer (success or rollback), matching v0's
	// markStarting/defer clearStarting.
	s.guard.Mark(name)
	defer s.guard.Clear(name)

	// Materialize the repo's GIT credential (opID = run id) for the worktree
	// fetch AND the spawned session's git env. Kept alive for the session on
	// success; cleaned only on rollback here / at Stop.
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

	// rollback restores the exact pre-Start state after the worktree exists:
	// RemoveWorktree + force DeleteBranch (both attempted, each logged) + the
	// run row/token (when created) + the credential files. Runs on a detached
	// context so a client disconnect can't strand a half-built instance.
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
		credCleanup()
	}

	// Seed the worktree (trust/MCP grants, .git/info/exclude). A failure aborts
	// the Start — nothing stranded.
	if err := prov.SeedWorkspace(wtPath, seedOpts()); err != nil {
		rollback(false)
		return store.Run{}, &StartFailedError{cause: err}
	}

	run := store.Run{
		ID:           runID,
		RepoID:       repo.ID,
		Kind:         store.RunKindManual,
		Provider:     prov.ID(),
		Branch:       branch,
		WorktreePath: wtPath,
		SessionName:  name,
		Model:        model,
		Effort:       effort,
		StartedAt:    s.now(),
		Outcome:      store.RunOutcomeActive,
	}
	created, err := s.store.CreateRun(ctx, run)
	if err != nil {
		rollback(false)
		return store.Run{}, err
	}

	// Mint the run token — manual runs have NO wall-clock expiry (§3a).
	token, tokenHash := ids.NewToken("run")
	if err := s.store.CreateRunToken(ctx, runID, tokenHash, nil, s.now()); err != nil {
		rollback(true)
		return store.Run{}, err
	}

	extraEnv, err := s.spawnEnv(ctx, repo, credEnv, token)
	if err != nil {
		rollback(true)
		return store.Run{}, err
	}

	// Spawn in the worktree (never the reference repo), prlimit-wrapped by the
	// runner. A spawn failure rolls the whole Start back.
	if err := s.runner.Start(ctx, name, wtPath, prov.SpawnArgv(name, model, effort), extraEnv); err != nil {
		rollback(true)
		return store.Run{}, &StartFailedError{cause: err}
	}

	// Recency is keyed by repo and stamped BEFORE the capture so the sort is
	// right even if the deep link never lands (error only logged).
	if err := s.store.TouchRepoOpened(ctx, repo.ID, s.now()); err != nil {
		s.log.Warn("stamping repo opened", "component", "instance", "repo", repo.ID, "err", err)
	}
	go s.runCapture(created)
	s.publishRunChanged(repo.ID)
	return created, nil
}

// effectiveCap is the live-instance cap for a start on repo: the repo's
// max_instances_override when set, else the global settings max_instances.
// A missing/blank/garbled setting falls back to the config default (6).
func (s *Service) effectiveCap(ctx context.Context, repo store.Repo) int {
	if repo.MaxInstancesOverride != nil {
		return *repo.MaxInstancesOverride
	}
	n, err := s.store.GetInt(ctx, store.SettingMaxInstances, defaultMaxInstances)
	if err != nil {
		s.log.Warn("reading max_instances setting; using default", "component", "instance", "err", err)
		return defaultMaxInstances
	}
	return n
}

// spawnEnv assembles a session's extra environment (design §3a/§6/M3 contract):
// the repo credential's git env + LAB_URL + LAB_TOKEN + the git author/committer
// identity when configured. The forge token is never included (§3a).
func (s *Service) spawnEnv(ctx context.Context, repo store.Repo, credEnv []string, runToken string) ([]string, error) {
	env := append([]string{}, credEnv...)
	env = append(env, "LAB_URL="+s.labURL, "LAB_TOKEN="+runToken)
	author, err := s.authorEnv(ctx, repo)
	if err != nil {
		return nil, err
	}
	return append(env, author...), nil
}

// defaultMaxInstances mirrors config.DefaultMaxInstances (6) without a service
// → config import edge. It is only a last-resort fallback: production always
// seeds the max_instances setting (from --max-instances), so this is reached
// only if that row is missing or unparseable.
const defaultMaxInstances = 6

// seedOpts is the M3 (empty) SeedOpts; a named helper so the M7 incogni wiring
// has one call site to grow.
func seedOpts() provider.SeedOpts { return provider.SeedOpts{} }
