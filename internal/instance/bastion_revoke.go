package instance

// bastion_revoke.go is the end of a run key's life (issue #39 / ADR-0068
// "Revocation goes through the one pre-wipe hook"). A key registered at spawn
// must not outlive the run it was minted for: it is live access to every
// target the repo's role carries, and the private half sits in the run's
// runtime dir until the tree is wiped. Two entry points, both best-effort:
//
//   - RevokeBastionKey — per run, from the revocation marker the launch path
//     wrote the moment the key was registered. cmd/lab composes it into the
//     ONE pre-wipe hook on instancehome.Manager, beside credrotate's
//     adopt-check (ADR-0055), so Stop, the AFK reaper, a launch rollback and
//     the orphan-tree sweep all revoke with no per-site code: the hook exists
//     precisely because every path that destroys a run's tree needs one last
//     look at it first.
//   - SweepBastionKeys — once, at startup, over every repo's Warpgate user,
//     for what the marker cannot catch: a crash between registration and the
//     marker write, or a wipe whose revoke call failed.
//
// Neither ever logs or wraps key material; a key is named by its Warpgate
// credential id, which is an opaque identifier.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// bastionRevokeTimeout bounds one key removal. RevokeBastionKey runs inside
// instancehome's pre-wipe hook — synchronously, ahead of a Stop's or a
// sweep's RemoveAll — so a Warpgate that accepts and then hangs must not hold
// a teardown hostage for the client's full per-request timeout. Ten seconds
// is generous for one DELETE; a key it gives up on is left to the startup
// sweep.
const bastionRevokeTimeout = 10 * time.Second

// bastionSweepTimeout bounds the WHOLE startup sweep, not one call — the
// reposvc.oneCLIAgentHealTimeout reasoning: a sidecar that is not listening
// yet fails instantly, so the bound is for the other shape, one that accepts
// and hangs, which would otherwise cost a per-request timeout per repo while
// the boot waits.
const bastionSweepTimeout = 30 * time.Second

// RevokeBastionKey removes the run's Warpgate key, if it has one (issue #39 /
// ADR-0068). It reads the revocation marker from the run's runtime dir —
// {user_id, key_id}, written by the launch path immediately after the key was
// registered — and deletes that one credential; on success it deletes the
// marker too, so a second call (the pre-wipe hook after a launch rollback's
// own call, a Wipe racing a sweep) finds nothing and does nothing.
//
// Best-effort by design and never an error to the caller: it runs on
// teardown paths — Stop, the AFK reaper, a launch rollback, the orphan-tree
// sweep — none of which may be failed by a bastion outage. What it does
// instead:
//
//   - s.warpgate nil (no REST client configured) → no-op. The REST client
//     alone is enough to revoke: a key registered under an earlier
//     configuration that also had an SSH address is still live.
//   - No marker — every unwired run, every run of a lab without Warpgate —
//     → no-op, silently. An unreadable or half-written marker → no-op with a
//     debug line: without a user and key id there is nothing to address, and
//     the startup sweep finds the key by its label.
//   - The removal fails → one warning naming the run, the Warpgate user and
//     credential ids and the cause, and the marker is KEPT so a later wipe
//     path can retry. A 404 is success (warpgate.Client.RemovePublicKey).
//
// The removal runs on context.WithoutCancel(ctx) with bastionRevokeTimeout:
// a teardown's own context may already be cancelled (a client that
// disconnected mid-Stop), and revoking live access is exactly the step that
// must still happen then.
//
// Safe to call from instancehome's pre-wipe hook, which carries no context —
// cmd/lab passes context.Background().
func (s *Service) RevokeBastionKey(ctx context.Context, runID string) {
	if s.warpgate == nil {
		return
	}
	// The run id becomes a path under the instances root; refuse anything
	// that could climb out of it (instancehome.checkRunID's rule — every id
	// this is ever called with already satisfies it).
	if runID == "" || strings.ContainsAny(runID, "./\\\x00") {
		return
	}
	runtimeDir := s.homes.RuntimePath(runID)
	m, err := readBastionMarker(runtimeDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.log.Debug("unreadable Warpgate run key marker; leaving the key to the startup sweep",
				"component", "instance", "run", runID, "err", err)
		}
		return
	}
	if !s.removeBastionKey(ctx, runID, m.UserID, m.KeyID) {
		return
	}
	if err := os.Remove(filepath.Join(runtimeDir, bastionMarkerName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Debug("removing the Warpgate run key marker after revocation", "component", "instance", "run", runID, "err", err)
	}
}

// removeBastionKey deletes one run key from Warpgate, bounded and detached
// from ctx's cancellation (see RevokeBastionKey), and reports whether it is
// gone. A failure is one warning and false. Also the launch path's direct
// revocation when the marker itself could not be written — the one moment a
// registered key exists that no file on disk names.
func (s *Service) removeBastionKey(ctx context.Context, runID, userID, keyID string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bastionRevokeTimeout)
	defer cancel()
	if err := s.warpgate.RemovePublicKey(ctx, userID, keyID); err != nil {
		s.log.Warn("revoking the run's Warpgate key failed; it stays valid until a later wipe or the next startup sweep removes it",
			"component", "instance", "run", runID, "warpgate_user", userID, "key", keyID, "err", err)
		return false
	}
	return true
}

// SweepBastionKeys removes orphaned run keys from every repo's Warpgate user
// at startup (issue #39 / ADR-0068): a key whose label is a lab run label
// (warpgate.RunIDFromLabel) naming a run that is not active in the store. It
// catches what the per-run marker cannot — a crash between the key's
// registration and the marker write, or a wipe whose revoke call failed — so
// crash orphans do not accumulate as live access. A run alive across a lab
// restart is active in the store and keeps its key.
//
// What it never touches:
//
//   - a key whose label does not parse as a lab run label. An operator may
//     have added a key to a repo user by hand; #35's rule against deleting
//     what lab did not create, one level down.
//   - a key whose run's revocation marker is still on disk naming exactly
//     that key. Such a run is either mid-launch — its key registered, its
//     run row not written yet, which a sweep running beside the first spawns
//     after boot would otherwise cut out from under it — or an orphaned tree
//     that instancehome's sweep will reap, revoking the key through the
//     pre-wipe hook as it does. Once the tree is gone the next startup sweep
//     removes the key regardless, so this defers a removal, never skips it.
//
// It is a no-op when s.warpgate is nil; the REST client alone is enough,
// because keys registered under a previous configuration are still live. The
// identity lookup is read-only (FindRepoIdentity): a repo whose Warpgate user
// does not exist has no keys to sweep, and creating one here would be a
// write the startup heal owns.
//
// Bounded by bastionSweepTimeout for the whole pass, and it stops at the
// FIRST Warpgate error with ONE warning naming the repo it stopped on and how
// many it never reached — reposvc.reconcileOneCLIAgents' rationale: every
// error reachable here is sidecar-level (unreachable, wedged, a rejected
// token), the same error waiting for every remaining repo, and pressing on
// would render one outage as a wall of identical warnings. It never fails
// boot. One info line reports how many keys it removed, when any.
func (s *Service) SweepBastionKeys(ctx context.Context) {
	if s.warpgate == nil {
		return
	}
	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("sweeping orphaned Warpgate run keys: list repos", "component", "instance", "err", err)
		return
	}
	active, err := s.store.ActiveRuns(ctx)
	if err != nil {
		s.log.Warn("sweeping orphaned Warpgate run keys: list active runs", "component", "instance", "err", err)
		return
	}
	live := make(map[string]bool, len(active))
	for _, r := range active {
		live[r.ID] = true
	}

	ctx, cancel := context.WithTimeout(ctx, bastionSweepTimeout)
	defer cancel()
	removed := 0
	for i, repo := range repos {
		n, err := s.sweepRepoBastionKeys(ctx, repo, live)
		removed += n
		if err != nil {
			s.log.Warn("sweeping orphaned Warpgate run keys", "component", "instance",
				"repo", repo.ID, "err", err, "skipped", len(repos)-i-1)
			break
		}
	}
	if removed > 0 {
		s.log.Info("removed orphaned Warpgate run keys", "component", "instance", "removed", removed)
	}
}

// sweepRepoBastionKeys is SweepBastionKeys for one repo: the keys it removed,
// and the first Warpgate error, which ends the whole pass.
func (s *Service) sweepRepoBastionKeys(ctx context.Context, repo store.Repo, live map[string]bool) (int, error) {
	identity, _, err := s.warpgate.FindRepoIdentity(ctx, repo.ID)
	if err != nil {
		return 0, err
	}
	// FindRepoIdentity fills in whichever half exists; keys hang off the
	// user alone, so a user without its role is still swept.
	if identity.User.ID == "" {
		return 0, nil
	}
	keys, err := s.warpgate.ListPublicKeys(ctx, identity.User.ID)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, k := range keys {
		runID, ok := warpgate.RunIDFromLabel(k.Label)
		if !ok || live[runID] || s.bastionKeyMarked(runID, identity.User.ID, k.ID) {
			continue
		}
		if err := s.warpgate.RemovePublicKey(ctx, identity.User.ID, k.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// bastionKeyMarked reports whether runID's tree still holds a revocation
// marker naming exactly this key (see SweepBastionKeys for why such a key is
// left to the tree's own wipe). runID has already passed RunIDFromLabel's
// strict run_<32 hex> shape, so it is safe to derive a path from.
func (s *Service) bastionKeyMarked(runID, userID, keyID string) bool {
	m, err := readBastionMarker(s.homes.RuntimePath(runID))
	return err == nil && m.UserID == userID && m.KeyID == keyID
}
