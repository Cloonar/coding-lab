package httpapi

// AFK operator endpoints (pinned M5 contract): manual start (claim + launch
// the lowest claimable ready issue NOW), the per-repo auto toggle, and the
// three-strikes reset. Business rules live in internal/afk; this file
// translates JSON ⇄ engine calls, maps the typed errors onto the pinned
// status codes (the 409 matrix: paused / no claimable / over cap / logged
// out), and publishes repo.changed on the toggle/reset transitions.

import (
	"errors"
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// publishRepoChanged emits repo.changed for repoID — the same envelope shape
// as every repo-scoped event ({type, repoID}).
func (s *Server) publishRepoChanged(repoID string) {
	s.bus.Publish(events.Event{Type: afk.EventRepoChanged, Payload: struct {
		Type   string `json:"type"`
		RepoID string `json:"repoID"`
	}{Type: afk.EventRepoChanged, RepoID: repoID}})
}

// handleAFKStart is POST /api/v1/repos/{id}/afk/start: 202 {run} — the
// pinned M5 envelope (the SPA unwraps it; instance-create's bare 201 run
// predates it). The 409 matrix carries distinct messages: repo paused after
// three consecutive failures, no claimable ready-for-agent issue,
// live-instance cap reached, provider logged out, repo clone not ready. A
// tracker round-trip failure is a 502 (the forge misbehaved, not lab).
func (s *Server) handleAFKStart(w http.ResponseWriter, r *http.Request) {
	run, err := s.afk.StartManualAFK(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAFKError(w, "starting afk run", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": runJSON(run)})
}

// writeAFKError maps AFK-engine errors onto the pinned status codes; anything
// the engine shares with the instance service (not-found, over-cap, logged-
// out, not-ready, start-failed) falls through to the shared instance mapping.
func (s *Server) writeAFKError(w http.ResponseWriter, doing string, err error) {
	var trkErr *afk.TrackerError
	switch {
	case errors.Is(err, afk.ErrRepoPaused):
		writeError(w, http.StatusConflict, afk.ErrRepoPaused.Error())
	case errors.Is(err, afk.ErrNoReady):
		writeError(w, http.StatusConflict, afk.ErrNoReady.Error())
	case isTrackerConfigError(err):
		// The repo's tracker binding can't be driven (unsupported forge kind,
		// bad/missing forge credential, unparseable remote/host, unknown
		// binding) — a configuration refusal, not a lab fault. Same 409 the
		// issues routes answer, never an opaque 500.
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &trkErr):
		s.log.Warn(doing, "component", "httpapi", "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		s.writeInstanceError(w, doing, err)
	}
}

type afkAutoRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleAFKAuto is PUT /api/v1/repos/{id}/afk/auto {enabled}: 200 with the
// updated repo. Enabling kicks one scheduler sweep immediately (v0 parity —
// the toggle should act now, not up to afk_schedule_seconds later); every
// claim is single-flighted under the engine lock, so the kick is race-safe
// against the ticker.
func (s *Server) handleAFKAuto(w http.ResponseWriter, r *http.Request) {
	var req afkAutoRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required and must be a boolean")
		return
	}
	// Enabling auto on a repo whose tracker binding can never be driven would
	// persist a silently dead loop (every scheduler tick fails at TrackerFor and
	// only logs). Validate the tracker resolves first, refusing with the same
	// 409 the issues/start routes use; disabling is always allowed.
	if *req.Enabled && s.tracker != nil {
		repo, ok := s.loadRepo(w, r)
		if !ok {
			return
		}
		if _, ok := s.trackerForRepo(w, r, repo); !ok {
			return
		}
	}
	repo, err := s.store.UpdateRepoSettings(r.Context(), r.PathValue("id"),
		store.RepoSettingsUpdate{AFKAutoEnabled: store.Set(*req.Enabled)})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "updating afk auto toggle", err)
		return
	}
	s.publishRepoChanged(repo.ID)
	if *req.Enabled {
		// Server-scoped context, not the request's: the sweep outlives this
		// handler and stops with the server.
		go s.afk.ScheduleOnce(s.shutdownCtx)
	}
	eff, err := s.afkPromptEffective(r.Context(), repo)
	if err != nil {
		s.internalError(w, "updating afk auto toggle", err)
		return
	}
	writeJSON(w, http.StatusOK, repoJSON(repo, eff))
}

// handleAFKReset is POST /api/v1/repos/{id}/afk/reset: zero the consecutive-
// failure counter (the ONLY un-pause besides a success reap — ADR-0007, never
// automatic) and answer 200 with the repo. repo.changed is published only on
// a real transition; a re-armed auto loop gets one immediate scheduler kick.
func (s *Server) handleAFKReset(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	changed, err := s.store.ResetRepoFailures(r.Context(), repo.ID)
	if err != nil {
		s.internalError(w, "resetting afk failures", err)
		return
	}
	repo.ConsecutiveFailures = 0
	if changed {
		s.publishRepoChanged(repo.ID)
		go s.afk.ScheduleOnce(s.shutdownCtx)
	}
	eff, err := s.afkPromptEffective(r.Context(), repo)
	if err != nil {
		s.internalError(w, "resetting afk failures", err)
		return
	}
	writeJSON(w, http.StatusOK, repoJSON(repo, eff))
}
