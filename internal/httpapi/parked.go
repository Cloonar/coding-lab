package httpapi

// Parked view + unguarded discard (pinned M3 contract): GET
// /api/v1/repos/{id}/parked and POST /api/v1/repos/{id}/parked/discard.
// Business rules live in internal/reconcile.

import (
	"errors"
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/reconcile"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// parkedResponse is one parked entry (pinned shape). worktree_path is "" for a
// bare claim branch.
type parkedResponse struct {
	Branch       string `json:"branch"`
	WorktreePath string `json:"worktree_path"`
	Dirty        bool   `json:"dirty"`
	CommitsAhead int    `json:"commits_ahead"`
	Unpushed     int    `json:"unpushed"`
}

// handleParkedList is GET /api/v1/repos/{id}/parked.
func (s *Server) handleParkedList(w http.ResponseWriter, r *http.Request) {
	entries, err := s.reconcile.Parked(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeParkedError(w, "listing parked work", err)
		return
	}
	items := make([]parkedResponse, 0, len(entries))
	for _, e := range entries {
		items = append(items, parkedResponse{
			Branch:       e.Branch,
			WorktreePath: e.WorktreePath,
			Dirty:        e.Dirty,
			CommitsAhead: e.CommitsAhead,
			Unpushed:     e.Unpushed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"parked": items})
}

type parkedDiscardRequest struct {
	Branch string `json:"branch"`
}

// handleParkedDiscard is POST /api/v1/repos/{id}/parked/discard {branch}: 204.
// The unguarded destroyer — 400 for a non-managed branch (never main or a
// human's branch), 404 for an unknown repo.
func (s *Server) handleParkedDiscard(w http.ResponseWriter, r *http.Request) {
	var req parkedDiscardRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if err := s.reconcile.Discard(r.Context(), r.PathValue("id"), req.Branch); err != nil {
		s.writeParkedError(w, "discarding parked branch", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeParkedError(w http.ResponseWriter, doing string, err error) {
	var bad *reconcile.BadRequestError
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		s.internalError(w, doing, err)
	}
}
