package instance

// bastion_revoke.go is the end of a run key's life (issue #39 / ADR-0068
// "Revocation goes through the one pre-wipe hook"). A key registered at spawn
// must not outlive the run it was minted for: it is live access to every
// target the repo's role carries, and the private half sits in the run's
// runtime dir until the tree is wiped. Three entry points, all best-effort:
//
//   - RevokeBastionKey — per run, from the revocation marker the launch path
//     wrote, at the root of the run's tree, the moment the key was
//     registered. cmd/lab composes it into the ONE pre-wipe hook on
//     instancehome.Manager, beside credrotate's adopt-check (ADR-0055), so
//     Stop, the AFK reaper, a launch rollback and the orphan-tree sweep all
//     revoke with no per-site code: the hook exists precisely because every
//     path that destroys a run's tree needs one last look at it first.
//   - removeBastionKeysByLabel — the launch path's cleanup when the
//     registration itself failed ambiguously, before any marker could exist.
//   - SweepBastionKeys — at startup, over every repo's Warpgate user, for
//     what the marker cannot catch: a crash between registration and the
//     marker write, or a wipe whose revoke call failed. cmd/lab retries it
//     with backoff until one pass completes.
//
// None ever logs or wraps key material; a key is named by its Warpgate
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
// ADR-0068). It reads the revocation marker from the root of the run's tree
// (instancehome.RunPath — never the runtime dir a container run can write,
// see bastionMarkerName) — {user_id, key_id}, written by the launch path
// immediately after the key was registered — and deletes that one
// credential; on success it deletes the marker too, so a second call (the
// pre-wipe hook after a launch rollback's own call, a Wipe racing a sweep)
// finds nothing and does nothing.
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
//     credential ids and the cause. A 404 is success
//     (warpgate.Client.RemovePublicKey). The marker is left in place, but
//     nothing on this path comes back for it: every caller is a wipe that
//     removes the whole tree, marker included, the moment the hook returns.
//     The key stays live until the next startup sweep (SweepBastionKeys)
//     finds it by its label with no tree on disk — that sweep, not a retry
//     here, is the backstop.
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
	runDir := s.homes.RunPath(runID)
	m, err := readBastionMarker(runDir)
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
	if err := os.Remove(filepath.Join(runDir, bastionMarkerName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
		s.log.Warn("revoking the run's Warpgate key failed; it stays valid until the next startup sweep removes it",
			"component", "instance", "run", runID, "warpgate_user", userID, "key", keyID, "err", err)
		return false
	}
	return true
}

// removeBastionKeysByLabel is the launch path's cleanup after AddPublicKey
// FAILED: it lists userID's keys and removes every one labelled
// warpgate.RunKeyLabel(runID). A failed registration is not proof that no key
// exists — a POST Warpgate committed before the client gave up (a timeout,
// the spawn request's context cancelled, an answer that would not decode)
// leaves a live key for this run that no marker names and no wipe would
// revoke. The label is the one handle on it, and it names this run alone, so
// removing by label can touch no sibling run's key and no operator's.
//
// Best-effort, like every revocation: on the detached, bastionRevokeTimeout-
// bounded context removeBastionKey uses (the spawn's own context may be the
// very thing that was cancelled), one warning if the listing or a removal
// fails — the startup sweep is the backstop — and never an error, because the
// caller is already refusing the spawn with the registration's own error,
// which is the one the operator needs to see.
func (s *Service) removeBastionKeysByLabel(ctx context.Context, runID, userID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bastionRevokeTimeout)
	defer cancel()
	keys, err := s.warpgate.ListPublicKeys(ctx, userID)
	if err != nil {
		s.log.Warn("checking Warpgate for a run key registered despite the failed registration; any such key stays valid until the next startup sweep removes it",
			"component", "instance", "run", runID, "warpgate_user", userID, "err", err)
		return
	}
	label := warpgate.RunKeyLabel(runID)
	for _, k := range keys {
		if k.Label != label {
			continue
		}
		if err := s.warpgate.RemovePublicKey(ctx, userID, k.ID); err != nil {
			s.log.Warn("a run key registered despite the failed registration could not be removed; it stays valid until the next startup sweep removes it",
				"component", "instance", "run", runID, "warpgate_user", userID, "key", k.ID, "err", err)
		}
	}
}

// SweepBastionKeys removes orphaned run keys from every repo's Warpgate user
// at startup (issue #39 / ADR-0068): a key whose label is a lab run label
// (warpgate.RunIDFromLabel) naming a run that is neither active in the store
// nor still on disk. It catches what the per-run marker cannot — a crash
// between the key's registration and the marker write, or a wipe whose
// revoke call failed — so crash orphans do not accumulate as live access. A
// run alive across a lab restart is active in the store and keeps its key.
//
// What it never touches:
//
//   - a key whose label does not parse as a lab run label. An operator may
//     have added a key to a repo user by hand; #35's rule against deleting
//     what lab did not create, one level down.
//   - a key whose run's tree (instancehome.RunPath) still exists, marker or
//     no marker. A tree on disk is a live or in-flight run: a spawn racing
//     this sweep has its tree and — from the moment AddPublicKey commits —
//     its key, but not yet its run row and, for an instant, not even its
//     marker, so matching on either would cut the key out from under it. Or
//     the tree is an orphan, which instancehome's own sweeps (the startup one
//     and the throttled one after it) reap, revoking the key through the
//     pre-wipe hook as they do. Once the tree is gone the next startup sweep
//     removes the key regardless, so this defers a removal, never skips it.
//     A tree that cannot be stat'ed counts as present: deferring is the safe
//     way to be wrong.
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
//
// It reports whether a FULL pass completed: true when every repo was swept,
// and when there is nothing to sweep (no REST client); false when a store
// read or a Warpgate error cut the pass short. It never retries on its own —
// cmd/lab runs it again with backoff until it reports true, because a
// Warpgate that is still starting when lab boots is the common reason a pass
// is cut short, and a startup sweep that never completes leaves every crash
// orphan live until the next restart.
func (s *Service) SweepBastionKeys(ctx context.Context) bool {
	if s.warpgate == nil {
		return true
	}
	repos, err := s.store.Repos(ctx)
	if err != nil {
		s.log.Warn("sweeping orphaned Warpgate run keys: list repos", "component", "instance", "err", err)
		return false
	}
	active, err := s.store.ActiveRuns(ctx)
	if err != nil {
		s.log.Warn("sweeping orphaned Warpgate run keys: list active runs", "component", "instance", "err", err)
		return false
	}
	live := make(map[string]bool, len(active))
	for _, r := range active {
		live[r.ID] = true
	}

	ctx, cancel := context.WithTimeout(ctx, bastionSweepTimeout)
	defer cancel()
	removed := 0
	complete := true
	for i, repo := range repos {
		n, err := s.sweepRepoBastionKeys(ctx, repo, live)
		removed += n
		if err != nil {
			s.log.Warn("sweeping orphaned Warpgate run keys", "component", "instance",
				"repo", repo.ID, "err", err, "skipped", len(repos)-i-1)
			complete = false
			break
		}
	}
	if removed > 0 {
		s.log.Info("removed orphaned Warpgate run keys", "component", "instance", "removed", removed)
	}
	return complete
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
		if !ok || live[runID] || s.runTreeOnDisk(runID) {
			continue
		}
		if err := s.warpgate.RemovePublicKey(ctx, identity.User.ID, k.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// runTreeOnDisk reports whether runID's per-run tree (instancehome.RunPath)
// still exists — or cannot be ruled out: any Lstat error but "does not exist"
// counts as present (see SweepBastionKeys for why such a run's key is left
// alone). runID has already passed RunIDFromLabel's strict run_<32 hex>
// shape, so it is safe to derive a path from.
func (s *Service) runTreeOnDisk(runID string) bool {
	_, err := os.Lstat(s.homes.RunPath(runID))
	return !errors.Is(err, fs.ErrNotExist)
}
