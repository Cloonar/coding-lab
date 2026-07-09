package provider

import (
	"fmt"
	"regexp"
	"slices"
)

// Registry resolves a provider id (the repos.provider column) to its
// AgentProvider. Registration happens once at wiring time; the registry is
// immutable afterwards, so reads need no locking. The union scrub/seeded-path
// slices and the compiled scrub regexps (issue #75 / ADR-0033) are
// precomputed once here, not on every UnionScrubPatterns/ScrubRegexps call.
type Registry struct {
	order []string
	byID  map[string]AgentProvider

	// scrubPatterns and seededPathPatterns are the precomputed cross-provider
	// unions (registration order, declaration order, first-occurrence-wins
	// dedup) backing UnionScrubPatterns/UnionSeededPathPatterns. scrubRegexps
	// is scrubPatterns compiled once via CompileScrubPatterns, backing
	// ScrubRegexps.
	scrubPatterns      []string
	seededPathPatterns []string
	scrubRegexps       []*regexp.Regexp
}

// NewRegistry builds a registry from providers, preserving their order for
// List (the /api/v1/providers rendering order). Empty and duplicate ids are
// wiring bugs and error out.
//
// It also precomputes the cross-provider unions of SeedMeta().ScrubPatterns
// and SeedMeta().SeededPathPatterns (registration order, declaration order,
// first-occurrence-wins dedup on exact string match) and compiles the scrub
// union once via CompileScrubPatterns (issue #75 / ADR-0033: both incogni
// enforcement points — the pre-push hook and the agent-API body sanitizer —
// apply this union, not just the resolving repo's own provider, because a
// per-session provider override (ADR-0030) can run any registered provider on
// any repo). If any provider's declared ScrubPatterns fail to compile as RE2
// (they have drifted out of the BRE∩RE2 common dialect), NewRegistry returns
// an error naming the provider id and the offending pattern — a marker that
// cannot be enforced at the body-sanitizer point is a wiring bug caught here
// at boot, not a silent gap discovered in production.
func NewRegistry(providers ...AgentProvider) (*Registry, error) {
	r := &Registry{byID: make(map[string]AgentProvider, len(providers))}
	scrubSeen := make(map[string]bool)
	pathSeen := make(map[string]bool)
	for _, p := range providers {
		id := p.ID()
		if id == "" {
			return nil, fmt.Errorf("provider registry: empty provider id (%T)", p)
		}
		if _, dup := r.byID[id]; dup {
			return nil, fmt.Errorf("provider registry: duplicate provider id %q", id)
		}
		r.byID[id] = p
		r.order = append(r.order, id)

		meta := p.SeedMeta()
		if _, err := CompileScrubPatterns(meta.ScrubPatterns); err != nil {
			return nil, fmt.Errorf("provider registry: provider %q: %w", id, err)
		}
		for _, pat := range meta.ScrubPatterns {
			if !scrubSeen[pat] {
				scrubSeen[pat] = true
				r.scrubPatterns = append(r.scrubPatterns, pat)
			}
		}
		for _, pat := range meta.SeededPathPatterns {
			if !pathSeen[pat] {
				pathSeen[pat] = true
				r.seededPathPatterns = append(r.seededPathPatterns, pat)
			}
		}
	}
	// Every pattern folded into scrubPatterns above already compiled
	// successfully per-provider, so compiling the deduped union cannot fail.
	regexps, err := CompileScrubPatterns(r.scrubPatterns)
	if err != nil {
		return nil, fmt.Errorf("provider registry: %w", err)
	}
	r.scrubRegexps = regexps
	return r, nil
}

// UnionScrubPatterns returns the union of every registered provider's
// SeedMeta().ScrubPatterns: providers in registration order, patterns in
// declaration order, exact-string duplicates dropped (first occurrence
// wins). Both incogni enforcement points apply this union rather than the
// resolving repo's own provider alone (ADR-0030's per-session override can
// run any registered provider on any repo, so a marker only one OTHER
// provider declares must still be caught — ADR-0033, issue #75). The
// returned slice is a clone of the registry's internal state.
func (r *Registry) UnionScrubPatterns() []string {
	return slices.Clone(r.scrubPatterns)
}

// UnionSeededPathPatterns returns the union of every registered provider's
// SeedMeta().SeededPathPatterns, with the same registration-order,
// declaration-order, first-occurrence-wins dedup as UnionScrubPatterns and
// for the same ADR-0030/ADR-0033 reason: the pre-push guard must recognize
// paths ANY registered provider seeds, not just the resolving repo's own. The
// returned slice is a clone of the registry's internal state.
func (r *Registry) UnionSeededPathPatterns() []string {
	return slices.Clone(r.seededPathPatterns)
}

// ScrubRegexps returns the compiled case-insensitive regexps backing
// UnionScrubPatterns (issue #75 / ADR-0033) — compiled once in NewRegistry
// via CompileScrubPatterns, ready for the agent-API body sanitizer to match
// against. The returned slice is a clone, but the *regexp.Regexp values
// themselves are immutable and safe to share across callers/goroutines.
func (r *Registry) ScrubRegexps() []*regexp.Regexp {
	return slices.Clone(r.scrubRegexps)
}

// Get returns the provider registered under id.
func (r *Registry) Get(id string) (AgentProvider, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// List returns all providers in registration order.
func (r *Registry) List() []AgentProvider {
	out := make([]AgentProvider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// DefaultFor resolves a skip-layer default chain (issue #66): the first
// candidate id that is non-empty AND registered wins; an empty or
// unregistered candidate is treated as unset and falls through — a stored
// default naming a provider this install does not carry must never wedge a
// spawn. When no candidate matches, the first registered provider is the
// final fallback. ok is false only for an empty registry.
func (r *Registry) DefaultFor(candidates ...string) (AgentProvider, bool) {
	for _, id := range candidates {
		if id == "" {
			continue
		}
		if p, ok := r.byID[id]; ok {
			return p, true
		}
	}
	if len(r.order) == 0 {
		return nil, false
	}
	return r.byID[r.order[0]], true
}
