package warpgate

// The per-repo identity. Lab maps ONE Warpgate user and ONE Warpgate role to
// one lab repo (ADR-0068): the role is what the operator assigns SSH targets
// to, the user holds that role and carries each run's ephemeral public key,
// and a run reaches a target as "<user>:<target>" through the bastion. Both
// are named RepoSlug(repoID) — derived from the repo's immutable store ID, so
// the match key holds still across every rename — and both carry the repo's
// human name as their description, which lab owns and heals.
//
// Everything here exists to make that mapping safe to establish lazily, from
// many goroutines, forever, against an upstream with two traps (wire.go
// points 4 and 6): usernames are unique but roles are NOT, and a user PUT
// replaces the whole object. The rules that follow from them:
//
//   - Match exactly. Upstream's search is a substring LIKE, so every lookup
//     filters the listing to the exact slug client-side.
//   - Duplicate roles are resolved deterministically: the lexicographically
//     smallest id wins, for ensure and find alike, so every caller in every
//     process converges on the same role.
//   - Never delete-and-recreate. A role carries the operator's target
//     assignments and a user carries live keys; recreating either to change
//     a description would silently revoke access. Descriptions are healed in
//     place (the user's with a GET-then-full-PUT that preserves every field
//     lab does not own).
//   - Serialize per slug inside the process (keyedMutex): two concurrent
//     ensures of one repo would otherwise both miss the role and both create
//     one, and upstream has no constraint to stop them.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// ticketSelectorPrefix is the SSH-username prefix Warpgate reads as ticket
// auth (warpgate-common/src/auth/selector.rs, consts.rs), so a username
// starting with it could never log in as a user. RepoSlug of a lab repo ID
// can never produce it; the guard exists for a caller that passes something
// else.
const ticketSelectorPrefix = "ticket-"

// RepoSlug derives the Warpgate username AND role name for a repo:
// lowercase, every byte outside [a-z0-9-] mapped to "-", leading hyphens
// stripped, capped at 50 characters. For lab's repo_<32 hex> IDs that is
// repo-<32 hex>.
//
// It is byte-for-byte the derivation onecli.AgentIdentifier uses for the same
// repo's OneCLI agent — a test pins the equality — so one repo has one slug
// across both sidecars and an operator reading either dashboard sees the same
// name. It is re-stated here rather than imported to keep this package free of
// a dependency on the OneCLI client; the test is what keeps the two from
// drifting. The alphabet also keeps the slug valid as a Warpgate SSH username
// (no ":" or "#", which Warpgate's selector would split on).
func RepoSlug(repoID string) string {
	var b strings.Builder
	b.Grow(len(repoID))
	for _, r := range strings.ToLower(repoID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	slug := strings.TrimLeft(b.String(), "-")
	if len(slug) > 50 {
		slug = slug[:50]
	}
	return slug
}

// repoSlug validates repoID and derives its slug. An empty ID, or one whose
// slug is empty or would be read as a ticket selector, is a caller bug that
// must fail locally and attributably rather than as an upstream 400 — or,
// worse, as an identity created under a name no other call site derives.
func repoSlug(repoID string) (string, error) {
	if strings.TrimSpace(repoID) == "" {
		return "", errors.New("warpgate: repo ID must not be empty")
	}
	slug := RepoSlug(repoID)
	if slug == "" {
		return "", fmt.Errorf("warpgate: repo ID %q derives an empty Warpgate name", repoID)
	}
	if strings.HasPrefix(slug, ticketSelectorPrefix) {
		return "", fmt.Errorf("warpgate: repo ID %q derives %q, which Warpgate would read as a ticket selector", repoID, slug)
	}
	return slug, nil
}

// User is a repo's Warpgate user: the identity a run's SSH key is attached
// to. Username is RepoSlug(repoID); Description is the repo's name.
type User struct{ ID, Username, Description string }

// Role is a repo's Warpgate role: what the operator assigns SSH targets to.
// Name is RepoSlug(repoID); Description is the repo's name.
type Role struct{ ID, Name, Description string }

// Identity is a repo's user and role together.
type Identity struct {
	User User
	Role Role
}

// EnsureRepoIdentity returns the repo's Warpgate user and role, creating
// whichever is missing, healing both descriptions to repoName, and making
// sure the user actively holds the role. It is idempotent and safe to call
// concurrently; the steady state is three GETs and no write.
//
// The sequence, and why each step is there:
//
//  1. User: list with ?search=<slug>, take the EXACT username match. Found →
//     heal the description (see healUser). Missing → POST. A 400 naming
//     "username" means another caller created it between our list and our
//     create (upstream's duplicate answer, wire.go point 4) — re-list once
//     and use the winner; if the re-list still has no exact match, the
//     collision was with a name differing only by case (upstream compares
//     usernames case-insensitively) and that is a loud error, never a
//     silently mismatched identity.
//  2. Role: list with ?search=<slug>, exact name, smallest id on duplicates.
//     Found → heal description and force is_default false (healRole). Missing
//     → POST, then re-list and take the smallest id again, so that a creator
//     racing another lab process converges on the same role the loser will
//     pick. Within this process slugLocks makes that race impossible.
//  3. Grant: GET the user's role rows and look for an ACTIVE row for the
//     role (revoked and expired rows are listed too, wire.go point 5); absent
//     → POST the grant with an {} body. That POST also re-activates a revoked
//     row. It answers 409 for an unknown ROLE, which is surfaced as the error
//     it is, never read as "already granted".
//
// Nothing is ever deleted here. A description heal that fails is swallowed
// (the stale description is returned): it is cosmetic, and the spawn path is
// fail-closed on this call, so surfacing it would take a spawn down over a
// display string — the same trade as onecli.EnsureAgent's rename. A role
// found marked is_default whose reset FAILS is an error, because that flag is
// not cosmetic (wire.go point 6).
func (c *Client) EnsureRepoIdentity(ctx context.Context, repoID, repoName string) (Identity, error) {
	slug, err := repoSlug(repoID)
	if err != nil {
		return Identity{}, err
	}
	unlock, err := c.slugLocks.lock(ctx, slug)
	if err != nil {
		return Identity{}, fmt.Errorf("warpgate: waiting for a concurrent operation on identity %q: %w", slug, err)
	}
	defer unlock()

	user, err := c.ensureUser(ctx, slug, repoName)
	if err != nil {
		return Identity{}, err
	}
	role, err := c.ensureRole(ctx, slug, repoName)
	if err != nil {
		return Identity{}, err
	}
	if err := c.ensureGrant(ctx, user.ID, role.ID); err != nil {
		return Identity{}, err
	}
	return Identity{User: user, Role: role}, nil
}

// FindRepoIdentity looks the repo's identity up without writing anything:
// found is true only when BOTH the user and the role exist. The returned
// Identity carries whichever half was found even when found is false (a zero
// User or Role for the missing half), so a caller that only needs the role —
// e.g. to unassign a target — can still use it.
//
// It takes no lock: it is read-only, and a concurrent ensure can only move it
// from "not found" to "found".
func (c *Client) FindRepoIdentity(ctx context.Context, repoID string) (Identity, bool, error) {
	slug, err := repoSlug(repoID)
	if err != nil {
		return Identity{}, false, err
	}
	user, userOK, err := c.findUser(ctx, slug)
	if err != nil {
		return Identity{}, false, err
	}
	roles, err := c.findRoles(ctx, slug)
	if err != nil {
		return Identity{}, false, err
	}
	var id Identity
	if userOK {
		id.User = user
	}
	if len(roles) > 0 {
		id.Role = roleFromWire(roles[0])
	}
	return id, userOK && len(roles) > 0, nil
}

// DeleteRepoIdentity removes the repo's user and EVERY role carrying its
// slug, reporting whether anything was removed. Lab calls it when a repo is
// deleted, so a repo's identity — its keys, its target assignments — does
// not outlive the repo.
//
// Every exact-name role is deleted, duplicates included: the slug is derived
// from a lab repo ID, so a role with that exact name is lab's by construction,
// and leaving a duplicate behind would leave its target assignments live.
// Upstream removes the user's role grants and the role's target assignments
// along with them (wire.go points 4 and 6).
//
// (false, nil) — nothing carried the slug — is an ordinary answer: a repo
// created before Warpgate was configured never got an identity. A 404 on a
// delete (someone else removed it between our list and our delete) counts as
// gone. On any other failure the error is returned with whatever was already
// deleted reported in the bool.
func (c *Client) DeleteRepoIdentity(ctx context.Context, repoID string) (bool, error) {
	slug, err := repoSlug(repoID)
	if err != nil {
		return false, err
	}
	unlock, err := c.slugLocks.lock(ctx, slug)
	if err != nil {
		return false, fmt.Errorf("warpgate: waiting for a concurrent operation on identity %q: %w", slug, err)
	}
	defer unlock()

	deleted := false
	user, ok, err := c.findUser(ctx, slug)
	if err != nil {
		return false, err
	}
	if ok {
		gone, err := c.deleteIdempotent(ctx, c.userURL(user.ID))
		if err != nil {
			return false, err
		}
		deleted = gone
	}
	roles, err := c.findRoles(ctx, slug)
	if err != nil {
		return deleted, err
	}
	for _, r := range roles {
		gone, err := c.deleteIdempotent(ctx, c.roleURL(r.ID))
		if err != nil {
			return deleted, err
		}
		deleted = deleted || gone
	}
	return deleted, nil
}

// --- users -----------------------------------------------------------------

func (c *Client) ensureUser(ctx context.Context, slug, description string) (User, error) {
	user, ok, err := c.findUser(ctx, slug)
	if err != nil {
		return User{}, err
	}
	if ok {
		return c.healUser(ctx, user, description), nil
	}

	body, err := c.do(ctx, http.MethodPost, c.usersCreateURL(), wireCreateUser{Username: slug, Description: description}, plainBody)
	switch {
	case err == nil:
		created, err := decodeOne[wireUser](body, "create user")
		if err != nil {
			return User{}, err
		}
		if created.ID == "" || created.Username != slug {
			return User{}, fmt.Errorf("warpgate: creating user %q answered a user without an id or with another name; see internal/warpgate/wire.go", slug)
		}
		return userFromWire(created), nil
	case isUsernameTaken(err):
		user, ok, lerr := c.findUser(ctx, slug)
		if lerr != nil {
			return User{}, fmt.Errorf("warpgate: resolving user %q after the create answered that the username is taken: %w", slug, lerr)
		}
		if !ok {
			return User{}, fmt.Errorf("warpgate: creating user %q answered that the username is taken, yet no user with exactly that name is listed; Warpgate compares usernames case-insensitively, so a user whose name differs only by case must be renamed or removed", slug)
		}
		return user, nil
	default:
		return User{}, err
	}
}

// isUsernameTaken recognizes upstream's duplicate-username answer: a 400
// whose body names the "username" field (wire.go point 4).
func isUsernameTaken(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Message), userFieldUsername)
}

// findUser returns the user whose username equals slug exactly. Usernames are
// unique upstream, so there is at most one.
func (c *Client) findUser(ctx context.Context, slug string) (User, bool, error) {
	body, err := c.do(ctx, http.MethodGet, c.usersURL(slug), nil, plainBody)
	if err != nil {
		return User{}, false, err
	}
	rows, err := decodeList[wireUser](body, segUsers, plainBody)
	if err != nil {
		return User{}, false, err
	}
	for _, r := range rows {
		if r.Username == slug && r.ID != "" {
			return userFromWire(r), true, nil
		}
	}
	return User{}, false, nil
}

// healUser brings a found user's description to the one lab says it should
// have. Nothing happens when it is already current — the steady state stays
// one GET. Otherwise the user is re-read by id and written back WHOLE with
// only the description replaced (userUpdateBody), because upstream's PUT
// resets every omitted field.
//
// Any failure is swallowed and the user returned with its stale description
// (see EnsureRepoIdentity). So is a re-read that names a different username:
// a heal must never become a rename.
func (c *Client) healUser(ctx context.Context, user User, description string) User {
	if user.Description == description {
		return user
	}
	raw, err := c.do(ctx, http.MethodGet, c.userURL(user.ID), nil, plainBody)
	if err != nil {
		return user
	}
	body, username, err := userUpdateBody(raw, description)
	if err != nil || username != user.Username {
		return user
	}
	if _, err := c.do(ctx, http.MethodPut, c.userURL(user.ID), body, plainBody); err != nil {
		return user
	}
	user.Description = description
	return user
}

func userFromWire(w wireUser) User {
	return User{ID: w.ID, Username: w.Username, Description: w.Description} //nolint:staticcheck // S1016: wire→domain mapping stays explicit (see wire.go)
}

// --- roles -----------------------------------------------------------------

func (c *Client) ensureRole(ctx context.Context, slug, description string) (Role, error) {
	roles, err := c.findRoles(ctx, slug)
	if err != nil {
		return Role{}, err
	}
	if len(roles) > 0 {
		return c.healRole(ctx, roles[0], description)
	}

	body, err := c.do(ctx, http.MethodPost, c.rolesCreateURL(), wireRoleWrite{Name: slug, Description: description, IsDefault: false}, plainBody)
	if err != nil {
		return Role{}, err
	}
	if _, err := decodeOne[wireRole](body, "create role"); err != nil {
		return Role{}, err
	}
	// Re-list rather than trust the create answer: if another process created
	// the same name concurrently, the smallest id is the one every caller —
	// including that one — will settle on.
	roles, err = c.findRoles(ctx, slug)
	if err != nil {
		return Role{}, fmt.Errorf("warpgate: resolving role %q after creating it: %w", slug, err)
	}
	if len(roles) == 0 {
		return Role{}, fmt.Errorf("warpgate: role %q is absent from the role listing right after it was created; refusing to report success", slug)
	}
	return roleFromWire(roles[0]), nil
}

// findRoles returns every role whose name equals slug exactly, smallest id
// first (the one ensure and find use). Upstream allows duplicate names
// (wire.go point 6).
func (c *Client) findRoles(ctx context.Context, slug string) ([]wireRole, error) {
	body, err := c.do(ctx, http.MethodGet, c.rolesURL(slug), nil, plainBody)
	if err != nil {
		return nil, err
	}
	rows, err := decodeList[wireRole](body, segRoles, plainBody)
	if err != nil {
		return nil, err
	}
	var out []wireRole
	for _, r := range rows {
		if r.Name == slug && r.ID != "" {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b wireRole) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// healRole brings a found role's description to repoName and its default
// flag to false, writing only when either differs. The description is
// cosmetic and a failed write of it is swallowed; the default flag is not —
// a default role is auto-granted to every new Warpgate user, which for a repo
// role means every other repo's user gets this repo's targets — so a role
// that is default and cannot be reset is an error.
func (c *Client) healRole(ctx context.Context, role wireRole, description string) (Role, error) {
	if role.Description == description && !role.IsDefault {
		return roleFromWire(role), nil
	}
	_, err := c.do(ctx, http.MethodPut, c.roleURL(role.ID), wireRoleWrite{Name: role.Name, Description: description, IsDefault: false}, plainBody)
	if err != nil {
		if role.IsDefault {
			return Role{}, fmt.Errorf("warpgate: role %q is marked default (auto-granted to every new Warpgate user) and resetting it failed: %w", role.Name, err)
		}
		return roleFromWire(role), nil
	}
	role.Description = description
	return roleFromWire(role), nil
}

func roleFromWire(w wireRole) Role {
	return Role{ID: w.ID, Name: w.Name, Description: w.Description}
}

// --- the grant ---------------------------------------------------------------

func (c *Client) ensureGrant(ctx context.Context, userID, roleID string) error {
	body, err := c.do(ctx, http.MethodGet, c.userRolesURL(userID), nil, plainBody)
	if err != nil {
		return err
	}
	rows, err := decodeList[wireUserRole](body, "user roles", plainBody)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID == roleID && r.IsActive {
			return nil
		}
	}
	if _, err := c.do(ctx, http.MethodPost, c.userRoleURL(userID, roleID), wireGrantRole{}, plainBody); err != nil {
		return fmt.Errorf("warpgate: granting role %s to user %s: %w", roleID, userID, err)
	}
	return nil
}

// --- per-slug serialization --------------------------------------------------

// keyedMutex serializes work per key (a repo slug) without serializing
// different keys against each other. Entries are reference-counted and
// dropped when the last holder or waiter leaves, so the map holds only keys
// in use rather than every repo ever ensured. Waiting honours ctx: a spawn
// that is cancelled while another ensure of the same repo is in flight gives
// up instead of queueing behind it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sem  chan struct{} // capacity 1: holding the lock is holding the slot
	refs int           // holders + waiters; guarded by keyedMutex.mu
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*keyedLock)}
}

// lock acquires key's lock, returning the function that releases it, or
// ctx's error if ctx ends first.
func (k *keyedMutex) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{sem: make(chan struct{}, 1)}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	select {
	case l.sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-l.sem
				k.release(key, l)
			})
		}, nil
	case <-ctx.Done():
		k.release(key, l)
		return nil, ctx.Err()
	}
}

func (k *keyedMutex) release(key string, l *keyedLock) {
	k.mu.Lock()
	defer k.mu.Unlock()
	l.refs--
	if l.refs == 0 {
		delete(k.locks, key)
	}
}
