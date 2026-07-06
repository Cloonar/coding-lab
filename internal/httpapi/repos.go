package httpapi

// Repos API (pinned M2 contract): create with async clone, list/get, the
// settings PATCH, guarded delete, and clone retry. All business rules live
// in internal/reposvc; this file translates JSON ⇄ service calls and maps
// service errors onto status codes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// repoResponse is the pinned repo JSON shape. Nullable columns render as
// JSON null (no omitempty), so the SPA always sees every key.
type repoResponse struct {
	ID                   string  `json:"id"`
	Name                 string  `json:"name"`
	RemoteURL            string  `json:"remote_url"`
	CredentialID         *string `json:"credential_id"`
	ForgeCredentialID    *string `json:"forge_credential_id"`
	TrackerBinding       string  `json:"tracker_binding"`
	ForgeKind            string  `json:"forge_kind"`
	DefaultBranch        string  `json:"default_branch"`
	Provider             string  `json:"provider"`
	Incogni              bool    `json:"incogni"`
	ModelDefault         *string `json:"model_default"`
	EffortDefault        *string `json:"effort_default"`
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

func repoJSON(r store.Repo) repoResponse {
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
		Incogni:           req.Incogni,
	})
	if err != nil {
		s.writeRepoError(w, "creating repo", err)
		return
	}
	writeJSON(w, http.StatusCreated, repoJSON(repo))
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
	items := make([]repoResponse, 0, len(repos))
	for _, repo := range repos {
		items = append(items, repoJSON(repo))
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
	writeJSON(w, http.StatusOK, repoJSON(repo))
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
		case "model_default":
			u.ModelDefault, err = patchNullableString(raw, key)
		case "effort_default":
			u.EffortDefault, err = patchNullableString(raw, key)
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
	writeJSON(w, http.StatusOK, repoJSON(repo))
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
