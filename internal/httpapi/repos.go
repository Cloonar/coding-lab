package httpapi

// Repos API (pinned M2 contract): create with async clone, list/get, the
// settings PATCH, guarded delete, and clone retry. All business rules live
// in internal/reposvc; this file translates JSON ⇄ service calls and maps
// service errors onto status codes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// repoResponse is the pinned repo JSON shape. Nullable columns render as
// JSON null (no omitempty), so the SPA always sees every key.
type repoResponse struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	RemoteURL         string  `json:"remote_url"`
	CredentialID      *string `json:"credential_id"`
	ForgeCredentialID *string `json:"forge_credential_id"`
	TrackerBinding    string  `json:"tracker_binding"`
	ForgeKind         string  `json:"forge_kind"`
	DefaultBranch     string  `json:"default_branch"`
	// Provider is the repo's agent-CLI override (issue #66). Nullable: null
	// means inherit (global provider_default, then the first registered
	// provider — resolved at spawn, never materialized here).
	Provider      *string `json:"provider"`
	Incogni       bool    `json:"incogni"`
	ModelDefault  *string `json:"model_default"`
	EffortDefault *string `json:"effort_default"`
	// RemoteDefault is the repo's remote-control override (issue #163) — the
	// layer between the per-spawn pick and the global spawn_remote_default.
	// A POINTER, and never omitempty: the knob is boolean, so `false` is a real
	// VALUE ("this repo never spawns remote", which must beat a global true) and
	// only JSON null means inherit. Collapsing the two would silently turn an
	// explicit off into an on.
	RemoteDefault *bool `json:"remote_default"`
	// AFK-override spawn defaults (issue #19 / ADR-0021; provider issue #66).
	// Nullable: null means inherit the base default. AFKOptions renders as a
	// JSON object (or null when unset); a nil map marshals to null (no
	// omitempty), so the SPA always sees every key.
	AFKModelDefault    *string           `json:"afk_model_default"`
	AFKEffortDefault   *string           `json:"afk_effort_default"`
	AFKProviderDefault *string           `json:"afk_provider_default"`
	AFKOptions         map[string]string `json:"afk_options"`
	// AFKRemoteDefault is the AFK-override remote-control layer (issue #163),
	// resolved before remote_default for an unattended run. Same tri-state as
	// RemoteDefault: null = inherit, false = an explicit off.
	AFKRemoteDefault *bool `json:"afk_remote_default"`
	// AFK seed-prompt override (issue #52 / ADR-0027). AFKPrompt is the repo's
	// own override (null = inherit). AFKPromptEffective is read-only and computed:
	// what the repo WOULD use if its own override were empty — the global
	// afk_prompt setting if set, else the built-in template for the repo's incogni
	// flag, tokens un-interpolated. It deliberately ignores the repo's own override
	// so the UI can show the fallback a cleared field reveals.
	AFKPrompt            *string `json:"afk_prompt"`
	AFKPromptEffective   string  `json:"afk_prompt_effective"`
	GitAuthorName        *string `json:"git_author_name"`
	GitAuthorEmail       *string `json:"git_author_email"`
	AFKBranchPattern     string  `json:"afk_branch_pattern"`
	ManualBranchPrefix   string  `json:"manual_branch_prefix"`
	AFKAutoEnabled       bool    `json:"afk_auto_enabled"`
	ConsecutiveFailures  int     `json:"consecutive_failures"`
	BudgetMinutes        *int    `json:"budget_minutes"`
	MaxInstancesOverride *int    `json:"max_instances_override"`
	CloneStatus          string  `json:"clone_status"`
	CloneError           *string `json:"clone_error"`
	CreatedAt            string  `json:"created_at"`
	LastOpenedAt         *string `json:"last_opened_at"`
}

// repoJSON renders a repo row as its pinned JSON shape. It is a pure function
// of (repo, afkPromptEffective): afkPromptEffective is computed by the caller —
// once per response for the singular handlers, once per list (a single settings
// read) for the list handler — because it depends on the global afk_prompt
// setting, which repoJSON must not read (keeping it side-effect free).
func repoJSON(r store.Repo, afkPromptEffective string) repoResponse {
	resp := repoResponse{
		ID:                   r.ID,
		Name:                 r.Name,
		RemoteURL:            r.RemoteURL,
		CredentialID:         r.CredentialID,
		ForgeCredentialID:    r.ForgeCredentialID,
		TrackerBinding:       r.TrackerBinding,
		ForgeKind:            r.ForgeKind,
		DefaultBranch:        r.DefaultBranch,
		Provider:             r.Provider,
		Incogni:              r.Incogni,
		ModelDefault:         r.ModelDefault,
		EffortDefault:        r.EffortDefault,
		RemoteDefault:        r.RemoteDefault,
		AFKModelDefault:      r.AFKModelDefault,
		AFKEffortDefault:     r.AFKEffortDefault,
		AFKProviderDefault:   r.AFKProviderDefault,
		AFKOptions:           r.AFKOptions,
		AFKRemoteDefault:     r.AFKRemoteDefault,
		AFKPrompt:            r.AFKPrompt,
		AFKPromptEffective:   afkPromptEffective,
		GitAuthorName:        r.GitAuthorName,
		GitAuthorEmail:       r.GitAuthorEmail,
		AFKBranchPattern:     r.AFKBranchPattern,
		ManualBranchPrefix:   r.ManualBranchPrefix,
		AFKAutoEnabled:       r.AFKAutoEnabled,
		ConsecutiveFailures:  r.ConsecutiveFailures,
		BudgetMinutes:        r.BudgetMinutes,
		MaxInstancesOverride: r.MaxInstancesOverride,
		CloneStatus:          r.CloneStatus,
		CloneError:           r.CloneError,
		CreatedAt:            store.FormatTime(r.CreatedAt),
	}
	if r.LastOpenedAt != nil {
		t := store.FormatTime(*r.LastOpenedAt)
		resp.LastOpenedAt = &t
	}
	return resp
}

// effectiveAFKPrompt is the pure afk_prompt_effective rule (issue #52 /
// ADR-0027) given an already-read global afk_prompt setting: a non-empty global
// override is what a repo without its own override would use; otherwise the
// built-in template for the repo's incogni flag (tokens un-interpolated). By
// definition it IGNORES the repo's own afk_prompt — it is the fallback a cleared
// field would reveal. Shared by the single-repo helper and the list handler so
// the layering has one source of truth.
func effectiveAFKPrompt(global string, repo store.Repo) string {
	if global != "" {
		return global
	}
	return afk.SeedPromptTemplate(repo.Incogni)
}

// afkPromptEffective computes afk_prompt_effective for one repo, reading the
// global afk_prompt setting once. The singular repo handlers (get/create/update/
// afk toggles) call this; the list handler reads the global itself once and calls
// effectiveAFKPrompt per repo, so a list of N repos still does ONE settings read.
func (s *Server) afkPromptEffective(ctx context.Context, repo store.Repo) (string, error) {
	global, err := s.store.GetString(ctx, store.SettingAFKPrompt, "")
	if err != nil {
		return "", err
	}
	return effectiveAFKPrompt(global, repo), nil
}

// writeRepoError maps reposvc/store errors onto the pinned status codes.
func (s *Server) writeRepoError(w http.ResponseWriter, doing string, err error) {
	var bad *reposvc.BadRequestError
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, store.ErrNameTaken.Error())
	case errors.Is(err, store.ErrCredentialGone):
		// FK race: the referenced credential was deleted after the kind
		// check but before the row write landed.
		writeError(w, http.StatusConflict, store.ErrCredentialGone.Error())
	case errors.Is(err, reposvc.ErrCloneInProgress):
		writeError(w, http.StatusConflict, reposvc.ErrCloneInProgress.Error())
	case errors.Is(err, reposvc.ErrCloneNotFailed):
		writeError(w, http.StatusConflict, reposvc.ErrCloneNotFailed.Error())
	case errors.Is(err, reposvc.ErrHasLiveInstances):
		writeError(w, http.StatusConflict, reposvc.ErrHasLiveInstances.Error())
	default:
		s.internalError(w, doing, err)
	}
}

type repoCreateRequest struct {
	RemoteURL         string  `json:"remote_url"`
	Name              string  `json:"name"`
	CredentialID      *string `json:"credential_id"`
	ForgeCredentialID *string `json:"forge_credential_id"`
	TrackerBinding    string  `json:"tracker_binding"`
	Provider          *string `json:"provider"` // optional agent-CLI override (issue #66); null/"" = inherit
	Incogni           bool    `json:"incogni"`
}

// handleRepoCreate is POST /api/v1/repos: 201 with the row in clone_status
// "cloning"; the async clone job is already running.
func (s *Server) handleRepoCreate(w http.ResponseWriter, r *http.Request) {
	var req repoCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	repo, err := s.repos.Add(r.Context(), reposvc.AddParams{
		RemoteURL:         req.RemoteURL,
		Name:              req.Name,
		CredentialID:      normalizeOptID(req.CredentialID),
		ForgeCredentialID: normalizeOptID(req.ForgeCredentialID),
		TrackerBinding:    req.TrackerBinding,
		Provider:          normalizeOptID(req.Provider),
		Incogni:           req.Incogni,
	})
	if err != nil {
		s.writeRepoError(w, "creating repo", err)
		return
	}
	eff, err := s.afkPromptEffective(r.Context(), repo)
	if err != nil {
		s.internalError(w, "creating repo", err)
		return
	}
	writeJSON(w, http.StatusCreated, repoJSON(repo, eff))
}

// normalizeOptID treats an absent, null, or empty-string id as nil.
func normalizeOptID(p *string) *string {
	if p == nil {
		return nil
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return nil
	}
	return &v
}

// handleRepoList is GET /api/v1/repos.
func (s *Server) handleRepoList(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.Repos(r.Context())
	if err != nil {
		s.internalError(w, "listing repos", err)
		return
	}
	// One settings read for the whole list (issue #52 / ADR-0027): the global
	// afk_prompt is the same fallback for every repo, so read it once and derive
	// each row's afk_prompt_effective from it — never a read per repo.
	global, err := s.store.GetString(r.Context(), store.SettingAFKPrompt, "")
	if err != nil {
		s.internalError(w, "listing repos", err)
		return
	}
	items := make([]repoResponse, 0, len(repos))
	for _, repo := range repos {
		items = append(items, repoJSON(repo, effectiveAFKPrompt(global, repo)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": items})
}

// handleRepoGet is GET /api/v1/repos/{id}.
func (s *Server) handleRepoGet(w http.ResponseWriter, r *http.Request) {
	repo, err := s.store.RepoByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRepoError(w, "loading repo", err)
		return
	}
	eff, err := s.afkPromptEffective(r.Context(), repo)
	if err != nil {
		s.internalError(w, "loading repo", err)
		return
	}
	writeJSON(w, http.StatusOK, repoJSON(repo, eff))
}

// handleRepoUpdate is PATCH /api/v1/repos/{id}. The body is read as raw
// JSON per field so absent, null, and zero values stay distinguishable
// (null clears nullable columns; absent leaves them untouched).
func (s *Server) handleRepoUpdate(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if decodeJSON(w, r, &body) != nil {
		return
	}
	var u store.RepoSettingsUpdate
	for key, raw := range body {
		var err error
		switch key {
		case "name":
			u.Name, err = patchString(raw, key)
		case "credential_id":
			u.CredentialID, err = patchNullableString(raw, key)
		case "forge_credential_id":
			u.ForgeCredentialID, err = patchNullableString(raw, key)
		case "tracker_binding":
			u.TrackerBinding, err = patchString(raw, key)
		case "default_branch":
			u.DefaultBranch, err = patchString(raw, key)
		case "provider":
			// null/"" clears to NULL (inherit); a non-empty id is validated
			// against the registry in reposvc.UpdateSettings (unknown → 400).
			u.Provider, err = patchNullableString(raw, key)
		case "afk_provider_default":
			u.AFKProviderDefault, err = patchNullableString(raw, key)
		case "model_default":
			u.ModelDefault, err = patchNullableString(raw, key)
		case "effort_default":
			u.EffortDefault, err = patchNullableString(raw, key)
		case "afk_model_default":
			u.AFKModelDefault, err = patchNullableString(raw, key)
		case "afk_effort_default":
			u.AFKEffortDefault, err = patchNullableString(raw, key)
		case "remote_default":
			// The tri-state boolean knob (issue #163): null clears to NULL
			// (inherit the global spawn_remote_default), true/false PIN the repo
			// — including false, which must beat a global true.
			u.RemoteDefault, err = patchNullableBool(raw, key)
		case "afk_remote_default":
			u.AFKRemoteDefault, err = patchNullableBool(raw, key)
		case "afk_options":
			u.AFKOptions, err = patchOptionsBag(raw, key)
		case "afk_prompt":
			// patchNullableString already maps null/whitespace-only → nil (NULL =
			// inherit); a real override is kept verbatim. Cap it (issue #52 /
			// ADR-0027) with the same message the settings PATCH uses.
			u.AFKPrompt, err = patchNullableString(raw, key)
			if err == nil && u.AFKPrompt.Value != nil && len(*u.AFKPrompt.Value) > afkPromptMaxBytes {
				err = fmt.Errorf("afk_prompt must be at most %d bytes", afkPromptMaxBytes)
			}
		case "incogni":
			u.Incogni, err = patchBool(raw, key)
		case "git_author_name":
			u.GitAuthorName, err = patchNullableString(raw, key)
		case "git_author_email":
			u.GitAuthorEmail, err = patchNullableString(raw, key)
		case "afk_branch_pattern":
			u.AFKBranchPattern, err = patchString(raw, key)
		case "manual_branch_prefix":
			u.ManualBranchPrefix, err = patchString(raw, key)
		case "afk_auto_enabled":
			u.AFKAutoEnabled, err = patchBool(raw, key)
		case "budget_minutes":
			u.BudgetMinutes, err = patchNullableInt(raw, key)
		case "max_instances_override":
			u.MaxInstancesOverride, err = patchNullableInt(raw, key)
		default:
			err = fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	repo, err := s.repos.UpdateSettings(r.Context(), r.PathValue("id"), u)
	if err != nil {
		s.writeRepoError(w, "updating repo", err)
		return
	}
	eff, err := s.afkPromptEffective(r.Context(), repo)
	if err != nil {
		s.internalError(w, "updating repo", err)
		return
	}
	writeJSON(w, http.StatusOK, repoJSON(repo, eff))
}

// handleRepoDelete is DELETE /api/v1/repos/{id}[?force=true]: 204, or 409
// while the clone job runs unless forced.
func (s *Server) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"
	if err := s.repos.Delete(r.Context(), r.PathValue("id"), force); err != nil {
		s.writeRepoError(w, "deleting repo", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRepoCloneRetry is POST /api/v1/repos/{id}/clone/retry: 202, only
// from clone_status "error".
func (s *Server) handleRepoCloneRetry(w http.ResponseWriter, r *http.Request) {
	if err := s.repos.Retry(r.Context(), r.PathValue("id")); err != nil {
		s.writeRepoError(w, "retrying clone", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"clone_status": store.CloneStatusCloning})
}

// patchString reads a required-string PATCH field (null rejected).
func patchString(raw json.RawMessage, field string) (store.Opt[string], error) {
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return store.Opt[string]{}, fmt.Errorf("field %s must be a string", field)
	}
	return store.Set(*v), nil
}

// patchNullableString reads a nullable-string PATCH field; null and ""
// both clear the column.
func patchNullableString(raw json.RawMessage, field string) (store.Opt[*string], error) {
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		return store.Opt[*string]{}, fmt.Errorf("field %s must be a string or null", field)
	}
	if v != nil && strings.TrimSpace(*v) == "" {
		v = nil
	}
	return store.Set(v), nil
}

// patchBool reads a boolean PATCH field (null rejected).
func patchBool(raw json.RawMessage, field string) (store.Opt[bool], error) {
	var v *bool
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return store.Opt[bool]{}, fmt.Errorf("field %s must be a boolean", field)
	}
	return store.Set(*v), nil
}

// patchNullableInt reads a nullable-integer PATCH field; null clears.
func patchNullableInt(raw json.RawMessage, field string) (store.Opt[*int], error) {
	var v *int
	if err := json.Unmarshal(raw, &v); err != nil {
		return store.Opt[*int]{}, fmt.Errorf("field %s must be an integer or null", field)
	}
	return store.Set(v), nil
}

// patchNullableBool reads a nullable-boolean PATCH field (issue #163): null
// clears the column to NULL (inherit the layer below), true/false pin it.
//
// The tri-state is the whole point, and it is why this cannot reuse
// patchNullableString's shape: THAT decoder collapses "" into nil because no
// string knob has a legal empty value, but `false` IS a legal boolean value —
// "this repo never spawns remote" — and must survive as one. Absent key →
// Opt.Set == false (untouched); null → Set with a nil value (inherit); false →
// Set with a pinned false that beats a true from any lower layer. Anything else
// (a string, a number) is a 400 — a boolean column takes booleans.
func patchNullableBool(raw json.RawMessage, field string) (store.Opt[*bool], error) {
	var v *bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return store.Opt[*bool]{}, fmt.Errorf("field %s must be a boolean or null", field)
	}
	return store.Set(v), nil
}

// patchOptionsBag reads the nullable afk_options PATCH field (issue #19): null
// clears the column back to NULL (inherit the global bag); a JSON object of
// string values sets the repo's explicit bag (even {} — "explicitly no
// options"). The bag is filtered + validated against the provider at spawn
// (mirroring how repo model_default is validated at spawn, not at PATCH), so
// this only enforces the shape.
func patchOptionsBag(raw json.RawMessage, field string) (store.Opt[map[string]string], error) {
	var v map[string]string
	if err := json.Unmarshal(raw, &v); err != nil {
		return store.Opt[map[string]string]{}, fmt.Errorf("field %s must be an object of string values or null", field)
	}
	return store.Set(v), nil
}
