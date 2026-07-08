package httpapi

// Change-request surface (pinned M6 contract; brief §8.1, D11). A change
// request is the lab-internal PR of a BUILTIN-bound repo, so every /crs route
// answers 409 on a forge-bound repo — its PRs live on the forge, exactly like
// the issue mutations. Reads come straight from the store (the CR rows are
// lab's own data, no tracker indirection needed); the detail view computes
// the diff live from the bare repo via gitx.CRDiff; merge and close delegate
// to the shared crmerge.Service (ADR-0011) — the same orchestration the agent
// surface's MergePull reuses — so the per-CR serialization, cancellation-
// immune git window, author identity, per-op credential, Closes #N closure,
// and cr.changed/issue.changed events live in ONE place. These handlers are
// the thin operator-side error map onto the M6 status codes.

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"git.cloonar.com/Cloonar/coding-lab/internal/crmerge"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// EventCRChanged is the SSE event name published on any change-request
// mutation (brief §8.1). Creation happens on the agent API (builtin PR
// create), which publishes the same event name from its own package.
const EventCRChanged = "cr.changed"

// forgeCRMessage is the pinned 409 body for /crs routes on a forge-bound
// repo — the sibling of forgeTrackerMessage, but pointing at pull requests.
const forgeCRMessage = "repository uses a forge tracker; manage pull requests on the forge"

// crResponse is the pinned list-view CR JSON: {number, title, state,
// head_branch, base_branch, closes, created_at, merged_at, merge_commit}.
type crResponse struct {
	Number      int     `json:"number"`
	Title       string  `json:"title"`
	State       string  `json:"state"`
	HeadBranch  string  `json:"head_branch"`
	BaseBranch  string  `json:"base_branch"`
	Closes      []int   `json:"closes"`
	CreatedAt   string  `json:"created_at"`
	MergedAt    *string `json:"merged_at"`
	MergeCommit *string `json:"merge_commit"`
}

// crFullResponse adds the body — the detail view and the {cr} envelope the
// merge/close mutations answer with (the SPA shows the body it just acted on
// without a refetch).
type crFullResponse struct {
	crResponse
	Body string `json:"body"`
}

// crDetailResponse is the GET /crs/{n} shape: the full CR plus the live diff.
// Exactly one of the two tails is present: Diff+DiffTruncated when the diff
// computed, Note when the head branch no longer resolves (discarded from
// Parked, or swept after a merge) — the pinned diff-omitted-with-a-note path.
type crDetailResponse struct {
	crFullResponse
	Diff          *string `json:"diff,omitempty"`
	DiffTruncated *bool   `json:"diff_truncated,omitempty"`
	Note          string  `json:"note,omitempty"`
}

func crJSON(cr store.CR) crResponse {
	closes := cr.Closes
	if closes == nil {
		closes = []int{}
	}
	out := crResponse{
		Number:     cr.Number,
		Title:      cr.Title,
		State:      cr.State,
		HeadBranch: cr.HeadBranch,
		BaseBranch: cr.BaseBranch,
		Closes:     closes,
		CreatedAt:  store.FormatTime(cr.CreatedAt),
	}
	if cr.MergedAt != nil {
		v := store.FormatTime(*cr.MergedAt)
		out.MergedAt = &v
	}
	out.MergeCommit = cr.MergeCommit
	return out
}

func crFullJSON(cr store.CR) crFullResponse {
	return crFullResponse{crResponse: crJSON(cr), Body: cr.Body}
}

// crBareDir is the repo's bare reference clone (design §7) — the same
// derivation as reposvc.bareDir, on the ReposDir this server was given.
func (s *Server) crBareDir(repoID string) string {
	return filepath.Join(s.reposDir, repoID+".git")
}

// requireBuiltinCRs answers the pinned 409 when repo is not builtin-bound:
// change requests exist only in the built-in tracker; a forge repo's PRs are
// reviewed and merged on the forge. ok=false means the response was written.
func (s *Server) requireBuiltinCRs(w http.ResponseWriter, repo store.Repo) bool {
	if repo.TrackerBinding != store.TrackerBindingBuiltin {
		writeError(w, http.StatusConflict, forgeCRMessage)
		return false
	}
	return true
}

// handleCRList is GET /api/v1/repos/{id}/crs?state=open|merged|closed|all.
// state defaults to all (the CR list is small and the SPA filters client-side
// with explicit chips); an unknown state is a 400.
func (s *Server) handleCRList(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinCRs(w, repo) {
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", store.CRStateAll:
		state = store.CRStateAll
	case store.CRStateOpen, store.CRStateMerged, store.CRStateClosed:
	default:
		writeError(w, http.StatusBadRequest, "state must be open, merged, closed, or all")
		return
	}
	crs, err := s.store.CRsByRepo(r.Context(), repo.ID, state)
	if err != nil {
		s.internalError(w, "listing change requests", err)
		return
	}
	items := make([]crResponse, 0, len(crs))
	for _, cr := range crs {
		items = append(items, crJSON(cr))
	}
	writeJSON(w, http.StatusOK, map[string]any{"crs": items})
}

// loadCR resolves the {n} path segment to the repo's CR, answering 404/500
// itself. ok=false means the response was already written.
func (s *Server) loadCR(w http.ResponseWriter, r *http.Request, repo store.Repo) (store.CR, bool) {
	n, ok := issueNumber(r) // same {n} grammar as the issue routes
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return store.CR{}, false
	}
	cr, err := s.store.CRByRepoNumber(r.Context(), repo.ID, n)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			s.internalError(w, "loading change request", err)
		}
		return store.CR{}, false
	}
	return cr, true
}

// handleCRGet is GET /api/v1/repos/{id}/crs/{n}: the CR in full plus the diff
// computed LIVE from the bare repo (three-dot merge-base semantics, bounded;
// gitx.CRDiff). A head branch that no longer resolves omits the diff with a
// note instead of failing the page; note that a MERGED CR whose head branch
// still exists diffs empty once the merge has been fetched — CRDiff reads the
// local origin ref by design.
func (s *Server) handleCRGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinCRs(w, repo) {
		return
	}
	cr, ok := s.loadCR(w, r, repo)
	if !ok {
		return
	}
	resp := crDetailResponse{crFullResponse: crFullJSON(cr)}
	diff, truncated, err := s.git.CRDiff(r.Context(), s.crBareDir(repo.ID), cr.BaseBranch, cr.HeadBranch, s.gitEnv)
	switch {
	case err == nil:
		resp.Diff = &diff
		resp.DiffTruncated = &truncated
	case errors.Is(err, gitx.ErrHeadMissing):
		resp.Note = fmt.Sprintf("head branch %s no longer exists; diff unavailable", cr.HeadBranch)
	default:
		s.internalError(w, "computing change-request diff", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCRMerge is POST /api/v1/repos/{id}/crs/{n}/merge — the operator entry
// to the shared CR-merge orchestration (crmerge.Service, ADR-0011). The
// service owns the whole contract: per-CR serialization, cancellation-immune
// git window, author identity, per-op credential, gitx.CRMerge against the
// real origin, store.MergeCR, best-effort Closes #N closure, and the
// cr.changed/issue.changed events. This handler is the thin operator-side
// error map: a non-open CR (and a store race that becomes one) stays 409 with
// the state, gitx's typed refusals surface their own words as the 409 body,
// and the no-author-identity refusal is a 409 too — exactly the M6 status
// codes, now shared byte-for-byte with the agent surface.
func (s *Server) handleCRMerge(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinCRs(w, repo) {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	merged, err := s.crmerge.Merge(r.Context(), repo.ID, n)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		case errors.Is(err, gitx.ErrHeadMissing), errors.Is(err, gitx.ErrPushRejected),
			errors.Is(err, gitx.ErrMergeConflict), errors.Is(err, store.ErrCRNotOpen),
			errors.Is(err, crmerge.ErrNoAuthorIdentity):
			writeError(w, http.StatusConflict, err.Error())
		default:
			s.internalError(w, "merging change request", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cr": crFullJSON(merged)})
}

// handleCRClose is POST /api/v1/repos/{id}/crs/{n}/close: open → closed
// (closed-unmerged — the head branch and its parked work are untouched; a
// closed CR reads as "no PR" to the reaper's done-signal, tracker.PRPresent).
func (s *Server) handleCRClose(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	if !s.requireBuiltinCRs(w, repo) {
		return
	}
	n, ok := issueNumber(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	// crmerge.Close shares the merge mutex, so a close never lands inside a
	// concurrent merge's git window (see handleCRMerge / ADR-0011).
	cr, err := s.crmerge.Close(r.Context(), repo.ID, n)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrCRNotOpen):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		default:
			s.internalError(w, "closing change request", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cr": crFullJSON(cr)})
}
