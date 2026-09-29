package httpapi

// The per-repo SSH targets picker's server side (issue #39 / ADR-0068): the
// analogue of onecli_grants.go for the bastion. A repo's runs reach exactly
// the Warpgate SSH targets that carry the repo's Warpgate ROLE; the picker
// lists Warpgate's SSH targets, marks which carry that role, and a toggle
// assigns or removes the role on one target through the admin API. Targets
// themselves — and the credentials they hold — are the operator's, defined in
// Warpgate's own admin UI; nothing here creates, edits or reads one, and a
// target's credential never reaches this package at all (warpgate.Target
// carries id, name and description and nothing else).
//
// Why lab proxies, as for OneCLI: the admin token is a credential that must
// never reach a browser, and the admin API is loopback-or-internal by design.
//
// Properties pinned here, each mirroring the OneCLI picker:
//
//   - A READ writes nothing to Warpgate. The listing resolves the repo's
//     identity with FindRepoIdentity; only the assign — an explicit operator
//     mutation — may call EnsureRepoIdentity. Opening the picker for every
//     repo must not litter Warpgate with a user and a role per repo anyone
//     ever looked at.
//   - "Not configured" is an ANSWER for the read (200, configured:false,
//     targets:[]) and an ERROR for the mutations (409).
//   - A Warpgate failure is a 502 naming Warpgate (writeWarpgateError), the
//     same mapping as writeOneCLIGatewayError: what broke is the upstream lab
//     proxies. Listing is the one Warpgate call that degrades to an error
//     message in the picker rather than failing closed (ADR-0068) — it is a
//     screen, and a screen that cannot load is not a security event.
//   - The toggle touches SSH targets only: an assign whose target id is not in
//     Warpgate's SSH listing is refused (404) before anything is written, so
//     the repo's role can never land on an HTTP or database target lab does
//     not wire and nobody reviewed for this.
//
// And one property the OneCLI picker does not have — the lab-side cache,
// repo_ssh_targets (ADR-0068's implementation pins). The spawn path decides
// from it, without calling Warpgate, whether a repo has any SSH target at all,
// so that a Warpgate outage never blocks a repo that has none. It is never
// authoritative (Warpgate enforces the role on every connection regardless):
// the listing REPLACES the repo's cached set with the assigned subset of what
// it just read, and each toggle adds or removes its one row after Warpgate
// accepted the change.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// warpgateNotConfiguredMessage is what a toggle answers when the REST pair is
// unset. It names the two flags that turn the integration on, because the
// operator reading the toast is the one who can set them.
const warpgateNotConfiguredMessage = "the Warpgate SSH bastion is not configured on this lab; set --warpgate-url and --warpgate-admin-token-file to assign SSH targets"

// WarpgateAPI is the Warpgate admin REST seam this package drives: health,
// the repo identity (read-only find, and the ensure only the assign uses),
// the SSH target listings, and the role toggle. *warpgate.Client satisfies
// it; the assertion below is the compile-time proof. It deliberately has no
// key or delete method — the per-run keys and the identity's removal belong
// to the spawn path and the repo lifecycle, and a handler here structurally
// cannot reach them.
type WarpgateAPI interface {
	Health(ctx context.Context) (warpgate.Info, error)
	FindRepoIdentity(ctx context.Context, repoID string) (warpgate.Identity, bool, error)
	EnsureRepoIdentity(ctx context.Context, repoID, repoName string) (warpgate.Identity, error)
	ListSSHTargets(ctx context.Context) ([]warpgate.Target, error)
	RoleSSHTargets(ctx context.Context, roleID string) ([]warpgate.Target, error)
	AssignTargetRole(ctx context.Context, targetID, roleID string) error
	UnassignTargetRole(ctx context.Context, targetID, roleID string) error
}

var _ WarpgateAPI = (*warpgate.Client)(nil)

// warpgateTargetEntry is one SSH target as the picker renders it. Display
// metadata and the assignment bit only — never an option or a credential,
// which lab does not hold.
type warpgateTargetEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Assigned    bool   `json:"assigned"`
}

// warpgateTargetsResponse is GET /api/v1/repos/{id}/warpgate/targets's one
// body shape. Targets is ALWAYS an array, never null; Configured is what
// separates "Warpgate has no SSH targets" from "Warpgate is not set up".
type warpgateTargetsResponse struct {
	Configured bool                  `json:"configured"`
	Targets    []warpgateTargetEntry `json:"targets"`
}

// handleWarpgateTargetList is GET /api/v1/repos/{id}/warpgate/targets: every
// Warpgate SSH target, in the client's order (by name, then id), each marked
// with whether the repo's role carries it.
func (s *Server) handleWarpgateTargetList(w http.ResponseWriter, r *http.Request) {
	// The repo is resolved FIRST, so an unknown id is a 404 whether or not the
	// integration is configured (a stale link must not look like an empty
	// picker).
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	api := s.warpgate
	if api == nil {
		writeJSON(w, http.StatusOK, warpgateTargetsResponse{Configured: false, Targets: []warpgateTargetEntry{}})
		return
	}
	ctx := r.Context()

	// Read-only: no identity yet is the normal state of a repo until its first
	// assign or target-bearing spawn, and emphatically not a reason to create
	// one.
	identity, _, err := api.FindRepoIdentity(ctx, repo.ID)
	if err != nil {
		s.writeWarpgateError(w, "resolving the repo's Warpgate identity", err)
		return
	}
	targets, err := api.ListSSHTargets(ctx)
	if err != nil {
		s.writeWarpgateError(w, "listing Warpgate's SSH targets", err)
		return
	}

	// The ROLE is what carries targets, so its existence alone decides whether
	// there are assignments to read — a user missing beside it (FindRepoIdentity's
	// found=false with a partial identity) changes nothing about which targets
	// the role holds, and the next ensure recreates the user.
	assigned := map[string]bool{}
	cache := []store.SSHTarget{}
	if identity.Role.ID != "" {
		roleTargets, err := api.RoleSSHTargets(ctx, identity.Role.ID)
		if err != nil {
			s.writeWarpgateError(w, "listing the SSH targets assigned to the repo's Warpgate role", err)
			return
		}
		for _, t := range roleTargets {
			assigned[t.ID] = true
			cache = append(cache, sshTargetRow(t))
		}
	}

	// This was a full read of the truth, so it REPLACES the repo's cached set
	// — an empty set included, which is how an assignment removed in Warpgate's
	// own UI stops making spawns call Warpgate. A failed write is logged, not
	// fatal: the operator asked to see the picker, Warpgate answered, and the
	// cache is corrected at the next read of the truth anyway.
	if err := s.store.ReplaceRepoSSHTargets(ctx, repo.ID, cache); err != nil {
		s.log.Warn("refreshing the repo's cached Warpgate SSH targets", "component", "httpapi", "repo", repo.ID, "err", err)
	}

	out := warpgateTargetsResponse{Configured: true, Targets: make([]warpgateTargetEntry, 0, len(targets))}
	for _, t := range targets {
		out.Targets = append(out.Targets, warpgateTargetEntry{ID: t.ID, Name: t.Name, Description: t.Description, Assigned: assigned[t.ID]})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWarpgateTargetAssign is PUT
// /api/v1/repos/{id}/warpgate/targets/{targetId}: 204, no body.
//
// The target is checked against Warpgate's SSH listing BEFORE the identity is
// ensured, so a refused id (404) has no side effect at all — not even a user
// and role created for a repo whose assign never happened. Then this is the
// ONE path here that may create the repo's identity (or heal its description
// to the repo's current name): an identity exists once someone decides the
// repo should reach something, which is exactly this click. Every step is
// idempotent (the ensure, and the assign, whose "already assigned" is
// success), so replaying a whole selection is safe.
func (s *Server) handleWarpgateTargetAssign(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	targetID, ok := warpgateTargetID(w, r)
	if !ok {
		return
	}
	api := s.warpgate
	if api == nil {
		writeError(w, http.StatusConflict, warpgateNotConfiguredMessage)
		return
	}
	ctx := r.Context()

	targets, err := api.ListSSHTargets(ctx)
	if err != nil {
		s.writeWarpgateError(w, "listing Warpgate's SSH targets", err)
		return
	}
	var target warpgate.Target
	found := false
	for _, t := range targets {
		if t.ID == targetID {
			target, found = t, true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("target %q is not an SSH target known to Warpgate", targetID))
		return
	}

	identity, err := api.EnsureRepoIdentity(ctx, repo.ID, repo.Name)
	if err != nil {
		s.writeWarpgateError(w, "ensuring the repo's Warpgate identity", err)
		return
	}
	if err := api.AssignTargetRole(ctx, target.ID, identity.Role.ID); err != nil {
		s.writeWarpgateError(w, "assigning the Warpgate SSH target to the repo's role", err)
		return
	}
	// Warpgate — the truth — has the assignment; the cache follows. A failed
	// write here is lab's own fault and is reported as one: the toggle's
	// write is the cache's whole point, and the next listing (which the SPA
	// issues after a toggle) replaces the cached set anyway.
	if err := s.store.AddRepoSSHTarget(ctx, repo.ID, sshTargetRow(target)); err != nil {
		s.internalError(w, "caching the repo's Warpgate SSH target", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWarpgateTargetUnassign is DELETE
// /api/v1/repos/{id}/warpgate/targets/{targetId}: 204, no body.
//
// Read-only resolution, never EnsureRepoIdentity: creating an identity in
// order to take something away from it would turn "clear everything" into a
// machine that manufactures identities. No identity (or no role) means there
// is nothing to unassign in Warpgate — the state the caller asked for already
// holds — but the cached row is removed regardless, so a stale cache entry is
// cleared by the very click meant to clear it. Unlike the assign, the target
// is not checked against the SSH listing: taking the repo's role OFF a target
// can only narrow access.
func (s *Server) handleWarpgateTargetUnassign(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	targetID, ok := warpgateTargetID(w, r)
	if !ok {
		return
	}
	api := s.warpgate
	if api == nil {
		writeError(w, http.StatusConflict, warpgateNotConfiguredMessage)
		return
	}
	ctx := r.Context()

	identity, _, err := api.FindRepoIdentity(ctx, repo.ID)
	if err != nil {
		s.writeWarpgateError(w, "resolving the repo's Warpgate identity", err)
		return
	}
	if identity.Role.ID != "" {
		if err := api.UnassignTargetRole(ctx, targetID, identity.Role.ID); err != nil {
			s.writeWarpgateError(w, "removing the Warpgate SSH target from the repo's role", err)
			return
		}
	}
	if err := s.store.RemoveRepoSSHTarget(ctx, repo.ID, targetID); err != nil {
		s.internalError(w, "uncaching the repo's Warpgate SSH target", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// warpgateTargetID validates the {targetId} path segment, answering 400
// itself; ok=false means the response was written. Checked at the HTTP edge
// before any Warpgate call, like oneCLIGrantTarget's: the id becomes a path
// segment in Warpgate's own URL, and the cheapest place to refuse a caller's
// string is before it has steered any request. The router already refuses an
// empty segment (a truncated URL lands on the API tree's 404), but a
// percent-encoded "/" arrives here decoded, and neither it nor a control
// character is ever part of a Warpgate id (a UUID).
func warpgateTargetID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("targetId")
	if strings.TrimSpace(id) == "" {
		writeError(w, http.StatusBadRequest, "the SSH target id must not be empty")
		return "", false
	}
	if id == "." || id == ".." || strings.ContainsRune(id, '/') || strings.ContainsFunc(id, unicode.IsControl) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid SSH target id %q", id))
		return "", false
	}
	return id, true
}

// sshTargetRow is a Warpgate target as the lab cache stores it. The cache's
// name column must not be blank; a nameless target (upstream requires a name,
// so this is defensive) is cached under its id rather than failing a whole
// replace and leaving a stale set behind.
func sshTargetRow(t warpgate.Target) store.SSHTarget {
	name := t.Name
	if name == "" {
		name = t.ID
	}
	return store.SSHTarget{ID: t.ID, Name: name}
}

// writeWarpgateError answers a failed Warpgate admin API call: 502, because
// what broke is the upstream lab proxies, not lab itself — the same mapping
// and reasoning as writeOneCLIGatewayError.
//
// The error is forwarded VERBATIM, deliberately: it is what distinguishes
// "Warpgate is down" from "the admin token was rejected" (the *APIError's 401
// text names --warpgate-admin-token-file) from "the token lacks a
// permission". It is safe to forward because of what the warpgate package
// guarantees: the token travels only in a request header, and a targets
// response body — which may carry target credentials — is never read into an
// error. Keep it that way: do not compose a message here out of configuration.
func (s *Server) writeWarpgateError(w http.ResponseWriter, doing string, err error) {
	s.log.Warn(doing, "component", "httpapi", "err", err)
	writeError(w, http.StatusBadGateway, fmt.Sprintf("%s: %s", doing, err))
}
