package afk

import (
	"context"
	"errors"
	"fmt"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// Engine sentinels the API layer maps onto status codes.
var (
	// ErrNoReady: the claimable queue is empty — nothing to start. NOT an
	// infrastructure error (v0's no-ready notice); API → 409 with exactly
	// this message.
	ErrNoReady = errors.New("no ready-for-agent issues to start")
	// ErrRepoPaused: the repo hit the three-strikes pause
	// (consecutive_failures >= PauseThreshold); a manual AFK start is
	// refused until a human POST reset (or a success reap) re-arms it.
	// API → 409. (D12 delta from v0, where a manual start bypassed the
	// pause — the M5 contract pins the 409.)
	ErrRepoPaused = errors.New("repository is paused after consecutive AFK failures — reset to re-arm")
)

// TrackerError wraps a tracker round-trip failure inside the engine so the
// API layer can answer 502 Bad Gateway (the forge, not lab, misbehaved).
type TrackerError struct{ cause error }

func (e *TrackerError) Error() string { return e.cause.Error() }
func (e *TrackerError) Unwrap() error { return e.cause }

// launchOutcome is the result of a launch attempt that returned no error
// (v0 afkLaunch, verbatim semantics): a run was spawned, the claimable queue
// was empty, lab was at its cap, or (auto only) the repo already had an auto
// run in flight.
type launchOutcome int

const (
	launchSpawned      launchOutcome = iota // a seeded run was claimed and spawned
	launchNoReady                           // empty claimable queue — nothing claimed
	launchAtCap                             // live-instance cap reached — nothing launched
	launchAutoInFlight                      // an auto run for this repo is already live (auto only)
)

// StartManualAFK claims and launches the lowest claimable ready-for-agent
// issue of a repo NOW — the operator POST /repos/{id}/afk/start entry point.
// Typed errors: store.ErrNotFound → 404, instance.ErrRepoNotReady /
// ErrRepoPaused / instance.ErrLoggedOut / instance.ErrOverCap / ErrNoReady →
// 409, *TrackerError → 502, anything else → 500. Manual AFK runs are
// additive — never gated on an auto run being in flight — and never touch
// the failure counter.
func (s *Service) StartManualAFK(ctx context.Context, repoID string) (store.Run, error) {
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return store.Run{}, err // ErrNotFound → 404
	}
	if repo.CloneStatus != store.CloneStatusReady {
		return store.Run{}, instance.ErrRepoNotReady
	}
	if repo.ConsecutiveFailures >= PauseThreshold {
		return store.Run{}, ErrRepoPaused
	}
	prov, ok := s.providers.Get(repo.Provider)
	if !ok {
		return store.Run{}, fmt.Errorf("repository provider %q is not registered", repo.Provider)
	}
	// FORCE the auth refresh — never a cached status: a spawn while logged
	// out strands a doomed remote-control session that reaps as a death,
	// parking the issue for nothing (v0-pinned, per click).
	if st, _ := prov.AuthStatus(ctx, true); !st.LoggedIn {
		return store.Run{}, instance.ErrLoggedOut
	}

	run, outcome, err := s.launch(ctx, repoID, false)
	if err != nil {
		return store.Run{}, err
	}
	switch outcome {
	case launchNoReady:
		return store.Run{}, ErrNoReady
	case launchAtCap:
		return store.Run{}, instance.ErrOverCap
	}
	return run, nil
}

// launch is the shared select→cap-check→claim→spawn core both the manual
// start and the auto scheduler drive, so there is exactly ONE claim path —
// race-freedom rests on it being single-flighted under s.mu (v0
// launchAFKRun). It holds the lock across the whole select-through-spawn:
// two concurrent launches can never claim the same issue, and an auto launch
// re-checks one-auto-run-per-repo HERE, against fresh state, not just in the
// scheduler's pre-tick snapshot.
//
// The claim is the creation of the isolated worktree on the repo's rendered
// claim branch (ADR-0013) — durable on disk, restart-safe, invisible to
// triage. instance.Launch owns claim creation and its rollback: any failure
// before a live seeded session tears the worktree and branch back down,
// releasing the claim so the issue is selectable again.
func (s *Service) launch(ctx context.Context, repoID string, auto bool) (store.Run, launchOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read the repo under the lock: branch pattern, budget, caps, and the
	// toggle must be current, not a caller snapshot.
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return store.Run{}, launchSpawned, err
	}
	if repo.CloneStatus != store.CloneStatusReady {
		return store.Run{}, launchSpawned, instance.ErrRepoNotReady
	}
	prov, ok := s.providers.Get(repo.Provider)
	if !ok {
		return store.Run{}, launchSpawned, fmt.Errorf("repository provider %q is not registered", repo.Provider)
	}
	trk, err := s.trackers.TrackerFor(ctx, repo)
	if err != nil {
		return store.Run{}, launchSpawned, err
	}

	// Pick before touching tmux or git: an empty queue is a no-op, not a
	// spawn and not a claim.
	issues, err := trk.ReadyIssues(ctx)
	if err != nil {
		return store.Run{}, launchSpawned, &TrackerError{cause: err}
	}
	if len(issues) == 0 {
		return store.Run{}, launchNoReady, nil
	}
	// Exclude any issue already claimed by a local claim branch (ADR-0013):
	// a parked/failed run's surviving branch — or a run a concurrent launch
	// just claimed — keeps its issue out of selection with no tracker label.
	// This read under s.mu is the authoritative claim check; the scheduler's
	// pre-tick one is only a hint.
	claimed, err := s.claimedIssueSet(ctx, repo)
	if err != nil {
		return store.Run{}, launchSpawned, err
	}
	issue, ok := PickLowestIssue(ClaimableIssues(issues, claimed))
	if !ok {
		return store.Run{}, launchNoReady, nil
	}

	// Serial per repo for AUTO runs: refuse a second auto run when one is
	// already in flight. Authoritative here, under the lock (the scheduler's
	// pre-tick check is an optimisation). Manual runs skip it — they are
	// additive, bounded only by the cap (ADR-0007).
	if auto {
		if _, inFlight, err := s.store.ActiveAutoRunForRepo(ctx, repoID); err != nil {
			return store.Run{}, launchSpawned, err
		} else if inFlight {
			return store.Run{}, launchAutoInFlight, nil
		}
	}

	// Cap guard on fresh liveness. Nothing is claimed yet, so at-cap is a
	// pure no-op.
	live, err := s.runner.List(ctx)
	if err != nil {
		return store.Run{}, launchSpawned, err
	}
	if instance.LiveInstanceCount(live) >= s.instances.EffectiveCap(ctx, repo) {
		return store.Run{}, launchAtCap, nil
	}

	model, effort, err := s.instances.ResolveModelEffort(ctx, prov, repo, "", "")
	if err != nil {
		return store.Run{}, launchSpawned, err
	}

	// An AFK run's identity: label afk-<N> / afk-auto-<N>, session
	// <repoName>~<label>, branch = the repo's afk_branch_pattern rendered
	// with N (NEVER a literal prefix — D15.4), worktree <repoName>-<N>. The
	// budget clock is persisted on the run row (D12b) and the run token dies
	// 30 minutes after it (§3a).
	n := issue.Number
	branch := gitx.RenderBranch(repo.AFKBranchPattern, n)
	deadline := s.now().Add(s.effectiveBudget(ctx, repo))
	tokenExpiry := deadline.Add(runTokenSlack)
	kind := store.RunKindAFKManual
	if auto {
		kind = store.RunKindAFKAuto
	}

	run, err := s.instances.Launch(ctx, instance.LaunchSpec{
		Repo:           repo,
		Provider:       prov,
		Kind:           kind,
		IssueNumber:    &n,
		SessionName:    gitx.ComposeSessionName(repo.Name, Label(n, auto)),
		Branch:         branch,
		WorktreePath:   s.worktreePath(repo.Name, n),
		Model:          model,
		Effort:         effort,
		BudgetDeadline: &deadline,
		TokenExpiry:    &tokenExpiry,
		SeedPrompt:     SeedPrompt(n, branch, repo.Incogni),
	})
	if err != nil {
		return store.Run{}, launchSpawned, err
	}
	return run, launchSpawned, nil
}

// claimedIssueSet reads the repo's claim branches from the bare reference
// clone (one for-each-ref under the pattern's prefix glob — design §4a) and
// strict-inverse-parses them into the claimed issue set.
func (s *Service) claimedIssueSet(ctx context.Context, repo store.Repo) (map[int]bool, error) {
	branches, err := s.git.Branches(ctx, s.bareDir(repo.ID), s.gitEnv, gitx.PatternPrefix(repo.AFKBranchPattern))
	if err != nil {
		return nil, err
	}
	return ClaimedIssues(branches, repo.AFKBranchPattern), nil
}

// FilterClaimable narrows an ALREADY-FETCHED ready queue to the currently
// claimable set (ready issues minus the repo's claim branches), reading only
// the bare clone's claim refs — no tracker round-trip. It is the
// GET /repos/{id}/ready hint source when the caller already holds the ready
// list (the operator handler fetches it once for the {issues} envelope), so the
// count and the list derive from one snapshot. Unlocked and best-effort by
// design, exactly like ClaimableIssuesFor.
func (s *Service) FilterClaimable(ctx context.Context, repo store.Repo, ready []tracker.Issue) ([]tracker.Issue, error) {
	claimed, err := s.claimedIssueSet(ctx, repo)
	if err != nil {
		return nil, err
	}
	return ClaimableIssues(ready, claimed), nil
}

// ClaimableIssuesFor returns a repo's currently claimable ready queue (open
// ready-for-agent issues minus claimed branches) — the scheduler's pre-tick
// hint. It fetches the ready queue then defers to FilterClaimable. Unlocked and
// best-effort by design: the count is stale the moment it renders; the
// authoritative check is remade inside the locked claim path when a run
// actually starts.
func (s *Service) ClaimableIssuesFor(ctx context.Context, repo store.Repo) ([]tracker.Issue, error) {
	trk, err := s.trackers.TrackerFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	issues, err := trk.ReadyIssues(ctx)
	if err != nil {
		return nil, &TrackerError{cause: err}
	}
	return s.FilterClaimable(ctx, repo, issues)
}
