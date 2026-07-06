package httpapi

// Label surface (pinned M4 contract): per-repo label CRUD for the built-in
// tracker. The list is readable on any repo (every repo carries the five
// seeded triage labels), but label mutations are builtin-only — a forge-bound
// repo's labels live on the forge, so mutations answer the pinned 409, same
// as the issue mutations. Label mutations publish issue.changed too: labels
// are issue-view state, and clients refetch on event.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// labelResponse is the pinned label JSON shape.
type labelResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func labelJSON(l store.Label) labelResponse {
	return labelResponse{ID: l.ID, Name: l.Name, Color: l.Color, Description: l.Description}
}

// handleLabelList is GET /api/v1/repos/{id}/labels.
func (s *Server) handleLabelList(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	labels, err := s.store.LabelsByRepo(r.Context(), repo.ID)
	if err != nil {
		s.internalError(w, "listing labels", err)
		return
	}
	items := make([]labelResponse, 0, len(labels))
	for _, l := range labels {
		items = append(items, labelJSON(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{"labels": items})
}

type labelCreateRequest struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// handleLabelCreate is POST /api/v1/repos/{id}/labels (builtin only): 201, or
// 409 on a duplicate name within the repo.
func (s *Server) handleLabelCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinTracker(w, repo) {
		return
	}
	var req labelCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	// Store the trimmed name: label matching is exact (ready queue, list
	// filters), so "bug " must not coexist with a distinct "bug".
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	l, err := s.store.CreateLabel(r.Context(), repo.ID, name, req.Color, req.Description)
	if err != nil {
		s.writeLabelError(w, "creating label", err)
		return
	}
	s.publishIssueChanged(repo.ID)
	writeJSON(w, http.StatusCreated, labelJSON(l))
}

// handleLabelUpdate is PATCH /api/v1/repos/{id}/labels/{lid} (builtin only).
func (s *Server) handleLabelUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinTracker(w, repo) {
		return
	}
	l, ok := s.loadRepoLabel(w, r, repo)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if decodeJSON(w, r, &body) != nil {
		return
	}
	var u store.LabelUpdate
	for key, raw := range body {
		var err error
		switch key {
		case "name":
			u.Name, err = patchString(raw, key)
			if err == nil {
				// Renames store trimmed too (same exact-match rationale as
				// create).
				u.Name.Value = strings.TrimSpace(u.Name.Value)
				if u.Name.Value == "" {
					err = fmt.Errorf("name must not be empty")
				}
			}
		case "color":
			u.Color, err = patchString(raw, key)
		case "description":
			u.Description, err = patchString(raw, key)
		default:
			err = fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	updated, err := s.store.UpdateLabel(r.Context(), l.ID, u)
	if err != nil {
		s.writeLabelError(w, "updating label", err)
		return
	}
	s.publishIssueChanged(repo.ID)
	writeJSON(w, http.StatusOK, labelJSON(updated))
}

// handleLabelDelete is DELETE /api/v1/repos/{id}/labels/{lid} (builtin only):
// 204; the label's issue_labels rows cascade.
func (s *Server) handleLabelDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinTracker(w, repo) {
		return
	}
	l, ok := s.loadRepoLabel(w, r, repo)
	if !ok {
		return
	}
	if err := s.store.DeleteLabel(r.Context(), l.ID); err != nil {
		s.writeLabelError(w, "deleting label", err)
		return
	}
	s.publishIssueChanged(repo.ID)
	w.WriteHeader(http.StatusNoContent)
}

// loadRepoLabel resolves {lid} to a label OF THIS REPO — a label id belonging
// to another repo is a 404, never cross-repo access. ok=false means the
// response was written.
func (s *Server) loadRepoLabel(w http.ResponseWriter, r *http.Request, repo store.Repo) (store.Label, bool) {
	l, err := s.store.LabelByID(r.Context(), r.PathValue("lid"))
	if err != nil {
		s.writeLabelError(w, "loading label", err)
		return store.Label{}, false
	}
	if l.RepoID != repo.ID {
		writeError(w, http.StatusNotFound, "not found")
		return store.Label{}, false
	}
	return l, true
}

// writeLabelError maps label accessor errors onto the pinned status codes.
func (s *Server) writeLabelError(w http.ResponseWriter, doing string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, store.ErrNameTaken.Error())
	default:
		s.internalError(w, doing, err)
	}
}
