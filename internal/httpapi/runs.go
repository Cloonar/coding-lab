package httpapi

// Runs history endpoint (pinned M3 contract: GET /api/v1/runs?repo=<id>&limit=50
// → {runs:[…]} newest first) plus the shared run JSON shape the instance
// listing embeds.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
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
	ID           string  `json:"id"`
	RepoID       string  `json:"repo_id"`
	Kind         string  `json:"kind"`
	Provider     string  `json:"provider"`
	IssueNumber  *int    `json:"issue_number"`
	Branch       string  `json:"branch"`
	WorktreePath string  `json:"worktree_path"`
	SessionName  string  `json:"session_name"`
	Model        string  `json:"model"`
	Effort       string  `json:"effort"`
	DeepLinkURL  *string `json:"deep_link_url"`
	// Remote is the resolved remote-control value stamped on the run at launch
	// (issue #163) — layered, then clamped by the provider's capability, so it
	// is the TRUTH about this session, not a request echo. The SPA gates its
	// Open affordance on it: a non-remote run never captures a deep link, so
	// deep_link_url stays null forever and no Open button should promise one.
	Remote         bool    `json:"remote"`
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

	// CommitsBehind is commits on origin/<base> not yet on this run's branch
	// (the "N behind" badge, issue #149) — read from the bare reference
	// clone's already-fetched local refs via gitx.CommitsBehind, no fetch in
	// the request path; freshness rides the existing fetch cadence.
	// Populated ONLY by handleRunGet (the run detail view) and
	// handleInstanceList (the instances dashboard rows) via
	// Server.commitsBehind; runJSON leaves it 0 for every other caller (the
	// runs list, the title PATCH echo) so listing a repo's run history never
	// pays for a git subprocess per row. omitempty hides the key when 0 or
	// uncomputable (an ended run, a broken/not-yet-cloned repo). base is the
	// repo's default branch until per-run bases (issue #130) exist.
	CommitsBehind int `json:"commits_behind,omitempty"`
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
		Remote:        r.Remote,
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

// commitsBehind is the commits_behind badge source (issue #149):
// origin/<defaultBranch> commits absent from run's branch, read against the
// bare reference clone's local refs (gitx.CommitsBehind never fetches).
// Computed only for a run that can still receive a pull — outcome active,
// the same predicate ActiveRuns filters on — since an ended run's worktree
// and branch may already be torn down and a badge on a dead run would
// mislead. s.git/s.reposDir unset (some test servers) and any git error
// (no bare clone yet, a gone branch) both degrade to 0 rather than failing
// the run/instance response; logged at debug since 0 is an expected,
// routine outcome (e.g. before a repo's first clone completes).
func (s *Server) commitsBehind(ctx context.Context, run store.Run, defaultBranch string) int {
	if run.Outcome != store.RunOutcomeActive {
		return 0
	}
	if s.git == nil || s.reposDir == "" {
		return 0
	}
	bareDir := filepath.Join(s.reposDir, run.RepoID+".git")
	n, err := s.git.CommitsBehind(ctx, bareDir, run.Branch, defaultBranch, s.gitEnv)
	if err != nil {
		s.log.Debug("commits behind", "component", "httpapi", "repo", run.RepoID, "branch", run.Branch, "err", err)
		return 0
	}
	return n
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
// "repoID", optional "runID") so a title PATCH looks like any other run
// mutation to the SSE rails' refetch. A rename concerns exactly one run, so
// it names it (issue #175) — the open chat skips sibling-run events.
type runChangedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
	RunID  string `json:"runID,omitempty"`
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
			Payload: runChangedPayload{Type: instance.EventRunChanged, RepoID: run.RepoID, RunID: run.ID}})
	}
	writeJSON(w, http.StatusOK, runJSON(run))
}
