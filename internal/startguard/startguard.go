// Package startguard owns the starting-set race guard (design §4b): the
// single mutex-guarded set of session names whose worktree + branch
// already exist but whose tmux session is not yet live. One Guard is
// injected into instance/, afk/, and reconcile/ so the sweep can't read
// a mid-Start worktree as a merged clean orphan and remove it.
//
// Invariants (v0-pinned, from lab-v0 handlers.go markStarting/
// clearStarting/startingSnapshot):
//
//   - Mark BEFORE the worktree is created (gitx.AddWorktree);
//   - Clear only after the session is live OR the rollback is complete;
//   - reconcile/sweep read Snapshot() STRICTLY BEFORE SessionRunner.List,
//     so the owner union is continuous across a Start: a name leaves the
//     starting set only once its tmux session is up, so a name missing
//     from the earlier Snapshot read is guaranteed present in the later
//     live read.
//
// A fresh fork reads clean+merged — ownership via this guard, not the
// merged/dirty check, is the only protection mid-Start.
package startguard

import (
	"sort"
	"sync"
)

// Guard is the starting set. The zero value is not usable; construct
// with New. Safe for concurrent use.
type Guard struct {
	mu       sync.Mutex
	starting map[string]bool
}

// New returns an empty Guard.
func New() *Guard {
	return &Guard{starting: map[string]bool{}}
}

// Mark records a session as mid-Start — its worktree + branch are about
// to exist (call BEFORE AddWorktree) but its tmux session is not yet
// live. Idempotent.
func (g *Guard) Mark(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.starting[name] = true
}

// Clear drops a session from the starting set — once it is live (so the
// live-session read covers it) or its Start has rolled back (so there is
// nothing left to protect). Clearing an unmarked name is a no-op.
func (g *Guard) Clear(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.starting, name)
}

// Has reports whether name is currently in the starting set — its
// worktree + branch exist but its tmux session is not yet live (or its
// Start is mid-rollback). The afk reaper consults this under its runsMu
// before classifying a run: a mid-Launch session (row active, session
// not yet spawned) must not be misread as a death and torn down. Clear
// fires only once the session is live or the rollback is complete, so a
// name that leaves the set is thereafter either genuinely reap-able or
// no longer a candidate.
func (g *Guard) Has(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.starting[name]
}

// Snapshot returns the current starting set as a sorted slice. The
// result is an isolated copy: later Mark/Clear calls, and mutation of
// the returned slice, leave each other untouched. Callers union it with
// the live session list — reading Snapshot strictly first (see the
// package invariants).
func (g *Guard) Snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	names := make([]string, 0, len(g.starting))
	for n := range g.starting {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
