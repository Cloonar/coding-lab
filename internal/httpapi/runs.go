package httpapi

// Runs history endpoint (pinned M3 contract: GET /api/v1/runs?repo=<id>&limit=50
// → {runs:[…]} newest first) plus the shared run JSON shape the instance
// listing embeds.

import (
	"net/http"
	"strconv"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// defaultRunsLimit / maxRunsLimit bound the runs history page.
const (
	defaultRunsLimit = 50
	maxRunsLimit     = 500
)

// runResponse is the pinned run JSON shape. Nullable columns render as JSON
// null (no omitempty) so the SPA always sees every key.
type runResponse struct {
	ID             string  `json:"id"`
	RepoID         string  `json:"repo_id"`
	Kind           string  `json:"kind"`
	Provider       string  `json:"provider"`
	IssueNumber    *int    `json:"issue_number"`
	Branch         string  `json:"branch"`
	WorktreePath   string  `json:"worktree_path"`
	SessionName    string  `json:"session_name"`
	Model          string  `json:"model"`
	Effort         string  `json:"effort"`
	DeepLinkURL    *string `json:"deep_link_url"`
	StartedAt      string  `json:"started_at"`
	BudgetDeadline *string `json:"budget_deadline"`
	EndedAt        *string `json:"ended_at"`
	Outcome        string  `json:"outcome"`
	FailureReason  *string `json:"failure_reason"`
}

func runJSON(r store.Run) runResponse {
	resp := runResponse{
		ID:            r.ID,
		RepoID:        r.RepoID,
		Kind:          r.Kind,
		Provider:      r.Provider,
		IssueNumber:   r.IssueNumber,
		Branch:        r.Branch,
		WorktreePath:  r.WorktreePath,
		SessionName:   r.SessionName,
		Model:         r.Model,
		Effort:        r.Effort,
		DeepLinkURL:   r.DeepLinkURL,
		StartedAt:     store.FormatTime(r.StartedAt),
		Outcome:       r.Outcome,
		FailureReason: r.FailureReason,
	}
	if r.BudgetDeadline != nil {
		t := store.FormatTime(*r.BudgetDeadline)
		resp.BudgetDeadline = &t
	}
	if r.EndedAt != nil {
		t := store.FormatTime(*r.EndedAt)
		resp.EndedAt = &t
	}
	return resp
}

// handleRunsList is GET /api/v1/runs?repo=<id>&limit=50 — a repo's run history,
// newest first. repo is required; limit defaults to 50 and is capped at 500.
func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	repoID := r.URL.Query().Get("repo")
	if repoID == "" {
		writeError(w, http.StatusBadRequest, "query parameter repo is required")
		return
	}
	limit := defaultRunsLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, maxRunsLimit)
	}
	runs, err := s.store.RunsByRepo(r.Context(), repoID, limit)
	if err != nil {
		s.internalError(w, "listing runs", err)
		return
	}
	items := make([]runResponse, 0, len(runs))
	for _, run := range runs {
		items = append(items, runJSON(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": items})
}
