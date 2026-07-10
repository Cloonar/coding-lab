package httpapi

// Repo secret surface (issue #104): write-only per-repo secrets — create,
// rotate, delete, and a metadata-only list. Mirrors credentials.go's
// discipline (vault.Encrypt seals the value before it reaches the store; no
// response ever carries the value back) and labels.go's per-repo sub-resource
// shape (s.loadRepo + a cross-repo guard that 404s a child belonging to
// another repo rather than leaking a cross-repo existence signal).

import (
	"errors"
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// repoSecretGrammarMessage is the pinned 400 message for a name that fails
// store.ValidSecretName — never echoes the offending value.
const repoSecretGrammarMessage = "name: must match ^[A-Z][A-Z0-9_]*$"

// repoSecretJSON is the metadata shape every secret response answers with —
// NEVER the value (design §12: no secret readback). exposed_run_id/exposed_at
// mirror store.RepoSecret's sticky exposure flag (issue #108): both null
// means never exposed since creation or the last rotation; both set is the
// settings-page exposure warning's source. Like created_at/updated_at, the
// keys are always present and render JSON null rather than being omitted, so
// the SPA never has to distinguish "absent" from "not exposed".
func repoSecretJSON(rs store.RepoSecret) map[string]any {
	m := map[string]any{
		"id":             rs.ID,
		"name":           rs.Name,
		"description":    rs.Description,
		"created_at":     store.FormatTime(rs.CreatedAt),
		"updated_at":     store.FormatTime(rs.UpdatedAt),
		"exposed_run_id": rs.ExposedRunID,
		"exposed_at":     nil,
	}
	if rs.ExposedAt != nil {
		exposedAt := store.FormatTime(*rs.ExposedAt)
		m["exposed_at"] = exposedAt
	}
	return m
}

// handleRepoSecretList is GET /api/v1/repos/{id}/secrets.
func (s *Server) handleRepoSecretList(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	secrets, err := s.store.RepoSecrets(r.Context(), repo.ID)
	if err != nil {
		s.internalError(w, "listing repo secrets", err)
		return
	}
	items := make([]map[string]any, 0, len(secrets))
	for _, rs := range secrets {
		items = append(items, repoSecretJSON(rs))
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": items})
}

type repoSecretCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Value       string `json:"value"`
}

// handleRepoSecretCreate is POST /api/v1/repos/{id}/secrets: encrypts the
// value with s.vault.Encrypt (raw bytes, not the JSON-payload envelope
// credentials use) before it ever reaches the store.
func (s *Server) handleRepoSecretCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	var req repoSecretCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if !store.ValidSecretName(req.Name) {
		writeError(w, http.StatusBadRequest, repoSecretGrammarMessage)
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}
	sealed, err := s.vault.Encrypt([]byte(req.Value))
	if err != nil {
		s.internalError(w, "encrypting repo secret", err)
		return
	}
	rs, err := s.store.CreateRepoSecret(r.Context(), ids.NewID("sec"), repo.ID, req.Name, req.Description, sealed, s.now())
	if err != nil {
		s.writeRepoSecretError(w, "creating repo secret", err)
		return
	}
	writeJSON(w, http.StatusCreated, repoSecretJSON(rs))
}

type repoSecretRotateRequest struct {
	Value string `json:"value"`
}

// handleRepoSecretRotate is PATCH /api/v1/repos/{id}/secrets/{sid}: rotate
// only — name/description are immutable via this endpoint. The old value is
// never read back; the caller supplies a fresh one wholesale.
func (s *Server) handleRepoSecretRotate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	existing, ok := s.loadRepoSecret(w, r, repo)
	if !ok {
		return
	}
	var req repoSecretRotateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}
	sealed, err := s.vault.Encrypt([]byte(req.Value))
	if err != nil {
		s.internalError(w, "encrypting repo secret", err)
		return
	}
	rotated, err := s.store.RotateRepoSecret(r.Context(), existing.ID, sealed, s.now())
	if err != nil {
		s.writeRepoSecretError(w, "rotating repo secret", err)
		return
	}
	writeJSON(w, http.StatusOK, repoSecretJSON(rotated))
}

// handleRepoSecretDelete is DELETE /api/v1/repos/{id}/secrets/{sid}: 204,
// immediate — unlike credentials, secrets are never referenced elsewhere, so
// there is no 409-while-in-use path.
func (s *Server) handleRepoSecretDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	existing, ok := s.loadRepoSecret(w, r, repo)
	if !ok {
		return
	}
	if err := s.store.DeleteRepoSecret(r.Context(), existing.ID); err != nil {
		s.writeRepoSecretError(w, "deleting repo secret", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loadRepoSecret resolves {sid} to a secret OF THIS REPO — a secret id
// belonging to another repo is a 404, never cross-repo access (labels.go's
// loadRepoLabel precedent).
func (s *Server) loadRepoSecret(w http.ResponseWriter, r *http.Request, repo store.Repo) (store.RepoSecret, bool) {
	rs, err := s.store.RepoSecretByID(r.Context(), r.PathValue("sid"))
	if err != nil {
		s.writeRepoSecretError(w, "loading repo secret", err)
		return store.RepoSecret{}, false
	}
	if rs.RepoID != repo.ID {
		writeError(w, http.StatusNotFound, "not found")
		return store.RepoSecret{}, false
	}
	return rs, true
}

// writeRepoSecretError maps repo-secret accessor errors onto the pinned
// status codes.
func (s *Server) writeRepoSecretError(w http.ResponseWriter, doing string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, store.ErrNameTaken.Error())
	case errors.Is(err, store.ErrInvalidSecretName):
		writeError(w, http.StatusBadRequest, repoSecretGrammarMessage)
	default:
		s.internalError(w, doing, err)
	}
}
