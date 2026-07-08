package crmerge

import (
	"testing"
	"time"
)

// keyedMutex is the per-CR merge/close serializer (ADR-0011: a close landing
// inside a merge's git window strands origin merged while the row reads
// closed-unmerged). Pin its two properties: same key excludes, different keys
// do not.
func TestKeyedMutex(t *testing.T) {
	var km keyedMutex
	unlockA := km.lock("a")
	// Different key: never blocks.
	done := make(chan struct{})
	go func() {
		unlock := km.lock("b")
		unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lock(b) blocked behind lock(a)")
	}
	// Same key: blocks until release.
	acquired := make(chan struct{})
	go func() {
		unlock := km.lock("a")
		unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second lock(a) acquired while held")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock(a) never acquired after release")
	}
}
