package podmanx

import "sync/atomic"

// Gate is the startup preflight's atomic result holder (issue #205): cmd/lab
// runs Preflight in a boot goroutine (an unresolved tools ref means a pull —
// possibly minutes) and Sets the result once; every container spawn consults
// Result through the instance/afk wiring and refuses with "retry in a
// moment" while nothing is published yet. A pointer swap over sync/atomic,
// not a mutex: the readers sit on the spawn path and must never block behind
// a still-running preflight, and the writer publishes exactly one immutable
// Result value (the pointee is never mutated after Set).
type Gate struct {
	v atomic.Pointer[Result]
}

// Set publishes a finished preflight result. Last write wins — cmd/lab's
// retry loop (issue #220) Sets once per preflight round while a tools-image
// pull failure persists, and spawns atomically see the newer verdict.
func (g *Gate) Set(r Result) { g.v.Store(&r) }

// Result returns the published result and whether one has been published
// yet. (Result{}, false) is the "preflight still running" state the spawn
// refusal distinguishes from a finished-and-failed result.
func (g *Gate) Result() (Result, bool) {
	p := g.v.Load()
	if p == nil {
		return Result{}, false
	}
	return *p, true
}
