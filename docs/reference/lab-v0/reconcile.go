package main

import (
	"log"
	"strings"
)

// reconcile.go is lab's worktree/branch cleanup (ADR-0017 slice 2). Two callers,
// one guarded rule:
//
//   - reconcileWorktrees runs once at startup, before the schedulers, to mop up
//     after a crash/restart: every orphan worktree (a lab//afk/ worktree with no
//     live session) gets the guarded teardown, and every merged lab//afk/ branch
//     left behind with no worktree is deleted.
//   - sweepMergedWorktrees runs periodically, piggybacked on the reaper but
//     throttled (afkSweepInterval): after a best-effort fetch it GCs merged lab//
//     afk/ branches and their clean worktrees — finally collecting the merged
//     afk/<N> claim branches lab used to keep forever. It is deliberately
//     merged-only and never touches a dirty or unmerged branch/worktree.
//
// Both route every worktree decision through teardownGuarded / decideTeardown
// (handlers.go), so there is exactly one rule for "what happens to a worktree +
// branch" across Stop, the reaper, startup, and the sweep.

// managedBranch reports whether branch is one lab created for an instance — a
// lab/<label> manual branch or an afk/<N> run branch (ADR-0017). Reconciliation and
// the sweep act only on these, never on a human's own branch or the reference
// repo's default-branch checkout (whose worktree carries no managed branch).
func managedBranch(branch string) bool {
	return strings.HasPrefix(branch, labBranchPrefix) || strings.HasPrefix(branch, afkBranchPrefix)
}

// ownedBranches is the set of branches a live session of project still occupies —
// the branches/worktrees reconciliation must never tear down. It derives each live
// instance's branch exactly as Start does (instanceBranch over the parsed session
// name), so a just-started instance whose branch is trivially an ancestor of
// origin/<default> — it forked there and has no commits yet, so BranchMerged reads
// "merged" — is protected by being OWNED, not by the merged-check. That ownership
// guard, not the merged-check, is what keeps the sweep from deleting a live run's
// branch. The afk/<N> ambiguity (a manual afk-<N> and an auto afk-auto-<N> both map
// to afk/<N>) is harmless: both collapse to the one branch this set must protect.
func ownedBranches(live []string, project string) map[string]bool {
	owned := map[string]bool{}
	for _, name := range live {
		if name == loginSession || !belongsTo(name, project) {
			continue
		}
		owned[instanceBranch(parseSessionName(name))] = true
	}
	return owned
}

// reconcileRefs is the per-project view the startup reconcile and the runtime sweep
// both weigh: the managed (lab//afk/) worktrees, an index of them by branch, every
// managed branch name, and the branches a live session owns.
type reconcileRefs struct {
	worktrees  []Worktree        // managed worktrees, in git's (deterministic) order
	wtByBranch map[string]string // branch -> worktree path, managed only
	branches   []string          // all lab//afk/ branch names
	owned      map[string]bool   // branches a live instance occupies — never touched
}

// gatherRefs reads a project's reconcileRefs. Ordering is load-bearing for
// race-safety against a concurrent Start: branches are listed FIRST, worktrees
// next, then the starting set, and the live session set LAST. The sweep is
// branch-driven, so a branch created after the branches read is invisible to it
// (never wrongly GC'd). Any branch that IS considered is checked against `owned`,
// the union of two owner sources read after the enumeration:
//   - sessions mid-Start (startingSnapshot) — their worktree exists but their tmux
//     session is not live yet; without this an in-flight Start's merged, clean
//     worktree (a fresh branch reads "merged") could be GC'd out from under it;
//   - live sessions.
//
// `starting` is read BEFORE `live` so the union is continuous across a Start: a
// session leaves `starting` (clearStarting) only after its tmux session is up, so a
// session missing from the earlier `starting` read is guaranteed present in the
// later `live` read. A per-step error logs and aborts this project (ok false),
// never the whole pass.
func (s *Server) gatherRefs(repoDir, project string) (reconcileRefs, bool) {
	branches, err := s.git.Branches(repoDir, labBranchPrefix, afkBranchPrefix)
	if err != nil {
		log.Printf("reconcile %s: list branches: %v", project, err)
		return reconcileRefs{}, false
	}
	wts, err := s.git.Worktrees(repoDir)
	if err != nil {
		log.Printf("reconcile %s: list worktrees: %v", project, err)
		return reconcileRefs{}, false
	}
	starting := s.startingSnapshot()
	live, err := s.sessions.List()
	if err != nil {
		log.Printf("reconcile %s: list sessions: %v", project, err)
		return reconcileRefs{}, false
	}
	refs := reconcileRefs{
		wtByBranch: map[string]string{},
		branches:   branches,
		owned:      ownedBranches(append(starting, live...), project),
	}
	for _, wt := range wts {
		if managedBranch(wt.Branch) {
			refs.worktrees = append(refs.worktrees, wt)
			refs.wtByBranch[wt.Branch] = wt.Path
		}
	}
	return refs, true
}

// reconcileWorktrees is the startup pass (ADR-0017): for every project, tear down
// orphan worktrees and GC merged dangling branches left by a previous lab lifetime.
// Runs synchronously before the schedulers start, so no concurrent Start can race
// it. A nil git seam (AFK/worktree features unwired) is a no-op.
func (s *Server) reconcileWorktrees() {
	if s.git == nil {
		return
	}
	projects, err := Scan(s.root)
	if err != nil {
		log.Printf("reconcile: scan: %v", err)
		return
	}
	for _, p := range projects {
		s.reconcileProject(p.Path, p.Name)
	}
}

// reconcileProject is the startup reconcile for one project. Pass A applies the
// guarded teardown to every orphan worktree (a managed worktree with no live
// session): a clean orphan's worktree is removed and its branch deleted iff merged,
// a dirty one is kept whole. Pass B deletes every managed branch that is merged and
// has no worktree — the dangling afk/<N> claim branch a finished run leaves behind.
// A managed branch that still has a worktree was handled by Pass A; an unmerged
// dangling branch is kept so its commits survive.
func (s *Server) reconcileProject(repoDir, project string) {
	refs, ok := s.gatherRefs(repoDir, project)
	if !ok {
		return
	}
	// Pass A: orphan worktrees → guarded teardown. git's order keeps it deterministic.
	for _, wt := range refs.worktrees {
		if refs.owned[wt.Branch] {
			continue
		}
		s.teardownGuarded(repoDir, wt.Path, wt.Branch)
	}
	// Pass B: bare merged branches → delete.
	for _, b := range refs.branches {
		if refs.owned[b] {
			continue
		}
		if _, hasWorktree := refs.wtByBranch[b]; hasWorktree {
			continue // handled by Pass A
		}
		merged, err := s.git.BranchMerged(repoDir, b)
		if err != nil {
			log.Printf("reconcile %s: merged check %s: %v (keeping branch)", project, b, err)
			continue
		}
		if merged {
			if err := s.git.DeleteBranch(repoDir, b); err != nil {
				log.Printf("reconcile %s: delete merged branch %s: %v", project, b, err)
			}
		}
	}
}

// sweepMergedWorktrees is the throttled runtime pass (ADR-0017), driven by the
// reaper loop. For each project it fetches (best-effort, so the merged-check sees
// current mainline) then GCs merged managed branches and their clean worktrees.
// A nil git seam is a no-op (the reaper loop that calls it is already gated on it).
func (s *Server) sweepMergedWorktrees() {
	if s.git == nil {
		return
	}
	projects, err := Scan(s.root)
	if err != nil {
		log.Printf("sweep: scan: %v", err)
		return
	}
	for _, p := range projects {
		// Best-effort: a fetch failure (offline / no origin) just leaves the
		// merged-check reading the last-known origin ref; the sweep still runs,
		// conservatively keeping anything it can't prove merged.
		if err := s.git.Fetch(p.Path); err != nil {
			log.Printf("sweep %s: fetch: %v (using last-known origin)", p.Name, err)
		}
		s.sweepProject(p.Path, p.Name)
	}
}

// sweepProject is the runtime merged-sweep for one project: merged-only, and never
// touching a dirty or unmerged branch/worktree. For every managed branch not owned
// by a live session and merged into origin/<default>, it removes a clean worktree
// (the guarded teardown keeps a dirty one) and deletes the branch; a merged branch
// with no worktree is just deleted. Unmerged branches, and branches whose merged
// status can't be read, are left for the next sweep or the next startup.
func (s *Server) sweepProject(repoDir, project string) {
	refs, ok := s.gatherRefs(repoDir, project)
	if !ok {
		return
	}
	for _, b := range refs.branches {
		if refs.owned[b] {
			continue // a live instance — never touch
		}
		merged, err := s.git.BranchMerged(repoDir, b)
		if err != nil {
			log.Printf("sweep %s: merged check %s: %v (keeping)", project, b, err)
			continue
		}
		if !merged {
			continue // runtime sweep is merged-only; never touch unmerged
		}
		if wt, hasWorktree := refs.wtByBranch[b]; hasWorktree {
			// Route through the one guarded teardown: it re-checks dirty and keeps a
			// dirty worktree + branch whole, else removes the clean worktree and
			// deletes the (merged) branch.
			s.teardownGuarded(repoDir, wt, b)
		} else if err := s.git.DeleteBranch(repoDir, b); err != nil {
			log.Printf("sweep %s: delete merged branch %s: %v", project, b, err)
		}
	}
}
