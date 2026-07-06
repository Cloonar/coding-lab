package httpapi

// Instance lifecycle endpoints (pinned M3 contract): start, list (live tmux
// joined with active runs), stop (guarded teardown → removed|parked), and
// stop-all. Business rules live in internal/instance; this file translates
// JSON ⇄ service calls and maps typed errors onto status codes.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// instanceResponse is one GET /api/v1/instances row: the run JSON plus the repo
// name and the two tmux/provider-derived flags. live comes from tmux (NOT the
// DB — rows are history); connecting is the provider's in-flight deep-link
// capture state.
type instanceResponse struct {
	runResponse
	RepoName   string `json:"repo_name"`
	Live       bool   `json:"live"`
	Connecting bool   `json:"connecting"`
}

type instanceCreateRequest struct {
	Label  string `json:"label"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// handleInstanceCreate is POST /api/v1/repos/{id}/instances: 201 with the run,
// or 409 (cap / logged out / repo not ready), 400 (unknown model/effort), 404
// (unknown repo), 500 (worktree/spawn failure — the git cause surfaces).
func (s *Server) handleInstanceCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeOptionalJSON[instanceCreateRequest](w, r)
	if !ok {
		return
	}
	run, err := s.instances.Start(r.Context(), instance.StartParams{
		RepoID: r.PathValue("id"),
		Label:  req.Label,
		Model:  req.Model,
		Effort: req.Effort,
	})
	if err != nil {
		s.writeInstanceError(w, "starting instance", err)
		return
	}
	writeJSON(w, http.StatusCreated, runJSON(run))
}

// handleInstanceList is GET /api/v1/instances.
func (s *Server) handleInstanceList(w http.ResponseWriter, r *http.Request) {
	views, err := s.instances.List(r.Context())
	if err != nil {
		s.internalError(w, "listing instances", err)
		return
	}
	items := make([]instanceResponse, 0, len(views))
	for _, v := range views {
		items = append(items, instanceResponse{
			runResponse: runJSON(v.Run),
			RepoName:    v.RepoName,
			Live:        v.Live,
			Connecting:  v.Connecting,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": items})
}

// handleInstanceDelete is DELETE /api/v1/instances/{session}: 200 with the
// teardown outcome (removed|parked). 404 for an unknown/inactive session; 501
// for an AFK instance (the M5 engine owns AFK Stop).
func (s *Server) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	outcome, err := s.instances.Stop(r.Context(), r.PathValue("session"))
	if err != nil {
		s.writeInstanceError(w, "stopping instance", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"outcome": outcome})
}

// handleStopAll is POST /api/v1/repos/{id}/stop-all: 200 with the count of
// manual instances torn down (the login session and AFK sessions are excluded).
func (s *Server) handleStopAll(w http.ResponseWriter, r *http.Request) {
	n, err := s.instances.StopAll(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeInstanceError(w, "stopping all instances", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"stopped": n})
}

// writeInstanceError maps instance-service errors onto the pinned status codes.
func (s *Server) writeInstanceError(w http.ResponseWriter, doing string, err error) {
	var bad *instance.BadRequestError
	var startFailed *instance.StartFailedError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
	case errors.Is(err, instance.ErrRepoNotReady):
		writeError(w, http.StatusConflict, instance.ErrRepoNotReady.Error())
	case errors.Is(err, instance.ErrOverCap):
		writeError(w, http.StatusConflict, instance.ErrOverCap.Error())
	case errors.Is(err, instance.ErrLoggedOut):
		writeError(w, http.StatusConflict, instance.ErrLoggedOut.Error())
	case errors.Is(err, instance.ErrAFKStopUnsupported):
		writeError(w, http.StatusNotImplemented, instance.ErrAFKStopUnsupported.Error())
	case errors.As(err, &startFailed):
		// The git/spawn cause surfaces verbatim (v0 property) so the operator's
		// banner shows what actually failed, not an opaque 500.
		s.log.Warn(doing, "component", "httpapi", "err", err)
		writeError(w, http.StatusInternalServerError, startFailed.Error())
	default:
		s.internalError(w, doing, err)
	}
}

// decodeOptionalJSON decodes a JSON body into T, tolerating an empty body (the
// instance-create fields are all optional). A malformed non-empty body is a
// 400; ok=false means the response was already written.
func decodeOptionalJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err := dec.Decode(&v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return v, false
	}
	return v, true
}
