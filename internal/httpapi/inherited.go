package httpapi

// Inherited values (issue #61): POST /api/v1/repos/{id}/inherited answers,
// for every overridable repo field, what that field resolves to when the
// repo's OWN value is null. Each answer comes from the very resolver the
// spawn path calls, run on a copy of the repo with that one field nulled, so
// the repo settings page never derives an effective value a second way — and
// the two can never drift, because there is only one chain to drift.
//
// It is a pure read: no write, no validation side effect, no network call
// and no process (no AuthStatus, no podman) — every resolver below reads the
// store and the provider registry's static catalogs only. The request body
// carries unsaved DRAFT values of the fields other fields' chains read, so a
// dependent field's inherited value follows an edit above it (the AFK agent
// follows the agent, a model follows its agent) before anything is saved.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// repoInheritedResponse is the RepoInherited JSON shape (web/src/api/repos.ts):
// every key always present, the inherited value or null. null means the chain
// below the repo could not be resolved (a store read failed, a strict lander
// request names an unknown id, no provider is registered…); one field's
// failure never fails the response.
type repoInheritedResponse struct {
	Provider           *string           `json:"provider"`
	AFKProviderDefault *string           `json:"afk_provider_default"`
	LanderProvider     *string           `json:"lander_provider"`
	ModelDefault       *string           `json:"model_default"`
	EffortDefault      *string           `json:"effort_default"`
	AFKModelDefault    *string           `json:"afk_model_default"`
	AFKEffortDefault   *string           `json:"afk_effort_default"`
	LanderModel        *string           `json:"lander_model"`
	LanderEffort       *string           `json:"lander_effort"`
	RemoteDefault      *bool             `json:"remote_default"`
	AFKRemoteDefault   *bool             `json:"afk_remote_default"`
	AFKOptions         map[string]string `json:"afk_options"`
	BudgetMinutes      *int              `json:"budget_minutes"`
	// MaxInstancesOverride is the instance cap that applies without a repo
	// override.
	MaxInstancesOverride *int    `json:"max_instances_override"`
	GitAuthorName        *string `json:"git_author_name"`
	GitAuthorEmail       *string `json:"git_author_email"`
	Runner               *string `json:"runner"`
	// ImageRef is "" when no dev image is configured anywhere below the repo.
	ImageRef        *string `json:"image_ref"`
	ContainerMemory *string `json:"container_memory"`
	ContainerPids   *int    `json:"container_pids"`
	ContainerNofile *int    `json:"container_nofile"`
}

// inheritedDraftKeys are the request keys the inherited endpoint resolves
// against instead of the saved repo — exactly the fields another field's
// chain reads (RepoInheritedDrafts in web/src/api/repos.ts).
var inheritedDraftKeys = []string{
	"afk_effort_default", "afk_model_default", "afk_provider_default", "effort_default",
	"lander_provider", "model_default", "provider", "remote_default",
}

// handleRepoInherited is POST /api/v1/repos/{id}/inherited: 200 with the
// inherited value of every overridable field. The body is an optional JSON
// object of draft overrides, with the PATCH's semantics per key — absent =
// the saved value, null or "" = inherit, a value = the draft. An empty body
// or {} resolves the saved repo; any other key is a 400 naming it. A draft
// provider id is not validated: an unknown one behaves exactly as a stale
// stored id does in the skip-layer chain (ADR-0030).
func (s *Server) handleRepoInherited(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	for _, key := range slices.Sorted(maps.Keys(body)) {
		if err := applyInheritedDraft(&repo, key, body[key]); err != nil {
			writeFieldError(w, http.StatusBadRequest, key, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, s.repoInherited(r.Context(), repo))
}

// applyInheritedDraft lays one draft value over repo, decoded the way the
// PATCH decodes the same key (patchNullableString folds null/blank into nil
// = inherit; patchNullableBool keeps false as a value). Provider ids are
// trimmed the way reposvc's validateProvider normalizes them on save.
func applyInheritedDraft(repo *store.Repo, key string, raw json.RawMessage) error {
	if key == "remote_default" {
		v, err := patchNullableBool(raw, key)
		if err != nil {
			return err
		}
		repo.RemoteDefault = v.Value
		return nil
	}
	var dst **string
	trim := false
	switch key {
	case "provider":
		dst, trim = &repo.Provider, true
	case "afk_provider_default":
		dst, trim = &repo.AFKProviderDefault, true
	case "lander_provider":
		dst, trim = &repo.LanderProvider, true
	case "model_default":
		dst = &repo.ModelDefault
	case "effort_default":
		dst = &repo.EffortDefault
	case "afk_model_default":
		dst = &repo.AFKModelDefault
	case "afk_effort_default":
		dst = &repo.AFKEffortDefault
	default:
		return fmt.Errorf("unknown field %q (drafts accepted: %s)", key, strings.Join(inheritedDraftKeys, ", "))
	}
	v, err := patchNullableString(raw, key)
	if err != nil {
		return err
	}
	if trim && v.Value != nil {
		id := strings.TrimSpace(*v.Value)
		v.Value = &id
	}
	*dst = v.Value
	return nil
}

// repoInherited resolves every RepoInherited key for repo (drafts already
// applied). Each key names the spawn-path resolver it asks; the repo copy it
// asks about differs from repo in exactly the one field being answered for.
// The manual kind stands for every run the operator starts, store.RunKindAFKAuto
// for every AFK-class run (all AFK kinds share one chain — isAFKKind), and
// store.RunKindLander for the validation class.
func (s *Server) repoInherited(ctx context.Context, repo store.Repo) repoInheritedResponse {
	inst := s.instances
	var out repoInheritedResponse

	// The agent per run class — instance.Service.ResolveProvider, the
	// skip-layer chain of ADR-0030. Manual: repo.provider → global
	// provider_default → first registered. AFK: repo.afk_provider_default →
	// global spawn_provider_default_afk → the manual chain.
	{
		c := repo
		c.Provider = nil
		if p, err := inst.ResolveProvider(ctx, c, store.RunKindManual, ""); err == nil {
			out.Provider = valPtr(p.ID())
		}
	}
	{
		c := repo
		c.AFKProviderDefault = nil
		if p, err := inst.ResolveProvider(ctx, c, store.RunKindAFKAuto, ""); err == nil {
			out.AFKProviderDefault = valPtr(p.ID())
		}
	}
	// Lander: afk.LanderChainProvider — repo.lander_provider as a strict
	// request, else the manual chain — the one function the lander and
	// escalate launches and their autoland gates resolve through.
	{
		c := repo
		c.LanderProvider = nil
		if p, err := afk.LanderChainProvider(ctx, inst, c, store.RunKindLander); err == nil {
			out.LanderProvider = valPtr(p.ID())
		}
	}

	// Model, effort and remote control belong to the run class's EFFECTIVE
	// provider — resolved with the repo's own (saved or draft) provider
	// fields, exactly as the spawn resolves it before asking for them.
	if prov, err := inst.ResolveProvider(ctx, repo, store.RunKindManual, ""); err == nil {
		// instance.Service.ResolveModelEffort, manual: repo.model_default →
		// global spawn_model_default → the provider's catalog default; the
		// effort against the RESOLVED model's own list (issue #156), so the
		// effort is asked with the repo's model_default in place.
		c := repo
		c.ModelDefault = nil
		if m, _, err := inst.ResolveModelEffort(ctx, prov, c, store.RunKindManual, "", ""); err == nil {
			out.ModelDefault = valPtr(m)
		}
		c = repo
		c.EffortDefault = nil
		if _, e, err := inst.ResolveModelEffort(ctx, prov, c, store.RunKindManual, "", ""); err == nil {
			out.EffortDefault = valPtr(e)
		}
		// instance.Service.ResolveRemote, manual: repo.remote_default →
		// global spawn_remote_default → false, clamped by the provider's
		// remote capability (ADR-0045) — the clamp is the truth to report.
		c = repo
		c.RemoteDefault = nil
		if v, err := inst.ResolveRemote(ctx, prov, c, store.RunKindManual, nil); err == nil {
			out.RemoteDefault = valPtr(v)
		}
	}
	if prov, err := inst.ResolveProvider(ctx, repo, store.RunKindAFKAuto, ""); err == nil {
		// AFK: the AFK override layer (repo.afk_* → global spawn_*_default_afk)
		// before the manual chain above.
		c := repo
		c.AFKModelDefault = nil
		if m, _, err := inst.ResolveModelEffort(ctx, prov, c, store.RunKindAFKAuto, "", ""); err == nil {
			out.AFKModelDefault = valPtr(m)
		}
		c = repo
		c.AFKEffortDefault = nil
		if _, e, err := inst.ResolveModelEffort(ctx, prov, c, store.RunKindAFKAuto, "", ""); err == nil {
			out.AFKEffortDefault = valPtr(e)
		}
		// repo.afk_remote_default → global spawn_remote_default_afk → the
		// manual chain (repo.remote_default first), capability-clamped.
		c = repo
		c.AFKRemoteDefault = nil
		if v, err := inst.ResolveRemote(ctx, prov, c, store.RunKindAFKAuto, nil); err == nil {
			out.AFKRemoteDefault = valPtr(v)
		}
		// instance.Service.ResolveSpawnOptions: the global spawn_options_afk
		// bag, filtered to and validated against the provider's schema.
		c = repo
		c.AFKOptions = nil
		if bag, err := inst.ResolveSpawnOptions(ctx, prov, c, store.RunKindAFKAuto); err == nil {
			out.AFKOptions = bag
		}
	}
	if prov, err := afk.LanderChainProvider(ctx, inst, repo, store.RunKindLander); err == nil {
		// afk.LanderModelEffort: the global spawn_*_default_lander layer →
		// repo base → global base → catalog, with repo.lander_model/effort as
		// STRICT requests on top. The inherited model is what resolves
		// without either request; the inherited effort keeps the repo's
		// lander_model, because the effort is checked against that model.
		c := repo
		c.LanderModel, c.LanderEffort = nil, nil
		if m, _, err := afk.LanderModelEffort(ctx, inst, prov, c, store.RunKindLander); err == nil {
			out.LanderModel = valPtr(m)
		}
		c = repo
		c.LanderEffort = nil
		if _, e, err := afk.LanderModelEffort(ctx, inst, prov, c, store.RunKindLander); err == nil {
			out.LanderEffort = valPtr(e)
		}
	}

	// AFK budget clock — afk.EffectiveBudget: global afk_budget_minutes
	// (default 120). Never fails: a bad row warns and uses the default, as
	// every launch does.
	{
		c := repo
		c.BudgetMinutes = nil
		out.BudgetMinutes = valPtr(int(afk.EffectiveBudget(ctx, s.store, s.log, c) / time.Minute))
	}
	// Instance cap — instance.Service.EffectiveCap: global max_instances
	// (default 6).
	{
		c := repo
		c.MaxInstancesOverride = nil
		out.MaxInstancesOverride = valPtr(inst.EffectiveCap(ctx, c))
	}
	// Git author — instance.Service.AuthorIdentity, the chain behind the
	// spawn's GIT_AUTHOR_* env: the global git_author_name /
	// git_author_email setting, "" when unset.
	{
		c := repo
		c.GitAuthorName = nil
		if name, _, err := inst.AuthorIdentity(ctx, c); err == nil {
			out.GitAuthorName = valPtr(name)
		}
		c = repo
		c.GitAuthorEmail = nil
		if _, email, err := inst.AuthorIdentity(ctx, c); err == nil {
			out.GitAuthorEmail = valPtr(email)
		}
	}
	// Runner — instance.EffectiveRunner: the global runner_default; an
	// unreadable or invalid row is the spawn's refusal, so null here.
	{
		c := repo
		c.Runner = nil
		if v, err := instance.EffectiveRunner(ctx, s.store, c); err == nil {
			out.Runner = valPtr(v)
		}
	}
	// Dev image — instance.Service.DevImage (EffectiveDevImage with the
	// spawn's own --container-image copy): dev_image_default → the flag.
	// Nothing configured at all is "" (ErrNoDevImage); any other error null.
	{
		c := repo
		c.ImageRef = nil
		v, err := inst.DevImage(ctx, c)
		switch {
		case err == nil:
			out.ImageRef = valPtr(v)
		case errors.Is(err, instance.ErrNoDevImage):
			out.ImageRef = valPtr("")
		}
	}
	// Container limits — instance.Service.EffectiveContainerLimits: the
	// global container_memory / container_pids / container_nofile settings,
	// else their seeded defaults. Each field's chain is independent of the
	// other two, so one call with all three overrides nulled answers all.
	{
		c := repo
		c.ContainerMemory, c.ContainerPids, c.ContainerNofile = nil, nil, nil
		if mem, pids, nofile, err := inst.EffectiveContainerLimits(ctx, c); err == nil {
			out.ContainerMemory, out.ContainerPids, out.ContainerNofile = valPtr(mem), valPtr(pids), valPtr(nofile)
		}
	}
	return out
}

// valPtr returns a pointer to a copy of v (a JSON value where nil is null).
func valPtr[T any](v T) *T { return &v }
