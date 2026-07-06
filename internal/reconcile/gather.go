package reconcile

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// refs is one project's reconciliation input.
type refs struct {
	branches   []string          // managed branches, git's deterministic order
	worktrees  []gitx.Worktree   // managed worktrees, git's order
	wtByBranch map[string]string // managed branch → worktree path
	owned      map[string]bool   // branches a live/mid-Start session occupies
}

// gatherRefs reads a project's refs in the ONE load-bearing order (design §3.1
// / port-spec §3.1): branches FIRST (the sweep is branch-driven, so a branch
// created after this read is invisible → never wrongly GC'd), then worktrees,
// then the starting snapshot, then live sessions LAST. Reading `starting`
// strictly before `live` makes the owner union continuous across a Start: a
// name leaves the starting set only after its tmux session is up, so a name
// missing from the earlier snapshot is guaranteed present in the later live
// read. Any step's error logs and aborts THIS project only (ok=false), never
// the whole pass.
func (s *Service) gatherRefs(ctx context.Context, repo store.Repo) (refs, bool) {
	bareDir := s.bareDir(repo.ID)
	branches, err := s.git.ManagedBranches(ctx, bareDir, repo.AFKBranchPattern, repo.ManualBranchPrefix, s.gitEnv)
	if err != nil {
		s.log.Warn("reconcile: list branches", "component", "reconcile", "repo", repo.Name, "err", err)
		return refs{}, false
	}
	wts, err := s.git.Worktrees(ctx, bareDir, s.gitEnv)
	if err != nil {
		s.log.Warn("reconcile: list worktrees", "component", "reconcile", "repo", repo.Name, "err", err)
		return refs{}, false
	}
	starting := s.guard.Snapshot()
	live, err := s.runner.List(ctx)
	if err != nil {
		s.log.Warn("reconcile: list sessions", "component", "reconcile", "repo", repo.Name, "err", err)
		return refs{}, false
	}

	owned := ownedBranches(repo.AFKBranchPattern, repo.ManualBranchPrefix, append(starting, live...), repo.Name)

	// An OPEN change request's head branch is owned too (builtin repos): the
	// branch is the CR's reviewable substance — the diff view reads it, and
	// the crash-recovery retry of an unrecorded merge needs it. Without this,
	// a merged-but-still-open head (merge pushed, MergeCR not yet recorded, or
	// merged outside lab) would be GC'd by pass B/the sweep and the CR would
	// be stranded reviewless. Merged/closed CRs release their heads to GC.
	if repo.TrackerBinding == store.TrackerBindingBuiltin {
		open, err := s.store.CRsByRepo(ctx, repo.ID, store.CRStateOpen)
		if err != nil {
			s.log.Warn("reconcile: list open CRs", "component", "reconcile", "repo", repo.Name, "err", err)
			return refs{}, false // conservative: no GC without the full owned set
		}
		for _, cr := range open {
			owned[cr.HeadBranch] = true
		}
	}

	r := refs{owned: owned, wtByBranch: map[string]string{}}
	r.branches = branches
	for _, wt := range wts {
		if wt.Branch != "" && gitx.ManagedBranch(repo.AFKBranchPattern, repo.ManualBranchPrefix, wt.Branch) {
			r.worktrees = append(r.worktrees, wt)
			r.wtByBranch[wt.Branch] = wt.Path
		}
	}
	return r, true
}

// reconcileProject is the startup pass for one project (port-spec §2.4):
//   - Pass A — orphan worktrees: every managed worktree whose branch is NOT
//     owned gets the guarded teardown (dirty → keep both; clean+unmerged →
//     remove worktree keep branch; clean+merged → remove both).
//   - Pass B — bare merged branches: every managed branch that is not owned and
//     has no worktree (a branch that had one was Pass A's) is deleted IFF merged
//     into origin/<default>. Unmerged dangling branches are kept (commits
//     survive) — this is the GC of the merged claim branch a finished run
//     leaves behind.
func (s *Service) reconcileProject(ctx context.Context, repo store.Repo) {
	r, ok := s.gatherRefs(ctx, repo)
	if !ok {
		return
	}
	bareDir := s.bareDir(repo.ID)

	for _, wt := range r.worktrees {
		if r.owned[wt.Branch] {
			continue
		}
		s.git.TeardownGuarded(ctx, s.log, bareDir, wt.Path, wt.Branch, repo.DefaultBranch, s.gitEnv)
	}

	for _, b := range r.branches {
		if r.owned[b] {
			continue
		}
		if _, hasWorktree := r.wtByBranch[b]; hasWorktree {
			continue // Pass A handled it (also keeps a dirty orphan's branch alive)
		}
		merged, err := s.git.BranchMerged(ctx, bareDir, b, repo.DefaultBranch, s.gitEnv)
		if err != nil {
			s.log.Warn("reconcile: merged check, keeping branch", "component", "reconcile", "repo", repo.Name, "branch", b, "err", err)
			continue
		}
		if !merged {
			continue
		}
		if err := s.git.DeleteBranch(ctx, bareDir, b, s.gitEnv); err != nil {
			s.log.Warn("reconcile: delete merged branch", "component", "reconcile", "repo", repo.Name, "branch", b, "err", err)
		}
	}
}

// sweepProject is the runtime pass for one project (port-spec §2.5). It differs
// from reconcileProject in EXACTLY one way: it is merged-ONLY. A clean UNMERGED
// orphan worktree is left (startup Pass A reclaims it) — the runtime sweep never
// touches unmerged work. Owned branches are never touched (the mid-Start guard,
// not the merged check, is what protects a fresh fork that trivially reads
// merged).
func (s *Service) sweepProject(ctx context.Context, repo store.Repo) {
	r, ok := s.gatherRefs(ctx, repo)
	if !ok {
		return
	}
	bareDir := s.bareDir(repo.ID)

	for _, b := range r.branches {
		if r.owned[b] {
			continue
		}
		merged, err := s.git.BranchMerged(ctx, bareDir, b, repo.DefaultBranch, s.gitEnv)
		if err != nil {
			s.log.Warn("sweep: merged check, keeping", "component", "reconcile", "repo", repo.Name, "branch", b, "err", err)
			continue
		}
		if !merged {
			continue // merged-only — startup reclaims a clean unmerged orphan, not the sweep
		}
		if wt, hasWorktree := r.wtByBranch[b]; hasWorktree {
			s.git.TeardownGuarded(ctx, s.log, bareDir, wt, b, repo.DefaultBranch, s.gitEnv)
		} else if err := s.git.DeleteBranch(ctx, bareDir, b, s.gitEnv); err != nil {
			s.log.Warn("sweep: delete merged branch", "component", "reconcile", "repo", repo.Name, "branch", b, "err", err)
		}
	}
}
