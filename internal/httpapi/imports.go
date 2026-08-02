package httpapi

// Repo imports API (issue #261 / ADR-0063): the repo-scoped read-only-import
// declarations — list, add, remove. Business rules (self-import, unknown
// target, idempotent add/remove) live in reposvc; this file translates JSON
// ⇄ service calls exactly the way repos.go and secrets.go do for their own
// sub-resources. The delete-guard 409 (a DIFFERENT repo's DeleteRepo refused
// because this repo still imports it) is mapped in writeRepoError, not here.

import (
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// importResponse is the pinned imports list/add item shape: id + name — what
// the settings picker and the delete-refusal message need, nothing more.
type importResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func importJSON(r store.Repo) importResponse {
	return importResponse{ID: r.ID, Name: r.Name}
}

// handleRepoImportsList is GET /api/v1/repos/{id}/imports: 200 with the
// targets repo {id} has declared, ordered by name. An empty set renders "[]",
// never null (repoSecretJSON's list precedent).
func (s *Server) handleRepoImportsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	imports, err := s.repos.Imports(r.Context(), repo.ID)
	if err != nil {
		s.writeRepoError(w, "listing repo imports", err)
		return
	}
	items := make([]importResponse, 0, len(imports))
	for _, i := range imports {
		items = append(items, importJSON(i))
	}
	writeJSON(w, http.StatusOK, map[string]any{"imports": items})
}

type repoImportCreateRequest struct {
	TargetRepoID string `json:"target_repo_id"`
}

// handleRepoImportsAdd is POST /api/v1/repos/{id}/imports: 201 with the
// target's {id, name}. Idempotent — re-adding an already-declared target
// still answers 201 with the same row (reposvc.AddImport).
func (s *Server) handleRepoImportsAdd(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	var req repoImportCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if req.TargetRepoID == "" {
		writeError(w, http.StatusBadRequest, "target_repo_id required")
		return
	}
	target, err := s.repos.AddImport(r.Context(), repo.ID, req.TargetRepoID)
	if err != nil {
		s.writeRepoError(w, "adding repo import", err)
		return
	}
	writeJSON(w, http.StatusCreated, importJSON(target))
}

// handleRepoImportsRemove is DELETE /api/v1/repos/{id}/imports/{target}:
// 204, idempotent — removing an absent import declaration is not an error
// (reposvc.RemoveImport).
func (s *Server) handleRepoImportsRemove(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if err := s.repos.RemoveImport(r.Context(), repo.ID, r.PathValue("target")); err != nil {
		s.writeRepoError(w, "removing repo import", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
