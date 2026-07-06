package provider

import "fmt"

// Registry resolves a provider id (the repos.provider column) to its
// AgentProvider. Registration happens once at wiring time; the registry is
// immutable afterwards, so reads need no locking.
type Registry struct {
	order []string
	byID  map[string]AgentProvider
}

// NewRegistry builds a registry from providers, preserving their order for
// List (the /api/v1/providers rendering order). Empty and duplicate ids are
// wiring bugs and error out.
func NewRegistry(providers ...AgentProvider) (*Registry, error) {
	r := &Registry{byID: make(map[string]AgentProvider, len(providers))}
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
	}
	return r, nil
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
