package httpapi

// Runs history endpoint (pinned M3 contract: GET /api/v1/runs?repo=<id>&limit=50
// → {runs:[…]} newest first) plus the shared run JSON shape the instance
// listing embeds.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
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
	Title          *string `json:"title"` // user-set display overlay (issue #111)
	StartedAt      string  `json:"started_at"`
	BudgetDeadline *string `json:"budget_deadline"`
	EndedAt        *string `json:"ended_at"`
	Outcome        string  `json:"outcome"`
	FailureReason  *string `json:"failure_reason"`

	// ExposedSecrets lists the names of this repo's secrets whose value has
	// appeared in THIS run's transcript (issue #108's chat-header warning
	// badge) — sorted, from store.ExposedSecretNamesForRun. Populated ONLY by
	// handleRunGet, the chat header's source; runJSON leaves it nil for every
	// other caller (the runs list, the title PATCH echo) so listing a repo's
	// run history never pays for an exposure lookup per row. omitempty keeps
	// the key entirely absent everywhere but the one response that sets it.
	ExposedSecrets []string `json:"exposed_secrets,omitempty"`
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
		Title:         r.Title,
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

// runTitleMaxRunes caps a run title (issue #111). Counted in runes, not bytes:
// the title is free display text that never reaches git/tmux/paths.
const runTitleMaxRunes = 120

// runChangedPayload mirrors instance's repoScopedPayload key-for-key ("type",
// "repoID") so a title PATCH looks like any other run mutation to the SSE
// rails' refetch.
type runChangedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
}

// handleRunUpdate is PATCH /api/v1/runs/{id}. The only known field is title —
// the user-set display overlay (issue #111): a string sets it (trimmed;
// whitespace-only clears), JSON null clears, absent leaves it untouched, and
// identity (session name, branch, worktree, tmux) never changes. Raw JSON per
// field keeps null and absent distinguishable, like the repos PATCH.
func (s *Server) handleRunUpdate(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if decodeJSON(w, r, &body) != nil {
		return
	}
	var title *string
	setTitle := false
	for key, raw := range body {
		switch key {
		case "title":
			var v *string
			if err := json.Unmarshal(raw, &v); err != nil {
				writeError(w, http.StatusBadRequest, "field title must be a string or null")
				return
			}
			if v != nil {
				t := strings.TrimSpace(*v)
				if utf8.RuneCountInString(t) > runTitleMaxRunes {
					writeError(w, http.StatusBadRequest,
						fmt.Sprintf("title must be at most %d characters", runTitleMaxRunes))
					return
				}
				if t != "" {
					title = &t
				}
			}
			setTitle = true
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown field %q", key))
			return
		}
	}
	id := r.PathValue("id")
	if setTitle {
		if err := s.store.UpdateRunTitle(r.Context(), id, title); err != nil {
			s.writeRunError(w, "updating run", err)
			return
		}
	}
	run, err := s.store.RunByID(r.Context(), id)
	if err != nil {
		s.writeRunError(w, "updating run", err)
		return
	}
	if setTitle {
		s.bus.Publish(events.Event{Type: instance.EventRunChanged,
			Payload: runChangedPayload{Type: instance.EventRunChanged, RepoID: run.RepoID}})
	}
	writeJSON(w, http.StatusOK, runJSON(run))
}
