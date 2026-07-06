package startguard

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestGuard_markClearSnapshot(t *testing.T) {
	g := New()

	if snap := g.Snapshot(); len(snap) != 0 {
		t.Fatalf("fresh Snapshot = %v; want empty", snap)
	}

	g.Mark("repo~20260608-1530")
	g.Mark("repo~afk-7")
	g.Mark("repo~afk-7") // idempotent
	if snap := g.Snapshot(); !slices.Equal(snap, []string{"repo~20260608-1530", "repo~afk-7"}) {
		t.Errorf("Snapshot = %v; want both names, sorted", snap)
	}

	g.Clear("repo~afk-7")
	if snap := g.Snapshot(); !slices.Equal(snap, []string{"repo~20260608-1530"}) {
		t.Errorf("Snapshot after Clear = %v; want [repo~20260608-1530]", snap)
	}

	// Clearing an unmarked name is a no-op.
	g.Clear("never-marked")
	if snap := g.Snapshot(); !slices.Equal(snap, []string{"repo~20260608-1530"}) {
		t.Errorf("Snapshot after no-op Clear = %v", snap)
	}

	g.Clear("repo~20260608-1530")
	if snap := g.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot after clearing all = %v; want empty", snap)
	}
}

// Has is the per-name reader the afk reaper uses to skip a mid-Launch
// session: true exactly while the name is marked, false before Mark and
// after Clear.
func TestGuard_has(t *testing.T) {
	g := New()

	if g.Has("repo~afk-7") {
		t.Errorf("Has on unmarked name = true; want false")
	}
	g.Mark("repo~afk-7")
	if !g.Has("repo~afk-7") {
		t.Errorf("Has after Mark = false; want true")
	}
	if g.Has("repo~afk-8") {
		t.Errorf("Has on a different, unmarked name = true; want false")
	}
	g.Clear("repo~afk-7")
	if g.Has("repo~afk-7") {
		t.Errorf("Has after Clear = true; want false")
	}
}

// Snapshot must be an isolated copy in both directions: later Mark/Clear
// calls don't mutate an already-taken snapshot, and writing into the
// returned slice doesn't corrupt the guard. The sweep holds its snapshot
// across git queries while Starts proceed concurrently — a shared
// backing array would tear the owner union.
func TestGuard_snapshotIsolation(t *testing.T) {
	g := New()
	g.Mark("a")

	snap := g.Snapshot()
	if !slices.Equal(snap, []string{"a"}) {
		t.Fatalf("Snapshot = %v; want [a]", snap)
	}

	// Later guard changes leave the earlier snapshot untouched.
	g.Mark("b")
	g.Clear("a")
	if !slices.Equal(snap, []string{"a"}) {
		t.Errorf("earlier snapshot mutated by later Mark/Clear: %v", snap)
	}

	// Mutating the returned slice leaves the guard untouched.
	snap[0] = "mangled"
	if got := g.Snapshot(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("guard corrupted by snapshot mutation: %v", got)
	}
}

// Concurrent Mark/Clear/Snapshot from many goroutines — the race
// detector is the assertion here (run with -race).
func TestGuard_concurrent(t *testing.T) {
	g := New()
	var wg sync.WaitGroup
	for i := range 32 {
		name := fmt.Sprintf("repo~s-%d", i)
		wg.Add(3)
		go func() { defer wg.Done(); g.Mark(name) }()
		go func() { defer wg.Done(); _ = g.Snapshot() }()
		go func() { defer wg.Done(); g.Clear(name) }()
	}
	wg.Wait()

	// Whatever interleaving happened, the guard must still be coherent:
	// re-mark everything, then clear everything, ending empty.
	for i := range 32 {
		g.Mark(fmt.Sprintf("repo~s-%d", i))
	}
	if snap := g.Snapshot(); len(snap) != 32 {
		t.Errorf("after re-mark: %d names; want 32", len(snap))
	}
	for i := range 32 {
		g.Clear(fmt.Sprintf("repo~s-%d", i))
	}
	if snap := g.Snapshot(); len(snap) != 0 {
		t.Errorf("after clear-all: %v; want empty", snap)
	}
}
