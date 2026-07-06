package httpapi

// Change-request surface (pinned M6 contract; brief §8.1, D11). A change
// request is the lab-internal PR of a BUILTIN-bound repo, so every /crs route
// answers 409 on a forge-bound repo — its PRs live on the forge, exactly like
// the issue mutations. Reads come straight from the store (the CR rows are
// lab's own data, no tracker indirection needed); the detail view computes
// the diff live from the bare repo via gitx.CRDiff; merge is the small
// orchestration pinned by the contract: credential env from the vault
// materializer (per-op, cleaned after), author identity from the repo
// override else global settings (D15 measure 5 — the repo's REAL identity,
// never a bot), gitx.CRMerge against the repo's real origin, store.MergeCR,
// then best-effort closing of every `Closes #N` built-in issue. Every CR
// mutation publishes cr.changed on the bus (clients refetch on event).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// EventCRChanged is the SSE event name published on any change-request
// mutation (brief §8.1). Creation happens on the agent API (builtin PR
// create), which publishes the same event name from its own package.
const EventCRChanged = "cr.changed"

// forgeCRMessage is the pinned 409 body for /crs routes on a forge-bound
// repo — the sibling of forgeTrackerMessage, but pointing at pull requests.
const forgeCRMessage = "repository uses a forge tracker; manage pull requests on the forge"

// noAuthorIdentityMessage answers a merge attempted before any git author
// identity exists. CRMerge's merge commit is authored per D15 measure 5 —
// the repo's configured REAL identity, never a fallback bot identity — so
// with neither the repo fields nor the global settings configured there is
// nothing lab may author as; refusing up front (409, configuration conflict)
// beats an opaque git failure halfway through the merge.
const noAuthorIdentityMessage = "git author identity is not configured; set git_author_name and git_author_email in settings (or on the repository)"

// publishCRChanged emits cr.changed for repoID. Same envelope shape as every
// repo-scoped event ({type, repoID}).
func (s *Server) publishCRChanged(repoID string) {
	s.bus.Publish(events.Event{Type: EventCRChanged, Payload: struct {
		Type   string `json:"type"`
		RepoID string `json:"repoID"`
	}{Type: EventCRChanged, RepoID: repoID}})
}

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

// handleCRMerge is POST /api/v1/repos/{id}/crs/{n}/merge — the M6 merge
// orchestration (pinned contract):
//
//  1. builtin-bound repo, existing OPEN CR (non-open → 409 with the state);
//  2. author identity: repo git_author_name/email else the global settings
//     pair (both required — see noAuthorIdentityMessage);
//  3. the repo's git credential materialized per-op (opID crmerge-<crID>),
//     removed again when the merge returns;
//  4. gitx.CRMerge against the repo's REAL origin: ErrHeadMissing and
//     ErrPushRejected are 409s whose body is the error text verbatim (the
//     push-rejection stderr IS what the operator needs to read);
//  5. past the push there is no return: store.MergeCR, the best-effort close
//     of every cr_closes built-in issue (per-issue, log failures), and the
//     cr.changed/issue.changed events run on a cancellation-immune context —
//     a dropped connection must not strand a pushed merge unrecorded. If a
//     concurrent merge won the store race (ErrCRNotOpen), git has already
//     converged (CRMerge invariant) and the 409 tells the operator the truth.
func (s *Server) handleCRMerge(w http.ResponseWriter, r *http.Request) {
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

	// Serialize against a concurrent close/merge of the SAME CR: the git
	// merge below is a seconds-wide network operation, and a close landing
	// inside it would strand origin merged while the row reads
	// closed-unmerged (no reopen exists). Under the lock, re-read the state
	// so a mutation that won the lock first is seen.
	unlock := s.crMu.lock(cr.ID)
	defer unlock()

	// The merge is not abortable once we commit to it: a phone dropping the
	// connection after the push but before the bookkeeping must not strand a
	// pushed merge unrecorded (or skip gitx's refresh fetch). Everything from
	// here runs cancellation-immune.
	ctx := context.WithoutCancel(r.Context())

	cr, err := s.store.CRByRepoNumber(ctx, repo.ID, cr.Number)
	if err != nil {
		s.internalError(w, "reloading change request", err)
		return
	}
	if cr.State != store.CRStateOpen {
		writeError(w, http.StatusConflict, fmt.Sprintf("change request is not open (state %q)", cr.State))
		return
	}

	authorName, authorEmail, err := s.crAuthorIdentity(ctx, repo)
	if err != nil {
		s.internalError(w, "resolving git author identity", err)
		return
	}
	if authorName == "" || authorEmail == "" {
		writeError(w, http.StatusConflict, noAuthorIdentityMessage)
		return
	}

	credEnv, cleanup, err := s.crCredentialEnv(ctx, repo, "crmerge-"+cr.ID)
	if err != nil {
		s.internalError(w, "materializing git credential", err)
		return
	}
	defer cleanup()

	env := append(append([]string{}, s.gitEnv...), credEnv...)
	message := fmt.Sprintf("Merge change request #%d: %s", cr.Number, cr.Title)
	mergeCommit, err := s.git.CRMerge(ctx, s.crBareDir(repo.ID),
		cr.BaseBranch, cr.HeadBranch, message, authorName, authorEmail, env)
	if err != nil {
		switch {
		case errors.Is(err, gitx.ErrHeadMissing), errors.Is(err, gitx.ErrPushRejected),
			errors.Is(err, gitx.ErrMergeConflict):
			writeError(w, http.StatusConflict, err.Error())
		default:
			s.internalError(w, "merging change request", err)
		}
		return
	}

	// The merge is on origin. Record it, close the closes-issues, publish.
	merged, err := s.store.MergeCR(ctx, repo.ID, cr.Number, mergeCommit, s.now())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrCRNotOpen):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		default:
			s.internalError(w, "recording change-request merge", err)
		}
		return
	}

	closedAny := false
	for _, n := range merged.Closes {
		if _, err := s.store.UpdateIssue(ctx, repo.ID, n,
			store.IssueUpdate{State: store.Set(store.IssueStateClosed)}, s.now()); err != nil {
			s.log.Warn("closing issue for merged change request",
				"component", "httpapi", "repo", repo.ID, "cr", merged.Number, "issue", n, "err", err)
			continue
		}
		closedAny = true
	}
	if closedAny {
		s.publishIssueChanged(repo.ID)
	}
	s.publishCRChanged(repo.ID)
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
	target, ok := s.loadCR(w, r, repo)
	if !ok {
		return
	}
	// Same per-CR lock as merge: a close must not land inside a concurrent
	// merge's git window (see handleCRMerge).
	unlock := s.crMu.lock(target.ID)
	defer unlock()
	cr, err := s.store.CloseCR(r.Context(), repo.ID, target.Number, s.now())
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
	s.publishCRChanged(repo.ID)
	writeJSON(w, http.StatusOK, map[string]any{"cr": crFullJSON(cr)})
}

// crAuthorIdentity resolves the merge commit's author/committer identity:
// the repo's git_author_name/email override the global settings pair,
// field-by-field (same layering as the spawned-session authorEnv in
// internal/instance). Empty results mean "not configured" — the caller
// refuses the merge rather than authoring as nobody or as a bot.
func (s *Server) crAuthorIdentity(ctx context.Context, repo store.Repo) (name, email string, err error) {
	if repo.GitAuthorName != nil {
		name = strings.TrimSpace(*repo.GitAuthorName)
	}
	if repo.GitAuthorEmail != nil {
		email = strings.TrimSpace(*repo.GitAuthorEmail)
	}
	if name == "" {
		if name, err = s.store.GetString(ctx, store.SettingGitAuthorName, ""); err != nil {
			return "", "", err
		}
	}
	if email == "" {
		if email, err = s.store.GetString(ctx, store.SettingGitAuthorEmail, ""); err != nil {
			return "", "", err
		}
	}
	// Whitespace-only values are "not configured": git strips whitespace
	// idents and rejects the commit with an opaque "empty ident name" —
	// catching it here keeps the up-front 409 honest.
	return strings.TrimSpace(name), strings.TrimSpace(email), nil
}

// keyedMutex is a per-key mutual exclusion helper: lock(key) blocks while
// another holder owns the same key and returns the unlock func. Entries are
// tiny and never evicted — key cardinality is bounded by real CR ids.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// crCredentialEnv materializes the repo's GIT credential for one merge op
// (design §6) and returns the git env entries plus a cleanup removing exactly
// this op's files. A repo without a credential yields no env and a no-op
// cleanup (a filesystem/local remote needs none). Mirrors the per-service
// credentialEnv helpers in reposvc and instance — the same §6 wiring, kept
// local so the merge path stays a plain httpapi orchestration.
func (s *Server) crCredentialEnv(ctx context.Context, repo store.Repo, opID string) (env []string, cleanup func(), err error) {
	noop := func() {}
	if repo.CredentialID == nil {
		return nil, noop, nil
	}
	if s.vault == nil || s.mat == nil {
		return nil, noop, errors.New("credentialed merge without a vault/materializer wired")
	}
	cred, err := s.store.CredentialByID(ctx, *repo.CredentialID)
	if err != nil {
		return nil, noop, err
	}
	remove := func() {
		if err := s.mat.Cleanup(cred.ID, opID); err != nil {
			s.log.Warn("cleaning materialized credential", "component", "httpapi", "err", err)
		}
	}
	switch cred.Kind {
	case store.CredentialKindSSHKey:
		var p vault.SSHKeyPayload
		if err := s.vault.DecryptPayload(cred.EncryptedPayload, &p); err != nil {
			return nil, noop, err
		}
		keyPath, sshAskpass, err := s.mat.MaterializeSSHKey(cred.ID, opID, p)
		if err != nil {
			remove()
			return nil, noop, err
		}
		if sshAskpass != "" {
			return vault.SSHEnvWithPassphrase(keyPath, s.mat.KnownHostsPath(), sshAskpass), remove, nil
		}
		return vault.SSHEnv(keyPath, s.mat.KnownHostsPath()), remove, nil
	case store.CredentialKindHTTPSToken:
		var p vault.HTTPSTokenPayload
		if err := s.vault.DecryptPayload(cred.EncryptedPayload, &p); err != nil {
			return nil, noop, err
		}
		askpass, err := s.mat.MaterializeAskpass(cred.ID, opID, p)
		if err != nil {
			remove()
			return nil, noop, err
		}
		return vault.HTTPSEnv(askpass), remove, nil
	default:
		// forge_token never authenticates git (design §3a).
		return nil, noop, fmt.Errorf("credential kind %s cannot authenticate git", cred.Kind)
	}
}
