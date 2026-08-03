package httpapi

// Human re-arm of an escalated PR (issue #188 / ADR-0048's amendment).
//
// Escalation stopped being a permanent property of a PR and became a
// statement about a MOMENT: "as of these N attempts, agents could not
// finish this." A human re-arm is the one gesture that supersedes that
// statement — store.RearmPull records the supersession instant AND zeroes
// the PR's fix/escalate attempt budgets, atomically, so the terminality
// gate (internal/afk, comparing an escalation signal's instant against
// store.PullRearmedAt) and the attempt bounds move together. This file's
// only job is translating the HTTP gesture into that one store call; the
// fold that makes supersession actually work lives in internal/afk, not
// here.
//
// PLACEMENT IS LOAD-BEARING, NOT INCIDENTAL. handleAutolandRearm is
// registered in server.go inside the `if s.afk != nil` block on the `api`
// mux, wrapped in s.requireAuth exactly like its afk/* neighbours — which
// means it inherits authMiddleware + csrfMiddleware + requireAuth via
// Handler()'s `operator := s.authMiddleware(s.csrfMiddleware(...))` chain.
// The run-token agent surface (internal/agentapi, mounted separately as
// `root.Handle("/agent/v1/", s.agent)`) is a COMPLETELY DISJOINT mux with
// its own run-token auth and no CSRF at all (see Handler()'s comment: "the
// agent API mounts its own run-token auth ... and bypasses the operator
// middleware entirely"). Re-arm is never registered there, structurally —
// not filtered out, never present — which is the whole point: escalation
// means "agents could not finish this," and an agent able to lift its own
// terminal hand-off would make the bound decorative. Do NOT add this to
// internal/agentapi and do NOT add a labctl (run-token) verb for it, ever.
import (
	"errors"
	"net/http"
	"strconv"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// autolandRearmResponse is the re-arm response: just enough for the SPA/CLI
// caller to confirm the gesture landed (repo, pull, and the moment it took
// effect) without a follow-up fetch.
type autolandRearmResponse struct {
	RepoID     string `json:"repo_id"`
	PullNumber int    `json:"pull_number"`
	RearmedAt  string `json:"rearmed_at"`
}

// handleAutolandRearm is POST /api/v1/repos/{id}/autoland/pulls/{pull}/rearm.
// Modeled on handleAFKReset (afk.go) — the sibling human-triggered repo
// action that un-sticks an automatic gate (there: the three-strikes pause;
// here: escalation terminality) — down to the immediate spawn-pass kick and
// the repo.changed publish so the human's gesture takes effect NOW, not on
// the next poller tick.
//
// Re-arm is idempotent from the caller's view: re-arming a PR that was
// never escalated, or one that is already re-armed, is a legal no-op that
// just moves the re-arm moment forward — never a 409. The gate this feeds
// is relational (terminal iff an escalation signal's instant is AFTER the
// last re-arm), so there is no invalid prior state to reject; there is only
// ever a new instant to record, and store.RearmPull's upsert already
// embodies that ("re-arm is indefinitely repeatable and only the newest
// gesture matters").
func (s *Server) handleAutolandRearm(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	// strconv.Atoi + n<1, the package's idiom for a numeric path segment
	// (see issueNumber in issues.go) — except a bad value here is a 400, not
	// a 404: {id} names a real thing that may not exist (404 is right), but
	// {pull} is caller input shaped wrong, which is a 400 regardless of
	// whether some pull with that number exists.
	pull, err := strconv.Atoi(r.PathValue("pull"))
	if err != nil || pull < 1 {
		writeError(w, http.StatusBadRequest, "pull must be a positive integer")
		return
	}

	at := s.now()
	if err := s.store.RearmPull(r.Context(), repo.ID, pull, at); err != nil {
		// ErrNotFound here is the repo vanishing between loadRepo and this
		// write (RearmPull's FK check on autoland_rearms) — defensive, not
		// the ordinary path, since loadRepo already authorized against a
		// repo that existed moments ago. Anything else is a genuine store
		// fault.
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "rearming pull", err)
		return
	}

	// repo.changed lets the SPA refresh the run/PR row immediately (the
	// escalated-run row is where the re-arm action lives) instead of
	// waiting on the slower schedule cadence; the spawn-pass kick lets a
	// re-armed PR that is otherwise claimable get picked up this instant
	// rather than up to afk_schedule_seconds later — the same reasoning
	// handleAFKReset's post-reset kick uses for the three-strikes pause.
	s.publishRepoChanged(repo.ID)
	go s.afk.SpawnOnce(s.shutdownCtx)

	writeJSON(w, http.StatusOK, autolandRearmResponse{
		RepoID:     repo.ID,
		PullNumber: pull,
		// store.FormatTime, not at.Format(...) or a re-read: the caller must
		// see the stored value byte-for-byte (store.FormatTime's documented
		// rule). Formatting `at` directly is still exact — fmtTime's layout
		// prints exactly 3 fractional digits, which TRUNCATES rather than
		// rounds, so it agrees with RearmPull's internal storedTime(at)
		// (also a millisecond truncation) to the byte; no round-trip read
		// is needed to guarantee the match.
		RearmedAt: store.FormatTime(at),
	})
}
